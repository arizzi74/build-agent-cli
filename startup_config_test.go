package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestLoadWebStartupConfigFetchesExactEndpointsAndParses(t *testing.T) {
	wantEndpoints := []string{
		"/api/sn_build_agent/build_agent_api/getVariant",
		"/api/sn_build_agent/build_agent_health/check",
		"/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/nameINsn_build_agent.use_mock_llm",
		"/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/name%3Dsn_build_agent.nirvana_websocket_url",
		"/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/nameINsn_build_agent.tool.execution.timeouts%2Csn_build_agent.tier_override%2Csn_build_agent.user_prompt_limit%2Csn_build_agent.user_prompt_limit_pdi%2Csn_build_agent.enable_wdf_server_discovery%2Csn_build_agent.use_mock_wdf_endpoint%2Cglide.regulated_instance",
		"/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/name%3Dsn_build_agent.properties_cache_ttl_minutes",
		"/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/name%3Dsn_build_agent.tool.execution.timeouts",
		"/api/sn_build_agent/build_agent_api/isProductAvailable",
		"/api/sn_build_agent/skills_api/summary",
		"/api/sn_ba_glide_tools/build_agent_glide_tools_search/getSemanticSearchStatus",
		"/api/sn_glider/applications/ide/fd254d9443a161100967247e6bb8f200/version/update-available",
	}
	var gotEndpoints []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqURI := r.RequestURI
		gotEndpoints = append(gotEndpoints, reqURI)
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch reqURI {
		case wantEndpoints[0]:
			_, _ = w.Write([]byte(`{"result":{"isFreemium":true}}`))
		case wantEndpoints[1]:
			_, _ = w.Write([]byte(`{"result":{"capabilities":{"keyword_search":{"available":true,"details":{"preview_available":true,"indexed_rows":42}}}}}`))
		case wantEndpoints[2]:
			writeStartupPropertyResponse(t, w, []map[string]string{{"name": "sn_build_agent.use_mock_llm", "value": "false"}})
		case wantEndpoints[3]:
			writeStartupPropertyResponse(t, w, []map[string]string{{"name": "sn_build_agent.nirvana_websocket_url", "value": "/custom/nirvana/web-socket"}})
		case wantEndpoints[4]:
			writeStartupPropertyResponse(t, w, []map[string]string{
				{"name": "sn_build_agent.tool.execution.timeouts", "value": `{"run_query":120000,"create_new_servicenow_app":300000,"default":240000}`},
				{"name": "sn_build_agent.tier_override", "value": "paid"},
				{"name": "sn_build_agent.user_prompt_limit", "value": "100"},
				{"name": "sn_build_agent.user_prompt_limit_pdi", "value": "25"},
				{"name": "sn_build_agent.enable_wdf_server_discovery", "value": "true"},
				{"name": "sn_build_agent.use_mock_wdf_endpoint", "value": "false"},
				{"name": "glide.regulated_instance", "value": "true"},
			})
		case wantEndpoints[5]:
			writeStartupPropertyResponse(t, w, []map[string]string{{"name": "sn_build_agent.properties_cache_ttl_minutes", "value": "15"}})
		case wantEndpoints[6]:
			writeStartupPropertyResponse(t, w, []map[string]string{{"name": "sn_build_agent.tool.execution.timeouts", "value": `{"run_query":120000,"create_new_servicenow_app":300000,"default":240000}`}})
		case wantEndpoints[7]:
			_, _ = w.Write([]byte(`{"result":{"available":true}}`))
		case wantEndpoints[8]:
			_, _ = w.Write([]byte(`{"result":{"skills":[{"name":"Build"}]}}`))
		case wantEndpoints[9]:
			_, _ = w.Write([]byte(`{"result":{"enabled":true}}`))
		case wantEndpoints[10]:
			_, _ = w.Write([]byte(`{"result":{"update_available":true,"app_id":"fd254d9443a161100967247e6bb8f200","current_version":"4.2.5","latest_version":"4.3.2"}}`))
		default:
			t.Fatalf("unexpected endpoint %s", reqURI)
		}
	}))
	defer server.Close()

	client := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	cfg := client.loadWebStartupConfig(context.Background())

	if !reflect.DeepEqual(gotEndpoints, wantEndpoints) {
		t.Fatalf("endpoints = %#v, want %#v", gotEndpoints, wantEndpoints)
	}
	if len(cfg.FetchErrors) != 0 {
		t.Fatalf("FetchErrors = %#v, want none", cfg.FetchErrors)
	}
	if !cfg.FreemiumKnown || !cfg.Freemium {
		t.Fatalf("freemium = known:%v value:%v, want known true", cfg.FreemiumKnown, cfg.Freemium)
	}
	keyword := cfg.HealthCaps["keyword_search"]
	if !keyword.Available || keyword.Details["preview_available"] != true || numericValue(keyword.Details["indexed_rows"]) != 42 {
		t.Fatalf("keyword_search health = %#v", keyword)
	}
	if got := cfg.Properties["sn_build_agent.tier_override"]; got != "paid" {
		t.Fatalf("tier override = %q", got)
	}
	if got := cfg.Properties["sn_build_agent.use_mock_llm"]; got != "false" {
		t.Fatalf("use_mock_llm = %q", got)
	}
	if got := cfg.Properties["glide.regulated_instance"]; got != "true" {
		t.Fatalf("regulated instance = %q", got)
	}
	wantWS := "ws://" + server.Listener.Addr().String() + "/custom/nirvana/web-socket"
	if cfg.WebSocketURL != wantWS {
		t.Fatalf("WebSocketURL = %q, want %q", cfg.WebSocketURL, wantWS)
	}
	if client.cfg.WSURL != wantWS {
		t.Fatalf("client cfg WSURL = %q, want applied %q", client.cfg.WSURL, wantWS)
	}
	if got := cfg.ToolTimeouts["run_query"]; got != 120*time.Second {
		t.Fatalf("run_query timeout = %s", got)
	}
	if got := cfg.DefaultTimeout; got != 240*time.Second {
		t.Fatalf("default timeout = %s", got)
	}
	if cfg.PropertiesTTL != 15*time.Minute || !cfg.ProductKnown || !cfg.ProductAvailable || cfg.SkillsSummary == nil || cfg.SemanticSearch == nil {
		t.Fatalf("extended startup config not parsed: %#v", cfg)
	}
	if got, ok := cfg.ToolTimeout("missing_tool"); !ok || got != 240*time.Second {
		t.Fatalf("fallback ToolTimeout = %s, %v", got, ok)
	}
	if !cfg.UpdateInfo.UpdateAvailable || cfg.UpdateInfo.CurrentVersion != "4.2.5" || cfg.UpdateInfo.LatestVersion != "4.3.2" || cfg.UpdateInfo.AppID != webStartupIDEAppID {
		t.Fatalf("UpdateInfo = %#v", cfg.UpdateInfo)
	}
}

func TestLoadWebStartupConfigBestEffortAndContextAware(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqURI := r.RequestURI
		mu.Lock()
		seen[reqURI]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch reqURI {
		case "/api/sn_build_agent/build_agent_api/getVariant":
			_, _ = w.Write([]byte(`{"result":{"isFreemium":false}}`))
		case "/api/sn_build_agent/build_agent_health/check":
			http.Error(w, "temporary health failure", http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(`{"result":{"query_results":"No matching records found."}}`))
		}
	}))
	defer server.Close()

	client := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	cfg := client.loadWebStartupConfig(context.Background())
	if !cfg.FreemiumKnown || cfg.Freemium {
		t.Fatalf("freemium = known:%v value:%v, want known false", cfg.FreemiumKnown, cfg.Freemium)
	}
	if cfg.FetchErrors["health"] == "" {
		t.Fatalf("health fetch error was not recorded: %#v", cfg.FetchErrors)
	}
	if len(seen) != len(webStartupEndpoints()) {
		t.Fatalf("best effort fetched %d endpoints, want %d", len(seen), len(webStartupEndpoints()))
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	seen = map[string]int{}
	cfg = client.loadWebStartupConfig(cancelled)
	if len(seen) != 0 {
		t.Fatalf("cancelled context still fetched endpoints: %#v", seen)
	}
	if len(cfg.FetchErrors) == 0 {
		t.Fatalf("cancelled context did not record an error")
	}
}

func writeStartupPropertyResponse(t *testing.T, w http.ResponseWriter, rows []map[string]string) {
	t.Helper()
	records := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		records = append(records, map[string]interface{}{"name": row["name"], "value": row["value"]})
	}
	rawRecords, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"query_results": string(rawRecords),
			"num_results":   len(records),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(payload)
}
