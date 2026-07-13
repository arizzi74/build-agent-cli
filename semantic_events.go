package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SemanticEventVersion is the stable schema version for the internal lifecycle
// envelope. It intentionally contains only normalized, redaction-safe data.
const SemanticEventVersion = 1

type SemanticEventType string

const (
	EventConnectionStateChanged SemanticEventType = "connection_state_changed"
	EventTurnAccepted           SemanticEventType = "turn_accepted"
	EventTurnStarted            SemanticEventType = "turn_started"
	EventAssistantDelta         SemanticEventType = "assistant_delta"
	EventAssistantCompleted     SemanticEventType = "assistant_completed"
	EventToolStarted            SemanticEventType = "tool_started"
	EventToolCompleted          SemanticEventType = "tool_completed"
	EventElicitationRequested   SemanticEventType = "elicitation_requested"
	EventUsageUpdated           SemanticEventType = "usage_updated"
	EventWorkingSetUpdated      SemanticEventType = "working_set_updated"
	EventAppScopeChanged        SemanticEventType = "app_scope_changed"
	EventConversationUpdated    SemanticEventType = "conversation_updated"
	EventTransportRetry         SemanticEventType = "transport_retry"
	EventTransportFallback      SemanticEventType = "transport_fallback"
	EventTurnCancelled          SemanticEventType = "turn_cancelled"
	EventTurnFailed             SemanticEventType = "turn_failed"
	EventTurnCompleted          SemanticEventType = "turn_completed"
	EventTelemetryFailed        SemanticEventType = "telemetry_failed"
)

// SemanticEvent is deliberately smaller than any transport frame. Metadata is
// restricted to safe diagnostic labels; raw requests, headers, tokens, cookies,
// and backend payloads must never enter this envelope.
type SemanticEvent struct {
	Version    int               `json:"version"`
	ID         string            `json:"id"`
	Sequence   uint64            `json:"sequence"`
	TurnID     string            `json:"turnId,omitempty"`
	Type       SemanticEventType `json:"type"`
	OccurredAt time.Time         `json:"occurredAt"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Payload    any               `json:"payload,omitempty"`
}

type ConnectionStateChangedPayload struct {
	State string `json:"state"`
}
type TurnContext struct {
	Profile           string `json:"profile,omitempty"`
	Workspace         string `json:"workspace,omitempty"`
	ConversationID    string `json:"conversationId,omitempty"`
	AppScopeID        string `json:"appScopeId,omitempty"`
	WorkingSetHash    string `json:"workingSetHash,omitempty"`
	RuntimeGeneration string `json:"runtimeGeneration,omitempty"`
	Transport         string `json:"transport,omitempty"`
}
type TurnAcceptedPayload struct {
	Context TurnContext `json:"context"`
}
type AssistantDeltaPayload struct {
	Delta string `json:"delta"`
}
type AssistantCompletedPayload struct {
	Reason string `json:"reason,omitempty"`
}
type ToolStartedPayload struct {
	ToolID string `json:"toolId"`
	Name   string `json:"name,omitempty"`
}
type ToolCompletedPayload struct {
	ToolID  string `json:"toolId"`
	Success bool   `json:"success"`
}
type ElicitationRequestedPayload struct {
	Kind string `json:"kind,omitempty"`
}
type UsageUpdatedPayload struct {
	InputTokens    int64 `json:"inputTokens"`
	OutputTokens   int64 `json:"outputTokens"`
	ThinkingTokens int64 `json:"thinkingTokens"`
}
type WorkingSetUpdatedPayload struct {
	Hash string `json:"hash,omitempty"`
}
type AppScopeChangedPayload struct {
	ScopeID string `json:"scopeId,omitempty"`
}
type ConversationUpdatedPayload struct {
	ConversationID string `json:"conversationId,omitempty"`
	Title          string `json:"title,omitempty"`
	State          string `json:"state,omitempty"`
}
type TransportRetryPayload struct {
	Transport string `json:"transport,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	Category  string `json:"category,omitempty"`
}
type TransportFallbackPayload struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}
type TurnCancelledPayload struct {
	Reason string `json:"reason,omitempty"`
}
type TurnFailedPayload struct {
	Code string `json:"code,omitempty"`
}
type TurnCompletedPayload struct {
	Reason string `json:"reason,omitempty"`
}
type TelemetryFailedPayload struct {
	Component string `json:"component,omitempty"`
	Code      string `json:"code,omitempty"`
}

type SemanticTurnStatus string

const (
	SemanticTurnIdle      SemanticTurnStatus = "idle"
	SemanticTurnAccepted  SemanticTurnStatus = "accepted"
	SemanticTurnStarted   SemanticTurnStatus = "started"
	SemanticTurnCancelled SemanticTurnStatus = "cancelled"
	SemanticTurnFailed    SemanticTurnStatus = "failed"
	SemanticTurnCompleted SemanticTurnStatus = "completed"
)

type SemanticToolState struct {
	Name      string
	Completed bool
	Success   bool
}

// SemanticTurnState is reducer-owned state. Context is captured at acceptance
// and never changed; context update events are staged for the next turn.
type SemanticTurnState struct {
	ConnectionState string
	TurnID          string
	Status          SemanticTurnStatus
	Context         TurnContext

	AssistantText      string
	AssistantCompleted bool
	Tools              map[string]SemanticToolState
	ElicitationPending bool
	Usage              UsageUpdatedPayload

	NextWorkingSetHash string
	NextAppScopeID     string
	NextConversation   ConversationUpdatedPayload
	RetryCount         int
	FallbackTransport  string
	TelemetryFailures  []TelemetryFailedPayload

	terminalSignature string
	seenEventIDs      map[string]string
}

func NewSemanticTurnState() SemanticTurnState {
	return SemanticTurnState{Status: SemanticTurnIdle, Tools: map[string]SemanticToolState{}, seenEventIDs: map[string]string{}}
}

func (s SemanticTurnState) Terminal() bool {
	return s.Status == SemanticTurnCancelled || s.Status == SemanticTurnFailed || s.Status == SemanticTurnCompleted
}

func (e SemanticEvent) validate() error {
	if e.Version != SemanticEventVersion {
		return fmt.Errorf("unsupported semantic event version %d", e.Version)
	}
	if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(string(e.Type)) == "" {
		return errors.New("semantic event requires id and type")
	}
	for k, v := range e.Metadata {
		lowerKey, lowerValue := strings.ToLower(k), strings.ToLower(v)
		if strings.Contains(lowerKey, "token") || strings.Contains(lowerKey, "secret") || strings.Contains(lowerKey, "password") || strings.Contains(lowerKey, "cookie") || strings.Contains(lowerKey, "authorization") {
			return fmt.Errorf("unsafe semantic event metadata key %q", k)
		}
		if strings.Contains(lowerValue, "bearer ") || strings.Contains(lowerValue, "basic ") || strings.Contains(lowerValue, "cookie:") || strings.Contains(lowerValue, "set-cookie:") {
			return fmt.Errorf("unsafe semantic event metadata value for key %q", k)
		}
	}
	return nil
}

func terminalEvent(t SemanticEventType) bool {
	return t == EventTurnCancelled || t == EventTurnFailed || t == EventTurnCompleted
}

func eventSignature(e SemanticEvent) (string, error) {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return "", err
	}
	return string(e.Type) + ":" + string(payload), nil
}

func cloneSemanticTurnState(state SemanticTurnState) SemanticTurnState {
	clone := state
	clone.Tools = make(map[string]SemanticToolState, len(state.Tools))
	for id, tool := range state.Tools {
		clone.Tools[id] = tool
	}
	clone.TelemetryFailures = append([]TelemetryFailedPayload(nil), state.TelemetryFailures...)
	clone.seenEventIDs = make(map[string]string, len(state.seenEventIDs))
	for id, signature := range state.seenEventIDs {
		clone.seenEventIDs[id] = signature
	}
	return clone
}

func activeTurnStatus(status SemanticTurnStatus) bool {
	return status == SemanticTurnAccepted || status == SemanticTurnStarted
}

// Apply reduces one event without mutating its input. Event IDs are deduplicated:
// replaying an identical event is idempotent, while reusing an ID for different
// content is rejected. Invalid transitions return an equivalent cloned state.
func Apply(state SemanticTurnState, event SemanticEvent) (SemanticTurnState, error) {
	state = cloneSemanticTurnState(state)
	if state.Tools == nil {
		state.Tools = map[string]SemanticToolState{}
	}
	if state.seenEventIDs == nil {
		state.seenEventIDs = map[string]string{}
	}
	if err := event.validate(); err != nil {
		return state, err
	}
	signature, err := eventSignature(event)
	if err != nil {
		return state, err
	}
	if previous, seen := state.seenEventIDs[event.ID]; seen {
		if previous == signature {
			return state, nil
		}
		return state, fmt.Errorf("semantic event id %q was reused with different content", event.ID)
	}

	if event.Type != EventTurnAccepted && state.TurnID != "" && event.TurnID != "" && event.TurnID != state.TurnID {
		return state, fmt.Errorf("event turn id %q does not match active turn %q", event.TurnID, state.TurnID)
	}
	if state.Terminal() {
		if terminalEvent(event.Type) && signature == state.terminalSignature {
			return state, nil
		}
		return state, fmt.Errorf("turn already reached terminal outcome %q", state.Status)
	}

	if terminalEvent(event.Type) {
		if !activeTurnStatus(state.Status) {
			return state, errors.New("terminal outcome before turn acceptance")
		}
		if event.Type != EventTurnCancelled && state.Status != SemanticTurnStarted {
			return state, errors.New("terminal outcome before turn start")
		}
		if event.Type == EventTurnCompleted && (state.Status != SemanticTurnStarted || !state.AssistantCompleted) {
			return state, errors.New("turn completion requires an active completed assistant response")
		}
		state.terminalSignature = signature
	}

	switch event.Type {
	case EventConnectionStateChanged:
		p, ok := event.Payload.(ConnectionStateChangedPayload)
		if !ok {
			return state, payloadTypeError(event.Type)
		}
		state.ConnectionState = p.State
	case EventTurnAccepted:
		p, ok := event.Payload.(TurnAcceptedPayload)
		if !ok {
			return state, payloadTypeError(event.Type)
		}
		if state.Status != SemanticTurnIdle {
			return state, errors.New("turn already accepted")
		}
		state.TurnID, state.Context, state.Status = event.TurnID, p.Context, SemanticTurnAccepted
	case EventTurnStarted:
		if state.Status != SemanticTurnAccepted && state.Status != SemanticTurnStarted {
			return state, errors.New("turn must be accepted before start")
		}
		if state.TurnID == "" {
			state.TurnID = event.TurnID
		} else if event.TurnID != "" && event.TurnID != state.TurnID {
			return state, fmt.Errorf("event turn id %q does not match active turn %q", event.TurnID, state.TurnID)
		}
		state.Status = SemanticTurnStarted
	case EventAssistantDelta:
		p, ok := event.Payload.(AssistantDeltaPayload)
		if !ok {
			return state, payloadTypeError(event.Type)
		}
		if state.Status != SemanticTurnStarted {
			return state, errors.New("assistant delta before turn start")
		}
		if state.AssistantCompleted {
			return state, errors.New("assistant delta after assistant completion")
		}
		state.AssistantText += p.Delta
	case EventAssistantCompleted:
		if state.Status != SemanticTurnStarted {
			return state, errors.New("assistant completion before turn start")
		}
		if state.AssistantCompleted {
			return state, errors.New("assistant already completed")
		}
		state.AssistantCompleted = true
	case EventToolStarted:
		p, ok := event.Payload.(ToolStartedPayload)
		if !ok || strings.TrimSpace(p.ToolID) == "" {
			return state, payloadTypeError(event.Type)
		}
		if state.Status != SemanticTurnStarted {
			return state, errors.New("tool start outside active turn")
		}
		if existing, exists := state.Tools[p.ToolID]; exists {
			if existing.Name == p.Name && !existing.Completed {
				break
			}
			return state, fmt.Errorf("conflicting duplicate tool start for %q", p.ToolID)
		}
		state.Tools[p.ToolID] = SemanticToolState{Name: p.Name}
	case EventToolCompleted:
		p, ok := event.Payload.(ToolCompletedPayload)
		if !ok || strings.TrimSpace(p.ToolID) == "" {
			return state, payloadTypeError(event.Type)
		}
		if state.Status != SemanticTurnStarted {
			return state, errors.New("tool completion outside active turn")
		}
		tool, exists := state.Tools[p.ToolID]
		if !exists {
			return state, fmt.Errorf("tool completion for unknown tool %q", p.ToolID)
		}
		if tool.Completed {
			if tool.Success == p.Success {
				break
			}
			return state, fmt.Errorf("conflicting duplicate tool completion for %q", p.ToolID)
		}
		tool.Completed, tool.Success = true, p.Success
		state.Tools[p.ToolID] = tool
	case EventElicitationRequested:
		if state.Status != SemanticTurnStarted {
			return state, errors.New("elicitation outside active turn")
		}
		state.ElicitationPending = true
	case EventUsageUpdated:
		p, ok := event.Payload.(UsageUpdatedPayload)
		if !ok {
			return state, payloadTypeError(event.Type)
		}
		if state.Status != SemanticTurnStarted {
			return state, errors.New("usage update outside active turn")
		}
		if p.InputTokens < 0 || p.OutputTokens < 0 || p.ThinkingTokens < 0 {
			return state, errors.New("usage counters cannot be negative")
		}
		if p.InputTokens < state.Usage.InputTokens || p.OutputTokens < state.Usage.OutputTokens || p.ThinkingTokens < state.Usage.ThinkingTokens {
			return state, errors.New("usage counters must be monotonic")
		}
		state.Usage = p
	case EventWorkingSetUpdated:
		p, ok := event.Payload.(WorkingSetUpdatedPayload)
		if !ok {
			return state, payloadTypeError(event.Type)
		}
		state.NextWorkingSetHash = p.Hash
	case EventAppScopeChanged:
		p, ok := event.Payload.(AppScopeChangedPayload)
		if !ok {
			return state, payloadTypeError(event.Type)
		}
		state.NextAppScopeID = p.ScopeID
	case EventConversationUpdated:
		p, ok := event.Payload.(ConversationUpdatedPayload)
		if !ok {
			return state, payloadTypeError(event.Type)
		}
		state.NextConversation = p
	case EventTransportRetry:
		if state.Status != SemanticTurnStarted {
			return state, errors.New("transport retry outside active turn")
		}
		state.RetryCount++
	case EventTransportFallback:
		p, ok := event.Payload.(TransportFallbackPayload)
		if !ok {
			return state, payloadTypeError(event.Type)
		}
		state.FallbackTransport = p.To
	case EventTurnCancelled:
		state.Status = SemanticTurnCancelled
	case EventTurnFailed:
		state.Status = SemanticTurnFailed
	case EventTurnCompleted:
		state.Status = SemanticTurnCompleted
	case EventTelemetryFailed:
		p, ok := event.Payload.(TelemetryFailedPayload)
		if !ok {
			return state, payloadTypeError(event.Type)
		}
		state.TelemetryFailures = append(state.TelemetryFailures, p)
	default:
		return state, fmt.Errorf("unknown semantic event type %q", event.Type)
	}
	state.seenEventIDs[event.ID] = signature
	return state, nil
}

func Reduce(events []SemanticEvent) (SemanticTurnState, error) {
	state := NewSemanticTurnState()
	for _, event := range events {
		var err error
		state, err = Apply(state, event)
		if err != nil {
			return state, err
		}
	}
	return state, nil
}

func payloadTypeError(eventType SemanticEventType) error {
	return fmt.Errorf("invalid payload for %s", eventType)
}
