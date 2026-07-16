package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
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

func (c *Client) conversationListApplicationIDs(ctx context.Context) ([]string, bool) {
	seen := map[string]struct{}{}
	var ids []string
	add := func(raw string) {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
		}) {
			id := normalizeConversationApplicationID(part)
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
	if err := c.ensureWorkspaceFoldersForAppPicker(ctx); err != nil && c.debug {
		c.debugf("[conversation list] workspace app refresh skipped: %v\n", err)
	}
	workspaceScoped := strings.TrimSpace(c.workspaceURI) != "" || len(c.workspaceFolders) > 0 || c.workingSet != nil
	for _, folder := range c.workspaceFolders {
		if app, ok := appScopeFromWorkspaceFolder(folder); ok {
			add(app.AppSysID)
			add(app.ScopeID)
			// Build Agent conversation rows vary by release: application_id can
			// be either the sys_id or the textual scope (for example x_snc_foo).
			// Resolve both aliases so the server query and local safety filter
			// cannot leak scope-valued conversations from another workspace.
			if metadata, err := c.fetchServiceNowAppBySysID(ctx, app.AppSysID); err == nil {
				add(metadata.Scope)
			} else if c.debug {
				c.debugf("[conversation list] app scope lookup skipped for %s: %v\n", app.AppSysID, err)
			}
		}
	}
	collectConversationApplicationIDs(c.workingSet, add)
	if len(ids) > 0 {
		return ids, true
	}
	add(c.opts.ApplicationIDList)
	add(firstEnv("BA_APPLICATION_ID_LIST", "BA_APP_ID_LIST", "BA_CONVERSATION_APP_IDS"))
	if len(ids) > 0 {
		return ids, workspaceScoped
	}
	if workspaceScoped {
		return nil, true
	}
	if c.currentApp != nil {
		add(c.currentApp.AppSysID)
		add(c.currentApp.ScopeID)
	}
	collectConversationApplicationIDs(c.appScope, add)
	return ids, false
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

func normalizeConversationApplicationID(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" || raw == "<nil>" || raw == "null" {
		return ""
	}
	if compact := normalizeGatewayConversationID(raw); compact != "" {
		return compact
	}
	return raw
}

func filterWebConversationsForApplications(conversations []WebConversation, applicationIDs []string) []WebConversation {
	return filterWebConversationsForWorkspaceApplications(conversations, applicationIDs, false)
}

func filterWebConversationsForWorkspaceApplications(conversations []WebConversation, applicationIDs []string, workspaceScoped bool) []WebConversation {
	if workspaceScoped && len(applicationIDs) == 0 {
		out := make([]WebConversation, 0, len(conversations))
		for _, conv := range conversations {
			if normalizeConversationApplicationID(conv.ApplicationID) == "" {
				out = append(out, conv)
			}
		}
		return out
	}
	if len(applicationIDs) == 0 || len(conversations) == 0 {
		return conversations
	}
	allowed := map[string]struct{}{}
	for _, id := range applicationIDs {
		if normalized := normalizeConversationApplicationID(id); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return conversations
	}
	out := make([]WebConversation, 0, len(conversations))
	for _, conv := range conversations {
		appID := normalizeConversationApplicationID(conv.ApplicationID)
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
	applicationIDs, workspaceScoped := c.conversationListApplicationIDs(ctx)
	if workspaceScoped && len(applicationIDs) == 0 {
		if c.debug {
			fmt.Fprintln(os.Stderr, "[conversation list] active workspace has no application ids; listing app-less/global conversations only")
		}
	}
	for _, candidate := range c.conversationListCandidates(applicationIDs) {
		endpoint := candidate.spec.URL(candidate.suffix)
		body, status, err := c.getJSON(ctx, endpoint)
		if err != nil {
			lastErr = err
			if c.debug {
				c.debugf("[conversation list] %s failed status=%d fallback=%v\n", endpoint, status, shouldFallbackToConversationTables(status, body))
			}
			if shouldFallbackToConversationTables(status, body) {
				continue
			}
			return nil, err
		}
		conversations := filterWebConversationsForWorkspaceApplications(parseWebConversations(body), candidate.applicationIDs, workspaceScoped)
		if c.debug {
			c.debugf("[conversation list] %s status=%d count=%d\n", endpoint, status, len(conversations))
		}
		if len(conversations) > 0 {
			if tableConversations, err := c.listTableConversations(ctx); err == nil && len(tableConversations) > 0 {
				tableConversations = filterWebConversationsForWorkspaceApplications(tableConversations, candidate.applicationIDs, workspaceScoped)
				conversations = mergeWebConversations(conversations, tableConversations)
			} else if err != nil && c.debug {
				c.debugf("[conversation list] table merge skipped: %v\n", err)
			}
			return conversations, nil
		}
		sawEmptyAPI = true
	}
	if conversations, err := c.listTableConversations(ctx); err == nil && len(conversations) > 0 {
		conversations = filterWebConversationsForWorkspaceApplications(conversations, applicationIDs, workspaceScoped)
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

// conversationMessageAPISpecs prioritizes the installed Build Agent API used by
// the web client. Older conversations_api routes remain read fallbacks.
func (c *Client) conversationMessageAPISpecs() []conversationAPISpec {
	specs := c.conversationAPISpecs()
	ordered := make([]conversationAPISpec, 0, len(specs))
	for _, spec := range specs {
		if spec.BuildAgentAPI {
			ordered = append(ordered, spec)
		}
	}
	for _, spec := range specs {
		if !spec.BuildAgentAPI {
			ordered = append(ordered, spec)
		}
	}
	return ordered
}

func shouldTryNextConversationMessageAPI(status int, body []byte) bool {
	// A conversation id selected from a successful list is valid. Some older
	// routes report an unsupported message resource as a bare 400 rather than a
	// structured missing-resource response, so continue the read-only chain.
	return status == http.StatusBadRequest || isConversationAPIMissing(status, body)
}

func (c *Client) fetchWebConversationMessages(ctx context.Context, id string) ([]interface{}, error) {
	if err := c.ensureConversationHTTPClient(); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errWebConversationNotFound
	}

	for _, spec := range c.conversationMessageAPISpecs() {
		result, err := c.retryGETResult(ctx, spec.URL("/conversation/"+url.PathEscape(id)+"/messages"), "conversation_history_read", "http", 16<<20, func(req *http.Request) {
			c.setGatewayHeaders(req)
			req.Header.Set("Accept", "application/json")
		})
		body, status := result.Body, result.Status
		if err == nil {
			return parseWebMessages(body), nil
		}
		if shouldTryNextConversationMessageAPI(status, body) {
			continue
		}
		// Do not hide authentication, authorization, or server failures behind a
		// different API/table path.
		return nil, err
	}

	messages, err := c.fetchTableConversationMessages(ctx, id)
	if err == nil {
		return messages, nil
	}
	if errors.Is(err, errWebConversationNotFound) {
		return nil, errWebConversationNotFound
	}
	return nil, err
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
	if err := c.applyWebConversationApp(ctx, conv); err != nil {
		return err
	}
	if messages, err := c.fetchWebConversationMessages(ctx, c.conversationID); err == nil {
		c.history = messages
	} else if c.debug && !errors.Is(err, errWebConversationNotFound) {
		c.debugf("warning: could not load conversation messages: %v\n", err)
	}
	if err := c.saveCurrentState(); err != nil {
		return err
	}
	if _, remote := telegramCommandSourceFromContext(ctx); remote {
		printCurrentConversationTo(slashCommandOutputWriter, c)
	} else {
		printCurrentConversation(c)
		printConversationHistoryScrollbackWithStatus(c.conversationTitle, c.history, c.statusBarState())
	}
	return nil
}

func (c *Client) applyWebConversationApp(ctx context.Context, conv WebConversation) error {
	applicationID := strings.TrimSpace(conv.ApplicationID)
	if applicationID == "" {
		c.currentApp = nil
		c.appScope = nil
		return deleteActiveApp(c.opts.Profile)
	}
	app := AppScope{
		ScopeID:   applicationID,
		ScopeName: strings.TrimSpace(conv.ApplicationName),
		AppSysID:  applicationID,
	}
	if app.ScopeName == "" {
		app.ScopeName = applicationID
	}
	if choices, err := c.ListWorkspaceAppChoices(ctx); err == nil {
		for _, choice := range choices {
			if sameAppScope(choice.App, app) {
				app = choice.App
				break
			}
		}
	} else if c.debug {
		c.debugf("warning: could not match conversation app to workspace apps: %v\n", err)
	}
	c.currentApp = &app
	c.appScope = app.ScopeID
	_ = c.ensureActiveAppMetadata(ctx)
	if err := saveActiveApp(c.opts.Profile, *c.currentApp); err != nil {
		return err
	}
	c.postAppSelectionStatus(ctx)
	return nil
}

func (c *Client) restoreSavedWebConversation(ctx context.Context) {
	if c.opts.CodeAssistWS {
		return
	}
	conversationID := strings.TrimSpace(c.conversationID)
	if conversationID == "" {
		return
	}
	if normalized := normalizeGatewayConversationID(conversationID); normalized != "" {
		conversationID = normalized
		c.conversationID = normalized
	}
	conv, err := c.getWebConversation(ctx, conversationID)
	if err != nil {
		if c.debug && !errors.Is(err, errWebConversationNotFound) {
			c.debugf("warning: could not refresh saved conversation %s: %v\n", shortConversationID(conversationID), err)
		}
		return
	}
	c.applyWebConversation(conv, false)
	if err := c.applyWebConversationApp(ctx, conv); err != nil && c.debug {
		c.debugf("warning: could not restore saved conversation app: %v\n", err)
	}
	if messages, err := c.fetchWebConversationMessages(ctx, c.conversationID); err == nil {
		c.history = messages
	} else if c.debug && !errors.Is(err, errWebConversationNotFound) {
		c.debugf("warning: could not refresh saved conversation messages: %v\n", err)
	}
	if err := c.saveCurrentState(); err != nil && c.debug {
		c.debugf("warning: could not save restored conversation state: %v\n", err)
	}
}

func (c *Client) restoreStartupConversationTranscript(status statusBarState) bool {
	if strings.TrimSpace(c.conversationID) == "" && strings.TrimSpace(c.conversationTitle) == "" && len(c.history) == 0 {
		return false
	}
	title := strings.TrimSpace(c.conversationTitle)
	if title == "" && strings.TrimSpace(c.conversationID) != "" {
		title = "Conversation " + shortConversationID(c.conversationID)
	}
	changed := terminalSetConversationHistory(title, c.history)
	if !changed {
		return false
	}
	if interactiveTerminalUIEnabled() {
		if _, replayed := terminalReplayManagedViewportWithScrollback(status); replayed {
			redrawPendingFooterPromptFromState()
			return true
		}
	}
	return changed
}

// replaceTerminalConversationTranscript establishes a hard UI boundary when
// changing profiles. In particular, a target profile with no saved
// conversation must clear the previous instance's transcript rather than
// leaving it visible merely because restoreStartupConversationTranscript has
// nothing to replay.
func (c *Client) replaceTerminalConversationTranscript(status statusBarState) bool {
	terminalSetConversationHistory("", nil)
	if c.restoreStartupConversationTranscript(status) {
		return true
	}
	if interactiveTerminalUIEnabled() {
		if _, replayed := terminalReplayManagedViewportWithScrollback(status); replayed {
			redrawPendingFooterPromptFromState()
			return true
		}
	}
	return false
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
	c.currentApp = nil
	c.appScope = nil
	if err := deleteActiveApp(c.opts.Profile); err != nil {
		return err
	}
	c.pendingUserContent = ""
	c.resetWebStream()
	c.updateAMBChannelForConversation()
	c.history = nil
	if err := c.saveCurrentState(); err != nil {
		return err
	}
	if _, remote := telegramCommandSourceFromContext(ctx); remote {
		printCurrentConversationTo(slashCommandOutputWriter, c)
		return nil
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
		if _, remote := telegramCommandSourceFromContext(ctx); remote {
			printCurrentConversationTo(slashCommandOutputWriter, c)
		} else {
			printCurrentConversation(c)
		}
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
		if hasGlobalConversations(conversations) {
			fmt.Fprintln(os.Stderr, "  🌐 Global / no app conversations are available across workspaces.")
		}
		for i, conv := range conversations {
			marker := " "
			if conversationIDMatches(conv.ID, currentID) {
				marker = "*"
			}
			fmt.Fprintf(os.Stderr, "%s %2d) %s\n", marker, i+1, conversationLabelWithCurrent(conv, conversationIDMatches(conv.ID, currentID)))
		}
	}
	fmt.Fprintln(os.Stderr, "   n) New conversation")
}

func printConversationList(conversations []WebConversation, currentID string) {
	printConversationListTo(os.Stderr, conversations, currentID)
}

func printConversationListTo(w io.Writer, conversations []WebConversation, currentID string) {
	if len(conversations) == 0 {
		fmt.Fprintln(w, "no Build Agent conversations found")
		return
	}
	fmt.Fprintln(w, "Build Agent conversations:")
	if hasGlobalConversations(conversations) {
		fmt.Fprintln(w, "  🌐 Global / no app conversations are available across workspaces.")
	}
	for i, conv := range conversations {
		marker := " "
		if conversationIDMatches(conv.ID, currentID) {
			marker = "*"
		}
		fmt.Fprintf(w, "%s %2d) %s\n", marker, i+1, conversationLabelWithCurrent(conv, conversationIDMatches(conv.ID, currentID)))
	}
}

func printCurrentConversation(c *Client) {
	printCurrentConversationTo(os.Stderr, c)
}

func printCurrentConversationTo(w io.Writer, c *Client) {
	if c.conversationID == "" || (!c.serverConversation && c.conversationTitle == "") {
		fmt.Fprintln(w, "conversation: <new>")
		return
	}
	label := c.conversationID
	if c.conversationTitle != "" {
		label = c.conversationTitle + "  " + shortConversationID(c.conversationID)
	}
	if c.conversationState != "" {
		label += "  [" + c.conversationState + "]"
	}
	fmt.Fprintf(w, "conversation: %s\n", label)
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
	return conversationLabelWithCurrent(conv, false)
}

func conversationLabelWithCurrent(conv WebConversation, current bool) string {
	title := singleLineLabel(conv.Title)
	if title == "" {
		title = "Untitled conversation"
	}
	appID := singleLineLabel(conv.ApplicationID)
	appName := singleLineLabel(conv.ApplicationName)
	prefix := "🌐 Global / no app"
	if appID != "" {
		if appName == "" {
			appName = shortConversationID(appID)
		}
		prefix = "📦 " + appName
	}
	parts := []string{prefix + " · " + title}
	if state := singleLineLabel(conv.State); state != "" {
		parts = append(parts, "["+state+"]")
	}
	if updatedAt := singleLineLabel(conv.UpdatedAt); updatedAt != "" {
		parts = append(parts, updatedAt)
	}
	if id := singleLineLabel(conv.ID); id != "" {
		parts = append(parts, shortConversationID(id))
	}
	if current {
		parts = append(parts, "🕘 Last used")
	}
	return strings.Join(parts, "  ")
}

func conversationIDMatches(id, currentID string) bool {
	id = strings.TrimSpace(id)
	currentID = strings.TrimSpace(currentID)
	if id == "" || currentID == "" {
		return false
	}
	if id == currentID {
		return true
	}
	currentNormalized := normalizeGatewayConversationID(currentID)
	return currentNormalized != "" && normalizeGatewayConversationID(id) == currentNormalized
}

func hasGlobalConversations(conversations []WebConversation) bool {
	for _, conv := range conversations {
		if strings.TrimSpace(conv.ApplicationID) == "" {
			return true
		}
	}
	return false
}

func singleLineLabel(s string) string {
	s = stripANSI(s)
	return strings.Join(strings.FieldsFunc(strings.TrimSpace(s), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
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
		contentValue := firstMessageContentValue(m)
		attachments := richAttachmentsFromMessageContentValue(contentValue)
		if len(attachments) == 0 {
			attachments = richAttachmentsFromValue(m["attachments"])
		}
		if contentValue == nil {
			if bodyMap := asMap(m["body"]); bodyMap != nil {
				contentValue = firstMessageContentValue(bodyMap)
				attachments = richAttachmentsFromMessageContentValue(contentValue)
				if len(attachments) == 0 {
					attachments = richAttachmentsFromValue(bodyMap["attachments"])
				}
			}
		}
		contentRole, contentText, contentHasRole := messageContentRoleTextValue(contentValue)
		content := strings.TrimSpace(messageContentTextValue(contentValue))
		if contentHasRole {
			if contentRole == "" {
				continue
			}
			role = contentRole
			content = contentText
		} else if contentText != "" {
			content = contentText
		} else if len(attachments) > 0 {
			// An attachment-only rich envelope has no display text. Do not
			// render its serialized metadata as the user's message body.
			content = ""
		}
		role = normalizeMessageRole(role)
		if rawRole == "" && !contentHasRole {
			role = ""
		}
		if role == "" && rawRole != "" {
			continue
		}
		if content == "" && len(attachments) == 0 {
			continue
		}
		messages = append(messages, webMessage{Index: i, Sequence: intFromAny(m["sequence"]), Timestamp: firstString(m, "timestamp", "sys_created_on"), Role: role, Content: content, Attachments: attachments})
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
		message := map[string]interface{}{"role": role, "content": msg.Content}
		if len(msg.Attachments) > 0 {
			message["attachments"] = cloneRichAttachments(msg.Attachments)
		}
		out = append(out, message)
		lastRole = role
	}
	return out
}

type webMessage struct {
	Index       int
	Sequence    int
	Timestamp   string
	Role        string
	Content     string
	Attachments []RichAttachment
}

func inferBareWebMessageRole(previousRole string) string {
	if previousRole == "user" {
		return "assistant"
	}
	return "user"
}

func firstMessageContentValue(m map[string]interface{}) interface{} {
	for _, key := range []string{"content", "text", "message"} {
		if value, ok := m[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func messageContentText(content string) string {
	return messageContentTextValue(content)
}

func messageContentTextValue(content interface{}) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return ""
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal([]byte(value), &decoded); err == nil {
			if text := strings.TrimSpace(firstString(decoded, "text", "message", "content")); text != "" {
				return text
			}
		}
		return value
	default:
		if decoded := asMap(value); decoded != nil {
			return strings.TrimSpace(firstString(decoded, "text", "message", "content"))
		}
		return strings.TrimSpace(stringify(value))
	}
}

func messageContentRoleText(content string) (string, string, bool) {
	return messageContentRoleTextValue(content)
}

func messageContentRoleTextValue(content interface{}) (string, string, bool) {
	var decoded map[string]interface{}
	switch value := content.(type) {
	case string:
		value = strings.TrimSpace(value)
		if value == "" || json.Unmarshal([]byte(value), &decoded) != nil {
			return "", "", false
		}
	default:
		decoded = asMap(value)
		if decoded == nil {
			return "", "", false
		}
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
