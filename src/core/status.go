package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// StatusSchemaVersion is the stable machine-readable /status schema version.
const StatusSchemaVersion = 1

var statusNow = func() time.Time { return time.Now().UTC() }

type runtimeStatusDocument struct {
	SchemaVersion int              `json:"schemaVersion"`
	GeneratedAt   string           `json:"generatedAt"`
	Overall       string           `json:"overall"`
	Profile       statusProfile    `json:"profile"`
	Transport     statusTransport  `json:"transport"`
	Connection    statusConnection `json:"connection"`
	Context       statusContext    `json:"context"`
	Turn          statusTurn       `json:"turn"`
	Servers       statusServers    `json:"servers"`
	WorkingSet    statusWorkingSet `json:"workingSet"`
	Runtime       statusRuntime    `json:"runtime"`
	Policy        statusPolicy     `json:"policy"`
	Journal       statusJournal    `json:"journal"`
	Remote        statusRemote     `json:"remote"`
	Goals         statusGoals      `json:"goals"`
	Approvals     statusApprovals  `json:"approvals"`
}

type statusProfile struct {
	Name         string `json:"name"`
	InstanceHost string `json:"instanceHost"`
	Status       string `json:"status"`
}
type statusTransport struct {
	Mode         string   `json:"mode"`
	AuthMode     string   `json:"authMode"`
	Capabilities []string `json:"capabilities"`
	Status       string   `json:"status"`
}
type statusConnection struct {
	State  string `json:"state"`
	Status string `json:"status"`
}
type statusContext struct {
	ConversationID string     `json:"conversationId,omitempty"`
	Workspace      string     `json:"workspace"`
	WorkspaceURI   string     `json:"workspaceUri,omitempty"`
	App            *statusApp `json:"app,omitempty"`
	Status         string     `json:"status"`
}
type statusApp struct {
	ScopeID string `json:"scopeId"`
	Scope   string `json:"scope,omitempty"`
	Name    string `json:"name,omitempty"`
}
type statusTurn struct {
	Active             bool   `json:"active"`
	Status             string `json:"status"`
	SnapshotID         string `json:"snapshotId,omitempty"`
	SnapshotGeneration string `json:"snapshotGeneration,omitempty"`
	CapturedAt         string `json:"capturedAt,omitempty"`
}
type statusServers struct {
	Inventory  []statusServer `json:"inventory"`
	Generation string         `json:"generation,omitempty"`
	Hash       string         `json:"hash,omitempty"`
	Status     string         `json:"status"`
}
type statusServer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Source    string `json:"source"`
}
type statusWorkingSet struct {
	Count  int    `json:"count"`
	Hash   string `json:"hash,omitempty"`
	Status string `json:"status"`
}
type statusRuntime struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Skill    string `json:"skill,omitempty"`
	Status   string `json:"status"`
}
type statusPolicy struct {
	DefaultTimeout string          `json:"defaultTimeout,omitempty"`
	ToolTimeouts   []statusTimeout `json:"toolTimeouts"`
	Retry          string          `json:"retry"`
	Attempts       int             `json:"attempts,omitempty"`
	LastDecision   string          `json:"lastDecision,omitempty"`
	Status         string          `json:"status"`
}
type statusTimeout struct {
	Name    string `json:"name"`
	Timeout string `json:"timeout"`
}
type statusJournal struct {
	Checkpoint   uint64 `json:"checkpoint"`
	LastSequence uint64 `json:"lastSequence"`
	Health       string `json:"health"`
	Recovery     string `json:"recovery"`
	Status       string `json:"status"`
}
type statusGoals struct {
	ActiveID     string `json:"activeId,omitempty"`
	ActiveStatus string `json:"activeStatus,omitempty"`
	Count        int    `json:"count"`
}
type statusApprovals struct {
	Pending int `json:"pending"`
	Recent  int `json:"recent"`
}
type statusRemote struct {
	MetadataSync   string `json:"metadataSync"`
	BuildReadiness string `json:"buildReadiness"`
	LastBuildAt    string `json:"lastBuildAt,omitempty"`
	Status         string `json:"status"`
}

func (c *Client) statusDocument() runtimeStatusDocument {
	now := statusNow().UTC()
	snapshot, active := c.statusSnapshot()
	caps := append([]string(nil), snapshot.AuthCapabilities...)
	for i := range caps {
		caps[i] = safeStatusString(caps[i])
	}
	sort.Strings(caps)
	serverList := canonicalMCPServers(snapshot.MCPServers)
	servers := make([]statusServer, 0, len(serverList))
	for _, server := range serverList {
		servers = append(servers, statusServer{ID: safeStatusString(server.ServerID), Name: safeStatusString(server.Name), Transport: safeStatusString(server.Transport), Source: safeStatusString(server.Source)})
	}
	workingSet := snapshot.WorkingSet
	if !active && workingSet == nil {
		workingSet, _ = nirvanaOutboundWorkingSet(c.workingSet)
		workingSet = safeSnapshotValue(workingSet).([]interface{})
	}
	workingHash := canonicalHash(workingSet)
	if len(workingSet) == 0 {
		workingHash = ""
	}
	app := snapshot.App
	if !active && app == nil {
		app = snapshotApp(c.currentApp, c.appScope)
		app = safeSnapshotApp(app)
	}
	var outApp *statusApp
	if app != nil {
		outApp = &statusApp{ScopeID: safeStatusString(app.ScopeID), Scope: safeStatusString(app.Scope), Name: safeStatusString(app.ScopeName)}
	}
	connectionState := c.statusConnectionState()
	turn := statusTurn{Active: active, Status: "healthy"}
	if active {
		turn.Status = "active"
		turn.SnapshotID = snapshotID(snapshot)
		turn.SnapshotGeneration = safeStatusString(snapshot.MCPGeneration)
		turn.CapturedAt = snapshot.CapturedAt.UTC().Format(time.RFC3339Nano)
	}
	journal := c.statusJournal()
	remote := c.statusRemote()
	attempts := c.retryTelemetry()
	lastDecision := ""
	if len(attempts) > 0 {
		lastDecision = attempts[len(attempts)-1].Decision
	}
	timeouts := make([]statusTimeout, 0, len(snapshot.Timeouts))
	for name, timeout := range snapshot.Timeouts {
		timeouts = append(timeouts, statusTimeout{Name: safeStatusString(name), Timeout: timeout.String()})
	}
	sort.Slice(timeouts, func(i, j int) bool { return timeouts[i].Name < timeouts[j].Name })
	overall := combineStatus("healthy", journal.Status, remote.Status, connectionState.Status)
	goals := c.listGoals()
	activeGoal := c.activeGoalReference()
	goalStatus := statusGoals{Count: len(goals)}
	if activeGoal != nil {
		goalStatus.ActiveID = safeStatusString(activeGoal.ID)
		goalStatus.ActiveStatus = string(activeGoal.Status)
	}
	approvals := c.listApprovals()
	approvalStatus := statusApprovals{Recent: len(approvals)}
	for _, approval := range approvals {
		if approval.Status == ApprovalPending || approval.Status == ApprovalApproved {
			approvalStatus.Pending++
		}
	}
	return runtimeStatusDocument{
		SchemaVersion: StatusSchemaVersion, GeneratedAt: now.Format(time.RFC3339Nano), Overall: overall,
		Profile:   statusProfile{Name: safeStatusString(strings.TrimSpace(snapshot.Profile)), InstanceHost: safeStatusString(statusSnapshotHost(snapshot, c.cfg.InstanceURL, active)), Status: "healthy"},
		Transport: statusTransport{Mode: safeStatusString(snapshot.Transport), AuthMode: safeStatusString(snapshot.AuthMode), Capabilities: caps, Status: "healthy"}, Connection: connectionState,
		Context: statusContext{ConversationID: safeStatusString(snapshot.ConversationID), Workspace: safeStatusString(snapshot.Workspace.Name), WorkspaceURI: safeStatusString(snapshot.Workspace.URI), App: outApp, Status: "healthy"}, Turn: turn,
		Servers:    statusServers{Inventory: servers, Generation: safeStatusString(snapshot.MCPGeneration), Hash: safeStatusString(snapshot.MCPHash), Status: statusKnownStatus(len(servers) > 0)},
		WorkingSet: statusWorkingSet{Count: len(workingSet), Hash: workingHash, Status: "healthy"},
		Runtime:    statusRuntime{Provider: safeStatusString(snapshot.Model.Provider), Model: safeStatusString(snapshot.Model.LargeModel), Skill: safeStatusString(snapshot.Model.SkillID), Status: "healthy"},
		Policy:     statusPolicy{DefaultTimeout: snapshot.DefaultTimeout.String(), ToolTimeouts: timeouts, Retry: "safe_reads_bounded", Attempts: len(attempts), LastDecision: safeStatusString(lastDecision), Status: "healthy"}, Journal: journal, Remote: remote, Goals: goalStatus, Approvals: approvalStatus,
	}
}

func (c *Client) statusSnapshot() (TurnRuntimeSnapshot, bool) {
	c.activeTurnMu.Lock()
	active := c.processing && c.turnRuntimeSnapshot != nil
	var snapshot TurnRuntimeSnapshot
	if active {
		snapshot = cloneTurnRuntimeSnapshot(*c.turnRuntimeSnapshot)
	}
	c.activeTurnMu.Unlock()
	if active {
		return snapshot, true
	}
	servers := []MCPServer(nil)
	if c.nirvanaMCPServersReady {
		servers = cloneMCPServers(c.nirvanaMCPServers)
	}
	return c.captureTurnRuntimeSnapshot(servers), false
}

func statusSnapshotHost(snapshot TurnRuntimeSnapshot, fallbackURL string, active bool) string {
	if host := strings.TrimSpace(snapshot.InstanceHost); host != "" {
		return host
	}
	if active {
		return ""
	}
	return normalizedInstanceHost(fallbackURL)
}

func normalizedInstanceHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
func statusKnownStatus(known bool) string {
	if known {
		return "healthy"
	}
	return "unknown"
}
func combineStatus(initial string, values ...string) string {
	rank := map[string]int{"healthy": 0, "unknown": 1, "degraded": 2, "error": 3}
	best := initial
	for _, value := range values {
		if rank[value] > rank[best] {
			best = value
		}
	}
	return best
}

func (c *Client) statusConnectionState() statusConnection {
	c.semanticMu.Lock()
	semantic := c.semanticState.ConnectionState
	c.semanticMu.Unlock()
	if semantic != "" {
		return statusConnection{State: safeStatusString(semantic), Status: "healthy"}
	}
	if c.conn != nil || c.ambSubscribed {
		return statusConnection{State: "connected_unverified", Status: "unknown"}
	}
	return statusConnection{State: "not_checked", Status: "unknown"}
}

func (c *Client) statusJournal() statusJournal {
	out := statusJournal{Checkpoint: c.semanticJournalSequence, Health: "not_checked", Recovery: "not_checked", Status: "unknown"}
	path := semanticJournalFile(c.opts.Profile, c.workspaceName)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			out.Health, out.Recovery = "missing", "not_needed"
			return out
		}
		out.Health, out.Recovery, out.Status = "unreadable", "unknown", "error"
		return out
	}
	entries, err := readSemanticJournalChain(c.opts.Profile, c.workspaceName)
	if err != nil {
		out.Health, out.Recovery, out.Status = "corrupt", "blocked", "error"
		return out
	}
	if len(entries) == 0 {
		out.Health, out.Recovery = "empty", "not_needed"
		return out
	}
	out.LastSequence = entries[len(entries)-1].Sequence
	switch {
	case out.Checkpoint == out.LastSequence:
		out.Health, out.Recovery, out.Status = "healthy", "current", "healthy"
	case out.Checkpoint < out.LastSequence:
		out.Health, out.Recovery, out.Status = "behind", "available", "degraded"
	default:
		out.Health, out.Recovery, out.Status = "ahead", "blocked", "error"
	}
	return out
}

func (c *Client) statusRemote() statusRemote {
	out := statusRemote{MetadataSync: "unknown", BuildReadiness: "unknown", Status: "unknown"}
	build := c.lastGliderBuild
	if build == nil {
		return out
	}
	if !build.BuiltAt.IsZero() {
		out.LastBuildAt = build.BuiltAt.UTC().Format(time.RFC3339Nano)
	}
	if build.BuildSucceeded {
		out.BuildReadiness = "build_succeeded"
	} else {
		out.BuildReadiness = "build_failed"
		out.Status = "degraded"
	}
	if build.TempDir == "" {
		return out
	}
	state, err := readMetadataSyncState(build.TempDir)
	if err != nil {
		out.MetadataSync, out.Status = "error", "error"
		return out
	}
	if state.Needed {
		out.MetadataSync = "required"
		if out.Status != "error" {
			out.Status = "degraded"
		}
	} else {
		out.MetadataSync = "current"
	}
	if out.Status == "unknown" && out.MetadataSync != "unknown" && out.BuildReadiness != "unknown" {
		out.Status = "healthy"
	}
	return out
}

func snapshotID(snapshot TurnRuntimeSnapshot) string {
	return canonicalHash(map[string]string{"capturedAt": snapshot.CapturedAt.UTC().Format(time.RFC3339Nano), "conversationId": snapshot.ConversationID, "workspace": snapshot.Workspace.Name})[:16]
}

func handleStatusCommand(_ context.Context, c *Client, args []string) (bool, error) {
	if len(args) > 1 || (len(args) == 1 && args[0] != "--json") {
		return true, fmt.Errorf("usage: /status [--json]")
	}
	doc := c.statusDocument()
	if len(args) == 1 {
		raw, err := json.Marshal(doc)
		if err != nil {
			return true, err
		}
		slashCommandPrintln(string(raw))
		return true, nil
	}
	printStatusHuman(doc)
	return true, nil
}

func printStatusHuman(doc runtimeStatusDocument) {
	slashCommandPrintf("status: %s  generated=%s\n", doc.Overall, doc.GeneratedAt)
	slashCommandPrintf("profile: %s  instance=%s\n", emptyStatus(doc.Profile.Name), emptyStatus(doc.Profile.InstanceHost))
	slashCommandPrintf("transport: %s  auth=%s  capabilities=%s\n", doc.Transport.Mode, emptyStatus(doc.Transport.AuthMode), statusList(doc.Transport.Capabilities))
	slashCommandPrintf("connection: %s (%s)  turn=%s\n", doc.Connection.State, doc.Connection.Status, doc.Turn.Status)
	if doc.Turn.Active {
		slashCommandPrintf("turn snapshot: id=%s generation=%s captured=%s\n", doc.Turn.SnapshotID, emptyStatus(doc.Turn.SnapshotGeneration), doc.Turn.CapturedAt)
	}
	app := "<none>"
	if doc.Context.App != nil {
		app = doc.Context.App.ScopeID
		if doc.Context.App.Name != "" {
			app += " (" + doc.Context.App.Name + ")"
		}
	}
	slashCommandPrintf("context: workspace=%s conversation=%s app=%s\n", emptyStatus(doc.Context.Workspace), emptyStatus(doc.Context.ConversationID), app)
	slashCommandPrintf("servers: %d generation=%s hash=%s (%s)\n", len(doc.Servers.Inventory), emptyStatus(doc.Servers.Generation), shortStatusHash(doc.Servers.Hash), doc.Servers.Status)
	for _, server := range doc.Servers.Inventory {
		slashCommandPrintf("  - %s id=%s transport=%s source=%s\n", server.Name, server.ID, server.Transport, server.Source)
	}
	slashCommandPrintf("working set: %d hash=%s\n", doc.WorkingSet.Count, shortStatusHash(doc.WorkingSet.Hash))
	slashCommandPrintf("runtime: provider=%s model=%s skill=%s\n", emptyStatus(doc.Runtime.Provider), emptyStatus(doc.Runtime.Model), emptyStatus(doc.Runtime.Skill))
	slashCommandPrintf("policy: default-timeout=%s retry=%s\n", emptyStatus(doc.Policy.DefaultTimeout), doc.Policy.Retry)
	slashCommandPrintf("journal: checkpoint=%d last=%d health=%s recovery=%s (%s)\n", doc.Journal.Checkpoint, doc.Journal.LastSequence, doc.Journal.Health, doc.Journal.Recovery, doc.Journal.Status)
	slashCommandPrintf("remote: metadata-sync=%s build=%s last-build=%s (%s)\n", doc.Remote.MetadataSync, doc.Remote.BuildReadiness, emptyStatus(doc.Remote.LastBuildAt), doc.Remote.Status)
}
func safeStatusString(value string) string {
	return safeSnapshotString(value)
}

func emptyStatus(value string) string {
	if strings.TrimSpace(value) == "" {
		return "<unknown>"
	}
	return value
}
func statusList(values []string) string {
	if len(values) == 0 {
		return "<none>"
	}
	return strings.Join(values, ",")
}
func shortStatusHash(value string) string {
	if value == "" {
		return "<none>"
	}
	if len(value) > 12 {
		return value[:12]
	}
	return value
}
