package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTurnDebugRegistryHelpCompletionValidation(t *testing.T) {
	c := newStatusClient(t)
	for _, name := range []string{"/turn", "/debug"} {
		command, ok := findSlashCommand(name)
		if !ok || !command.AvailableWhileProcessing {
			t.Fatalf("command %s = %#v %v", name, command, ok)
		}
	}
	var help strings.Builder
	_, err := withSlashCommandOutput(&help, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/help") })
	if err != nil || !strings.Contains(help.String(), "/turn") || !strings.Contains(help.String(), "/debug") {
		t.Fatalf("help=%q err=%v", help.String(), err)
	}
	for _, line := range []string{"/turn nope", "/debug", "/debug tail nope", "/debug event"} {
		if _, err := handleSlashCommand(context.Background(), c, line); err == nil {
			t.Fatalf("%s unexpectedly valid", line)
		}
	}
}
func TestTurnNoActiveLastAndSnapshotConsistency(t *testing.T) {
	c := newStatusClient(t)
	doc := c.turnDocument()
	if doc.Availability != "no_turn" || doc.Turn.State != "idle" {
		t.Fatalf("no turn=%+v", doc)
	}
	c.semanticState = NewSemanticTurnState()
	c.semanticState.Status = SemanticTurnCompleted
	c.semanticState.TurnID = "server"
	c.semanticState.Context = TurnContext{Workspace: "last"}
	doc = c.turnDocument()
	if doc.Availability != "last" || doc.Turn.State != "completed" {
		t.Fatalf("last=%+v", doc)
	}
	c.processing = true
	c.turnRuntimeSnapshot = &TurnRuntimeSnapshot{CapturedAt: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC), Profile: "frozen", Workspace: TurnWorkspaceSnapshot{Name: "frozen-ws"}, ConversationID: "frozen-conv", Transport: "nirvana_websocket", MCPGeneration: "g"}
	c.semanticState.Status = SemanticTurnStarted
	c.workspaceName = "changed"
	doc = c.turnDocument()
	if doc.Turn.AcceptedSnapshot.Profile != "frozen" || doc.Turn.AcceptedSnapshot.Workspace != "frozen-ws" {
		t.Fatalf("snapshot=%+v", doc.Turn.AcceptedSnapshot)
	}
}

func TestTurnJournalTimingFreezesTerminalAndAdvancesActive(t *testing.T) {
	c := newStatusClient(t)
	base := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	entries := []SemanticJournalEnvelope{
		{Version: SemanticJournalVersion, Workspace: c.workspaceName, Sequence: 1, ScopeSequence: 1, Event: SemanticEvent{Version: SemanticEventVersion, ID: "accepted", Sequence: 1, Type: EventTurnAccepted, OccurredAt: base, Payload: TurnAcceptedPayload{}}},
		{Version: SemanticJournalVersion, Workspace: c.workspaceName, Sequence: 2, ScopeSequence: 2, Event: SemanticEvent{Version: SemanticEventVersion, ID: "started", Sequence: 2, TurnID: "turn-1", Type: EventTurnStarted, OccurredAt: base.Add(2 * time.Second)}},
		{Version: SemanticJournalVersion, Workspace: c.workspaceName, Sequence: 3, ScopeSequence: 3, Event: SemanticEvent{Version: SemanticEventVersion, ID: "assistant", Sequence: 3, TurnID: "turn-1", Type: EventAssistantCompleted, OccurredAt: base.Add(4 * time.Second), Payload: AssistantCompletedPayload{}}},
		{Version: SemanticJournalVersion, Workspace: c.workspaceName, Sequence: 4, ScopeSequence: 4, Event: SemanticEvent{Version: SemanticEventVersion, ID: "completed", Sequence: 4, TurnID: "turn-1", Type: EventTurnCompleted, OccurredAt: base.Add(7 * time.Second), Payload: TurnCompletedPayload{}}},
	}
	if err := writeSemanticJournal(semanticJournalFile(c.opts.Profile, c.workspaceName), entries); err != nil {
		t.Fatal(err)
	}
	c.semanticState = NewSemanticTurnState()
	c.semanticState.Status = SemanticTurnCompleted
	c.semanticState.TurnID = "turn-1"
	statusNow = func() time.Time { return base.Add(time.Hour) }
	defer func() { statusNow = func() time.Time { return time.Now().UTC() } }()
	first := c.turnDocument()
	statusNow = func() time.Time { return base.Add(2 * time.Hour) }
	second := c.turnDocument()
	if first.Availability != "last" || first.Turn.StartedAt != base.Add(2*time.Second).Format(time.RFC3339Nano) || first.Turn.TerminalAt != base.Add(7*time.Second).Format(time.RFC3339Nano) || first.Turn.Duration != "5s" || second.Turn.Duration != first.Turn.Duration {
		t.Fatalf("terminal timing first=%+v second=%+v", first.Turn, second.Turn)
	}
	c.processing = true
	c.semanticState.Status = SemanticTurnStarted
	c.turnRuntimeSnapshot = &TurnRuntimeSnapshot{CapturedAt: base}
	c.turnStartedAt = base.Add(10 * time.Second)
	statusNow = func() time.Time { return base.Add(12 * time.Second) }
	active := c.turnDocument()
	statusNow = func() time.Time { return base.Add(15 * time.Second) }
	advancing := c.turnDocument()
	if active.Availability != "active" || active.Turn.Duration != "2s" || advancing.Turn.Duration != "5s" {
		t.Fatalf("active timing active=%+v later=%+v", active.Turn, advancing.Turn)
	}
}

func TestTurnTimingUnknownForMissingCorruptAndPrunedJournal(t *testing.T) {
	c := newStatusClient(t)
	c.semanticState = NewSemanticTurnState()
	c.semanticState.Status = SemanticTurnCompleted
	c.semanticState.TurnID = "turn-1"
	c.turnStartedAt = time.Now().UTC().Add(-time.Hour)
	if doc := c.turnDocument(); doc.Turn.StartedAt != "" || doc.Turn.TerminalAt != "" || doc.Turn.Duration != "" || doc.Journal.Health != "missing" {
		t.Fatalf("missing=%+v", doc)
	}
	entry := SemanticJournalEnvelope{Version: SemanticJournalVersion, Workspace: c.workspaceName, Sequence: 50, ScopeSequence: 1, Event: SemanticEvent{Version: SemanticEventVersion, ID: "completed", Sequence: 1, TurnID: "turn-1", Type: EventTurnCompleted, OccurredAt: time.Now().UTC(), Payload: TurnCompletedPayload{}}}
	if err := writeSemanticJournal(semanticJournalFile(c.opts.Profile, c.workspaceName), []SemanticJournalEnvelope{entry}); err != nil {
		t.Fatal(err)
	}
	if doc := c.turnDocument(); doc.Journal.Retention != "pruned" || doc.Turn.StartedAt != "" || doc.Turn.TerminalAt != "" || doc.Turn.Duration != "" {
		t.Fatalf("pruned=%+v", doc)
	}
}
func TestTurnJSONDeterministicAndRedacted(t *testing.T) {
	c := newStatusClient(t)
	c.semanticState = NewSemanticTurnState()
	c.semanticState.Status = SemanticTurnStarted
	c.semanticState.TurnID = "Bearer secret"
	c.semanticState.Tools = map[string]SemanticToolState{"b": {Name: "token=secret"}, "a": {Name: "read", Completed: true, Success: true}}
	c.processing = true
	c.turnRuntimeSnapshot = &TurnRuntimeSnapshot{CapturedAt: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC), Profile: "token=secret", Workspace: TurnWorkspaceSnapshot{Name: "ws"}}
	statusNow = func() time.Time { return time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC) }
	defer func() { statusNow = func() time.Time { return time.Now().UTC() } }()
	a, _ := json.Marshal(c.turnDocument())
	b, _ := json.Marshal(c.turnDocument())
	if string(a) != string(b) || strings.Contains(string(a), "secret") {
		t.Fatalf("unstable/unsafe=%s", a)
	}
}
func TestDebugStateTailAndEventReadOnly(t *testing.T) {
	c := newStatusClient(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("unexpected HTTP") }))
	defer server.Close()
	c.cfg.InstanceURL = server.URL
	c.httpClient = server.Client()
	if _, err := handleSlashCommand(context.Background(), c, "/debug on"); err != nil || !c.debugLocal || c.debug {
		t.Fatal("debug on")
	}
	if _, err := handleSlashCommand(context.Background(), c, "/debug off"); err != nil || c.debugLocal || c.debug {
		t.Fatal("debug off")
	}
	entries := []SemanticJournalEnvelope{}
	for i := 1; i <= 105; i++ {
		entries = append(entries, SemanticJournalEnvelope{Version: SemanticJournalVersion, Workspace: c.workspaceName, Sequence: uint64(i), ScopeSequence: uint64(i), Event: SemanticEvent{Version: SemanticEventVersion, ID: "event-" + string(rune(i+64)), Sequence: uint64(i), Type: EventUsageUpdated, OccurredAt: time.Date(2026, 7, 13, 10, 0, i, 0, time.UTC), Payload: UsageUpdatedPayload{InputTokens: int64(i)}}})
	}
	if err := writeSemanticJournal(semanticJournalFile(c.opts.Profile, c.workspaceName), entries); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(semanticJournalFile(c.opts.Profile, c.workspaceName))
	var output strings.Builder
	if _, err := withSlashCommandOutput(&output, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/debug tail 999 --json") }); err != nil {
		t.Fatal(err)
	}
	var doc debugDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Events) != debugTailMaximum || doc.Events[0].Sequence != 6 || doc.Events[len(doc.Events)-1].Sequence != 105 {
		t.Fatalf("tail=%+v", doc.Events)
	}
	output.Reset()
	if _, err := withSlashCommandOutput(&output, func() (bool, error) {
		return handleSlashCommand(context.Background(), c, "/debug event event-A --json")
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "event-A") {
		t.Fatalf("event=%q", output.String())
	}
	if _, err := handleSlashCommand(context.Background(), c, "/debug event no-such"); err == nil {
		t.Fatal("missing event accepted")
	}
	after, _ := os.ReadFile(semanticJournalFile(c.opts.Profile, c.workspaceName))
	if string(before) != string(after) {
		t.Fatal("inspection wrote journal")
	}
}
func TestDebugJournalFailuresAndContentRedaction(t *testing.T) {
	c := newStatusClient(t)
	var output strings.Builder
	if _, err := withSlashCommandOutput(&output, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/debug tail --json") }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"health":"missing"`) {
		t.Fatalf("missing=%q", output.String())
	}
	if err := os.WriteFile(semanticJournalFile(c.opts.Profile, c.workspaceName), []byte("{broken}\nnext\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if _, err := withSlashCommandOutput(&output, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/debug tail --json") }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"health":"corrupt"`) {
		t.Fatalf("corrupt=%q", output.String())
	}
	details := redactedSemanticDetails(SemanticEvent{Type: EventAssistantDelta, Payload: AssistantDeltaPayload{Delta: "full prompt Authorization: Bearer secret"}})
	raw, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "full prompt") || !strings.Contains(string(raw), "[omitted]") {
		t.Fatalf("leaked=%q", raw)
	}
}

func TestDebugErrorsAndHumanEventOutputAreRedactedAndDeterministic(t *testing.T) {
	c := newStatusClient(t)
	for _, line := range []string{"/debug token=secret", "/debug event token=secret", "/debug on password=secret", "/turn Bearer secret"} {
		_, err := handleSlashCommand(context.Background(), c, line)
		if err == nil || strings.Contains(strings.ToLower(err.Error()), "secret") || strings.Contains(strings.ToLower(err.Error()), "bearer") || strings.Contains(strings.ToLower(err.Error()), "password") {
			t.Fatalf("unsafe error %q: %v", line, err)
		}
	}
	entry := SemanticJournalEnvelope{Version: SemanticJournalVersion, Workspace: c.workspaceName, Sequence: 1, ScopeSequence: 1, Event: SemanticEvent{Version: SemanticEventVersion, ID: "event", Sequence: 1, Type: EventTurnAccepted, OccurredAt: time.Now().UTC(), Payload: TurnAcceptedPayload{Context: TurnContext{Workspace: "ws", AppScopeID: "app", WorkingSetHash: "hash", Transport: "nirvana"}}}}
	if err := writeSemanticJournal(semanticJournalFile(c.opts.Profile, c.workspaceName), []SemanticJournalEnvelope{entry}); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if _, err := withSlashCommandOutput(&output, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/debug event event") }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `details={"appScopeId":"app","conversationId":"","transport":"nirvana","workingSetHash":"hash","workspace":"ws"}`) {
		t.Fatalf("nondeterministic details=%q", output.String())
	}
}
