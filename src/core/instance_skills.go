package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	instanceSkillsAPIPath     = "/api/sn_build_agent/skills_api/"
	instanceSkillsMaxBody     = 1 << 20
	instanceSkillMaxParamSize = 512
)

// answerInstanceSkillsList mirrors the Forge client-side summary request. It
// is deliberately remote-read-only: it never creates a checkout, syncs an
// app, or writes local state.
func (c *Client) answerInstanceSkillsList(ctx context.Context, payloads ...map[string]interface{}) (map[string]interface{}, string) {
	var payload map[string]interface{}
	if len(payloads) > 0 {
		payload = payloads[0]
	}
	suffix, valid := c.instanceInstructionPath("summary", payload)
	if !valid {
		return instanceSkillError("INVALID_PARAM", "Invalid applicationId"), "error"
	}
	body, statusCode, err := c.getInstanceSkillJSON(ctx, suffix)
	if err != nil {
		if statusCode == http.StatusBadRequest {
			return map[string]interface{}{
				"content": "[]",
				"warning": instanceSkillsUnsupportedWarning(c.cfg.InstanceURL),
			}, "complete"
		}
		return instanceSkillError("INSTANCE_SKILLS_ERROR", "Failed to fetch instance skills"), "error"
	}
	response, ok := parseInstanceSkillPayload(body)
	if !ok || !isJSONArray(response["skills"]) {
		return instanceSkillError("INSTANCE_SKILLS_ERROR", "Invalid instance skills response"), "error"
	}
	return map[string]interface{}{"content": string(response["skills"])}, "complete"
}

// answerInstanceRulesList fetches the active rules applicable to this turn's
// application. Rules are intentionally refreshed on every request, matching
// the web client: cached instructions may no longer be active or in scope.
func (c *Client) answerInstanceRulesList(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	suffix, valid := c.instanceInstructionPath("rules/summary", payload)
	if !valid {
		return instanceSkillError("INVALID_PARAM", "Invalid applicationId"), "error"
	}
	body, statusCode, err := c.getInstanceSkillJSON(ctx, suffix)
	if err != nil {
		// Rules were added after skills. Older instances may have the skills
		// API without this route; a missing route is an empty additive ruleset.
		if statusCode == http.StatusBadRequest || statusCode == http.StatusNotFound {
			return map[string]interface{}{
				"content": "[]",
				"warning": instanceSkillsUnsupportedWarning(c.cfg.InstanceURL),
			}, "complete"
		}
		return instanceSkillError("INSTANCE_RULES_ERROR", "Failed to fetch instance rules"), "error"
	}
	response, ok := parseInstanceSkillPayload(body)
	if !ok {
		return instanceSkillError("INSTANCE_RULES_ERROR", "Invalid instance rules response"), "error"
	}
	rules := response["rules"]
	if len(rules) == 0 || strings.TrimSpace(string(rules)) == "null" {
		return map[string]interface{}{"content": "[]"}, "complete"
	}
	if !isJSONArray(rules) {
		return instanceSkillError("INSTANCE_RULES_ERROR", "Invalid instance rules response"), "error"
	}
	return map[string]interface{}{"content": string(rules)}, "complete"
}

// answerInstanceSkillBody retrieves a single named skill body through the
// same read-only Forge Skills API endpoint used by the web client.
func (c *Client) answerInstanceSkillBody(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	name, valid := safeInstanceSkillName(payload["name"])
	if name == "" {
		return instanceSkillError("MISSING_PARAM", "Missing skill name"), "error"
	}
	if !valid {
		return instanceSkillError("INVALID_PARAM", "Invalid skill name"), "error"
	}
	suffix, valid := c.instanceInstructionPath(url.PathEscape(name), payload)
	if !valid {
		return instanceSkillError("INVALID_PARAM", "Invalid applicationId"), "error"
	}
	body, _, err := c.getInstanceSkillJSON(ctx, suffix)
	if err != nil {
		return instanceSkillError("INSTANCE_ERROR", "Failed to fetch skill body: "+name), "error"
	}
	response, ok := parseInstanceSkillPayload(body)
	if !ok || !isJSONString(response["body"]) {
		return instanceSkillError("INSTANCE_ERROR", "Failed to fetch skill body: "+name), "error"
	}
	var content string
	if err := json.Unmarshal(response["body"], &content); err != nil {
		return instanceSkillError("INSTANCE_ERROR", "Failed to fetch skill body: "+name), "error"
	}
	return map[string]interface{}{"content": content}, "complete"
}

// instanceInstructionPath mirrors the current web client's application
// precedence: an explicit applicationId, then the active scope ID/name. Keep
// it in the query, never the path, and never fall back to an unscoped request
// when a caller supplied malformed application context.
func (c *Client) instanceInstructionPath(path string, payload map[string]interface{}) (string, bool) {
	applicationID := ""
	if raw, supplied := payload["applicationId"]; supplied && raw != nil {
		var valid bool
		applicationID, valid = safeInstanceSkillName(raw)
		if !valid {
			return "", false
		}
	}
	if applicationID == "" {
		if appScope, ok := c.nirvanaOutboundAppScope(); ok {
			applicationID = firstString(appScope, "scopeId", "scopeName")
		}
	}
	if applicationID == "" {
		return path, true
	}
	if !safeInstanceSkillText(applicationID) {
		return "", false
	}
	return path + "?" + url.Values{"application_id": []string{applicationID}}.Encode(), true
}

// answerInstanceSkillResource retrieves a single resource without treating its
// name as a filesystem path. Resource names are sent only as an encoded query
// value and traversal-like filename input is rejected before the HTTP request.
func (c *Client) answerInstanceSkillResource(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	skillName, skillNameValid := safeInstanceSkillName(payload["skill_name"])
	filename, filenameValid := safeInstanceSkillFilename(payload["filename"])
	if skillName == "" || filename == "" {
		return instanceSkillError("MISSING_PARAM", "Missing skill_name or filename"), "error"
	}
	if !skillNameValid || !filenameValid {
		return instanceSkillError("INVALID_PARAM", "Invalid skill_name or filename"), "error"
	}
	query := url.Values{"filename": []string{filename}}
	body, _, err := c.getInstanceSkillJSON(ctx, url.PathEscape(skillName)+"/resources?"+query.Encode())
	if err != nil {
		return instanceSkillError("INSTANCE_ERROR", "Failed to fetch resource: "+filename), "error"
	}
	response, ok := parseInstanceSkillPayload(body)
	if !ok || !isJSONString(response["content"]) {
		return instanceSkillError("INSTANCE_ERROR", "Failed to fetch resource: "+filename), "error"
	}
	var content string
	if err := json.Unmarshal(response["content"], &content); err != nil {
		return instanceSkillError("INSTANCE_ERROR", "Failed to fetch resource: "+filename), "error"
	}
	return map[string]interface{}{"content": content}, "complete"
}

func (c *Client) getInstanceSkillJSON(ctx context.Context, suffix string) ([]byte, int, error) {
	if c.httpClient == nil {
		if err := c.initGatewayHTTPClient(); err != nil {
			return nil, 0, err
		}
	}
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + instanceSkillsAPIPath + suffix
	result, err := c.retryGETResult(ctx, endpoint, "instance_skills_read", "http", instanceSkillsMaxBody, func(req *http.Request) {
		c.setGatewayHeaders(req)
		req.Header.Set("Accept", "application/json")
	})
	return result.Body, result.Status, err
}

func instanceSkillsUnsupportedWarning(instanceURL string) string {
	return "not supported by instance " + instanceNameFromURL(instanceURL)
}

func safeInstanceSkillName(raw interface{}) (string, bool) {
	name, ok := raw.(string)
	if !ok {
		return "", false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", true
	}
	return name, safeInstanceSkillText(name)
}

func safeInstanceSkillFilename(raw interface{}) (string, bool) {
	filename, ok := raw.(string)
	if !ok {
		return "", false
	}
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return "", true
	}
	if !safeInstanceSkillText(filename) || strings.HasPrefix(filename, "/") || strings.Contains(filename, `\`) {
		return filename, false
	}
	for _, segment := range strings.Split(filename, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return filename, false
		}
	}
	return filename, true
}

func safeInstanceSkillText(value string) bool {
	if len(value) > instanceSkillMaxParamSize || !utf8.ValidString(value) {
		return false
	}
	return !strings.ContainsAny(value, "\x00\r\n\t")
}

// parseInstanceSkillPayload accepts the raw ServiceNow REST envelope used by
// GlideUtils.request (where data is response.result), while retaining direct
// payload support for compatible older instances and focused test servers.
func parseInstanceSkillPayload(body []byte) (map[string]json.RawMessage, bool) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return nil, false
	}
	if rawResult, hasResult := root["result"]; hasResult {
		var result map[string]json.RawMessage
		if err := json.Unmarshal(rawResult, &result); err != nil || result == nil {
			return nil, false
		}
		return result, true
	}
	return root, true
}

func isJSONArray(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "[") {
		return false
	}
	var values []json.RawMessage
	return json.Unmarshal(raw, &values) == nil
}

func isJSONString(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, `"`) {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func instanceSkillError(code, message string) map[string]interface{} {
	return map[string]interface{}{"error": message, "code": code}
}
