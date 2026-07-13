package main

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

var snapshotCredentialPattern = regexp.MustCompile(`(?i)(bearer\s+\S+|basic\s+\S+|(?:oauth[_-]?)?token\s*[:=]\s*\S+|password\s*[:=]\s*\S+|g_ck\s*[:=]\s*\S+|cookie\s*[:=]\s*\S+)`)

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
	servers := safeSnapshotValue(canonicalMCPServers(mcpServers)).([]MCPServer)
	app := snapshotApp(c.currentApp, c.appScope)
	timeouts := make(map[string]time.Duration, len(c.webStartupConfig.ToolTimeouts))
	for name, timeout := range c.webStartupConfig.ToolTimeouts {
		timeouts[name] = timeout
	}
	return TurnRuntimeSnapshot{
		CapturedAt: time.Now().UTC(), Profile: strings.TrimSpace(c.opts.Profile), InstanceURL: instanceURL, InstanceHost: host,
		Transport: c.turnTransport(), AuthMode: authMode, AuthCapabilities: caps, ConversationID: c.conversationID,
		Workspace: TurnWorkspaceSnapshot{Name: c.workspaceName, URI: c.workspaceURI, Checksum: c.workspaceChecksum, Folders: append([]WebWorkspaceFolder(nil), c.workspaceFolders...)},
		App:       app, WorkingSet: workingSet, WorkingSetHash: canonicalHash(workingSet), IDEContext: safeSnapshotValue(c.currentIDEContext()).(map[string]interface{}), ConversationHistory: safeSnapshotValue(nirvanaConversationHistory(c.history)).([]interface{}),
		Model:      TurnModelSnapshot{Provider: c.runtime.Provider, LargeModel: effectiveLargeModel(c), SmallModel: c.runtime.SmallModel, CapabilityID: effectiveCapabilityID(c), SkillID: strings.TrimSpace(c.webAgentConfig.SkillID), LargeMaxOutputTokens: c.runtime.LargeMaxOutputTokens, SmallMaxOutputTokens: c.runtime.SmallMaxOutputTokens, LargeThinkingTokens: c.runtime.LargeThinkingTokens, SmallThinkingTokens: c.runtime.SmallThinkingTokens, LargeTemperature: c.runtime.LargeTemperature, SmallTemperature: c.runtime.SmallTemperature},
		MCPServers: servers, MCPGeneration: canonicalHash(servers), MCPHash: canonicalHash(servers), Timeouts: timeouts, DefaultTimeout: c.webStartupConfig.DefaultTimeout, RetryPolicy: "transport-managed", CLIVersion: turnRuntimeCLIVersion, BuildSHA: strings.TrimSpace(turnRuntimeBuildSHA),
	}
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
		return snapshotCredentialPattern.ReplaceAllString(typed, "[redacted]")
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
		out := cloneMCPServers(typed)
		for i := range out {
			out[i].URL = safeSnapshotURL(out[i].URL)
		}
		return out
	default:
		return cloneJSONValue(typed)
	}
}

func safeSnapshotURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return snapshotCredentialPattern.ReplaceAllString(raw, "[redacted]")
	}
	if u.User != nil {
		u.User = nil
	}
	query := u.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "auth") {
			query.Del(key)
		}
	}
	u.RawQuery = query.Encode()
	return u.String()
}
