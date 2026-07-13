package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestReadMetadataSyncStateAbsentAndFalseDoNotNeedSync(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		state, err := readMetadataSyncState(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if state.Needed {
			t.Fatalf("absent marker unexpectedly needs sync: %#v", state)
		}
	})

	t.Run("false", func(t *testing.T) {
		dir := t.TempDir()
		writeMetadataSyncTestAppData(t, dir, `{"lastSync":1783926386123,"syncStatus":"completed","syncNeeded":false}`)
		state, err := readMetadataSyncState(dir)
		if err != nil {
			t.Fatal(err)
		}
		if state.Needed {
			t.Fatalf("false marker unexpectedly needs sync: %#v", state)
		}
	})
}

func TestMetadataSyncLastPullConvertsMillisecondsToUTCSDKFormat(t *testing.T) {
	instant := time.Date(2026, time.July, 13, 7, 46, 26, 123000000, time.UTC)
	got, err := metadataSyncLastPull(instant.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-07-13 07:46:26"; got != want {
		t.Fatalf("metadataSyncLastPull() = %q, want %q", got, want)
	}
}

func TestUpdateMetadataSyncCompletedPreservesUnknownFieldsAndAtomicallyReplaces(t *testing.T) {
	dir := t.TempDir()
	original := `{"lastSync":1,"syncBy":"admin","syncStatus":"failed","syncNeeded":true,"futureFlag":{"nested":7},"custom":"keep-me"}`
	writeMetadataSyncTestAppData(t, dir, original)
	completedAt := time.Date(2026, time.July, 13, 8, 1, 2, 345000000, time.FixedZone("CEST", 2*60*60))

	if err := updateMetadataSyncCompleted(dir, completedAt); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(metadataSyncAppDataRel)))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("updated app-data is invalid JSON: %v\n%s", err, raw)
	}
	if got["custom"] != "keep-me" || !reflect.DeepEqual(got["futureFlag"], map[string]interface{}{"nested": float64(7)}) {
		t.Fatalf("unknown app-data fields were not preserved: %#v", got)
	}
	if got["syncNeeded"] != false || got["syncStatus"] != "completed" || int64(got["lastSync"].(float64)) != completedAt.UnixMilli() {
		t.Fatalf("completion fields = %#v", got)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".now", ".app-data-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("atomic replacement left temporary files behind: %#v", matches)
	}
}

func TestDiffProjectForGliderDetectsCreateUpdateDeleteAndIgnoresGeneratedDirs(t *testing.T) {
	dir := t.TempDir()
	writeMetadataSyncTestFile(t, dir, "src/updated.now.ts", "after")
	writeMetadataSyncTestFile(t, dir, "src/created.now.ts", "new")
	writeMetadataSyncTestFile(t, dir, "node_modules/pkg/index.js", "ignored")
	writeMetadataSyncTestFile(t, dir, "dist/output.js", "ignored")
	writeMetadataSyncTestFile(t, dir, "target/app.zip", "ignored")

	project := syncedBuildProject{
		Dir: dir,
		Original: map[string][]byte{
			"src/updated.now.ts":        []byte("before"),
			"src/deleted.now.ts":        []byte("gone"),
			"node_modules/old/index.js": []byte("ignored deletion"),
			"dist/old.js":               []byte("ignored deletion"),
			"target/old.zip":            []byte("ignored deletion"),
		},
	}
	diff, err := diffProjectForGlider(project)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(diff.Create["src/created.now.ts"]); got != "new" {
		t.Fatalf("created content = %q", got)
	}
	if got := string(diff.Update["src/updated.now.ts"]); got != "after" {
		t.Fatalf("updated content = %q", got)
	}
	if want := []string{"src/deleted.now.ts"}; !reflect.DeepEqual(diff.Remove, want) {
		t.Fatalf("remove = %#v, want %#v", diff.Remove, want)
	}
	for rel := range diff.Create {
		if strings.HasPrefix(rel, "node_modules/") || strings.HasPrefix(rel, "dist/") || strings.HasPrefix(rel, "target/") {
			t.Fatalf("generated path included in create diff: %q", rel)
		}
	}
}

func TestPersistProjectGliderDiffUsesExactRemoveURIEntries(t *testing.T) {
	var removed []gliderChangeEntry
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/sn_glider/v2/sync/state" {
			var payload struct {
				URIs []string `json:"uris"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			files := make([]gliderChangeEntry, 0, len(payload.URIs))
			for _, uri := range payload.URIs {
				if strings.Contains(uri, "src/a.now.ts") {
					files = append(files, gliderChangeEntry{URI: uri, Type: "file", Checksum: "old-a"})
				}
				if strings.Contains(uri, "src/b.now.ts") {
					files = append(files, gliderChangeEntry{URI: uri, Type: "file", Checksum: "old-b"})
				}
			}
			// Before apply both files exist; after apply the test only needs deletion confirmation.
			if len(removed) > 0 {
				files = nil
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": map[string]interface{}{"files": files}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/sn_glider/v2/sync/changes/apply" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if part.FormName() != "remove" {
				continue
			}
			raw, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &removed); err != nil {
				t.Fatalf("decode remove part: %v\n%s", err, raw)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	root := "now-file:/app-sys-id"
	project := syncedBuildProject{
		RootURI: root,
		Entries: map[string]gliderChangeEntry{
			root + "/src/a.now.ts": {URI: root + "/src/a.now.ts", Type: "file", Checksum: "old-a"},
			root + "/src/b.now.ts": {URI: root + "/src/b.now.ts", Type: "file", Checksum: "old-b"},
		},
	}
	client := &Client{cfg: CLIConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
	if err := client.persistProjectGliderDiff(context.Background(), project, projectGliderDiff{Remove: []string{"src/b.now.ts", "src/a.now.ts"}}); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(removed))
	for _, entry := range removed {
		got = append(got, entry.URI)
	}
	sort.Strings(got)
	want := []string{root + "/src/a.now.ts", root + "/src/b.now.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remove URIs = %#v, want %#v", got, want)
	}
}

func TestVerifyMetadataSyncBaselineAcceptsNormalizedChecksumWhenBytesMatch(t *testing.T) {
	root := "now-file:/app"
	content := []byte("same bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": map[string]interface{}{"files": []interface{}{map[string]interface{}{"uri": root + "/src/a.ts", "type": "file", "checksum": "normalized-different"}}}})
		case "/api/sn_glider/v2/sync/files":
			writeMultipartSyncFilesResponse(t, w, "normalized-different", content)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	project := syncedBuildProject{
		RootURI:  root,
		Entries:  map[string]gliderChangeEntry{root + "/src/a.ts": {URI: root + "/src/a.ts", Type: "file", Checksum: "root-checksum"}},
		Original: map[string][]byte{"src/a.ts": content},
	}
	diff := projectGliderDiff{Update: map[string][]byte{"src/a.ts": []byte("new bytes")}}
	if err := c.verifyMetadataSyncBaseline(context.Background(), project, diff); err != nil {
		t.Fatal(err)
	}
}

func TestRunMetadataIncrementalTransformKeepsTokenOffArgvAndReturnsChangedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake node helper is a Unix shell script")
	}
	dir := t.TempDir()
	fakeBin := t.TempDir()
	argvFile := filepath.Join(t.TempDir(), "argv")
	stdinFile := filepath.Join(t.TempDir(), "stdin")
	fakeNode := filepath.Join(fakeBin, "node")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$METADATA_SYNC_TEST_ARGV"
cat > "$METADATA_SYNC_TEST_STDIN"
printf '%s' '{"changedFiles":["src/fluent/generated/table.now.ts","metadata/sys_ui_view.xml"],"handledPaths":["sys_db_object"]}'
`
	if err := os.WriteFile(fakeNode, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("METADATA_SYNC_TEST_ARGV", argvFile)
	t.Setenv("METADATA_SYNC_TEST_STDIN", stdinFile)

	const token = "oauth-super-secret-token"
	client := &Client{cfg: CLIConfig{InstanceURL: "https://example.service-now.com"}, oauthAccessToken: token}
	result, err := client.runMetadataIncrementalTransform(context.Background(), dir, "2026-07-13 07:46:26")
	if err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), token) {
		t.Fatalf("OAuth token leaked into node argv: %q", argv)
	}
	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]string
	if err := json.Unmarshal(stdin, &request); err != nil {
		t.Fatalf("node stdin is not JSON: %v\n%s", err, stdin)
	}
	if request["token"] != token || request["lastPull"] != "2026-07-13 07:46:26" {
		t.Fatalf("node stdin request = %#v", request)
	}
	wantChanged := []string{"src/fluent/generated/table.now.ts", "metadata/sys_ui_view.xml"}
	if !reflect.DeepEqual(result.ChangedFiles, wantChanged) {
		t.Fatalf("changed files = %#v, want %#v", result.ChangedFiles, wantChanged)
	}
}

func TestRunMetadataSyncNodeCommandIgnoresSuccessfulStderrWarnings(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper.js")
	script := `process.stderr.write("DeprecationWarning: harmless\n"); process.stdout.write('{"changedFiles":["src/a.ts"],"handledPaths":[]}');`
	if err := os.WriteFile(helper, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	output, err := runMetadataSyncNodeCommand(context.Background(), dir, node, helper, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "DeprecationWarning") {
		t.Fatalf("stderr leaked into stdout: %s", output)
	}
	if !strings.Contains(string(output), `"changedFiles"`) {
		t.Fatalf("missing JSON output: %s", output)
	}
}

func TestMetadataTransformFailureLeavesAppDataUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake node helper is a Unix shell script")
	}
	dir := t.TempDir()
	original := []byte("{\n  \"lastSync\": 1783926386123,\n  \"syncNeeded\": true,\n  \"syncStatus\": \"failed\",\n  \"unknown\": \"preserve byte-for-byte\"\n}\n")
	writeMetadataSyncTestAppDataBytes(t, dir, original)
	fakeBin := t.TempDir()
	fakeNode := filepath.Join(fakeBin, "node")
	if err := os.WriteFile(fakeNode, []byte("#!/bin/sh\necho transform-boom >&2\nexit 23\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	client := &Client{cfg: CLIConfig{InstanceURL: "https://example.service-now.com"}, oauthAccessToken: "token"}
	project := syncedBuildProject{Dir: dir, RootURI: "now-file:/app", Entries: map[string]gliderChangeEntry{}, Original: map[string][]byte{metadataSyncAppDataRel: append([]byte(nil), original...)}}
	if _, err := client.syncMetadataBeforeBuild(context.Background(), project); err == nil {
		t.Fatal("syncMetadataBeforeBuild() succeeded, want transform failure")
	}
	got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(metadataSyncAppDataRel)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("app-data changed after failed transform:\n got: %s\nwant: %s", got, original)
	}
}

func TestMetadataSyncNoOpSuccess(t *testing.T) {
	dir := t.TempDir()
	writeMetadataSyncTestAppData(t, dir, `{"lastSync":1783926386123,"syncStatus":"completed","syncNeeded":false}`)
	client := &Client{cfg: CLIConfig{InstanceURL: ":// deliberately invalid; no request expected"}}
	result, err := client.syncMetadataBeforeBuild(context.Background(), syncedBuildProject{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedFiles) != 0 || len(result.HandledPaths) != 0 {
		t.Fatalf("no-op result = %#v", result)
	}
	if err := client.persistProjectGliderDiff(context.Background(), syncedBuildProject{}, projectGliderDiff{Create: map[string][]byte{}, Update: map[string][]byte{}}); err != nil {
		t.Fatalf("empty Glider diff should be a no-op: %v", err)
	}
}

func writeMetadataSyncTestAppData(t *testing.T, dir, body string) {
	t.Helper()
	writeMetadataSyncTestAppDataBytes(t, dir, []byte(body))
}

func writeMetadataSyncTestAppDataBytes(t *testing.T, dir string, body []byte) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(metadataSyncAppDataRel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeMetadataSyncTestFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
