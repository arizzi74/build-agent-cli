package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	wdfMCPServersAPIPath = "/api/sn_wdf_mcp_client/mcp/servers/"
	mcpToolsMaxBody      = 1 << 20
)

// The WDF discovery route is filtered by connected=true. This is the
// instance's connection state, not a claim that bacli owns an MCP transport.
func mcpServerListing(servers []MCPServer) map[string]interface{} {
	connected := []MCPServer{}
	unknown := []MCPServer{}
	for _, server := range servers {
		if server.Source == "wdf" {
			connected = append(connected, server)
		} else {
			unknown = append(unknown, server)
		}
	}
	result := map[string]interface{}{
		"available": servers, "connected": connected, "unknown": unknown,
		"connectionState": "instance-reported-connected", "managedBy": "ServiceNow backend/Nirvana handshake",
		"message": fmt.Sprintf("%d WDF server(s) reported connected by the instance; %d static/backend-managed server(s) have unknown connection state in bacli.", len(connected), len(unknown)),
	}
	if len(unknown) > 0 {
		result["connectionState"] = "mixed-instance-reported-and-unknown"
	}
	return mcpToolsResultContent(result)
}

func (c *Client) answerMCPToolsList(ctx context.Context, servers []MCPServer, payload map[string]interface{}) (map[string]interface{}, string) {
	id := ""
	for _, key := range []string{"serverId", "server_id"} {
		if raw, present := payload[key]; present && raw != nil {
			value, ok := raw.(string)
			if !ok {
				return instanceSkillError("INVALID_MCP_SERVER_ID", "serverId must be a string"), "error"
			}
			id = strings.TrimSpace(value)
			if id != "" {
				break
			}
		}
	}
	selected := servers
	if id != "" {
		selected = nil
		for _, server := range servers {
			if server.ServerID == id {
				selected = append(selected, server)
				break
			}
		}
		if len(selected) == 0 {
			return instanceSkillError("MCP_SERVER_NOT_FOUND", fmt.Sprintf("No MCP server found with ID: %s", id)), "error"
		}
	}

	// Keep each server's schema intact, and retain server IDs on the flattened
	// list because two configured servers can legitimately share a tool name.
	allTools := []map[string]interface{}{}
	serverResults := []map[string]interface{}{}
	succeeded := 0
	for _, server := range selected {
		if ctx.Err() != nil {
			return instanceSkillError("MCP_TOOLS_CANCELLED", "MCP tool discovery was cancelled"), "error"
		}
		entry := map[string]interface{}{"serverId": server.ServerID, "name": server.Name, "source": server.Source, "available": false}
		if server.Source != "wdf" {
			entry["code"] = "MCP_TOOL_SCHEMAS_UNAVAILABLE"
			entry["error"] = "Tool schemas for this static/backend-managed server are not exposed through the instance WDF API; no direct MCP connection was attempted."
			serverResults = append(serverResults, entry)
			continue
		}
		tools, status, err := c.fetchWDFMCPTools(ctx, server.ServerID)
		if err != nil {
			entry["code"] = "MCP_TOOLS_READ_FAILED"
			entry["error"] = "Failed to retrieve configured WDF server tool schemas"
			if status != 0 {
				entry["httpStatus"] = status
			}
			serverResults = append(serverResults, entry)
			continue
		}
		entry["available"] = true
		entry["tools"] = tools
		serverResults = append(serverResults, entry)
		succeeded++
		for _, tool := range tools {
			identified := make(map[string]interface{}, len(tool)+1)
			for key, value := range tool {
				identified[key] = value
			}
			identified["serverId"] = server.ServerID
			allTools = append(allTools, identified)
		}
	}
	partial := succeeded != len(selected)
	result := map[string]interface{}{
		"tools": allTools, "servers": serverResults, "serverId": id, "partial": partial,
		"dynamicTools": true, "managedBy": "ServiceNow backend/Nirvana handshake",
		"message": fmt.Sprintf("Retrieved %d tool schema(s) from %d configured WDF server(s).", len(allTools), succeeded),
	}
	if partial {
		result["message"] = stringify(result["message"]) + " Some servers could not be enumerated; see per-server errors."
	}
	if partial && succeeded == 0 {
		result["code"] = "MCP_TOOL_SCHEMAS_UNAVAILABLE"
		result["error"] = "No selected server's tool schemas could be enumerated; see per-server errors."
		return mcpToolsResultContent(result), "error"
	}
	return mcpToolsResultContent(result), "complete"
}

func mcpToolsResultContent(result map[string]interface{}) map[string]interface{} {
	// Nirvana consumers use content; keep the structured fields as well for
	// local frontends without replacing actual schemas with display summaries.
	content, _ := json.Marshal(result)
	result["content"] = string(content)
	return result
}

// fetchWDFMCPTools only addresses the instance's fixed read-only API. The
// caller has already selected an entry from connected WDF discovery; URLs
// supplied in tool requests or MCP configuration are never contacted here.
func (c *Client) fetchWDFMCPTools(ctx context.Context, serverID string) ([]map[string]interface{}, int, error) {
	endpoint, err := wdfMCPToolsURL(c.cfg.InstanceURL, serverID)
	if err != nil {
		return nil, 0, err
	}
	if c.httpClient == nil {
		if err := c.initGatewayHTTPClient(); err != nil {
			return nil, 0, err
		}
	}
	httpClient := *c.httpClient
	// This route is fixed; do not send session cookies or custom auth headers
	// through redirects, even to another route on the same instance.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	reader := &Client{
		cfg: c.cfg, opts: c.opts, httpClient: &httpClient,
		gatewayAuth: c.gatewayAuth, basicUser: c.basicUser, basicPass: c.basicPass,
		sessionCookieHeader: c.sessionCookieHeader, userToken: c.userToken,
		oauthAccessToken: c.oauthAccessToken, remoteRetryPolicy: c.remoteRetryPolicy,
	}
	response, err := reader.retryGETResult(ctx, endpoint, "mcp_tools_read", "http", mcpToolsMaxBody, func(req *http.Request) {
		reader.setGatewayHeaders(req)
		req.Header.Set("Accept", "application/json")
	})
	for _, attempt := range reader.attemptTelemetry {
		c.recordAttempt(attempt)
	}
	if err != nil {
		return nil, response.Status, err
	}
	tools, err := parseWDFMCPTools(response.Body)
	return tools, response.Status, err
}

func wdfMCPToolsURL(instanceURL, serverID string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(instanceURL))
	if err != nil || base.Host == "" || base.User != nil || (base.Scheme != "http" && base.Scheme != "https") ||
		(base.Path != "" && base.Path != "/") || base.RawQuery != "" || base.Fragment != "" {
		return "", errors.New("invalid instance URL for WDF tool discovery")
	}
	if serverID == "" || len(serverID) > 512 || !utf8.ValidString(serverID) || serverID == "." || serverID == ".." ||
		strings.ContainsAny(serverID, "/\\%") || strings.IndexFunc(serverID, unicode.IsControl) >= 0 {
		return "", errors.New("invalid WDF server ID")
	}
	base.Path = wdfMCPServersAPIPath + serverID + "/tools"
	base.RawPath = wdfMCPServersAPIPath + url.PathEscape(serverID) + "/tools"
	return base.String(), nil
}

// The current WebUI reads response.data.tools (ServiceNow REST result.tools).
// Preserve every schema field, including annotations and future extensions.
// The tools route is not paginated in that contract; pagination belongs to
// the server discovery route, which is handled before selecting server IDs.
func parseWDFMCPTools(body []byte) ([]map[string]interface{}, error) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(body, &response); err != nil || response == nil {
		return nil, errors.New("invalid WDF tools response")
	}
	for depth := 0; depth < 4; depth++ {
		if raw, ok := response["tools"]; ok {
			if !isJSONArray(raw) {
				return nil, errors.New("invalid WDF tools array")
			}
			tools := []map[string]interface{}{}
			if err := json.Unmarshal(raw, &tools); err != nil {
				return nil, errors.New("invalid WDF tool schema")
			}
			for _, tool := range tools {
				if tool == nil {
					return nil, errors.New("invalid WDF tool schema")
				}
			}
			return tools, nil
		}
		var nested map[string]json.RawMessage
		raw := response["result"]
		if len(raw) == 0 {
			raw = response["data"]
		}
		if err := json.Unmarshal(raw, &nested); err != nil || nested == nil {
			break
		}
		response = nested
	}
	return nil, errors.New("WDF response does not contain tool schemas")
}
