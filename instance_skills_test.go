package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestInstanceSkillsHTTPContracts(t *testing.T) {
	var mu sync.Mutex
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s, want GET", r.Method)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("accept=%q", r.Header.Get("Accept"))
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "user" || password != "pass" {
			t.Errorf("basic auth=%q/%q ok=%v", user, password, ok)
		}
		if body, _ := io.ReadAll(r.Body); len(body) != 0 {
			t.Errorf("unexpected request body=%q", body)
		}
		mu.Lock()
		requests = append(requests, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/sn_build_agent/skills_api/summary":
			_, _ = io.WriteString(w, `{"result":{"skills":[{"name":"alpha","summary":"A"}]}}`)
		case "/api/sn_build_agent/skills_api/build skill/one":
			_, _ = io.WriteString(w, `{"result":{"body":"# alpha"}}`)
		case "/api/sn_build_agent/skills_api/build skill/resources":
			if got := r.URL.Query().Get("filename"); got != "references/guide one.md" {
				t.Errorf("filename=%q", got)
			}
			_, _ = io.WriteString(w, `{"result":{"content":"resource body"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), gatewayAuth: authModeBasic, basicUser: "user", basicPass: "pass"}
	result, status := c.answerInstanceSkillsList(context.Background())
	if status != "complete" || result["content"] != `[{"name":"alpha","summary":"A"}]` {
		t.Fatalf("list status=%q result=%#v", status, result)
	}
	result, status = c.answerInstanceSkillBody(context.Background(), map[string]interface{}{"name": "build skill/one"})
	if status != "complete" || result["content"] != "# alpha" {
		t.Fatalf("body status=%q result=%#v", status, result)
	}
	result, status = c.answerInstanceSkillResource(context.Background(), map[string]interface{}{"skill_name": "build skill", "filename": "references/guide one.md"})
	if status != "complete" || result["content"] != "resource body" {
		t.Fatalf("resource status=%q result=%#v", status, result)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 || requests[0] != "/api/sn_build_agent/skills_api/summary?" || !strings.Contains(requests[1], "build%20skill%2Fone") || requests[2] != "/api/sn_build_agent/skills_api/build%20skill/resources?filename=references%2Fguide+one.md" {
		t.Fatalf("requests=%#v", requests)
	}
}

func TestParseInstanceSkillPayloadResultEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"direct compatibility", `{"skills":[]}`, true},
		{"result object", `{"result":{"skills":[]}}`, true},
		{"missing result field", `{"result":{}}`, true},
		{"null result", `{"result":null}`, false},
		{"array result", `{"result":[]}`, false},
		{"string result", `{"result":"bad"}`, false},
		{"malformed JSON", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := parseInstanceSkillPayload([]byte(tc.body))
			if ok != tc.ok {
				t.Fatalf("parse %s ok=%v, want %v", tc.body, ok, tc.ok)
			}
		})
	}
}

func TestInstanceSkillsRejectMalformedAndUnsafeInputWithoutRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/api/sn_build_agent/skills_api/summary":
			_, _ = io.WriteString(w, `{"result":{"skills":{}}}`)
		case "/api/sn_build_agent/skills_api/valid":
			_, _ = io.WriteString(w, `{"result":{"body":42}}`)
		case "/api/sn_build_agent/skills_api/missing":
			_, _ = io.WriteString(w, `{"result":{}}`)
		case "/api/sn_build_agent/skills_api/nonobject":
			_, _ = io.WriteString(w, `{"result":[]}`)
		case "/api/sn_build_agent/skills_api/valid/resources":
			_, _ = io.WriteString(w, `{"result":{"content":null}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}

	result, status := c.answerInstanceSkillsList(context.Background())
	assertInstanceSkillError(t, result, status, "INSTANCE_SKILLS_ERROR", "Invalid instance skills response")
	result, status = c.answerInstanceSkillBody(context.Background(), map[string]interface{}{"name": "valid"})
	assertInstanceSkillError(t, result, status, "INSTANCE_ERROR", "Failed to fetch skill body: valid")
	result, status = c.answerInstanceSkillBody(context.Background(), map[string]interface{}{"name": "missing"})
	assertInstanceSkillError(t, result, status, "INSTANCE_ERROR", "Failed to fetch skill body: missing")
	result, status = c.answerInstanceSkillBody(context.Background(), map[string]interface{}{"name": "nonobject"})
	assertInstanceSkillError(t, result, status, "INSTANCE_ERROR", "Failed to fetch skill body: nonobject")
	result, status = c.answerInstanceSkillResource(context.Background(), map[string]interface{}{"skill_name": "valid", "filename": "file.md"})
	assertInstanceSkillError(t, result, status, "INSTANCE_ERROR", "Failed to fetch resource: file.md")
	before := calls
	for _, payload := range []map[string]interface{}{
		{},
		{"name": "\n"},
	} {
		result, status = c.answerInstanceSkillBody(context.Background(), payload)
		assertInstanceSkillError(t, result, status, "MISSING_PARAM", "Missing skill name")
	}
	for _, payload := range []map[string]interface{}{
		{},
		{"skill_name": "valid", "filename": "../secret"},
		{"skill_name": "valid", "filename": "references/../secret"},
		{"skill_name": "valid", "filename": "/absolute.md"},
		{"skill_name": "valid", "filename": "references//secret.md"},
		{"skill_name": "valid", "filename": `..\secret`},
		{"skill_name": "valid", "filename": `..\\secret`},
	} {
		result, status = c.answerInstanceSkillResource(context.Background(), payload)
		if payload["filename"] == nil {
			assertInstanceSkillError(t, result, status, "MISSING_PARAM", "Missing skill_name or filename")
		} else {
			assertInstanceSkillError(t, result, status, "INVALID_PARAM", "Invalid skill_name or filename")
		}
	}
	if calls != before {
		t.Fatalf("unsafe/missing input made HTTP request: before=%d after=%d", before, calls)
	}
}

func TestInstanceSkillsSummaryHTTP400IsUnsupportedWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sn_build_agent/skills_api/summary" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"}}`, http.StatusBadRequest)
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	result, status := c.answerInstanceSkillsList(context.Background())
	if status != "complete" || result["content"] != "[]" || stringify(result["warning"]) != "not supported by instance 127" {
		t.Fatalf("400 summary result=%#v status=%q", result, status)
	}
}

func TestInstanceSkillsSummaryNon400RemainsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	result, status := c.answerInstanceSkillsList(context.Background())
	assertInstanceSkillError(t, result, status, "INSTANCE_SKILLS_ERROR", "Failed to fetch instance skills")
}

func TestInstanceSkillsUnsupportedWarningUsesConfiguredInstanceName(t *testing.T) {
	if got, want := instanceSkillsUnsupportedWarning("https://zaiagents.service-now.com"), "not supported by instance zaiagents"; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
}

func TestInstanceSkillsCancelledElicitationStaysSilent(t *testing.T) {
	started := make(chan struct{})
	serverConn := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			serverConn <- conn
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer := <-serverConn
	defer peer.Close()

	parent, cancel := context.WithCancel(context.Background())
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), conn: conn}
	c.beginActiveTurn(parent)
	defer c.endActiveTurn()
	done := make(chan error, 1)
	go func() {
		done <- c.handleElicitation(map[string]interface{}{"elicitation_id": "cancelled", "action": "instance_skills_list", "payload": map[string]interface{}{}})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled elicitation did not return")
	}
	_ = peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var response map[string]interface{}
	if err := peer.ReadJSON(&response); err == nil {
		t.Fatalf("cancelled elicitation sent response: %#v", response)
	}
}

func TestInstanceSkillsDispatchesWithoutUnexpectedClientAction(t *testing.T) {
	responses := make(chan map[string]interface{}, 3)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer conn.Close()
			for i := 0; i < 3; i++ {
				var response map[string]interface{}
				if err := conn.ReadJSON(&response); err != nil {
					t.Errorf("read response: %v", err)
					return
				}
				responses <- response
			}
			return
		}
		switch r.URL.Path {
		case "/api/sn_build_agent/skills_api/summary":
			_, _ = io.WriteString(w, `{"result":{"skills":[]}}`)
		case "/api/sn_build_agent/skills_api/sample":
			_, _ = io.WriteString(w, `{"result":{"body":"body"}}`)
		case "/api/sn_build_agent/skills_api/sample/resources":
			_, _ = io.WriteString(w, `{"result":{"content":"resource"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), conn: conn}
	c.beginActiveTurn(context.Background())
	defer c.endActiveTurn()
	for i, event := range []map[string]interface{}{
		{"elicitation_id": "list", "action": "instance_skills_list", "payload": map[string]interface{}{}},
		{"elicitation_id": "body", "action": "instance_skill_body", "payload": map[string]interface{}{"name": "sample"}},
		{"elicitation_id": "resource", "action": "instance_skill_resource", "payload": map[string]interface{}{"skill_name": "sample", "filename": "guide.md"}},
	} {
		if err := c.handleElicitation(event); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		select {
		case response := <-responses:
			result := asMap(response["result"])
			if !result["success"].(bool) || stringify(asMap(result["error"])["code"]) == "UNEXPECTED_CLIENT_ACTION" {
				t.Fatalf("response=%#v", response)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for dispatch response")
		}
	}
}

func assertInstanceSkillError(t *testing.T, result map[string]interface{}, status, code, message string) {
	t.Helper()
	if status != "error" || stringify(result["code"]) != code || stringify(result["error"]) != message {
		t.Fatalf("status=%q result=%#v want %s/%q", status, result, code, message)
	}
}
