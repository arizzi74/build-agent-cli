package main

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
func (c *Client) answerInstanceSkillsList(ctx context.Context) (map[string]interface{}, string) {
	body, statusCode, err := c.getInstanceSkillJSON(ctx, "summary")
	if err != nil {
		if statusCode == http.StatusBadRequest {
			return map[string]interface{}{
				"content": "[]",
				"warning": instanceSkillsUnsupportedWarning(c.cfg.InstanceURL),
			}, "complete"
		}
		return instanceSkillError("INSTANCE_SKILLS_ERROR", "Failed to fetch instance skills"), "error"
	}
	payload, ok := parseInstanceSkillPayload(body)
	if !ok || !isJSONArray(payload["skills"]) {
		return instanceSkillError("INSTANCE_SKILLS_ERROR", "Invalid instance skills response"), "error"
	}
	return map[string]interface{}{"content": string(payload["skills"])}, "complete"
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
	body, _, err := c.getInstanceSkillJSON(ctx, url.PathEscape(name))
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
