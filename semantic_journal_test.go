package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func journalEvent(id string, typ SemanticEventType, payload any) SemanticEvent {
	return SemanticEvent{Version: SemanticEventVersion, ID: id, TurnID: "turn-1", Type: typ, OccurredAt: time.Now().UTC(), Payload: payload}
}

func TestSemanticJournalAppendReplayAndPermissions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile, workspace := "default", "default"
	for _, event := range []SemanticEvent{
		journalEvent("1", EventTurnAccepted, TurnAcceptedPayload{Context: TurnContext{Workspace: workspace, ConversationID: "conv-1"}}),
		journalEvent("2", EventTurnStarted, nil),
		journalEvent("3", EventUsageUpdated, UsageUpdatedPayload{InputTokens: 3, OutputTokens: 2}),
		journalEvent("4", EventAssistantCompleted, AssistantCompletedPayload{}),
		journalEvent("5", EventTurnCompleted, TurnCompletedPayload{}),
	} {
		if _, err := appendSemanticJournalEvent(profile, workspace, "conv-1", event); err != nil {
			t.Fatal(err)
		}
	}
	path := semanticJournalFile(profile, workspace)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal permissions = %o, want 600", info.Mode().Perm())
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm() != 0o700 {
		t.Fatalf("journal directory permissions = %o, want 700", dir.Mode().Perm())
	}
	recovered, err := recoverWorkspaceFromJournal(profile, workspace, newWorkspaceState(workspace), false)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.LastSequence != 5 || recovered.Workspace.ConversationID != "conv-1" || recovered.Workspace.UsageInputTokens != 3 || recovered.IncompleteTurn {
		t.Fatalf("unexpected recovery: %#v", recovered)
	}
}

func TestSemanticJournalCorruptTrailingLineAndSnapshotBehind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile, workspace := "default", "default"
	accepted := journalEvent("1", EventTurnAccepted, TurnAcceptedPayload{Context: TurnContext{ConversationID: "conv-1"}})
	started := journalEvent("2", EventTurnStarted, nil)
	usage := journalEvent("3", EventUsageUpdated, UsageUpdatedPayload{InputTokens: 7, OutputTokens: 4})
	for _, event := range []SemanticEvent{accepted, started, usage} {
		if _, err := appendSemanticJournalEvent(profile, workspace, "conv-1", event); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(semanticJournalFile(profile, workspace), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"version":`)
	_ = f.Close()
	snapshot := newWorkspaceState(workspace)
	// A checkpoint before the accepted boundary can safely replay this turn.
	snapshot.SemanticJournalSequence = 0
	recovered, err := recoverWorkspaceFromJournal(profile, workspace, snapshot, true)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.LastSequence != 3 || recovered.Workspace.UsageInputTokens != 7 || !recovered.IncompleteTurn {
		t.Fatalf("bad trailing-corruption recovery: %#v", recovered)
	}
}

func TestSemanticJournalRejectsCredentialsBeforeAppend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	event := journalEvent("unsafe", EventAssistantDelta, AssistantDeltaPayload{Delta: "Authorization: Bearer secret-value"})
	if _, err := appendSemanticJournalEvent("default", "default", "conv", event); err == nil {
		t.Fatal("credential-bearing payload was accepted")
	}
	if _, err := os.Stat(semanticJournalFile("default", "default")); !os.IsNotExist(err) {
		t.Fatalf("unsafe event created journal: %v", err)
	}
	event = journalEvent("unsafe-metadata", EventTurnStarted, nil)
	event.Metadata = map[string]string{"g_ck": "secret"}
	if _, err := appendSemanticJournalEvent("default", "default", "conv", event); err == nil {
		t.Fatal("g_ck metadata was accepted")
	}
}

func TestSemanticJournalRetentionArchivesAndKeepsBoundedTail(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile, workspace := "default", "default"
	for i := 0; i < semanticJournalMaxEntries+1; i++ {
		event := journalEvent(fmt.Sprintf("event-%d", i+1), EventConnectionStateChanged, ConnectionStateChangedPayload{State: "connected"})
		if _, err := appendSemanticJournalEvent(profile, workspace, "", event); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := readSemanticJournal(semanticJournalFile(profile, workspace))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != semanticJournalRetainEntries {
		t.Fatalf("retained entries = %d, want %d", len(entries), semanticJournalRetainEntries)
	}
	archives, err := filepath.Glob(filepath.Join(semanticJournalArchiveDir(profile), workspace+"-*.jsonl"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("archives = %v, err=%v", archives, err)
	}
	recovered, err := recoverWorkspaceFromJournal(profile, workspace, newWorkspaceState(workspace), false)
	if err != nil || recovered.LastSequence != semanticJournalMaxEntries+1 {
		t.Fatalf("no-snapshot archive recovery = %#v, err=%v", recovered, err)
	}
}

func TestSemanticJournalDuplicateAndIncompleteTurnRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile, workspace := "default", "default"
	accepted := journalEvent("accepted", EventTurnAccepted, TurnAcceptedPayload{Context: TurnContext{ConversationID: "conv"}})
	if _, err := appendSemanticJournalEvent(profile, workspace, "conv", accepted); err != nil {
		t.Fatal(err)
	}
	// Journal duplicates are replayed through the reducer and remain idempotent.
	if _, err := appendSemanticJournalEvent(profile, workspace, "conv", accepted); err != nil {
		t.Fatal(err)
	}
	if _, err := appendSemanticJournalEvent(profile, workspace, "conv", journalEvent("started", EventTurnStarted, nil)); err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverWorkspaceFromJournal(profile, workspace, newWorkspaceState(workspace), false)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.IncompleteTurn || recovered.Workspace.ConversationID != "conv" {
		t.Fatalf("incomplete turn was not deterministically abandoned: %#v", recovered)
	}
	// A conflicting duplicate is rejected by reducer replay, rather than silently
	// changing an already persisted semantic event.
	conflict := accepted
	conflict.Payload = TurnAcceptedPayload{Context: TurnContext{ConversationID: "different"}}
	if _, err := appendSemanticJournalEvent(profile, workspace, "conv", conflict); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverWorkspaceFromJournal(profile, workspace, newWorkspaceState(workspace), false); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("conflicting duplicate recovery error = %v", err)
	}
}

func TestSemanticJournalSnapshotOlderThanLiveTailReplaysArchives(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile, workspace := "default", "default"
	entries := journalChain(workspace, "conv", 1, 6)
	writeJournalFixture(t, filepath.Join(semanticJournalArchiveDir(profile), workspace+"-001.jsonl"), entries[:3])
	writeJournalFixture(t, semanticJournalFile(profile, workspace), entries[3:])
	snapshot := newWorkspaceState(workspace)
	snapshot.SemanticJournalSequence = 2
	recovered, err := recoverWorkspaceFromJournal(profile, workspace, snapshot, true)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.LastSequence != 6 {
		t.Fatalf("last sequence = %d, want 6", recovered.LastSequence)
	}
}

func TestSemanticJournalMalformedNonTrailingRecordFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile, workspace := "default", "default"
	entries := journalChain(workspace, "conv", 1, 2)
	path := semanticJournalFile(profile, workspace)
	writeJournalFixture(t, path, entries[:1])
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{bad json}\n")
	raw, _ := json.Marshal(entries[1])
	_, _ = f.Write(append(raw, '\n'))
	_ = f.Close()
	if _, err := recoverWorkspaceFromJournal(profile, workspace, newWorkspaceState(workspace), false); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("non-trailing corruption error = %v", err)
	}
}

func TestSemanticJournalArchiveCapAndPrunedSnapshotPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile, workspace := "default", "default"
	// Five retained archives cover 101..600, and the live tail covers 601..700.
	// The older 1..100 range represents intentionally pruned history.
	for i := 0; i < semanticJournalArchiveLimit; i++ {
		start := uint64(101 + i*100)
		writeJournalFixture(t, filepath.Join(semanticJournalArchiveDir(profile), fmt.Sprintf("%s-%03d.jsonl", workspace, i)), journalChain(workspace, "conv", start, start+99))
	}
	writeJournalFixture(t, semanticJournalFile(profile, workspace), journalChain(workspace, "conv", 601, 700))
	if err := retainSemanticJournal(profile, workspace, append(journalChain(workspace, "conv", 601, 700), journalChain(workspace, "conv", 701, 1601)...)); err != nil {
		t.Fatal(err)
	}
	archives, err := filepath.Glob(filepath.Join(semanticJournalArchiveDir(profile), workspace+"-*.jsonl"))
	if err != nil || len(archives) != semanticJournalArchiveLimit {
		t.Fatalf("archive cap = %d, err=%v", len(archives), err)
	}
	entries, err := readSemanticJournalChain(profile, workspace)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := newWorkspaceState(workspace)
	snapshot.SemanticJournalSequence = entries[0].Sequence - 1
	if _, err := recoverWorkspaceFromJournal(profile, workspace, snapshot, true); err != nil {
		t.Fatalf("retained recoverable range failed: %v", err)
	}
	if _, err := recoverWorkspaceFromJournal(profile, workspace, newWorkspaceState(workspace), false); err == nil || !strings.Contains(err.Error(), "pruned") {
		t.Fatalf("pruned no-snapshot policy error = %v", err)
	}
}

func TestSemanticJournalSequenceOverlapAndGapAreSafe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile, workspace := "default", "default"
	entries := journalChain(workspace, "conv", 1, 3)
	writeJournalFixture(t, filepath.Join(semanticJournalArchiveDir(profile), workspace+"-001.jsonl"), entries[:2])
	// Exact archive/live overlap is a safe interrupted-rotation case.
	writeJournalFixture(t, semanticJournalFile(profile, workspace), entries[1:])
	if got, err := readSemanticJournalChain(profile, workspace); err != nil || len(got) != 3 {
		t.Fatalf("identical overlap = %d entries, err=%v", len(got), err)
	}
	conflict := entries[1]
	conflict.Event.ID = "conflict"
	writeJournalFixture(t, semanticJournalFile(profile, workspace), []SemanticJournalEnvelope{conflict, entries[2]})
	if _, err := readSemanticJournalChain(profile, workspace); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting overlap error = %v", err)
	}
	gap := entries[2]
	gap.Sequence = 4
	writeJournalFixture(t, semanticJournalFile(profile, workspace), []SemanticJournalEnvelope{gap})
	if _, err := readSemanticJournalChain(profile, workspace); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("sequence gap error = %v", err)
	}
}

func TestSemanticJournalRecoverySkipsCheckpointedMidTurnAndRequiresNewBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile, workspace := "default", "default"
	// Retained history begins mid-turn, but the snapshot has already checkpointed
	// it. The fresh post-checkpoint TurnAccepted is independently replayable.
	midTurn := []SemanticJournalEnvelope{
		journalEnvelope(workspace, "conv", 50, EventTurnStarted, nil),
		journalEnvelope(workspace, "conv", 51, EventAssistantDelta, AssistantDeltaPayload{Delta: "old"}),
		journalEnvelope(workspace, "conv", 52, EventAssistantCompleted, AssistantCompletedPayload{}),
		journalEnvelope(workspace, "conv", 53, EventTurnCompleted, TurnCompletedPayload{}),
		journalEnvelope(workspace, "conv", 54, EventTurnAccepted, TurnAcceptedPayload{Context: TurnContext{ConversationID: "conv"}}),
		journalEnvelope(workspace, "conv", 55, EventTurnStarted, nil),
	}
	writeJournalFixture(t, semanticJournalFile(profile, workspace), midTurn)
	snapshot := newWorkspaceState(workspace)
	snapshot.SemanticJournalSequence = 53
	recovered, err := recoverWorkspaceFromJournal(profile, workspace, snapshot, true)
	if err != nil || !recovered.IncompleteTurn || recovered.LastSequence != 55 {
		t.Fatalf("checkpointed mid-turn recovery = %#v, err=%v", recovered, err)
	}
	snapshot.SemanticJournalSequence = 50
	if _, err := recoverWorkspaceFromJournal(profile, workspace, snapshot, true); err == nil || !strings.Contains(err.Error(), "safe lifecycle boundary") {
		t.Fatalf("mid-turn post-checkpoint error = %v", err)
	}
}

func TestSemanticJournalAppendRepairsPartialTrailingRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile, workspace := "default", "default"
	first := journalEnvelope(workspace, "conv", 1, EventConnectionStateChanged, ConnectionStateChangedPayload{State: "connected"})
	writeJournalFixture(t, semanticJournalFile(profile, workspace), []SemanticJournalEnvelope{first})
	f, err := os.OpenFile(semanticJournalFile(profile, workspace), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"version":`)
	_ = f.Close()
	if _, err := appendSemanticJournalEvent(profile, workspace, "conv", journalEvent("accepted", EventTurnAccepted, TurnAcceptedPayload{Context: TurnContext{ConversationID: "conv"}})); err != nil {
		t.Fatal(err)
	}
	entries, err := readSemanticJournalChain(profile, workspace)
	if err != nil || len(entries) != 2 || entries[1].Sequence != 2 {
		t.Fatalf("repaired append entries=%#v err=%v", entries, err)
	}
	if _, err := recoverWorkspaceFromJournal(profile, workspace, newWorkspaceState(workspace), false); err != nil {
		t.Fatalf("repaired append recovery: %v", err)
	}
}

func journalChain(workspace, conversation string, start, end uint64) []SemanticJournalEnvelope {
	entries := make([]SemanticJournalEnvelope, 0, end-start+1)
	for sequence := start; sequence <= end; sequence++ {
		entries = append(entries, SemanticJournalEnvelope{
			Version: SemanticJournalVersion, Workspace: workspace, ConversationID: conversation,
			Sequence: sequence, ScopeSequence: sequence,
			Event: SemanticEvent{Version: SemanticEventVersion, ID: fmt.Sprintf("fixture-%d", sequence), Type: EventConnectionStateChanged, OccurredAt: time.Unix(int64(sequence), 0).UTC(), Payload: ConnectionStateChangedPayload{State: "connected"}},
		})
	}
	return entries
}

func journalEnvelope(workspace, conversation string, sequence uint64, typ SemanticEventType, payload any) SemanticJournalEnvelope {
	return SemanticJournalEnvelope{
		Version: SemanticJournalVersion, Workspace: workspace, ConversationID: conversation,
		Sequence: sequence, ScopeSequence: sequence,
		Event: SemanticEvent{Version: SemanticEventVersion, ID: fmt.Sprintf("fixture-%d", sequence), TurnID: "turn-1", Type: typ, OccurredAt: time.Unix(int64(sequence), 0).UTC(), Payload: payload},
	}
}

func writeJournalFixture(t *testing.T, path string, entries []SemanticJournalEnvelope) {
	t.Helper()
	if err := writeSemanticJournal(path, entries); err != nil {
		t.Fatal(err)
	}
}
