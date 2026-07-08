package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseWebConversationsAndMessages(t *testing.T) {
	conversations := parseWebConversations([]byte(`{"result":[{"conversation_id":"conv2","title":"Second","state":"completed","sys_updated_on":"2026-07-06 08:00:00"},{"sys_id":"conv1","title":"First","state":"active"}]}`))
	if len(conversations) != 2 {
		t.Fatalf("conversations len = %d, want 2: %+v", len(conversations), conversations)
	}
	if conversations[0].ID != "conv2" || conversations[0].Title != "Second" || conversations[0].State != "completed" {
		t.Fatalf("unexpected first conversation: %+v", conversations[0])
	}

	messages := parseWebMessages([]byte(`{"result":[{"sequence":2,"role":"assistant","content":"{\"text\":\"hello there\"}"},{"sequence":1,"role":"user","content":"{\"text\":\"hi\"}"}]}`))
	want := []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
		map[string]interface{}{"role": "assistant", "content": "hello there"},
	}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("messages = %#v, want %#v", messages, want)
	}

	messages = parseWebMessages([]byte(`{"result":[{"sequence":1,"content":"{\"sender\":\"user\",\"text\":\"ask\"}"},{"sequence":2,"content":"{\"sender\":\"assistant-thinking\",\"text\":\"hidden reasoning\"}"},{"sequence":3,"content":"{\"sender\":\"assistant-tool\",\"text\":\"tool output\"}"},{"sequence":4,"content":"{\"sender\":\"assistant\",\"text\":\"final answer\"}"}]}`))
	want = []interface{}{
		map[string]interface{}{"role": "user", "content": "ask"},
		map[string]interface{}{"role": "assistant", "content": "final answer"},
	}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("filtered messages = %#v, want %#v", messages, want)
	}

	messages = parseWebMessages([]byte(`{"result":[{"sequence":10,"content":"{\"text\":\"bare prompt\"}"},{"sequence":11,"content":"{\"text\":\"bare answer\"}"},{"sequence":12,"content":"{\"text\":\"second prompt\"}"},{"sequence":13,"content":"{\"text\":\"second answer\"}"}]}`))
	want = []interface{}{
		map[string]interface{}{"role": "user", "content": "bare prompt"},
		map[string]interface{}{"role": "assistant", "content": "bare answer"},
		map[string]interface{}{"role": "user", "content": "second prompt"},
		map[string]interface{}{"role": "assistant", "content": "second answer"},
	}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("bare messages = %#v, want %#v", messages, want)
	}
}

func TestConversationLabelIsSingleLine(t *testing.T) {
	label := conversationLabel(WebConversation{ID: "abcdef1234567890", Title: "Global: CMDB\nAssessment   Prompt", State: "open"})
	if strings.Contains(label, "\n") || strings.Contains(label, "  Assessment") || !strings.Contains(label, "Global: CMDB Assessment Prompt") {
		t.Fatalf("label was not compacted: %q", label)
	}
}

func TestSendGatewayMessageMirrorsWebUIConversationPersistence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "cabf08f350d642268fc809f8f6b1e54e"
	var paths []string
	var persistedMessage bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_ba_core/conversations_api/conversation/" + conversationID,
			"/api/sn_build_agent/conversations_api/conversation/" + conversationID,
			"/api/sn_build_agent/build_agent_api/conversations/" + conversationID:
			http.NotFound(w, r)
		case "/api/now/table/sn_ba_core_conversation/" + conversationID,
			"/api/now/table/sn_build_agent_conversation/" + conversationID:
			http.Error(w, `{"error":{"message":"Invalid table"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_ba_core/conversations_api/create":
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			inner := asMap(payload["payload"])
			if inner["conversationId"] != conversationID {
				t.Fatalf("create conversationId = %v, want %s", inner["conversationId"], conversationID)
			}
			if inner["title"] != "hello" {
				t.Fatalf("create title = %v, want hello", inner["title"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"sys_id":"` + conversationID + `","title":"hello","state":"active"}}`))
		case "/api/sn_ba_core/conversations_api/conversation/" + conversationID + "/message":
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			inner := asMap(payload["payload"])
			if inner["role"] != "user" || inner["message_type"] != "text" {
				t.Fatalf("unexpected message payload: %+v", inner)
			}
			var content map[string]interface{}
			if err := json.Unmarshal([]byte(stringify(inner["content"])), &content); err != nil {
				t.Fatal(err)
			}
			if content["sender"] != "user" || content["text"] != "hello" || content["id"] == "" || content["hasCheckpoints"] != false {
				t.Fatalf("persisted content is not Glider-compatible: %+v", content)
			}
			if got := messageContentText(stringify(inner["content"])); got != "hello" {
				t.Fatalf("persisted content = %q, want hello", got)
			}
			persistedMessage = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"sys_id":"msg1"}}`))
		case "/api/sn_ba_core/agent_gateway_api/conversation":
			if !persistedMessage {
				t.Fatal("gateway called before user message was persisted")
			}
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			inner := asMap(payload["payload"])
			if inner["conversationId"] != conversationID || inner["content"] != "hello" {
				t.Fatalf("unexpected gateway payload: %+v", inner)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":"accepted"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{
		cfg:            CLIConfig{InstanceURL: server.URL},
		opts:           Options{Profile: "default", AuthMode: authModeCookie},
		httpClient:     server.Client(),
		gatewayAuth:    authModeCookie,
		ambClient:      "amb-client",
		ambChannel:     "/build_agent_core/stream/" + conversationID,
		ambSubscribed:  true,
		conversationID: conversationID,
	}
	if err := client.sendGatewayMessage(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if !client.serverConversation || client.conversationTitle != "hello" {
		t.Fatalf("conversation was not marked server-backed: server=%v title=%q", client.serverConversation, client.conversationTitle)
	}
	joined := strings.Join(paths, "\n")
	if !strings.Contains(joined, "POST /api/sn_ba_core/conversations_api/create") || !strings.Contains(joined, "POST /api/sn_ba_core/conversations_api/conversation/"+conversationID+"/message") || !strings.Contains(joined, "POST /api/sn_ba_core/agent_gateway_api/conversation") {
		t.Fatalf("missing expected paths:\n%s", joined)
	}
}

func TestSendGatewayMessageSubscribesToAMBBeforeGatewayPost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "cabf08f350d642268fc809f8f6b1e54e"
	var mu sync.Mutex
	var subscribedBeforeGateway bool
	var sawGateway bool
	var sawSubscribe bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/amb":
			raw, _ := io.ReadAll(r.Body)
			body := string(raw)
			if strings.Contains(body, "/meta/subscribe") {
				if strings.Contains(body, "/build_agent_core/stream/") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`[{"successful":false,"error":"404::message_deleted","ext":{"glide.amb.reply.status.message":"No processor matches the provided channel"}}]`))
					return
				}
				mu.Lock()
				sawSubscribe = true
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"successful":true}]`))
				return
			}
			if strings.Contains(body, "/meta/connect") {
				time.Sleep(20 * time.Millisecond)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"successful":true}]`))
		case "/api/sn_ba_core/conversations_api/conversation/" + conversationID + "/message":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"sys_id":"msg1"}}`))
		case "/api/sn_ba_core/agent_gateway_api/conversation":
			mu.Lock()
			subscribedBeforeGateway = sawSubscribe
			sawGateway = true
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":"accepted"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{
		cfg:                CLIConfig{InstanceURL: server.URL},
		opts:               Options{Profile: "default", AuthMode: authModeCookie},
		httpClient:         server.Client(),
		gatewayAuth:        authModeCookie,
		ambURL:             server.URL + "/amb",
		ambClient:          "amb-client",
		ambChannel:         "/build_agent_core/stream/" + conversationID,
		conversationID:     conversationID,
		serverConversation: true,
	}
	defer func() { _ = client.Close() }()
	if err := client.sendGatewayMessage(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawGateway || !subscribedBeforeGateway {
		t.Fatalf("gateway saw subscribed=%v sawGateway=%v sawSubscribe=%v", subscribedBeforeGateway, sawGateway, sawSubscribe)
	}
	if client.ambChannel != "/build_agent/stream/"+conversationID {
		t.Fatalf("ambChannel = %q, want legacy build_agent stream channel", client.ambChannel)
	}
}

func TestConversationCommandUseLoadsServerMessages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "cabf08f350d642268fc809f8f6b1e54e"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_build_agent/build_agent_api/conversations":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_ba_core/conversations_api/conversations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"conversation_id":"` + conversationID + `","title":"Existing chat","state":"active"}]}`))
		case "/api/sn_ba_core/conversations_api/conversation/" + conversationID + "/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sequence":1,"role":"user","content":"{\"text\":\"old prompt\"}"},{"sequence":2,"role":"assistant","content":"{\"text\":\"old answer\"}"}]}`))
		case "/api/now/table/sn_ba_core_conversation", "/api/now/table/sn_build_agent_conversation":
			http.Error(w, `{"error":{"message":"Invalid table"},"status":"failure"}`, http.StatusBadRequest)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "default", AuthMode: authModeCookie})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	client.gatewayAuth = authModeCookie
	if err := handleConversationCommand(context.Background(), client, []string{"use", "1"}); err != nil {
		t.Fatal(err)
	}
	if client.conversationID != conversationID || !client.serverConversation || client.conversationTitle != "Existing chat" {
		t.Fatalf("conversation not selected: id=%q server=%v title=%q", client.conversationID, client.serverConversation, client.conversationTitle)
	}
	if len(client.history) != 2 || asMap(client.history[1])["content"] != "old answer" {
		t.Fatalf("history not loaded: %#v", client.history)
	}
}

func TestBrokenConversationAPIFallsBackForCSRFNull(t *testing.T) {
	body := []byte(`{"error":{"message":"Cannot invoke \"com.glide.rest.security.CSRFEvaluator.applyRotatedTokens()\" because \"this.fCSRFEvaluator\" is null"},"status":"failure"}`)
	if !isConversationAPIMissing(http.StatusInternalServerError, body) {
		t.Fatalf("broken conversation REST API should trigger fallback")
	}
}

func TestConversationAPIFallsBackToTablesOnUnauthorized(t *testing.T) {
	if !shouldFallbackToConversationTables(http.StatusUnauthorized, []byte(`{"error":{"message":"User is not authenticated"}}`)) {
		t.Fatalf("conversation API 401 should trigger table fallback")
	}
	if !shouldFallbackToConversationTables(http.StatusForbidden, []byte(`{"error":{"message":"Forbidden"}}`)) {
		t.Fatalf("conversation API 403 should trigger table fallback")
	}
}

func TestNirvanaConversationCommandUseLoadsServerMessages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "6987b1aceba80750998efcf000d0cdf9"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_build_agent/build_agent_api/conversations":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_ba_core/conversations_api/conversations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"conversation_id":"` + conversationID + `","title":"Nirvana chat","state":"active"}]}`))
		case "/api/sn_ba_core/conversations_api/conversation/" + conversationID + "/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sequence":1,"role":"user","content":"{\"text\":\"old nirvana prompt\"}"},{"sequence":2,"role":"assistant","content":"{\"text\":\"old nirvana answer\"}"}]}`))
		case "/api/now/table/sn_ba_core_conversation", "/api/now/table/sn_build_agent_conversation":
			http.Error(w, `{"error":{"message":"Invalid table"},"status":"failure"}`, http.StatusBadRequest)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "default", Nirvana: true})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	if err := handleConversationCommand(context.Background(), client, []string{"use", "latest"}); err != nil {
		t.Fatal(err)
	}
	if client.conversationID != conversationID || !client.serverConversation || client.conversationTitle != "Nirvana chat" {
		t.Fatalf("conversation not selected: id=%q server=%v title=%q", client.conversationID, client.serverConversation, client.conversationTitle)
	}
	if len(client.history) != 2 || asMap(client.history[1])["content"] != "old nirvana answer" {
		t.Fatalf("history not loaded for Nirvana resume: %#v", client.history)
	}
	payloadHistory := nirvanaConversationHistory(client.history)
	if len(payloadHistory) != 2 || asMap(payloadHistory[0])["role"] != "user" || asMap(payloadHistory[1])["role"] != "assistant" {
		t.Fatalf("Nirvana payload history not reusable: %#v", payloadHistory)
	}
}

func TestListConversationsContinuesPastEmptyIntermediateAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "6987b1aceba80750998efcf000d0cdf9"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_ba_core/conversations_api/conversations":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_build_agent/conversations_api/conversations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_build_agent/build_agent_api/conversations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sys_id":"` + conversationID + `","title":"Build Agent API chat","state":"open"}]}`))
		case "/api/now/table/sn_ba_core_conversation", "/api/now/table/sn_build_agent_conversation":
			http.Error(w, `{"error":{"message":"Invalid table"},"status":"failure"}`, http.StatusBadRequest)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "default", Nirvana: true})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	conversations, err := client.ListWebConversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ID != conversationID || conversations[0].Title != "Build Agent API chat" {
		t.Fatalf("unexpected conversations: %#v", conversations)
	}
}

func TestBuildAgentAPIConversationCreateMatchesWebUIHAR(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	localID := "11111111111141118111111111111111"
	serverID := "c0b016332bb5cf106969f1cc6e91bf3f"
	prompt := "how are you today that I am asking this QUESTION?"
	var sawCreate, sawMessage, sawGateway bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_ba_core/conversations_api/conversation/" + localID,
			"/api/sn_build_agent/conversations_api/conversation/" + localID,
			"/api/sn_build_agent/build_agent_api/conversations/" + localID:
			http.NotFound(w, r)
		case "/api/now/table/sn_ba_core_conversation/" + localID:
			http.Error(w, `{"error":{"message":"Invalid table"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/now/table/sn_build_agent_conversation/" + localID:
			http.NotFound(w, r)
		case "/api/sn_ba_core/conversations_api/create", "/api/sn_build_agent/conversations_api/create":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_ba_core/conversations_api/conversation/" + serverID + "/message",
			"/api/sn_build_agent/conversations_api/conversation/" + serverID + "/message":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_build_agent/build_agent_api/conversations":
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected create method %s", r.Method)
			}
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["payload"] != nil || payload["conversationId"] != nil || payload["title"] != prompt || payload["applicationId"] != nil || payload["applicationName"] != "" || payload["client"] != "ide" {
				t.Fatalf("create payload does not match web UI shape: %+v", payload)
			}
			sawCreate = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"title":"` + prompt + `","applicationId":null,"applicationName":"","client":"ide","sysId":"` + serverID + `"}}`))
		case "/api/sn_build_agent/build_agent_api/conversations/" + serverID + "/messages":
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected message method %s", r.Method)
			}
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["payload"] != nil || payload["role"] != nil || payload["message_type"] != nil || messageContentText(stringify(payload["content"])) != prompt {
				t.Fatalf("message payload does not match web UI shape: %+v", payload)
			}
			sawMessage = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"conversation":"` + serverID + `","sysId":"msg1"}}`))
		case "/api/sn_ba_core/agent_gateway_api/conversation":
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			inner := asMap(payload["payload"])
			if inner["conversationId"] != serverID || inner["content"] != prompt {
				t.Fatalf("gateway used wrong conversation/content: %+v", inner)
			}
			sawGateway = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":"accepted"}`))
		case "/amb":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"successful":true}]`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{
		cfg:            CLIConfig{InstanceURL: server.URL},
		opts:           Options{Profile: "default", AuthMode: authModeCookie},
		httpClient:     server.Client(),
		gatewayAuth:    authModeCookie,
		ambURL:         server.URL + "/amb",
		ambClient:      "amb-client",
		conversationID: localID,
	}
	defer func() { _ = client.Close() }()
	if err := client.sendGatewayMessage(context.Background(), prompt); err != nil {
		t.Fatal(err)
	}
	if !sawCreate || !sawMessage || !sawGateway {
		t.Fatalf("saw create=%v message=%v gateway=%v", sawCreate, sawMessage, sawGateway)
	}
	if client.conversationID != serverID || client.conversationTitle != prompt || !client.serverConversation {
		t.Fatalf("client did not adopt server conversation: id=%q title=%q server=%v", client.conversationID, client.conversationTitle, client.serverConversation)
	}
}

func TestBuildAgentAPIListMergesBackingTableConversations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	apiID := "c0b016332bb5cf106969f1cc6e91bf3f"
	tableOnlyID := "465fcefb2b75cf106969f1cc6e91bf7a"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_ba_core/conversations_api/conversations", "/api/sn_build_agent/conversations_api/conversations":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_build_agent/build_agent_api/conversations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sysId":"` + apiID + `","title":"web created","state":"open"}]}`))
		case "/api/now/table/sn_ba_core_conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/now/table/sn_build_agent_conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sys_id":"` + apiID + `","title":"duplicate"},{"sys_id":"` + tableOnlyID + `","title":"New conversation","state":"open"}]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "default", AuthMode: authModeCookie})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	client.gatewayAuth = authModeCookie
	conversations, err := client.ListWebConversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 2 || conversations[0].ID != apiID || conversations[1].ID != tableOnlyID {
		t.Fatalf("conversations were not merged/de-duped: %#v", conversations)
	}
}

func TestListTableConversationsContinuesPastEmptyCoreTable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	buildAgentID := "0df02ca393794310451bfbb4a903d61d"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/now/table/sn_ba_core_conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/now/table/sn_build_agent_conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sys_id":"` + buildAgentID + `","title":"App Refactor Studio: Analyze the app. DO NOT CHANGE ANYTHING","state":"open"}]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "default", AuthMode: authModeCookie})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	client.gatewayAuth = authModeCookie
	conversations, err := client.listTableConversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ID != buildAgentID || !strings.Contains(conversations[0].Title, "App Refactor Studio") {
		t.Fatalf("did not continue from empty core table to sn_build_agent table: %#v", conversations)
	}
}

func TestListWebConversationsMergesTablesWhenFirstAPIReturnsFilteredRows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	apiID := "ab205dbd3bb7b250d6531d9c73e45a75"
	tableID := "0df02ca393794310451bfbb4a903d61d"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_build_agent/build_agent_api/conversations":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_ba_core/conversations_api/conversations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"conversation_id":"` + apiID + `","title":"can you list me all skills you have","state":"open"}]}`))
		case "/api/now/table/sn_ba_core_conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/now/table/sn_build_agent_conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sys_id":"` + tableID + `","title":"App Refactor Studio: Analyze the app. DO NOT CHANGE ANYTHING","state":"open"},{"sys_id":"` + apiID + `","title":"can you list me all skills you have","state":"open"}]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "default", AuthMode: authModeCookie})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	client.gatewayAuth = authModeCookie
	conversations, err := client.ListWebConversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 2 || conversations[0].ID != apiID || conversations[1].ID != tableID {
		t.Fatalf("API result was not merged with backing table rows: %#v", conversations)
	}
}

func TestBuildAgentAPIConversationListUsesWebUIApplicationIDList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	myExpApp := "9c6e6abc3b91c350d6531d9c73e45a99"
	baAnalyticsApp := "16f4c0513b1d4750d6531d9c73e45a6d"
	appRefactorApp := "2b13ea795f2f4b4991f19ebe67652891"
	noAppID := "ab205dbd3bb7b250d6531d9c73e45a75"
	baID := "fc5540513b1d4750d6531d9c73e45acd"
	myExpID := "506fe6bc3b91c350d6531d9c73e45acc"
	appRefactorID := "0df02ca393794310451bfbb4a903d61d"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_build_agent/build_agent_api/conversations":
			if got := r.URL.Query().Get("application_id_list"); got != myExpApp+","+baAnalyticsApp {
				t.Fatalf("application_id_list = %q", got)
			}
			if got := r.URL.Query().Get("client"); got != "ide" {
				t.Fatalf("client = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[` +
				`{"application_id":null,"client":"ide","title":"can you list me all skills you have","last_message_at":"2026-07-07T05:13:55Z","sys_id":"` + noAppID + `","state":"open"},` +
				`{"application_id":"` + baAnalyticsApp + `","client":"ide","title":"BA Analytics: Are you able to create a dashboard in platform analytics? just...","last_message_at":"2026-07-06T07:30:47Z","sys_id":"` + baID + `","state":"open"},` +
				`{"application_id":"` + myExpApp + `","client":"ide","title":"My Exp Approval: execute Spect/implementation_plan.md","last_message_at":"2026-06-03T16:33:44Z","sys_id":"` + myExpID + `","state":"open"}` +
				`]}`))
		case "/api/now/table/sn_ba_core_conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/now/table/sn_build_agent_conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[` +
				`{"sys_id":"` + appRefactorID + `","title":"App Refactor Studio: Analyze the app. DO NOT CHANGE ANYTHING","application_id":"` + appRefactorApp + `"},` +
				`{"sys_id":"` + baID + `","title":"BA Analytics: Are you able to create a dashboard in platform analytics? just...","application_id":"` + baAnalyticsApp + `"},` +
				`{"sys_id":"` + myExpID + `","title":"My Exp Approval: execute Spect/implementation_plan.md","application_id":"` + myExpApp + `"}` +
				`]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.String())
		}
	}))
	defer server.Close()

	client, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "default", AuthMode: authModeCookie})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	client.gatewayAuth = authModeCookie
	client.workingSet = []interface{}{
		map[string]interface{}{"appDir": myExpApp},
		map[string]interface{}{"appDir": baAnalyticsApp},
	}
	conversations, err := client.ListWebConversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 3 || conversations[0].ID != noAppID || conversations[1].ID != baID || conversations[2].ID != myExpID {
		t.Fatalf("web UI conversation list parity failed: %#v", conversations)
	}
	for _, conv := range conversations {
		if conv.ID == appRefactorID {
			t.Fatalf("out-of-workspace conversation leaked into web UI parity list: %#v", conversations)
		}
	}
}

func TestBuildAgentAPIConversationListKeepsGlobalRowsWhenWorkspaceHasNoAppIDs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	globalID := "aa6fe6bc3b91c350d6531d9c73e45aaa"
	appID := "bb6fe6bc3b91c350d6531d9c73e45bbb"
	appConversationID := "cc6fe6bc3b91c350d6531d9c73e45ccc"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			t.Fatalf("conversation list should not discover every IDE app as a workspace fallback")
		case "/api/sn_build_agent/build_agent_api/conversations":
			if got := r.URL.Query().Get("application_id_list"); got != "" {
				t.Fatalf("application_id_list = %q, want empty for global-only workspace fallback", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[` +
				`{"application_id":null,"client":"ide","title":"List all custom apps in this instance","sys_id":"` + globalID + `","state":"open"},` +
				`{"application_id":"` + appID + `","client":"ide","title":"Other workspace app chat","sys_id":"` + appConversationID + `","state":"open"}` +
				`]}`))
		case "/api/now/table/sn_ba_core_conversation", "/api/now/table/sn_build_agent_conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.String())
		}
	}))
	defer server.Close()

	client, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "default", AuthMode: authModeCookie})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	client.gatewayAuth = authModeCookie
	client.workingSet = []interface{}{}
	conversations, err := client.ListWebConversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ID != globalID {
		t.Fatalf("workspace with no app ids should keep only app-less/global conversations: %#v", conversations)
	}
}

func TestRestoreSavedWebConversationRefreshesWithoutListing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "506fe6bc3b91c350d6531d9c73e45acc"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_ba_core/conversations_api/conversation/" + conversationID,
			"/api/sn_build_agent/conversations_api/conversation/" + conversationID,
			"/api/sn_ba_core/conversations_api/conversation/" + conversationID + "/messages",
			"/api/sn_build_agent/conversations_api/conversation/" + conversationID + "/messages":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_build_agent/build_agent_api/conversations/" + conversationID:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"sys_id":"` + conversationID + `","title":"Saved workspace chat","state":"open","application_id":null}}`))
		case "/api/sn_build_agent/build_agent_api/conversations/" + conversationID + "/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sequence":"1","content":"{\"sender\":\"user\",\"text\":\"old prompt\"}"},{"sequence":"2","content":"{\"sender\":\"assistant\",\"text\":\"old answer\"}"}]}`))
		case "/api/sn_build_agent/build_agent_api/conversations":
			t.Fatalf("startup restore should not list conversations")
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	ws := newWorkspaceState("default")
	ws.ConversationID = conversationID
	ws.ServerConversation = true
	if err := saveWorkspace("default", ws); err != nil {
		t.Fatal(err)
	}
	if err := saveActiveWorkspaceName("default", "default"); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "default", AuthMode: authModeCookie})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	client.gatewayAuth = authModeCookie
	client.restoreSavedWebConversation(context.Background())
	if client.conversationTitle != "Saved workspace chat" || client.conversationState != "open" {
		t.Fatalf("conversation metadata not restored: title=%q state=%q", client.conversationTitle, client.conversationState)
	}
	if len(client.history) != 2 || asMap(client.history[0])["content"] != "old prompt" || asMap(client.history[1])["content"] != "old answer" {
		t.Fatalf("conversation messages not restored: %#v", client.history)
	}
	if saved, ok := loadWorkspace("default", "default"); !ok || saved.ConversationTitle != "Saved workspace chat" || len(saved.ConversationHistory) != 2 {
		t.Fatalf("restored conversation was not saved: %#v ok=%v", saved, ok)
	}
}

func TestBuildAgentAPIConversationBaseFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "fa88a21ceb7dc750998efcf000d0cd6d"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_ba_core/conversations_api/conversations", "/api/sn_build_agent/conversations_api/conversations",
			"/api/sn_ba_core/conversations_api/conversation/" + conversationID,
			"/api/sn_build_agent/conversations_api/conversation/" + conversationID,
			"/api/sn_ba_core/conversations_api/conversation/" + conversationID + "/message",
			"/api/sn_build_agent/conversations_api/conversation/" + conversationID + "/message":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_ba_core/conversations_api/conversation/" + conversationID + "/messages",
			"/api/sn_build_agent/conversations_api/conversation/" + conversationID + "/messages":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_build_agent/build_agent_api/conversations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sys_id":"` + conversationID + `","title":"Build Agent API chat","state":"open"}]}`))
		case "/api/now/table/sn_ba_core_conversation", "/api/now/table/sn_build_agent_conversation":
			http.Error(w, `{"error":{"message":"Invalid table"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/sn_build_agent/build_agent_api/conversations/" + conversationID + "/messages":
			if r.Method == http.MethodPost {
				var payload map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload["payload"] != nil || payload["role"] != nil || payload["message_type"] != nil || messageContentText(stringify(payload["content"])) != "new prompt" {
					t.Fatalf("unexpected build_agent_api append payload: %+v", payload)
				}
				var content map[string]interface{}
				if err := json.Unmarshal([]byte(stringify(payload["content"])), &content); err != nil {
					t.Fatal(err)
				}
				if content["sender"] != "user" || content["text"] != "new prompt" || content["id"] == "" {
					t.Fatalf("build_agent_api content is not Glider-compatible: %+v", content)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":{"sys_id":"msg3"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sequence":"1","content":"{\"sender\":\"user\",\"text\":\"old prompt\"}"},{"sequence":"2","content":"{\"sender\":\"assistant\",\"text\":\"old answer\"}"}]}`))
		case "/api/sn_build_agent/build_agent_api/conversations/" + conversationID:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"sys_id":"` + conversationID + `","title":"Build Agent API chat","state":"open"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "default", AuthMode: authModeCookie})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	client.gatewayAuth = authModeCookie
	conversations, err := client.ListWebConversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ID != conversationID || conversations[0].Title != "Build Agent API chat" {
		t.Fatalf("unexpected conversations: %+v", conversations)
	}
	messages, err := client.fetchWebConversationMessages(context.Background(), conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || asMap(messages[1])["content"] != "old answer" {
		t.Fatalf("unexpected messages: %#v", messages)
	}
	client.conversationID = conversationID
	if err := client.persistWebUserMessage(context.Background(), "new prompt"); err != nil {
		t.Fatal(err)
	}
}

func TestNirvanaPersistsUserMessageWithGliderContent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "6987b1aceba80750998efcf000d0cdf9"
	var persisted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_ba_core/conversations_api/conversation/" + conversationID + "/message":
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			inner := asMap(payload["payload"])
			if inner["role"] != "user" || inner["message_type"] != "text" {
				t.Fatalf("unexpected Nirvana message payload: %+v", inner)
			}
			var content map[string]interface{}
			if err := json.Unmarshal([]byte(stringify(inner["content"])), &content); err != nil {
				t.Fatal(err)
			}
			if content["sender"] != "user" || content["text"] != "hello nirvana" || content["id"] == "" || content["hasCheckpoints"] != false {
				t.Fatalf("unexpected Nirvana content: %+v", content)
			}
			persisted = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"sys_id":"msg1"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{
		cfg:                CLIConfig{InstanceURL: server.URL},
		opts:               Options{Profile: "default", Nirvana: true, AuthMode: authModeCookie},
		httpClient:         server.Client(),
		gatewayAuth:        authModeCookie,
		conversationID:     conversationID,
		serverConversation: true,
	}
	if err := client.persistWebUserMessage(context.Background(), "hello nirvana"); err != nil {
		t.Fatal(err)
	}
	if !persisted {
		t.Fatal("Nirvana user message was not persisted")
	}
}

func TestNirvanaTurnEndPersistsAssistantAndAppendsHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "6987b1aceba80750998efcf000d0cdf9"
	var assistantPersisted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_ba_core/conversations_api/conversation/" + conversationID + "/message":
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			inner := asMap(payload["payload"])
			if inner["role"] != "assistant" || inner["message_type"] != "text" {
				t.Fatalf("unexpected assistant payload: %+v", inner)
			}
			var content map[string]interface{}
			if err := json.Unmarshal([]byte(stringify(inner["content"])), &content); err != nil {
				t.Fatal(err)
			}
			if content["sender"] != "assistant" || content["text"] != "nirvana answer" || content["complete"] != true {
				t.Fatalf("unexpected assistant content: %+v", content)
			}
			assistantPersisted = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"sys_id":"msg2"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{
		cfg:                CLIConfig{InstanceURL: server.URL},
		opts:               Options{Profile: "default", Nirvana: true, AuthMode: authModeCookie},
		httpClient:         server.Client(),
		gatewayAuth:        authModeCookie,
		conversationID:     conversationID,
		serverConversation: true,
		webStreamText:      "nirvana answer",
		pendingUserContent: "nirvana prompt",
		processing:         true,
	}
	if err := client.handleEvent([]byte(`{"type":"turn_end"}`)); err != nil {
		t.Fatal(err)
	}
	if !assistantPersisted {
		t.Fatal("assistant message was not persisted")
	}
	if len(client.history) != 2 || asMap(client.history[0])["role"] != "user" || asMap(client.history[1])["role"] != "assistant" {
		t.Fatalf("history not appended: %#v", client.history)
	}
	if client.pendingUserContent != "" || client.webStreamText != "" || client.processing {
		t.Fatalf("turn state not reset: pending=%q stream=%q processing=%v", client.pendingUserContent, client.webStreamText, client.processing)
	}
}

func TestConversationTableFallbackListsAndPersistsMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "fa88a21ceb7dc750998efcf000d0cd6d"
	var persisted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_ba_core/conversations_api/conversations", "/api/sn_build_agent/conversations_api/conversations", "/api/sn_build_agent/build_agent_api/conversations":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/now/table/sn_ba_core_conversation", "/api/now/table/sn_ba_core_message":
			http.Error(w, `{"error":{"message":"Invalid table sn_ba_core"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/now/table/sn_ba_core_conversation/" + conversationID:
			http.Error(w, `{"error":{"message":"Invalid table sn_ba_core"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/now/table/sn_build_agent_conversation":
			if r.Method != http.MethodGet {
				t.Fatalf("unexpected method for conversation table: %s", r.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[{"sys_id":"` + conversationID + `","title":"Existing table chat","state":"open","sys_updated_on":"2026-07-06 09:00:00"}]}`))
		case "/api/now/table/sn_build_agent_conversation/" + conversationID:
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":{"sys_id":"` + conversationID + `","title":"Existing table chat","state":"open"}}`))
			case http.MethodPatch:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":{"sys_id":"` + conversationID + `"}}`))
			default:
				t.Fatalf("unexpected method for conversation record: %s", r.Method)
			}
		case "/api/now/table/sn_build_agent_message":
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":[{"sequence":"4"}]}`))
			case http.MethodPost:
				var payload map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload["conversation"] != conversationID || intFromAny(payload["sequence"]) != 5 || payload["message_id"] == "" {
					t.Fatalf("unexpected table message payload: %+v", payload)
				}
				var content map[string]interface{}
				if err := json.Unmarshal([]byte(stringify(payload["content"])), &content); err != nil {
					t.Fatal(err)
				}
				if content["sender"] != "user" || content["text"] != "hello table" {
					t.Fatalf("unexpected table content: %+v", content)
				}
				persisted = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":{"sys_id":"msg1"}}`))
			default:
				t.Fatalf("unexpected method for message table: %s", r.Method)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "default", AuthMode: authModeCookie}, httpClient: server.Client(), gatewayAuth: authModeCookie, conversationID: conversationID}
	conversations, err := client.ListWebConversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ID != conversationID || conversations[0].Title != "Existing table chat" {
		t.Fatalf("unexpected table conversations: %+v", conversations)
	}
	if err := client.persistTableUserMessage(context.Background(), "hello table"); err != nil {
		t.Fatal(err)
	}
	if !persisted {
		t.Fatal("message was not persisted through table fallback")
	}
}

func TestLegacyFallbackPersistsAssistantMessageToTableConversation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conversationID := "fa88a21ceb7dc750998efcf000d0cd6d"
	lastSequence := 4
	var userPersisted, assistantPersisted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/applications/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":[]}`))
		case "/api/sn_ba_core/conversations_api/conversation/" + conversationID,
			"/api/sn_build_agent/conversations_api/conversation/" + conversationID,
			"/api/sn_build_agent/build_agent_api/conversations/" + conversationID,
			"/api/sn_ba_core/conversations_api/conversation/" + conversationID + "/message",
			"/api/sn_build_agent/conversations_api/conversation/" + conversationID + "/message",
			"/api/sn_build_agent/build_agent_api/conversations/" + conversationID + "/messages":
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/now/table/sn_ba_core_conversation", "/api/now/table/sn_ba_core_message",
			"/api/now/table/sn_ba_core_conversation/" + conversationID:
			http.Error(w, `{"error":{"message":"Invalid table sn_ba_core"},"status":"failure"}`, http.StatusBadRequest)
		case "/api/now/table/sn_build_agent_conversation/" + conversationID:
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":{"sys_id":"` + conversationID + `","title":"Existing table chat","state":"open"}}`))
			case http.MethodPatch:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":{"sys_id":"` + conversationID + `"}}`))
			default:
				t.Fatalf("unexpected method for conversation record: %s", r.Method)
			}
		case "/api/now/table/sn_build_agent_message":
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":[{"sequence":"` + strconv.Itoa(lastSequence) + `"}]}`))
			case http.MethodPost:
				var payload map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload["conversation"] != conversationID || intFromAny(payload["sequence"]) != lastSequence+1 || payload["message_id"] == "" {
					t.Fatalf("unexpected table message payload: %+v", payload)
				}
				lastSequence = intFromAny(payload["sequence"])
				var content map[string]interface{}
				if err := json.Unmarshal([]byte(stringify(payload["content"])), &content); err != nil {
					t.Fatal(err)
				}
				switch content["sender"] {
				case "user":
					if content["text"] != "hello legacy" {
						t.Fatalf("unexpected user content: %+v", content)
					}
					userPersisted = true
				case "assistant":
					if content["text"] != "legacy answer" || content["complete"] != true {
						t.Fatalf("unexpected assistant content: %+v", content)
					}
					assistantPersisted = true
				default:
					t.Fatalf("unexpected sender content: %+v", content)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":{"sys_id":"msg1"}}`))
			default:
				t.Fatalf("unexpected method for message table: %s", r.Method)
			}
		case "/api/sn_ba_core/agent_gateway_api/conversation":
			if !userPersisted {
				t.Fatal("gateway called before user message was persisted")
			}
			http.Error(w, `{"error":{"message":"Requested URI does not represent any resource"}}`, http.StatusBadRequest)
		case "/api/sn_build_agent/build_agent_api/send":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"capabilities":{"answer":{"response":"legacy answer"}}}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{
		cfg:            CLIConfig{InstanceURL: server.URL},
		opts:           Options{Profile: "default", AuthMode: authModeCookie},
		httpClient:     server.Client(),
		gatewayAuth:    authModeCookie,
		ambClient:      "amb-client",
		conversationID: conversationID,
	}
	if err := client.sendGatewayMessage(context.Background(), "hello legacy"); err != nil {
		t.Fatal(err)
	}
	if !userPersisted || !assistantPersisted {
		t.Fatalf("persisted user=%v assistant=%v", userPersisted, assistantPersisted)
	}
	if len(client.history) != 2 || asMap(client.history[1])["role"] != "assistant" || asMap(client.history[1])["content"] != "legacy answer" {
		t.Fatalf("history not updated: %#v", client.history)
	}
}
