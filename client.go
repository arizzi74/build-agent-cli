package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	cfg     CLIConfig
	opts    Options
	runtime RuntimeModelConfig

	conn    *websocket.Conn
	writeMu sync.Mutex

	httpClient          *http.Client
	gatewayAuth         string
	basicUser           string
	basicPass           string
	sessionCookieHeader string
	userToken           string
	oauthAccessToken    string
	ambURL              string
	ambChannel          string
	ambClient           string
	ambMsgID            int
	ambUseForm          bool
	ambSubscribed       bool
	ambUnavailable      bool
	ambCancel           context.CancelFunc

	conversationID         string
	conversationTitle      string
	conversationState      string
	serverConversation     bool
	history                []interface{}
	usageInputTokens       int64
	usageOutputTokens      int64
	usageThinkingTokens    int64
	appScope               interface{}
	currentApp             *AppScope
	workingSet             interface{}
	workspaceName          string
	streamTypes            map[string]string
	webAgentConfig         WebAgentConfig
	webStreamID            string
	webStreamType          string
	webStreamText          string
	webStreamTS            string
	webStreamRows          int
	webStreamPlain         bool
	webStreamBulletStarted bool
	nirvanaMCPServers      []MCPServer
	nirvanaMCPServersReady bool
	toolCallNames          map[string]string
	turnCompletedByAMB     bool
	responseTransport      string
	warnedLegacySend       bool
	pendingUserContent     string
	nirvanaThinkingText    string
	nirvanaThinkingStarted time.Time
	nirvanaThinkingDone    bool
	turnStatusActive       bool
	turnStatusStop         chan struct{}
	turnStatusDone         chan struct{}
	statusMu               sync.Mutex

	connected chan error
	turnDone  chan error
	closed    chan struct{}

	processing bool
	debug      bool
}

type WebAgentConfig struct {
	Model           string
	ProviderURL     string
	SkillID         string
	GlideAttributes map[string]interface{}
}

type MCPServer struct {
	ServerID  string `json:"serverId"`
	Transport string `json:"transport"`
	Name      string `json:"name"`
	URL       string `json:"url,omitempty"`
	Source    string `json:"source"`
}

var excludedWDFMCPServerGitPattern = regexp.MustCompile(`(?i)\bgit\b`)

func NewClient(cfg CLIConfig, opts Options) (*Client, error) {
	runtimeCfg, err := selectRuntimeModel(opts.Provider, opts.Model)
	if err != nil {
		return nil, err
	}
	c := &Client{
		cfg:           cfg,
		opts:          opts,
		runtime:       runtimeCfg,
		streamTypes:   map[string]string{},
		toolCallNames: map[string]string{},
		connected:     make(chan error, 1),
		closed:        make(chan struct{}),
		debug:         opts.Debug,
	}
	if err := c.loadInitialState(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) loadInitialState() error {
	name := readActiveWorkspaceName(c.opts.Profile)
	ws, ok := loadWorkspace(c.opts.Profile, name)
	if !ok {
		ws = newWorkspaceState(name)
		if app, ok := loadActiveApp(c.opts.Profile); ok {
			ws.App = app
			ws.AppScope = app.ScopeID
		}
		if err := saveWorkspace(c.opts.Profile, ws); err != nil {
			return err
		}
	}
	c.applyWorkspace(ws)
	return saveActiveWorkspaceName(c.opts.Profile, c.workspaceName)
}

func (c *Client) applyWorkspace(ws WorkspaceState) {
	if ws.Name == "" {
		ws.Name = defaultWorkspaceName
	}
	c.workspaceName = ws.Name
	c.conversationID = ws.ConversationID
	c.conversationTitle = ws.ConversationTitle
	c.conversationState = ws.ConversationState
	c.serverConversation = ws.ServerConversation
	c.history = ws.ConversationHistory
	c.usageInputTokens = ws.UsageInputTokens
	c.usageOutputTokens = ws.UsageOutputTokens
	c.usageThinkingTokens = ws.UsageThinkingTokens
	c.workingSet = ws.WorkingSet
	c.appScope = ws.AppScope
	c.currentApp = ws.App
	if c.currentApp != nil && c.currentApp.ScopeID != "" {
		c.appScope = c.currentApp.ScopeID
	} else if c.appScope == nil {
		if app, ok := loadActiveApp(c.opts.Profile); ok {
			c.currentApp = app
			c.appScope = app.ScopeID
		}
	}
}

func (c *Client) WorkspaceState() WorkspaceState {
	name := c.workspaceName
	if name == "" {
		name = defaultWorkspaceName
	}
	return WorkspaceState{
		Name:                name,
		ConversationID:      c.conversationID,
		ConversationTitle:   c.conversationTitle,
		ConversationState:   c.conversationState,
		ServerConversation:  c.serverConversation,
		ConversationHistory: c.history,
		UsageInputTokens:    c.usageInputTokens,
		UsageOutputTokens:   c.usageOutputTokens,
		UsageThinkingTokens: c.usageThinkingTokens,
		WorkingSet:          c.workingSet,
		AppScope:            c.appScope,
		App:                 c.currentApp,
	}
}

func (c *Client) saveCurrentState() error {
	return saveWorkspace(c.opts.Profile, c.WorkspaceState())
}

func (c *Client) SwitchWorkspace(name string, create bool) error {
	if c.processing {
		return errors.New("cannot switch workspace while a turn is processing")
	}
	if !isValidWorkspaceName(name) {
		return fmt.Errorf("invalid workspace %q: use letters, numbers, dash or underscore", name)
	}
	if c.workspaceName != "" {
		if err := c.saveCurrentState(); err != nil {
			return err
		}
	}
	ws, ok := loadWorkspace(c.opts.Profile, name)
	if !ok {
		if !create {
			return fmt.Errorf("workspace %q does not exist", name)
		}
		ws = newWorkspaceState(name)
		if c.currentApp != nil {
			ws.App = c.currentApp
			ws.AppScope = c.currentApp.ScopeID
		}
		if err := saveWorkspace(c.opts.Profile, ws); err != nil {
			return err
		}
	}
	c.applyWorkspace(ws)
	if err := saveActiveWorkspaceName(c.opts.Profile, name); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "workspace: %s\n", name)
	return nil
}

func (c *Client) ResetWorkspace() error {
	if c.processing {
		return errors.New("cannot reset workspace while a turn is processing")
	}
	c.conversationID = ""
	c.conversationTitle = ""
	c.conversationState = ""
	c.serverConversation = false
	c.history = nil
	c.resetUsageTotals()
	c.workingSet = nil
	if err := c.saveCurrentState(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "workspace reset: %s\n", c.workspaceName)
	return nil
}

func (c *Client) CurrentApp() *AppScope {
	return c.currentApp
}

func (c *Client) SetApp(app AppScope) error {
	app.ScopeID = strings.TrimSpace(app.ScopeID)
	if app.ScopeID == "" {
		return errors.New("scope id is required")
	}
	if app.ScopeName == "" {
		app.ScopeName = app.ScopeID
	}
	c.currentApp = &app
	c.appScope = app.ScopeID
	if err := saveActiveApp(c.opts.Profile, app); err != nil {
		return err
	}
	return c.saveCurrentState()
}

func (c *Client) ClearApp() error {
	c.currentApp = nil
	c.appScope = nil
	if err := deleteActiveApp(c.opts.Profile); err != nil {
		return err
	}
	return c.saveCurrentState()
}

func (c *Client) absorbAppScope(v interface{}) {
	c.appScope = v
	switch typed := v.(type) {
	case string:
		if typed != "" && (c.currentApp == nil || c.currentApp.ScopeID != typed) {
			c.currentApp = &AppScope{ScopeID: typed, ScopeName: typed}
			_ = saveActiveApp(c.opts.Profile, *c.currentApp)
		}
	case map[string]interface{}:
		if app := appFromPayload(typed); app != nil {
			c.currentApp = app
			c.appScope = app.ScopeID
			_ = saveActiveApp(c.opts.Profile, *app)
		}
	}
}

func (c *Client) Connect(ctx context.Context) error {
	if !c.opts.Nirvana {
		return c.connectGateway(ctx)
	}
	if err := c.prepareNirvanaConversationSelection(ctx); err != nil {
		return err
	}

	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	conn, _, err := dialer.DialContext(ctx, c.cfg.WSURL, c.nirvanaDialHeaders())
	if err != nil {
		return fmt.Errorf("websocket dial failed: %w", err)
	}
	c.conn = conn
	go c.readLoop()

	invokeOptions, params, err := c.buildInvokePayload(ctx, false)
	if err != nil {
		_ = c.Close()
		return err
	}
	mcpServers := c.nirvanaMCPServerPayload(ctx)
	payload := map[string]interface{}{
		"type":             "connect",
		"protocol_version": 1,
		"capabilities":     c.clientCapabilities(),
		"invokeOptions":    invokeOptions,
		"params":           params,
		"mcpServers":       mcpServers,
	}
	if err := c.writeJSON(payload); err != nil {
		_ = c.Close()
		return err
	}

	select {
	case err := <-c.connected:
		return err
	case <-ctx.Done():
		_ = c.Close()
		return ctx.Err()
	case <-time.After(30 * time.Second):
		_ = c.Close()
		return errors.New("connection handshake timed out after 30s")
	}
}

func (c *Client) prepareNirvanaConversationSelection(ctx context.Context) error {
	if c.opts.CodeAssistWS || (c.opts.Conversation == "" && !c.shouldPromptStartupConversation()) {
		return nil
	}
	tok, err := getAccessToken(ctx, oauthConfig(c.cfg), c.opts.Profile, c.cfg.InstanceURL, c.opts.NoOpen, false)
	if err != nil {
		return err
	}
	c.oauthAccessToken = tok.AccessToken
	if c.opts.Conversation != "" {
		return c.SelectConversationByArg(ctx, c.opts.Conversation, true)
	}
	return c.PromptWebConversation(ctx, "startup")
}

func (c *Client) Close() error {
	if c.ambCancel != nil {
		c.ambCancel()
		c.ambCancel = nil
	}
	if c.conn == nil {
		return nil
	}
	_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second))
	err := c.conn.Close()
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return err
}

func (c *Client) SendMessage(ctx context.Context, content string) error {
	if !c.opts.Nirvana {
		return c.sendGatewayMessage(ctx, content)
	}

	if c.conn == nil {
		return errors.New("not connected")
	}
	if c.processing {
		return errors.New("a turn is already processing")
	}
	c.ensureNirvanaConversationID()
	invokeOptions, params, err := c.buildInvokePayload(ctx, false)
	if err != nil {
		return err
	}
	mcpServers := c.nirvanaMCPServerPayload(ctx)
	payload := map[string]interface{}{
		"type":                "message",
		"conversation_id":     c.conversationID,
		"content":             content,
		"conversationHistory": nirvanaConversationHistory(c.history),
		"ideContext":          emptyIDEContext(),
		"images":              []interface{}{},
		"attachments":         []interface{}{},
		"isGreeting":          false,
		"isMCPRetry":          false,
		"mcpServers":          mcpServers,
		"workingSet":          []interface{}{},
		"invokeOptions":       invokeOptions,
		"params":              params,
	}
	if c.appScope != nil {
		payload["appScope"] = c.appScope
	}
	if c.workingSet != nil {
		payload["workingSet"] = c.workingSet
	}

	if err := c.ensureWebConversationWithTitle(ctx, content); err != nil {
		return err
	}
	if err := c.persistWebUserMessage(ctx, content); err != nil {
		return err
	}
	payload["conversation_id"] = c.conversationID
	if attrs := asMap(invokeOptions["attributes"]); attrs != nil {
		attrs["conversationId"] = c.conversationID
	}
	c.resetWebStream()
	c.pendingUserContent = content
	c.turnDone = make(chan error, 1)
	c.processing = true
	if err := c.writeJSON(payload); err != nil {
		c.processing = false
		c.pendingUserContent = ""
		return err
	}
	return nil
}

func (c *Client) ensureNirvanaConversationID() {
	if normalizedConversationID := normalizeGatewayConversationID(c.conversationID); normalizedConversationID != "" {
		c.conversationID = normalizedConversationID
		return
	}
	c.conversationID = uuidV4Compact()
}

func (c *Client) WaitTurn(ctx context.Context) error {
	if c.turnDone == nil {
		return nil
	}
	select {
	case err := <-c.turnDone:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errors.New("connection closed")
	}
}

func (c *Client) connectGateway(ctx context.Context) error {
	if c.opts.CodeAssistWS {
		return c.connectCodeAssistGateway(ctx)
	}
	if err := c.configureGatewayAuth(ctx); err != nil {
		return err
	}
	if c.opts.Conversation != "" {
		if err := c.SelectConversationByArg(ctx, c.opts.Conversation, true); err != nil {
			return err
		}
	} else if c.shouldPromptStartupConversation() {
		if err := c.PromptWebConversation(ctx, "startup"); err != nil {
			return err
		}
	}
	normalizedConversationID := normalizeGatewayConversationID(c.conversationID)
	if normalizedConversationID == "" {
		normalizedConversationID = uuidV4Compact()
	}
	if c.conversationID != normalizedConversationID {
		c.conversationID = normalizedConversationID
		c.serverConversation = false
		c.conversationTitle = ""
		c.conversationState = ""
		if err := c.saveCurrentState(); err != nil {
			return err
		}
	}
	c.ambURL = strings.TrimRight(c.cfg.InstanceURL, "/") + "/amb"
	c.ambChannel = "/build_agent_core/stream/" + c.conversationID
	c.ambMsgID = 0

	if err := c.ambHandshake(ctx); err != nil {
		return err
	}
	if !c.suppressInteractiveStartupScrollback() {
		fmt.Fprintf(os.Stderr, "connected to %s via web gateway\n", c.cfg.InstanceURL)
		fmt.Fprintf(os.Stderr, "stream channel: %s\n", c.ambChannel)
	}
	return nil
}

func (c *Client) connectCodeAssistGateway(ctx context.Context) error {
	if err := c.configureGatewayAuth(ctx); err != nil {
		return err
	}
	if c.userToken == "" && c.gatewayAuth != authModeBasic {
		c.userToken = c.fetchUserToken(ctx)
	}
	agentConfig, err := c.fetchWebAgentConfig(ctx)
	if err != nil {
		return err
	}
	c.webAgentConfig = agentConfig

	wsURL := deriveCodeAssistWSURL(c.cfg.InstanceURL)
	if c.opts.WSURL != "" {
		wsURL = c.opts.WSURL
	}
	header := http.Header{}
	setBrowserishHeadersForBase(header, c.cfg.InstanceURL)
	if c.gatewayAuth == authModeBasic && c.basicUser != "" {
		header.Set("Authorization", c.basicAuthorization())
	}
	if c.sessionCookieHeader != "" {
		header.Set("Cookie", c.sessionCookieHeader)
	}
	if c.userToken != "" {
		header.Set("X-UserToken", c.userToken)
	}

	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	if c.httpClient != nil {
		dialer.Jar = c.httpClient.Jar
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return fmt.Errorf("web UI websocket dial failed: %w%s", err, websocketResponseSuffix(resp))
	}
	c.conn = conn
	go c.readGatewayWebSocketLoop()
	if !c.suppressInteractiveStartupScrollback() {
		fmt.Fprintf(os.Stderr, "connected to %s via web UI websocket\n", c.cfg.InstanceURL)
		fmt.Fprintf(os.Stderr, "agent config: model=%s skill=%s\n", c.webAgentConfig.Model, c.webAgentConfig.SkillID)
	}
	return nil
}

func (c *Client) suppressInteractiveStartupScrollback() bool {
	return len(c.opts.Prompts) == 0 && interactiveTerminalUIEnabled()
}

func (c *Client) nirvanaDialHeaders() http.Header {
	header := http.Header{}
	setBrowserishHeadersForBase(header, c.cfg.InstanceURL)
	header.Set("Referer", strings.TrimRight(c.cfg.InstanceURL, "/")+"/sn_glider_app/ide.do")
	if session, ok := loadMatchingWebSession(c.opts.Profile, c.cfg.InstanceURL); ok {
		if cookieHeader := normalizeCookieHeader(session.CookieHeader); cookieHeader != "" {
			header.Set("Cookie", cookieHeader)
		}
		if userToken := strings.TrimSpace(session.UserToken); userToken != "" {
			header.Set("X-UserToken", userToken)
		}
	}
	return header
}

func (c *Client) sendGatewayMessage(ctx context.Context, content string) error {
	if c.opts.CodeAssistWS {
		return c.sendCodeAssistGatewayMessage(ctx, content)
	}

	if c.httpClient == nil || c.ambClient == "" {
		return errors.New("not connected to web gateway")
	}
	if c.processing {
		return errors.New("a turn is already processing")
	}
	if c.conversationID == "" {
		c.conversationID = uuidV4Compact()
	} else if normalizedConversationID := normalizeGatewayConversationID(c.conversationID); normalizedConversationID != "" && normalizedConversationID != c.conversationID {
		c.conversationID = normalizedConversationID
		c.serverConversation = false
	}
	if err := c.ensureWebConversationWithTitle(ctx, content); err != nil {
		return err
	}
	if err := c.persistWebUserMessage(ctx, content); err != nil {
		return err
	}
	c.resetWebStream()
	c.pendingUserContent = content
	c.turnDone = make(chan error, 1)
	c.processing = true
	streamReady := false
	if c.canUseAMB() {
		if err := c.retryAMBSubscribe(ctx, 15*time.Second); err != nil {
			if c.debug {
				fmt.Fprintf(os.Stderr, "warning: AMB subscribe before send failed; will continue without live stream: %v\n", err)
			}
		} else {
			streamReady = true
		}
	}
	c.showTurnStatus()
	body, needsStream, err := c.postBuildAgentMessage(ctx, content)
	if err != nil {
		c.clearTurnStatus()
		c.processing = false
		c.pendingUserContent = ""
		return err
	}
	if !needsStream {
		if c.responseTransport == "legacy-send" && !c.warnedLegacySend {
			c.clearTurnStatus()
			if streamReady {
				fmt.Fprintln(os.Stderr, "response transport: legacy /send fallback; using AMB if this instance emits stream chunks")
			} else {
				fmt.Fprintln(os.Stderr, "response transport: legacy /send fallback; this HTTP endpoint is non-streaming")
			}
			fmt.Fprintln(os.Stderr, "hint: Glider Build Agent web UI streaming uses the Nirvana websocket; restart with --nirvana for streaming parity on this instance")
			c.warnedLegacySend = true
		}
		if c.turnCompletedByAMB {
			return nil
		}
		if strings.TrimSpace(c.webStreamText) != "" {
			c.finishGatewayStreamOutput()
			c.clearTurnStatus()
			c.appendGatewayRESTTurnHistory()
			c.turnCompletedByAMB = true
			if err := c.saveCurrentState(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not save workspace %q: %v\n", c.workspaceName, err)
			}
			c.processing = false
			if c.turnDone != nil {
				select {
				case c.turnDone <- nil:
				default:
				}
			}
			return nil
		}
		if err := c.printLegacyBuildAgentResponse(ctx, body, content); err != nil {
			c.clearTurnStatus()
			c.processing = false
			c.pendingUserContent = ""
			return err
		}
		c.processing = false
		c.pendingUserContent = ""
		if c.turnDone != nil {
			select {
			case c.turnDone <- nil:
			default:
			}
		}
		return nil
	}
	if needsStream && !streamReady && !c.ambSubscribed {
		if err := c.retryAMBSubscribe(ctx, 15*time.Second); err != nil {
			c.clearTurnStatus()
			c.processing = false
			return fmt.Errorf("gateway message sent, but AMB subscribe failed after POST: %w", err)
		}
	}
	if c.debug && len(body) > 0 {
		fmt.Fprintf(os.Stderr, "\n[gateway] %s\n", trimBody(body))
	}
	return nil
}

func (c *Client) sendCodeAssistGatewayMessage(_ context.Context, content string) error {
	if c.conn == nil {
		return errors.New("not connected to web UI websocket")
	}
	if c.processing {
		return errors.New("a turn is already processing")
	}
	if normalizedConversationID := normalizeCodeAssistConversationID(c.conversationID); normalizedConversationID != "" {
		c.conversationID = normalizedConversationID
	} else {
		c.conversationID = uuidV4()
	}
	c.turnDone = make(chan error, 1)
	c.processing = true
	if err := c.sendCodeAssistPayload(content); err != nil {
		c.processing = false
		return err
	}
	c.showTurnStatus()
	return nil
}

func (c *Client) fetchWebAgentConfig(ctx context.Context) (WebAgentConfig, error) {
	fallback := WebAgentConfig{Model: c.runtime.LargeModel, ProviderURL: c.cfg.LLMProxyURL, SkillID: c.cfg.CapabilityID}
	base := strings.TrimRight(c.cfg.InstanceURL, "/")
	endpoints := []string{
		base + "/api/sn_ba_core/agent_config_api/config",
		base + "/api/sn_build_agent/build_agent_api/providerConfig",
	}
	var lastErr error
	for _, endpoint := range endpoints {
		body, _, err := c.getJSON(ctx, endpoint)
		if err != nil {
			lastErr = err
			continue
		}
		cfg := parseWebAgentConfig(body, fallback)
		if cfg.Model != "" && cfg.ProviderURL != "" && cfg.SkillID != "" {
			if c.opts.Model != "" || c.opts.Provider != "" {
				cfg.Model = c.runtime.LargeModel
			}
			return cfg, nil
		}
	}
	if fallback.Model != "" && fallback.ProviderURL != "" && fallback.SkillID != "" {
		if c.debug && lastErr != nil {
			fmt.Fprintf(os.Stderr, "warning: using fallback agent config after config API failure: %v\n", lastErr)
		}
		return fallback, nil
	}
	if lastErr != nil {
		return WebAgentConfig{}, lastErr
	}
	return WebAgentConfig{}, errors.New("agent config API did not return model/providerUrl/skillId")
}

func (c *Client) tryLoadNirvanaWebAgentConfig(ctx context.Context) {
	if !c.opts.Nirvana || c.webAgentConfig.Model != "" || c.cfg.InstanceURL == "" {
		return
	}
	session, ok := loadMatchingWebSession(c.opts.Profile, c.cfg.InstanceURL)
	if !ok {
		return
	}
	if err := c.initGatewayHTTPClient(); err != nil {
		return
	}
	c.applyWebSession(session)
	cfg, err := c.fetchWebAgentConfig(ctx)
	if err != nil {
		if c.debug {
			fmt.Fprintf(os.Stderr, "warning: could not load web provider config for Nirvana websocket: %v\n", err)
		}
		return
	}
	c.webAgentConfig = cfg
}

func parseWebAgentConfig(body []byte, fallback WebAgentConfig) WebAgentConfig {
	var envelope map[string]interface{}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fallback
	}
	result := asMap(envelope["result"])
	if result == nil {
		result = envelope
	}
	cfg := fallback
	if model := stringify(result["model"]); model != "" {
		cfg.Model = model
	}
	if providerURL := firstString(result, "providerUrl", "provider_url"); providerURL != "" {
		cfg.ProviderURL = providerURL
	}
	if skillID := firstString(result, "skillId", "skill_id"); skillID != "" {
		cfg.SkillID = skillID
	}

	if modelConfig := asMap(result["modelConfig"]); modelConfig != nil {
		if large := asMap(modelConfig["largeConfig"]); large != nil {
			if model := stringify(large["model"]); model != "" {
				cfg.Model = model
			}
		}
	}
	if attrs := asMap(result["attributes"]); attrs != nil {
		if providerURL := firstString(attrs, "baseUrl", "providerUrl", "provider_url"); providerURL != "" {
			cfg.ProviderURL = providerURL
		}
		if skillID := firstString(attrs, "capabilityId", "skillId", "skill_id"); skillID != "" {
			cfg.SkillID = skillID
		}
	}
	if glideAttrs := asMap(result["glideAttributes"]); glideAttrs != nil {
		cfg.GlideAttributes = cloneMap(glideAttrs)
	}
	return cfg
}

func cloneMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(stringify(m[key])); v != "" {
			return v
		}
	}
	return ""
}

func emptyIDEContext() map[string]interface{} {
	return map[string]interface{}{
		"currentFile":      "",
		"currentDir":       "",
		"selectedText":     "",
		"workspaceFolders": []interface{}{},
		"openEditors":      []interface{}{},
	}
}

func instanceNameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "local_dev"
	}
	host := u.Hostname()
	if i := strings.Index(host, "."); i > 0 {
		return host[:i]
	}
	return host
}

func (c *Client) getJSON(ctx context.Context, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	c.setGatewayHeaders(req)
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return body, res.StatusCode, fmt.Errorf("GET %s failed (%d): %s", endpoint, res.StatusCode, trimBody(body))
	}
	return body, res.StatusCode, nil
}

func (c *Client) nirvanaMCPServerPayload(ctx context.Context) []MCPServer {
	if c.nirvanaMCPServersReady {
		return cloneMCPServers(c.nirvanaMCPServers)
	}
	servers, err := c.fetchWDFMCPServers(ctx)
	if err != nil && c.debug {
		fmt.Fprintf(os.Stderr, "warning: could not discover WDF MCP servers; using static Glider MCP defaults: %v\n", err)
	}
	servers = mergeMCPServers(servers, gliderStaticMCPServers())
	c.nirvanaMCPServers = cloneMCPServers(servers)
	c.nirvanaMCPServersReady = true
	return cloneMCPServers(c.nirvanaMCPServers)
}

func (c *Client) fetchWDFMCPServers(ctx context.Context) ([]MCPServer, error) {
	if c.httpClient == nil {
		if err := c.initGatewayHTTPClient(); err != nil {
			return nil, err
		}
	}
	if c.sessionCookieHeader == "" {
		if session, ok := loadMatchingWebSession(c.opts.Profile, c.cfg.InstanceURL); ok {
			c.applyWebSession(session)
		}
	}
	const limit = 50
	base := strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/sn_wdf_mcp_client/mcp/servers"
	servers := []MCPServer{}
	for offset := 0; ; offset += limit {
		endpoint := fmt.Sprintf("%s?limit=%d&offset=%d&connected=true", base, limit, offset)
		body, _, err := c.getJSON(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		pageServers, total, hasTotal := parseWDFMCPServersPage(body)
		servers = append(servers, pageServers...)
		if !hasTotal || offset+limit >= total {
			break
		}
	}
	return servers, nil
}

func parseWDFMCPServers(body []byte) []MCPServer {
	servers, _, _ := parseWDFMCPServersPage(body)
	return servers
}

func parseWDFMCPServersPage(body []byte) ([]MCPServer, int, bool) {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, 0, false
	}
	serversRaw := findServersArray(root)
	servers := make([]MCPServer, 0, len(serversRaw))
	for _, raw := range serversRaw {
		m := asMap(raw)
		if m == nil {
			continue
		}
		serverID := firstString(m, "serverId", "server_id", "id")
		name := firstString(m, "name", "label", "displayName", "display_name")
		if serverID == "" || name == "" {
			continue
		}
		if isExcludedWDFMCPServerName(name) {
			continue
		}
		transport := normalizeMCPTransport(firstString(m, "transport", "transportType", "transport_type"))
		if !isValidMCPTransport(transport) {
			continue
		}
		servers = append(servers, MCPServer{
			ServerID:  serverID,
			Transport: transport,
			Name:      name,
			Source:    "wdf",
		})
	}
	total, hasTotal := findMetaTotal(root)
	return servers, total, hasTotal
}

func findServersArray(v interface{}) []interface{} {
	switch typed := v.(type) {
	case map[string]interface{}:
		if servers, ok := typed["servers"].([]interface{}); ok {
			return servers
		}
		for _, key := range []string{"result", "data"} {
			if child, ok := typed[key]; ok {
				if servers := findServersArray(child); servers != nil {
					return servers
				}
			}
		}
	case []interface{}:
		return typed
	}
	return nil
}

func findMetaTotal(v interface{}) (int, bool) {
	switch typed := v.(type) {
	case map[string]interface{}:
		if meta := asMap(typed["meta"]); meta != nil {
			if total, ok := interfaceToInt(meta["total"]); ok {
				return total, true
			}
		}
		for _, key := range []string{"result", "data"} {
			if child, ok := typed[key]; ok {
				if total, ok := findMetaTotal(child); ok {
					return total, true
				}
			}
		}
	}
	return 0, false
}

func interfaceToInt(v interface{}) (int, bool) {
	switch typed := v.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		if i, err := typed.Int64(); err == nil {
			return int(i), true
		}
	case string:
		var n json.Number = json.Number(strings.TrimSpace(typed))
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

func normalizeMCPTransport(transport string) string {
	transport = strings.ToLower(strings.TrimSpace(transport))
	transport = strings.ReplaceAll(transport, "_", "-")
	transport = strings.ReplaceAll(transport, " ", "-")
	return transport
}

func isValidMCPTransport(transport string) bool {
	switch transport {
	case "sse", "streamable-http", "http", "stdio":
		return true
	default:
		return false
	}
}

func isExcludedWDFMCPServerName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(normalized, "github") || excludedWDFMCPServerGitPattern.MatchString(normalized)
}

func gliderStaticMCPServers() []MCPServer {
	// The Glider Build Agent extension ships this static server enabled by
	// default (build-agent.enableAtfMcpServer = "on"). The HAR shows the web UI
	// sending it alongside WDF-discovered servers on both connect and message
	// frames.
	return []MCPServer{{
		ServerID:  "atf-cloud-runner",
		Name:      "ATF Cloud runner",
		URL:       "https://atf-rel-boq/mcp",
		Transport: "streamable-http",
		Source:    "static",
	}}
}

func mergeMCPServers(groups ...[]MCPServer) []MCPServer {
	merged := []MCPServer{}
	seen := map[string]bool{}
	for _, group := range groups {
		for _, server := range group {
			server.ServerID = strings.TrimSpace(server.ServerID)
			server.Name = strings.TrimSpace(server.Name)
			server.Transport = normalizeMCPTransport(server.Transport)
			server.Source = strings.TrimSpace(server.Source)
			if server.ServerID == "" || server.Name == "" {
				continue
			}
			if server.Transport == "" {
				server.Transport = "sse"
			}
			if !isValidMCPTransport(server.Transport) {
				continue
			}
			if server.Source == "" {
				server.Source = "wdf"
			}
			if seen[server.ServerID] {
				continue
			}
			seen[server.ServerID] = true
			merged = append(merged, server)
		}
	}
	return merged
}

func cloneMCPServers(in []MCPServer) []MCPServer {
	out := make([]MCPServer, len(in))
	copy(out, in)
	return out
}

func (c *Client) sendCodeAssistPayload(content string) error {
	messages := codeAssistMessages(c.history)
	lastSnapshot := lastCodeAssistSnapshot(messages)
	isResumeTurn := lastCodeAssistMessageType(messages) == "tool_use"
	messageID := uuidV4()
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	messageEnvelope := codeAssistUserMessage(messageID, timestamp, content)
	chatHistory := codeAssistChatHistory(append(messages, messageEnvelope))

	payload := map[string]interface{}{
		"content":         content,
		"conversationId":  c.conversationID,
		"model":           c.webAgentConfig.Model,
		"providerUrl":     c.webAgentConfig.ProviderURL,
		"skillId":         c.webAgentConfig.SkillID,
		"chatHistory":     chatHistory,
		"snapshot":        lastSnapshot,
		"instanceOrigin":  strings.TrimRight(c.cfg.InstanceURL, "/"),
		"id":              messageID,
		"messageSentTime": time.Now().UnixMilli(),
	}
	if isResumeTurn {
		payload["content"] = nil
		payload["result"] = map[string]interface{}{
			"toolUseId": toolUseIDFromSnapshot(lastSnapshot),
			"status":    "complete",
			"content": map[string]interface{}{
				"json": map[string]interface{}{"answer": content},
			},
		}
	}
	c.addCodeAssistAuth(payload)
	c.history = append(messages, messageEnvelope)
	c.resetWebStream()
	return c.writeJSON(payload)
}

func (c *Client) postBuildAgentMessage(ctx context.Context, content string) ([]byte, bool, error) {
	c.responseTransport = "core-gateway"
	base := strings.TrimRight(c.cfg.InstanceURL, "/")
	primaryPayload := map[string]interface{}{
		"payload": map[string]interface{}{
			"conversationId":   c.conversationID,
			"content":          content,
			"clientToolSchema": []interface{}{},
		},
	}
	if c.appScope != nil {
		primaryPayload["payload"].(map[string]interface{})["appScope"] = c.appScope
	}
	if c.workingSet != nil {
		primaryPayload["payload"].(map[string]interface{})["workingSet"] = c.workingSet
	}
	body, status, err := c.postJSON(ctx, base+"/api/sn_ba_core/agent_gateway_api/conversation", primaryPayload)
	if err == nil {
		c.responseTransport = "core-gateway"
		return body, true, nil
	}
	if !isMissingRESTResource(status, body) {
		return body, false, err
	}

	legacyPayload := map[string]interface{}{
		"payload": map[string]interface{}{
			"requestId": c.conversationID,
			"message":   content,
			"messages":  c.legacyBuildAgentMessages(content),
		},
	}
	if c.appScope != nil {
		legacyPayload["payload"].(map[string]interface{})["appScope"] = c.appScope
	}
	if c.workingSet != nil {
		legacyPayload["payload"].(map[string]interface{})["workingSet"] = c.workingSet
	}
	c.responseTransport = "legacy-send"
	body, _, legacyErr := c.postJSON(ctx, base+"/api/sn_build_agent/build_agent_api/send", legacyPayload)
	if legacyErr != nil {
		if isUserNotAuthenticated(body) {
			return body, false, c.buildAgentAuthRejectedError(body)
		}
		return body, false, legacyErr
	}
	return body, false, nil
}

func (c *Client) legacyBuildAgentMessages(content string) []map[string]string {
	messages := make([]map[string]string, 0, len(c.history)+1)
	for _, raw := range c.history {
		msg := asMap(raw)
		if msg == nil {
			continue
		}
		role := legacyChatRole(firstString(msg, "role", "author", "type", "sender"))
		text := strings.TrimSpace(firstString(msg, "content", "text"))
		if text == "" {
			if body := asMap(msg["body"]); body != nil {
				text = strings.TrimSpace(firstString(body, "text", "message", "content"))
			}
		}
		if role != "" && text != "" {
			messages = append(messages, map[string]string{"role": role, "content": text})
		}
	}
	messages = append(messages, map[string]string{"role": "user", "content": content})
	return messages
}

func nirvanaConversationHistory(history []interface{}) []interface{} {
	out := make([]interface{}, 0, len(history))
	for _, raw := range history {
		msg := asMap(raw)
		if msg == nil {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(firstString(msg, "role", "author", "type", "sender")))
		text := historyMessageText(msg)
		if text == "" {
			continue
		}
		switch role {
		case "user", "assistant":
			out = append(out, map[string]interface{}{"role": role, "content": text})
		case "system", "stop":
			// The Glider web client serializes interruption/system notes as user
			// messages, not as a distinct chat role. Keep the useful context while
			// avoiding backend paths that assume user/assistant-only history rows.
			if !strings.HasPrefix(text, "[System:") {
				text = "[System: " + strings.TrimSpace(text) + "]"
			}
			out = append(out, map[string]interface{}{"role": "user", "content": text})
		default:
			if strings.Contains(role, "tool") || strings.Contains(role, "thinking") || role == "loading" {
				continue
			}
			if strings.Contains(role, "assistant") {
				out = append(out, map[string]interface{}{"role": "assistant", "content": text})
			}
		}
	}
	return out
}

func historyMessageText(msg map[string]interface{}) string {
	text := strings.TrimSpace(firstString(msg, "content", "text", "message"))
	if text == "" {
		if body := asMap(msg["body"]); body != nil {
			text = strings.TrimSpace(firstString(body, "text", "message", "content"))
		}
	}
	return text
}

func legacyChatRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	switch role {
	case "user", "assistant", "system":
		return role
	case "assistant-thinking", "thinking", "assistant-tool", "tool", "remote_tool", "loading":
		return ""
	default:
		if strings.Contains(role, "tool") || strings.Contains(role, "thinking") {
			return ""
		}
		if strings.Contains(role, "assistant") {
			return "assistant"
		}
		return ""
	}
}

func (c *Client) buildAgentAuthRejectedError(body []byte) error {
	if c.gatewayAuth == authModeBasic {
		return fmt.Errorf("Build Agent API rejected Basic Auth: %s; on this instance Basic Auth is not enough for Build Agent, use --auth form or --auth cookie", trimBody(body))
	}
	return fmt.Errorf("Build Agent API rejected the saved web session: %s; cookies may be expired, run --logout and authenticate again", trimBody(body))
}

func (c *Client) printLegacyBuildAgentResponse(ctx context.Context, body []byte, userContent string) error {
	var envelope map[string]interface{}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	result := asMap(envelope["result"])
	if result == nil {
		return fmt.Errorf("unexpected Build Agent response: %s", trimBody(body))
	}
	if status := stringify(result["status"]); strings.EqualFold(status, "error") || strings.EqualFold(status, "failed") {
		return fmt.Errorf("Build Agent failed: %s", trimBody(body))
	}
	printed := false
	var assistantParts []string
	if capabilities := asMap(result["capabilities"]); capabilities != nil {
		for _, rawCapability := range capabilities {
			capability := asMap(rawCapability)
			if capability == nil {
				continue
			}
			if errText := strings.TrimSpace(stringify(capability["error"])); errText != "" {
				return fmt.Errorf("Build Agent failed: %s", errText)
			}
			if response := strings.TrimSpace(stringify(capability["response"])); response != "" {
				assistantParts = append(assistantParts, response)
				printed = true
			}
			if usage := asMap(capability["usage"]); usage != nil {
				c.recordUsage(usage)
			}
		}
	}
	if !printed {
		if message := strings.TrimSpace(stringify(result["message"])); message != "" {
			assistantParts = append(assistantParts, message)
			printed = true
		}
	}
	if !printed && c.debug {
		fmt.Fprintf(os.Stderr, "\n[gateway] %s\n", trimBody(body))
	}
	if strings.TrimSpace(userContent) != "" {
		c.history = append(c.history, map[string]interface{}{"role": "user", "content": userContent})
	}
	if assistantText := strings.TrimSpace(strings.Join(assistantParts, "\n")); assistantText != "" {
		c.clearTurnStatus()
		printAssistantText(assistantText)
		if err := c.persistWebAssistantMessage(ctx, assistantText); err != nil {
			return err
		}
		c.history = append(c.history, map[string]interface{}{"role": "assistant", "content": assistantText})
	}
	c.clearTurnStatus()
	return c.saveCurrentState()
}

func asMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, endpoint string, payload interface{}) ([]byte, int, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	c.setGatewayHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return body, res.StatusCode, fmt.Errorf("gateway message failed (%d): %s", res.StatusCode, trimBody(body))
	}
	return body, res.StatusCode, nil
}

func (c *Client) patchJSON(ctx context.Context, endpoint string, payload interface{}) ([]byte, int, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	c.setGatewayHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return body, res.StatusCode, fmt.Errorf("gateway PATCH failed (%d): %s", res.StatusCode, trimBody(body))
	}
	return body, res.StatusCode, nil
}

func isMissingRESTResource(status int, body []byte) bool {
	return status == http.StatusBadRequest && bytes.Contains(bytes.ToLower(body), []byte("requested uri does not represent any resource"))
}

func isUserNotAuthenticated(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("user is not authenticated")) || bytes.Contains(lower, []byte("required to provide auth information"))
}

func (c *Client) ensureAMBSubscribed(ctx context.Context) error {
	if c.ambSubscribed {
		return nil
	}
	if !c.canUseAMBBase() {
		return errors.New("AMB stream channel is not initialized")
	}
	if err := c.ambSubscribe(ctx); err != nil {
		return err
	}
	c.startAMBLoop()
	return nil
}

func (c *Client) canUseAMB() bool {
	return !c.ambUnavailable && c.canUseAMBBase()
}

func (c *Client) canUseAMBBase() bool {
	return strings.TrimSpace(c.ambURL) != "" && strings.TrimSpace(c.ambClient) != "" && (strings.TrimSpace(c.ambChannel) != "" || strings.TrimSpace(c.conversationID) != "")
}

func (c *Client) startAMBLoop() {
	c.ambSubscribed = true
	c.ambUnavailable = false
	if c.ambCancel == nil {
		loopCtx, cancel := context.WithCancel(context.Background())
		c.ambCancel = cancel
		go c.ambLoop(loopCtx)
	}
}

func (c *Client) retryAMBSubscribe(ctx context.Context, maxWait time.Duration) error {
	if c.ambSubscribed {
		return nil
	}
	if c.ambUnavailable {
		return errors.New("AMB stream channel is unavailable on this instance")
	}
	if !c.canUseAMBBase() {
		return errors.New("AMB stream channel is not initialized")
	}
	var lastErr error
	allPermanent := true
	for _, channel := range c.ambChannelCandidates() {
		c.ambChannel = channel
		err := c.retryAMBSubscribeCurrent(ctx, maxWait)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isPermanentAMBSubscribeError(err) {
			allPermanent = false
		}
	}
	if allPermanent {
		c.ambUnavailable = true
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("AMB subscribe failed")
}

func (c *Client) retryAMBSubscribeCurrent(ctx context.Context, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.ensureAMBSubscribed(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if isPermanentAMBSubscribeError(err) {
			return err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return lastErr
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (c *Client) ambChannelCandidates() []string {
	id := strings.TrimSpace(c.conversationID)
	if id == "" && c.ambChannel != "" {
		parts := strings.Split(strings.TrimRight(c.ambChannel, "/"), "/")
		id = parts[len(parts)-1]
	}
	var candidates []string
	add := func(channel string) {
		channel = strings.TrimSpace(channel)
		if channel == "" {
			return
		}
		for _, existing := range candidates {
			if existing == channel {
				return
			}
		}
		candidates = append(candidates, channel)
	}
	add(c.ambChannel)
	if id != "" {
		if strings.HasPrefix(c.ambChannel, "/build_agent/stream/") {
			add("/build_agent/stream/" + id)
			add("/build_agent_core/stream/" + id)
		} else {
			add("/build_agent_core/stream/" + id)
			add("/build_agent/stream/" + id)
		}
	}
	return candidates
}

func isPermanentAMBSubscribeError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "no processor matches") || strings.Contains(lower, "404::message_deleted")
}

func (c *Client) ambHandshake(ctx context.Context) error {
	resp, err := c.bayeux(ctx, []map[string]interface{}{
		{
			"channel":                  "/meta/handshake",
			"version":                  "1.0",
			"minimumVersion":           "1.0",
			"supportedConnectionTypes": []string{"long-polling"},
			"advice":                   map[string]interface{}{"timeout": 60000, "interval": 0},
			"id":                       c.nextAmbID(),
		},
	})
	if err != nil {
		return err
	}
	if len(resp) == 0 {
		return errors.New("AMB handshake returned no response")
	}
	first := resp[0]
	if ok, _ := first["successful"].(bool); !ok {
		return fmt.Errorf("AMB handshake failed: %v", first)
	}
	clientID, _ := first["clientId"].(string)
	if clientID == "" {
		return fmt.Errorf("AMB handshake did not return clientId: %v", first)
	}
	c.ambClient = clientID
	return nil
}

func (c *Client) ambSubscribe(ctx context.Context) error {
	resp, err := c.bayeux(ctx, []map[string]interface{}{
		{
			"channel":      "/meta/subscribe",
			"clientId":     c.ambClient,
			"subscription": c.ambChannel,
			"id":           c.nextAmbID(),
		},
	})
	if err != nil {
		return err
	}
	if len(resp) == 0 {
		return errors.New("AMB subscribe returned no response")
	}
	if ok, _ := resp[0]["successful"].(bool); !ok {
		return fmt.Errorf("AMB subscribe failed: %v", resp[0])
	}
	return nil
}

func (c *Client) ambLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		connectCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
		resp, err := c.bayeux(connectCtx, []map[string]interface{}{
			{
				"channel":        "/meta/connect",
				"clientId":       c.ambClient,
				"connectionType": "long-polling",
				"id":             c.nextAmbID(),
			},
		})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(os.Stderr, "\n[amb] %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}
		for _, msg := range resp {
			c.handleAMBMessage(msg)
		}
	}
}

func (c *Client) handleAMBMessage(msg map[string]interface{}) {
	channel, _ := msg["channel"].(string)
	if channel != c.ambChannel {
		return
	}
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		if raw, ok := msg["data"].(string); ok && raw != "" {
			_ = json.Unmarshal([]byte(raw), &data)
		}
	}
	if data == nil {
		return
	}
	if contentType, _ := data["contentType"].(string); contentType == "stream" {
		if content, _ := data["content"].(string); content != "" {
			if text := collectGatewayStreamContent(content); text != "" {
				c.webStreamText += text
				c.printGatewayStreamUpdate(text)
			}
		}
		return
	}
	if status, _ := data["status"].(string); status == "completed" || status == "complete" {
		if c.turnCompletedByAMB {
			return
		}
		if err := gatewayFinalError(data); err != nil {
			c.processing = false
			c.pendingUserContent = ""
			c.resetWebStream()
			if c.turnDone != nil {
				select {
				case c.turnDone <- err:
				default:
				}
			}
			return
		}
		c.finishGatewayStreamOutput()
		c.clearTurnStatus()
		c.appendGatewayRESTTurnHistory()
		c.turnCompletedByAMB = true
		if err := c.saveCurrentState(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save workspace %q: %v\n", c.workspaceName, err)
		}
		c.processing = false
		if c.turnDone != nil {
			select {
			case c.turnDone <- nil:
			default:
			}
		}
	}
}

func (c *Client) bayeux(ctx context.Context, messages []map[string]interface{}) ([]map[string]interface{}, error) {
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	resp, err := c.bayeuxPost(ctx, raw, c.ambUseForm)
	if err == nil {
		return resp, nil
	}
	if !c.ambUseForm {
		resp, formErr := c.bayeuxPost(ctx, raw, true)
		if formErr == nil {
			c.ambUseForm = true
			return resp, nil
		}
	}
	return nil, err
}

func (c *Client) bayeuxPost(ctx context.Context, raw []byte, form bool) ([]map[string]interface{}, error) {
	var body io.Reader
	contentType := "application/json;charset=UTF-8"
	if form {
		values := url.Values{}
		values.Set("message", string(raw))
		body = strings.NewReader(values.Encode())
		contentType = "application/x-www-form-urlencoded;charset=UTF-8"
	} else {
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ambURL, body)
	if err != nil {
		return nil, err
	}
	c.setGatewayHeaders(req)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	respBody, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("AMB request failed (%d): %s", res.StatusCode, trimBody(respBody))
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(respBody, &arr); err != nil {
		var one map[string]interface{}
		if oneErr := json.Unmarshal(respBody, &one); oneErr == nil {
			arr = []map[string]interface{}{one}
		} else {
			return nil, fmt.Errorf("AMB JSON decode failed: %w; body=%s", err, trimBody(respBody))
		}
	}
	if c.debug {
		fmt.Fprintf(os.Stderr, "\n[amb] %s\n", redactDebugJSONBytes(respBody))
	}
	return arr, nil
}

func (c *Client) setGatewayHeaders(req *http.Request) {
	base := strings.TrimRight(c.cfg.InstanceURL, "/")
	req.Header.Set("User-Agent", "build-agent-go-cli")
	req.Header.Set("Origin", base)
	referer := base + "/now/build-agent"
	if c.opts.Nirvana {
		referer = base + "/sn_glider_app/ide.do"
	}
	req.Header.Set("Referer", referer)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if c.gatewayAuth == authModeBasic {
		req.SetBasicAuth(c.basicUser, c.basicPass)
	}
	nirvanaBearer := ""
	if c.opts.Nirvana {
		nirvanaBearer = c.nirvanaRESTAccessToken()
		if nirvanaBearer != "" && req.Header.Get("Authorization") == "" {
			req.Header.Set("Authorization", "Bearer "+nirvanaBearer)
		}
	}
	if c.sessionCookieHeader != "" && nirvanaBearer == "" {
		req.Header.Set("Cookie", c.sessionCookieHeader)
	}
	if c.userToken != "" && nirvanaBearer == "" {
		req.Header.Set("X-UserToken", c.userToken)
	}
}

func (c *Client) nirvanaRESTAccessToken() string {
	if token := strings.TrimSpace(c.oauthAccessToken); token != "" {
		return token
	}
	tok, ok := loadCachedToken(c.opts.Profile)
	if !ok || tokenExpired(tok) || strings.TrimSpace(tok.AccessToken) == "" {
		return ""
	}
	if tok.InstanceURL != "" && tok.InstanceURL != c.cfg.InstanceURL {
		return ""
	}
	return strings.TrimSpace(tok.AccessToken)
}

func setBrowserishHeadersForBase(header http.Header, instanceURL string) {
	base := strings.TrimRight(instanceURL, "/")
	header.Set("User-Agent", "Mozilla/5.0 build-agent-go-cli")
	header.Set("Origin", base)
	header.Set("Referer", base+"/now/build-agent")
	header.Set("X-Requested-With", "XMLHttpRequest")
}

func (c *Client) basicAuthorization() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.basicUser+":"+c.basicPass))
}

func (c *Client) addCodeAssistAuth(payload map[string]interface{}) {
	if c.userToken != "" {
		payload["usertoken"] = c.userToken
		return
	}
	if c.gatewayAuth == authModeBasic && c.basicUser != "" {
		payload["authorization"] = c.basicAuthorization()
	}
}

func websocketResponseSuffix(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if len(body) == 0 {
		return fmt.Sprintf(" (HTTP %d)", resp.StatusCode)
	}
	return fmt.Sprintf(" (HTTP %d: %s)", resp.StatusCode, trimBody(body))
}

func (c *Client) nextAmbID() string {
	c.ambMsgID++
	return fmt.Sprintf("%d", c.ambMsgID)
}

func collectGatewayStreamContent(content string) string {
	hasAssistantText := false
	var assistantParts []string
	for _, text := range extractTagContent(content, "text") {
		if strings.TrimSpace(text) != "" {
			assistantParts = append(assistantParts, text)
			hasAssistantText = true
		}
	}
	for _, text := range extractTagContent(content, "error_message") {
		if strings.TrimSpace(text) != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n", text)
		}
	}
	if hasAssistantText {
		return strings.Join(assistantParts, "")
	}
	if strings.Contains(content, "<thinking>") || strings.Contains(content, "<signature>") {
		return ""
	}
	cleaned := stripSimpleTags(content)
	if strings.TrimSpace(cleaned) != "" {
		return cleaned
	}
	return ""
}

func (c *Client) printGatewayStreamUpdate(chunk string) {
	if c.webStreamText == "" {
		return
	}
	if c.debug {
		// Debug mode prints raw websocket frames to stderr. In-place terminal
		// repainting makes combined debug logs look duplicated/garbled, so stream
		// plain chunks while debugging and keep the live renderer for normal use.
		fmt.Print(chunk)
		c.webStreamPlain = true
		return
	}
	if terminalLiveStreamEnabled() {
		// opencode-style streaming: commit only newly stable rendered rows to the
		// terminal scrollback. Avoid repainting the whole transcript on every chunk.
		if c.commitAssistantStreamRows(false) {
			return
		}
	}
	c.printAssistantStreamBullet()
	fmt.Print(chunk)
	c.webStreamPlain = true
}

func (c *Client) commitAssistantStreamRows(done bool) bool {
	if !interactiveTerminalUIEnabled() || !terminalLiveStreamEnabled() {
		return false
	}
	text := c.webStreamText
	rows := assistantStreamRenderedRows(text)
	if !done {
		if streamContainsMarkdownTable(text) {
			// Markdown tables are not append-stable while streaming: later rows can
			// change column widths and wrapping. Commit only the blank-line-delimited
			// stable prefix before/in front of the current table block; final completion
			// appends the fully rendered table once.
			rows = assistantStreamRenderedRows(stableAssistantStreamPrefix(text))
		} else if len(rows) > 0 {
			// Keep the last rendered row uncommitted while streaming; it is likely still
			// changing. This mirrors opencode's retained stream surface committing only
			// stable rows/blocks, but keeps the Go implementation deliberately small.
			rows = rows[:len(rows)-1]
		}
	}
	if len(rows) == 0 {
		return true
	}
	target := len(rows)
	if target <= c.webStreamRows {
		return true
	}
	if terminalAppendRowsToScrollback(rows[c.webStreamRows:target], c.statusBarState()) {
		c.webStreamRows = target
		return true
	}
	return false
}

func assistantStreamRenderedRows(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	width := terminalOutputWidth()
	formatted := strings.TrimRight(formatAssistantResponseTerminal(text, terminalANSIEnabled(), width), "\n")
	if formatted == "" {
		return nil
	}
	return wrapReplayRows(strings.Split(formatted, "\n"), width)
}

func streamContainsMarkdownTable(text string) bool {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.Contains(trimmed, "|") {
			continue
		}
		if strings.HasPrefix(trimmed, "|") || strings.HasSuffix(trimmed, "|") {
			return true
		}
		if i+1 < len(lines) && isMarkdownTableSeparator(lines[i+1]) {
			return true
		}
	}
	return false
}

func stableAssistantStreamPrefix(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lastBoundary := -1
	lineStart := 0
	for lineStart <= len(text) {
		lineEnd := strings.IndexByte(text[lineStart:], '\n')
		if lineEnd < 0 {
			break
		}
		lineEnd += lineStart
		if strings.TrimSpace(text[lineStart:lineEnd]) == "" {
			lastBoundary = lineEnd + 1
		}
		lineStart = lineEnd + 1
	}
	if lastBoundary <= 0 {
		return ""
	}
	return strings.TrimSpace(text[:lastBoundary])
}

func (c *Client) printAssistantStreamBullet() {
	if c.webStreamBulletStarted || !terminalANSIEnabled() {
		return
	}
	fmt.Println()
	fmt.Print(style("• ", ansiWasabiGreen, true))
	c.webStreamBulletStarted = true
}

func (c *Client) showTurnStatus() {
	if c.debug || !terminalStatusANSIEnabled() {
		return
	}
	c.statusMu.Lock()
	if c.turnStatusActive {
		c.statusMu.Unlock()
		return
	}
	c.turnStatusActive = true
	stop := make(chan struct{})
	done := make(chan struct{})
	c.turnStatusStop = stop
	c.turnStatusDone = done
	c.statusMu.Unlock()
	go c.animateTurnStatus(stop, done)
}

func (c *Client) clearTurnStatus() {
	c.statusMu.Lock()
	if !c.turnStatusActive {
		c.statusMu.Unlock()
		return
	}
	stop := c.turnStatusStop
	done := c.turnStatusDone
	c.turnStatusActive = false
	c.turnStatusStop = nil
	c.turnStatusDone = nil
	c.statusMu.Unlock()
	if stop != nil {
		close(stop)
	}
	if done != nil {
		<-done
	}
	c.statusMu.Lock()
	if terminalStatusANSIEnabled() {
		if interactiveTerminalUIEnabled() {
			clearTerminalFooterWorkingLine(c.statusBarState())
		} else {
			fmt.Fprint(os.Stderr, "\r\x1b[2K")
		}
	}
	c.statusMu.Unlock()
}

func (c *Client) animateTurnStatus(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(110 * time.Millisecond)
	defer ticker.Stop()
	frame := 0
	for {
		c.statusMu.Lock()
		if terminalStatusANSIEnabled() {
			if interactiveTerminalUIEnabled() {
				drawTerminalFooterWorkingLine(animatedWorkingStatus(frame), c.statusBarState())
			} else {
				fmt.Fprintf(os.Stderr, "\r\x1b[2K%s", animatedWorkingStatus(frame))
			}
		}
		c.statusMu.Unlock()
		frame++
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func (c *Client) finishGatewayStreamOutput() {
	// Keep the animated Working indicator alive while text streams; clear it only
	// when the server closes the turn and final output is committed.
	c.clearTurnStatus()
	assistantText := strings.TrimSpace(c.webStreamText)
	if assistantText == "" {
		return
	}
	if c.webStreamRows > 0 {
		c.commitAssistantStreamRows(true)
		terminalRecordAssistantText(assistantText)
		redrawPendingFooterPromptFromState()
		return
	}
	if c.webStreamPlain {
		if terminalRecordAssistantTextAndAppend(assistantText, c.statusBarState()) {
			return
		}
		if !strings.HasSuffix(c.webStreamText, "\n") {
			fmt.Println()
		}
		if terminalANSIEnabled() {
			fmt.Println()
		}
		redrawPendingFooterPromptFromState()
		return
	}
	printAssistantText(assistantText)
}

func extractTagContent(s, tag string) []string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	var out []string
	for {
		start := strings.Index(s, open)
		if start < 0 {
			return out
		}
		s = s[start+len(open):]
		end := strings.Index(s, close)
		if end < 0 {
			return out
		}
		out = append(out, s[:end])
		s = s[end+len(close):]
	}
}

func stripSimpleTags(s string) string {
	for _, tag := range []string{"text", "stop_reason", "error_message"} {
		s = strings.ReplaceAll(s, "<"+tag+">", "")
		s = strings.ReplaceAll(s, "</"+tag+">", "")
	}
	return s
}

func gatewayFinalError(data map[string]interface{}) error {
	features, _ := data["features"].(map[string]interface{})
	for _, feature := range features {
		featureMap, _ := feature.(map[string]interface{})
		result, _ := featureMap["result"].(map[string]interface{})
		if result == nil {
			continue
		}
		if errText := stringify(result["error"]); errText != "" {
			return errors.New(errText)
		}
	}
	return nil
}

func codeAssistUserMessage(id, timestamp, text string) interface{} {
	return map[string]interface{}{
		"id":        id,
		"timestamp": timestamp,
		"type":      "user",
		"status":    "complete",
		"body": map[string]interface{}{
			"author_id": "user",
			"text":      text,
		},
		"author": "user",
	}
}

func codeAssistMessages(history []interface{}) []interface{} {
	out := make([]interface{}, 0, len(history))
	for _, msg := range history {
		m := asMap(msg)
		if m == nil {
			continue
		}
		if asMap(m["body"]) == nil {
			continue
		}
		if stringify(m["author"]) == "" && stringify(m["type"]) != "user" {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func codeAssistChatHistory(messages []interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(messages))
	for _, msg := range messages {
		m := asMap(msg)
		if m == nil {
			continue
		}
		body := asMap(m["body"])
		if body == nil {
			continue
		}
		author := stringify(m["author"])
		if author == "" {
			author = stringify(body["author_id"])
		}
		if author == "" {
			author = stringify(m["type"])
		}
		content := firstString(body, "text", "message", "tool_result")
		out = append(out, map[string]interface{}{
			"id":        stringify(m["id"]),
			"timestamp": stringify(m["timestamp"]),
			"type":      author,
			"content":   content,
			"author":    author,
		})
	}
	return out
}

func lastCodeAssistSnapshot(messages []interface{}) interface{} {
	for i := len(messages) - 1; i >= 0; i-- {
		m := asMap(messages[i])
		if m == nil {
			continue
		}
		if snapshot, ok := m["snapshot"]; ok {
			return snapshot
		}
	}
	return nil
}

func lastCodeAssistMessageType(messages []interface{}) string {
	if len(messages) == 0 {
		return ""
	}
	m := asMap(messages[len(messages)-1])
	if m == nil {
		return ""
	}
	return stringify(m["type"])
}

func toolUseIDFromSnapshot(snapshot interface{}) string {
	m := asMap(snapshot)
	if m == nil {
		return ""
	}
	toolUse := asMap(m["tool_use"])
	if toolUse == nil {
		return ""
	}
	return firstString(toolUse, "tool_use_id", "toolUseId", "id")
}

func (c *Client) resetWebStream() {
	c.clearTurnStatus()
	c.webStreamID = ""
	c.webStreamType = ""
	c.webStreamText = ""
	c.webStreamTS = ""
	c.webStreamRows = 0
	c.webStreamPlain = false
	c.webStreamBulletStarted = false
	c.turnCompletedByAMB = false
	c.nirvanaThinkingText = ""
	c.nirvanaThinkingStarted = time.Time{}
	c.nirvanaThinkingDone = false
}

func (c *Client) appendGatewayRESTTurnHistory() {
	userText := strings.TrimSpace(c.pendingUserContent)
	assistantText := strings.TrimSpace(c.webStreamText)
	if userText != "" {
		c.history = append(c.history, map[string]interface{}{"role": "user", "content": userText})
	}
	if assistantText != "" {
		c.history = append(c.history, map[string]interface{}{"role": "assistant", "content": assistantText})
	}
	c.pendingUserContent = ""
	c.resetWebStream()
}

func (c *Client) appendWebStreamMessage() {
	if c.webStreamText == "" {
		return
	}
	messageType := c.webStreamType
	if messageType == "" {
		messageType = "text"
	}
	messageID := c.webStreamID
	if messageID == "" {
		messageID = uuidV4()
	}
	timestamp := c.webStreamTS
	if timestamp == "" {
		timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	c.history = append(codeAssistMessages(c.history), map[string]interface{}{
		"id":        messageID,
		"timestamp": timestamp,
		"type":      messageType,
		"status":    "complete",
		"body": map[string]interface{}{
			"author_id": "assistant",
			"text":      c.webStreamText,
		},
		"author": "assistant",
	})
	c.resetWebStream()
}

func (c *Client) appendCodeAssistAssistantMessage(message map[string]interface{}) {
	if _, ok := message["author"]; !ok {
		message["author"] = "assistant"
	}
	if stringify(message["status"]) == "" {
		message["status"] = "complete"
	}
	c.history = append(codeAssistMessages(c.history), message)
}

func (c *Client) attachSnapshotToLastMessage(snapshot interface{}) {
	if snapshot == nil || len(c.history) == 0 {
		return
	}
	messages := codeAssistMessages(c.history)
	if len(messages) == 0 {
		return
	}
	if last := asMap(messages[len(messages)-1]); last != nil {
		last["snapshot"] = snapshot
	}
	c.history = messages
}

func (c *Client) writeJSON(v interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.debug {
		fmt.Fprintf(os.Stderr, "\n>>> %s\n", redactDebugJSON(v))
	}
	return c.conn.WriteJSON(v)
}

func (c *Client) readGatewayWebSocketLoop() {
	defer func() {
		select {
		case <-c.closed:
		default:
			close(c.closed)
		}
	}()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if c.processing && c.turnDone != nil {
				c.processing = false
				select {
				case c.turnDone <- err:
				default:
				}
			}
			return
		}
		if c.debug {
			fmt.Fprintf(os.Stderr, "\n<<< %s\n", redactDebugJSONBytes(data))
		}
		if err := c.handleGatewayWebSocketEvent(data); err != nil {
			fmt.Fprintf(os.Stderr, "event handling error: %v\n", err)
		}
	}
}

func (c *Client) handleGatewayWebSocketEvent(data []byte) error {
	var event map[string]interface{}
	if err := json.Unmarshal(data, &event); err != nil {
		return err
	}
	status := stringify(event["status"])
	typeName := stringify(event["type"])
	body := asMap(event["body"])
	if body == nil {
		body = map[string]interface{}{}
	}

	if status == "error" {
		err := codeAssistError(event, "websocket message error")
		fmt.Fprintf(os.Stderr, "\n%s\n", err)
		c.processing = false
		if c.turnDone != nil {
			select {
			case c.turnDone <- err:
			default:
			}
		}
		return nil
	}

	switch status {
	case "streaming":
		text := firstString(body, "text", "message")
		if text == "" {
			return nil
		}
		if typeName != "thinking" {
			fmt.Print(text)
		}
		if c.webStreamID == "" {
			c.webStreamID = stringify(event["id"])
			c.webStreamTS = stringify(event["timestamp"])
			c.webStreamType = typeName
		}
		if c.webStreamType == "" || c.webStreamType == typeName {
			c.webStreamType = typeName
			c.webStreamText += text
		}
		return nil
	case "complete":
		return c.handleGatewayCompleteMessage(event, typeName, body)
	default:
		if typeName == "end_turn" {
			return c.handleGatewayCompleteMessage(event, typeName, body)
		}
		if c.debug {
			fmt.Fprintf(os.Stderr, "\n[unhandled websocket message] status=%s type=%s\n", status, typeName)
		}
	}
	return nil
}

func (c *Client) handleGatewayCompleteMessage(event map[string]interface{}, typeName string, body map[string]interface{}) error {
	switch typeName {
	case "end_turn":
		c.appendWebStreamMessage()
		c.attachSnapshotToLastMessage(event["snapshot"])
		if stats := asMap(body["stats"]); stats != nil {
			c.recordUsage(stats)
		}
		c.clearTurnStatus()
		if err := c.saveCurrentState(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save workspace %q: %v\n", c.workspaceName, err)
		}
		c.processing = false
		if c.turnDone != nil {
			select {
			case c.turnDone <- nil:
			default:
			}
		}
	case "tool_use":
		c.appendWebStreamMessage()
		c.appendCodeAssistAssistantMessage(event)
		name, inputs := codeAssistToolUse(body)
		if name != "" && c.debug {
			fmt.Fprintf(os.Stderr, "\n[client tool] %s\n", name)
		}
		if name == "interview" {
			c.clearTurnStatus()
			printInterviewInputs(inputs)
			if err := c.saveCurrentState(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not save workspace %q: %v\n", c.workspaceName, err)
			}
			c.processing = false
			if c.turnDone != nil {
				select {
				case c.turnDone <- nil:
				default:
				}
			}
			return nil
		}
		err := fmt.Errorf("server requested unsupported client tool %q", name)
		c.processing = false
		if c.turnDone != nil {
			select {
			case c.turnDone <- err:
			default:
			}
		}
	case "error":
		err := codeAssistError(event, "websocket message error")
		c.clearTurnStatus()
		fmt.Fprintf(os.Stderr, "\n%s\n", err)
		c.processing = false
		if c.turnDone != nil {
			select {
			case c.turnDone <- err:
			default:
			}
		}
	default:
		text := firstString(body, "text", "message")
		if c.webStreamText != "" && (typeName == c.webStreamType || typeName == "") {
			c.appendWebStreamMessage()
			return nil
		}
		if text != "" {
			fmt.Print(text)
		}
		if typeName != "" || text != "" {
			c.appendCodeAssistAssistantMessage(event)
		}
	}
	return nil
}

func codeAssistToolUse(body map[string]interface{}) (string, map[string]interface{}) {
	toolUse := asMap(body["tool_use"])
	if toolUse == nil {
		return "", nil
	}
	inputs := asMap(toolUse["inputs"])
	if inputs == nil {
		inputs = asMap(toolUse["input"])
	}
	return stringify(toolUse["name"]), inputs
}

func (c *Client) resetTurnToolTracking() {
	if c.toolCallNames == nil {
		c.toolCallNames = map[string]string{}
		return
	}
	for key := range c.toolCallNames {
		delete(c.toolCallNames, key)
	}
}

func (c *Client) recordToolCall(event map[string]interface{}) (string, string) {
	name := eventToolName(event)
	callID := eventToolCallID(event)
	if name == "" {
		name = "tool"
	}
	if callID != "" {
		if c.toolCallNames == nil {
			c.toolCallNames = map[string]string{}
		}
		c.toolCallNames[callID] = name
	}
	return name, callID
}

func (c *Client) recordToolResult(event map[string]interface{}) (string, string, bool) {
	callID := eventToolCallID(event)
	name := eventToolName(event)
	if name == "" && callID != "" && c.toolCallNames != nil {
		name = c.toolCallNames[callID]
	}
	if name == "" {
		name = "tool"
	}
	if callID != "" && c.toolCallNames != nil {
		delete(c.toolCallNames, callID)
	}
	return name, callID, eventToolSuccess(event)
}

func (c *Client) printToolResultStatus(name string, success bool) {
	if terminalRecordToolResultAndAppend(name, success, c.statusBarState()) {
		return
	}
	fmt.Fprintf(os.Stderr, "\n%s\n", formatToolResultTerminal(name, success, terminalStatusANSIEnabled()))
}

func eventToolName(event map[string]interface{}) string {
	maps := eventCandidateMaps(event)
	name := ""
	server := ""
	for _, m := range maps {
		if name == "" {
			name = firstString(m, "name", "tool_name", "toolName", "displayName", "tool")
		}
		if server == "" {
			server = firstString(m, "server", "server_name", "serverName", "mcp_server", "mcpServer", "mcpServerName")
		}
	}
	if name != "" && server != "" && !strings.Contains(name, server) {
		return server + "/" + name
	}
	return name
}

func eventToolCallID(event map[string]interface{}) string {
	for _, m := range eventCandidateMaps(event) {
		if id := firstString(m, "call_id", "callId", "tool_call_id", "toolCallId", "tool_use_id", "toolUseId", "id"); id != "" {
			return id
		}
	}
	return ""
}

func eventToolSuccess(event map[string]interface{}) bool {
	for _, m := range eventCandidateMaps(event) {
		for _, key := range []string{"success", "successful", "ok"} {
			if v, ok := m[key].(bool); ok {
				return v
			}
		}
		if failed, ok := m["failed"].(bool); ok {
			return !failed
		}
		if isError, ok := m["is_error"].(bool); ok {
			return !isError
		}
		status := strings.ToLower(strings.TrimSpace(firstString(m, "status", "state")))
		switch status {
		case "success", "successful", "succeeded", "ok", "complete", "completed":
			return true
		case "error", "failed", "failure", "unsuccessful":
			return false
		}
		if firstString(m, "error", "errorMessage", "error_message") != "" {
			return false
		}
	}
	return true
}

func eventCandidateMaps(event map[string]interface{}) []map[string]interface{} {
	maps := []map[string]interface{}{event}
	for _, key := range []string{"tool", "tool_call", "toolCall", "tool_result", "toolResult", "body", "result", "data", "payload"} {
		if m := asMap(event[key]); m != nil {
			maps = append(maps, m)
		}
	}
	return maps
}

func printInterviewInputs(inputs map[string]interface{}) {
	if inputs == nil {
		return
	}
	if question := firstString(inputs, "question", "message"); question != "" {
		fmt.Fprintf(os.Stderr, "%s\n", question)
	}
	printChoices(inputs)
	fmt.Fprintln(os.Stderr, "Reply with your answer to continue this turn.")
}

func codeAssistError(event map[string]interface{}, fallback string) error {
	body := asMap(event["body"])
	if body != nil {
		for _, key := range []string{"suggestion", "message", "text", "error"} {
			if value := stringify(body[key]); value != "" {
				return errors.New(value)
			}
		}
	}
	if value := stringify(event["error"]); value != "" {
		return errors.New(value)
	}
	return errors.New(fallback)
}

func (c *Client) readLoop() {
	defer func() {
		select {
		case <-c.closed:
		default:
			close(c.closed)
		}
	}()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			select {
			case c.connected <- err:
			default:
			}
			if c.processing && c.turnDone != nil {
				select {
				case c.turnDone <- err:
				default:
				}
			}
			return
		}
		if c.debug {
			fmt.Fprintf(os.Stderr, "\n<<< %s\n", redactDebugJSONBytes(data))
		}
		if err := c.handleEvent(data); err != nil {
			fmt.Fprintf(os.Stderr, "event handling error: %v\n", err)
		}
	}
}

func (c *Client) handleEvent(data []byte) error {
	var event map[string]interface{}
	if err := json.Unmarshal(data, &event); err != nil {
		return err
	}
	typeName, _ := event["type"].(string)
	switch typeName {
	case "connected":
		if !c.suppressInteractiveStartupScrollback() {
			fmt.Fprintf(os.Stderr, "connected to %s\n", c.cfg.InstanceURL)
		}
		select {
		case c.connected <- nil:
		default:
		}
	case "turn_start":
		c.resetTurnToolTracking()
		c.showTurnStatus()
	case "turn_resumed":
		c.showTurnStatus()
	case "stream_start":
		streamID, _ := event["stream_id"].(string)
		contentType, _ := event["content_type"].(string)
		if streamID != "" {
			c.streamTypes[streamID] = contentType
		}
		c.handleNirvanaStreamStart(streamID, contentType, event)
	case "stream_delta":
		c.handleNirvanaStreamDelta(event)
	case "stream_end":
		streamID, _ := event["stream_id"].(string)
		contentType := c.streamTypes[streamID]
		c.handleNirvanaStreamEnd(streamID, contentType)
		delete(c.streamTypes, streamID)
	case "tool_call":
		name, callID := c.recordToolCall(event)
		if c.debug {
			fmt.Fprintf(os.Stderr, "\n[tool call] %s %s\n", name, callID)
		}
	case "tool_result":
		name, callID, success := c.recordToolResult(event)
		if c.debug {
			fmt.Fprintf(os.Stderr, "\n[tool result] %s success=%v\n", callID, success)
		}
		c.printToolResultStatus(name, success)
	case "client_elicitation":
		return c.handleElicitation(event)
	case "turn_summary":
		if summary, ok := event["summary"].(string); ok && summary != "" {
			fmt.Fprintf(os.Stderr, "\n[summary] %s\n", summary)
		}
	case "sub_agent_start":
		name, _ := event["name"].(string)
		fmt.Fprintf(os.Stderr, "\n[sub-agent started] %s\n", name)
	case "sub_agent_end":
		name, _ := event["name"].(string)
		fmt.Fprintf(os.Stderr, "\n[sub-agent ended] %s\n", name)
	case "turn_end":
		c.finishGatewayStreamOutput()
		c.clearTurnStatus()
		assistantText := strings.TrimSpace(c.webStreamText)
		if assistantText != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			if err := c.persistWebAssistantMessage(ctx, assistantText); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not persist assistant message: %v\n", err)
			}
			cancel()
		}
		if snapshot, ok := event["snapshot"].([]interface{}); ok {
			c.history = snapshot
			c.pendingUserContent = ""
			c.resetWebStream()
		} else {
			c.appendGatewayRESTTurnHistory()
		}
		if v, ok := event["appScope"]; ok {
			c.absorbAppScope(v)
		}
		if v, ok := event["workingSet"]; ok {
			c.workingSet = v
		}
		if usage, ok := event["usage"].(map[string]interface{}); ok {
			c.recordUsage(usage)
		}
		if err := c.saveCurrentState(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save workspace %q: %v\n", c.workspaceName, err)
		}
		c.processing = false
		if c.turnDone != nil {
			select {
			case c.turnDone <- nil:
			default:
			}
		}
	case "turn_error":
		err := eventError(event, "turn error")
		c.clearTurnStatus()
		fmt.Fprintf(os.Stderr, "\n%s\n", err)
		c.processing = false
		if c.turnDone != nil {
			select {
			case c.turnDone <- err:
			default:
			}
		}
	case "error":
		err := eventError(event, "server error")
		c.clearTurnStatus()
		fmt.Fprintf(os.Stderr, "\n%s\n", err)
		select {
		case c.connected <- err:
		default:
		}
		if c.processing && c.turnDone != nil {
			select {
			case c.turnDone <- err:
			default:
			}
		}
	default:
		if typeName == "pong" || typeName == "thinking_signature" {
			return nil
		}
		if typeName != "" {
			fmt.Fprintf(os.Stderr, "\n[unhandled event] %s\n", typeName)
		}
	}
	return nil
}

func (c *Client) handleNirvanaStreamStart(streamID, contentType string, event map[string]interface{}) {
	switch contentType {
	case "text":
		c.webStreamID = streamID
		c.webStreamType = "text"
		c.webStreamText = ""
		c.webStreamRows = 0
		c.webStreamPlain = false
		c.webStreamTS = stringify(event["timestamp"])
	case "thinking":
		c.nirvanaThinkingText = ""
		c.nirvanaThinkingStarted = time.Now()
		c.nirvanaThinkingDone = false
	}
}

func (c *Client) handleNirvanaStreamDelta(event map[string]interface{}) {
	delta, _ := event["delta"].(string)
	if delta == "" {
		return
	}
	streamID, _ := event["stream_id"].(string)
	contentType := c.streamTypes[streamID]
	switch contentType {
	case "text":
		c.webStreamText += delta
		c.printGatewayStreamUpdate(delta)
	case "thinking":
		c.nirvanaThinkingText += delta
	case "pending":
		// Keep pending status off stdout so scripted output remains the assistant text only.
		if c.debug {
			fmt.Fprintf(os.Stderr, "\n[pending] %s\n", strings.TrimSpace(delta))
		}
	default:
		// Unknown stream types are rare; preserve visibility without breaking stdout.
		if c.debug {
			fmt.Fprintf(os.Stderr, "\n[%s] %s\n", contentType, strings.TrimSpace(delta))
		}
	}
}

func (c *Client) handleNirvanaStreamEnd(_ string, contentType string) {
	switch contentType {
	case "thinking":
		text := strings.TrimSpace(c.nirvanaThinkingText)
		if text == "" || c.nirvanaThinkingDone {
			return
		}
		duration := time.Since(c.nirvanaThinkingStarted)
		if c.nirvanaThinkingStarted.IsZero() {
			duration = 0
		}
		if c.debug {
			fmt.Fprintf(os.Stderr, "Finished thinking after %.1f seconds\n", duration.Seconds())
		}
		c.nirvanaThinkingDone = true
	}
}

func (c *Client) buildInvokePayload(ctx context.Context, silentAuth bool) (map[string]interface{}, map[string]interface{}, error) {
	c.tryLoadNirvanaWebAgentConfig(ctx)
	tok, err := getAccessToken(ctx, oauthConfig(c.cfg), c.opts.Profile, c.cfg.InstanceURL, c.opts.NoOpen, silentAuth)
	if err != nil {
		return nil, nil, err
	}
	c.oauthAccessToken = tok.AccessToken
	claims := decodeJWTPayload(tok.AccessToken)
	instanceID := "cli-instance"
	if v, _ := claims["instance_id"].(string); v != "" {
		instanceID = v
	}
	userID := "cli-user"
	if v, _ := claims["sub"].(string); v != "" {
		userID = v
	}
	providerURL := c.cfg.LLMProxyURL
	capabilityID := c.cfg.CapabilityID
	largeModel := c.runtime.LargeModel
	if c.webAgentConfig.ProviderURL != "" {
		providerURL = c.webAgentConfig.ProviderURL
	}
	if c.webAgentConfig.SkillID != "" {
		capabilityID = c.webAgentConfig.SkillID
	}
	if c.webAgentConfig.Model != "" && c.opts.Model == "" && c.opts.Provider == "" {
		largeModel = c.webAgentConfig.Model
	}
	glideAttributes := map[string]interface{}{
		"instanceUrl":       strings.TrimRight(c.cfg.InstanceURL, "/") + "/",
		"glideVersion":      "australia",
		"instanceName":      instanceNameFromURL(c.cfg.InstanceURL),
		"instanceId":        instanceID,
		"userId":            userID,
		"ideVersion":        "4.2.0-alpha.6",
		"buildAgentVersion": "2.3.3",
	}
	for k, v := range c.webAgentConfig.GlideAttributes {
		glideAttributes[k] = v
	}
	// The web client sends these extension version fields even when the instance
	// providerConfig does not include them.
	glideAttributes["ideVersion"] = "4.2.0-alpha.6"
	glideAttributes["buildAgentVersion"] = "2.3.3"
	if instanceURL := stringify(glideAttributes["instanceUrl"]); instanceURL == "" {
		glideAttributes["instanceUrl"] = strings.TrimRight(c.cfg.InstanceURL, "/") + "/"
	}

	invokeOptions := map[string]interface{}{
		"applicationName": "Build Agent",
		"stream":          true,
		"modelConfig": map[string]interface{}{
			"provider": c.runtime.Provider,
			"largeConfig": map[string]interface{}{
				"temperature":     c.runtime.LargeTemperature,
				"maxOutputTokens": c.runtime.LargeMaxOutputTokens,
				"model":           largeModel,
				"thinkingTokens":  c.runtime.LargeThinkingTokens,
			},
			"smallConfig": map[string]interface{}{
				"temperature":     c.runtime.SmallTemperature,
				"maxOutputTokens": c.runtime.SmallMaxOutputTokens,
				"model":           c.runtime.SmallModel,
				"thinkingTokens":  c.runtime.SmallThinkingTokens,
			},
		},
		"glideAttributes": glideAttributes,
		"attributes": map[string]interface{}{
			"baseUrl":                          providerURL,
			"api_version":                      "v1",
			"azure_content_safety_api_version": "2024-09-01",
			"capabilityId":                     capabilityID,
			"chat_completions_api_version":     "2024-10-21",
			"audio_transcriptions_api_version": "2024-10-21",
			"conversationId":                   c.conversationID,
			"transactionId":                    uuidV4(),
			"turnId":                           uuidV4(),
			"solutionId":                       capabilityID,
			"solutionName":                     c.runtime.SolutionName,
			"solutionCapability":               "Build Agent",
			"x_allow_routing":                  "regional",
			"skillName":                        "Build Agent",
			"responses_api_version":            "preview",
		},
		"genAIConfig": map[string]interface{}{
			"capabilityId": capabilityID,
			"modelDefinitionIdMap": map[string]string{
				largeModel:           c.runtime.LargeDefinitionID,
				c.runtime.SmallModel: c.runtime.SmallDefinitionID,
			},
		},
		"useMockLlm": false,
	}
	params := map[string]interface{}{
		"arguments": map[string]interface{}{
			"action": "BuildAgent",
			"token": map[string]interface{}{
				"access_token": tok.AccessToken,
				"expires_in":   tok.ExpiresIn,
				"token_type":   "OAUTH",
				"issued_at":    tok.IssuedAt,
			},
		},
	}
	return invokeOptions, params, nil
}

func (c *Client) clientCapabilities() map[string]interface{} {
	caps := map[string]interface{}{
		"change_log":              map[string]interface{}{},
		"interview_choice_picker": map[string]interface{}{},
		"plan_approval":           map[string]interface{}{},
		"server_tools":            map[string]interface{}{},
		"working_set":             map[string]interface{}{},
		"app_picker":              map[string]interface{}{},
		"streaming":               map[string]interface{}{"receive": true},
		"sub_agents":              map[string]interface{}{},
		"product_availability":    map[string]interface{}{},
		"atf_with_app":            map[string]interface{}{},
	}
	if c.opts.Nirvana {
		// Match the Glider Build Agent web client closely enough for the
		// websocket runtime to send the same streaming event family observed in
		// Chrome HARs. The corresponding client-side actions are handled with
		// safe stubs below; the CLI does not execute arbitrary local filesystem
		// tools.
		caps["client_ide"] = map[string]interface{}{}
		caps["elicitation"] = map[string]interface{}{}
		caps["fluent_docs"] = map[string]interface{}{}
		caps["glob_and_grep"] = map[string]interface{}{}
		caps["keyword_search"] = map[string]interface{}{"preview_available": true}
		caps["tools"] = map[string]interface{}{"execute": true}
		return caps
	}
	// Deliberately do not advertise tools.execute by default. This first Go CLI
	// is meant to use instance-side/server tools (including instance-defined MCP),
	// not execute local filesystem/build tools like the TypeScript CLI.
	if c.opts.AdvertiseLocalTools {
		caps["tools"] = map[string]interface{}{"execute": true}
		caps["memfs"] = map[string]interface{}{}
	}
	return caps
}

func (c *Client) handleElicitation(event map[string]interface{}) error {
	elicitationID, _ := event["elicitation_id"].(string)
	action, _ := event["action"].(string)
	payload, _ := event["payload"].(map[string]interface{})
	if payload == nil {
		payload = map[string]interface{}{}
	}

	if c.debug {
		fmt.Fprintf(os.Stderr, "\n[client request] %s\n", action)
	}
	var result map[string]interface{}
	var status string
	var err error

	switch action {
	case "interview", "interview_choice_picker":
		result, status, err = c.answerInterview(payload)
	case "approval", "plan_approval":
		result, status, err = c.answerApproval(action, payload)
	case "app_picker":
		result, status, err = c.answerAppPicker(payload)
	case "set_app_scope":
		result, status, err = c.answerSetAppScope(payload)
	case "is_product_available":
		result, status = map[string]interface{}{"available": true, "message": "Product is available."}, "complete"
	case "fs_read_directory":
		result, status = c.answerFSReadDirectory(payload)
	case "instance_skills_list":
		result, status = map[string]interface{}{"content": "[]"}, "complete"
	case "fluent_topics_list":
		result, status = map[string]interface{}{
			"error": "No active fluent project scope. Catalog will be available once an app is selected or created.",
			"code":  "NO_FLUENT_PROJECT",
		}, "error"
	default:
		result, status, err = c.answerUnknownElicitation(action, payload)
	}
	if err != nil {
		result = map[string]interface{}{"error": err.Error(), "code": "CLI_INPUT_ERROR"}
		status = "error"
	}
	return c.sendElicitationResponse(elicitationID, status, result)
}

func (c *Client) answerFSReadDirectory(payload map[string]interface{}) (map[string]interface{}, string) {
	msg := "Tool fs_read_directory requires an application to have been created - run `create_new_servicenow_app` before reattempting."
	return map[string]interface{}{
		"content":    msg,
		"error":      msg,
		"code":       "TOOL_ERROR",
		"ideContext": emptyIDEContext(),
	}, "error"
}

func (c *Client) answerInterview(payload map[string]interface{}) (map[string]interface{}, string, error) {
	c.clearTurnStatus()
	question := stringify(payload["question"])
	if question == "" {
		question = stringify(payload["message"])
	}
	if question != "" {
		fmt.Fprintf(os.Stderr, "%s\n", question)
	}
	printChoices(payload)
	if c.opts.AutoApprove {
		return map[string]interface{}{"answer": "Please proceed with the most reasonable default option."}, "complete", nil
	}
	answer, err := promptLine("Answer: ")
	return map[string]interface{}{"answer": answer}, "complete", err
}

func (c *Client) answerApproval(action string, payload map[string]interface{}) (map[string]interface{}, string, error) {
	c.clearTurnStatus()
	if plan, ok := payload["plan"]; ok {
		if raw, err := json.MarshalIndent(plan, "", "  "); err == nil {
			fmt.Fprintf(os.Stderr, "%s\n", raw)
		}
	}
	message := stringify(payload["message"])
	if message == "" && action == "plan_approval" {
		message = "Approve this plan to begin applying changes?"
	}
	if message != "" {
		fmt.Fprintf(os.Stderr, "%s\n", message)
	}
	approved := c.opts.AutoApprove
	var err error
	if !c.opts.AutoApprove {
		approved, err = promptYesNo("Approve", true)
	}
	return map[string]interface{}{"approved": approved}, "complete", err
}

func (c *Client) answerAppPicker(payload map[string]interface{}) (map[string]interface{}, string, error) {
	c.clearTurnStatus()
	choices, _ := payload["choices"].([]interface{})
	if len(choices) == 0 {
		return map[string]interface{}{"error": "No applications available to select.", "code": "NO_CHOICES"}, "error", nil
	}
	for i, choice := range choices {
		fmt.Fprintf(os.Stderr, "%d) %s\n", i+1, choiceLabel(choice))
	}
	selected := choiceLabel(choices[0])
	if !c.opts.AutoApprove {
		answer, err := promptLine("Select application number: ")
		if err != nil {
			return nil, "error", err
		}
		if answer != "" {
			var idx int
			if _, err := fmt.Sscanf(answer, "%d", &idx); err == nil && idx >= 1 && idx <= len(choices) {
				selected = choiceLabel(choices[idx-1])
			} else {
				selected = answer
			}
		}
	}
	return map[string]interface{}{"success": true, "selection": selected}, "complete", nil
}

func (c *Client) answerSetAppScope(payload map[string]interface{}) (map[string]interface{}, string, error) {
	c.clearTurnStatus()
	app := appFromPayload(payload)
	if app == nil {
		return map[string]interface{}{"error": "set_app_scope did not include scopeId", "code": "NO_SCOPE_ID"}, "error", nil
	}
	if err := c.SetApp(*app); err != nil {
		return nil, "error", err
	}
	fmt.Fprintf(os.Stderr, "scope set: %s\n", app.ScopeID)
	return map[string]interface{}{"success": true}, "complete", nil
}

func (c *Client) answerUnknownElicitation(action string, payload map[string]interface{}) (map[string]interface{}, string, error) {
	if c.opts.AdvertiseLocalTools {
		c.clearTurnStatus()
		if raw, err := json.MarshalIndent(payload, "", "  "); err == nil {
			fmt.Fprintf(os.Stderr, "%s\n", raw)
		}
		return map[string]interface{}{
			"error": fmt.Sprintf("Go CLI does not implement local action %q yet", action),
			"code":  "NOT_IMPLEMENTED",
		}, "error", nil
	}
	return map[string]interface{}{
		"error": fmt.Sprintf("unexpected client-side action %q; this CLI only advertises server-side tools by default", action),
		"code":  "UNEXPECTED_CLIENT_ACTION",
	}, "error", nil
}

func (c *Client) sendElicitationResponse(elicitationID string, status string, result map[string]interface{}) error {
	if elicitationID == "" {
		return errors.New("missing elicitation_id")
	}
	responseResult := map[string]interface{}{
		"success": status == "complete",
	}
	for k, v := range result {
		responseResult[k] = v
	}
	if status == "error" {
		message := stringify(result["error"])
		if message == "" {
			message = stringify(result["message"])
		}
		if message == "" {
			message = "Unknown error"
		}
		code := stringify(result["code"])
		if code == "" {
			code = "UNKNOWN_ERROR"
		}
		responseResult["error"] = map[string]interface{}{"code": code, "message": message}
		delete(responseResult, "code")
	}
	payload := map[string]interface{}{
		"type":            "response",
		"conversation_id": c.conversationID,
		"elicitation_id":  elicitationID,
		"result":          responseResult,
	}
	return c.writeJSON(payload)
}

func (c *Client) recordUsage(usage map[string]interface{}) {
	if len(usage) == 0 {
		return
	}
	c.usageInputTokens += usageTokenValue(usage, "input_tokens", "inputTokens", "prompt_tokens", "promptTokens")
	c.usageOutputTokens += usageTokenValue(usage, "output_tokens", "outputTokens", "completion_tokens", "completionTokens")
	c.usageThinkingTokens += usageTokenValue(usage, "thinking_tokens", "thinkingTokens")
	if c.debug || !interactiveTerminalUIEnabled() {
		printUsage(usage)
	} else {
		c.drawPersistentStatus()
	}
}

func (c *Client) resetUsageTotals() {
	c.usageInputTokens = 0
	c.usageOutputTokens = 0
	c.usageThinkingTokens = 0
}

func (c *Client) printStatusBar() {
	if !interactiveTerminalUIEnabled() {
		return
	}
	c.drawPersistentStatus()

}

func (c *Client) printLiveUserTurn(content string) {
	if c.debug || len(c.opts.Prompts) > 0 || !interactiveTerminalUIEnabled() || !terminalANSIEnabled() {
		return
	}
	printLiveUserPrompt(content)
	// `printLiveUserPrompt` writes the submitted turn into the scrollback region.
	// Put the terminal cursor back in the input footer immediately afterwards so
	// any typeahead while `Working ...` is animating echoes in the prompt bar, not
	// in the transcript/working area.
	placeTerminalFooterPromptCursor("ba> ", 0)
}

func (c *Client) drawPersistentStatus() {
	if !interactiveTerminalUIEnabled() {
		return
	}
	c.statusMu.Lock()
	drawTerminalFooterStatus(c.statusBarState())
	c.statusMu.Unlock()
}

func (c *Client) statusBarState() statusBarState {
	model := c.runtime.LargeModel
	if c.webAgentConfig.Model != "" && c.opts.Model == "" && c.opts.Provider == "" {
		model = c.webAgentConfig.Model
	}
	if model == "" {
		model = "unknown"
	}
	return statusBarState{
		Model:         model,
		InputMessages: conversationInputMessageCount(c.history),
		InputTokens:   c.usageInputTokens,
		OutputTokens:  c.usageOutputTokens,
		Instance:      c.cfg.InstanceURL,
	}
}

func conversationInputMessageCount(history []interface{}) int {
	count := 0
	for _, msg := range collectTerminalHistoryMessages(history) {
		if msg.Role == "user" {
			count++
		}
	}
	return count
}

func usageTokenValue(usage map[string]interface{}, keys ...string) int64 {
	for _, key := range keys {
		if value, ok := usage[key]; ok {
			if n := tokenValueInt64(value); n > 0 {
				return n
			}
		}
	}
	return 0
}

func tokenValueInt64(value interface{}) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int8:
		return int64(v)
	case int16:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return v
	case uint:
		return int64(v)
	case uint8:
		return int64(v)
	case uint16:
		return int64(v)
	case uint32:
		return int64(v)
	case uint64:
		if v > uint64(^uint64(0)>>1) {
			return 0
		}
		return int64(v)
	case float32:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
		if f, err := v.Float64(); err == nil {
			return int64(f)
		}
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return 0
		}
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return int64(f)
		}
	}
	return 0
}

func printUsage(usage map[string]interface{}) {
	parts := []string{}
	for _, key := range []string{"input_tokens", "output_tokens", "thinking_tokens"} {
		if v, ok := usage[key]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", key, v))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(os.Stderr, "usage: %s\n", strings.Join(parts, " "))
	}
}

func eventError(event map[string]interface{}, fallback string) error {
	if errObj, ok := event["error"].(map[string]interface{}); ok {
		message := stringify(errObj["message"])
		code := stringify(errObj["code"])
		if message != "" && code != "" {
			return fmt.Errorf("%s: %s", code, message)
		}
		if message != "" {
			return errors.New(message)
		}
	}
	return errors.New(fallback)
}

func stringify(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func printChoices(payload map[string]interface{}) {
	choices, ok := payload["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return
	}
	for i, choice := range choices {
		fmt.Fprintf(os.Stderr, "%d) %s\n", i+1, choiceLabel(choice))
	}
}

func choiceLabel(choice interface{}) string {
	if m, ok := choice.(map[string]interface{}); ok {
		for _, key := range []string{"label", "name", "displayName", "value"} {
			if v := stringify(m[key]); v != "" {
				return v
			}
		}
	}
	return stringify(choice)
}
