package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const webStartupIDEAppID = "fd254d9443a161100967247e6bb8f200"

type WebStartupConfig struct {
	Freemium         bool
	FreemiumKnown    bool
	Properties       map[string]string
	HealthCaps       map[string]WebHealthCapability
	UpdateInfo       WebUpdateInfo
	WebSocketURL     string
	ToolTimeouts     map[string]time.Duration
	DefaultTimeout   time.Duration
	FetchErrors      map[string]string
	ProductAvailable bool
	ProductKnown     bool
	SkillsSummary    map[string]interface{}
	SemanticSearch   map[string]interface{}
	PropertiesTTL    time.Duration
}

type WebHealthCapability struct {
	Available bool
	Details   map[string]interface{}
}

type WebUpdateInfo struct {
	UpdateAvailable bool
	AppID           string
	CurrentVersion  string
	LatestVersion   string
}

type webStartupEndpoint struct {
	key  string
	path string
}

func (c *Client) loadWebStartupConfig(ctx context.Context) WebStartupConfig {
	cfg := newWebStartupConfig()
	if strings.TrimSpace(c.cfg.InstanceURL) == "" {
		return cfg
	}
	if c.httpClient == nil {
		if err := c.initGatewayHTTPClient(); err != nil {
			cfg.FetchErrors["http_client"] = err.Error()
			return cfg
		}
	}
	if c.sessionCookieHeader == "" && c.userToken == "" && c.gatewayAuth != authModeBasic {
		if session, ok := loadMatchingWebSession(c.opts.Profile, c.cfg.InstanceURL); ok {
			c.applyWebSession(session)
		}
	}

	base := strings.TrimRight(c.cfg.InstanceURL, "/")
	for _, endpoint := range webStartupEndpoints() {
		if err := ctx.Err(); err != nil {
			cfg.FetchErrors[endpoint.key] = err.Error()
			break
		}
		body, _, err := c.getJSON(ctx, base+endpoint.path)
		if err != nil {
			cfg.FetchErrors[endpoint.key] = err.Error()
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if err := cfg.absorbStartupResponse(c.cfg.InstanceURL, endpoint.key, body); err != nil {
			cfg.FetchErrors[endpoint.key] = err.Error()
		}
	}
	cfg.deriveStartupFields(c.cfg.InstanceURL)
	c.applyWebStartupConfig(cfg)
	return cfg
}

func (c *Client) applyWebStartupConfig(cfg WebStartupConfig) {
	if cfg.WebSocketURL != "" && c.opts.WSURL == "" {
		c.cfg.WSURL = cfg.WebSocketURL
	}
}

func newWebStartupConfig() WebStartupConfig {
	return WebStartupConfig{
		Properties:   map[string]string{},
		HealthCaps:   map[string]WebHealthCapability{},
		ToolTimeouts: map[string]time.Duration{},
		FetchErrors:  map[string]string{},
	}
}

func webStartupEndpoints() []webStartupEndpoint {
	return []webStartupEndpoint{
		{key: "variant", path: "/api/sn_build_agent/build_agent_api/getVariant"},
		{key: "health", path: "/api/sn_build_agent/build_agent_health/check"},
		{key: "property_use_mock_llm", path: webStartupPropertyEndpoint("nameIN", []string{"sn_build_agent.use_mock_llm"})},
		{key: "property_nirvana_websocket_url", path: webStartupPropertyEndpoint("name=", []string{"sn_build_agent.nirvana_websocket_url"})},
		{key: "property_bundle", path: webStartupPropertyEndpoint("nameIN", []string{
			"sn_build_agent.tool.execution.timeouts",
			"sn_build_agent.tier_override",
			"sn_build_agent.user_prompt_limit",
			"sn_build_agent.user_prompt_limit_pdi",
			"sn_build_agent.enable_wdf_server_discovery",
			"sn_build_agent.use_mock_wdf_endpoint",
			"glide.regulated_instance",
		})},
		{key: "property_cache_ttl", path: webStartupPropertyEndpoint("name=", []string{"sn_build_agent.properties_cache_ttl_minutes"})},
		{key: "property_timeout", path: webStartupPropertyEndpoint("name=", []string{"sn_build_agent.tool.execution.timeouts"})},
		{key: "product_available", path: "/api/sn_build_agent/build_agent_api/isProductAvailable"},
		{key: "skills_summary", path: "/api/sn_build_agent/skills_api/summary"},
		{key: "semantic_search", path: "/api/sn_ba_glide_tools/build_agent_glide_tools_search/getSemanticSearchStatus"},
		{key: "update_available", path: "/api/sn_glider/applications/ide/" + webStartupIDEAppID + "/version/update-available"},
	}
}

func webStartupPropertyEndpoint(operator string, names []string) string {
	base := "/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/"
	if operator == "name=" && len(names) == 1 {
		return base + "name%3D" + url.PathEscape(names[0])
	}
	query := operator + strings.Join(names, ",")
	return base + url.PathEscape(query)
}

func (cfg *WebStartupConfig) absorbStartupResponse(instanceURL, key string, body []byte) error {
	result, err := startupResult(body)
	if err != nil {
		return err
	}
	switch key {
	case "variant":
		cfg.absorbVariant(result)
	case "health":
		cfg.absorbHealth(result)
	case "property_use_mock_llm", "property_nirvana_websocket_url", "property_bundle", "property_cache_ttl", "property_timeout":
		return cfg.absorbProperties(result)
	case "product_available":
		cfg.ProductAvailable, cfg.ProductKnown = startupBool(firstNonNilStartup(result["isProductAvailable"], result["available"], result["value"], result["result"]))
	case "skills_summary":
		cfg.SkillsSummary = cloneStartupMap(result)
	case "semantic_search":
		cfg.SemanticSearch = cloneStartupMap(result)
	case "update_available":
		cfg.absorbUpdateInfo(result)
	}
	return nil
}

func startupResult(body []byte) (map[string]interface{}, error) {
	var envelope map[string]interface{}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if result := asMap(envelope["result"]); result != nil {
		return result, nil
	}
	return envelope, nil
}

func (cfg *WebStartupConfig) absorbVariant(result map[string]interface{}) {
	if v, ok := startupBool(result["isFreemium"]); ok {
		cfg.Freemium = v
		cfg.FreemiumKnown = true
		return
	}
	if v, ok := startupBool(result["is_freemium"]); ok {
		cfg.Freemium = v
		cfg.FreemiumKnown = true
	}
}

func (cfg *WebStartupConfig) absorbHealth(result map[string]interface{}) {
	capabilities := asMap(result["capabilities"])
	for name, raw := range capabilities {
		capability := asMap(raw)
		if capability == nil {
			continue
		}
		available, _ := startupBool(capability["available"])
		cfg.HealthCaps[name] = WebHealthCapability{
			Available: available,
			Details:   cloneStartupMap(asMap(capability["details"])),
		}
	}
}

func (cfg *WebStartupConfig) absorbProperties(result map[string]interface{}) error {
	records, err := startupPropertyRecords(result)
	if err != nil {
		return err
	}
	for _, record := range records {
		name := strings.TrimSpace(stringify(record["name"]))
		if name == "" {
			name = strings.TrimSpace(stringify(record["sys_name"]))
		}
		if name == "" {
			continue
		}
		cfg.Properties[name] = stringify(record["value"])
	}
	return nil
}

func startupPropertyRecords(result map[string]interface{}) ([]map[string]interface{}, error) {
	if rows, ok := result["query_results"].([]interface{}); ok {
		return startupRowsToMaps(rows), nil
	}
	text := strings.TrimSpace(stringify(result["query_results"]))
	if text == "" || strings.HasPrefix(strings.ToLower(text), "no matching records found") {
		return nil, nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return nil, fmt.Errorf("parse sys_properties query_results: %w", err)
	}
	return rows, nil
}

func startupRowsToMaps(rows []interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		if m := asMap(row); m != nil {
			out = append(out, m)
		}
	}
	return out
}

func (cfg *WebStartupConfig) absorbUpdateInfo(result map[string]interface{}) {
	if v, ok := startupBool(result["update_available"]); ok {
		cfg.UpdateInfo.UpdateAvailable = v
	} else if v, ok := startupBool(result["updateAvailable"]); ok {
		cfg.UpdateInfo.UpdateAvailable = v
	}
	cfg.UpdateInfo.AppID = firstString(result, "app_id", "appId")
	cfg.UpdateInfo.CurrentVersion = firstString(result, "current_version", "currentVersion")
	cfg.UpdateInfo.LatestVersion = firstString(result, "latest_version", "latestVersion")
}

func (cfg *WebStartupConfig) deriveStartupFields(instanceURL string) {
	if raw := strings.TrimSpace(cfg.Properties["sn_build_agent.nirvana_websocket_url"]); raw != "" {
		cfg.WebSocketURL = deriveStartupWebSocketURL(instanceURL, raw)
	}
	if raw := strings.TrimSpace(cfg.Properties["sn_build_agent.tool.execution.timeouts"]); raw != "" {
		cfg.ToolTimeouts = parseStartupToolTimeouts(raw)
		cfg.DefaultTimeout = cfg.ToolTimeouts["default"]
	}
	if raw := strings.TrimSpace(cfg.Properties["sn_build_agent.properties_cache_ttl_minutes"]); raw != "" {
		if minutes := numericValue(raw); minutes > 0 {
			cfg.PropertiesTTL = time.Duration(minutes) * time.Minute
		}
	}
}

func firstNonNilStartup(values ...interface{}) interface{} {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func deriveStartupWebSocketURL(instanceURL, raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.IsAbs() {
		switch parsed.Scheme {
		case "ws", "wss":
			return parsed.String()
		case "http":
			parsed.Scheme = "ws"
			return parsed.String()
		case "https":
			parsed.Scheme = "wss"
			return parsed.String()
		default:
			return ""
		}
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	base, err := url.Parse(strings.TrimRight(instanceURL, "/"))
	if err != nil || base.Host == "" {
		return ""
	}
	scheme := "wss"
	if base.Scheme == "http" {
		scheme = "ws"
	}
	return scheme + "://" + base.Host + value
}

func parseStartupToolTimeouts(raw string) map[string]time.Duration {
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return map[string]time.Duration{}
	}
	out := make(map[string]time.Duration, len(decoded))
	for key, value := range decoded {
		ms := numericValue(value)
		if ms <= 0 {
			continue
		}
		out[key] = time.Duration(ms) * time.Millisecond
	}
	return out
}

func (cfg WebStartupConfig) ToolTimeout(name string) (time.Duration, bool) {
	if cfg.ToolTimeouts != nil {
		if timeout, ok := cfg.ToolTimeouts[name]; ok && timeout > 0 {
			return timeout, true
		}
	}
	if cfg.DefaultTimeout > 0 {
		return cfg.DefaultTimeout, true
	}
	return 0, false
}

func startupBool(v interface{}) (bool, bool) {
	switch typed := v.(type) {
	case bool:
		return typed, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "y":
			return true, true
		case "false", "0", "no", "n":
			return false, true
		}
	case float64:
		if typed == 1 {
			return true, true
		}
		if typed == 0 {
			return false, true
		}
	case int:
		if typed == 1 {
			return true, true
		}
		if typed == 0 {
			return false, true
		}
	}
	return false, false
}

func cloneStartupMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
