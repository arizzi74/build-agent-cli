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
	context := TurnContext{
		Profile:        c.opts.Profile,
		Workspace:      c.workspaceName,
		ConversationID: c.conversationID,
		WorkingSetHash: semanticWorkingSetHash(c.workingSet),
	}
	if c.currentApp != nil {
		context.AppScopeID = c.currentApp.ScopeID
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
	c.semanticState = next
}

func semanticWorkingSetHash(value interface{}) string {
	if value == nil {
		return ""
	}
	// This is an intentionally non-secret placeholder identity. Unit 4 will
	// replace it with the immutable runtime snapshot checksum.
	return fmt.Sprintf("items:%d", len(fmt.Sprint(value)))
}
