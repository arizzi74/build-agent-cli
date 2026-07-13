package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SemanticJournalVersion identifies the append-only local recovery format.
// It is intentionally separate from SemanticEventVersion so the envelope can
// evolve without changing the reducer contract.
const SemanticJournalVersion = 1

const (
	semanticJournalRetainEntries = 500
	semanticJournalMaxEntries    = 1000
	semanticJournalArchiveLimit  = 5
)

type SemanticJournalEnvelope struct {
	Version        int           `json:"version"`
	Workspace      string        `json:"workspace"`
	ConversationID string        `json:"conversationId,omitempty"`
	Sequence       uint64        `json:"sequence"`      // monotonic per workspace journal
	ScopeSequence  uint64        `json:"scopeSequence"` // monotonic per workspace/conversation
	Event          SemanticEvent `json:"event"`
}

type semanticJournalRecovery struct {
	Workspace      WorkspaceState
	LastSequence   uint64
	IncompleteTurn bool
}

func semanticJournalFile(profile, workspace string) string {
	return filepath.Join(workspacesDir(profile), workspace+".semantic.jsonl")
}

func semanticJournalArchiveDir(profile string) string {
	return filepath.Join(workspacesDir(profile), "semantic-archive")
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func appendSemanticJournalEvent(profile, workspace, conversationID string, event SemanticEvent) (SemanticJournalEnvelope, error) {
	if !isValidWorkspaceName(workspace) {
		return SemanticJournalEnvelope{}, fmt.Errorf("invalid workspace %q", workspace)
	}
	if err := validateSemanticEventForJournal(event); err != nil {
		return SemanticJournalEnvelope{}, err
	}
	if err := ensurePrivateDir(workspacesDir(profile)); err != nil {
		return SemanticJournalEnvelope{}, err
	}
	path := semanticJournalFile(profile, workspace)
	liveEntries, err := readSemanticJournal(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return SemanticJournalEnvelope{}, err
	}
	if err := repairSemanticJournalTrailingRecord(path); err != nil {
		return SemanticJournalEnvelope{}, err
	}
	var sequence, scopeSequence uint64
	for _, entry := range liveEntries {
		if entry.Sequence > sequence {
			sequence = entry.Sequence
		}
		if entry.ConversationID == conversationID && entry.ScopeSequence > scopeSequence {
			scopeSequence = entry.ScopeSequence
		}
	}
	// The live journal always owns the highest workspace sequence after a
	// completed rotation. Only scan retained archives when this conversation
	// is absent from the live tail and needs its prior scope counter.
	if scopeSequence == 0 {
		entries, err := readSemanticJournalChain(profile, workspace)
		if err != nil {
			return SemanticJournalEnvelope{}, err
		}
		for _, entry := range entries {
			if entry.ConversationID == conversationID && entry.ScopeSequence > scopeSequence {
				scopeSequence = entry.ScopeSequence
			}
		}
	}
	envelope := SemanticJournalEnvelope{
		Version: SemanticJournalVersion, Workspace: workspace, ConversationID: conversationID,
		Sequence: sequence + 1, ScopeSequence: scopeSequence + 1, Event: event,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return SemanticJournalEnvelope{}, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return SemanticJournalEnvelope{}, err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return SemanticJournalEnvelope{}, err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return SemanticJournalEnvelope{}, err
	}
	// A semantic event is small and is the recovery boundary, so flush each
	// append rather than risking a completed local turn being lost on a crash.
	if err := f.Sync(); err != nil {
		return SemanticJournalEnvelope{}, err
	}
	if err := retainSemanticJournal(profile, workspace, append(liveEntries, envelope)); err != nil {
		return SemanticJournalEnvelope{}, err
	}
	return envelope, nil
}

func readSemanticJournal(path string) ([]SemanticJournalEnvelope, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries := []SemanticJournalEnvelope{}
	scanner := bufio.NewScanner(f)
	// Assistant text can be sizeable. The envelope remains bounded by this
	// local safety limit while still allowing normal Build Agent replies.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var envelope SemanticJournalEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			// A final short/invalid JSONL record is a normal crash signature. Do
			// not let it invalidate earlier fsynced events. Corruption in the
			// middle remains an error and is not silently skipped.
			if !scannerHasAnotherNonEmptyLine(scanner) {
				break
			}
			return nil, fmt.Errorf("semantic journal line %d: %w", line, err)
		}
		if err := validateSemanticJournalEnvelope(envelope); err != nil {
			return nil, fmt.Errorf("semantic journal line %d: %w", line, err)
		}
		entries = append(entries, envelope)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// repairSemanticJournalTrailingRecord removes only an incomplete final JSONL
// record before O_APPEND. Without this repair, appending after crash bytes
// would permanently join malformed JSON to the next valid envelope.
func repairSemanticJournalTrailingRecord(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || len(raw) == 0 || raw[len(raw)-1] == '\n' {
		return err
	}
	lastNewline := bytes.LastIndexByte(raw, '\n')
	trailing := bytes.TrimSpace(raw[lastNewline+1:])
	if len(trailing) == 0 {
		return nil
	}
	var envelope SemanticJournalEnvelope
	if err := json.Unmarshal(trailing, &envelope); err == nil {
		// A complete record without its delimiter is valid but must be
		// terminated before the next O_APPEND record.
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return err
		}
		return f.Sync()
	}
	// Verify the prefix through the normal reader before truncating. This makes
	// repair limited to the known interrupted-final-record case.
	if _, err := readSemanticJournal(path); err != nil {
		return err
	}
	return os.Truncate(path, int64(lastNewline+1))
}

// scannerHasAnotherNonEmptyLine consumes only the remainder of this scanner.
// It is used solely after a malformed record: a malformed final line is safely
// ignored, but a malformed middle line must be reported.
func scannerHasAnotherNonEmptyLine(scanner *bufio.Scanner) bool {
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) != 0 {
			return true
		}
	}
	return false
}

func validateSemanticJournalEnvelope(envelope SemanticJournalEnvelope) error {
	if envelope.Version != SemanticJournalVersion {
		return fmt.Errorf("unsupported semantic journal version %d", envelope.Version)
	}
	if !isValidWorkspaceName(envelope.Workspace) || envelope.Sequence == 0 || envelope.ScopeSequence == 0 {
		return errors.New("invalid semantic journal envelope scope or sequence")
	}
	return validateSemanticEventForJournal(envelope.Event)
}

func retainSemanticJournal(profile, workspace string, entries []SemanticJournalEnvelope) error {
	if len(entries) <= semanticJournalMaxEntries {
		return nil
	}
	cut := len(entries) - semanticJournalRetainEntries
	if err := ensurePrivateDir(semanticJournalArchiveDir(profile)); err != nil {
		return err
	}
	archive := filepath.Join(semanticJournalArchiveDir(profile), fmt.Sprintf("%s-%s.jsonl", workspace, time.Now().UTC().Format("20060102T150405.000000000Z")))
	if err := writeSemanticJournal(archive, entries[:cut]); err != nil {
		return err
	}
	if err := writeSemanticJournal(semanticJournalFile(profile, workspace), entries[cut:]); err != nil {
		return err
	}
	archives, err := filepath.Glob(filepath.Join(semanticJournalArchiveDir(profile), workspace+"-*.jsonl"))
	if err != nil {
		return err
	}
	sort.Strings(archives)
	for len(archives) > semanticJournalArchiveLimit {
		if err := os.Remove(archives[0]); err != nil {
			return err
		}
		archives = archives[1:]
	}
	return nil
}

// readSemanticJournalChain reads the retained archive chain followed by the
// live journal. Rotation writes the archive before replacing the live file, so
// a crash can temporarily leave exact records in both places; those duplicates
// are accepted only when byte-for-byte identical. Any conflicting overlap or
// gap in the retained sequence is a recovery error.
func readSemanticJournalChain(profile, workspace string) ([]SemanticJournalEnvelope, error) {
	archivePaths, err := filepath.Glob(filepath.Join(semanticJournalArchiveDir(profile), workspace+"-*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(archivePaths)
	paths := append(archivePaths, semanticJournalFile(profile, workspace))
	bySequence := map[uint64]SemanticJournalEnvelope{}
	for _, path := range paths {
		entries, err := readSemanticJournal(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.Workspace != workspace {
				return nil, fmt.Errorf("semantic journal %q contains workspace %q", path, entry.Workspace)
			}
			if prior, exists := bySequence[entry.Sequence]; exists {
				if !sameSemanticJournalEnvelope(prior, entry) {
					return nil, fmt.Errorf("conflicting semantic journal sequence %d", entry.Sequence)
				}
				continue
			}
			bySequence[entry.Sequence] = entry
		}
	}
	sequences := make([]uint64, 0, len(bySequence))
	for sequence := range bySequence {
		sequences = append(sequences, sequence)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	entries := make([]SemanticJournalEnvelope, 0, len(sequences))
	for i, sequence := range sequences {
		if i > 0 && sequence != sequences[i-1]+1 {
			return nil, fmt.Errorf("semantic journal sequence gap between %d and %d", sequences[i-1], sequence)
		}
		entries = append(entries, bySequence[sequence])
	}
	return entries, nil
}

func sameSemanticJournalEnvelope(a, b SemanticJournalEnvelope) bool {
	araw, aerr := json.Marshal(a)
	braw, berr := json.Marshal(b)
	return aerr == nil && berr == nil && bytes.Equal(araw, braw)
}

func writeSemanticJournal(path string, entries []SemanticJournalEnvelope) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	var out bytes.Buffer
	for _, entry := range entries {
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		out.Write(raw)
		out.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".semantic-journal-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func recoverWorkspaceFromJournal(profile, name string, snapshot WorkspaceState, snapshotOK bool) (semanticJournalRecovery, error) {
	if !snapshotOK {
		snapshot = newWorkspaceState(name)
	}
	if snapshot.Name == "" {
		snapshot.Name = name
	}
	entries, err := readSemanticJournalChain(profile, name)
	if err != nil {
		return semanticJournalRecovery{}, err
	}
	if len(entries) == 0 {
		return semanticJournalRecovery{Workspace: snapshot, LastSequence: snapshot.SemanticJournalSequence}, nil
	}
	firstSequence := entries[0].Sequence
	if !snapshotOK && firstSequence != 1 {
		return semanticJournalRecovery{}, fmt.Errorf("semantic journal history before sequence %d was pruned; no workspace snapshot is available", firstSequence)
	}
	if snapshotOK && snapshot.SemanticJournalSequence+1 < firstSequence {
		return semanticJournalRecovery{}, fmt.Errorf("workspace snapshot checkpoint %d predates retained semantic journal sequence %d", snapshot.SemanticJournalSequence, firstSequence)
	}
	if snapshot.SemanticJournalSequence > entries[len(entries)-1].Sequence {
		return semanticJournalRecovery{}, fmt.Errorf("workspace snapshot checkpoint %d is ahead of semantic journal sequence %d", snapshot.SemanticJournalSequence, entries[len(entries)-1].Sequence)
	}
	active := map[string]SemanticTurnState{}
	seen := map[string]struct{}{}
	last := snapshot.SemanticJournalSequence
	for _, entry := range entries {
		if entry.Sequence > last {
			last = entry.Sequence
		}
		if entry.Sequence <= snapshot.SemanticJournalSequence {
			continue
		}
		event, err := decodeSemanticJournalEvent(entry.Event)
		if err != nil {
			return semanticJournalRecovery{}, err
		}
		// The accepted event intentionally precedes the server-provided TurnID,
		// so recovery scopes reducer state by workspace/conversation, resetting
		// only after a terminal event rather than keying on the later TurnID.
		key := entry.ConversationID
		if _, exists := active[key]; !exists {
			if !semanticRecoveryBoundaryEvent(event) {
				return semanticJournalRecovery{}, fmt.Errorf("semantic journal sequence %d begins after snapshot checkpoint without a safe lifecycle boundary", entry.Sequence)
			}
			active[key] = NewSemanticTurnState()
		} else if active[key].Terminal() {
			if !semanticRecoveryBoundaryEvent(event) {
				return semanticJournalRecovery{}, fmt.Errorf("semantic journal sequence %d follows a terminal turn without a new safe lifecycle boundary", entry.Sequence)
			}
			active[key] = NewSemanticTurnState()
		}
		state, err := Apply(active[key], event)
		if err != nil {
			return semanticJournalRecovery{}, fmt.Errorf("semantic journal sequence %d: %w", entry.Sequence, err)
		}
		active[key] = state
		seen[key] = struct{}{}
		if entry.Sequence > snapshot.SemanticJournalSequence {
			applySemanticEventToWorkspace(&snapshot, event)
		}
	}
	snapshot.SemanticJournalSequence = last
	incomplete := false
	for key := range seen {
		if active[key].Status == SemanticTurnAccepted || active[key].Status == SemanticTurnStarted {
			incomplete = true
		}
	}
	// Recovery deliberately does not create a synthetic remote terminal event or
	// add partial assistant text to conversation history. A locally interrupted
	// turn is treated as abandoned; the next user prompt starts a clean turn.
	return semanticJournalRecovery{Workspace: snapshot, LastSequence: last, IncompleteTurn: incomplete}, nil
}

func semanticRecoveryBoundaryEvent(event SemanticEvent) bool {
	return event.Type == EventConnectionStateChanged || event.Type == EventTurnAccepted
}

func applySemanticEventToWorkspace(ws *WorkspaceState, event SemanticEvent) {
	switch event.Type {
	case EventTurnAccepted:
		p := event.Payload.(TurnAcceptedPayload)
		if p.Context.ConversationID != "" {
			ws.ConversationID = p.Context.ConversationID
		}
	case EventConversationUpdated:
		p := event.Payload.(ConversationUpdatedPayload)
		if p.ConversationID != "" {
			ws.ConversationID = p.ConversationID
		}
		if p.Title != "" {
			ws.ConversationTitle = p.Title
		}
		if p.State != "" {
			ws.ConversationState = p.State
		}
	case EventAppScopeChanged:
		p := event.Payload.(AppScopeChangedPayload)
		if p.ScopeID != "" {
			ws.AppScope = p.ScopeID
		}
	case EventUsageUpdated:
		p := event.Payload.(UsageUpdatedPayload)
		ws.UsageInputTokens, ws.UsageOutputTokens, ws.UsageThinkingTokens = p.InputTokens, p.OutputTokens, p.ThinkingTokens
	}
}

func decodeSemanticJournalEvent(event SemanticEvent) (SemanticEvent, error) {
	raw, err := json.Marshal(event.Payload)
	if err != nil {
		return event, err
	}
	var target any
	switch event.Type {
	case EventConnectionStateChanged:
		target = &ConnectionStateChangedPayload{}
	case EventTurnAccepted:
		target = &TurnAcceptedPayload{}
	case EventAssistantDelta:
		target = &AssistantDeltaPayload{}
	case EventAssistantCompleted:
		target = &AssistantCompletedPayload{}
	case EventToolStarted:
		target = &ToolStartedPayload{}
	case EventToolCompleted:
		target = &ToolCompletedPayload{}
	case EventElicitationRequested:
		target = &ElicitationRequestedPayload{}
	case EventUsageUpdated:
		target = &UsageUpdatedPayload{}
	case EventWorkingSetUpdated:
		target = &WorkingSetUpdatedPayload{}
	case EventAppScopeChanged:
		target = &AppScopeChangedPayload{}
	case EventConversationUpdated:
		target = &ConversationUpdatedPayload{}
	case EventTransportRetry:
		target = &TransportRetryPayload{}
	case EventRetryScheduled, EventRetryAttempted, EventRetryExhausted:
		target = &RetryPayload{}
	case EventTransportFallback:
		target = &TransportFallbackPayload{}
	case EventTurnCancelled:
		target = &TurnCancelledPayload{}
	case EventTurnFailed:
		target = &TurnFailedPayload{}
	case EventTurnCompleted:
		target = &TurnCompletedPayload{}
	case EventTelemetryFailed:
		target = &TelemetryFailedPayload{}
	case EventTurnStarted:
		event.Payload = nil
		return event, nil
	default:
		return event, fmt.Errorf("unknown semantic event type %q", event.Type)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return event, err
	}
	switch value := target.(type) {
	case *ConnectionStateChangedPayload:
		event.Payload = *value
	case *TurnAcceptedPayload:
		event.Payload = *value
	case *AssistantDeltaPayload:
		event.Payload = *value
	case *AssistantCompletedPayload:
		event.Payload = *value
	case *ToolStartedPayload:
		event.Payload = *value
	case *ToolCompletedPayload:
		event.Payload = *value
	case *ElicitationRequestedPayload:
		event.Payload = *value
	case *UsageUpdatedPayload:
		event.Payload = *value
	case *WorkingSetUpdatedPayload:
		event.Payload = *value
	case *AppScopeChangedPayload:
		event.Payload = *value
	case *ConversationUpdatedPayload:
		event.Payload = *value
	case *TransportRetryPayload:
		event.Payload = *value
	case *RetryPayload:
		event.Payload = *value
	case *TransportFallbackPayload:
		event.Payload = *value
	case *TurnCancelledPayload:
		event.Payload = *value
	case *TurnFailedPayload:
		event.Payload = *value
	case *TurnCompletedPayload:
		event.Payload = *value
	case *TelemetryFailedPayload:
		event.Payload = *value
	}
	return event, nil
}

var credentialValuePattern = regexp.MustCompile(`(?i)(bearer\s+\S+|basic\s+\S+|(?:^|[\s,{])g_ck\s*[:=]|set-cookie\s*:|cookie\s*:|authorization\s*:)`)

func validateSemanticEventForJournal(event SemanticEvent) error {
	if err := event.validate(); err != nil {
		return err
	}
	for key, value := range event.Metadata {
		if unsafeJournalKey(strings.ToLower(key)) || credentialValuePattern.MatchString(value) {
			return fmt.Errorf("unsafe semantic event metadata %q", key)
		}
	}
	raw, err := json.Marshal(event.Payload)
	if err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	return validateJournalValue(value, "payload")
}

func validateJournalValue(value any, path string) error {
	switch value := value.(type) {
	case map[string]any:
		for key, nested := range value {
			lower := strings.ToLower(key)
			if unsafeJournalKey(lower) {
				return fmt.Errorf("unsafe semantic event %s key %q", path, key)
			}
			if err := validateJournalValue(nested, path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		for i, nested := range value {
			if err := validateJournalValue(nested, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case string:
		if credentialValuePattern.MatchString(value) {
			return fmt.Errorf("unsafe semantic event %s value", path)
		}
	}
	return nil
}

func unsafeJournalKey(key string) bool {
	if key == "inputtokens" || key == "outputtokens" || key == "thinkingtokens" {
		return false
	}
	return strings.Contains(key, "token") || strings.Contains(key, "password") || strings.Contains(key, "cookie") || strings.Contains(key, "authorization") || strings.Contains(key, "g_ck") || strings.Contains(key, "secret") || strings.Contains(key, "credential") || strings.Contains(key, "header") || strings.Contains(key, "frame")
}
