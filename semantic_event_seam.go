package main

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
	if snapshot.App != nil {
		context.AppScopeID = snapshot.App.ScopeID
	} else {
		context.AppScopeID = strings.TrimSpace(stringify(c.appScope))
	}
	c.semanticMu.Lock()
	c.semanticState = NewSemanticTurnState()
	c.semanticSequence = 0
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
	if c.workspaceName != "" {
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
		} else {
			c.semanticJournalSequence = envelope.Sequence
		}
	}
	c.semanticState = next
}

func semanticWorkingSetHash(value interface{}) string {
	return canonicalHash(value)
}
