package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestNirvanaCapabilitiesMatchStreamingWebClient(t *testing.T) {
	c := &Client{opts: Options{Nirvana: true}}
	caps := c.clientCapabilities()

	streaming := asMap(caps["streaming"])
	if streaming == nil || streaming["receive"] != true {
		t.Fatalf("streaming capability = %#v, want receive=true", caps["streaming"])
	}
	tools := asMap(caps["tools"])
	if tools == nil || tools["execute"] != true {
		t.Fatalf("tools capability = %#v, want execute=true", caps["tools"])
	}
	keywordSearch := asMap(caps["keyword_search"])
	if keywordSearch == nil || keywordSearch["preview_available"] != true {
		t.Fatalf("keyword_search capability = %#v, want preview_available=true", caps["keyword_search"])
	}
	for _, key := range []string{"client_ide", "elicitation", "fluent_docs", "glob_and_grep", "semantic_search", "server_tools", "sub_agents"} {
		if _, ok := caps[key]; !ok {
			t.Fatalf("missing Nirvana web-client capability %q in %#v", key, caps)
		}
	}
	wantKeys := []string{"client_ide", "elicitation", "fluent_docs", "glob_and_grep", "interview_choice_picker", "keyword_search", "plan_approval", "product_availability", "semantic_search", "server_tools", "streaming", "sub_agents", "tools"}
	if len(caps) != len(wantKeys) {
		t.Fatalf("Nirvana capability count = %d, want %d: %#v", len(caps), len(wantKeys), caps)
	}
	for _, key := range wantKeys {
		if _, ok := caps[key]; !ok {
			t.Fatalf("missing HAR-observed capability %q", key)
		}
	}

	defaultCaps := (&Client{}).clientCapabilities()
	if _, ok := defaultCaps["tools"]; ok {
		t.Fatalf("default web gateway capabilities should not advertise tools.execute: %#v", defaultCaps["tools"])
	}
}

func TestDialWebSocketCancelableInterruptsUpgradeHandshake(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := dialWebSocketCancelable(ctx, websocket.Dialer{HandshakeTimeout: 30 * time.Second}, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("websocket handshake did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled websocket dial error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled websocket handshake did not return promptly")
	}
}

func TestNirvanaCapabilitiesExcludeUnobservedExtensionOnlyKeys(t *testing.T) {
	caps := (&Client{opts: Options{Nirvana: true}}).clientCapabilities()
	for _, key := range []string{"change_log", "working_set", "app_picker", "atf_with_app", "memfs"} {
		if _, ok := caps[key]; ok {
			t.Fatalf("unexpected non-HAR Nirvana capability %q in %#v", key, caps)
		}
	}
}

func TestGatewayStreamUpdateKeepsWorkingStatusActive(t *testing.T) {
	c := &Client{webStreamText: "hello", turnStatusActive: true}
	out := captureStdout(t, func() {
		c.printGatewayStreamUpdate("hello")
	})
	if !c.turnStatusActive {
		t.Fatalf("stream update should keep Working status active until turn_end")
	}
	if out != "hello" {
		t.Fatalf("stdout = %q, want streamed chunk", out)
	}
}

func TestElicitationResponseRequestsWorkingStatusDuringActiveTurn(t *testing.T) {
	c := &Client{processing: true, turnDone: make(chan error, 1), turnStatusActive: true}
	if !c.clearTurnStatus() {
		t.Fatalf("clearTurnStatus should report that Working was active")
	}
	if c.turnStatusActive {
		t.Fatalf("clearTurnStatus should mark Working inactive while a blocking prompt owns the terminal")
	}
	if !c.ensureTurnStatusVisible() {
		t.Fatalf("elicitation response should request Working redraw while the turn is still processing")
	}

	c.processing = false
	if c.ensureTurnStatusVisible() {
		t.Fatalf("Working redraw must not be requested after the turn is no longer processing")
	}
}

func TestRecordActiveStreamTranscriptDeltaIsIdempotent(t *testing.T) {
	terminalTranscript.Lock()
	previousEntries := terminalTranscript.entries
	previousActive := terminalTranscript.activeAssistant
	terminalTranscript.entries = nil
	terminalTranscript.activeAssistant = ""
	terminalTranscript.Unlock()
	defer func() {
		terminalTranscript.Lock()
		terminalTranscript.entries = previousEntries
		terminalTranscript.activeAssistant = previousActive
		terminalTranscript.Unlock()
	}()

	c := &Client{webStreamText: "first"}
	c.recordActiveStreamTranscriptDelta()
	c.recordActiveStreamTranscriptDelta()
	c.webStreamText = "first second"
	c.recordActiveStreamTranscriptDelta()

	entries := terminalTranscriptSnapshot()
	if len(entries) != 2 || entries[0].Text != "first" || entries[1].Text != "second" {
		t.Fatalf("unexpected stream transcript entries: %#v", entries)
	}
}

func TestCancelActiveTurnReleasesProcessingButKeepsCancellationLatched(t *testing.T) {
	c := &Client{
		conversationID:     "conversation-1",
		processing:         true,
		pendingUserContent: "cancel me",
		turnDone:           make(chan error, 1),
	}
	turnCtx := c.beginActiveTurn(context.Background())
	if !c.cancelActiveTurn() {
		t.Fatal("expected active turn cancellation")
	}
	if c.processing {
		t.Fatal("processing remained true after cancellation")
	}
	if c.pendingUserContent != "" {
		t.Fatalf("pendingUserContent = %q, want empty", c.pendingUserContent)
	}
	if !c.activeTurnCancelled() || turnCtx.Err() == nil {
		t.Fatal("cancelled turn was not kept latched")
	}
}

func TestCancelledTurnEventsStayIgnoredAfterNextTurnBegins(t *testing.T) {
	c := &Client{cancelledServerTurnIDs: map[string]struct{}{"turn-old": {}}}
	if !c.shouldIgnoreCancelledTurnEvent(map[string]interface{}{"type": "stream_delta", "turn_id": "turn-old"}) {
		t.Fatal("late event from cancelled turn was accepted")
	}
	if c.shouldIgnoreCancelledTurnEvent(map[string]interface{}{"type": "stream_delta", "turn_id": "turn-new"}) {
		t.Fatal("event from new turn was incorrectly ignored")
	}
}

func TestStreamingTableCommitsOnlyStablePrefixUntilFinal(t *testing.T) {
	partial := "Intro before table.\n\n| # | Message |\n| --- | --- |\n| 1 | short |\n| 2 | much wider table cell still streaming"
	if !streamContainsMarkdownTable(partial) {
		t.Fatalf("expected table detection")
	}
	if got := stableAssistantStreamPrefix(partial); got != "Intro before table." {
		t.Fatalf("stable prefix while table streams = %q", got)
	}
	complete := partial + " |\n\nAfter table."
	if got := stableAssistantStreamPrefix(complete); !strings.Contains(got, "much wider table cell") || strings.Contains(got, "After table") {
		t.Fatalf("stable prefix after table completion = %q", got)
	}
}

func TestNirvanaStreamDeltaOnlyPrintsAssistantText(t *testing.T) {
	c := &Client{streamTypes: map[string]string{}, debug: false}
	out := captureStdout(t, func() {
		_ = c.handleEvent([]byte(`{"type":"stream_start","stream_id":"p1","content_type":"pending"}`))
		_ = c.handleEvent([]byte(`{"type":"stream_delta","stream_id":"p1","delta":"Looking..."}`))
		_ = c.handleEvent([]byte(`{"type":"stream_start","stream_id":"t1","content_type":"thinking"}`))
		_ = c.handleEvent([]byte(`{"type":"stream_delta","stream_id":"t1","delta":"private reasoning"}`))
		_ = c.handleEvent([]byte(`{"type":"stream_start","stream_id":"x1","content_type":"text"}`))
		_ = c.handleEvent([]byte(`{"type":"stream_delta","stream_id":"x1","delta":"Hello "}`))
		_ = c.handleEvent([]byte(`{"type":"stream_delta","stream_id":"x1","delta":"**42**"}`))
	})
	if out != "Hello **42**" {
		t.Fatalf("stdout = %q, want only assistant text deltas", out)
	}
	if c.webStreamText != "Hello **42**" {
		t.Fatalf("webStreamText = %q", c.webStreamText)
	}
}

func TestNirvanaRoutineEventsStayQuietInNormalUI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := &Client{streamTypes: map[string]string{}, opts: Options{Profile: "default", Nirvana: true}}
	stderr := captureStderr(t, func() {
		_ = c.handleEvent([]byte(`{"type":"turn_start"}`))
		_ = c.handleEvent([]byte(`{"type":"tool_call","name":"MCP_Script_Runner__run_script","call_id":"abc"}`))
		_ = c.handleEvent([]byte(`{"type":"tool_result","call_id":"abc","success":true}`))
		_ = c.handleEvent([]byte(`{"type":"stream_start","stream_id":"thinking","content_type":"thinking"}`))
		_ = c.handleEvent([]byte(`{"type":"stream_delta","stream_id":"thinking","delta":"private"}`))
		_ = c.handleEvent([]byte(`{"type":"stream_end","stream_id":"thinking"}`))
		_ = c.handleEvent([]byte(`{"type":"turn_end"}`))
	})
	for _, noisy := range []string{"turn started", "turn ended", "tool call", "tool result", "Finished thinking"} {
		if strings.Contains(stderr, noisy) {
			t.Fatalf("normal UI leaked %q in stderr: %q", noisy, stderr)
		}
	}
	if !strings.Contains(stderr, "✓ MCP_Script_Runner__run_script") {
		t.Fatalf("normal UI should show concise successful tool result, stderr=%q", stderr)
	}
}

func TestNirvanaToolResultUsesStoredNameAndFailureMarker(t *testing.T) {
	c := &Client{toolCallNames: map[string]string{}}
	name, callID := c.recordToolCall(map[string]interface{}{
		"type":    "tool_call",
		"name":    "run_script",
		"call_id": "abc",
		"server":  "MCP Script Runner",
	})
	if name != "MCP Script Runner/run_script" || callID != "abc" {
		t.Fatalf("unexpected tool call tracking: name=%q callID=%q", name, callID)
	}
	name, callID, success, summary := c.recordToolResult(map[string]interface{}{
		"type":    "tool_result",
		"call_id": "abc",
		"success": false,
	})
	if name != "MCP Script Runner/run_script" || callID != "abc" || success || summary != "" {
		t.Fatalf("unexpected tool result tracking: name=%q callID=%q success=%v summary=%q", name, callID, success, summary)
	}
	if got := formatToolResultTerminal(name, success, false); got != "✗ MCP Script Runner/run_script" {
		t.Fatalf("unexpected tool result line: %q", got)
	}
}

func TestNirvanaToolResultSummaryFromNestedJSON(t *testing.T) {
	c := &Client{toolCallNames: map[string]string{"abc": "fs_write_file"}}
	name, _, success, summary := c.recordToolResult(map[string]interface{}{
		"type":    "tool_result",
		"call_id": "abc",
		"success": true,
		"result":  `{"success":true,"result":{"message":"Successfully wrote to src/fluent/demo.now.ts","path":"src/fluent/demo.now.ts"}}`,
	})
	if name != "fs_write_file" || !success || summary != "Successfully wrote to src/fluent/demo.now.ts" {
		t.Fatalf("name=%q success=%v summary=%q", name, success, summary)
	}
	if got := toolResultDisplay(name, summary); got != "fs_write_file\nSuccessfully wrote to src/fluent/demo.now.ts" {
		t.Fatalf("display = %q", got)
	}
}

func TestStatusBarStateCountsInputsAndCumulativeUsage(t *testing.T) {
	c := &Client{
		cfg:           CLIConfig{InstanceURL: "https://demo.example.com"},
		runtime:       RuntimeModelConfig{LargeModel: "claude-opus-4-6"},
		workspaceName: "Default - admin",
		currentApp:    &AppScope{ScopeID: "x_demo", ScopeName: "Demo App", AppSysID: "appsysid123"},
		history: []interface{}{
			map[string]interface{}{"role": "user", "content": "first"},
			map[string]interface{}{"role": "assistant", "content": "answer"},
			map[string]interface{}{"role": "assistant-tool", "content": "hidden"},
			map[string]interface{}{"sender": "user", "text": "second"},
		},
	}
	stderr := captureStderr(t, func() {
		c.recordUsage(map[string]interface{}{"input_tokens": float64(4007), "output_tokens": "81"})
		c.recordUsage(map[string]interface{}{"input_tokens": json.Number("3"), "output_tokens": 4})
	})
	if !strings.Contains(stderr, "input_tokens=4007 output_tokens=81") {
		t.Fatalf("non-interactive usage line missing first usage: %q", stderr)
	}
	state := c.statusBarState()
	if state.Model != "claude-opus-4-6" || state.InputMessages != 2 || state.InputTokens != 4010 || state.OutputTokens != 85 || state.Workspace != "Default - admin" || state.App != "Demo App" || state.Instance != "https://demo.example.com" {
		t.Fatalf("unexpected status state: %#v", state)
	}
}

func TestStatusBarStateUsesCachedProjectPath(t *testing.T) {
	c := &Client{
		cfg:        CLIConfig{InstanceURL: "https://demo.example.com"},
		runtime:    RuntimeModelConfig{LargeModel: "model"},
		currentApp: &AppScope{ScopeID: "app", ScopeName: "Demo", AppSysID: "app"},
	}
	c.setStatusProjectPath("/cached/project")
	if got := c.statusBarState().Project; got != "/cached/project" {
		t.Fatalf("status project = %q, want cached path", got)
	}
	c.setStatusProjectPath("")
	if got := c.statusBarState().Project; got != "" {
		t.Fatalf("status project after clear = %q", got)
	}
}

func TestAnswerSetAppScopeRunsOneNonFatalPostSelectionStatusCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	c.postSelectionStatus = func(context.Context) error { checks++; return errors.New("offline") }
	result, status, err := c.answerSetAppScope(map[string]interface{}{"scopeId": "app-id", "scopeName": "Demo"})
	if err != nil || status != "complete" || result["success"] != true {
		t.Fatalf("answerSetAppScope result=%#v status=%q err=%v", result, status, err)
	}
	if checks != 1 {
		t.Fatalf("post-selection status checks = %d, want 1", checks)
	}
}

func TestEmptyWebUIApprovalAutoApproves(t *testing.T) {
	c := &Client{}
	result, status, err := c.answerApproval("approval", map[string]interface{}{})
	if err != nil {
		t.Fatalf("answerApproval error = %v", err)
	}
	if status != "complete" || result["approved"] != true {
		t.Fatalf("empty web approval should auto-approve, status=%q result=%#v", status, result)
	}
}

func TestEmptyWebUIApprovalOnlyMatchesApprovalAction(t *testing.T) {
	if emptyWebUIApproval("plan_approval", map[string]interface{}{}) {
		t.Fatalf("empty plan_approval must not be treated as WebUI tool approval")
	}
	if emptyWebUIApproval("approval", map[string]interface{}{"message": "Approve?"}) {
		t.Fatalf("non-empty approval payload must remain interactive")
	}
}

func TestNirvanaFSReadDirectoryRequiresActiveApp(t *testing.T) {
	c := &Client{}
	result, status := c.answerFSReadDirectory(map[string]interface{}{"path": "."})
	if status != "error" {
		t.Fatalf("status = %q, want error", status)
	}
	msg := stringify(result["content"])
	if !strings.Contains(msg, "application") || stringify(result["code"]) != "TOOL_ERROR" {
		t.Fatalf("unexpected fs_read_directory result: %#v", result)
	}
}

func TestResolveGliderPathAcceptsObservedShapes(t *testing.T) {
	c := &Client{currentApp: &AppScope{ScopeID: "appsysid", ScopeName: "Demo", AppSysID: "appsysid"}}
	cases := map[string]string{
		".":                         "now-file:/appsysid",
		"src/fluent/table.now.ts":   "now-file:/appsysid/src/fluent/table.now.ts",
		"appsysid/src/fluent/a.ts":  "now-file:/appsysid/src/fluent/a.ts",
		"/appsysid/src/fluent/a.ts": "now-file:/appsysid/src/fluent/a.ts",
		"now-file:/appsysid/foo.ts": "now-file:/appsysid/foo.ts",
	}
	for input, wantURI := range cases {
		gotURI, _, err := c.resolveGliderPath(input)
		if err != nil {
			t.Fatalf("resolveGliderPath(%q) error = %v", input, err)
		}
		if gotURI != wantURI {
			t.Fatalf("resolveGliderPath(%q) uri = %q, want %q", input, gotURI, wantURI)
		}
	}
}

func TestCurrentIDEContextUsesStringWorkspaceFolders(t *testing.T) {
	c := &Client{
		currentApp: &AppScope{ScopeID: "appsysid123", ScopeName: "Geronimo", Scope: "x_snc_geronimo_2", AppSysID: "appsysid123"},
		workspaceFolders: []WebWorkspaceFolder{
			{Name: "Geronimo", URI: "now-file:/appsysid123"},
			{Name: "Other", URI: "now-file:/otherapp"},
		},
	}
	ctx := c.currentIDEContext()
	folders, ok := ctx["workspaceFolders"].([]string)
	if !ok {
		t.Fatalf("workspaceFolders = %#v, want []string", ctx["workspaceFolders"])
	}
	if len(folders) != 1 || folders[0] != "/appsysid123" {
		t.Fatalf("workspaceFolders = %#v, want active app path only", folders)
	}
	if ctx["currentDir"] != "/appsysid123" {
		t.Fatalf("currentDir = %#v", ctx["currentDir"])
	}
	if ctx["scopeName"] != "x_snc_geronimo_2" {
		t.Fatalf("scopeName = %#v", ctx["scopeName"])
	}
}

func TestSendMessageUsesCurrentIDEContextForActiveApp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile := "payload-test"
	if err := saveCachedToken(profile, TokenResponse{AccessToken: "header.payload.sig", IssuedAt: time.Now().UnixMilli(), ExpiresIn: 3600, InstanceURL: "https://demo.service-now.com"}); err != nil {
		t.Fatal(err)
	}

	received := make(chan map[string]interface{}, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		var payload map[string]interface{}
		if err := conn.ReadJSON(&payload); err != nil {
			t.Errorf("read payload: %v", err)
			return
		}
		received <- payload
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	c := &Client{
		cfg:        CLIConfig{InstanceURL: "https://demo.service-now.com"},
		opts:       Options{Nirvana: true, CodeAssistWS: true, Profile: profile},
		conn:       conn,
		currentApp: &AppScope{ScopeID: "appsysid123", ScopeName: "Geronimo", AppSysID: "appsysid123"},
	}
	if err := c.SendMessage(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}

	var payload map[string]interface{}
	select {
	case payload = <-received:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket payload")
	}
	ctx := asMap(payload["ideContext"])
	if ctx == nil {
		t.Fatalf("ideContext missing from payload: %#v", payload)
	}
	folders, ok := ctx["workspaceFolders"].([]interface{})
	if !ok || len(folders) != 1 || folders[0] != "/appsysid123" {
		t.Fatalf("workspaceFolders = %#v, want active app path", ctx["workspaceFolders"])
	}
	if ctx["currentDir"] != "/appsysid123" {
		t.Fatalf("currentDir = %#v", ctx["currentDir"])
	}
}

func TestNirvanaOutboundAppScopeUsesObjectShape(t *testing.T) {
	c := &Client{currentApp: &AppScope{ScopeID: "appsysid123", ScopeName: "Geronimo", AppSysID: "appsysid123"}}
	got, ok := c.nirvanaOutboundAppScope()
	if !ok {
		t.Fatalf("nirvana appScope missing")
	}
	if got["scopeId"] != "appsysid123" || got["scopeName"] != "Geronimo" || got["appSysId"] != "appsysid123" {
		t.Fatalf("unexpected appScope object: %#v", got)
	}
}

func TestNirvanaOutboundAppScopeConvertsLegacyString(t *testing.T) {
	c := &Client{appScope: "legacyappsysid"}
	got, ok := c.nirvanaOutboundAppScope()
	if !ok {
		t.Fatalf("legacy string appScope should be converted")
	}
	if got["scopeId"] != "legacyappsysid" || got["appSysId"] != "legacyappsysid" {
		t.Fatalf("unexpected converted appScope: %#v", got)
	}
}

func TestNirvanaOutboundWorkingSetRejectsWorkspaceFolderShape(t *testing.T) {
	folderShape := []interface{}{
		map[string]interface{}{"name": "Existing App", "uri": "now-file:/existingappsysid"},
	}
	if got, ok := nirvanaOutboundWorkingSet(folderShape); ok || got != nil {
		t.Fatalf("folder-shaped workingSet should be suppressed, got %#v ok=%v", got, ok)
	}
}

func TestNirvanaOutboundWorkingSetAllowsServerRecordShape(t *testing.T) {
	serverShape := []interface{}{
		map[string]interface{}{"table": "sys_app", "sysId": "appsysid", "scopeId": "scopeid", "extra": true},
	}
	got, ok := nirvanaOutboundWorkingSet(serverShape)
	if !ok || len(got) != 1 {
		t.Fatalf("server-shaped workingSet rejected: %#v ok=%v", got, ok)
	}
}

func TestSaveCurrentStatePreservesPersistedWebWorkspaceScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile := "scope-preserve"
	existing := newWorkspaceState("Instance Scan")
	existing.WebWorkspaceURI = "settings:/users/test/workspaces/Instance Scan.code-workspace"
	existing.WebWorkspaceChecksum = "checksum"
	existing.WebWorkspaceDescription = "workspace description"
	existing.WebWorkspaceFolders = []WebWorkspaceFolder{
		{Name: "My Exp Approval", URI: "now-file:/app-one"},
		{Name: "BA Analytics", URI: "now-file:/app-two"},
	}
	if err := saveWorkspace(profile, existing); err != nil {
		t.Fatal(err)
	}

	c := &Client{
		opts:              Options{Profile: profile},
		workspaceName:     existing.Name,
		conversationID:    "conversation-one",
		conversationTitle: "Selected conversation",
	}
	if err := c.saveCurrentState(); err != nil {
		t.Fatal(err)
	}
	saved, ok := loadWorkspace(profile, existing.Name)
	if !ok {
		t.Fatal("saved workspace not found")
	}
	if saved.WebWorkspaceURI != existing.WebWorkspaceURI || saved.WebWorkspaceChecksum != existing.WebWorkspaceChecksum || saved.WebWorkspaceDescription != existing.WebWorkspaceDescription {
		t.Fatalf("web workspace metadata was erased: %#v", saved)
	}
	if len(saved.WebWorkspaceFolders) != 2 || saved.WebWorkspaceFolders[1].Name != "BA Analytics" {
		t.Fatalf("web workspace folders were erased: %#v", saved.WebWorkspaceFolders)
	}
	if saved.ConversationID != c.conversationID || saved.ConversationTitle != c.conversationTitle {
		t.Fatalf("conversation update was not saved: %#v", saved)
	}
}

func TestWorkspaceStateOmitsInvalidWorkingSet(t *testing.T) {
	c := &Client{workingSet: []interface{}{map[string]interface{}{"name": "Existing App", "uri": "now-file:/existingappsysid"}}}
	if c.WorkspaceState().WorkingSet != nil {
		t.Fatalf("invalid workingSet should not be persisted: %#v", c.WorkspaceState().WorkingSet)
	}
}

func TestApplyWorkspaceDropsInvalidWorkingSet(t *testing.T) {
	c := &Client{}
	c.applyWorkspace(WorkspaceState{WorkingSet: []interface{}{map[string]interface{}{"name": "Existing App", "uri": "now-file:/existingappsysid"}}})
	if c.workingSet != nil {
		t.Fatalf("invalid workingSet should not be restored: %#v", c.workingSet)
	}
}

func TestNirvanaHARShapeHelpers(t *testing.T) {
	ctx := emptyIDEContext()
	for _, key := range []string{"currentFile", "currentDir", "selectedText"} {
		if stringify(ctx[key]) != "" {
			t.Fatalf("%s = %q, want empty", key, ctx[key])
		}
	}
	if _, ok := ctx["workspaceFolders"].([]string); !ok {
		t.Fatalf("workspaceFolders = %#v, want []string", ctx["workspaceFolders"])
	}
	if got := instanceNameFromURL("https://demoalectriallwfze140800.service-now.com/"); got != "demoalectriallwfze140800" {
		t.Fatalf("instanceNameFromURL = %q", got)
	}
	if defaultLLMProxyURL != "https://llmproxy-prod-gateway" {
		t.Fatalf("defaultLLMProxyURL = %q", defaultLLMProxyURL)
	}
}

func TestParseWDFMCPServersMatchesHARShape(t *testing.T) {
	body := []byte(`{"result":{"result":{"servers":[{"server_id":"90db06d43b3147505d77b80f23e45ad9","name":"MCP Script Runner","transport":"SSE","governance":{"aict_status":"approved"}}],"meta":{"total":1}}}}`)
	servers := parseWDFMCPServers(body)
	if len(servers) != 1 {
		t.Fatalf("servers len = %d, want 1: %#v", len(servers), servers)
	}
	server := servers[0]
	if server.ServerID != "90db06d43b3147505d77b80f23e45ad9" || server.Name != "MCP Script Runner" || server.Transport != "sse" || server.Source != "wdf" {
		t.Fatalf("unexpected WDF MCP server: %#v", server)
	}
}

func TestNirvanaMCPServerPayloadDiscoversWDFAndAddsGliderStaticATF(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var requestedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"result":{"servers":[{"server_id":"90db06d43b3147505d77b80f23e45ad9","name":"MCP Script Runner","transport":"SSE","governance":{"aict_status":"approved"}}]}}}`))
	}))
	defer srv.Close()

	c := &Client{cfg: CLIConfig{InstanceURL: srv.URL}, opts: Options{Profile: "default", Nirvana: true}, httpClient: srv.Client(), debug: true}
	servers := c.nirvanaMCPServerPayload(context.Background())
	if requestedPath != "/api/sn_wdf_mcp_client/mcp/servers?limit=50&offset=0&connected=true" {
		t.Fatalf("WDF endpoint = %q", requestedPath)
	}
	if len(servers) != 2 {
		t.Fatalf("mcpServers len = %d, want WDF + static ATF: %#v", len(servers), servers)
	}
	if servers[0].ServerID != "90db06d43b3147505d77b80f23e45ad9" || servers[0].Name != "MCP Script Runner" || servers[0].Transport != "sse" || servers[0].Source != "wdf" {
		t.Fatalf("unexpected WDF server payload: %#v", servers[0])
	}
	if servers[1].ServerID != "atf-cloud-runner" || servers[1].Name != "ATF Cloud runner" || servers[1].Transport != "streamable-http" || servers[1].URL != "https://atf-rel-boq/mcp" || servers[1].Source != "static" {
		t.Fatalf("unexpected static ATF server payload: %#v", servers[1])
	}
}

func TestNirvanaMCPServerPayloadPaginatesAndFiltersLikeGliderWDF(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	requestedPaths := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.RawQuery, "offset=0"):
			_, _ = w.Write([]byte(`{"result":{"result":{"servers":[{"server_id":"script-runner","name":"MCP Script Runner","transport":"SSE"},{"server_id":"github","name":"GitHub MCP","transport":"SSE"},{"server_id":"git","name":"Git Tools","transport":"SSE"}],"meta":{"total":51}}}}`))
		case strings.Contains(r.URL.RawQuery, "offset=50"):
			_, _ = w.Write([]byte(`{"result":{"result":{"servers":[{"server_id":"bad-transport","name":"Bad Transport","transport":"websocket"},{"server_id":"second","name":"Second MCP","transport":"HTTP"}],"meta":{"total":51}}}}`))
		default:
			t.Fatalf("unexpected WDF request: %s", r.URL.String())
		}
	}))
	defer srv.Close()

	c := &Client{cfg: CLIConfig{InstanceURL: srv.URL}, opts: Options{Profile: "default", Nirvana: true}, httpClient: srv.Client()}
	servers := c.nirvanaMCPServerPayload(context.Background())
	if len(requestedPaths) != 2 || !strings.Contains(requestedPaths[0], "offset=0") || !strings.Contains(requestedPaths[1], "offset=50") {
		t.Fatalf("WDF pagination requests = %#v", requestedPaths)
	}
	ids := []string{}
	for _, server := range servers {
		ids = append(ids, server.ServerID)
		if server.ServerID == "github" || server.ServerID == "git" || server.ServerID == "bad-transport" {
			t.Fatalf("Glider-excluded/unsupported WDF server leaked into payload: %#v", servers)
		}
	}
	want := []string{"script-runner", "second", "atf-cloud-runner"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("server ids = %#v, want %#v", ids, want)
	}
}

func TestNirvanaConversationIDUsesGliderCompactShape(t *testing.T) {
	c := &Client{conversationID: "6987b1ac-eba8-0750-998e-fcf000d0cdf9"}
	c.ensureNirvanaConversationID()
	if c.conversationID != "6987b1aceba80750998efcf000d0cdf9" {
		t.Fatalf("Nirvana Glider conversation id = %q, want compact 32-hex", c.conversationID)
	}
}

func TestNirvanaConversationHistorySanitizesUnsupportedRoles(t *testing.T) {
	history := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
		map[string]interface{}{"role": "assistant", "content": "hi"},
		map[string]interface{}{"role": "stop", "content": "Processing was stopped. You can send a new message to continue."},
		map[string]interface{}{"role": "assistant-thinking", "content": "hidden"},
		map[string]interface{}{"role": "tool", "content": "tool output"},
	}
	got := nirvanaConversationHistory(history)
	if len(got) != 3 {
		t.Fatalf("sanitized len = %d, want 3: %#v", len(got), got)
	}
	last := asMap(got[2])
	if last["role"] != "user" || !strings.Contains(stringify(last["content"]), "Processing was stopped") || !strings.HasPrefix(stringify(last["content"]), "[System:") {
		t.Fatalf("unexpected stop conversion: %#v", last)
	}
	for _, raw := range got {
		role := stringify(asMap(raw)["role"])
		if role != "user" && role != "assistant" {
			t.Fatalf("unsupported role leaked to Nirvana history: %#v", raw)
		}
	}
}

func TestNirvanaDialHeadersReuseSavedWebSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	instanceURL := "https://demoalectriallwfze140800.service-now.com"
	if err := saveWebSession("default", WebSession{
		InstanceURL:  instanceURL,
		AuthMode:     authModeForm,
		CookieHeader: "JSESSIONID=fake; glide_user=alsofake",
		UserToken:    "fake-user-token",
	}); err != nil {
		t.Fatal(err)
	}
	c := &Client{cfg: CLIConfig{InstanceURL: instanceURL}, opts: Options{Profile: "default", Nirvana: true}}
	headers := c.nirvanaDialHeaders()
	if headers.Get("Cookie") != "JSESSIONID=fake; glide_user=alsofake" {
		t.Fatalf("Cookie header = %q", headers.Get("Cookie"))
	}
	if headers.Get("X-UserToken") != "fake-user-token" {
		t.Fatalf("X-UserToken header = %q", headers.Get("X-UserToken"))
	}
	if !strings.HasSuffix(headers.Get("Referer"), "/sn_glider_app/ide.do") {
		t.Fatalf("Referer header = %q", headers.Get("Referer"))
	}
}

func TestNirvanaRESTHeadersUseOAuthBearer(t *testing.T) {
	c := &Client{cfg: CLIConfig{InstanceURL: "https://demo.example.com"}, opts: Options{Nirvana: true}, oauthAccessToken: "oauth-token", sessionCookieHeader: "JSESSIONID=stale", userToken: "stale-gck"}
	req, err := http.NewRequest(http.MethodGet, "https://demo.example.com/api/now/table/sys_user", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.setGatewayHeaders(req)
	if req.Header.Get("Authorization") != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Origin") != "https://demo.example.com" {
		t.Fatalf("Origin = %q", req.Header.Get("Origin"))
	}
	if req.Header.Get("Referer") != "https://demo.example.com/sn_glider_app/ide.do" {
		t.Fatalf("Referer = %q", req.Header.Get("Referer"))
	}
	if req.Header.Get("Cookie") != "" || req.Header.Get("X-UserToken") != "" {
		t.Fatalf("stale web session headers should be suppressed when Nirvana bearer is available: Cookie=%q X-UserToken=%q", req.Header.Get("Cookie"), req.Header.Get("X-UserToken"))
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestUnknownNirvanaElicitationReportsUnsupportedAdvertisedAction(t *testing.T) {
	c := &Client{opts: Options{Nirvana: true}}
	result, status, err := c.answerUnknownElicitation("future_web_action", map[string]interface{}{})
	if err != nil || status != "error" {
		t.Fatalf("status=%q err=%v result=%#v", status, err, result)
	}
	if got := stringify(result["code"]); got != "UNEXPECTED_CLIENT_ACTION" {
		t.Fatalf("code=%q", got)
	}
	message := stringify(result["error"])
	if !strings.Contains(message, "unsupported client-side action") || !strings.Contains(message, "advertised web-client action") {
		t.Fatalf("message=%q", message)
	}
	if strings.Contains(message, "only advertises server-side tools") {
		t.Fatalf("misleading Nirvana message=%q", message)
	}
}
