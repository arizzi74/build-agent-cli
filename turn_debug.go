package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TurnSchemaVersion and DebugSchemaVersion identify the stable read-only command documents.
const (
	TurnSchemaVersion  = 1
	DebugSchemaVersion = 1
	debugTailDefault   = 20
	debugTailMaximum   = 100
)

type turnDocument struct {
	SchemaVersion int             `json:"schemaVersion"`
	GeneratedAt   string          `json:"generatedAt"`
	Availability  string          `json:"availability"`
	Turn          turnDetail      `json:"turn"`
	Journal       turnJournalInfo `json:"journal"`
}
type turnDetail struct {
	State            string              `json:"state"`
	ServerTurnID     string              `json:"serverTurnId,omitempty"`
	AcceptedSnapshot *turnSnapshot       `json:"acceptedSnapshot,omitempty"`
	StartedAt        string              `json:"startedAt,omitempty"`
	TerminalAt       string              `json:"terminalAt,omitempty"`
	Duration         string              `json:"duration,omitempty"`
	Tools            []turnTool          `json:"tools"`
	Elicitation      string              `json:"elicitation"`
	Usage            turnUsage           `json:"usage"`
	TerminalReason   string              `json:"terminalReason,omitempty"`
	ErrorCategory    string              `json:"errorCategory,omitempty"`
	PendingNextTurn  turnNextContext     `json:"pendingNextTurn"`
	Retry            turnRetry           `json:"retry"`
	Telemetry        turnTelemetry       `json:"telemetry"`
	Approvals        turnApprovalSummary `json:"approvals"`
}
type turnApprovalSummary struct {
	Pending int `json:"pending"`
	Recent  int `json:"recent"`
}
type turnSnapshot struct {
	ID             string `json:"id,omitempty"`
	Generation     string `json:"generation,omitempty"`
	CapturedAt     string `json:"capturedAt,omitempty"`
	Profile        string `json:"profile,omitempty"`
	Workspace      string `json:"workspace,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	Transport      string `json:"transport,omitempty"`
	AppScopeID     string `json:"appScopeId,omitempty"`
	WorkingSetHash string `json:"workingSetHash,omitempty"`
	GoalID         string `json:"goalId,omitempty"`
}
type turnTool struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	State string `json:"state"`
}
type turnUsage struct {
	InputTokens    int64  `json:"inputTokens"`
	OutputTokens   int64  `json:"outputTokens"`
	ThinkingTokens int64  `json:"thinkingTokens"`
	Status         string `json:"status"`
}
type turnNextContext struct {
	WorkingSetHash string `json:"workingSetHash,omitempty"`
	AppScopeID     string `json:"appScopeId,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	State          string `json:"state"`
}
type turnRetry struct {
	Count             int    `json:"count"`
	FallbackTransport string `json:"fallbackTransport,omitempty"`
	State             string `json:"state"`
}
type turnTelemetry struct {
	FailureCount int      `json:"failureCount"`
	Components   []string `json:"components"`
	Attempts     int      `json:"attempts,omitempty"`
	LastDecision string   `json:"lastDecision,omitempty"`
	State        string   `json:"state"`
}
type turnJournalInfo struct {
	Checkpoint   uint64 `json:"checkpoint"`
	LastSequence uint64 `json:"lastSequence"`
	Health       string `json:"health"`
	Retention    string `json:"retention"`
}

type debugDocument struct {
	SchemaVersion int          `json:"schemaVersion"`
	GeneratedAt   string       `json:"generatedAt"`
	Debug         string       `json:"debug"`
	Journal       debugJournal `json:"journal"`
	Events        []debugEvent `json:"events,omitempty"`
}
type debugJournal struct {
	Health       string `json:"health"`
	Retention    string `json:"retention"`
	LastSequence uint64 `json:"lastSequence"`
}
type debugEvent struct {
	ID            string                 `json:"id"`
	Sequence      uint64                 `json:"sequence"`
	ScopeSequence uint64                 `json:"scopeSequence"`
	TurnID        string                 `json:"turnId,omitempty"`
	Type          string                 `json:"type"`
	OccurredAt    string                 `json:"occurredAt"`
	Details       map[string]interface{} `json:"details"`
}

func (c *Client) turnDocument() turnDocument {
	state, snapshot, active, started, checkpoint := c.turnReadState()
	entries, journal := readTurnJournal(c.opts.Profile, c.workspaceName, checkpoint)
	doc := turnDocument{SchemaVersion: TurnSchemaVersion, GeneratedAt: statusNow().UTC().Format(time.RFC3339Nano), Availability: "no_turn", Turn: turnDetail{State: string(state.Status), Tools: []turnTool{}, Elicitation: "unknown", Usage: turnUsage{Status: "unknown"}, PendingNextTurn: turnNextContext{State: "none"}, Retry: turnRetry{State: "unknown"}, Telemetry: turnTelemetry{Components: []string{}, State: "unknown"}}, Journal: journal}
	for _, approval := range c.listApprovals() {
		doc.Turn.Approvals.Recent++
		if approval.Status == ApprovalPending || approval.Status == ApprovalApproved {
			doc.Turn.Approvals.Pending++
		}
	}
	if state.Status == SemanticTurnIdle {
		return doc
	}
	doc.Availability = "last"
	doc.Turn.ServerTurnID = safeSnapshotString(state.TurnID)
	if active {
		doc.Availability = "active"
		doc.Turn.State = "active"
	}
	if snapshot != nil {
		ctx := state.Context
		doc.Turn.AcceptedSnapshot = &turnSnapshot{ID: snapshotID(*snapshot), Generation: safeSnapshotString(snapshot.MCPGeneration), CapturedAt: snapshot.CapturedAt.UTC().Format(time.RFC3339Nano), Profile: safeSnapshotString(snapshot.Profile), Workspace: safeSnapshotString(snapshot.Workspace.Name), ConversationID: safeSnapshotString(snapshot.ConversationID), Transport: safeSnapshotString(snapshot.Transport), WorkingSetHash: safeSnapshotString(ctx.WorkingSetHash), GoalID: safeSnapshotString(ctx.GoalID)}
		if snapshot.App != nil {
			doc.Turn.AcceptedSnapshot.AppScopeID = safeSnapshotString(snapshot.App.ScopeID)
		}
	} else {
		ctx := state.Context
		doc.Turn.AcceptedSnapshot = &turnSnapshot{Profile: safeSnapshotString(ctx.Profile), Workspace: safeSnapshotString(ctx.Workspace), ConversationID: safeSnapshotString(ctx.ConversationID), Transport: safeSnapshotString(ctx.Transport), Generation: safeSnapshotString(ctx.RuntimeGeneration), WorkingSetHash: safeSnapshotString(ctx.WorkingSetHash), AppScopeID: safeSnapshotString(ctx.AppScopeID), GoalID: safeSnapshotString(ctx.GoalID)}
	}
	timing := journalTurnTiming{}
	// A pruned or unhealthy chain cannot prove that its first lifecycle marker
	// belongs to the retained beginning of this turn, so terminal timing remains
	// explicitly unknown instead of inferred from a partial history.
	if journal.Health == "healthy" && journal.Retention == "complete" {
		timing = turnJournalTiming(entries, state.TurnID)
	}
	if !timing.startedAt.IsZero() {
		doc.Turn.StartedAt = timing.startedAt.UTC().Format(time.RFC3339Nano)
	}
	if !timing.terminalAt.IsZero() {
		doc.Turn.TerminalAt = timing.terminalAt.UTC().Format(time.RFC3339Nano)
	}
	if active && !started.IsZero() {
		if doc.Turn.StartedAt == "" {
			doc.Turn.StartedAt = started.UTC().Format(time.RFC3339Nano)
		}
		doc.Turn.Duration = statusNow().UTC().Sub(started).Round(time.Millisecond).String()
	} else if !timing.startedAt.IsZero() && !timing.terminalAt.IsZero() {
		doc.Turn.Duration = timing.terminalAt.Sub(timing.startedAt).Round(time.Millisecond).String()
	}
	for id, tool := range state.Tools {
		status := "running"
		if tool.Completed {
			status = "failed"
			if tool.Success {
				status = "completed"
			}
		}
		doc.Turn.Tools = append(doc.Turn.Tools, turnTool{ID: safeSnapshotString(id), Name: safeSnapshotString(tool.Name), State: status})
	}
	sort.Slice(doc.Turn.Tools, func(i, j int) bool { return doc.Turn.Tools[i].ID < doc.Turn.Tools[j].ID })
	if state.ElicitationPending {
		doc.Turn.Elicitation = "pending"
	} else {
		doc.Turn.Elicitation = "none"
	}
	doc.Turn.Usage = turnUsage{InputTokens: state.Usage.InputTokens, OutputTokens: state.Usage.OutputTokens, ThinkingTokens: state.Usage.ThinkingTokens, Status: "observed"}
	if state.Status == SemanticTurnFailed {
		doc.Turn.ErrorCategory = "server_turn_error"
	}
	if state.Status == SemanticTurnCancelled {
		doc.Turn.TerminalReason = "user_requested"
	}
	if state.Status == SemanticTurnCompleted {
		doc.Turn.TerminalReason = "server_turn_end"
	}
	doc.Turn.PendingNextTurn = turnNextContext{WorkingSetHash: safeSnapshotString(state.NextWorkingSetHash), AppScopeID: safeSnapshotString(state.NextAppScopeID), ConversationID: safeSnapshotString(state.NextConversation.ConversationID), State: "none"}
	if state.NextWorkingSetHash != "" || state.NextAppScopeID != "" || state.NextConversation.ConversationID != "" {
		doc.Turn.PendingNextTurn.State = "staged"
	}
	doc.Turn.Retry = turnRetry{Count: state.RetryCount, FallbackTransport: safeSnapshotString(state.FallbackTransport), State: "none"}
	if state.RetryCount > 0 || state.FallbackTransport != "" {
		doc.Turn.Retry.State = "observed"
	}
	components := make([]string, 0, len(state.TelemetryFailures))
	for _, failure := range state.TelemetryFailures {
		components = append(components, safeSnapshotString(failure.Component))
	}
	sort.Strings(components)
	attempts := c.turnRetryTelemetry(active)
	lastDecision := ""
	if len(attempts) > 0 {
		lastDecision = attempts[len(attempts)-1].Decision
	}
	doc.Turn.Telemetry = turnTelemetry{FailureCount: len(components), Components: components, Attempts: len(attempts), LastDecision: safeSnapshotString(lastDecision), State: "none"}
	if len(components) > 0 {
		doc.Turn.Telemetry.State = "failed"
	} else if len(attempts) > 0 {
		doc.Turn.Telemetry.State = "observed"
	}
	return doc
}

func (c *Client) turnReadState() (SemanticTurnState, *TurnRuntimeSnapshot, bool, time.Time, uint64) {
	c.semanticMu.Lock()
	state := cloneSemanticTurnState(c.semanticState)
	checkpoint := c.semanticJournalSequence
	c.semanticMu.Unlock()
	c.activeTurnMu.Lock()
	active := c.processing && c.turnRuntimeSnapshot != nil
	var snapshot *TurnRuntimeSnapshot
	if active {
		v := cloneTurnRuntimeSnapshot(*c.turnRuntimeSnapshot)
		snapshot = &v
	}
	started := c.turnStartedAt
	c.activeTurnMu.Unlock()
	return state, snapshot, active, started, checkpoint
}
func readTurnJournal(profile, workspace string, checkpoint uint64) ([]SemanticJournalEnvelope, turnJournalInfo) {
	out := turnJournalInfo{Checkpoint: checkpoint, Health: "missing", Retention: "unknown"}
	if _, err := os.Stat(semanticJournalFile(profile, workspace)); os.IsNotExist(err) {
		return nil, out
	}
	entries, err := readSemanticJournalChain(profile, workspace)
	if os.IsNotExist(err) {
		return nil, out
	}
	if err != nil {
		out.Health = "corrupt"
		return nil, out
	}
	if len(entries) == 0 {
		out.Health = "empty"
		return entries, out
	}
	out.LastSequence = entries[len(entries)-1].Sequence
	out.Health = "healthy"
	out.Retention = "complete"
	if entries[0].Sequence > 1 {
		out.Retention = "pruned"
	}
	return entries, out
}

type journalTurnTiming struct{ startedAt, terminalAt time.Time }

// turnJournalTiming examines only lifecycle markers. It decodes journal payloads
// locally so the JSON-unmarshalled envelope never leaks into diagnostic output.
func turnJournalTiming(entries []SemanticJournalEnvelope, serverTurnID string) journalTurnTiming {
	var started, terminal time.Time
	for _, entry := range entries {
		event, err := decodeSemanticJournalEvent(entry.Event)
		if err != nil {
			return journalTurnTiming{}
		}
		if event.Type == EventTurnAccepted {
			started, terminal = time.Time{}, time.Time{}
			continue
		}
		if serverTurnID != "" && event.TurnID != "" && event.TurnID != serverTurnID {
			continue
		}
		if event.Type == EventTurnStarted {
			started = event.OccurredAt
		}
		if terminalEvent(event.Type) && !started.IsZero() {
			terminal = event.OccurredAt
		}
	}
	return journalTurnTiming{startedAt: started, terminalAt: terminal}
}

func redactedDebugEvent(entry SemanticJournalEnvelope) debugEvent {
	event := entry.Event
	if decoded, err := decodeSemanticJournalEvent(event); err == nil {
		event = decoded
	}
	return debugEvent{ID: safeSnapshotString(event.ID), Sequence: entry.Sequence, ScopeSequence: entry.ScopeSequence, TurnID: safeSnapshotString(event.TurnID), Type: string(event.Type), OccurredAt: event.OccurredAt.UTC().Format(time.RFC3339Nano), Details: redactedSemanticDetails(event)}
}
func redactedSemanticDetails(event SemanticEvent) map[string]interface{} {
	out := map[string]interface{}{}
	switch p := event.Payload.(type) {
	case TurnAcceptedPayload:
		out["workspace"] = safeSnapshotString(p.Context.Workspace)
		out["conversationId"] = safeSnapshotString(p.Context.ConversationID)
		out["appScopeId"] = safeSnapshotString(p.Context.AppScopeID)
		out["workingSetHash"] = safeSnapshotString(p.Context.WorkingSetHash)
		out["transport"] = safeSnapshotString(p.Context.Transport)
	case ToolStartedPayload:
		out["toolId"] = safeSnapshotString(p.ToolID)
		out["name"] = safeSnapshotString(p.Name)
	case ToolCompletedPayload:
		out["toolId"] = safeSnapshotString(p.ToolID)
		out["success"] = p.Success
	case UsageUpdatedPayload:
		out["inputTokens"] = p.InputTokens
		out["outputTokens"] = p.OutputTokens
		out["thinkingTokens"] = p.ThinkingTokens
	case ElicitationRequestedPayload:
		out["kind"] = safeSnapshotString(p.Kind)
	case WorkingSetUpdatedPayload:
		out["hash"] = safeSnapshotString(p.Hash)
	case AppScopeChangedPayload:
		out["scopeId"] = safeSnapshotString(p.ScopeID)
	case ConversationUpdatedPayload:
		out["conversationId"] = safeSnapshotString(p.ConversationID)
		out["state"] = safeSnapshotString(p.State)
	case TransportRetryPayload:
		out["transport"] = safeSnapshotString(p.Transport)
		out["attempt"] = p.Attempt
		out["category"] = safeSnapshotString(p.Category)
	case RetryPayload:
		out["operation"] = safeSnapshotString(p.Operation)
		out["attempt"] = p.Attempt
		out["category"] = safeSnapshotString(p.Category)
		out["delayMillis"] = p.DelayMillis
	case TransportFallbackPayload:
		out["from"] = safeSnapshotString(p.From)
		out["to"] = safeSnapshotString(p.To)
		out["reason"] = safeSnapshotString(p.Reason)
	case TurnCancelledPayload:
		out["reason"] = safeSnapshotString(p.Reason)
	case TurnFailedPayload:
		out["code"] = safeSnapshotString(p.Code)
	case TurnCompletedPayload:
		out["reason"] = safeSnapshotString(p.Reason)
	case TelemetryFailedPayload:
		out["component"] = safeSnapshotString(p.Component)
		out["code"] = safeSnapshotString(p.Code)
	case ConnectionStateChangedPayload:
		out["state"] = safeSnapshotString(p.State)
	case AssistantDeltaPayload:
		out["content"] = "[omitted]"
	case AssistantCompletedPayload:
		out["reason"] = safeSnapshotString(p.Reason)
	}
	return out
}

func handleTurnCommand(_ context.Context, c *Client, args []string) (bool, error) {
	if len(args) > 1 || len(args) == 1 && args[0] != "--json" {
		return true, fmt.Errorf("usage: /turn [--json]")
	}
	doc := c.turnDocument()
	if len(args) == 1 {
		raw, err := json.Marshal(doc)
		if err != nil {
			return true, err
		}
		slashCommandPrintln(string(raw))
		return true, nil
	}
	slashCommandPrintf("turn: %s (%s)\n", doc.Turn.State, doc.Availability)
	slashCommandPrintf("server-turn=%s tools=%d elicitation=%s usage=%d/%d/%d journal=%s\n", emptyStatus(doc.Turn.ServerTurnID), len(doc.Turn.Tools), doc.Turn.Elicitation, doc.Turn.Usage.InputTokens, doc.Turn.Usage.OutputTokens, doc.Turn.Usage.ThinkingTokens, doc.Journal.Health)
	return true, nil
}
func handleDebugCommand(_ context.Context, c *Client, args []string) (bool, error) {
	if len(args) == 0 {
		return true, fmt.Errorf("usage: /debug status|on|off|tail [N] [--json]|event <event-id> [--json]")
	}
	sub := strings.ToLower(args[0])
	switch sub {
	case "status":
		if len(args) != 1 {
			return true, fmt.Errorf("usage: /debug status")
		}
		if c.debugLocal {
			slashCommandPrintln("debug: on")
		} else {
			slashCommandPrintln("debug: off")
		}
		return true, nil
	case "on", "off":
		if len(args) != 1 {
			return true, fmt.Errorf("usage: /debug %s", sub)
		}
		c.debugLocal = sub == "on"
		slashCommandPrintf("debug: %s\n", sub)
		return true, nil
	case "tail":
		return handleDebugTail(c, args[1:])
	case "event":
		return handleDebugEvent(c, args[1:])
	default:
		return true, fmt.Errorf("unknown /debug command")
	}
}
func debugJournalEntries(c *Client) ([]SemanticJournalEnvelope, debugJournal) {
	info := debugJournal{Health: "missing", Retention: "unknown"}
	if _, err := os.Stat(semanticJournalFile(c.opts.Profile, c.workspaceName)); os.IsNotExist(err) {
		return nil, info
	}
	entries, err := readSemanticJournalChain(c.opts.Profile, c.workspaceName)
	if os.IsNotExist(err) {
		return nil, info
	}
	if err != nil {
		info.Health = "corrupt"
		return nil, info
	}
	if len(entries) == 0 {
		info.Health = "empty"
		return entries, info
	}
	info.Health = "healthy"
	info.Retention = "complete"
	if len(entries) > 0 {
		info.LastSequence = entries[len(entries)-1].Sequence
		if entries[0].Sequence > 1 {
			info.Retention = "pruned"
		}
	}
	return entries, info
}
func handleDebugTail(c *Client, args []string) (bool, error) {
	n := debugTailDefault
	jsonOut := false
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		v, err := strconv.Atoi(arg)
		if err != nil || v < 1 {
			return true, fmt.Errorf("usage: /debug tail [N] [--json]")
		}
		n = v
	}
	if n > debugTailMaximum {
		n = debugTailMaximum
	}
	entries, journal := debugJournalEntries(c)
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	doc := debugDocument{SchemaVersion: DebugSchemaVersion, GeneratedAt: statusNow().UTC().Format(time.RFC3339Nano), Debug: map[bool]string{true: "on", false: "off"}[c.debugLocal], Journal: journal, Events: make([]debugEvent, 0, len(entries))}
	for _, entry := range entries {
		doc.Events = append(doc.Events, redactedDebugEvent(entry))
	}
	if jsonOut {
		raw, err := json.Marshal(doc)
		if err != nil {
			return true, err
		}
		slashCommandPrintln(string(raw))
		return true, nil
	}
	slashCommandPrintf("debug: %s journal=%s events=%d\n", doc.Debug, journal.Health, len(doc.Events))
	for _, e := range doc.Events {
		slashCommandPrintf("%d %s %s %s\n", e.Sequence, e.OccurredAt, e.Type, e.ID)
	}
	return true, nil
}
func handleDebugEvent(c *Client, args []string) (bool, error) {
	if len(args) < 1 || len(args) > 2 || (len(args) == 2 && args[1] != "--json") {
		return true, fmt.Errorf("usage: /debug event <event-id> [--json]")
	}
	entries, journal := debugJournalEntries(c)
	for _, entry := range entries {
		if entry.Event.ID != args[0] {
			continue
		}
		event := redactedDebugEvent(entry)
		if len(args) == 2 {
			raw, err := json.Marshal(struct {
				SchemaVersion int          `json:"schemaVersion"`
				GeneratedAt   string       `json:"generatedAt"`
				Journal       debugJournal `json:"journal"`
				Event         debugEvent   `json:"event"`
			}{DebugSchemaVersion, statusNow().UTC().Format(time.RFC3339Nano), journal, event})
			if err != nil {
				return true, err
			}
			slashCommandPrintln(string(raw))
		} else {
			details, err := json.Marshal(event.Details)
			if err != nil {
				return true, err
			}
			slashCommandPrintf("event: %s type=%s sequence=%d occurred=%s details=%s\n", event.ID, event.Type, event.Sequence, event.OccurredAt, details)
		}
		return true, nil
	}
	if journal.Health != "healthy" {
		return true, fmt.Errorf("debug journal is %s", journal.Health)
	}
	return true, fmt.Errorf("semantic event not found")
}
