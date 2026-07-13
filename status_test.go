package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func newStatusClient(t *testing.T) *Client {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://USER:secret@example.service-now.com/path"}, Options{Profile: "test", Nirvana: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStatusRegistryHelpCompletionAndValidation(t *testing.T) {
	c := newStatusClient(t)
	command, ok := findSlashCommand("/status")
	if !ok || command.Canonical != "/status" || !command.AvailableWhileProcessing {
		t.Fatalf("status registry = %#v, %v", command, ok)
	}
	seen := map[string]bool{}
	for _, suggestion := range slashCommandSuggestionsForClient(c) {
		seen[suggestion.Text] = true
	}
	if !seen["/status"] || !seen["/status --json"] {
		t.Fatalf("status completions missing: %#v", seen)
	}
	var help strings.Builder
	_, err := withSlashCommandOutput(&help, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/help") })
	if err != nil || !strings.Contains(help.String(), "/status") {
		t.Fatalf("help=%q err=%v", help.String(), err)
	}
	if _, err := handleSlashCommand(context.Background(), c, "/status json"); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("invalid status option error=%v", err)
	}
}

func TestStatusHumanAndJSONAreStableAndOfflineSafe(t *testing.T) {
	c := newStatusClient(t)
	called := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		t.Errorf("unexpected status HTTP call: %s", r.URL)
		http.Error(w, "no", 500)
	}))
	defer s.Close()
	c.cfg.InstanceURL = s.URL
	c.httpClient = s.Client()
	c.oauthAccessToken, c.sessionCookieHeader, c.basicPass = "token-secret", "JSESSIONID=secret", "password-secret"
	c.nirvanaMCPServersReady = true
	c.nirvanaMCPServers = []MCPServer{{ServerID: "z", Name: "Zulu", Transport: "sse", Source: "wdf", URL: "https://token:secret@host/path?token=secret"}, {ServerID: "a", Name: "Alpha", Transport: "http", Source: "static"}}
	c.workingSet = []interface{}{map[string]interface{}{"table": "x", "sysId": "1", "scopeId": "s"}}
	c.runtime.Provider, c.runtime.LargeModel = "provider", "model"
	c.webAgentConfig.SkillID = "skill"
	stateBefore := statusTestState(t, c.opts.Profile)
	conversationBefore, workspaceBefore := c.conversationID, c.workspaceName
	var human strings.Builder
	if _, err := withSlashCommandOutput(&human, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/status") }); err != nil {
		t.Fatal(err)
	}
	if got := human.String(); !strings.Contains(got, "status:") || !strings.Contains(got, "servers: 2") || strings.Contains(got, "secret") {
		t.Fatalf("unsafe or incomplete human output: %q", got)
	}
	var raw strings.Builder
	if _, err := withSlashCommandOutput(&raw, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/status --json") }); err != nil {
		t.Fatal(err)
	}
	var doc runtimeStatusDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw.String())), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != StatusSchemaVersion || doc.Servers.Inventory[0].ID != "a" || doc.WorkingSet.Count != 1 {
		t.Fatalf("status JSON=%+v", doc)
	}
	if bytes := raw.String(); strings.Contains(bytes, "secret") || strings.Contains(bytes, "token-secret") {
		t.Fatalf("secret leaked: %q", bytes)
	}
	if called != 0 {
		t.Fatalf("/status made %d HTTP calls", called)
	}
	if c.conversationID != conversationBefore || c.workspaceName != workspaceBefore || statusTestState(t, c.opts.Profile) != stateBefore {
		t.Fatal("/status mutated client or local state")
	}
}

func statusTestState(t *testing.T, profile string) string {
	t.Helper()
	var parts []string
	root := profileDir(profile)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		parts = append(parts, path+"="+string(raw))
		return nil
	})
	sort.Strings(parts)
	return strings.Join(parts, "\\n")
}

func TestStatusActiveTurnUsesImmutableSnapshot(t *testing.T) {
	c := newStatusClient(t)
	fixed := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	c.processing = true
	c.turnRuntimeSnapshot = &TurnRuntimeSnapshot{CapturedAt: fixed, Profile: "frozen", InstanceHost: "frozen.example", Transport: "nirvana_websocket", AuthMode: "oauth", Workspace: TurnWorkspaceSnapshot{Name: "frozen-ws"}, ConversationID: "frozen-conversation", Model: TurnModelSnapshot{Provider: "frozen-provider", LargeModel: "frozen-model"}, MCPGeneration: "frozen-generation"}
	c.opts.Profile, c.workspaceName, c.conversationID = "changed", "changed-ws", "changed-conversation"
	doc := c.statusDocument()
	if !doc.Turn.Active || doc.Profile.Name != "frozen" || doc.Context.Workspace != "frozen-ws" || doc.Context.ConversationID != "frozen-conversation" || doc.Turn.SnapshotGeneration != "frozen-generation" {
		t.Fatalf("active status not snapshot-consistent: %+v", doc)
	}
}

func TestStatusActiveTurnTreatsAbsentSnapshotFieldsAsAuthoritative(t *testing.T) {
	c := newStatusClient(t)
	c.processing = true
	c.turnRuntimeSnapshot = &TurnRuntimeSnapshot{
		CapturedAt: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC), Profile: "frozen", InstanceHost: "frozen.example",
		Transport: "nirvana_websocket", Workspace: TurnWorkspaceSnapshot{Name: "frozen-ws"}, Model: TurnModelSnapshot{},
		// Nil App, WorkingSet, and MCPServers are deliberately authoritative.
	}
	c.currentApp = &AppScope{ScopeID: "new-app", ScopeName: "new app"}
	c.workingSet = []interface{}{map[string]interface{}{"table": "new"}}
	c.nirvanaMCPServersReady = true
	c.nirvanaMCPServers = []MCPServer{{ServerID: "new-server", Name: "new server"}}
	doc := c.statusDocument()
	if doc.Context.App != nil || doc.WorkingSet.Count != 0 || len(doc.Servers.Inventory) != 0 || doc.Servers.Status != "unknown" {
		t.Fatalf("active snapshot read mutable client state: %+v", doc)
	}
}

func TestStatusRedactsEveryFreeFormField(t *testing.T) {
	c := newStatusClient(t)
	secret := "status-secret-value"
	c.processing = true
	c.turnRuntimeSnapshot = &TurnRuntimeSnapshot{
		CapturedAt: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC),
		Profile:    "token=" + secret, InstanceHost: "host token=" + secret, Transport: "Bearer " + secret, AuthMode: "cookie=" + secret,
		AuthCapabilities: []string{"Authorization: " + secret}, ConversationID: "password=" + secret,
		Workspace:     TurnWorkspaceSnapshot{Name: "g_ck=" + secret, URI: "cookie=" + secret},
		App:           &AppScope{ScopeID: "token=" + secret, Scope: "Bearer " + secret, ScopeName: "authorization=" + secret},
		MCPServers:    []MCPServer{{ServerID: "token=" + secret, Name: "Bearer " + secret, Transport: "cookie=" + secret, Source: "password=" + secret}},
		MCPGeneration: "token=" + secret, MCPHash: "cookie=" + secret,
		Model:       TurnModelSnapshot{Provider: "Bearer " + secret, LargeModel: "token=" + secret, SkillID: "password=" + secret},
		RetryPolicy: "Authorization: " + secret,
		Timeouts:    map[string]time.Duration{"token=" + secret: time.Second},
	}
	c.semanticState.ConnectionState = "Bearer " + secret
	doc := c.statusDocument()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var human strings.Builder
	print := func() (bool, error) { printStatusHuman(doc); return true, nil }
	if _, err := withSlashCommandOutput(&human, print); err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"json": string(raw), "human": human.String()} {
		if strings.Contains(output, secret) {
			t.Fatalf("%s leaked free-form secret: %q", name, output)
		}
	}
	if !strings.Contains(string(raw), "[redacted]") {
		t.Fatalf("JSON did not preserve redaction marker: %s", raw)
	}
}

func TestStatusJournalStatesAreReadOnly(t *testing.T) {
	c := newStatusClient(t)
	before, _ := os.ReadFile(semanticJournalFile(c.opts.Profile, c.workspaceName))
	missing := c.statusJournal()
	if missing.Health != "missing" || missing.Status != "unknown" {
		t.Fatalf("missing journal=%+v", missing)
	}
	event := SemanticJournalEnvelope{Version: SemanticJournalVersion, Workspace: c.workspaceName, Sequence: 1, ScopeSequence: 1, Event: SemanticEvent{Version: SemanticEventVersion, ID: "e", Sequence: 1, Type: EventConnectionStateChanged, OccurredAt: time.Now().UTC(), Payload: ConnectionStateChangedPayload{State: "connected"}}}
	if err := writeSemanticJournal(semanticJournalFile(c.opts.Profile, c.workspaceName), []SemanticJournalEnvelope{event}); err != nil {
		t.Fatal(err)
	}
	c.semanticJournalSequence = 0
	behind := c.statusJournal()
	if behind.Health != "behind" || behind.Status != "degraded" {
		t.Fatalf("behind journal=%+v", behind)
	}
	if err := os.WriteFile(semanticJournalFile(c.opts.Profile, c.workspaceName), []byte("{broken}\nnext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := c.statusJournal()
	if corrupt.Health != "corrupt" || corrupt.Status != "error" {
		t.Fatalf("corrupt journal=%+v", corrupt)
	}
	after, _ := os.ReadFile(semanticJournalFile(c.opts.Profile, c.workspaceName))
	if string(before) == string(after) { /* initial missing state intentionally changed by test setup */
	}
	if string(after) != "{broken}\nnext\n" {
		t.Fatalf("status mutated corrupt journal: %q", after)
	}
}
