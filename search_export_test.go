package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSearchRegistryHelpValidationAndDeterminism(t *testing.T) {
	inSupportBundleTestDir(t)
	c := newStatusClient(t)
	c.history = []interface{}{map[string]interface{}{"role": "user", "content": "Bearer private-token", "id": "m1"}, map[string]interface{}{"role": "assistant", "content": "answer", "id": "m2"}}
	for _, name := range []string{"/search", "/export"} {
		if _, ok := findSlashCommand(name); !ok {
			t.Fatalf("missing %s", name)
		}
	}
	if err := validateSlashCommandRegistry(slashCommandRegistry); err != nil {
		t.Fatal(err)
	}
	var help strings.Builder
	if _, err := withSlashCommandOutput(&help, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/help") }); err != nil || !strings.Contains(help.String(), "/search") || !strings.Contains(help.String(), "/export") {
		t.Fatalf("help=%q err=%v", help.String(), err)
	}
	a := c.offlineSearch("assistant", 20)
	b := c.offlineSearch("assistant", 20)
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	if !bytes.Equal(ra, rb) {
		t.Fatal("search non-deterministic")
	}
	if len(a.Results) == 0 || a.Results[0].Source == "" || a.Results[0].MatchedField == "" {
		t.Fatalf("results=%+v", a.Results)
	}
	if _, _, _, err := parseSearchArgs([]string{"Bearer", "private-token"}); err == nil || strings.Contains(strings.ToLower(err.Error()), "private") {
		t.Fatalf("unsafe query err=%v", err)
	}
	if _, _, _, err := parseSearchArgs([]string{strings.Repeat("x", searchQueryMaxBytes+1)}); err == nil {
		t.Fatal("long query accepted")
	}
	var out strings.Builder
	if _, err := withSlashCommandOutput(&out, func() (bool, error) {
		return handleSlashCommand(context.Background(), c, "/search assistant --json --limit 1")
	}); err != nil {
		t.Fatal(err)
	}
	var doc offlineSearchDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &doc); err != nil || doc.Limit != 1 || len(doc.Results) > 1 {
		t.Fatalf("doc=%s err=%v", out.String(), err)
	}
}

func TestSearchJournalMissingCorruptAndPruned(t *testing.T) {
	inSupportBundleTestDir(t)
	c := newStatusClient(t)
	doc := c.offlineSearch("default", 20)
	if doc.Availability != "degraded" || doc.Sources.Journal != "missing" {
		t.Fatalf("availability=%s", doc.Availability)
	}
	if err := ensurePrivateDir(workspacesDir(c.opts.Profile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(semanticJournalFile(c.opts.Profile, c.workspaceName), []byte("{bad}\n{also bad}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc = c.offlineSearch("default", 20)
	if doc.Availability != "degraded" || doc.Sources.Journal != "corrupt" {
		t.Fatalf("availability=%s", doc.Availability)
	}
}

func TestExportDeterminismSafetyAndPaths(t *testing.T) {
	inSupportBundleTestDir(t)
	c := newStatusClient(t)
	c.history = []interface{}{map[string]interface{}{"role": "user", "content": "Bearer should-not-leak", "id": "user-1"}, map[string]interface{}{"role": "assistant", "content": "full assistant answer", "id": "assistant-1"}}
	fixed := time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC)
	statusNow = func() time.Time { return fixed }
	defer func() { statusNow = func() time.Time { return time.Now().UTC() } }()
	first, manifest, err := c.conversationExportPayloads(fixed)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := c.conversationExportPayloads(fixed)
	if err != nil {
		t.Fatal(err)
	}
	with := func(p map[string][]byte) map[string][]byte {
		out := map[string][]byte{}
		for k, v := range p {
			out[k] = v
		}
		raw, _ := json.Marshal(manifest)
		out["manifest.json"] = raw
		return out
	}
	var a, b bytes.Buffer
	if err := writeDeterministicConversationExportZIP(&a, with(first), fixed); err != nil {
		t.Fatal(err)
	}
	if err := writeDeterministicConversationExportZIP(&b, with(second), fixed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("bytes nondeterministic")
	}
	zr, err := zip.NewReader(bytes.NewReader(a.Bytes()), int64(a.Len()))
	if err != nil {
		t.Fatal(err)
	}
	want := "config.json,context.json,events.json,journal.json,manifest.json,messages.json,status.json,turn.json"
	got := []string{}
	for _, f := range zr.File {
		got = append(got, f.Name)
		if f.Mode().Perm() != 0o600 {
			t.Fatal("member permissions")
		}
		r, _ := f.Open()
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(r)
		_ = r.Close()
		for _, canary := range []string{"should-not-leak", "full assistant answer", "/home/", "prompt"} {
			if strings.Contains(strings.ToLower(raw.String()), strings.ToLower(canary)) {
				t.Fatalf("canary %s in %s", canary, f.Name)
			}
		}
	}
	if strings.Join(got, ",") != want {
		t.Fatalf("members=%v", got)
	}
	var messages []exportMessageMetadata
	for _, f := range zr.File {
		if f.Name == "messages.json" {
			r, _ := f.Open()
			_ = json.NewDecoder(r).Decode(&messages)
			_ = r.Close()
		}
	}
	if len(messages) != 2 || messages[0].Content != "[omitted]" || messages[0].ContentHash != "" {
		t.Fatalf("messages=%+v", messages)
	}
	result, err := c.createConversationExport("")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(profileDir(c.opts.Profile), result.Path)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact=%v mode=%v", err, info.Mode())
	}
	dirInfo, _ := os.Stat(filepath.Dir(path))
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatal("private dir mode")
	}
	if _, err := c.createConversationExport("custom.zip"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.createConversationExport("custom.zip"); err == nil {
		t.Fatal("overwrite accepted")
	}
	for _, unsafe := range []string{"../x.zip", "/tmp/x.zip", "token-secret.zip", "a.txt"} {
		if _, _, err := conversationExportDestination(c.opts.Profile, unsafe, fixed); err == nil {
			t.Fatalf("unsafe %q accepted", unsafe)
		}
	}
	if err := os.Symlink(t.TempDir(), "unsafe-parent"); err != nil {
		t.Fatal(err)
	}
	defer os.Remove("unsafe-parent")
	if _, err := c.createConversationExport("unsafe-parent/out.zip"); err == nil {
		t.Fatal("symlink ancestry accepted")
	}
}

func TestExportConcurrentNoOverwriteAndActiveSnapshot(t *testing.T) {
	inSupportBundleTestDir(t)
	c := newStatusClient(t)
	fixed := time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC)
	statusNow = func() time.Time { return fixed }
	defer func() { statusNow = func() time.Time { return time.Now().UTC() } }()
	c.processing = true
	c.turnRuntimeSnapshot = &TurnRuntimeSnapshot{}
	_, manifest, err := c.conversationExportPayloads(fixed)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Snapshot != "immutable_active_snapshot" {
		t.Fatalf("snapshot=%s", manifest.Snapshot)
	}
	var wg sync.WaitGroup
	successes := 0
	var mu sync.Mutex
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.createConversationExport("race.zip"); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("successes=%d", successes)
	}
}

func TestExportScopesEventsMessagesAndJournalHealth(t *testing.T) {
	inSupportBundleTestDir(t)
	c := newStatusClient(t)
	c.workspaceName, c.conversationID = "default", "conv-a"
	c.semanticState = SemanticTurnState{TurnID: "turn-a", Status: SemanticTurnCompleted, Context: TurnContext{Workspace: "default", ConversationID: "conv-a"}}
	c.history = []interface{}{
		map[string]interface{}{"id": "a", "role": "user", "content": strings.Repeat("safe content ", 10), "conversationId": "conv-a"},
		map[string]interface{}{"id": "b", "role": "assistant", "content": "wrong", "conversationId": "conv-b"},
	}
	entries := []SemanticJournalEnvelope{
		exportJournalEvent(1, "conv-a", "turn-a", EventTurnAccepted), exportJournalEvent(2, "conv-b", "turn-b", EventTurnAccepted),
		exportJournalEvent(3, "conv-a", "turn-a", EventToolCompleted), exportJournalEvent(4, "conv-b", "turn-b", EventTurnCompleted),
	}
	if err := ensurePrivateDir(workspacesDir(c.opts.Profile)); err != nil {
		t.Fatal(err)
	}
	if err := writeSemanticJournal(semanticJournalFile(c.opts.Profile, c.workspaceName), entries); err != nil {
		t.Fatal(err)
	}
	payloads, manifest, err := c.conversationExportPayloads(time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Scope.Kind != "last_turn" || manifest.Scope.ConversationID != "conv-a" || manifest.Scope.TurnID != "turn-a" {
		t.Fatalf("scope=%+v", manifest.Scope)
	}
	var events []debugEvent
	if err := json.Unmarshal(payloads["events.json"], &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events=%+v", events)
	}
	for _, e := range events {
		if e.TurnID != "turn-a" {
			t.Fatalf("mixed event=%+v", e)
		}
	}
	var messages []exportMessageMetadata
	_ = json.Unmarshal(payloads["messages.json"], &messages)
	if len(messages) != 1 || messages[0].ID != "a" || !strings.HasPrefix(messages[0].ContentHash, "sha256:") {
		t.Fatalf("messages=%+v", messages)
	}
	// Active scope includes entries appended after the accepted snapshot checkpoint.
	c.processing = true
	c.turnRuntimeSnapshot = &TurnRuntimeSnapshot{Workspace: TurnWorkspaceSnapshot{Name: "default"}, ConversationID: "conv-a", ConversationHistory: c.history}
	c.semanticState.Status = SemanticTurnStarted
	c.semanticJournalSequence = 1
	payloads, manifest, err = c.conversationExportPayloads(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(payloads["events.json"], &events)
	if manifest.Scope.Kind != "active_turn" || len(events) != 2 {
		t.Fatalf("active scope=%+v events=%+v", manifest.Scope, events)
	}
	// A pruned retained suffix remains safely exportable, while corrupt is fail-closed.
	entries[0].Sequence = 5
	entries[1].Sequence = 6
	entries[2].Sequence = 7
	entries[3].Sequence = 8
	if err := writeSemanticJournal(semanticJournalFile(c.opts.Profile, c.workspaceName), entries); err != nil {
		t.Fatal(err)
	}
	c.processing = false
	payloads, _, err = c.conversationExportPayloads(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(payloads["events.json"], &events)
	if len(events) != 2 {
		t.Fatalf("pruned events=%+v", events)
	}
	if err := os.WriteFile(semanticJournalFile(c.opts.Profile, c.workspaceName), []byte("{bad}\n{bad}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payloads, _, err = c.conversationExportPayloads(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(payloads["events.json"], &events)
	if len(events) != 0 {
		t.Fatalf("corrupt events=%+v", events)
	}
}

func exportJournalEvent(sequence uint64, conversation, turn string, typ SemanticEventType) SemanticJournalEnvelope {
	payload := interface{}(TurnAcceptedPayload{Context: TurnContext{Workspace: "default", ConversationID: conversation}})
	if typ == EventToolCompleted {
		payload = ToolCompletedPayload{ToolID: "tool", Success: true}
	}
	if typ == EventTurnCompleted {
		payload = TurnCompletedPayload{}
	}
	return SemanticJournalEnvelope{Version: SemanticJournalVersion, Workspace: "default", ConversationID: conversation, Sequence: sequence, ScopeSequence: sequence, Event: SemanticEvent{Version: SemanticEventVersion, ID: fmt.Sprintf("event-%d", sequence), Sequence: sequence, TurnID: turn, Type: typ, OccurredAt: time.Unix(int64(sequence), 0).UTC(), Payload: payload}}
}
