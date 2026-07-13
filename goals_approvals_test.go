package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func goalApprovalClient(t *testing.T) *Client {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{}, Options{Profile: "default", Nirvana: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestGoalsCRUDScopeSnapshotAndPrivateStore(t *testing.T) {
	c := goalApprovalClient(t)
	c.workspaceName = "ws"
	c.conversationID = "conv"
	g, err := c.addGoal(" Ship safe goals ")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != GoalActive || g.Source != "manual" {
		t.Fatalf("bad goal %#v", g)
	}
	s := c.captureTurnRuntimeSnapshot(nil)
	if s.Goal == nil || s.Goal.ID != g.ID {
		t.Fatal("goal missing from future snapshot")
	}
	if _, err := c.transitionGoal(g.ID, GoalCompleted); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.transitionGoal(g.ID, GoalCompleted); got.Status != GoalCompleted {
		t.Fatal("done not idempotent")
	}
	info, err := os.Stat(goalsFile(c.opts.Profile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("goal permissions %v %v", info, err)
	}
	if info, err := os.Stat(profileDir(c.opts.Profile)); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("profile permissions %v %v", info, err)
	}
}
func TestGoalsOneActiveAndCorruptionRecovery(t *testing.T) {
	c := goalApprovalClient(t)
	c.workspaceName = "ws"
	g1, _ := c.addGoal("one")
	g2, _ := c.addGoal("two")
	goals := c.listGoals()
	for _, g := range goals {
		if g.ID == g1.ID && g.Status != GoalPending {
			t.Fatalf("old goal not pending: %#v", g)
		}
		if g.ID == g2.ID && g.Status != GoalActive {
			t.Fatal("new goal inactive")
		}
	}
	if err := os.WriteFile(goalsFile(c.opts.Profile), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if len(c.listGoals()) != 0 {
		t.Fatal("corrupt view should be empty")
	}
	matches, _ := filepath.Glob(goalsFile(c.opts.Profile) + ".corrupt-*")
	if len(matches) != 0 {
		t.Fatal("read-only list must not quarantine")
	}
	if _, err := c.addGoal("repaired"); err != nil {
		t.Fatal(err)
	}
	matches, _ = filepath.Glob(goalsFile(c.opts.Profile) + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatal("mutating command did not quarantine")
	}
}
func TestApprovalWorkspaceDeleteExactlyOnceAndNoPayloadLeak(t *testing.T) {
	c := goalApprovalClient(t)
	c.workspaceName = "active"
	if err := saveWorkspace(c.opts.Profile, newWorkspaceState("victim")); err != nil {
		t.Fatal(err)
	}
	r, err := c.requestWorkspaceDeleteApproval("victim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspaceFile(c.opts.Profile, "victim")); err != nil {
		t.Fatal("deleted before approval")
	}
	out, err := c.approveApproval(r.ID)
	if err != nil || out.Status != ApprovalExecuted {
		t.Fatalf("approve %#v %v", out, err)
	}
	if _, err := os.Stat(workspaceFile(c.opts.Profile, "victim")); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}
	if _, err := c.approveApproval(r.ID); err == nil {
		t.Fatal("replay allowed")
	}
	raw, _ := os.ReadFile(approvalsFile(c.opts.Profile))
	if strings.Contains(string(raw), "credential") || strings.Contains(string(raw), "secret") {
		t.Fatal("unsafe payload persisted")
	}
}
func TestApprovalRejectExpiryAndConcurrentClaim(t *testing.T) {
	c := goalApprovalClient(t)
	c.workspaceName = "active"
	_ = saveWorkspace(c.opts.Profile, newWorkspaceState("victim"))
	r, _ := c.requestWorkspaceDeleteApproval("victim")
	if got, err := c.rejectApproval(r.ID); err != nil || got.Status != ApprovalRejected {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspaceFile(c.opts.Profile, "victim")); err != nil {
		t.Fatal("reject executed")
	}
	r, _ = c.requestWorkspaceDeleteApproval("victim")
	old := goalApprovalNow
	goalApprovalNow = func() time.Time { return time.Now().UTC().Add(11 * time.Minute) }
	defer func() { goalApprovalNow = old }()
	if _, err := c.approveApproval(r.ID); err == nil {
		t.Fatal("expiry allowed")
	}
	goalApprovalNow = old
	r, _ = c.requestWorkspaceDeleteApproval("victim")
	var wg sync.WaitGroup
	successes := 0
	var mu sync.Mutex
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := c.approveApproval(r.ID); e == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("executed %d times", successes)
	}
}
func TestGoalAndApprovalCommandsOffline(t *testing.T) {
	c := goalApprovalClient(t)
	if _, err := handleGoalCommand(context.Background(), c, []string{"add", "offline"}); err != nil {
		t.Fatal(err)
	}
	if _, err := handleApprovalsCommand(context.Background(), c, nil); err != nil {
		t.Fatal(err)
	}
}
