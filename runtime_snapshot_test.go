package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func snapshotTestClient() *Client {
	return &Client{
		cfg:            CLIConfig{InstanceURL: "https://DEV123.service-now.com", CapabilityID: "capability"},
		opts:           Options{Profile: "demo", Nirvana: true, AuthMode: authModeCookie},
		runtime:        RuntimeModelConfig{Provider: "openai", LargeModel: "large", SmallModel: "small", LargeMaxOutputTokens: 10, SmallMaxOutputTokens: 2},
		conversationID: "conversation-a", workspaceName: "workspace-a", workspaceURI: "now-workspace:/a", workspaceChecksum: "checksum-a",
		workspaceFolders: []WebWorkspaceFolder{{Name: "a"}}, currentApp: &AppScope{ScopeID: "scope-a", ScopeName: "App A"},
		workingSet:       []interface{}{map[string]interface{}{"table": "sys_app", "sysId": "app-a", "scopeId": "scope-a"}},
		history:          []interface{}{map[string]interface{}{"role": "user", "content": "before"}},
		webStartupConfig: WebStartupConfig{ToolTimeouts: map[string]time.Duration{"default": time.Minute}, DefaultTimeout: time.Minute},
		oauthAccessToken: "super-secret-oauth", sessionCookieHeader: "JSESSIONID=super-secret-cookie", userToken: "super-secret-gck",
		streamTypes: map[string]string{},
	}
}

func TestTurnRuntimeSnapshotDeepImmutableAndSecretFree(t *testing.T) {
	c := snapshotTestClient()
	c.workingSet = []interface{}{map[string]interface{}{"table": "sys_app", "sysId": "app-a", "scopeId": "scope-a", "token": "super-secret-oauth"}}
	c.history = []interface{}{map[string]interface{}{"role": "user", "content": "Bearer super-secret-oauth"}}
	s := c.captureTurnRuntimeSnapshot([]MCPServer{{ServerID: "b", Name: "B", URL: "https://user:super-secret-oauth@example.test/mcp?token=super-secret-oauth"}})
	c.currentApp.ScopeID = "changed"
	c.workingSet.([]interface{})[0].(map[string]interface{})["sysId"] = "changed"
	c.workspaceFolders[0].Name = "changed"
	c.webStartupConfig.ToolTimeouts["default"] = time.Second
	if s.App.ScopeID != "scope-a" || s.WorkingSet[0].(map[string]interface{})["sysId"] != "app-a" || s.Workspace.Folders[0].Name != "a" || s.Timeouts["default"] != time.Minute {
		t.Fatalf("snapshot changed after source mutation: %#v", s)
	}
	raw := string(mustJSON(t, s))
	for _, secret := range []string{"super-secret-oauth", "super-secret-cookie", "super-secret-gck", "JSESSIONID"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("secret leaked in snapshot: %q", raw)
		}
	}
}

func TestTurnRuntimeSnapshotDeterministicWorkingSetAndMCP(t *testing.T) {
	c := snapshotTestClient()
	first := c.captureTurnRuntimeSnapshot([]MCPServer{{ServerID: "b", Name: "B"}, {ServerID: "a", Name: "A"}})
	second := c.captureTurnRuntimeSnapshot([]MCPServer{{ServerID: "a", Name: "A"}, {ServerID: "b", Name: "B"}})
	if first.WorkingSetHash == "" || first.WorkingSetHash != second.WorkingSetHash || first.MCPGeneration == "" || first.MCPGeneration != second.MCPGeneration || first.MCPServers[0].ServerID != "a" {
		t.Fatalf("non-deterministic snapshot: %#v %#v", first, second)
	}
}

func TestTurnRuntimeSnapshotSanitizesAllMCPFieldsBeforeHashing(t *testing.T) {
	c := snapshotTestClient()
	secrets := []string{"mcp-token-secret", "mcp-bearer-secret", "mcp-cookie-secret", "mcp-password-secret", "mcp-auth-secret"}
	servers := []MCPServer{{
		ServerID: "id token=mcp-token-secret", Name: "name Bearer mcp-bearer-secret", Transport: "Authorization: mcp-auth-secret",
		Source: "cookie=mcp-cookie-secret", URL: "https://user:mcp-password-secret@example.test/mcp?token=mcp-token-secret",
	}}
	snapshot := c.captureTurnRuntimeSnapshot(servers)
	raw := string(mustJSON(t, snapshot))
	for _, secret := range secrets {
		if strings.Contains(raw, secret) || strings.Contains(snapshot.MCPGeneration, secret) || strings.Contains(snapshot.MCPHash, secret) {
			t.Fatalf("MCP secret leaked into snapshot/hash source: %q", raw)
		}
	}
	if !strings.Contains(raw, "[redacted]") {
		t.Fatalf("MCP fields were not redacted: %q", raw)
	}
	first := c.captureTurnRuntimeSnapshot([]MCPServer{{ServerID: "b token=mcp-token-secret"}, {ServerID: "a cookie=mcp-cookie-secret"}})
	second := c.captureTurnRuntimeSnapshot([]MCPServer{{ServerID: "a cookie=mcp-cookie-secret"}, {ServerID: "b token=mcp-token-secret"}})
	if first.MCPGeneration != second.MCPGeneration || first.MCPServers[0].ServerID > first.MCPServers[1].ServerID {
		t.Fatalf("sanitized MCP ordering/hash is not deterministic: %#v %#v", first, second)
	}
}

func TestTurnRuntimeSnapshotTurnBoundaryAndNextTurnRefresh(t *testing.T) {
	c := snapshotTestClient()
	c.conn = nil // SendMessage must reject before creating a snapshot.
	if err := c.SendMessage(context.Background(), "no connection"); err == nil || c.turnRuntimeSnapshot != nil {
		t.Fatalf("snapshot captured before accepted turn: err=%v snapshot=%#v", err, c.turnRuntimeSnapshot)
	}
	first := c.captureTurnRuntimeSnapshot(nil)
	c.conversationID, c.workspaceName, c.workingSet = "conversation-b", "workspace-b", []interface{}{map[string]interface{}{"table": "sys_app", "sysId": "app-b", "scopeId": "scope-b"}}
	if first.ConversationID != "conversation-a" || first.Workspace.Name != "workspace-a" {
		t.Fatalf("active snapshot mutated: %#v", first)
	}
	second := c.captureTurnRuntimeSnapshot(nil)
	if second.ConversationID != "conversation-b" || second.Workspace.Name != "workspace-b" || second.WorkingSetHash == first.WorkingSetHash {
		t.Fatalf("next turn did not refresh: first=%#v second=%#v", first, second)
	}
}

func TestTurnAcceptedUsesRuntimeSnapshotIdentity(t *testing.T) {
	c := snapshotTestClient()
	c.semanticState = NewSemanticTurnState()
	c.semanticLifecycleID = "test"
	s := c.captureTurnRuntimeSnapshot([]MCPServer{{ServerID: "mcp"}})
	c.beginSemanticTurnSnapshot(s)
	context := c.semanticState.Context
	if context.ConversationID != s.ConversationID || context.WorkingSetHash != s.WorkingSetHash || context.RuntimeGeneration != s.MCPGeneration || context.Transport != s.Transport {
		t.Fatalf("semantic context did not use snapshot: %#v", context)
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
