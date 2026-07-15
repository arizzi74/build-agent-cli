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

func TestFetchPersistentRemoteFilesReconcilesStaleDeletedState(t *testing.T) {
	content := []byte("deleted")
	stateCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			stateCalls++
			files := []map[string]interface{}{}
			if stateCalls == 1 {
				files = append(files, map[string]interface{}{"uri": "now-file:/app/src/deleted.ts", "checksum": sha1Hex(content), "type": "file", "size": len(content)})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": files})
		case "/api/sn_glider/v2/sync/files":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	entries, contents, err := c.fetchPersistentRemoteFiles(context.Background(), "now-file:/app")
	if err != nil {
		t.Fatal(err)
	}
	if stateCalls != 2 || len(entries) != 0 || len(contents) != 0 {
		t.Fatalf("stateCalls=%d entries=%#v contents=%#v", stateCalls, entries, contents)
	}
}

func testSyncClient(server *httptest.Server) *Client {
	return &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), workspaceName: "My Workspace", currentApp: &AppScope{ScopeID: "app", AppSysID: "app", ScopeName: "Bocota"}}
}

func setPersistentProjectTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestPersistentSyncTreatsRootPackageLockAsLocalDependencyArtifact(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{
		"now.config.json":   []byte(`{"scope":"x_demo"}`),
		"package-lock.json": []byte("remote lock"),
	}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	result, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pulled) != 1 || result.Pulled[0] != "now.config.json" {
		t.Fatalf("initial sync = %+v", result)
	}
	projectDir := filepath.Join(home, "BA", "Bocota")
	if err := os.WriteFile(filepath.Join(projectDir, "package-lock.json"), []byte("local lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err := c.syncPersistentApp(context.Background(), persistentSyncStatus)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Conflicts) != 0 || len(status.PendingPush) != 0 || len(status.Pulled) != 0 {
		t.Fatalf("package lock affected sync status: %+v", status)
	}
	manifest, ok, err := loadPersistentSyncManifest(projectDir)
	if err != nil || !ok {
		t.Fatalf("manifest ok=%v err=%v", ok, err)
	}
	if _, exists := manifest.Files["package-lock.json"]; exists {
		t.Fatal("local dependency package-lock.json was persisted in the source manifest")
	}
}

func TestPersistentProjectDirFallsBackToAppIDWhenNoNameIsKnown(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	one, err := persistentProjectDir("https://dev.service-now.com", "A B", "app")
	if err != nil {
		t.Fatal(err)
	}
	two, _ := persistentProjectDir("https://dev.service-now.com", "A/B", "app")
	if one != filepath.Join(home, "BA", "app") || one != two {
		t.Fatalf("paths=%q %q", one, two)
	}
}

func TestCanonicalProjectCollisionIsTypedForInvalidManifestAndUnsafeSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(target, persistentSyncDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, persistentSyncDirName, persistentSyncManifestFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := canonicalProjectCollision(target, "https://example.test", "app", "now-file:/app")
	var collision *canonicalProjectCollisionError
	if !errors.As(err, &collision) || !collision.Recoverable || collision.Reason != "invalid sync manifest" {
		t.Fatalf("err=%T %v collision=%+v", err, err, collision)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err = canonicalProjectCollision(link, "https://example.test", "app", "now-file:/app")
	if !errors.As(err, &collision) || collision.Recoverable {
		t.Fatalf("err=%T %v collision=%+v", err, err, collision)
	}
}

func TestPersistentProjectDirUsesExactHumanAppName(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	c := &Client{currentApp: &AppScope{ScopeID: "app-id", AppSysID: "app-id", ScopeName: "Bocota"}}
	dir, err := c.persistentProjectDirForActiveApp()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "BA", "Bocota"); dir != want {
		t.Fatalf("dir=%q want %q", dir, want)
	}
}

func TestConfiguredProjectRootBuildsCanonicalPathAndMigratesRegisteredPrimary(t *testing.T) {
	setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	c.opts.Profile = "configured-root"
	c.cfg.ProjectRoot = filepath.Join(t.TempDir(), "projects")

	historical := filepath.Join(t.TempDir(), "historical")
	manifest := writeProjectTestManifest(t, c, historical, "app")
	if _, err := c.registerPersistentProject(historical, "app", "now-file:/app", manifest.CheckoutID, true); err != nil {
		t.Fatal(err)
	}
	result, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(c.cfg.ProjectRoot, "Bocota")
	if result.LocalDir != want {
		t.Fatalf("local dir = %q, want configured canonical %q", result.LocalDir, want)
	}
	if _, err := os.Stat(historical); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historical registered primary was not migrated: %v", err)
	}
}

func TestConfiguredProjectRootRejectsMigrationIntoRegisteredSource(t *testing.T) {
	setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	c.opts.Profile = "overlapping-root"

	historical := filepath.Join(t.TempDir(), "historical")
	manifest := writeProjectTestManifest(t, c, historical, "app")
	if _, err := c.registerPersistentProject(historical, "app", "now-file:/app", manifest.CheckoutID, true); err != nil {
		t.Fatal(err)
	}
	c.cfg.ProjectRoot = filepath.Join(historical, "nested-root")
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err == nil || !strings.Contains(err.Error(), "overlapping paths") {
		t.Fatalf("overlapping configured root err = %v", err)
	}
	if _, err := os.Stat(historical); err != nil {
		t.Fatalf("registered source changed after overlap rejection: %v", err)
	}
	if _, err := os.Stat(c.cfg.ProjectRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overlapping target root was created: %v", err)
	}
}

func TestConfiguredProjectRootKeepsCanonicalCollisionProtection(t *testing.T) {
	setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	c.cfg.ProjectRoot = filepath.Join(t.TempDir(), "projects")
	canonical := filepath.Join(c.cfg.ProjectRoot, "Bocota")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "keep.txt"), []byte("do not claim"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("configured root collision err = %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(canonical, "keep.txt")); err != nil || string(content) != "do not claim" {
		t.Fatalf("collision changed configured-root content = %q, %v", content, err)
	}
}

func TestPersistentProjectDirPreservesSpacesAndSanitizesUnsafeNames(t *testing.T) {
	if got, err := persistentAppDirName("My Test App", "app"); err != nil || got != "My Test App" {
		t.Fatalf("safe name = %q, %v", got, err)
	}
	got, err := persistentAppDirName("Bad/Name\x00", "app")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Bad-Name-" || strings.ContainsAny(got, "/\\\x00") || got == "." || got == ".." {
		t.Fatalf("unsafe name was not safely sanitized: %q", got)
	}
	if got, err := persistentAppDirName("Trailing. ", "app"); err != nil || got != "Trailing" {
		t.Fatalf("trailing Windows-unsafe characters = %q, %v", got, err)
	}
	if got, err := persistentAppDirName("CON.txt", "app"); err != nil || got != "app-CON.txt" {
		t.Fatalf("Windows reserved name = %q, %v", got, err)
	}
}

func TestPersistentSyncPullPushConflictAndExclusions(t *testing.T) {
	home := setPersistentProjectTestHome(t)
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
	if want := filepath.Join(home, "BA", "Bocota"); first.LocalDir != want {
		t.Fatalf("sync local dir %q want active app folder %q", first.LocalDir, want)
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
	home := setPersistentProjectTestHome(t)
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
	if want := filepath.Join(home, "BA", "Bocota"); project.Dir != want {
		t.Fatalf("project dir %q want %q", project.Dir, want)
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
	setPersistentProjectTestHome(t)
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
	if err != nil || len(status.PendingPush) != 1 {
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
	home := setPersistentProjectTestHome(t)
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	remote := &syncTestRemote{files: map[string][]byte{"now.config.json": []byte(`{"scope":"x_demo"}`)}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	c.processing = true
	c.currentApp = &AppScope{ScopeID: "different", AppSysID: "different", ScopeName: "Different"}
	project, _, err := c.syncGliderBuildProjectToPersistentNamed(context.Background(), "app", "Build App", "now-file:/app")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "BA", "Build App"); project.Dir != want {
		t.Fatalf("project dir %q does not use requested app name %q", project.Dir, want)
	}
}

func TestPersistentSyncRefusesSameNamedFolderOwnedByAnotherApp(t *testing.T) {
	setPersistentProjectTestHome(t)
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err != nil {
		t.Fatal(err)
	}
	_, err := c.syncPersistentAppForBuildNamed(context.Background(), "other-app", "Bocota", "now-file:/other-app", persistentSyncAuto)
	if err == nil || !strings.Contains(err.Error(), "collision") || !strings.Contains(err.Error(), "another instance or app") {
		t.Fatalf("err = %v, want manifest collision", err)
	}
}

func TestPersistentSyncRefusesSameNameCanonicalFolderFromAnotherInstance(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	canonical := filepath.Join(home, "BA", "Bocota")
	foreign := &Client{cfg: CLIConfig{InstanceURL: "https://other.example.test"}}
	writeProjectTestManifest(t, foreign, canonical, "app")
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	_, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err == nil || !strings.Contains(err.Error(), "collision") || !strings.Contains(err.Error(), "another instance or app") {
		t.Fatalf("err = %v, want cross-instance collision", err)
	}
}

func TestPersistentSyncRefusesNonemptyCanonicalFolderWithoutManifest(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	canonical := filepath.Join(home, "BA", "Bocota")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "keep.txt"), []byte("do not claim"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	_, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err == nil || !strings.Contains(err.Error(), "collision") || !strings.Contains(err.Error(), "nonempty") {
		t.Fatalf("err = %v, want canonical no-claim collision", err)
	}
	if content, readErr := os.ReadFile(filepath.Join(canonical, "keep.txt")); readErr != nil || string(content) != "do not claim" {
		t.Fatalf("canonical content changed: %q %v", content, readErr)
	}
}

func TestPersistentSyncMigratesRegisteredPrimaryToCanonicalLocation(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	source := filepath.Join(t.TempDir(), "Bocota")
	writeProjectTestManifest(t, c, source, "app")
	manifest, ok, err := loadPersistentSyncManifest(source)
	if err != nil || !ok {
		t.Fatalf("source manifest=%#v ok=%v err=%v", manifest, ok, err)
	}
	if _, err := c.registerPersistentProject(source, "app", "now-file:/app", manifest.CheckoutID, true); err != nil {
		t.Fatal(err)
	}
	result, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(home, "BA", "Bocota")
	if result.LocalDir != canonical {
		t.Fatalf("local dir=%q want canonical %q", result.LocalDir, canonical)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registered source was not moved: %v", err)
	}
	if primary, err := c.projectPrimaryCheckoutID("app"); err != nil || primary != manifest.CheckoutID {
		t.Fatalf("primary=%q err=%v want=%q", primary, err, manifest.CheckoutID)
	}
}

func TestPersistentSyncMigratesRegisteredPrimaryOverEmptyCanonicalDirectory(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	source := filepath.Join(t.TempDir(), "Bocota")
	manifest := writeProjectTestManifest(t, c, source, "app")
	if _, err := c.registerPersistentProject(source, "app", "now-file:/app", manifest.CheckoutID, true); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(home, "BA", "Bocota")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil || result.LocalDir != canonical {
		t.Fatalf("result=%+v err=%v canonical=%q", result, err, canonical)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source was not moved over empty canonical directory: %v", err)
	}
}

func TestPersistentSyncUsesMatchingCanonicalCheckoutWithoutDeletingRegisteredPrimary(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	source := filepath.Join(t.TempDir(), "old")
	sourceManifest := writeProjectTestManifest(t, c, source, "app")
	if _, err := c.registerPersistentProject(source, "app", "now-file:/app", sourceManifest.CheckoutID, true); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(home, "BA", "Bocota")
	canonicalManifest := writeProjectTestManifest(t, c, canonical, "app")
	result, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil || result.LocalDir != canonical {
		t.Fatalf("result=%+v err=%v canonical=%q", result, err, canonical)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("matching canonical checkout deleted old source: %v", err)
	}
	if primary, err := c.projectPrimaryCheckoutID("app"); err != nil || primary != canonicalManifest.CheckoutID {
		t.Fatalf("primary=%q err=%v want canonical=%q", primary, err, canonicalManifest.CheckoutID)
	}
}

func TestPersistentSyncMigratesMatchingLegacyProjectOnce(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	legacyDir, err := legacyPersistentProjectDir(c.cfg.InstanceURL, c.workspaceName, "app")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacyDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "src", "a.ts"), []byte("remote"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.refreshPersistentSyncManifest(legacyDir, "app", "now-file:/app"); err != nil {
		t.Fatal(err)
	}
	result, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "BA", "Bocota"); result.LocalDir != want {
		t.Fatalf("migrated dir %q want %q", result.LocalDir, want)
	}
	if _, err := os.Stat(filepath.Join(result.LocalDir, "src", "a.ts")); err != nil {
		t.Fatalf("migrated source missing: %v", err)
	}
	if _, err := os.Stat(legacyDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy project was not moved: %v", err)
	}
}

func TestPersistentSyncMigratesRegisteredPrimaryAtArbitraryHistoricalPath(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	source := filepath.Join(t.TempDir(), "arbitrary-historical-checkout")
	if err := ensurePersistentProjectRoot(source); err != nil {
		t.Fatal(err)
	}
	legacy := persistentSyncManifest{Version: 1, AppID: "app", RootURI: "now-file:/app", CheckoutID: "historical-checkout", Files: map[string]string{}}
	if err := writePersistentSyncManifest(source, legacy); err != nil {
		t.Fatal(err)
	}
	registry := projectRegistry{Version: projectRegistryVersion, Projects: map[string]registeredProject{
		projectRegistryKey(c.cfg.InstanceURL, "app"): {InstanceURL: normalizedProjectInstance(c.cfg.InstanceURL), AppID: "app", PrimaryCheckoutID: legacy.CheckoutID, Checkouts: []projectCheckout{{ID: legacy.CheckoutID, Path: source}}},
	}}
	if err := saveProjectRegistry(c.opts.Profile, registry); err != nil {
		t.Fatal(err)
	}
	result, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(home, "BA", "Bocota")
	if result.LocalDir != canonical {
		t.Fatalf("local dir=%q want %q", result.LocalDir, canonical)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historical source was not moved: %v", err)
	}
	manifest, ok, err := loadPersistentSyncManifest(canonical)
	if err != nil || !ok || manifest.Version != 2 || manifest.InstanceURL != normalizedProjectInstance(c.cfg.InstanceURL) || manifest.CheckoutID != legacy.CheckoutID {
		t.Fatalf("migrated manifest=%+v ok=%v err=%v", manifest, ok, err)
	}
	registry, err = loadProjectRegistry(c.opts.Profile)
	if err != nil {
		t.Fatal(err)
	}
	project, ok := registryProject(registry, c.cfg.InstanceURL, "app")
	if !ok || project.PrimaryCheckoutID != legacy.CheckoutID || len(project.Checkouts) != 1 || project.Checkouts[0].ID != legacy.CheckoutID || project.Checkouts[0].Path != canonical {
		t.Fatalf("registry after migration=%+v found=%v", project, ok)
	}
}

func TestPersistentSyncMigrationReplacesExistingEmptyCanonicalDirectory(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	source := filepath.Join(t.TempDir(), "historical")
	manifest := writeProjectTestManifest(t, c, source, "app")
	if _, err := c.registerPersistentProject(source, "app", "now-file:/app", manifest.CheckoutID, true); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(home, "BA", "Bocota")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source was not renamed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(canonical, persistentSyncDirName, persistentSyncManifestFile)); err != nil {
		t.Fatalf("canonical manifest missing after empty-target migration: %v", err)
	}
}

func TestPersistentSyncSelectsExistingMatchingCanonicalCheckoutAndPreservesHistoricalSecondary(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	source := filepath.Join(t.TempDir(), "historical")
	sourceManifest := writeProjectTestManifest(t, c, source, "app")
	if _, err := c.registerPersistentProject(source, "app", "now-file:/app", sourceManifest.CheckoutID, true); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(home, "BA", "Bocota")
	canonicalManifest := writeProjectTestManifest(t, c, canonical, "app")
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("historical source should remain secondary: %v", err)
	}
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		t.Fatal(err)
	}
	project, ok := registryProject(registry, c.cfg.InstanceURL, "app")
	if !ok || project.PrimaryCheckoutID != canonicalManifest.CheckoutID || len(project.Checkouts) != 2 {
		t.Fatalf("registry=%+v found=%v", project, ok)
	}
	if _, found := registeredCheckoutAtPath(project, source); !found {
		t.Fatalf("historical checkout missing from registry: %+v", project.Checkouts)
	}
}

func TestPersistentSyncRejectsCrossInstanceSameNameCanonicalCollision(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	canonical := filepath.Join(home, "BA", "Bocota")
	if err := ensurePersistentProjectRoot(canonical); err != nil {
		t.Fatal(err)
	}
	if err := savePersistentSyncManifest(canonical, persistentSyncManifest{InstanceURL: "https://other.example.test", AppID: "app", RootURI: "now-file:/app", Files: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err == nil || !strings.Contains(err.Error(), "another instance or app") {
		t.Fatalf("cross-instance canonical collision err=%v", err)
	}
}

func TestPersistentSyncRejectsAmbiguousCanonicalV1Manifest(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	canonical := filepath.Join(home, "BA", "Bocota")
	if err := ensurePersistentProjectRoot(canonical); err != nil {
		t.Fatal(err)
	}
	legacy := persistentSyncManifest{Version: 1, AppID: "app", RootURI: "now-file:/app", CheckoutID: "legacy", Files: map[string]string{}}
	if err := writePersistentSyncManifest(canonical, legacy); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err == nil || !strings.Contains(err.Error(), "another instance or app") {
		t.Fatalf("ambiguous canonical v1 err=%v", err)
	}
	manifest, ok, err := loadPersistentSyncManifest(canonical)
	if err != nil || !ok || manifest.Version != 1 {
		t.Fatalf("ambiguous canonical manifest was claimed: %+v ok=%v err=%v", manifest, ok, err)
	}
}

func TestPersistentSyncPinsRegisteredCanonicalV1Manifest(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	canonical := filepath.Join(home, "BA", "Bocota")
	if err := ensurePersistentProjectRoot(canonical); err != nil {
		t.Fatal(err)
	}
	legacy := persistentSyncManifest{Version: 1, AppID: "app", RootURI: "now-file:/app", CheckoutID: "legacy", Files: map[string]string{}}
	if err := writePersistentSyncManifest(canonical, legacy); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	registry := projectRegistry{Version: projectRegistryVersion, Projects: map[string]registeredProject{
		projectRegistryKey(c.cfg.InstanceURL, "app"): {InstanceURL: normalizedProjectInstance(c.cfg.InstanceURL), AppID: "app", PrimaryCheckoutID: legacy.CheckoutID, Checkouts: []projectCheckout{{ID: legacy.CheckoutID, Path: canonical}}},
	}}
	if err := saveProjectRegistry(c.opts.Profile, registry); err != nil {
		t.Fatal(err)
	}
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err != nil {
		t.Fatal(err)
	}
	manifest, ok, err := loadPersistentSyncManifest(canonical)
	if err != nil || !ok || manifest.Version != 2 || manifest.InstanceURL != normalizedProjectInstance(c.cfg.InstanceURL) || manifest.CheckoutID != legacy.CheckoutID {
		t.Fatalf("registered canonical manifest was not pinned: %+v ok=%v err=%v", manifest, ok, err)
	}
}

func TestResolvePersistentProjectReadOnlyIgnoresAmbiguousCanonicalV1WhenRegisteredCheckoutExists(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	registered := filepath.Join(t.TempDir(), "Earth")
	registeredManifest := writeProjectTestManifest(t, c, registered, "app")
	if _, err := c.registerPersistentProject(registered, "app", "now-file:/app", registeredManifest.CheckoutID, true); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(home, "BA", "Bocota")
	if err := ensurePersistentProjectRoot(canonical); err != nil {
		t.Fatal(err)
	}
	legacy := persistentSyncManifest{Version: 1, AppID: "app", RootURI: "now-file:/app", CheckoutID: "ambiguous", Files: map[string]string{}}
	if err := writePersistentSyncManifest(canonical, legacy); err != nil {
		t.Fatal(err)
	}
	registryPath := projectRegistryPath(c.opts.Profile)
	beforeRegistry, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := c.resolvePersistentProject("app", "Bocota", "now-file:/app", false)
	if err != nil || resolution.Dir != registered || resolution.Source != "registered checkout" {
		t.Fatalf("read-only resolution=%+v err=%v", resolution, err)
	}
	afterRegistry, err := os.ReadFile(registryPath)
	if err != nil || string(afterRegistry) != string(beforeRegistry) {
		t.Fatalf("read-only resolution mutated registry: before=%q after=%q err=%v", beforeRegistry, afterRegistry, err)
	}
	manifest, ok, err := loadPersistentSyncManifest(canonical)
	if err != nil || !ok || manifest.Version != 1 {
		t.Fatalf("read-only resolution mutated canonical manifest: %+v ok=%v err=%v", manifest, ok, err)
	}
	if _, err := c.syncPersistentApp(context.Background(), persistentSyncAuto); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("mutating sync should reject ambiguous canonical target: %v", err)
	}
}

func TestPersistentSyncMigratesRegisteredHistoricalV1UsingRegistryCheckoutID(t *testing.T) {
	home := setPersistentProjectTestHome(t)
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("remote")}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)

	historical := filepath.Join(t.TempDir(), "historical-bocota")
	if err := ensurePersistentProjectRoot(historical); err != nil {
		t.Fatal(err)
	}
	legacy := persistentSyncManifest{Version: 1, AppID: "app", RootURI: "now-file:/app", Files: map[string]string{}}
	if err := writePersistentSyncManifest(historical, legacy); err != nil {
		t.Fatal(err)
	}
	const checkoutID = "registry-checkout"
	registry := projectRegistry{Version: projectRegistryVersion, Projects: map[string]registeredProject{
		projectRegistryKey(c.cfg.InstanceURL, "app"): {
			InstanceURL:       normalizedProjectInstance(c.cfg.InstanceURL),
			AppID:             "app",
			PrimaryCheckoutID: checkoutID,
			Checkouts:         []projectCheckout{{ID: checkoutID, Path: historical}},
		},
	}}
	if err := saveProjectRegistry(c.opts.Profile, registry); err != nil {
		t.Fatal(err)
	}

	result, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(home, "BA", "Bocota")
	if result.LocalDir != canonical {
		t.Fatalf("local dir=%q want %q", result.LocalDir, canonical)
	}
	if _, err := os.Lstat(historical); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historical checkout still exists or stat failed: %v", err)
	}
	manifest, ok, err := loadPersistentSyncManifest(canonical)
	if err != nil || !ok || manifest.Version != 2 || manifest.InstanceURL != normalizedProjectInstance(c.cfg.InstanceURL) || manifest.CheckoutID != checkoutID {
		t.Fatalf("migrated manifest=%+v ok=%v err=%v", manifest, ok, err)
	}
	checkouts, err := c.listPersistentProjectCheckouts("app", "now-file:/app")
	if err != nil || len(checkouts) != 1 || checkouts[0].ID != checkoutID || checkouts[0].Path != canonical {
		t.Fatalf("registered checkouts=%+v err=%v", checkouts, err)
	}
}

func TestPersistentSyncRejectsSymlinkDestination(t *testing.T) {
	setPersistentProjectTestHome(t)
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
	if err := savePersistentSyncManifest(projectDir, persistentSyncManifest{InstanceURL: c.cfg.InstanceURL, AppID: "app", RootURI: "now-file:/app"}); err != nil {
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
