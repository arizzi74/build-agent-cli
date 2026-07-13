package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fileState(path string) ([]byte, time.Time) {
	b, _ := os.ReadFile(path)
	i, _ := os.Stat(path)
	if i == nil {
		return b, time.Time{}
	}
	return b, i.ModTime()
}
func TestGoalApprovalDiagnosticViewsNeverRepairCorruptStores(t *testing.T) {
	c := goalApprovalClient(t)
	_ = os.MkdirAll(profileDir(c.opts.Profile), 0700)
	_ = os.WriteFile(goalsFile(c.opts.Profile), []byte("bad"), 0600)
	_ = os.WriteFile(approvalsFile(c.opts.Profile), []byte("bad"), 0600)
	gb, gm := fileState(goalsFile(c.opts.Profile))
	ab, am := fileState(approvalsFile(c.opts.Profile))
	_ = c.statusDocument()
	_ = c.turnDocument()
	_ = c.offlineSearch("goal", 10)
	_, _, _ = c.conversationExportPayloads(time.Now())
	_, _, _ = c.supportBundlePayloads(time.Now())
	gb2, gm2 := fileState(goalsFile(c.opts.Profile))
	ab2, am2 := fileState(approvalsFile(c.opts.Profile))
	if string(gb) != string(gb2) || string(ab) != string(ab2) || !gm.Equal(gm2) || !am.Equal(am2) {
		t.Fatal("diagnostic read mutated corrupt stores")
	}
	m, _ := filepath.Glob(goalsFile(c.opts.Profile) + ".corrupt-*")
	if len(m) != 0 {
		t.Fatal("diagnostic created quarantine")
	}
}
func TestApprovalInterruptedExecutionNeverReplays(t *testing.T) {
	c := goalApprovalClient(t)
	c.workspaceName = "active"
	_ = saveWorkspace(c.opts.Profile, newWorkspaceState("victim"))
	r, _ := c.requestWorkspaceDeleteApproval("victim")
	s, _ := readApprovalsView(c.opts.Profile)
	for i := range s.Requests {
		if s.Requests[i].ID == r.ID {
			s.Requests[i].Status = ApprovalExecuting
			s.Requests[i].ClaimedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
	}
	if err := privateAtomicWrite(approvalsFile(c.opts.Profile), s); err != nil {
		t.Fatal(err)
	}
	if _, err := c.approveApproval(r.ID); err == nil {
		t.Fatal("uncertain execution replayed")
	}
	if _, err := os.Stat(workspaceFile(c.opts.Profile, "victim")); err != nil {
		t.Fatal("replayed delete")
	}
	s, _ = readApprovalsView(c.opts.Profile)
	if s.Requests[0].Status != ApprovalFailed {
		t.Fatalf("got %s", s.Requests[0].Status)
	}
}
func TestRetentionPreservesActionable(t *testing.T) {
	s := goalsStore{SchemaVersion: GoalSchemaVersion}
	s.Goals = append(s.Goals, Goal{ID: "goal-active", Status: GoalActive, UpdatedAt: "2000"})
	for i := 0; i < 200; i++ {
		s.Goals = append(s.Goals, Goal{ID: "goal-x", Status: GoalCompleted, UpdatedAt: time.Now().Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)})
	}
	trimGoals(&s)
	found := false
	for _, g := range s.Goals {
		found = found || g.Status == GoalActive
	}
	if !found {
		t.Fatal("active goal pruned")
	}
}
func TestSnapshotGoalIsDeepSafeCopy(t *testing.T) {
	c := goalApprovalClient(t)
	c.workspaceName = "ws"
	g, _ := c.addGoal("safe")
	s := c.captureTurnRuntimeSnapshot(nil)
	g.Checklist = []string{"Bearer secret"}
	g.Title = "token=bad"
	if s.Goal == nil || s.Goal.ID == "" || s.Goal.Title != "safe" {
		t.Fatal("snapshot goal mutated")
	}
}
