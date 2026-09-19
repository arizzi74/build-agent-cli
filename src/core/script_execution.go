package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	instanceScriptMaxBytes         = 1 << 20
	instanceScriptMaxIntentBytes   = 16 << 10
	instanceScriptMaxScopeBytes    = 1 << 10
	instanceScriptMaxResponseBytes = 4 << 20
	instanceScriptMaxReasonBytes   = 512
)

// These are client safety limits, not limits advertised by the instance. Scope
// is deliberately opaque: the backend accepts a scope name, sys_id, or global.
type instanceScriptRequest struct {
	Script string `json:"script"`
	Intent string `json:"intent"`
	Scope  string `json:"scope,omitempty"`
}

func parseInstanceScriptRequest(payload map[string]interface{}) (instanceScriptRequest, error) {
	script, ok := payload["script"].(string)
	if !ok || strings.TrimSpace(script) == "" || !utf8.ValidString(script) || len(script) > instanceScriptMaxBytes {
		return instanceScriptRequest{}, errors.New("script must be nonempty UTF-8 text of at most 1 MiB")
	}
	intent, ok := payload["intent"].(string)
	if !ok || strings.TrimSpace(intent) == "" || !utf8.ValidString(intent) || len(intent) > instanceScriptMaxIntentBytes {
		return instanceScriptRequest{}, errors.New("intent must be nonempty UTF-8 text of at most 16 KiB")
	}
	var scope string
	if raw, exists := payload["scope"]; exists {
		scope, ok = raw.(string)
		if !ok || !utf8.ValidString(scope) || len(scope) > instanceScriptMaxScopeBytes {
			return instanceScriptRequest{}, errors.New("scope must be UTF-8 text of at most 1 KiB")
		}
	}
	return instanceScriptRequest{Script: script, Intent: intent, Scope: scope}, nil
}

// runInstanceScript must only be called after explicit approval of this exact
// script, intent, scope, and current instance. It does not prompt or auto-approve.
// A canceled/failed request is not retried: execution may already have started.
func (c *Client) runInstanceScript(ctx context.Context, request instanceScriptRequest) (map[string]interface{}, string) {
	if _, err := parseInstanceScriptRequest(map[string]interface{}{"script": request.Script, "intent": request.Intent, "scope": request.Scope}); err != nil {
		return instanceScriptError("RUN_SCRIPT_ERROR", err.Error()), "error"
	}
	data, err := c.postInstanceScript(ctx, "runScript", request)
	if err != nil {
		return instanceScriptError("RUN_SCRIPT_ERROR", err.Error()), "error"
	}
	ran, err := instanceScriptResponseBool(data, "ran")
	if err != nil {
		return instanceScriptError("RUN_SCRIPT_ERROR", "Invalid instance script response: missing or invalid ran flag; execution state is unknown, so the request was not retried"), "error"
	}
	if !ran {
		content := c.instanceScriptFailureDiagnostics(data)
		content["error"] = instanceScriptProcessorFailure("Script", content)
		return instanceScriptContent(false, content), "complete"
	}
	content := map[string]interface{}{"scope": data["scope"]}
	if id, exists := data["sys_id"]; exists {
		content["sys_id"] = id
	}
	content["execution_history"] = data["execution_history"]
	content["rollback_context"] = data["rollback_context"]
	content["output"] = data["result"]
	return instanceScriptContent(true, content), "complete"
}

// rollbackInstanceScript has the same explicit, exact-payload approval
// precondition as runInstanceScript. A rollback is itself a mutating request.
func (c *Client) rollbackInstanceScript(ctx context.Context, rollbackContext string) (map[string]interface{}, string) {
	if err := validateInstanceRollbackContext(rollbackContext); err != nil {
		return instanceScriptError("ROLLBACK_SCRIPT_ERROR", err.Error()), "error"
	}
	data, err := c.postInstanceScript(ctx, "rollbackScript", map[string]string{"rollback_context": rollbackContext})
	if err != nil {
		return instanceScriptError("ROLLBACK_SCRIPT_ERROR", err.Error()), "error"
	}
	rolledBack, err := instanceScriptResponseBool(data, "rolled_back")
	if err != nil {
		return instanceScriptError("ROLLBACK_SCRIPT_ERROR", "Invalid instance rollback response: missing or invalid rolled_back flag; rollback state is unknown, so the request was not retried"), "error"
	}
	if !rolledBack {
		content := c.instanceScriptFailureDiagnostics(data)
		content["rolled_back"] = false
		if value, ok := c.safeInstanceScriptDiagnostic(data["rollback_context"], instanceScriptMaxScopeBytes); ok {
			content["rollback_context"] = value
		}
		content["error"] = instanceScriptProcessorFailure("Rollback", content)
		if _, exists := content["reason"]; !exists {
			content["reason"] = "Rollback was not performed; the instance did not provide a safe diagnostic reason"
		}
		return instanceScriptContent(false, content), "complete"
	}
	content := map[string]interface{}{"rolled_back": rolledBack}
	if value, exists := data["rollback_context"]; exists {
		content["rollback_context"] = value
	}
	return instanceScriptContent(true, content), "complete"
}

// A processor failure is distinct from a rejected approval. Forward only its
// documented diagnostic fields, never an upstream HTML page or auth material.
func (c *Client) instanceScriptFailureDiagnostics(data map[string]interface{}) map[string]interface{} {
	content := make(map[string]interface{})
	for _, field := range []string{"scope", "sys_id"} {
		if value, ok := c.safeInstanceScriptDiagnostic(data[field], instanceScriptMaxScopeBytes); ok {
			content[field] = value
		}
	}
	if value, exists := data["http_status"]; exists && value != nil {
		content["http_status"] = value
	}
	if reason, ok := c.safeInstanceScriptDiagnostic(data["reason"], instanceScriptMaxReasonBytes); ok {
		content["reason"] = reason
	}
	return content
}

func instanceScriptProcessorFailure(operation string, content map[string]interface{}) string {
	status, _ := content["http_status"].(float64)
	switch status {
	case http.StatusUnauthorized:
		return operation + " processor rejected the web session (HTTP 401); reconnect before trying again. The request was not automatically retried"
	case http.StatusForbidden:
		return operation + " processor rejected the web session or script permissions (HTTP 403); reconnect and verify script permissions before trying again. The request was not automatically retried"
	}
	message := operation + " was not performed by the instance"
	if operation == "Script" {
		message = "Script was not executed by the instance"
	}
	if status != 0 {
		message += fmt.Sprintf(" (HTTP %.0f)", status)
	}
	if reason, ok := content["reason"].(string); ok {
		message += ": " + reason
	} else {
		message += "; the instance did not provide a safe diagnostic reason"
	}
	return message + ". The request was not automatically retried"
}

func (c *Client) safeInstanceScriptDiagnostic(raw interface{}, limit int) (string, bool) {
	value, ok := raw.(string)
	if !ok || len(value) > limit || !utf8.ValidString(value) {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "<>&{}[]=`") {
		return "", false
	}
	for _, ch := range value {
		if unicode.IsControl(ch) || unicode.In(ch, unicode.Cf) {
			return "", false
		}
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"password", "passwd", "secret", "bearer", "basic ", "cookie", "sessionid", "session:", "token", "authorization", "credential", "csrf", "api key", "api_key", "apikey"} {
		if strings.Contains(lower, marker) {
			return "", false
		}
	}
	sensitive := []string{c.sessionCookieHeader, c.userToken, c.oauthAccessToken, c.basicPass}
	for _, cookie := range strings.Split(c.sessionCookieHeader, ";") {
		if _, cookieValue, found := strings.Cut(cookie, "="); found {
			sensitive = append(sensitive, strings.TrimSpace(cookieValue))
		}
	}
	for _, secret := range sensitive {
		if secret != "" && strings.Contains(value, secret) {
			return "", false
		}
	}
	return value, true
}

func validateInstanceRollbackContext(value string) error {
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || len(value) > instanceScriptMaxScopeBytes {
		return errors.New("rollback_context must be nonempty UTF-8 text of at most 1 KiB")
	}
	return nil
}

func parseInstanceRollbackContext(payload map[string]interface{}) (string, error) {
	value, ok := payload["rollback_context"].(string)
	if !ok {
		return "", errors.New("rollback_context must be nonempty UTF-8 text of at most 1 KiB")
	}
	if err := validateInstanceRollbackContext(value); err != nil {
		return "", err
	}
	return value, nil
}

func instanceScriptError(code, message string) map[string]interface{} {
	return map[string]interface{}{"success": false, "error": map[string]interface{}{"code": code, "message": message}}
}

func instanceScriptContent(success bool, content map[string]interface{}) map[string]interface{} {
	// Values are either fixed strings or already decoded JSON, so marshaling
	// cannot fail. Only the contract's fields are forwarded, never the raw body.
	raw, _ := json.Marshal(content)
	return map[string]interface{}{"success": success, "content": string(raw)}
}

func instanceScriptResponseBool(data map[string]interface{}, key string) (bool, error) {
	value, exists := data[key]
	if !exists || value == nil {
		return false, errors.New("missing script response flag")
	}
	flag, ok := value.(bool)
	if !ok {
		return false, errors.New("invalid script response flag")
	}
	return flag, nil
}

func (c *Client) scriptExecutionSessionAvailable() bool {
	return c.scriptSessionError == "" && c.scriptExecutionCredentialsAvailable()
}

func (c *Client) scriptExecutionCredentialsAvailable() bool {
	return (c.gatewayAuth == authModeForm || c.gatewayAuth == authModeCookie) &&
		strings.TrimSpace(c.sessionCookieHeader) != "" && strings.TrimSpace(c.userToken) != ""
}

// postInstanceScript is intentionally separate from retrying JSON helpers and
// bearer-authenticated Nirvana requests. The instance processor needs the
// current interactive session and its CSRF token, not OAuth or Basic Auth.
// Existing connection setup binds these in-memory credentials to cfg.InstanceURL.
func (c *Client) postInstanceScript(ctx context.Context, action string, payload interface{}) (map[string]interface{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.New("Instance script request canceled before submission")
	}
	if action != "runScript" && action != "rollbackScript" {
		return nil, errors.New("Invalid instance script operation")
	}
	if !c.scriptExecutionSessionAvailable() {
		return nil, errors.New("Instance scripts require an authenticated web session and CSRF token; reconnect using --auth form or --auth cookie")
	}
	base, err := url.Parse(c.cfg.InstanceURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, errors.New("Invalid instance URL for script execution")
	}
	endpoint := *base
	endpoint.Path = "/api/sn_build_agent/build_agent_api/" + action
	endpoint.RawPath = ""
	if !sameOriginInstance(c.cfg.InstanceURL, endpoint.String()) {
		return nil, errors.New("Refusing cross-origin instance script request")
	}
	// Approval may have taken long enough for the browser session to expire.
	// Validate again without renewing or retrying a potentially submitted POST.
	if err := c.validateInstanceScriptSession(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.New("Instance script request canceled before submission")
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > 6*(instanceScriptMaxBytes+instanceScriptMaxIntentBytes+instanceScriptMaxScopeBytes) {
		return nil, errors.New("Invalid or oversized instance script request")
	}
	// Supplying a non-rewindable body (and no idempotency header) prevents the
	// standard HTTP transport from replaying this POST after a connection failure.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), io.NopCloser(bytes.NewReader(raw)))
	if err != nil {
		return nil, errors.New("Could not prepare instance script request")
	}
	req.ContentLength = int64(len(raw))
	setBrowserishHeadersForBase(req.Header, strings.TrimRight(base.String(), "/"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", c.sessionCookieHeader)
	req.Header.Set("X-UserToken", c.userToken)
	hc := http.Client{}
	if c.httpClient != nil {
		hc = *c.httpClient
	}
	// Use exactly the authenticated session above, not additional jar cookies;
	// reject every redirect, including same-origin 307/308 POST-preserving ones.
	hc.Jar = nil
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Instance script request canceled; execution state is unknown, so it was not retried")
		}
		return nil, errors.New("Instance script request failed; execution state is unknown, so it was not retried")
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return nil, errors.New("Instance script request was redirected and was not followed; reconnect and check execution state before retrying")
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return nil, errors.New("Instance rejected the script session or permissions; reconnect and check execution state before retrying")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("Instance script request returned HTTP %d; it was not retried", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, instanceScriptMaxResponseBytes+1))
	if err != nil {
		return nil, errors.New("Could not read instance script response; execution state is unknown")
	}
	if len(body) > instanceScriptMaxResponseBytes {
		return nil, errors.New("Instance script response exceeds 4 MiB; execution state is unknown")
	}
	var envelope map[string]interface{}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, errors.New("Invalid instance script JSON response; execution state is unknown")
	}
	data, ok := envelope["result"].(map[string]interface{})
	if !ok || data == nil {
		return nil, errors.New("Invalid instance script result envelope; execution state is unknown")
	}
	for _, field := range []string{"sys_id", "rollback_context", "scope", "reason"} {
		if value := data[field]; value != nil {
			if _, ok := value.(string); !ok {
				return nil, errors.New("Invalid instance script response fields; execution state is unknown")
			}
		}
	}
	if value := data["http_status"]; value != nil {
		status, ok := value.(float64)
		if !ok || status != math.Trunc(status) {
			return nil, errors.New("Invalid instance script response status; execution state is unknown")
		}
	}
	return data, nil
}
