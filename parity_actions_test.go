package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestGliderFSCopyMoveAndFindReplaceRemoteOnly(t *testing.T) {
	files := map[string][]byte{"now-file:/app/src/a.ts": []byte("One one\n"), "now-file:/app/src/nested/b.ts": []byte("two\n")}
	server := mutableGliderServer(t, files, nil, nil, nil)
	defer server.Close()
	c := gliderTestClient(server, "app")
	result, status := c.answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "src", "destinationPath": "backup"}, false)
	if status != "complete" || !strings.Contains(stringify(result["message"]), "copied") {
		t.Fatalf("copy status=%q result=%#v", status, result)
	}
	result, status = c.answerGliderFSFindAndReplace(context.Background(), map[string]interface{}{"path": "backup/a.ts", "search": "one", "replace": "three", "replaceAll": true, "caseSensitive": false})
	if status != "complete" || result["replacements"] != 2 {
		t.Fatalf("replace status=%q result=%#v", status, result)
	}
	result, status = c.answerGliderFSCopy(context.Background(), map[string]interface{}{"oldPath": "backup", "newPath": "moved", "overwrite": false}, true)
	if status != "complete" || string(files["now-file:/app/moved/a.ts"]) != "three three\n" {
		t.Fatalf("move status=%q result=%#v files=%q", status, result, files)
	}
	if _, ok := files["now-file:/app/backup/a.ts"]; ok {
		t.Fatalf("source remains after move: %q", files)
	}
}

func TestGliderFSCopyRejectsBothOverlappingTreeDirections(t *testing.T) {
	files := map[string][]byte{"now-file:/app/src/a.ts": []byte("a"), "now-file:/app/src/child/b.ts": []byte("b")}
	server := mutableGliderServer(t, files, nil, nil, nil)
	defer server.Close()
	c := gliderTestClient(server, "app")
	for _, payload := range []map[string]interface{}{
		{"sourcePath": "src", "destinationPath": "src/child/copy", "overwrite": true},
		{"sourcePath": "src/child", "destinationPath": "src", "overwrite": true},
	} {
		result, status := c.answerGliderFSCopy(context.Background(), payload, true)
		if status != "error" || !strings.Contains(stringify(result["error"]), "overlap") {
			t.Fatalf("payload=%#v status=%q result=%#v", payload, status, result)
		}
	}
	if string(files["now-file:/app/src/a.ts"]) != "a" || string(files["now-file:/app/src/child/b.ts"]) != "b" {
		t.Fatalf("overlap attempt mutated files: %#v", files)
	}
}

func TestGliderFSCopyRejectsFileAncestorAndDescendantOverlap(t *testing.T) {
	files := map[string][]byte{"now-file:/app/a.ts": []byte("a"), "now-file:/app/dir/a.ts": []byte("b")}
	server := mutableGliderServer(t, files, nil)
	defer server.Close()
	c := gliderTestClient(server, "app")
	for _, payload := range []map[string]interface{}{
		{"sourcePath": "a.ts", "destinationPath": "a.ts/child.ts", "overwrite": true},
		{"sourcePath": "dir/a.ts", "destinationPath": "dir", "overwrite": true},
	} {
		result, status := c.answerGliderFSCopy(context.Background(), payload, false)
		if status != "error" || !strings.Contains(stringify(result["error"]), "overlap") {
			t.Fatalf("payload=%#v status=%q result=%#v", payload, status, result)
		}
	}
}

func TestGliderFSCopyDeduplicatesChecksumsUsesActualSizesAndRemovesStaleDestination(t *testing.T) {
	shared := []byte("same")
	files := map[string][]byte{
		"now-file:/app/src/a.ts":      shared,
		"now-file:/app/src/b.ts":      shared,
		"now-file:/app/dest/stale.ts": []byte("stale"),
	}
	server := mutableGliderServer(t, files, map[string]int{"now-file:/app/src/a.ts": 0, "now-file:/app/src/b.ts": 0}, nil, nil)
	defer server.Close()
	c := gliderTestClient(server, "app")
	result, status := c.answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "src", "destinationPath": "dest", "overwrite": true}, false)
	if status != "complete" || string(files["now-file:/app/dest/a.ts"]) != "same" || string(files["now-file:/app/dest/b.ts"]) != "same" {
		t.Fatalf("duplicate copy status=%q result=%#v files=%#v", status, result, files)
	}
	if _, ok := files["now-file:/app/dest/stale.ts"]; ok {
		t.Fatalf("stale overwritten destination survived: %#v", files)
	}

	large := make([]byte, fsEditMaxBytes+1)
	files["now-file:/app/large.ts"] = large
	result, status = c.answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "large.ts", "destinationPath": "large-copy.ts"}, false)
	if status != "error" || !strings.Contains(stringify(result["error"]), "safe content limit") {
		t.Fatalf("stale-size large copy status=%q result=%#v", status, result)
	}
	if _, ok := files["now-file:/app/large-copy.ts"]; ok {
		t.Fatal("large source mutated destination despite actual-size limit")
	}
}

func TestGliderFSCopyOverwriteUsesUpdateWithoutCreateRemoveCollision(t *testing.T) {
	files := map[string][]byte{
		"now-file:/app/src/a.ts":  []byte("new"),
		"now-file:/app/dest/a.ts": []byte("old"),
	}
	var applies []gliderApplyPayload
	server := mutableGliderServerWithApplyCapture(t, files, nil, &applies)
	defer server.Close()
	c := gliderTestClient(server, "app")
	result, status := c.answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "src/a.ts", "destinationPath": "dest/a.ts", "overwrite": true}, false)
	if status != "complete" || string(files["now-file:/app/dest/a.ts"]) != "new" {
		t.Fatalf("status=%q result=%#v files=%#v", status, result, files)
	}
	if len(applies) != 1 || len(applies[0].Create) != 0 || len(applies[0].Remove) != 0 || len(applies[0].Update) != 1 || applies[0].Update[0].URI != "now-file:/app/dest/a.ts" {
		t.Fatalf("unexpected overwrite multipart payloads: %#v", applies)
	}
}

func TestGliderFSCopyRecursiveOverwriteRemovesStaleBeforeCreates(t *testing.T) {
	files := map[string][]byte{
		"now-file:/app/src/a.ts":          []byte("new"),
		"now-file:/app/src/nested/b.ts":   []byte("nested"),
		"now-file:/app/dest/a.ts":         []byte("old"),
		"now-file:/app/dest/stale/old.ts": []byte("stale"),
	}
	var applies []gliderApplyPayload
	server := mutableGliderServerWithApplyCapture(t, files, nil, &applies)
	defer server.Close()
	c := gliderTestClient(server, "app")
	result, status := c.answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "src", "destinationPath": "dest", "overwrite": true}, false)
	if status != "complete" || string(files["now-file:/app/dest/nested/b.ts"]) != "nested" {
		t.Fatalf("status=%q result=%#v files=%#v", status, result, files)
	}
	if _, ok := files["now-file:/app/dest/stale/old.ts"]; ok {
		t.Fatalf("stale file survived: %#v", files)
	}
	if len(applies) != 2 || len(applies[0].Create) != 0 || len(applies[0].Update) != 0 || len(applies[0].Remove) == 0 {
		t.Fatalf("expected removal phase then creation phase, got %#v", applies)
	}
	if len(applies[1].Remove) != 0 || len(applies[1].Update) != 1 || len(applies[1].Create) == 0 {
		t.Fatalf("unexpected creation phase: %#v", applies[1])
	}
	for _, apply := range applies {
		created := map[string]bool{}
		for _, entry := range apply.Create {
			created[entry.URI] = true
		}
		for _, entry := range apply.Remove {
			if created[entry.URI] {
				t.Fatalf("multipart apply sends %s in both create and remove: %#v", entry.URI, apply)
			}
		}
	}
}

func TestGliderFSCopyRejectsExistingFileDestinationParentWithoutApply(t *testing.T) {
	files := map[string][]byte{"now-file:/app/a.ts": []byte("a"), "now-file:/app/existing-file": []byte("x")}
	var applies []gliderApplyPayload
	server := mutableGliderServerWithApplyCapture(t, files, nil, &applies)
	defer server.Close()
	c := gliderTestClient(server, "app")
	result, status := c.answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "a.ts", "destinationPath": "existing-file/child.ts"}, false)
	if status != "error" || !strings.Contains(stringify(result["error"]), "destination parent is a file") || len(applies) != 0 {
		t.Fatalf("status=%q result=%#v applies=%#v", status, result, applies)
	}
}

func TestGliderFSCopyRejectsMislabeledSourceBlobWithoutApply(t *testing.T) {
	files := map[string][]byte{"now-file:/app/src/a.ts": []byte("expected")}
	capture := &gliderApplyCapture{}
	server := mutableGliderServer(t, files, nil, capture, gliderSyncContentOverride{"now-file:/app/src/a.ts": []byte("wrong")})
	defer server.Close()
	result, status := gliderTestClient(server, "app").answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "src/a.ts", "destinationPath": "dest/a.ts"}, false)
	if status != "error" || !strings.Contains(stringify(result["error"]), "checksum mismatch") || !strings.Contains(stringify(result["error"]), "src/a.ts") || len(capture.applies) != 0 {
		t.Fatalf("status=%q result=%#v applies=%#v", status, result, capture.applies)
	}
}

func TestGliderFSMoveKeepsSourceWhenDestinationWriteFails(t *testing.T) {
	files := map[string][]byte{"now-file:/app/src/a.ts": []byte("source")}
	capture := &gliderApplyCapture{}
	server := mutableGliderServer(t, files, nil, capture, gliderApplyFailure(func(apply int) bool { return apply == 1 }))
	defer server.Close()
	result, status := gliderTestClient(server, "app").answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "src/a.ts", "destinationPath": "dest/a.ts"}, true)
	if status != "error" || string(files["now-file:/app/src/a.ts"]) != "source" {
		t.Fatalf("status=%q result=%#v files=%#v", status, result, files)
	}
	if _, exists := files["now-file:/app/dest/a.ts"]; exists {
		t.Fatalf("failed move wrote destination: %#v", files)
	}
	if len(capture.applies) != 1 || !hasGliderURI(capture.applies[0].Remove, "now-file:/app/src/a.ts") {
		t.Fatalf("move did not attempt source removal only with destination write: %#v", capture.applies)
	}
}

func TestGliderFSMoveWritesDestinationAndRemovesSourceInSameApply(t *testing.T) {
	files := map[string][]byte{"now-file:/app/src/a.ts": []byte("source")}
	capture := &gliderApplyCapture{}
	server := mutableGliderServer(t, files, nil, capture)
	defer server.Close()
	result, status := gliderTestClient(server, "app").answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "src/a.ts", "destinationPath": "dest/a.ts"}, true)
	if status != "complete" || string(files["now-file:/app/dest/a.ts"]) != "source" {
		t.Fatalf("status=%q result=%#v files=%#v", status, result, files)
	}
	if _, exists := files["now-file:/app/src/a.ts"]; exists {
		t.Fatalf("source remains after move: %#v", files)
	}
	if len(capture.applies) != 1 || !hasGliderURI(capture.applies[0].Create, "now-file:/app/dest/a.ts") || !hasGliderURI(capture.applies[0].Remove, "now-file:/app/src/a.ts") {
		t.Fatalf("move destination write and source removal were not one apply: %#v", capture.applies)
	}
}

func TestGliderFSCopyOverwriteUsesUpdateAndRetainsMatchingDirectory(t *testing.T) {
	t.Run("existing file becomes update", func(t *testing.T) {
		files := map[string][]byte{"now-file:/app/src/a.ts": []byte("new"), "now-file:/app/dest/a.ts": []byte("old")}
		capture := &gliderApplyCapture{}
		server := mutableGliderServer(t, files, nil, nil, capture)
		defer server.Close()
		result, status := gliderTestClient(server, "app").answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "src/a.ts", "destinationPath": "dest/a.ts", "overwrite": true}, false)
		if status != "complete" || !hasGliderURI(capture.update, "now-file:/app/dest/a.ts") || hasGliderURI(capture.create, "now-file:/app/dest/a.ts") || hasGliderURI(capture.remove, "now-file:/app/dest/a.ts") {
			t.Fatalf("result=%#v status=%q apply=%+v", result, status, capture)
		}
	})
	t.Run("matching directory retained and stale child removed", func(t *testing.T) {
		files := map[string][]byte{"now-file:/app/src/new.ts": []byte("new"), "now-file:/app/dest/stale.ts": []byte("old")}
		capture := &gliderApplyCapture{}
		server := mutableGliderServer(t, files, nil, []gliderChangeEntry{{URI: "now-file:/app/dest", Type: "dir"}}, capture)
		defer server.Close()
		result, status := gliderTestClient(server, "app").answerGliderFSCopy(context.Background(), map[string]interface{}{"sourcePath": "src", "destinationPath": "dest", "overwrite": true}, false)
		if status != "complete" || hasGliderURI(capture.create, "now-file:/app/dest") || hasGliderURI(capture.update, "now-file:/app/dest") || hasGliderURI(capture.remove, "now-file:/app/dest") || !hasGliderURI(capture.remove, "now-file:/app/dest/stale.ts") {
			t.Fatalf("result=%#v status=%q apply=%+v", result, status, capture)
		}
	})
}

func hasGliderURI(entries []gliderChangeEntry, uri string) bool {
	for _, entry := range entries {
		if entry.URI == uri {
			return true
		}
	}
	return false
}

func TestGliderFSCopyGuardsXMLCollisionEscapingAndCancellation(t *testing.T) {
	files := map[string][]byte{"now-file:/app/src/a.ts": []byte("a"), "now-file:/app/dest/blocked.xml": []byte("x")}
	server := mutableGliderServer(t, files, nil, nil, nil)
	defer server.Close()
	c := gliderTestClient(server, "app")
	for _, payload := range []map[string]interface{}{{"sourcePath": "../bad", "destinationPath": "x"}, {"sourcePath": "src", "destinationPath": "dest", "overwrite": true}, {"sourcePath": "src/a.ts", "destinationPath": "x.xml"}} {
		result, status := c.answerGliderFSCopy(context.Background(), payload, false)
		if status != "error" {
			t.Fatalf("payload=%#v status=%q result=%#v", payload, status, result)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, status := c.answerGliderFSCopy(ctx, map[string]interface{}{"sourcePath": "src/a.ts", "destinationPath": "z.ts"}, false)
	if status != "error" || !strings.Contains(stringify(result["error"]), context.Canceled.Error()) {
		t.Fatalf("cancel status=%q result=%#v", status, result)
	}
}

func TestFindReplaceWhitespaceAndLiteralReplacement(t *testing.T) {
	files := map[string][]byte{"now-file:/app/a.ts": []byte("a  b  c")}
	server := mutableGliderServer(t, files, nil, nil, nil)
	defer server.Close()
	c := gliderTestClient(server, "app")
	result, status := c.answerGliderFSFindAndReplace(context.Background(), map[string]interface{}{"path": "a.ts", "search": "  ", "replace": "$1", "replaceAll": true})
	if status != "complete" || string(files["now-file:/app/a.ts"]) != "a$1b$1c" {
		t.Fatalf("whitespace/literal replacement status=%q result=%#v content=%q", status, result, files["now-file:/app/a.ts"])
	}
	for _, payload := range []map[string]interface{}{{"path": "x.xml", "search": "a", "replace": "b"}, {"path": "a.ts", "search": "", "replace": "b"}} {
		result, status = c.answerGliderFSFindAndReplace(context.Background(), payload)
		if status != "error" {
			t.Fatalf("replace payload=%#v status=%q result=%#v", payload, status, result)
		}
	}
}

func TestUIAndMCPCompatibilityLimitationsDoNotLeakPayloadSecrets(t *testing.T) {
	c := &Client{currentApp: &AppScope{AppSysID: "app"}}
	result, status := c.answerUIDiagnostics(map[string]interface{}{"path": "/nav_to.do", "authorization": "secret"})
	if status != "error" || result["code"] != "UI_DIAGNOSTICS_UNAVAILABLE" || strings.Contains(fmt.Sprint(result), "secret") {
		t.Fatalf("ui result=%#v status=%q", result, status)
	}
	result, status = c.answerMCPManagement(context.Background(), "connect_to_mcp_server", map[string]interface{}{"serverId": "x", "name": "X", "url": "https://example", "transport": "sse", "token": "secret"})
	if status != "error" || result["code"] != "MCP_CLIENT_TRANSPORT_UNAVAILABLE" || strings.Contains(fmt.Sprint(result), "secret") {
		t.Fatalf("mcp result=%#v status=%q", result, status)
	}
	result, status = c.answerMCPManagement(context.Background(), "list_mcp_servers", nil)
	if status != "complete" || fmt.Sprint(result["connected"]) != "[]" {
		t.Fatalf("mcp list result=%#v status=%q", result, status)
	}
}

func TestOpenAppValidatesMetadataBeforeChangingState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/now/table/sys_app/good" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"result":{"sys_id":"good","name":"Authoritative","scope":"x_demo"}}`)
	}))
	defer server.Close()
	c, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient = server.Client()
	c.currentApp = &AppScope{AppSysID: "old", ScopeID: "old", ScopeName: "Old"}
	result, status, err := c.answerOpenApp(context.Background(), map[string]interface{}{"appId": "good", "appName": "untrusted"})
	if err != nil || status != "complete" || c.currentApp.AppSysID != "good" || c.currentApp.ScopeName != "Authoritative" || result["appName"] != "Authoritative" {
		t.Fatalf("result=%#v status=%q app=%#v err=%v", result, status, c.currentApp, err)
	}
	result, status, err = c.answerOpenApp(context.Background(), map[string]interface{}{"appId": "missing"})
	if err != nil || status != "error" || result["code"] != "APP_LOOKUP_FAILED" || c.currentApp.AppSysID != "good" {
		t.Fatalf("failure result=%#v status=%q app=%#v err=%v", result, status, c.currentApp, err)
	}
}

func TestOpenAppRestoresInMemoryStateWhenPersistenceFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, stateDirName), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"result":{"sys_id":"good","name":"Authoritative","scope":"x_demo"}}`)
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "test"}, httpClient: server.Client()}
	c.currentApp = &AppScope{AppSysID: "old", ScopeID: "old", ScopeName: "Old"}
	c.appScope = "old"
	c.setStatusProjectPath("/old/project")
	_, status, err := c.answerOpenApp(context.Background(), map[string]interface{}{"appId": "good"})
	if err == nil || status != "error" || c.currentApp.AppSysID != "old" || c.appScope != "old" || c.cachedStatusProjectPath() != "/old/project" {
		t.Fatalf("status=%q err=%v app=%#v appScope=%#v project=%q", status, err, c.currentApp, c.appScope, c.cachedStatusProjectPath())
	}
}

func TestParityActionsDispatchWithoutUnexpectedClientAction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	responses := make(chan map[string]interface{}, 9)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer conn.Close()
			for i := 0; i < 9; i++ {
				var response map[string]interface{}
				if err := conn.ReadJSON(&response); err != nil {
					t.Errorf("read response: %v", err)
					return
				}
				responses <- response
			}
			return
		}
		if r.URL.Path == "/api/now/table/sys_app/good" {
			_, _ = io.WriteString(w, `{"result":{"sys_id":"good","name":"Good","scope":"x_good"}}`)
			return
		}
		if r.URL.Path == "/api/now/ai/mcp/servers" {
			_, _ = io.WriteString(w, `{"result":{"result":{"servers":[]}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "parity-dispatch-test"}, httpClient: server.Client(), conn: conn, currentApp: &AppScope{AppSysID: "app", ScopeID: "app"}}
	c.beginActiveTurn(context.Background())
	defer c.endActiveTurn()
	actions := []struct {
		action  string
		payload map[string]interface{}
		code    string
	}{
		{"fs_copy", map[string]interface{}{}, "TOOL_ERROR"},
		{"fs_move", map[string]interface{}{}, "TOOL_ERROR"},
		{"fs_find_and_replace", map[string]interface{}{}, "TOOL_ERROR"},
		{"open_app", map[string]interface{}{"appId": "good"}, ""},
		{"ui_diagnostics", map[string]interface{}{"path": "/"}, "UI_DIAGNOSTICS_UNAVAILABLE"},
		{"connect_to_mcp_server", map[string]interface{}{"serverId": "x", "name": "X", "url": "https://example", "transport": "sse"}, "MCP_CLIENT_TRANSPORT_UNAVAILABLE"},
		{"disconnect_from_mcp_server", map[string]interface{}{"serverId": "x"}, "MCP_CLIENT_TRANSPORT_UNAVAILABLE"},
		{"list_mcp_servers", map[string]interface{}{}, ""},
		{"list_mcp_tools", map[string]interface{}{}, ""},
	}
	for i, action := range actions {
		if err := c.handleElicitation(map[string]interface{}{"elicitation_id": fmt.Sprintf("e-%d", i), "action": action.action, "payload": action.payload}); err != nil {
			t.Fatalf("action=%s dispatch error: %v", action.action, err)
		}
		select {
		case response := <-responses:
			result := asMap(response["result"])
			errorResult := asMap(result["error"])
			if result == nil || stringify(errorResult["code"]) == "UNEXPECTED_CLIENT_ACTION" {
				t.Fatalf("action=%s response=%#v", action.action, response)
			}
			if action.code != "" && stringify(errorResult["code"]) != action.code {
				t.Fatalf("action=%s response=%#v, want code=%s", action.action, response, action.code)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s response", action.action)
		}
	}
}

func gliderTestClient(server *httptest.Server, app string) *Client {
	return &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), currentApp: &AppScope{AppSysID: app, ScopeID: app}}
}

type gliderApplyCapture struct {
	create  []gliderChangeEntry
	update  []gliderChangeEntry
	remove  []gliderChangeEntry
	applies []gliderApplyPayload
	sink    *[]gliderApplyPayload
}

type gliderApplyPayload struct {
	Create []gliderChangeEntry
	Update []gliderChangeEntry
	Remove []gliderChangeEntry
}

type gliderApplyFailure func(int) bool

// gliderSyncContentOverride returns intentionally mismatched bytes under the
// checksum advertised by state, exercising client-side blob validation.
type gliderSyncContentOverride map[string][]byte

func mutableGliderServer(t *testing.T, files map[string][]byte, stateSizes map[string]int, options ...interface{}) *httptest.Server {
	t.Helper()
	var extraEntries []gliderChangeEntry
	var capture *gliderApplyCapture
	var failApply gliderApplyFailure
	var syncContentOverride gliderSyncContentOverride
	for _, option := range options {
		switch typed := option.(type) {
		case []gliderChangeEntry:
			extraEntries = typed
		case *gliderApplyCapture:
			capture = typed
		case gliderApplyFailure:
			failApply = typed
		case gliderSyncContentOverride:
			syncContentOverride = typed
		}
	}
	applyCount := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			directories := map[string]bool{}
			for _, entry := range extraEntries {
				if entry.Type == "dir" || entry.Type == "directory" {
					directories[entry.URI] = true
				}
			}
			for uri := range files {
				for parent := gliderURIParent(uri); parent != "now-file:/app" && parent != ""; parent = gliderURIParent(parent) {
					directories[parent] = true
				}
			}
			entries := make([]map[string]interface{}, 0, len(files)+len(directories))
			for uri := range directories {
				entries = append(entries, map[string]interface{}{"uri": uri, "type": "dir"})
			}
			for uri, content := range files {
				size := len(content)
				if stateSizes != nil {
					if override, ok := stateSizes[uri]; ok {
						size = override
					}
				}
				entries = append(entries, map[string]interface{}{"uri": uri, "checksum": sha1Hex(content), "type": "file", "size": size})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": entries})
		case "/api/sn_glider/v2/sync/files":
			if err := r.ParseMultipartForm(10 << 20); err != nil {
				t.Fatal(err)
			}
			part, _, err := r.FormFile("uris")
			if err != nil {
				t.Fatal(err)
			}
			defer part.Close()
			var uris []string
			_ = json.NewDecoder(part).Decode(&uris)
			out := make([]map[string]interface{}, 0, len(uris))
			for _, uri := range uris {
				if content, ok := files[uri]; ok {
					responseContent := content
					if overridden, ok := syncContentOverride[uri]; ok {
						responseContent = overridden
					}
					out = append(out, map[string]interface{}{"checksum": sha1Hex(content), "content": string(responseContent)})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": out})
		case "/api/sn_glider/v2/sync/changes/apply":
			if err := r.ParseMultipartForm(10 << 20); err != nil {
				t.Fatal(err)
			}
			var create, update, remove []gliderChangeEntry
			for _, item := range []struct {
				name   string
				target *[]gliderChangeEntry
			}{{"create", &create}, {"update", &update}, {"remove", &remove}} {
				part, _, err := r.FormFile(item.name)
				if err != nil {
					t.Fatal(err)
				}
				_ = json.NewDecoder(part).Decode(item.target)
				_ = part.Close()
			}
			if capture != nil {
				capture.create = append(capture.create, create...)
				capture.update = append(capture.update, update...)
				capture.remove = append(capture.remove, remove...)
				capture.applies = append(capture.applies, gliderApplyPayload{Create: append([]gliderChangeEntry(nil), create...), Update: append([]gliderChangeEntry(nil), update...), Remove: append([]gliderChangeEntry(nil), remove...)})
				if capture.sink != nil {
					*capture.sink = append(*capture.sink, capture.applies[len(capture.applies)-1])
				}
			}
			applyCount++
			if failApply != nil && failApply(applyCount) {
				http.Error(w, "injected apply failure", http.StatusInternalServerError)
				return
			}
			for _, entry := range remove {
				delete(files, entry.URI)
			}
			create = append(create, update...)
			for _, entry := range create {
				if entry.Type != "file" {
					continue
				}
				part, _, err := r.FormFile(entry.Checksum)
				if err != nil {
					t.Fatal(err)
				}
				content, err := io.ReadAll(part)
				if err != nil {
					t.Fatal(err)
				}
				_ = part.Close()
				files[entry.URI] = content
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func mutableGliderServerWithApplyCapture(t *testing.T, files map[string][]byte, stateSizes map[string]int, applies *[]gliderApplyPayload) *httptest.Server {
	return mutableGliderServer(t, files, stateSizes, nil, &gliderApplyCapture{sink: applies})
}
