package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadMatchingWebSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session := WebSession{
		InstanceURL:  "https://dev123.service-now.com/",
		AuthMode:     authModeCookie,
		CookieHeader: "JSESSIONID=abc",
		CreatedAt:    time.Now(),
	}
	if err := saveWebSession("default", session); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadMatchingWebSession("default", "https://dev123.service-now.com"); !ok {
		t.Fatal("expected matching session")
	}
	if _, ok := loadMatchingWebSession("default", "https://other.service-now.com"); ok {
		t.Fatal("did not expect session for another instance")
	}
}

func TestConfigureCookieAuthReusesSavedSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var sawCookie, sawToken bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sn_build_agent/build_agent_api/providerConfig" {
			t.Fatalf("unexpected validation path %s", r.URL.Path)
		}
		sawCookie = strings.Contains(r.Header.Get("Cookie"), "JSESSIONID=abc")
		sawToken = r.Header.Get("X-UserToken") == "tok"
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"model":"gpt_large","providerUrl":"https://provider","skillId":"skill"}}`))
	}))
	defer server.Close()

	if err := saveWebSession("default", WebSession{
		InstanceURL:  server.URL,
		AuthMode:     authModeCookie,
		CookieHeader: "JSESSIONID=abc",
		UserToken:    "tok",
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	client := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "default", AuthMode: authModeCookie}}
	if err := client.configureGatewayAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sawCookie || !sawToken {
		t.Fatalf("validation did not receive saved cookie/token: cookie=%v token=%v", sawCookie, sawToken)
	}
	if client.sessionCookieHeader != "JSESSIONID=abc" || client.userToken != "tok" {
		t.Fatalf("saved session not applied: cookie=%q token=%q", client.sessionCookieHeader, client.userToken)
	}
}

func TestValidateWebSessionRejectsLoginPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<form><input name="user_name"><input name="user_password"><button>login</button></form>`))
	}))
	defer server.Close()

	client := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "default", AuthMode: authModeCookie}, gatewayAuth: authModeCookie}
	if err := client.initGatewayHTTPClient(); err != nil {
		t.Fatal(err)
	}
	client.applyWebSession(WebSession{InstanceURL: server.URL, CookieHeader: "JSESSIONID=stale"})
	if err := client.validateWebSession(context.Background()); !errors.Is(err, errInvalidWebSession) {
		t.Fatalf("validateWebSession error = %v, want errInvalidWebSession", err)
	}
}

func TestConfigureCookieAuthRejectsAndDoesNotSaveInvalidNewCookie(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<form><input name="user_name"><input name="user_password"><button>login</button></form>`))
	}))
	defer server.Close()

	withTestStdin(t, "Cookie: JSESSIONID=bad\ntok\n", func() {
		client := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "default", AuthMode: authModeCookie}}
		err := client.configureGatewayAuth(context.Background())
		if err == nil || !strings.Contains(err.Error(), "provided cookie session is not authenticated") {
			t.Fatalf("configureGatewayAuth error = %v, want invalid new cookie", err)
		}
		if client.sessionCookieHeader != "" || client.userToken != "" {
			t.Fatalf("invalid cookie remained applied: cookie=%q token=%q", client.sessionCookieHeader, client.userToken)
		}
		if _, ok := loadWebSession("default"); ok {
			t.Fatal("invalid newly provided cookie session was saved")
		}
	})
}

func TestSetGatewayHeadersIncludesSavedSession(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://example.service-now.com/api", nil)
	client := &Client{
		cfg:                 CLIConfig{InstanceURL: "https://example.service-now.com"},
		gatewayAuth:         authModeCookie,
		sessionCookieHeader: "JSESSIONID=abc",
		userToken:           "tok",
	}
	client.setGatewayHeaders(req)
	if req.Header.Get("Cookie") != "JSESSIONID=abc" {
		t.Fatalf("Cookie header = %q", req.Header.Get("Cookie"))
	}
	if req.Header.Get("X-UserToken") != "tok" {
		t.Fatalf("X-UserToken = %q", req.Header.Get("X-UserToken"))
	}
	if req.Header.Get("X-Requested-With") != "XMLHttpRequest" {
		t.Fatalf("X-Requested-With = %q", req.Header.Get("X-Requested-With"))
	}
}

func TestPostBuildAgentMessageFallsBackToLegacySend(t *testing.T) {
	var sawPrimary, sawLegacy bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_ba_core/agent_gateway_api/conversation":
			sawPrimary = true
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"}}`, http.StatusBadRequest)
		case "/api/sn_build_agent/build_agent_api/send":
			sawLegacy = true
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			inner := asMap(payload["payload"])
			if inner["requestId"] != "conv123" || inner["message"] != "hello" || inner["appScope"] != "x_app" {
				t.Fatalf("unexpected legacy payload: %+v", inner)
			}
			messages, ok := inner["messages"].([]interface{})
			if !ok || len(messages) != 1 || asMap(messages[0])["role"] != "user" || asMap(messages[0])["content"] != "hello" {
				t.Fatalf("unexpected legacy messages: %+v", inner["messages"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"capabilities":{"answer":{"response":"ok"}}}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{
		cfg:            CLIConfig{InstanceURL: server.URL},
		httpClient:     server.Client(),
		gatewayAuth:    authModeCookie,
		conversationID: "conv123",
		appScope:       "x_app",
	}
	body, needsStream, err := client.postBuildAgentMessage(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if needsStream {
		t.Fatal("legacy fallback should not require AMB streaming")
	}
	if !sawPrimary || !sawLegacy || !strings.Contains(string(body), "ok") {
		t.Fatalf("fallback failed: primary=%v legacy=%v body=%s", sawPrimary, sawLegacy, body)
	}
}

func TestLegacyBuildAgentMessagesIncludesHistory(t *testing.T) {
	client := &Client{history: []interface{}{
		map[string]interface{}{"role": "user", "content": "first"},
		map[string]interface{}{"role": "assistant", "content": "second"},
		map[string]interface{}{"role": "tool", "content": "tool output must not be sent as chat role"},
		map[string]interface{}{"role": "assistant-thinking", "content": "private thinking must not be replayed"},
		map[string]interface{}{"type": "debug", "content": "ignored"},
	}}
	messages := client.legacyBuildAgentMessages("third")
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3: %+v", len(messages), messages)
	}
	if messages[0]["role"] != "user" || messages[0]["content"] != "first" || messages[2]["content"] != "third" {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	for _, msg := range messages {
		if msg["role"] != "user" && msg["role"] != "assistant" && msg["role"] != "system" {
			t.Fatalf("legacy message has unsupported role: %+v", msg)
		}
	}
}

func TestPrintLegacyBuildAgentResponseReturnsCapabilityError(t *testing.T) {
	client := &Client{}
	body := []byte(`{"result":{"status":"completed","capabilities":{"cap":{"error":"Bedrock rejected messages.4.member.role tool","errorCode":1}}}}`)
	err := client.printLegacyBuildAgentResponse(context.Background(), body, "hello")
	if err == nil || !strings.Contains(err.Error(), "Bedrock rejected") {
		t.Fatalf("expected capability error, got %v", err)
	}
}

func TestPostBuildAgentMessageLegacyAuthErrorIsModeAware(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_ba_core/agent_gateway_api/conversation":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"}}`, http.StatusBadRequest)
		case "/api/sn_build_agent/build_agent_api/send":
			http.Error(w, `{"error":{"message":"User is not authenticated"}}`, http.StatusUnauthorized)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client(), gatewayAuth: authModeCookie, conversationID: "conv123"}
	_, _, err := client.postBuildAgentMessage(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "saved web session") || !strings.Contains(err.Error(), "--logout") {
		t.Fatalf("unexpected auth error: %v", err)
	}
}

func withTestStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	oldStdin := os.Stdin
	stdinState.Lock()
	oldReader := stdinState.reader
	stdinState.Unlock()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString(input)
	_ = w.Close()
	os.Stdin = r
	stdinState.Lock()
	stdinState.reader = bufio.NewReader(r)
	stdinState.Unlock()
	defer func() {
		os.Stdin = oldStdin
		stdinState.Lock()
		stdinState.reader = oldReader
		stdinState.Unlock()
		_ = r.Close()
	}()
	fn()
}
