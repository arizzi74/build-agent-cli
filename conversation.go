package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

var errWebConversationNotFound = errors.New("web conversation not found")

type WebConversation struct {
	ID              string
	Title           string
	State           string
	Summary         string
	CreatedBy       string
	UpdatedAt       string
	ApplicationID   string
	ApplicationName string
}

type conversationTableSpec struct {
	ConversationTable string
	MessageTable      string
	CoreStyle         bool
}

type conversationAPISpec struct {
	Base          string
	BuildAgentAPI bool
}

type conversationListCandidate struct {
	spec           conversationAPISpec
	suffix         string
	applicationIDs []string
}

func (c *Client) shouldPromptStartupConversation() bool {
	if c.opts.CodeAssistWS || len(c.opts.Prompts) > 0 || c.opts.Conversation != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

func (c *Client) ensureConversationHTTPClient() error {
	if c.httpClient == nil {
		if err := c.initGatewayHTTPClient(); err != nil {
			return err
		}
	}
	if c.sessionCookieHeader == "" && c.gatewayAuth != authModeBasic {
		if session, ok := loadMatchingWebSession(c.opts.Profile, c.cfg.InstanceURL); ok {
			c.applyWebSession(session)
		}
	}
	return nil
}

func (c *Client) conversationAPISpecs() []conversationAPISpec {
	base := strings.TrimRight(c.cfg.InstanceURL, "/")
	return []conversationAPISpec{
		{Base: base + "/api/sn_ba_core/conversations_api"},
		{Base: base + "/api/sn_build_agent/conversations_api"},
		{Base: base + "/api/sn_build_agent/build_agent_api", BuildAgentAPI: true},
	}
}

func (spec conversationAPISpec) URL(suffix string) string {
	if spec.BuildAgentAPI {
		suffix = buildAgentAPIConversationSuffix(suffix)
	}
	return spec.Base + suffix
}

func (spec conversationAPISpec) Payload(suffix string, payload interface{}) interface{} {
	if !spec.BuildAgentAPI {
		return payload
	}
	return buildAgentAPIConversationPayload(suffix, payload)
}

func (c *Client) conversationListCandidates(applicationIDs []string) []conversationListCandidate {
	specs := c.conversationAPISpecs()
	candidates := make([]conversationListCandidate, 0, len(specs)+1)
	for _, spec := range specs {
		if spec.BuildAgentAPI {
			candidates = append(candidates, conversationListCandidate{spec: spec, suffix: buildAgentConversationListSuffix(applicationIDs), applicationIDs: applicationIDs})
		}
	}
	for _, spec := range specs {
		if !spec.BuildAgentAPI {
			candidates = append(candidates, conversationListCandidate{spec: spec, suffix: "/conversations", applicationIDs: applicationIDs})
		}
	}
	return candidates
}

func buildAgentAPIConversationSuffix(suffix string) string {
	switch suffix {
	case "/create":
		return "/conversations"
	case "/conversations":
		return suffix
	}
	if strings.HasPrefix(suffix, "/conversation/") {
		rest := strings.TrimPrefix(suffix, "/conversation/")
		if strings.HasSuffix(rest, "/message") {
			rest = strings.TrimSuffix(rest, "/message") + "/messages"
		}
		return "/conversations/" + rest
	}
	return suffix
}

func buildAgentAPIConversationPayload(suffix string, payload interface{}) interface{} {
	if outer := asMap(payload); outer != nil {
		if inner := asMap(outer["payload"]); inner != nil {
			switch {
			case suffix == "/create":
				out := map[string]interface{}{
					"title":           conversationCreateTitle(firstString(inner, "title")),
					"applicationId":   nil,
					"applicationName": "",
					"client":          "ide",
				}
				if v, ok := inner["applicationId"]; ok {
					out["applicationId"] = v
				}
				if v, ok := inner["applicationName"]; ok {
					out["applicationName"] = v
				}
				if client := firstString(inner, "client"); client != "" {
					out["client"] = client
				}
				return out
			case strings.HasPrefix(suffix, "/conversation/") && strings.HasSuffix(suffix, "/message"):
				return map[string]interface{}{"content": inner["content"]}
			default:
				return inner
			}
		}
	}
	return payload
}

func (c *Client) conversationTableSpecs() []conversationTableSpec {
	return []conversationTableSpec{
		{ConversationTable: "sn_ba_core_conversation", MessageTable: "sn_ba_core_message", CoreStyle: true},
		{ConversationTable: "sn_build_agent_conversation", MessageTable: "sn_build_agent_message"},
	}
}

func (c *Client) getConversationJSON(ctx context.Context, suffix string) ([]byte, int, error) {
	if err := c.ensureConversationHTTPClient(); err != nil {
		return nil, 0, err
	}
	var lastBody []byte
	var lastStatus int
	var lastErr error
	for _, spec := range c.conversationAPISpecs() {
		body, status, err := c.getJSON(ctx, spec.URL(suffix))
		if err == nil {
			return body, status, nil
		}
		lastBody, lastStatus, lastErr = body, status, err
		if !isConversationAPIMissing(status, body) {
			return body, status, err
		}
	}
	return lastBody, lastStatus, lastErr
}

func (c *Client) postConversationJSON(ctx context.Context, suffix string, payload interface{}) ([]byte, int, error) {
	if err := c.ensureConversationHTTPClient(); err != nil {
		return nil, 0, err
	}
	var lastBody []byte
	var lastStatus int
	var lastErr error
	for _, spec := range c.conversationAPISpecs() {
		body, status, err := c.postJSON(ctx, spec.URL(suffix), spec.Payload(suffix, payload))
		if err == nil {
			return body, status, nil
		}
		lastBody, lastStatus, lastErr = body, status, err
		if !isConversationAPIMissing(status, body) {
			return body, status, err
		}
	}
	return lastBody, lastStatus, lastErr
}

func (c *Client) tableAPIURL(table string) string {
	return strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/now/table/" + table
}

func (c *Client) listTableConversations(ctx context.Context) ([]WebConversation, error) {
	fields := "sys_id,title,state,summary,sys_updated_on,sys_created_on,sys_created_by,last_message_at,application_name,application_id,client"
	query := url.Values{}
	query.Set("sysparm_limit", "50")
	query.Set("sysparm_fields", fields)
	query.Set("sysparm_query", "ORDERBYDESCsys_updated_on")
	var lastErr error
	var out []WebConversation
	seen := map[string]struct{}{}
	sawTable := false
	for _, spec := range c.conversationTableSpecs() {
		body, status, err := c.getJSON(ctx, c.tableAPIURL(spec.ConversationTable)+"?"+query.Encode())
		if err == nil {
			sawTable = true
			for _, conv := range parseWebConversations(body) {
				if conv.ID == "" {
					continue
				}
				if _, ok := seen[conv.ID]; ok {
					continue
				}
				seen[conv.ID] = struct{}{}
				out = append(out, conv)
			}
			continue
		}
		lastErr = err
		if !shouldFallbackToConversationTables(status, body) {
			return nil, err
		}
	}
	if sawTable {
		return out, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errWebConversationNotFound
}

func (c *Client) getTableConversation(ctx context.Context, id string) (WebConversation, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return WebConversation{}, errWebConversationNotFound
	}
	fields := "sys_id,title,state,summary,sys_updated_on,sys_created_on,sys_created_by,last_message_at,application_name,application_id,client"
	query := url.Values{}
	query.Set("sysparm_fields", fields)
	var lastErr error
	for _, spec := range c.conversationTableSpecs() {
		conv, ok, err := c.getTableConversationForSpec(ctx, spec, id, query)
		if err != nil {
			lastErr = err
			return WebConversation{}, err
		}
		if ok {
			return conv, nil
		}
	}
	if lastErr != nil {
		return WebConversation{}, errWebConversationNotFound
	}
	return WebConversation{}, errWebConversationNotFound
}

func (c *Client) getTableConversationForSpec(ctx context.Context, spec conversationTableSpec, id string, query url.Values) (WebConversation, bool, error) {
	if query == nil {
		query = url.Values{}
	}
	body, status, err := c.getJSON(ctx, c.tableAPIURL(spec.ConversationTable)+"/"+url.PathEscape(id)+"?"+query.Encode())
	if err == nil {
		conv := parseSingleWebConversation(body)
		if conv.ID == "" {
			conv.ID = id
		}
		return conv, true, nil
	}
	if status == http.StatusNotFound || bytes.Contains(bytes.ToLower(body), []byte("record not found")) || shouldFallbackToConversationTables(status, body) {
		return WebConversation{}, false, nil
	}
	return WebConversation{}, false, err
}

func (c *Client) createTableConversation(ctx context.Context, id string, title string) (WebConversation, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = uuidV4Compact()
	}
	var lastErr error
	for _, spec := range c.conversationTableSpecs() {
		payload := c.tableConversationPayload(spec, id, title)
		body, status, err := c.postJSON(ctx, c.tableAPIURL(spec.ConversationTable), payload)
		if err == nil {
			conv := parseSingleWebConversation(body)
			if conv.ID == "" {
				conv.ID = id
			}
			if conv.Title == "" {
				conv.Title = conversationCreateTitle(title)
			}
			if conv.State == "" {
				if spec.CoreStyle {
					conv.State = "active"
				} else {
					conv.State = "open"
				}
			}
			return conv, nil
		}
		lastErr = err
		if !shouldFallbackToConversationTables(status, body) {
			return WebConversation{}, err
		}
	}
	if lastErr != nil {
		return WebConversation{}, lastErr
	}
	return WebConversation{}, errors.New("no Build Agent conversation table is available")
}

func (c *Client) tableConversationPayload(spec conversationTableSpec, id string, title string) map[string]interface{} {
	now := serviceNowDateTime(time.Now())
	title = conversationCreateTitle(title)
	if spec.CoreStyle {
		return map[string]interface{}{
			"sys_id":  id,
			"title":   title,
			"state":   "active",
			"summary": "",
		}
	}
	applicationID, applicationName := c.conversationApplicationFields()
	payload := map[string]interface{}{
		"sys_id":                    id,
		"title":                     title,
		"state":                     "open",
		"client":                    "ide",
		"active":                    "true",
		"application_id":            applicationID,
		"application_name":          applicationName,
		"last_message_at":           now,
		"last_summarized_msg_index": 0,
		"ba_variant_type":           "paid",
		"working_set":               "[]",
	}
	return payload
}

func (c *Client) persistTableUserMessage(ctx context.Context, content string) error {
	return c.persistTableMessage(ctx, "user", content)
}

func (c *Client) persistTableAssistantMessage(ctx context.Context, content string) error {
	return c.persistTableMessage(ctx, "assistant", content)
}

func (c *Client) persistTableMessage(ctx context.Context, role, content string) error {
	role = normalizePersistedRole(role)
	if role == "" {
		return fmt.Errorf("unsupported Build Agent message role %q", role)
	}
	var lastErr error
	for _, spec := range c.conversationTableSpecs() {
		if _, ok, err := c.getTableConversationForSpec(ctx, spec, c.conversationID, nil); err != nil {
			return err
		} else if !ok {
			continue
		}
		sequence, err := c.nextTableMessageSequence(ctx, spec)
		if err != nil {
			lastErr = err
			continue
		}
		payload, err := c.tableMessagePayload(spec, role, content, sequence)
		if err != nil {
			return err
		}
		body, status, err := c.postJSON(ctx, c.tableAPIURL(spec.MessageTable), payload)
		if err == nil {
			return c.touchTableConversation(ctx, spec)
		}
		lastErr = err
		if !shouldFallbackToConversationTables(status, body) {
			return fmt.Errorf("could not persist user message to %s: %w", spec.MessageTable, err)
		}
	}
	if lastErr != nil {
		return fmt.Errorf("could not persist %s message to Build Agent message table: %w", role, lastErr)
	}
	return errors.New("no Build Agent message table is available")
}

func (c *Client) tableUserMessagePayload(spec conversationTableSpec, content string, sequence int) (map[string]interface{}, error) {
	return c.tableMessagePayload(spec, "user", content, sequence)
}

func (c *Client) tableMessagePayload(spec conversationTableSpec, role, content string, sequence int) (map[string]interface{}, error) {
	role = normalizePersistedRole(role)
	if role == "" {
		return nil, fmt.Errorf("unsupported Build Agent message role %q", role)
	}
	messageID := uuidV4()
	contentJSON, err := gliderChatMessageContent(role, content, messageID)
	if err != nil {
		return nil, err
	}
	if spec.CoreStyle {
		return map[string]interface{}{
			"conversation_id": c.conversationID,
			"role":            role,
			"message_type":    "text",
			"content":         contentJSON,
			"sequence":        sequence,
			"timestamp":       time.Now().UTC().Format(time.RFC3339),
		}, nil
	}
	return map[string]interface{}{
		"conversation": c.conversationID,
		"content":      contentJSON,
		"message_id":   messageID,
		"sequence":     sequence,
		"active":       "true",
	}, nil
}

func gliderChatMessageContent(role, content, messageID string) (string, error) {
	role = normalizePersistedRole(role)
	if role == "" {
		return "", fmt.Errorf("unsupported Build Agent message role %q", role)
	}
	if messageID == "" {
		messageID = uuidV4()
	}
	payload := map[string]interface{}{
		"id":     messageID,
		"sender": role,
		"text":   content,
	}
	switch role {
	case "user":
		payload["hasCheckpoints"] = false
	case "assistant":
		payload["complete"] = true
		payload["duration"] = 0
	}
	contentJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(contentJSON), nil
}

func (c *Client) nextTableMessageSequence(ctx context.Context, spec conversationTableSpec) (int, error) {
	query := url.Values{}
	query.Set("sysparm_limit", "1")
	query.Set("sysparm_fields", "sequence")
	if spec.CoreStyle {
		query.Set("sysparm_query", "conversation_id="+c.conversationID+"^ORDERBYDESCsequence")
	} else {
		query.Set("sysparm_query", "conversation="+c.conversationID+"^ORDERBYDESCsequence")
	}
	body, _, err := c.getJSON(ctx, c.tableAPIURL(spec.MessageTable)+"?"+query.Encode())
	if err != nil {
		return 0, err
	}
	messages := findConversationArray(jsonAny(body))
	if len(messages) == 0 {
		return 1, nil
	}
	if first := asMap(messages[0]); first != nil {
		return intFromAny(first["sequence"]) + 1, nil
	}
	return 1, nil
}

func (c *Client) fetchTableConversationMessages(ctx context.Context, id string) ([]interface{}, error) {
	var lastErr error
	for _, spec := range c.conversationTableSpecs() {
		if _, ok, err := c.getTableConversationForSpec(ctx, spec, id, nil); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		query := url.Values{}
		query.Set("sysparm_limit", "500")
		query.Set("sysparm_fields", "sys_id,conversation_id,conversation,role,message_type,content,sequence,message_id,timestamp,sys_created_on")
		if spec.CoreStyle {
			query.Set("sysparm_query", "conversation_id="+id+"^ORDERBYsequence")
		} else {
			query.Set("sysparm_query", "conversation="+id+"^ORDERBYsequence")
		}
		body, status, err := c.getJSON(ctx, c.tableAPIURL(spec.MessageTable)+"?"+query.Encode())
		if err == nil {
			return parseWebMessages(body), nil
		}
		lastErr = err
		if !shouldFallbackToConversationTables(status, body) {
			return nil, err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errWebConversationNotFound
}

func (c *Client) touchTableConversation(ctx context.Context, spec conversationTableSpec) error {
	if c.conversationID == "" {
		return nil
	}
	payload := map[string]interface{}{"last_message_at": serviceNowDateTime(time.Now())}
	if spec.CoreStyle {
		return nil
	}
	_, status, err := c.patchJSON(ctx, c.tableAPIURL(spec.ConversationTable)+"/"+url.PathEscape(c.conversationID), payload)
	if err != nil && !isConversationAPIMissing(status, nil) {
		return err
	}
	return nil
}

func jsonAny(body []byte) interface{} {
	var raw interface{}
	_ = json.Unmarshal(body, &raw)
	return raw
}

func serviceNowDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

func conversationCreateTitle(seed string) string {
	title := singleLineLabel(seed)
	if title == "" {
		return "New conversation"
	}
	return title
}

func (c *Client) conversationApplicationFields() (interface{}, string) {
	if c.currentApp == nil {
		return nil, ""
	}
	var applicationID interface{}
	if strings.TrimSpace(c.currentApp.ScopeID) != "" {
		applicationID = strings.TrimSpace(c.currentApp.ScopeID)
	}
	return applicationID, strings.TrimSpace(c.currentApp.ScopeName)
}

func (c *Client) conversationListApplicationIDs() []string {
	seen := map[string]struct{}{}
	var ids []string
	add := func(raw string) {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
		}) {
			id := normalizeGatewayConversationID(part)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	add(c.opts.ApplicationIDList)
	add(firstEnv("BA_APPLICATION_ID_LIST", "BA_APP_ID_LIST", "BA_CONVERSATION_APP_IDS"))
	if c.currentApp != nil {
		add(c.currentApp.AppSysID)
		add(c.currentApp.ScopeID)
	}
	collectConversationApplicationIDs(c.appScope, add)
	collectConversationApplicationIDs(c.workingSet, add)
	return ids
}

func (c *Client) discoverConversationApplicationIDs(ctx context.Context) ([]string, error) {
	body, status, err := c.getJSON(ctx, strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/sn_glider/applications/all")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("applications/all returned status %d", status)
	}
	apps := findConversationArray(jsonAny(body))
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(apps))
	for _, item := range apps {
		m := asMap(item)
		if m == nil || !isGliderIDEApplication(m) {
			continue
		}
		id := normalizeGatewayConversationID(firstString(m, "sys_id", "sysId", "id"))
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func isGliderIDEApplication(app map[string]interface{}) bool {
	if !strings.EqualFold(strings.TrimSpace(stringify(app["ide_created"])), "IDE") {
		return false
	}
	if active := strings.TrimSpace(stringify(app["active"])); active != "" && active != "1" && !strings.EqualFold(active, "true") {
		return false
	}
	return true
}

func collectConversationApplicationIDs(v interface{}, add func(string)) {
	switch typed := v.(type) {
	case nil:
		return
	case string:
		add(typed)
	case []interface{}:
		for _, item := range typed {
			collectConversationApplicationIDs(item, add)
		}
	case []map[string]interface{}:
		for _, item := range typed {
			collectConversationApplicationIDs(item, add)
		}
	case map[string]interface{}:
		for _, key := range []string{"application_id", "applicationId", "app_id", "appId", "appSysId", "app_sys_id", "appDir", "app_dir", "scopeId", "scope_id", "sys_id", "sysId", "id"} {
			if value, ok := typed[key]; ok {
				add(stringify(value))
			}
		}
		for _, value := range typed {
			collectConversationApplicationIDs(value, add)
		}
	}
}

func buildAgentConversationListSuffix(applicationIDs []string) string {
	query := url.Values{}
	if len(applicationIDs) > 0 {
		query.Set("application_id_list", strings.Join(applicationIDs, ","))
	}
	query.Set("client", "ide")
	return "/conversations?" + query.Encode()
}

func filterWebConversationsForApplications(conversations []WebConversation, applicationIDs []string) []WebConversation {
	if len(applicationIDs) == 0 || len(conversations) == 0 {
		return conversations
	}
	allowed := map[string]struct{}{}
	for _, id := range applicationIDs {
		if normalized := normalizeGatewayConversationID(id); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return conversations
	}
	out := make([]WebConversation, 0, len(conversations))
	for _, conv := range conversations {
		appID := normalizeGatewayConversationID(conv.ApplicationID)
		if appID == "" {
			out = append(out, conv)
			continue
		}
		if _, ok := allowed[appID]; ok {
			out = append(out, conv)
		}
	}
	return out
}

func mergeWebConversations(primary, extra []WebConversation) []WebConversation {
	if len(extra) == 0 {
		return primary
	}
	merged := make([]WebConversation, 0, len(primary)+len(extra))
	seen := map[string]struct{}{}
	appendUnique := func(conv WebConversation) {
		if conv.ID == "" {
			return
		}
		if _, ok := seen[conv.ID]; ok {
			return
		}
		seen[conv.ID] = struct{}{}
		merged = append(merged, conv)
	}
	for _, conv := range primary {
		appendUnique(conv)
	}
	for _, conv := range extra {
		appendUnique(conv)
	}
	return merged
}

func isConversationAPIMissing(status int, body []byte) bool {
	if status == http.StatusNotFound || isMissingRESTResource(status, body) {
		return true
	}
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("no api")) ||
		bytes.Contains(lower, []byte("api not found")) ||
		bytes.Contains(lower, []byte("no rest service")) ||
		bytes.Contains(lower, []byte("fcsrfevaluator")) ||
		bytes.Contains(lower, []byte("applyrotatedtokens")) ||
		bytes.Contains(lower, []byte("requested uri does not represent any resource")) ||
		bytes.Contains(lower, []byte("invalid table"))
}

func shouldFallbackToConversationTables(status int, body []byte) bool {
	return isConversationAPIMissing(status, body) || status == http.StatusUnauthorized || status == http.StatusForbidden
}

func (c *Client) ListWebConversations(ctx context.Context) ([]WebConversation, error) {
	if err := c.ensureConversationHTTPClient(); err != nil {
		return nil, err
	}
	var lastErr error
	sawEmptyAPI := false
	applicationIDs := c.conversationListApplicationIDs()
	if len(applicationIDs) == 0 {
		if discovered, err := c.discoverConversationApplicationIDs(ctx); err == nil && len(discovered) > 0 {
			applicationIDs = discovered
			if c.debug {
				fmt.Fprintf(os.Stderr, "[conversation list] discovered %d IDE application ids from /api/sn_glider/applications/all\n", len(applicationIDs))
			}
		} else if err != nil && c.debug {
			fmt.Fprintf(os.Stderr, "[conversation list] application id discovery skipped: %v\n", err)
		}
	}
	for _, candidate := range c.conversationListCandidates(applicationIDs) {
		endpoint := candidate.spec.URL(candidate.suffix)
		body, status, err := c.getJSON(ctx, endpoint)
		if err != nil {
			lastErr = err
			if c.debug {
				fmt.Fprintf(os.Stderr, "[conversation list] %s failed status=%d fallback=%v\n", endpoint, status, shouldFallbackToConversationTables(status, body))
			}
			if shouldFallbackToConversationTables(status, body) {
				continue
			}
			return nil, err
		}
		conversations := parseWebConversations(body)
		if c.debug {
			fmt.Fprintf(os.Stderr, "[conversation list] %s status=%d count=%d\n", endpoint, status, len(conversations))
		}
		if len(conversations) > 0 {
			if tableConversations, err := c.listTableConversations(ctx); err == nil && len(tableConversations) > 0 {
				tableConversations = filterWebConversationsForApplications(tableConversations, candidate.applicationIDs)
				conversations = mergeWebConversations(conversations, tableConversations)
			} else if err != nil && c.debug {
				fmt.Fprintf(os.Stderr, "[conversation list] table merge skipped: %v\n", err)
			}
			return conversations, nil
		}
		sawEmptyAPI = true
	}
	if conversations, err := c.listTableConversations(ctx); err == nil && len(conversations) > 0 {
		conversations = filterWebConversationsForApplications(conversations, applicationIDs)
		return conversations, nil
	} else if err != nil {
		lastErr = err
	}
	if sawEmptyAPI {
		return nil, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errWebConversationNotFound
}

func (c *Client) getWebConversation(ctx context.Context, id string) (WebConversation, error) {
	body, status, err := c.getConversationJSON(ctx, "/conversation/"+id)
	if err != nil {
		if shouldFallbackToConversationTables(status, body) {
			return c.getTableConversation(ctx, id)
		}
		if status == http.StatusNotFound || bytes.Contains(bytes.ToLower(body), []byte("not found")) {
			return WebConversation{}, errWebConversationNotFound
		}
		return WebConversation{}, err
	}
	conv := parseSingleWebConversation(body)
	if conv.ID == "" {
		conv.ID = id
	}
	return conv, nil
}

func (c *Client) createWebConversation(ctx context.Context, id string) (WebConversation, error) {
	return c.createWebConversationWithTitle(ctx, id, "")
}

func (c *Client) createWebConversationWithTitle(ctx context.Context, id string, title string) (WebConversation, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = uuidV4Compact()
	}
	title = conversationCreateTitle(title)
	applicationID, applicationName := c.conversationApplicationFields()
	payload := map[string]interface{}{
		"payload": map[string]interface{}{
			"conversationId":  id,
			"title":           title,
			"applicationId":   applicationID,
			"applicationName": applicationName,
			"client":          "ide",
		},
	}
	body, status, err := c.postConversationJSON(ctx, "/create", payload)
	if err != nil {
		if shouldFallbackToConversationTables(status, body) {
			return c.createTableConversation(ctx, id, title)
		}
		return WebConversation{}, err
	}
	conv := parseSingleWebConversation(body)
	if conv.ID == "" {
		conv.ID = id
	}
	if conv.Title == "" {
		conv.Title = title
	}
	return conv, nil
}

func (c *Client) ensureWebConversation(ctx context.Context) error {
	return c.ensureWebConversationWithTitle(ctx, "")
}

func (c *Client) ensureWebConversationWithTitle(ctx context.Context, title string) error {
	if c.opts.CodeAssistWS {
		return nil
	}
	if c.conversationID == "" {
		c.conversationID = uuidV4Compact()
	}
	if normalized := normalizeGatewayConversationID(c.conversationID); normalized != "" && normalized != c.conversationID {
		c.conversationID = normalized
		c.serverConversation = false
	}
	if c.serverConversation {
		return nil
	}
	conv, err := c.getWebConversation(ctx, c.conversationID)
	if err != nil && !errors.Is(err, errWebConversationNotFound) {
		return fmt.Errorf("could not verify Build Agent conversation %s: %w", c.conversationID, err)
	}
	if errors.Is(err, errWebConversationNotFound) {
		conv, err = c.createWebConversationWithTitle(ctx, c.conversationID, title)
		if err != nil {
			return fmt.Errorf("could not create Build Agent conversation %s: %w", c.conversationID, err)
		}
	}
	c.applyWebConversation(conv, false)
	return c.saveCurrentState()
}

func (c *Client) persistWebUserMessage(ctx context.Context, content string) error {
	return c.persistWebMessage(ctx, "user", content)
}

func (c *Client) persistWebAssistantMessage(ctx context.Context, content string) error {
	return c.persistWebMessage(ctx, "assistant", content)
}

func (c *Client) persistWebMessage(ctx context.Context, role, content string) error {
	if c.opts.CodeAssistWS {
		return nil
	}
	role = normalizePersistedRole(role)
	if role == "" {
		return fmt.Errorf("unsupported Build Agent message role %q", role)
	}
	if strings.TrimSpace(content) == "" {
		return nil
	}
	title := ""
	if role == "user" {
		title = content
	}
	if err := c.ensureWebConversationWithTitle(ctx, title); err != nil {
		return err
	}
	contentJSON, err := gliderChatMessageContent(role, content, "")
	if err != nil {
		return err
	}
	payload := map[string]interface{}{
		"payload": map[string]interface{}{
			"role":         role,
			"content":      string(contentJSON),
			"message_type": "text",
		},
	}
	body, status, err := c.postConversationJSON(ctx, "/conversation/"+c.conversationID+"/message", payload)
	if err != nil {
		if shouldFallbackToConversationTables(status, body) {
			return c.persistTableMessage(ctx, role, content)
		}
		return fmt.Errorf("could not persist %s message to Build Agent conversation %s: %w", role, c.conversationID, err)
	}
	return nil
}

func normalizePersistedRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user", "assistant", "system":
		return strings.ToLower(strings.TrimSpace(role))
	default:
		return ""
	}
}

func (c *Client) fetchWebConversationMessages(ctx context.Context, id string) ([]interface{}, error) {
	body, status, err := c.getConversationJSON(ctx, "/conversation/"+id+"/messages")
	if err != nil {
		if shouldFallbackToConversationTables(status, body) {
			return c.fetchTableConversationMessages(ctx, id)
		}
		if status == http.StatusNotFound || isConversationAPIMissing(status, body) {
			return nil, errWebConversationNotFound
		}
		return nil, err
	}
	return parseWebMessages(body), nil
}

func (c *Client) applyWebConversation(conv WebConversation, loadMessages bool) {
	oldID := c.conversationID
	if strings.TrimSpace(conv.ID) != "" {
		c.conversationID = strings.TrimSpace(conv.ID)
	}
	c.conversationTitle = strings.TrimSpace(conv.Title)
	c.conversationState = strings.TrimSpace(conv.State)
	c.serverConversation = true
	c.pendingUserContent = ""
	c.resetWebStream()
	if oldID != c.conversationID || c.ambChannel == "" {
		c.updateAMBChannelForConversation()
	}
	if loadMessages {
		c.history = nil
	}
}

func (c *Client) updateAMBChannelForConversation() {
	if c.opts.CodeAssistWS || c.conversationID == "" {
		return
	}
	if c.ambCancel != nil {
		c.ambCancel()
		c.ambCancel = nil
	}
	c.ambSubscribed = false
	if strings.HasPrefix(c.ambChannel, "/build_agent/stream/") {
		c.ambChannel = "/build_agent/stream/" + c.conversationID
	} else {
		c.ambChannel = "/build_agent_core/stream/" + c.conversationID
	}
}

func (c *Client) UseWebConversation(ctx context.Context, conv WebConversation) error {
	if c.processing {
		return errors.New("cannot switch conversation while a turn is processing")
	}
	newID := strings.TrimSpace(conv.ID)
	if newID == "" {
		return errors.New("conversation id is required")
	}
	changedConversation := c.conversationID != "" && c.conversationID != newID
	if c.workspaceName != "" {
		if err := c.saveCurrentState(); err != nil {
			return err
		}
	}
	if changedConversation {
		c.resetUsageTotals()
	}
	c.applyWebConversation(conv, true)
	if messages, err := c.fetchWebConversationMessages(ctx, c.conversationID); err == nil {
		c.history = messages
	} else if c.debug && !errors.Is(err, errWebConversationNotFound) {
		fmt.Fprintf(os.Stderr, "warning: could not load conversation messages: %v\n", err)
	}
	if err := c.saveCurrentState(); err != nil {
		return err
	}
	printCurrentConversation(c)
	printConversationHistoryScrollbackWithStatus(c.conversationTitle, c.history, c.statusBarState())
	return nil
}

func (c *Client) StartNewWebConversation(ctx context.Context) error {
	if c.processing {
		return errors.New("cannot switch conversation while a turn is processing")
	}
	c.resetUsageTotals()
	c.conversationID = uuidV4Compact()
	c.conversationTitle = ""
	c.conversationState = ""
	c.serverConversation = false
	c.pendingUserContent = ""
	c.resetWebStream()
	c.updateAMBChannelForConversation()
	c.history = nil
	if err := c.saveCurrentState(); err != nil {
		return err
	}
	terminalSetConversationHistory("New conversation", nil)
	if replayConversationHistoryFromTranscript(c.statusBarState()) {
		return nil
	}
	printCurrentConversation(c)
	return nil
}

func (c *Client) PromptWebConversation(ctx context.Context, source string) error {
	if c.processing {
		return errors.New("cannot switch conversation while a turn is processing")
	}
	conversations, err := c.ListWebConversations(ctx)
	if err != nil {
		if source == "startup" {
			fmt.Fprintf(os.Stderr, "warning: could not load Build Agent conversations; continuing with a new conversation id (%v)\n", err)
			return nil
		}
		return err
	}
	return c.SelectWebConversation(ctx, conversations, true)
}

func (c *Client) SelectWebConversation(ctx context.Context, conversations []WebConversation, allowNew bool) error {
	for {
		answer, err := promptConversationSelection(conversations, c.conversationID, allowNew)
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" || answer == "__new__" || strings.EqualFold(answer, "n") || strings.EqualFold(answer, "new") {
			if allowNew {
				return c.StartNewWebConversation(ctx)
			}
			fmt.Fprintln(os.Stderr, "conversation unchanged")
			return nil
		}
		if answer == "__cancel__" || strings.EqualFold(answer, "q") || strings.EqualFold(answer, "cancel") {
			fmt.Fprintln(os.Stderr, "conversation unchanged")
			return nil
		}
		conv, ok := conversationBySelection(conversations, answer)
		if !ok {
			fmt.Fprintln(os.Stderr, "unknown conversation; enter a list number, id/prefix, n, or q")
			continue
		}
		return c.UseWebConversation(ctx, conv)
	}
}

func (c *Client) SelectConversationByArg(ctx context.Context, selection string, allowNew bool) error {
	selection = strings.TrimSpace(selection)
	if selection == "" || strings.EqualFold(selection, "current") {
		printCurrentConversation(c)
		return nil
	}
	if strings.EqualFold(selection, "new") || strings.EqualFold(selection, "__new__") {
		if !allowNew {
			return errors.New("creating a new conversation is not allowed here")
		}
		return c.StartNewWebConversation(ctx)
	}

	conversations, err := c.ListWebConversations(ctx)
	if err != nil {
		return err
	}
	if len(conversations) == 0 {
		return errors.New("no Build Agent conversations found")
	}
	var conv WebConversation
	var ok bool
	if strings.EqualFold(selection, "latest") {
		conv, ok = conversations[0], true
	} else {
		conv, ok = conversationBySelection(conversations, selection)
	}
	if !ok {
		return fmt.Errorf("conversation %q not found in server list", selection)
	}
	return c.UseWebConversation(ctx, conv)
}

func printConversationPicker(conversations []WebConversation, currentID string) {
	fmt.Fprintln(os.Stderr, "Build Agent conversations:")
	if len(conversations) == 0 {
		fmt.Fprintln(os.Stderr, "  <none found>")
	} else {
		for i, conv := range conversations {
			marker := " "
			if conv.ID != "" && conv.ID == currentID {
				marker = "*"
			}
			fmt.Fprintf(os.Stderr, "%s %2d) %s\n", marker, i+1, conversationLabel(conv))
		}
	}
	fmt.Fprintln(os.Stderr, "   n) New conversation")
}

func printConversationList(conversations []WebConversation, currentID string) {
	if len(conversations) == 0 {
		fmt.Fprintln(os.Stderr, "no Build Agent conversations found")
		return
	}
	fmt.Fprintln(os.Stderr, "Build Agent conversations:")
	for i, conv := range conversations {
		marker := " "
		if conv.ID != "" && conv.ID == currentID {
			marker = "*"
		}
		fmt.Fprintf(os.Stderr, "%s %2d) %s\n", marker, i+1, conversationLabel(conv))
	}
}

func printCurrentConversation(c *Client) {
	if c.conversationID == "" || (!c.serverConversation && c.conversationTitle == "") {
		fmt.Fprintln(os.Stderr, "conversation: <new>")
		return
	}
	label := c.conversationID
	if c.conversationTitle != "" {
		label = c.conversationTitle + "  " + shortConversationID(c.conversationID)
	}
	if c.conversationState != "" {
		label += "  [" + c.conversationState + "]"
	}
	fmt.Fprintf(os.Stderr, "conversation: %s\n", label)
}

func conversationBySelection(conversations []WebConversation, answer string) (WebConversation, bool) {
	if idx, err := strconv.Atoi(answer); err == nil {
		if idx >= 1 && idx <= len(conversations) {
			return conversations[idx-1], true
		}
		return WebConversation{}, false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	var matched *WebConversation
	for i := range conversations {
		id := strings.ToLower(conversations[i].ID)
		if id == answer || strings.HasPrefix(id, answer) {
			if matched != nil {
				return WebConversation{}, false
			}
			matched = &conversations[i]
		}
	}
	if matched == nil {
		return WebConversation{}, false
	}
	return *matched, true
}

func conversationLabel(conv WebConversation) string {
	title := singleLineLabel(conv.Title)
	if title == "" {
		title = "Untitled conversation"
	}
	parts := []string{title}
	if conv.State != "" {
		parts = append(parts, "["+conv.State+"]")
	}
	if conv.UpdatedAt != "" {
		parts = append(parts, conv.UpdatedAt)
	}
	if conv.ID != "" {
		parts = append(parts, shortConversationID(conv.ID))
	}
	return strings.Join(parts, "  ")
}

func singleLineLabel(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func shortConversationID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:8] + "…" + id[len(id)-4:]
}

func parseWebConversations(body []byte) []WebConversation {
	var raw interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	arr := findConversationArray(raw)
	out := make([]WebConversation, 0, len(arr))
	seen := map[string]struct{}{}
	for _, item := range arr {
		if m := asMap(item); m != nil {
			conv := conversationFromMap(m)
			if conv.ID == "" {
				continue
			}
			if _, exists := seen[conv.ID]; exists {
				continue
			}
			seen[conv.ID] = struct{}{}
			out = append(out, conv)
		}
	}
	return out
}

func parseSingleWebConversation(body []byte) WebConversation {
	var raw interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return WebConversation{}
	}
	if root := asMap(raw); root != nil {
		if result := asMap(root["result"]); result != nil {
			return conversationFromMap(result)
		}
		return conversationFromMap(root)
	}
	arr := findConversationArray(raw)
	if len(arr) > 0 {
		if m := asMap(arr[0]); m != nil {
			return conversationFromMap(m)
		}
	}
	return WebConversation{}
}

func findConversationArray(raw interface{}) []interface{} {
	if arr, ok := raw.([]interface{}); ok {
		return arr
	}
	m := asMap(raw)
	if m == nil {
		return nil
	}
	for _, key := range []string{"result", "conversations", "items", "records"} {
		if arr, ok := m[key].([]interface{}); ok {
			return arr
		}
		if nested := asMap(m[key]); nested != nil {
			if arr := findConversationArray(nested); len(arr) > 0 {
				return arr
			}
		}
	}
	return nil
}

func conversationFromMap(m map[string]interface{}) WebConversation {
	return WebConversation{
		ID:              strings.TrimSpace(firstString(m, "conversation_id", "conversationId", "conversationID", "sys_id", "sysId", "id")),
		Title:           strings.TrimSpace(firstString(m, "title", "name")),
		State:           strings.TrimSpace(firstString(m, "state", "status")),
		Summary:         strings.TrimSpace(firstString(m, "summary")),
		CreatedBy:       strings.TrimSpace(firstString(m, "created_by", "sys_created_by")),
		UpdatedAt:       strings.TrimSpace(firstString(m, "updated_at", "sys_updated_on", "timestamp", "updatedOn", "last_message_at", "lastMessageAt")),
		ApplicationID:   strings.TrimSpace(firstString(m, "application_id", "applicationId", "app_id", "appId")),
		ApplicationName: strings.TrimSpace(firstString(m, "application_name", "applicationName", "app_name", "appName")),
	}
}

func parseWebMessages(body []byte) []interface{} {
	var raw interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	arr := findConversationArray(raw)
	messages := make([]webMessage, 0, len(arr))
	for i, item := range arr {
		m := asMap(item)
		if m == nil {
			continue
		}
		rawRole := strings.ToLower(strings.TrimSpace(firstString(m, "role", "author", "author_type", "type", "sender")))
		role := rawRole
		if rawRole == "" {
			if bodyMap := asMap(m["body"]); bodyMap != nil {
				rawRole = strings.ToLower(strings.TrimSpace(firstString(bodyMap, "author_type", "author_id", "role", "sender")))
				role = rawRole
			}
		}
		content := strings.TrimSpace(firstString(m, "content", "text", "message"))
		if bodyMap := asMap(m["body"]); content == "" && bodyMap != nil {
			content = strings.TrimSpace(firstString(bodyMap, "text", "message", "content"))
		}
		contentRole, contentText, contentHasRole := messageContentRoleText(content)
		if contentHasRole {
			if contentRole == "" {
				continue
			}
			role = contentRole
		}
		if contentText != "" {
			content = contentText
		} else {
			content = messageContentText(content)
		}
		role = normalizeMessageRole(role)
		if rawRole == "" && !contentHasRole {
			role = ""
		}
		if role == "" && rawRole != "" {
			continue
		}
		if content == "" {
			continue
		}
		messages = append(messages, webMessage{Index: i, Sequence: intFromAny(m["sequence"]), Timestamp: firstString(m, "timestamp", "sys_created_on"), Role: role, Content: content})
	}
	sort.SliceStable(messages, func(i, j int) bool {
		if messages[i].Sequence != 0 || messages[j].Sequence != 0 {
			return messages[i].Sequence < messages[j].Sequence
		}
		if messages[i].Timestamp != messages[j].Timestamp {
			return messages[i].Timestamp < messages[j].Timestamp
		}
		return messages[i].Index < messages[j].Index
	})
	out := make([]interface{}, 0, len(messages))
	lastRole := ""
	for _, msg := range messages {
		role := msg.Role
		if role == "" {
			role = inferBareWebMessageRole(lastRole)
		}
		if role == "" {
			continue
		}
		out = append(out, map[string]interface{}{"role": role, "content": msg.Content})
		lastRole = role
	}
	return out
}

type webMessage struct {
	Index     int
	Sequence  int
	Timestamp string
	Role      string
	Content   string
}

func inferBareWebMessageRole(previousRole string) string {
	if previousRole == "user" {
		return "assistant"
	}
	return "user"
}

func messageContentText(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(content), &decoded); err == nil {
		if text := strings.TrimSpace(firstString(decoded, "text", "message", "content")); text != "" {
			return text
		}
	}
	return content
}

func messageContentRoleText(content string) (string, string, bool) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", "", false
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(content), &decoded); err != nil {
		return "", "", false
	}
	rawRole := strings.TrimSpace(firstString(decoded, "sender", "role", "author", "type"))
	role := ""
	hasRole := rawRole != ""
	if rawRole != "" {
		role = normalizeMessageRole(rawRole)
	}
	text := strings.TrimSpace(firstString(decoded, "text", "message", "content"))
	return role, text, hasRole
}

func normalizeMessageRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	switch role {
	case "", "text":
		return "user"
	case "assistant":
		return "assistant"
	case "assistant-thinking", "thinking", "assistant-tool", "tool", "remote_tool", "loading":
		return ""
	case "user", "system":
		return role
	default:
		if strings.Contains(role, "tool") || strings.Contains(role, "thinking") {
			return ""
		}
		if strings.Contains(role, "assistant") {
			return "assistant"
		}
		return role
	}
}

func intFromAny(v interface{}) int {
	switch typed := v.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		i, _ := typed.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(typed))
		return i
	default:
		return 0
	}
}

func conversationCommandContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, 45*time.Second)
}
