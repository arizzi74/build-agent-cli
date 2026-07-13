package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type syncTestRemote struct {
	files map[string][]byte
}

func newSyncTestServer(t *testing.T, remote *syncTestRemote) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			entries := make([]map[string]interface{}, 0, len(remote.files))
			for rel, content := range remote.files {
				entries = append(entries, map[string]interface{}{"uri": "now-file:/app/" + rel, "checksum": sha1Hex(content), "type": "file", "size": len(content)})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": entries})
		case "/api/sn_glider/v2/sync/files":
			ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if !strings.HasPrefix(ct, "multipart/") {
				t.Fatalf("sync/files content type %q", ct)
			}
			mr, err := r.MultipartReader()
			if err != nil {
				t.Fatal(err)
			}
			out := []map[string]string{}
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(part)
				var uris []string
				if part.FormName() == "uris" {
					_ = json.Unmarshal(raw, &uris)
				}
				for _, uri := range uris {
					rel := strings.TrimPrefix(uri, "now-file:/app/")
					content := remote.files[rel]
					out = append(out, map[string]string{"checksum": sha1Hex(content), "content": string(content)})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": out})
		case "/api/sn_glider/v2/sync/changes/apply":
			_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			mr := multipart.NewReader(r.Body, params["boundary"])
			blobs := map[string][]byte{}
			var create, update, remove []gliderChangeEntry
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(part)
				switch part.FormName() {
				case "create":
					_ = json.Unmarshal(raw, &create)
				case "update":
					_ = json.Unmarshal(raw, &update)
				case "remove":
					_ = json.Unmarshal(raw, &remove)
				default:
					if part.Header.Get("Content-Type") == "application/octet-stream" {
						blobs[part.FormName()] = raw
					}
				}
			}
			for _, entry := range append(create, update...) {
				if entry.Type != "file" {
					continue
				}
				remote.files[strings.TrimPrefix(entry.URI, "now-file:/app/")] = blobs[entry.Checksum]
			}
			for _, entry := range remove {
				delete(remote.files, strings.TrimPrefix(entry.URI, "now-file:/app/"))
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestFetchPersistentRemoteFilesRetriesOmittedBatchFileIndividually(t *testing.T) {
	files := map[string][]byte{"src/a.ts": []byte("a"), "src/client/app.jsx": []byte("app")}
	var singleRetry bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": []map[string]interface{}{
				{"uri": "now-file:/app/src/a.ts", "checksum": sha1Hex(files["src/a.ts"]), "type": "file", "size": 1},
				{"uri": "now-file:/app/src/client/app.jsx", "checksum": sha1Hex(files["src/client/app.jsx"]), "type": "file", "size": 3},
			}})
		case "/api/sn_glider/v2/sync/files":
			mr, err := r.MultipartReader()
			if err != nil {
				t.Fatal(err)
			}
			var uris []string
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(part)
				if part.FormName() == "uris" {
					_ = json.Unmarshal(raw, &uris)
				}
			}
			out := []map[string]string{}
			for _, uri := range uris {
				rel := strings.TrimPrefix(uri, "now-file:/app/")
				if len(uris) > 1 && rel == "src/client/app.jsx" {
					continue
				}
				if len(uris) == 1 && rel == "src/client/app.jsx" {
					singleRetry = true
				}
				out = append(out, map[string]string{"checksum": sha1Hex(files[rel]), "content": string(files[rel])})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": out})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	_, contents, err := c.fetchPersistentRemoteFiles(context.Background(), "now-file:/app")
	if err != nil {
		t.Fatal(err)
	}
	if !singleRetry || string(contents["src/client/app.jsx"]) != "app" {
		t.Fatalf("singleRetry=%v contents=%q", singleRetry, contents["src/client/app.jsx"])
	}
}

func testSyncClient(server *httptest.Server) *Client {
	return &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), workspaceName: "My Workspace", currentApp: &AppScope{ScopeID: "app", AppSysID: "app"}}
}

func TestPersistentProjectDirIsCWDScopedAndCollisionSafe(t *testing.T) {
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	one, err := persistentProjectDir("https://dev.service-now.com", "A B", "app")
	if err != nil {
		t.Fatal(err)
	}
	two, _ := persistentProjectDir("https://dev.service-now.com", "A/B", "app")
	if !strings.HasPrefix(one, filepath.Join(wd, ".build-agent")) || one == two {
		t.Fatalf("paths=%q %q", one, two)
	}
}

func TestPersistentSyncPullPushConflictAndExclusions(t *testing.T) {
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	remote := &syncTestRemote{files: map[string][]byte{"now.config.json": []byte("{}"), "src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	first, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil || len(first.Pulled) != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if err := os.MkdirAll(filepath.Join(first.LocalDir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.LocalDir, "node_modules", "keep"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.LocalDir, "src", "a.ts"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	pushed, err := c.syncPersistentApp(context.Background(), persistentSyncPush)
	if err != nil || len(pushed.Pushed) != 1 || string(remote.files["src/a.ts"]) != "local" {
		t.Fatalf("push=%+v remote=%q err=%v", pushed, remote.files["src/a.ts"], err)
	}
	if _, err := os.Stat(filepath.Join(first.LocalDir, "node_modules", "keep")); err != nil {
		t.Fatalf("excluded local dependency removed: %v", err)
	}
	remote.files["src/a.ts"] = []byte("remote-two")
	if err := os.WriteFile(filepath.Join(first.LocalDir, "src", "a.ts"), []byte("local-two"), 0o644); err != nil {
		t.Fatal(err)
	}
	conflict, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err == nil || !strings.Contains(err.Error(), "conflict") || len(conflict.Conflicts) != 1 {
		t.Fatalf("conflict=%+v err=%v", conflict, err)
	}
}

func TestReplaceLastGliderBuildPreservesPersistentProject(t *testing.T) {
	projectDir := t.TempDir()
	marker := filepath.Join(projectDir, "node_modules", "keep")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Client{lastGliderBuild: &gliderBuildState{TempDir: projectDir, Persistent: true}}
	c.invalidateLastGliderBuild()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("persistent project removed during invalidation: %v", err)
	}
}

func TestBuildProjectReusesPersistentDirectory(t *testing.T) {
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	remote := &syncTestRemote{files: map[string][]byte{"now.config.json": []byte(`{"scope":"x_demo"}`), "package.json": []byte(`{"name":"demo","version":"1.0.0"}`)}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	project, cleanup, err := c.syncGliderBuildProjectToTemp(context.Background(), "now-file:/app")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if !strings.HasPrefix(project.Dir, filepath.Join(wd, ".build-agent")) {
		t.Fatalf("project dir %q not persistent", project.Dir)
	}
	if err := os.MkdirAll(filepath.Join(project.Dir, "node_modules", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.syncGliderBuildProjectToTemp(context.Background(), "now-file:/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project.Dir, "node_modules", "keep")); err != nil {
		t.Fatalf("persistent dependencies not reused: %v", err)
	}
}

func TestPersistentSyncDeleteAndStatus(t *testing.T) {
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("one")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	first, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(first.LocalDir, "src", "a.ts")); err != nil {
		t.Fatal(err)
	}
	status, err := c.syncPersistentApp(context.Background(), persistentSyncStatus)
	if err != nil || len(status.Pushed) != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	_, err = c.syncPersistentApp(context.Background(), persistentSyncPush)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := remote.files["src/a.ts"]; ok {
		t.Fatal("remote deletion was not applied")
	}
}

func TestPersistentSyncRefusesProcessingTurn(t *testing.T) {
	c := &Client{processing: true}
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err == nil || !strings.Contains(err.Error(), "processing") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildSyncAllowedWhileTurnProcessingAndUsesRequestedApp(t *testing.T) {
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	remote := &syncTestRemote{files: map[string][]byte{"now.config.json": []byte(`{"scope":"x_demo"}`)}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	c.processing = true
	c.currentApp = &AppScope{ScopeID: "different", AppSysID: "different"}
	project, _, err := c.syncGliderBuildProjectToPersistent(context.Background(), "app", "now-file:/app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(project.Dir, string(filepath.Separator)+"app") {
		t.Fatalf("project dir %q does not use requested app", project.Dir)
	}
}

func TestPersistentSyncRejectsSymlinkDestination(t *testing.T) {
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	projectDir, err := c.persistentProjectDirForActiveApp()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(projectDir, "src")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "a.ts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote content escaped project root: %v", err)
	}
}

func TestPersistentSyncSafePaths(t *testing.T) {
	for _, rel := range []string{"../x", "src/../../x", "node_modules/x", ".ba-cli-sync/manifest.json"} {
		if isPersistentSyncFile(rel) {
			t.Fatalf("unsafe/excluded %q allowed", rel)
		}
	}
	if !isPersistentSyncFile("src/fluent/a.ts") {
		t.Fatal("normal source excluded")
	}
}
