package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TurnRuntimeSnapshot is the redaction-safe, immutable view used by one submitted turn.
// It contains references and effective settings only: never credentials or transport headers.
type TurnRuntimeSnapshot struct {
	CapturedAt          time.Time                `json:"capturedAt"`
	Profile             string                   `json:"profile"`
	InstanceURL         string                   `json:"instanceUrl,omitempty"`
	InstanceHost        string                   `json:"instanceHost,omitempty"`
	Transport           string                   `json:"transport"`
	AuthMode            string                   `json:"authMode,omitempty"`
	AuthCapabilities    []string                 `json:"authCapabilities,omitempty"`
	ConversationID      string                   `json:"conversationId,omitempty"`
	Workspace           TurnWorkspaceSnapshot    `json:"workspace"`
	App                 *AppScope                `json:"app,omitempty"`
	WorkingSet          []interface{}            `json:"workingSet,omitempty"`
	WorkingSetHash      string                   `json:"workingSetHash,omitempty"`
	IDEContext          map[string]interface{}   `json:"ideContext,omitempty"`
	ConversationHistory []interface{}            `json:"conversationHistory,omitempty"`
	Model               TurnModelSnapshot        `json:"model"`
	MCPServers          []MCPServer              `json:"mcpServers,omitempty"`
	MCPGeneration       string                   `json:"mcpGeneration,omitempty"`
	MCPHash             string                   `json:"mcpHash,omitempty"`
	Timeouts            map[string]time.Duration `json:"timeouts,omitempty"`
	DefaultTimeout      time.Duration            `json:"defaultTimeout,omitempty"`
	RetryPolicy         string                   `json:"retryPolicy"`
	CLIVersion          string                   `json:"cliVersion"`
	BuildSHA            string                   `json:"buildSha,omitempty"`
	Goal                *Goal                    `json:"goal,omitempty"`
}

type TurnWorkspaceSnapshot struct {
	Name, URI, Checksum string
	Folders             []WebWorkspaceFolder
}
type TurnModelSnapshot struct {
	Provider, LargeModel, SmallModel, CapabilityID, SkillID                              string
	LargeMaxOutputTokens, SmallMaxOutputTokens, LargeThinkingTokens, SmallThinkingTokens int
	LargeTemperature, SmallTemperature                                                   float64
}

const turnRuntimeCLIVersion = "2.3.3"

var turnRuntimeBuildSHA = ""

var snapshotCredentialPattern = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:(?:bearer|basic)\s+)?\S+|bearer\s+\S+|basic\s+\S+|(?:oauth[_-]?)?token\s*[:=]\s*\S+|(?:password|secret|api[_-]?key|g_ck|cookie|set-cookie|jsessionid|session(?:[_-]?id)?)\s*[:=]\s*\S+)`)

func (c *Client) captureTurnRuntimeSnapshot(mcpServers []MCPServer) TurnRuntimeSnapshot {
	instanceURL := strings.TrimRight(strings.TrimSpace(c.cfg.InstanceURL), "/")
	host := ""
	if u, err := url.Parse(instanceURL); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	authMode, _ := normalizeAuthMode(c.opts.AuthMode)
	caps := []string{}
	if c.oauthAccessToken != "" {
		caps = append(caps, "oauth_bearer")
	}
	if c.sessionCookieHeader != "" {
		caps = append(caps, "web_session")
	}
	if c.gatewayAuth == authModeBasic {
		caps = append(caps, "basic_auth")
	}
	sort.Strings(caps)
	workingSet, _ := nirvanaOutboundWorkingSet(c.workingSet)
	if workingSet != nil {
		workingSet = safeSnapshotValue(workingSet).([]interface{})
	}
	servers := canonicalMCPServers(safeMCPServers(mcpServers))
	app := safeSnapshotApp(snapshotApp(c.currentApp, c.appScope))
	timeouts := make(map[string]time.Duration, len(c.webStartupConfig.ToolTimeouts))
	for name, timeout := range c.webStartupConfig.ToolTimeouts {
		timeouts[name] = timeout
	}
	return TurnRuntimeSnapshot{
		CapturedAt: time.Now().UTC(), Profile: safeSnapshotString(strings.TrimSpace(c.opts.Profile)), InstanceURL: safeSnapshotURL(instanceURL), InstanceHost: safeSnapshotString(host),
		Transport: safeSnapshotString(c.turnTransport()), AuthMode: safeSnapshotString(authMode), AuthCapabilities: safeSnapshotStrings(caps), ConversationID: safeSnapshotString(c.conversationID),
		Workspace: safeSnapshotWorkspace(TurnWorkspaceSnapshot{Name: c.workspaceName, URI: c.workspaceURI, Checksum: c.workspaceChecksum, Folders: append([]WebWorkspaceFolder(nil), c.workspaceFolders...)}),
		App:       app, WorkingSet: workingSet, WorkingSetHash: canonicalHash(workingSet), IDEContext: safeSnapshotValue(c.currentIDEContext()).(map[string]interface{}), ConversationHistory: safeSnapshotValue(nirvanaConversationHistory(c.history)).([]interface{}),
		Model:      TurnModelSnapshot{Provider: safeSnapshotString(c.runtime.Provider), LargeModel: safeSnapshotString(effectiveLargeModel(c)), SmallModel: safeSnapshotString(c.runtime.SmallModel), CapabilityID: safeSnapshotString(effectiveCapabilityID(c)), SkillID: safeSnapshotString(strings.TrimSpace(c.webAgentConfig.SkillID)), LargeMaxOutputTokens: c.runtime.LargeMaxOutputTokens, SmallMaxOutputTokens: c.runtime.SmallMaxOutputTokens, LargeThinkingTokens: c.runtime.LargeThinkingTokens, SmallThinkingTokens: c.runtime.SmallThinkingTokens, LargeTemperature: c.runtime.LargeTemperature, SmallTemperature: c.runtime.SmallTemperature},
		MCPServers: servers, MCPGeneration: canonicalHash(servers), MCPHash: canonicalHash(servers), Timeouts: safeSnapshotTimeouts(timeouts), DefaultTimeout: c.webStartupConfig.DefaultTimeout, RetryPolicy: safeSnapshotString("transport-managed"), CLIVersion: turnRuntimeCLIVersion, BuildSHA: safeSnapshotString(strings.TrimSpace(turnRuntimeBuildSHA)), Goal: c.activeGoalReference(),
	}
}

func cloneTurnRuntimeSnapshot(snapshot TurnRuntimeSnapshot) TurnRuntimeSnapshot {
	clone := snapshot
	clone.AuthCapabilities = append([]string(nil), snapshot.AuthCapabilities...)
	clone.Workspace.Folders = append([]WebWorkspaceFolder(nil), snapshot.Workspace.Folders...)
	clone.WorkingSet = cloneJSONSlice(snapshot.WorkingSet)
	clone.IDEContext = cloneJSONMap(snapshot.IDEContext)
	clone.ConversationHistory = cloneJSONSlice(snapshot.ConversationHistory)
	clone.MCPServers = cloneMCPServers(snapshot.MCPServers)
	clone.Timeouts = safeSnapshotTimeouts(snapshot.Timeouts)
	if snapshot.App != nil {
		clone.App = safeSnapshotApp(snapshot.App)
	}
	if snapshot.Goal != nil {
		g := *snapshot.Goal
		g.Checklist = append([]string(nil), snapshot.Goal.Checklist...)
		clone.Goal = safeGoalReference(g)
	}
	return clone
}

func (c *Client) turnTransport() string {
	if c.opts.Nirvana {
		return "nirvana_websocket"
	}
	if c.opts.CodeAssistWS {
		return "code_assist_websocket"
	}
	return "web_gateway"
}
func effectiveLargeModel(c *Client) string {
	if c.webAgentConfig.Model != "" && c.opts.Model == "" && c.opts.Provider == "" {
		return c.webAgentConfig.Model
	}
	return c.runtime.LargeModel
}
func effectiveCapabilityID(c *Client) string {
	if c.webAgentConfig.SkillID != "" {
		return c.webAgentConfig.SkillID
	}
	return c.cfg.CapabilityID
}
func snapshotApp(current *AppScope, scope interface{}) *AppScope {
	if current != nil {
		v := *current
		return &v
	}
	if m := asMap(scope); m != nil {
		return appFromPayload(m)
	}
	if id := strings.TrimSpace(stringify(scope)); id != "" {
		return &AppScope{ScopeID: id, ScopeName: id, AppSysID: id}
	}
	return nil
}
func canonicalMCPServers(servers []MCPServer) []MCPServer {
	out := cloneMCPServers(servers)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return strings.Join([]string{a.ServerID, a.Transport, a.Name, a.URL, a.Source}, "\x00") < strings.Join([]string{b.ServerID, b.Transport, b.Name, b.URL, b.Source}, "\x00")
	})
	return out
}

func safeMCPServers(servers []MCPServer) []MCPServer {
	out := cloneMCPServers(servers)
	for i := range out {
		out[i].ServerID = safeSnapshotString(out[i].ServerID)
		out[i].Name = safeSnapshotString(out[i].Name)
		out[i].Transport = safeSnapshotString(out[i].Transport)
		out[i].Source = safeSnapshotString(out[i].Source)
		out[i].URL = safeSnapshotURL(out[i].URL)
	}
	return out
}

func safeSnapshotApp(app *AppScope) *AppScope {
	if app == nil {
		return nil
	}
	out := *app
	out.ScopeID = safeSnapshotString(out.ScopeID)
	out.Scope = safeSnapshotString(out.Scope)
	out.ScopeName = safeSnapshotString(out.ScopeName)
	out.AppSysID = safeSnapshotString(out.AppSysID)
	return &out
}

func safeSnapshotWorkspace(workspace TurnWorkspaceSnapshot) TurnWorkspaceSnapshot {
	workspace.Name = safeSnapshotString(workspace.Name)
	workspace.URI = safeSnapshotString(workspace.URI)
	workspace.Checksum = safeSnapshotString(workspace.Checksum)
	for i := range workspace.Folders {
		workspace.Folders[i].Name = safeSnapshotString(workspace.Folders[i].Name)
		workspace.Folders[i].URI = safeSnapshotString(workspace.Folders[i].URI)
	}
	return workspace
}

func safeSnapshotStrings(values []string) []string {
	out := make([]string, len(values))
	for i := range values {
		out[i] = safeSnapshotString(values[i])
	}
	return out
}

func safeSnapshotTimeouts(values map[string]time.Duration) map[string]time.Duration {
	out := make(map[string]time.Duration, len(values))
	for name, timeout := range values {
		out[safeSnapshotString(name)] = timeout
	}
	return out
}
func canonicalHash(v interface{}) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func cloneJSONValue(v interface{}) interface{} {
	raw, _ := json.Marshal(v)
	var out interface{}
	_ = json.Unmarshal(raw, &out)
	return out
}
func cloneJSONSlice(v []interface{}) []interface{} {
	if v == nil {
		return nil
	}
	return cloneJSONValue(v).([]interface{})
}
func cloneJSONMap(v map[string]interface{}) map[string]interface{} {
	if v == nil {
		return nil
	}
	return cloneJSONValue(v).(map[string]interface{})
}

func safeSnapshotValue(v interface{}) interface{} {
	switch typed := v.(type) {
	case nil:
		return nil
	case string:
		return safeSnapshotString(typed)
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i := range typed {
			out[i] = safeSnapshotValue(typed[i])
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for key, value := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "token") || strings.Contains(lower, "cookie") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || strings.Contains(lower, "g_ck") || lower == "auth" {
				continue
			}
			out[key] = safeSnapshotValue(value)
		}
		return out
	case []MCPServer:
		return safeMCPServers(typed)
	default:
		return cloneJSONValue(typed)
	}
}

func safeSnapshotString(value string) string {
	return snapshotCredentialPattern.ReplaceAllString(value, "[redacted]")
}

func safeSnapshotURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return safeSnapshotString(raw)
	}
	if u.User != nil {
		u.User = nil
	}
	query := u.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "auth") {
			query.Del(key)
			continue
		}
		for i, value := range query[key] {
			query[key][i] = safeSnapshotString(value)
		}
	}
	u.RawQuery = query.Encode()
	u.Path = safeSnapshotString(u.Path)
	u.RawPath = ""
	u.Fragment = safeSnapshotString(u.Fragment)
	u.RawFragment = ""
	u.Opaque = safeSnapshotString(u.Opaque)
	return safeSnapshotString(u.String())
}
