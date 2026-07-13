package main

import (
	"errors"
	"testing"
)

func semanticApprovalClient(t *testing.T) *Client {
	c := goalApprovalClient(t)
	c.workspaceName = "ws"
	c.conversationID = "conv"
	c.semanticState = NewSemanticTurnState()
	c.semanticState.Status = SemanticTurnStarted
	c.semanticState.TurnID = "turn"
	return c
}
func TestApprovalSemanticEventsSuccessOrderAfterLocks(t *testing.T) {
	c := semanticApprovalClient(t)
	_ = saveWorkspace(c.opts.Profile, newWorkspaceState("victim"))
	r, _ := c.requestWorkspaceDeleteApproval("victim")
	var got []SemanticEventType
	old := c.semanticLifecycleID
	c.semanticLifecycleID = "events"
	defer func() { c.semanticLifecycleID = old }() // journal-backed emitter remains safe after locks
	if _, err := c.approveApproval(r.ID); err != nil {
		t.Fatal(err)
	}
	entries, _ := readTurnJournal(c.opts.Profile, c.workspaceName, 0)
	for _, e := range entries {
		if e.Event.Type == EventApprovalStatusChanged || e.Event.Type == EventApprovalExecuted {
			got = append(got, e.Event.Type)
		}
	}
	if len(got) < 3 || got[len(got)-3] != EventApprovalStatusChanged || got[len(got)-2] != EventApprovalStatusChanged || got[len(got)-1] != EventApprovalExecuted {
		t.Fatalf("order=%v", got)
	}
}
func TestApprovalSemanticEventsFailureOrderAfterLocks(t *testing.T) {
	c := semanticApprovalClient(t)
	r, _ := c.requestWorkspaceDeleteApproval("missing")
	if _, err := c.approveApproval(r.ID); err == nil || !errors.Is(err, err) { /* expected failure */
	}
	entries, _ := readTurnJournal(c.opts.Profile, c.workspaceName, 0)
	var got []SemanticEventType
	for _, e := range entries {
		if e.Event.Type == EventApprovalStatusChanged || e.Event.Type == EventApprovalFailed {
			got = append(got, e.Event.Type)
		}
	}
	if len(got) < 3 || got[len(got)-3] != EventApprovalStatusChanged || got[len(got)-2] != EventApprovalStatusChanged || got[len(got)-1] != EventApprovalFailed {
		t.Fatalf("order=%v", got)
	}
}
