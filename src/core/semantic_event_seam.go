package core

import (
	"fmt"
	"strings"
	"time"
)

// beginSemanticTurn establishes the Nirvana-only compatibility seam. It observes
// normalized events only; existing rendering and persistence remain the source
// of user-visible behavior while other transports are not initialized here.
func (c *Client) beginSemanticTurn() {
	c.beginSemanticTurnSnapshot(c.captureTurnRuntimeSnapshot(nil))
}

func (c *Client) beginSemanticTurnSnapshot(snapshot TurnRuntimeSnapshot) {
	context := TurnContext{
		Profile:           snapshot.Profile,
		Workspace:         snapshot.Workspace.Name,
		ConversationID:    snapshot.ConversationID,
		WorkingSetHash:    snapshot.WorkingSetHash,
		RuntimeGeneration: snapshot.MCPGeneration,
		Transport:         snapshot.Transport,
	}
	if snapshot.Goal != nil {
		context.GoalID = snapshot.Goal.ID
	}
	if snapshot.App != nil {
		context.AppScopeID = snapshot.App.ScopeID
	} else {
		context.AppScopeID = strings.TrimSpace(stringify(c.appScope))
	}
	c.semanticMu.Lock()
	c.semanticState = NewSemanticTurnState()
	c.semanticSequence = 0
	c.journalBoundaryMissed = false
	if c.semanticLifecycleID == "" {
		c.semanticLifecycleID = uuidV4Compact()
	}
	c.semanticMu.Unlock()
	c.emitSemanticEvent(EventTurnAccepted, "", TurnAcceptedPayload{Context: context})
}

func (c *Client) emitSemanticEvent(eventType SemanticEventType, turnID string, payload any) {
	if !c.opts.Nirvana {
		return
	}
	c.semanticMu.Lock()
	defer c.semanticMu.Unlock()
	c.semanticSequence++
	c.semanticEventIDSequence++
	if c.semanticLifecycleID == "" {
		c.semanticLifecycleID = uuidV4Compact()
	}
	if turnID == "" {
		turnID = c.semanticState.TurnID
	}
	event := SemanticEvent{
		Version:    SemanticEventVersion,
		ID:         fmt.Sprintf("se-%s-%d", c.semanticLifecycleID, c.semanticEventIDSequence),
		Sequence:   c.semanticSequence,
		TurnID:     turnID,
		Type:       eventType,
		OccurredAt: time.Now().UTC(),
		Metadata:   map[string]string{"source": "nirvana_compatibility_seam"},
		Payload:    payload,
	}
	next, err := Apply(c.semanticState, event)
	if err != nil {
		// This seam is observational: reducer errors never alter the established
		// transport, renderer, persistence, or telemetry behavior.
		c.debugf("semantic event ignored: %v\n", err)
		return
	}
	journalThisEvent := c.workspaceName != ""
	if eventType != EventTurnAccepted && (c.semanticState.Status == SemanticTurnAccepted || c.semanticState.Status == SemanticTurnStarted) && c.journalBoundaryMissed {
		// If the accepted boundary could not be persisted (for example because
		// workspace selection completed after the turn began), skip the whole
		// turn. Appending later retry/tool/delta events would create a journal
		// that cannot be safely replayed after restart.
		journalThisEvent = false
	}
	if eventType == EventTurnAccepted && !journalThisEvent {
		c.journalBoundaryMissed = true
	}
	if journalThisEvent {
		conversationID := c.conversationID
		if accepted, ok := payload.(TurnAcceptedPayload); ok && accepted.Context.ConversationID != "" {
			conversationID = accepted.Context.ConversationID
		}
		envelope, journalErr := appendSemanticJournalEvent(c.opts.Profile, c.workspaceName, conversationID, event)
		if journalErr != nil {
			// Journal persistence is deliberately local-only and must not change
			// a live ServiceNow turn. The event was validated/redacted before an
			// append is attempted, so a failure cannot leak a raw transport frame.
			c.debugf("semantic journal append ignored: %v\n", journalErr)
			if eventType == EventTurnAccepted {
				c.journalBoundaryMissed = true
			}
		} else {
			c.semanticJournalSequence = envelope.Sequence
			if eventType == EventTurnAccepted {
				c.journalBoundaryMissed = false
			}
		}
	}
	c.semanticState = next
}

func (c *Client) activeSemanticTurn() bool {
	c.semanticMu.Lock()
	defer c.semanticMu.Unlock()
	return c.semanticState.Status == SemanticTurnAccepted || c.semanticState.Status == SemanticTurnStarted
}
func (c *Client) emitRetryAttempted(operation string, attempt int, category string) {
	if c.activeSemanticTurn() {
		c.publishTurnPresentation(turnPresentationEvent{Kind: turnPresentationRetry, Text: fmt.Sprintf("Retrying %s (attempt %d, %s)", safeTelemetryLabel(operation), attempt, safeTelemetryLabel(category))})
		c.emitSemanticEvent(EventRetryAttempted, "", RetryPayload{Operation: safeTelemetryLabel(operation), Attempt: attempt, Category: safeTelemetryLabel(category)})
	}
}
func (c *Client) emitRetryScheduled(operation string, attempt int, category string, delay time.Duration) {
	if c.activeSemanticTurn() {
		c.publishTurnPresentation(turnPresentationEvent{Kind: turnPresentationRetry, Text: fmt.Sprintf("Retry scheduled for %s in %s (attempt %d, %s)", safeTelemetryLabel(operation), delay.Round(time.Millisecond), attempt, safeTelemetryLabel(category))})
		c.emitSemanticEvent(EventRetryScheduled, "", RetryPayload{Operation: safeTelemetryLabel(operation), Attempt: attempt, Category: safeTelemetryLabel(category), DelayMillis: delay.Milliseconds()})
	}
}
func (c *Client) emitRetryExhausted(operation string, attempt int, category, reason string) {
	if c.activeSemanticTurn() {
		c.publishTurnPresentation(turnPresentationEvent{Kind: turnPresentationRetry, Text: fmt.Sprintf("Retries exhausted for %s after attempt %d (%s)", safeTelemetryLabel(operation), attempt, safeTelemetryLabel(category+"_"+reason))})
		c.emitSemanticEvent(EventRetryExhausted, "", RetryPayload{Operation: safeTelemetryLabel(operation), Attempt: attempt, Category: safeTelemetryLabel(category + "_" + reason)})
	}
}
func (c *Client) emitTransportFallback(from, to, reason string) {
	if c.canFallback(from, to, reason).Allowed {
		c.publishTurnPresentation(turnPresentationEvent{Kind: turnPresentationFallback, Text: fmt.Sprintf("Transport fallback: %s to %s (%s)", safeTelemetryLabel(from), safeTelemetryLabel(to), safeTelemetryLabel(reason))})
		c.emitSemanticEvent(EventTransportFallback, "", TransportFallbackPayload{From: from, To: to, Reason: reason})
	}
}

func semanticWorkingSetHash(value interface{}) string {
	return canonicalHash(value)
}
