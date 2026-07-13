package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// GoalSchemaVersion and ApprovalSchemaVersion version the private, local-only stores.
const (
	GoalSchemaVersion     = 1
	ApprovalSchemaVersion = 1
	goalRetention         = 100
	approvalRetention     = 100
	goalActionableCap     = 256
	approvalActionableCap = 256
)

type GoalStatus string

const (
	GoalPending   GoalStatus = "pending"
	GoalActive    GoalStatus = "active"
	GoalCompleted GoalStatus = "completed"
	GoalCancelled GoalStatus = "cancelled"
	GoalBlocked   GoalStatus = "blocked"
)

type Goal struct {
	SchemaVersion  int        `json:"schemaVersion"`
	ID             string     `json:"id"`
	CreatedAt      string     `json:"createdAt"`
	UpdatedAt      string     `json:"updatedAt"`
	Status         GoalStatus `json:"status"`
	Title          string     `json:"title"`
	Summary        string     `json:"summary,omitempty"`
	Profile        string     `json:"profile,omitempty"`
	Workspace      string     `json:"workspace,omitempty"`
	ConversationID string     `json:"conversationId,omitempty"`
	AppScopeID     string     `json:"appScopeId,omitempty"`
	TurnID         string     `json:"turnId,omitempty"`
	SnapshotID     string     `json:"snapshotId,omitempty"`
	Checklist      []string   `json:"checklist,omitempty"`
	Progress       int        `json:"progress,omitempty"`
	Source         string     `json:"source"`
}
type goalsStore struct {
	SchemaVersion int    `json:"schemaVersion"`
	Goals         []Goal `json:"goals"`
}

type ApprovalStatus string

const (
	ApprovalPending   ApprovalStatus = "pending"
	ApprovalApproved  ApprovalStatus = "approved"
	ApprovalExecuting ApprovalStatus = "executing"
	ApprovalRejected  ApprovalStatus = "rejected"
	ApprovalExpired   ApprovalStatus = "expired"
	ApprovalExecuted  ApprovalStatus = "executed"
	ApprovalFailed    ApprovalStatus = "failed"
)

type ApprovalRequest struct {
	SchemaVersion  int            `json:"schemaVersion"`
	ID             string         `json:"id"`
	ActionClass    string         `json:"actionClass"`
	Summary        string         `json:"summary"`
	CreatedAt      string         `json:"createdAt"`
	ExpiresAt      string         `json:"expiresAt"`
	Profile        string         `json:"profile,omitempty"`
	Workspace      string         `json:"workspace,omitempty"`
	ConversationID string         `json:"conversationId,omitempty"`
	Risk           string         `json:"risk"`
	PayloadHash    string         `json:"payloadHash"`
	Preview        string         `json:"preview"`
	Status         ApprovalStatus `json:"status"`
	ExecutedAt     string         `json:"executedAt,omitempty"`
	FailedAt       string         `json:"failedAt,omitempty"`
	Result         string         `json:"result,omitempty"`
	// ActionRef is a normalized safe identifier, never credentials or content.
	ActionRef string `json:"actionRef,omitempty"`
	// ClaimedAt records an uncertain execution boundary. An interrupted execution is
	// never replayed: it is converted to failed on the next mutating load.
	ClaimedAt string `json:"claimedAt,omitempty"`
}
type approvalsStore struct {
	SchemaVersion int               `json:"schemaVersion"`
	Requests      []ApprovalRequest `json:"requests"`
}

var goalApprovalMu sync.Mutex
var goalApprovalNow = func() time.Time { return time.Now().UTC() }

func withApprovalProfileLock(profile string, fn func() error) error {
	return withProfileLock(profile, "approvals.lock", fn)
}
func withGoalProfileLock(profile string, fn func() error) error {
	return withProfileLock(profile, "goals.lock", fn)
}
func withProfileLock(profile, name string, fn func() error) error {
	path := filepath.Join(profileDir(profile), name)
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if err := lockProfileFile(f); err != nil {
		return err
	}
	defer unlockProfileFile(f)
	return fn()
}

func goalsFile(profile string) string { return filepath.Join(profileDir(profile), "goals.json") }
func approvalsFile(profile string) string {
	return filepath.Join(profileDir(profile), "approvals.json")
}
func privateAtomicWrite(path string, v any) error {
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("unsafe non-regular storage target")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
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
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func rejectSymlinkPath(path string) error {
	for dir := filepath.Clean(filepath.Dir(path)); ; dir = filepath.Dir(dir) {
		if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe symlink storage path")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe symlink storage path")
	}
	return nil
}
func quarantineCorrupt(path string) error {
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	for i := 0; ; i++ {
		candidate := fmt.Sprintf("%s.corrupt-%s-%d", path, goalApprovalNow().Format("20060102T150405.000000000Z"), i)
		if err := os.Link(path, candidate); err == nil {
			return os.Remove(path)
		} else if !errors.Is(err, os.ErrExist) {
			return err
		}
	}
}
func readGoalsView(profile string) (goalsStore, string) {
	var s goalsStore
	path := goalsFile(profile)
	if err := rejectSymlinkPath(path); err != nil {
		return goalsStore{SchemaVersion: GoalSchemaVersion}, "unsafe"
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return goalsStore{SchemaVersion: GoalSchemaVersion, Goals: []Goal{}}, "missing"
	}
	if err != nil || json.Unmarshal(raw, &s) != nil || s.SchemaVersion != GoalSchemaVersion {
		return goalsStore{SchemaVersion: GoalSchemaVersion, Goals: []Goal{}}, "corrupt"
	}
	if !sanitizeGoalsStore(&s) {
		return goalsStore{SchemaVersion: GoalSchemaVersion, Goals: []Goal{}}, "corrupt"
	}
	return s, "healthy"
}
func readApprovalsView(profile string) (approvalsStore, string) {
	var s approvalsStore
	path := approvalsFile(profile)
	if err := rejectSymlinkPath(path); err != nil {
		return approvalsStore{SchemaVersion: ApprovalSchemaVersion}, "unsafe"
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return approvalsStore{SchemaVersion: ApprovalSchemaVersion, Requests: []ApprovalRequest{}}, "missing"
	}
	if err != nil || json.Unmarshal(raw, &s) != nil || s.SchemaVersion != ApprovalSchemaVersion {
		return approvalsStore{SchemaVersion: ApprovalSchemaVersion, Requests: []ApprovalRequest{}}, "corrupt"
	}
	if !sanitizeApprovalsStore(&s) {
		return approvalsStore{SchemaVersion: ApprovalSchemaVersion, Requests: []ApprovalRequest{}}, "corrupt"
	}
	return s, "healthy"
}
func safeStoredText(s string, max int) (string, bool) {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	return s, s != "" && len(s) <= max && !credentialValuePattern.MatchString(s) && !snapshotCredentialPattern.MatchString(s)
}
func safeStoredOptional(s string, max int) (string, bool) {
	if s == "" {
		return "", true
	}
	return safeStoredText(s, max)
}
func validGoalStatus(s GoalStatus) bool {
	return s == GoalPending || s == GoalActive || s == GoalCompleted || s == GoalCancelled || s == GoalBlocked
}
func validApprovalStatus(s ApprovalStatus) bool {
	return s == ApprovalPending || s == ApprovalApproved || s == ApprovalExecuting || s == ApprovalRejected || s == ApprovalExpired || s == ApprovalExecuted || s == ApprovalFailed
}
func sanitizeGoalsStore(s *goalsStore) bool {
	if len(s.Goals) > goalActionableCap+goalRetention {
		return false
	}
	seen := map[string]bool{}
	for i := range s.Goals {
		g := &s.Goals[i]
		if g.SchemaVersion != GoalSchemaVersion || !safeReferenceID(g.ID, "goal-") || seen[g.ID] || !validGoalStatus(g.Status) || g.Progress < 0 || g.Progress > 100 {
			return false
		}
		seen[g.ID] = true
		var ok bool
		if g.Title, ok = safeStoredText(g.Title, 160); !ok {
			return false
		}
		for _, p := range []*string{&g.Summary, &g.Profile, &g.Workspace, &g.ConversationID, &g.AppScopeID, &g.TurnID, &g.SnapshotID, &g.Source} {
			if *p, ok = safeStoredOptional(*p, 160); !ok {
				return false
			}
		}
		if len(g.Checklist) > 20 {
			return false
		}
		for j := range g.Checklist {
			if g.Checklist[j], ok = safeStoredText(g.Checklist[j], 160); !ok {
				return false
			}
		}
	}
	return true
}
func sanitizeApprovalsStore(s *approvalsStore) bool {
	if len(s.Requests) > approvalActionableCap+approvalRetention {
		return false
	}
	seen := map[string]bool{}
	for i := range s.Requests {
		r := &s.Requests[i]
		if r.SchemaVersion != ApprovalSchemaVersion || !safeReferenceID(r.ID, "apr-") || seen[r.ID] || !validApprovalStatus(r.Status) {
			return false
		}
		seen[r.ID] = true
		var ok bool
		for _, p := range []*string{&r.ActionClass, &r.Summary, &r.CreatedAt, &r.ExpiresAt, &r.Profile, &r.Workspace, &r.ConversationID, &r.Risk, &r.PayloadHash, &r.Preview, &r.ExecutedAt, &r.FailedAt, &r.Result, &r.ActionRef, &r.ClaimedAt} {
			if *p, ok = safeStoredOptional(*p, 256); !ok {
				return false
			}
		}
		if r.ActionClass != "workspace_delete" || r.Risk != "high" || !strings.HasPrefix(r.PayloadHash, "sha256:") {
			return false
		}
	}
	return true
}
func repairGoals(profile string) (goalsStore, error) {
	s, h := readGoalsView(profile)
	if h == "corrupt" {
		if err := quarantineCorrupt(goalsFile(profile)); err != nil {
			return s, err
		}
	} else if h != "healthy" && h != "missing" {
		return s, errors.New("unsafe goal storage")
	}
	return s, nil
}
func repairApprovals(profile string) (approvalsStore, error) {
	s, h := readApprovalsView(profile)
	if h == "corrupt" {
		if err := quarantineCorrupt(approvalsFile(profile)); err != nil {
			return s, err
		}
	} else if h != "healthy" && h != "missing" {
		return s, errors.New("unsafe approval storage")
	}
	for i := range s.Requests {
		if s.Requests[i].Status == ApprovalExecuting {
			s.Requests[i].Status = ApprovalFailed
			s.Requests[i].FailedAt = goalApprovalNow().Format(time.RFC3339Nano)
			s.Requests[i].Result = "failed_uncertain_restart"
			s.Requests[i].ClaimedAt = ""
		}
	}
	return s, nil
}
func safeGoalText(s string) (string, error) {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if s == "" || len(s) > 160 || credentialValuePattern.MatchString(s) || snapshotCredentialPattern.MatchString(s) {
		return "", errors.New("goal title must be a safe single-line label of at most 160 bytes")
	}
	return s, nil
}
func safeReferenceID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+32 {
		return false
	}
	for _, r := range id[len(prefix):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func safeGoalReference(g Goal) *Goal {
	if !safeReferenceID(g.ID, "goal-") || g.Status != GoalActive {
		return nil
	}
	g.Title = safeSnapshotString(g.Title)
	g.Summary = safeSnapshotString(g.Summary)
	g.Profile = safeSnapshotString(g.Profile)
	g.Workspace = safeSnapshotString(g.Workspace)
	g.ConversationID = safeSnapshotString(g.ConversationID)
	g.AppScopeID = safeSnapshotString(g.AppScopeID)
	g.TurnID = safeSnapshotString(g.TurnID)
	g.SnapshotID = safeSnapshotString(g.SnapshotID)
	g.Source = safeSnapshotString(g.Source)
	if len(g.Checklist) > 20 {
		g.Checklist = g.Checklist[:20]
	}
	for i := range g.Checklist {
		g.Checklist[i] = safeSnapshotString(g.Checklist[i])
	}
	return &g
}
func goalSort(goals []Goal) {
	sort.Slice(goals, func(i, j int) bool {
		if goals[i].UpdatedAt != goals[j].UpdatedAt {
			return goals[i].UpdatedAt > goals[j].UpdatedAt
		}
		return goals[i].ID < goals[j].ID
	})
}
func approvalSort(a []ApprovalRequest) {
	sort.Slice(a, func(i, j int) bool {
		if a[i].CreatedAt != a[j].CreatedAt {
			return a[i].CreatedAt > a[j].CreatedAt
		}
		return a[i].ID < a[j].ID
	})
}
func trimGoals(s *goalsStore) {
	actionable, terminal := []Goal{}, []Goal{}
	for _, g := range s.Goals {
		if g.Status == GoalActive || g.Status == GoalPending || g.Status == GoalBlocked {
			actionable = append(actionable, g)
		} else {
			terminal = append(terminal, g)
		}
	}
	goalSort(actionable)
	goalSort(terminal)
	// Actionable records are never pruned. Creation rejects once the cap is reached.
	remaining := goalRetention - len(actionable)
	if remaining < 0 {
		remaining = 0
	}
	if len(terminal) > remaining {
		terminal = terminal[:remaining]
	}
	s.Goals = append(actionable, terminal...)
	goalSort(s.Goals)
}
func trimApprovals(s *approvalsStore) {
	actionable, terminal := []ApprovalRequest{}, []ApprovalRequest{}
	for _, r := range s.Requests {
		if r.Status == ApprovalPending || r.Status == ApprovalApproved || r.Status == ApprovalExecuting {
			actionable = append(actionable, r)
		} else {
			terminal = append(terminal, r)
		}
	}
	approvalSort(actionable)
	approvalSort(terminal)
	// Actionable records are never pruned. Creation rejects once the cap is reached.
	remaining := approvalRetention - len(actionable)
	if remaining < 0 {
		remaining = 0
	}
	if len(terminal) > remaining {
		terminal = terminal[:remaining]
	}
	s.Requests = append(actionable, terminal...)
	approvalSort(s.Requests)
}
func (c *Client) goalScope() (string, string, string) {
	return c.workspaceName, c.conversationID, safeStatusAppScope(func() *statusApp {
		if c.currentApp == nil {
			return nil
		}
		return &statusApp{ScopeID: c.currentApp.ScopeID}
	}())
}
func (c *Client) listGoals() []Goal {
	goalApprovalMu.Lock()
	defer goalApprovalMu.Unlock()
	s, _ := readGoalsView(c.opts.Profile)
	goalSort(s.Goals)
	return s.Goals
}
func (c *Client) addGoal(title string) (Goal, error) {
	title, err := safeGoalText(title)
	if err != nil {
		return Goal{}, err
	}
	var g Goal
	err = withGoalProfileLock(c.opts.Profile, func() error {
		goalApprovalMu.Lock()
		defer goalApprovalMu.Unlock()
		s, err := repairGoals(c.opts.Profile)
		if err != nil {
			return err
		}
		actionable := 0
		for _, existing := range s.Goals {
			if existing.Status == GoalActive || existing.Status == GoalPending || existing.Status == GoalBlocked {
				actionable++
			}
		}
		if actionable >= goalActionableCap {
			return errors.New("goal actionable limit reached")
		}
		ws, conv, app := c.goalScope()
		now := goalApprovalNow().Format(time.RFC3339Nano)
		for i := range s.Goals {
			if s.Goals[i].Status == GoalActive && s.Goals[i].Workspace == ws && s.Goals[i].ConversationID == conv {
				s.Goals[i].Status = GoalPending
				s.Goals[i].UpdatedAt = now
			}
		}
		g = Goal{SchemaVersion: GoalSchemaVersion, ID: "goal-" + uuidV4Compact(), CreatedAt: now, UpdatedAt: now, Status: GoalActive, Title: title, Profile: c.opts.Profile, Workspace: ws, ConversationID: conv, AppScopeID: app, Source: "manual"}
		s.Goals = append(s.Goals, g)
		trimGoals(&s)
		return privateAtomicWrite(goalsFile(c.opts.Profile), s)
	})
	if err == nil {
		c.emitGoalStatus(g)
	}
	return g, err
}
func (c *Client) transitionGoal(id string, status GoalStatus) (Goal, error) {
	if !safeReferenceID(id, "goal-") {
		return Goal{}, errors.New("invalid goal id")
	}
	var out Goal
	err := withGoalProfileLock(c.opts.Profile, func() error {
		goalApprovalMu.Lock()
		defer goalApprovalMu.Unlock()
		s, err := repairGoals(c.opts.Profile)
		if err != nil {
			return err
		}
		for i := range s.Goals {
			if s.Goals[i].ID == id {
				g := &s.Goals[i]
				if g.Status == status {
					out = *g
					return nil
				}
				if g.Status == GoalCompleted || g.Status == GoalCancelled {
					out = *g
					return nil
				}
				g.Status = status
				g.UpdatedAt = goalApprovalNow().Format(time.RFC3339Nano)
				trimGoals(&s)
				err := privateAtomicWrite(goalsFile(c.opts.Profile), s)
				out = *g
				return err
			}
		}
		return errors.New("goal not found")
	})
	if err == nil {
		c.emitGoalStatus(out)
	}
	return out, err
}
func (c *Client) activeGoalReference() *Goal {
	goals := c.listGoals()
	ws, conv, _ := c.goalScope()
	for _, g := range goals {
		if g.Status == GoalActive && g.Workspace == ws && g.ConversationID == conv {
			return safeGoalReference(g)
		}
	}
	return nil
}
func handleGoalCommand(_ context.Context, c *Client, args []string) (bool, error) {
	jsonOutput := false
	filtered := []string{}
	for _, a := range args {
		if a == "--json" {
			jsonOutput = true
		} else {
			filtered = append(filtered, a)
		}
	}
	args = filtered
	if len(args) == 0 || args[0] == "list" || args[0] == "current" {
		goals := c.listGoals()
		if jsonOutput {
			raw, _ := json.Marshal(struct {
				SchemaVersion int    `json:"schemaVersion"`
				Goals         []Goal `json:"goals"`
			}{GoalSchemaVersion, goals})
			slashCommandPrintln(string(raw))
			return true, nil
		}
		if len(goals) == 0 {
			slashCommandPrintln("goals: none")
			return true, nil
		}
		for _, g := range goals {
			slashCommandPrintf("%s %s %s\n", g.ID, g.Status, g.Title)
		}
		return true, nil
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return true, errors.New("usage: /goal add <safe title>")
		}
		g, e := c.addGoal(strings.Join(args[1:], " "))
		if e == nil {
			slashCommandPrintf("goal created: %s\n", g.ID)
		}
		return true, e
	case "done":
		if len(args) != 2 {
			return true, errors.New("usage: /goal done <id>")
		}
		g, e := c.transitionGoal(args[1], GoalCompleted)
		if e == nil {
			slashCommandPrintf("goal %s: %s\n", g.ID, g.Status)
		}
		return true, e
	case "cancel":
		if len(args) != 2 {
			return true, errors.New("usage: /goal cancel <id>")
		}
		g, e := c.transitionGoal(args[1], GoalCancelled)
		if e == nil {
			slashCommandPrintf("goal %s: %s\n", g.ID, g.Status)
		}
		return true, e
	default:
		return true, errors.New("unknown /goal command")
	}
}
func approvalPayloadHash(action, ref string) string {
	h := sha256.Sum256([]byte(action + "\x00" + ref))
	return "sha256:" + hex.EncodeToString(h[:])
}
func (c *Client) requestWorkspaceDeleteApproval(name string) (ApprovalRequest, error) {
	if !isValidWorkspaceName(name) {
		return ApprovalRequest{}, errors.New("invalid workspace name")
	}
	if _, ok := safeStoredText(name, 160); !ok {
		return ApprovalRequest{}, errors.New("unsafe workspace name")
	}
	var r ApprovalRequest
	err := withApprovalProfileLock(c.opts.Profile, func() error {
		goalApprovalMu.Lock()
		defer goalApprovalMu.Unlock()
		s, err := repairApprovals(c.opts.Profile)
		if err != nil {
			return err
		}
		actionable := 0
		for _, existing := range s.Requests {
			if existing.Status == ApprovalPending || existing.Status == ApprovalApproved || existing.Status == ApprovalExecuting {
				actionable++
			}
		}
		if actionable >= approvalActionableCap {
			return errors.New("approval actionable limit reached")
		}
		now := goalApprovalNow()
		r = ApprovalRequest{SchemaVersion: ApprovalSchemaVersion, ID: "apr-" + uuidV4Compact(), ActionClass: "workspace_delete", Summary: "Delete local workspace " + name, CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339Nano), Profile: c.opts.Profile, Workspace: name, Risk: "high", PayloadHash: approvalPayloadHash("workspace_delete", name), Preview: "workspace=" + name, ActionRef: name, Status: ApprovalPending}
		s.Requests = append(s.Requests, r)
		trimApprovals(&s)
		return privateAtomicWrite(approvalsFile(c.opts.Profile), s)
	})
	if err == nil {
		c.emitApprovalEvent(EventApprovalRequested, r)
	}
	return r, err
}
func expireApprovals(s *approvalsStore) bool {
	changed := false
	now := goalApprovalNow()
	for i := range s.Requests {
		r := &s.Requests[i]
		if r.Status == ApprovalPending || r.Status == ApprovalApproved {
			if t, e := time.Parse(time.RFC3339Nano, r.ExpiresAt); e == nil && now.After(t) {
				r.Status = ApprovalExpired
				r.Result = "expired"
				changed = true
			}
		}
	}
	return changed
}
func (c *Client) listApprovals() []ApprovalRequest {
	goalApprovalMu.Lock()
	defer goalApprovalMu.Unlock()
	s, _ := readApprovalsView(c.opts.Profile)
	// Expiry is derived for views only; diagnostics must not mutate the store.
	_ = expireApprovals(&s)
	approvalSort(s.Requests)
	return s.Requests
}
func (c *Client) rejectApproval(id string) (ApprovalRequest, error) {
	if !safeReferenceID(id, "apr-") {
		return ApprovalRequest{}, errors.New("invalid approval id")
	}
	var out ApprovalRequest
	err := withApprovalProfileLock(c.opts.Profile, func() error {
		goalApprovalMu.Lock()
		defer goalApprovalMu.Unlock()
		s, err := repairApprovals(c.opts.Profile)
		if err != nil {
			return err
		}
		expireApprovals(&s)
		for i := range s.Requests {
			if s.Requests[i].ID == id {
				r := &s.Requests[i]
				if r.Status == ApprovalPending || r.Status == ApprovalApproved {
					r.Status = ApprovalRejected
					r.Result = "rejected"
				}
				err := privateAtomicWrite(approvalsFile(c.opts.Profile), s)
				out = *r
				return err
			}
		}
		return errors.New("approval not found")
	})
	if err == nil {
		c.emitApprovalEvent(EventApprovalStatusChanged, out)
	}
	return out, err
}
func (c *Client) approveApproval(id string) (ApprovalRequest, error) {
	if !safeReferenceID(id, "apr-") {
		return ApprovalRequest{}, errors.New("invalid approval id")
	}
	// The advisory lock is intentionally held through execution. It serializes
	// separate CLI processes; an interrupted execution remains `executing` and
	// is conservatively failed on a later mutating load instead of replayed.
	return c.approveApprovalLocked(id)
}
func (c *Client) approveApprovalLocked(id string) (out ApprovalRequest, retErr error) {
	type pendingEvent struct {
		typ     SemanticEventType
		payload ApprovalEventPayload
	}
	deferredEvents := []pendingEvent{}
	err := withApprovalProfileLock(c.opts.Profile, func() error {
		goalApprovalMu.Lock()
		s, err := repairApprovals(c.opts.Profile)
		if err != nil {
			goalApprovalMu.Unlock()
			return err
		}
		expireApprovals(&s)
		var claimed *ApprovalRequest
		for i := range s.Requests {
			if s.Requests[i].ID == id {
				r := &s.Requests[i]
				if r.Status == ApprovalPending {
					r.Status = ApprovalApproved
				}
				if r.Status == ApprovalExecuting {
					out = *r
					goalApprovalMu.Unlock()
					return errors.New("approval is already executing")
				}
				if r.Status != ApprovalApproved {
					out = *r
					_ = privateAtomicWrite(approvalsFile(c.opts.Profile), s)
					goalApprovalMu.Unlock()
					return errors.New("approval is not executable")
				}
				r.Status = ApprovalExecuting
				r.ClaimedAt = goalApprovalNow().Format(time.RFC3339Nano)
				claimed = r
				break
			}
		}
		if claimed == nil {
			goalApprovalMu.Unlock()
			return errors.New("approval not found")
		}
		if err := privateAtomicWrite(approvalsFile(c.opts.Profile), s); err != nil {
			goalApprovalMu.Unlock()
			return err
		}
		// Both transitions above are now durable, so both may be observed later.
		deferredEvents = append(deferredEvents,
			pendingEvent{EventApprovalStatusChanged, ApprovalEventPayload{ApprovalID: claimed.ID, Status: string(ApprovalApproved), ActionClass: claimed.ActionClass}},
			pendingEvent{EventApprovalStatusChanged, ApprovalEventPayload{ApprovalID: claimed.ID, Status: string(ApprovalExecuting), ActionClass: claimed.ActionClass}},
		)
		action := claimed.ActionClass
		workspace := claimed.ActionRef
		goalApprovalMu.Unlock()
		var executionErr error
		switch action {
		case "workspace_delete":
			executionErr = deleteWorkspace(c.opts.Profile, c.workspaceName, workspace)
		default:
			executionErr = errors.New("unsupported approval action")
		}
		goalApprovalMu.Lock()
		defer goalApprovalMu.Unlock()
		s, health := readApprovalsView(c.opts.Profile)
		if health != "healthy" {
			return errors.New("approval storage changed during execution")
		}
		for i := range s.Requests {
			if s.Requests[i].ID == id {
				r := &s.Requests[i]
				if executionErr != nil {
					r.Status = ApprovalFailed
					r.FailedAt = goalApprovalNow().Format(time.RFC3339Nano)
					r.Result = "failed"
					r.ClaimedAt = ""
				} else {
					r.Status = ApprovalExecuted
					r.ExecutedAt = goalApprovalNow().Format(time.RFC3339Nano)
					r.Result = "executed"
					r.ClaimedAt = ""
				}
				out = *r
				werr := privateAtomicWrite(approvalsFile(c.opts.Profile), s)
				if werr == nil {
					if executionErr != nil {
						deferredEvents = append(deferredEvents, pendingEvent{EventApprovalFailed, ApprovalEventPayload{ApprovalID: out.ID, Status: string(out.Status), ActionClass: out.ActionClass}})
					} else {
						deferredEvents = append(deferredEvents, pendingEvent{EventApprovalExecuted, ApprovalEventPayload{ApprovalID: out.ID, Status: string(out.Status), ActionClass: out.ActionClass}})
					}
				}
				if executionErr != nil {
					return executionErr
				}
				return werr
			}
		}
		return errors.New("approval disappeared")
	})
	for _, event := range deferredEvents {
		c.emitSemanticEvent(event.typ, "", event.payload)
	}
	return out, err
}
func (c *Client) emitGoalStatus(g Goal) {
	if c.activeSemanticTurn() {
		c.emitSemanticEvent(EventGoalStatusChanged, "", GoalStatusChangedPayload{GoalID: g.ID, Status: string(g.Status)})
	}
}
func (c *Client) emitApprovalEvent(t SemanticEventType, r ApprovalRequest) {
	if c.activeSemanticTurn() {
		c.emitSemanticEvent(t, "", ApprovalEventPayload{ApprovalID: r.ID, Status: string(r.Status), ActionClass: r.ActionClass})
	}
}
func handleApprovalsCommand(_ context.Context, c *Client, args []string) (bool, error) {
	jsonOutput := len(args) == 1 && args[0] == "--json"
	if len(args) > 0 && !jsonOutput {
		return true, errors.New("usage: /approvals [--json]")
	}
	rs := c.listApprovals()
	if jsonOutput {
		raw, _ := json.Marshal(struct {
			SchemaVersion int               `json:"schemaVersion"`
			Requests      []ApprovalRequest `json:"requests"`
		}{ApprovalSchemaVersion, rs})
		slashCommandPrintln(string(raw))
		return true, nil
	}
	if len(rs) == 0 {
		slashCommandPrintln("approvals: none")
		return true, nil
	}
	for _, r := range rs {
		slashCommandPrintf("%s %s %s\n", r.ID, r.Status, r.Summary)
	}
	return true, nil
}
func handleApproveCommand(_ context.Context, c *Client, args []string) (bool, error) {
	if len(args) != 1 {
		return true, errors.New("usage: /approve <id>")
	}
	r, e := c.approveApproval(args[0])
	if e == nil {
		slashCommandPrintf("approval %s: %s\n", r.ID, r.Status)
	}
	return true, e
}
func handleRejectCommand(_ context.Context, c *Client, args []string) (bool, error) {
	if len(args) != 1 {
		return true, errors.New("usage: /reject <id>")
	}
	r, e := c.rejectApproval(args[0])
	if e == nil {
		slashCommandPrintf("approval %s: %s\n", r.ID, r.Status)
	}
	return true, e
}
