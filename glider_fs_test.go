package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

func TestFetchGliderStateReportsNonJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><title>Login required</title></html>"))
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	_, err := c.fetchGliderStateForURIs(context.Background(), []string{"now-file:/app"})
	if err == nil || !strings.Contains(err.Error(), "non-JSON response") || !strings.Contains(err.Error(), "Login required") {
		t.Fatalf("err = %v", err)
	}
}

func TestGliderFSWriteFileTreatsVerifiedApply500AsSuccess(t *testing.T) {
	content := "export const ok = true\n"
	checksum := sha1Hex([]byte(content))
	var applyCalled bool
	var filesCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[]}`))
		case "/api/sn_glider/v2/sync/changes/apply":
			applyCalled = true
			http.Error(w, "", http.StatusInternalServerError)
		case "/api/sn_glider/v2/sync/files":
			filesCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"files":[{"checksum":%q,"content":%q}]}`, checksum, content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := &Client{
		cfg:        CLIConfig{InstanceURL: server.URL},
		httpClient: server.Client(),
		currentApp: &AppScope{ScopeID: "appsysid123", AppSysID: "appsysid123", ScopeName: "Demo", Scope: "x_snc_demo"},
	}
	result, status := c.answerGliderFSWriteFile(context.Background(), map[string]interface{}{"path": "src/fluent/demo.now.ts", "data": content})
	if status != "complete" {
		t.Fatalf("status = %q, result = %#v", status, result)
	}
	if !applyCalled || !filesCalled {
		t.Fatalf("applyCalled=%v filesCalled=%v, want both true", applyCalled, filesCalled)
	}
	if warning := stringify(result["warning"]); !strings.Contains(warning, "verified remote content") {
		t.Fatalf("warning = %q", warning)
	}
	if stringify(result["syncError"]) == "" {
		t.Fatalf("syncError missing: %#v", result)
	}
}

func TestGliderFSDeleteFileAndRecursiveDirectoryPayload(t *testing.T) {
	entries := []map[string]interface{}{
		{"uri": "now-file:/app/src/a.ts", "type": "file", "size": 1},
		{"uri": "now-file:/app/src/nested", "type": "dir", "size": 0},
		{"uri": "now-file:/app/src/nested/b.ts", "type": "file", "size": 1},
	}
	var removed []gliderChangeEntry
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": entries})
		case "/api/sn_glider/v2/sync/changes/apply":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			part, _, err := r.FormFile("remove")
			if err != nil {
				t.Fatal(err)
			}
			defer part.Close()
			if err := json.NewDecoder(part).Decode(&removed); err != nil {
				t.Fatal(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), currentApp: &AppScope{AppSysID: "app"}}
	result, status := c.answerGliderFSDelete(context.Background(), map[string]interface{}{"path": "src"})
	if status != "complete" || result["removed"] != 3 {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	if len(removed) != 3 || removed[0].URI != "now-file:/app/src/nested/b.ts" || removed[1].URI != "now-file:/app/src/a.ts" {
		t.Fatalf("remove=%#v", removed)
	}
}

func TestGliderFSDeleteSafetyIdempotenceAndVerifiedApplyError(t *testing.T) {
	c := &Client{currentApp: &AppScope{AppSysID: "app"}}
	for _, payload := range []map[string]interface{}{{"path": "."}, {"path": "../outside"}, {"path": "x.xml"}} {
		result, status := c.answerGliderFSDelete(context.Background(), payload)
		if status != "error" {
			t.Fatalf("payload=%#v result=%#v", payload, result)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c.cfg.InstanceURL, c.httpClient = server.URL, server.Client()
	result, status := c.answerGliderFSDelete(context.Background(), map[string]interface{}{"path": "missing.ts"})
	if status != "complete" || result["deleted"] != false || !strings.Contains(stringify(result["message"]), "already absent") {
		t.Fatalf("status=%q result=%#v", status, result)
	}
}

func TestGliderFSDeleteRejectsDirectoryContainingXML(t *testing.T) {
	applyCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": []interface{}{
				map[string]interface{}{"uri": "now-file:/app/src/code.ts", "type": "file"},
				map[string]interface{}{"uri": "now-file:/app/src/blocked.xml", "type": "file"},
			}})
		case "/api/sn_glider/v2/sync/changes/apply":
			applyCalled = true
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), currentApp: &AppScope{AppSysID: "app"}}
	result, status := c.answerGliderFSDelete(context.Background(), map[string]interface{}{"path": "src"})
	if status != "error" || result["code"] != "XML_BLOCKED" || applyCalled {
		t.Fatalf("status=%q apply=%v result=%#v", status, applyCalled, result)
	}
}

func TestGliderFSDeleteHonorsCancellationBeforeMutation(t *testing.T) {
	applyCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			<-r.Context().Done()
		case "/api/sn_glider/v2/sync/changes/apply":
			applyCalled = true
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), currentApp: &AppScope{AppSysID: "app"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, status := c.answerGliderFSDelete(ctx, map[string]interface{}{"path": "a.ts"})
	if status != "error" || applyCalled || !strings.Contains(strings.ToLower(stringify(result["error"])), "canceled") {
		t.Fatalf("status=%q apply=%v result=%#v", status, applyCalled, result)
	}
}

func TestGliderFSDeleteTreatsVerifiedApplyErrorAsSuccess(t *testing.T) {
	stateCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			stateCalls++
			files := []interface{}{}
			if stateCalls == 1 {
				files = []interface{}{map[string]interface{}{"uri": "now-file:/app/a.ts", "type": "file", "size": 1}}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": files})
		case "/api/sn_glider/v2/sync/changes/apply":
			http.Error(w, "late failure", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), currentApp: &AppScope{AppSysID: "app"}}
	result, status := c.answerGliderFSDelete(context.Background(), map[string]interface{}{"path": "a.ts"})
	if status != "complete" || !strings.Contains(stringify(result["warning"]), "verified remote path is absent") || stringify(result["syncError"]) == "" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
}

func TestEnsureActiveAppMetadataUsesSysAppScopeForIDEContextAndAppScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/now/table/sys_app/appsysid123":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"sys_id":"appsysid123","name":"Geronimo","scope":"x_snc_geronimo_2"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := &Client{
		cfg:        CLIConfig{InstanceURL: server.URL},
		httpClient: server.Client(),
		currentApp: &AppScope{ScopeID: "appsysid123", AppSysID: "appsysid123", ScopeName: "Geronimo"},
	}
	if err := c.ensureActiveAppMetadata(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.currentApp.Scope != "x_snc_geronimo_2" {
		t.Fatalf("scope = %q", c.currentApp.Scope)
	}
	ctx := c.currentIDEContext()
	if ctx["scopeName"] != "x_snc_geronimo_2" {
		t.Fatalf("ideContext scopeName = %#v", ctx["scopeName"])
	}
	appScope, ok := c.nirvanaOutboundAppScope()
	if !ok {
		t.Fatal("missing outbound appScope")
	}
	if appScope["scopeName"] != "x_snc_geronimo_2" || appScope["scope"] != "x_snc_geronimo_2" || appScope["appName"] != "Geronimo" {
		t.Fatalf("unexpected outbound appScope: %#v", appScope)
	}
}

func TestGliderFSGrepRegexPathGlobAndSchema(t *testing.T) {
	files := map[string][]byte{
		"now-file:/app/src/a.ts":        []byte("const x = Record(1);\n"),
		"now-file:/app/src/nested/b.ts": []byte("αRecord(2)\n"),
		"now-file:/app/src/skip.js":     []byte("Record(3)\n"),
		"now-file:/app/other/c.ts":      []byte("Record(4)\n"),
	}
	c := newFSGrepTestClient(t, files, nil)
	result, status := c.answerGliderFSGrep(context.Background(), map[string]interface{}{
		"pattern": `Record\(`,
		"path":    "src",
		"glob":    "**/*.ts",
	})
	if status != "complete" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	if got, want := result["files"], []string{"src/a.ts", "src/nested/b.ts"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("files=%#v want %#v", got, want)
	}
	matches, ok := result["matches"].([]map[string]interface{})
	if !ok || len(matches) != 2 {
		t.Fatalf("matches=%#v", result["matches"])
	}
	if matches[0]["file"] != "src/a.ts" || matches[0]["line"] != 1 || matches[0]["column"] != 11 || matches[0]["text"] != "const x = Record(1);" {
		t.Fatalf("first match=%#v", matches[0])
	}
	if matches[1]["file"] != "src/nested/b.ts" || matches[1]["column"] != 2 {
		t.Fatalf("second match=%#v", matches[1])
	}
	if _, ok := result["ideContext"].(map[string]interface{}); !ok {
		t.Fatalf("ideContext missing: %#v", result)
	}
}

func TestGliderFSGrepRejectsInvalidRegexAndEscapingPath(t *testing.T) {
	c := newFSGrepTestClient(t, nil, nil)
	for _, payload := range []map[string]interface{}{
		{"pattern": "["},
		{"pattern": "ok", "path": "../outside"},
	} {
		result, status := c.answerGliderFSGrep(context.Background(), payload)
		if status != "error" || stringify(result["error"]) == "" {
			t.Fatalf("payload=%#v status=%q result=%#v", payload, status, result)
		}
	}
}

func TestGliderFSGrepSkipsBinaryAndExcludedFiles(t *testing.T) {
	files := map[string][]byte{
		"now-file:/app/src/good.ts":   []byte("Record(1)"),
		"now-file:/app/src/blob.ts":   []byte("Record(2)\x00"),
		"now-file:/app/src/image.png": []byte("Record(3)"),
	}
	c := newFSGrepTestClient(t, files, nil)
	result, status := c.answerGliderFSGrep(context.Background(), map[string]interface{}{"pattern": `Record\(`, "path": "src"})
	if status != "complete" || fmt.Sprint(result["files"]) != "[src/good.ts]" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
}

func TestGliderFSGrepBoundsFilesMatchesAndLineText(t *testing.T) {
	files := make(map[string][]byte, fsGrepMaxFiles+10)
	for i := 0; i < fsGrepMaxFiles+10; i++ {
		files[fmt.Sprintf("now-file:/app/src/%03d.ts", i)] = []byte("Record(1)")
	}
	files["now-file:/app/src/many.ts"] = []byte(strings.Repeat("Record(1)\n", fsGrepMaxMatches+10))
	files["now-file:/app/src/long.ts"] = []byte(strings.Repeat("x", fsGrepMaxLineBytes+100) + "Record(1)")
	c := newFSGrepTestClient(t, files, nil)
	result, status := c.answerGliderFSGrep(context.Background(), map[string]interface{}{"pattern": `Record\(`, "path": "src", "glob": "[0-9][0-9][0-9].ts"})
	if status != "complete" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	if got := len(result["files"].([]string)); got != fsGrepMaxFiles {
		t.Fatalf("searched files=%d want %d", got, fsGrepMaxFiles)
	}
	result, status = c.answerGliderFSGrep(context.Background(), map[string]interface{}{"pattern": `Record\(`, "path": "src", "glob": "many.ts"})
	if status != "complete" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	if matches := result["matches"].([]map[string]interface{}); len(matches) != fsGrepMaxMatches {
		t.Fatalf("matches=%d want %d", len(matches), fsGrepMaxMatches)
	}
	result, status = c.answerGliderFSGrep(context.Background(), map[string]interface{}{"pattern": `Record\(`, "path": "src", "glob": "long.ts"})
	if status != "complete" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	text := result["matches"].([]map[string]interface{})[0]["text"].(string)
	if len(text) > fsGrepMaxLineBytes || !strings.HasPrefix(text, "…") {
		t.Fatalf("bounded line text=%q", text)
	}
}

func TestGliderFSGrepHonorsCancellationBeforeNetwork(t *testing.T) {
	called := false
	c := newFSGrepTestClient(t, map[string][]byte{"now-file:/app/a.ts": []byte("Record(")}, &called)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, status := c.answerGliderFSGrep(ctx, map[string]interface{}{"pattern": `Record\(`})
	if status != "error" || !strings.Contains(stringify(result["error"]), context.Canceled.Error()) || called {
		t.Fatalf("status=%q called=%v result=%#v", status, called, result)
	}
}

func newFSGrepTestClient(t *testing.T, files map[string][]byte, called *bool) *Client {
	t.Helper()
	entries := make([]map[string]interface{}, 0, len(files))
	for uri, content := range files {
		entries = append(entries, map[string]interface{}{"uri": uri, "type": "file", "size": len(content)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i]["uri"].(string) < entries[j]["uri"].(string) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if called != nil {
			*called = true
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": entries})
		case "/api/sn_glider/v2/sync/files":
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				t.Fatalf("parse multipart: %v", err)
			}
			part, _, err := r.FormFile("uris")
			if err != nil {
				t.Fatalf("uris part: %v", err)
			}
			defer part.Close()
			var uris []string
			if err := json.NewDecoder(part).Decode(&uris); err != nil {
				t.Fatalf("decode uris: %v", err)
			}
			out := make([]map[string]interface{}, 0, len(uris))
			for _, uri := range uris {
				if content, ok := files[uri]; ok {
					out = append(out, map[string]interface{}{"checksum": uri, "content": string(content)})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": out})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), currentApp: &AppScope{ScopeID: "app", AppSysID: "app"}}
}
