package main

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
)

func TestValidJSONCanaryIsNeverExposed(t *testing.T) {
	c := goalApprovalClient(t)
	bad := "Bearer top-secret"
	g := Goal{SchemaVersion: GoalSchemaVersion, ID: "goal-0123456789abcdef0123456789abcdef", Status: GoalActive, Title: bad, Source: "manual"}
	raw, _ := json.Marshal(goalsStore{SchemaVersion: GoalSchemaVersion, Goals: []Goal{g}})
	_ = os.MkdirAll(profileDir(c.opts.Profile), 0700)
	_ = os.WriteFile(goalsFile(c.opts.Profile), raw, 0600)
	if len(c.listGoals()) != 0 {
		t.Fatal("unsafe valid JSON exposed")
	}
	if c.statusDocument().Goals.Count != 0 {
		t.Fatal("unsafe goal reached status")
	}
	a := ApprovalRequest{SchemaVersion: ApprovalSchemaVersion, ID: "apr-0123456789abcdef0123456789abcdef", ActionClass: "workspace_delete", Summary: bad, CreatedAt: "x", ExpiresAt: "x", Risk: "high", PayloadHash: "sha256:x", Preview: "x", ActionRef: "x", Status: ApprovalPending}
	raw, _ = json.Marshal(approvalsStore{SchemaVersion: ApprovalSchemaVersion, Requests: []ApprovalRequest{a}})
	_ = os.WriteFile(approvalsFile(c.opts.Profile), raw, 0600)
	if len(c.listApprovals()) != 0 {
		t.Fatal("unsafe approval exposed")
	}
}
func TestConcurrentGoalAddsKeepOneActive(t *testing.T) {
	c := goalApprovalClient(t)
	c.workspaceName = "ws"
	c.conversationID = "conv"
	c2 := goalApprovalClient(t)
	c2.opts.Profile = c.opts.Profile
	c2.workspaceName = "ws"
	c2.conversationID = "conv"
	var wg sync.WaitGroup
	for _, x := range []*Client{c, c2} {
		wg.Add(1)
		go func(x *Client) { defer wg.Done(); _, _ = x.addGoal("safe") }(x)
	}
	wg.Wait()
	s, _ := readGoalsView(c.opts.Profile)
	active := 0
	for _, g := range s.Goals {
		if g.Status == GoalActive {
			active++
		}
	}
	if active != 1 || len(s.Goals) != 2 {
		t.Fatalf("active=%d total=%d", active, len(s.Goals))
	}
}
func TestNonRegularStoreRejected(t *testing.T) {
	c := goalApprovalClient(t)
	_ = os.MkdirAll(goalsFile(c.opts.Profile), 0700)
	if _, err := c.addGoal("safe"); err == nil {
		t.Fatal("directory store accepted")
	}
}
