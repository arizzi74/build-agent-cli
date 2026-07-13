package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPersistRichWebMessageContentPostsExactEndpointPayloadAndParsesSysID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/sn_build_agent/build_agent_api/conversations/conv123/messages" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		wantContent := `{"id":"tool-row","sender":"assistant-tool","toolName":"Read File","toolActualName":"fs_read_file","complete":true,"duration":82,"toolUseId":"c-tool","startTime":"2026-07-09T09:19:30.371Z","requiresApproval":false,"requiresSelection":false,"toolInput":{"path":"src/fluent/x.now.ts"},"success":true,"result":"file contents"}`
		wantBody := `{"content":"` + escapeJSONStringForTest(wantContent) + `"}`
		if string(body) != wantBody {
			t.Fatalf("body = %s\nwant = %s", body, wantBody)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"result":{"content":"ignored","conversation":"conv123","sysId":"server-sys-id"}}`))
	}))
	defer server.Close()

	client := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	sysID, err := client.PersistRichWebMessageContent(context.Background(), "conv123", NewRichAssistantToolContent(RichAssistantToolContentOptions{
		ID:              "tool-row",
		ToolName:        "Read File",
		ToolActualName:  "fs_read_file",
		ToolUseID:       "c-tool",
		StartTimeString: "2026-07-09T09:19:30.371Z",
		DurationMS:      82,
		ToolInput:       map[string]interface{}{"path": "src/fluent/x.now.ts"},
		Success:         true,
		Result:          "file contents",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !sawRequest {
		t.Fatal("server did not receive request")
	}
	if sysID != "server-sys-id" {
		t.Fatalf("sysID = %q", sysID)
	}
}

func TestRichAssistantThinkingAndFinalContentMatchObservedShape(t *testing.T) {
	thinking := NewRichAssistantThinkingContentMS("think-id", "hidden reasoning", 1598, "2026-07-09T09:19:28.770Z")
	raw, err := json.Marshal(thinking)
	if err != nil {
		t.Fatal(err)
	}
	wantThinking := `{"id":"think-id","text":"hidden reasoning","complete":true,"duration":1598,"sender":"assistant-thinking","startTime":"2026-07-09T09:19:28.770Z"}`
	if string(raw) != wantThinking {
		t.Fatalf("thinking content = %s\nwant = %s", raw, wantThinking)
	}

	start := time.Date(2026, 7, 9, 9, 21, 29, 894*int(time.Millisecond), time.UTC)
	final := NewRichAssistantFinalContent("final-id", "done", 3081*time.Millisecond, start)
	raw, err = json.Marshal(final)
	if err != nil {
		t.Fatal(err)
	}
	wantFinal := `{"id":"final-id","text":"done","complete":true,"duration":3081,"sender":"assistant","startTime":"2026-07-09T09:21:29.894Z"}`
	if string(raw) != wantFinal {
		t.Fatalf("final content = %s\nwant = %s", raw, wantFinal)
	}
}

func TestRichAssistantToolContentMatchesObservedShape(t *testing.T) {
	content := NewRichAssistantToolContent(RichAssistantToolContentOptions{
		ID:                "5ded6031-2163-4542-ab49-0967fec692c0",
		ToolName:          "Build",
		ToolActualName:    "build",
		ToolUseID:         "c-0aefc923-f497-4278-ba48-4a460714dc49",
		StartTimeString:   "2026-07-09T09:19:33.749Z",
		DurationMS:        90850,
		RequiresApproval:  false,
		RequiresSelection: false,
		ToolInput:         map[string]interface{}{"path": "."},
		Success:           true,
		Result:            "ServiceNow application Lima built successfully!",
	})
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"5ded6031-2163-4542-ab49-0967fec692c0","sender":"assistant-tool","toolName":"Build","toolActualName":"build","complete":true,"duration":90850,"toolUseId":"c-0aefc923-f497-4278-ba48-4a460714dc49","startTime":"2026-07-09T09:19:33.749Z","requiresApproval":false,"requiresSelection":false,"toolInput":{"path":"."},"success":true,"result":"ServiceNow application Lima built successfully!"}`
	if string(raw) != want {
		t.Fatalf("tool content = %s\nwant = %s", raw, want)
	}
}

func TestParseRichWebMessageSysIDRobustShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"camel result", `{"result":{"sysId":"camel"}}`, "camel"},
		{"snake result", `{"result":{"sys_id":"snake"}}`, "snake"},
		{"top level", `{"sysID":"top"}`, "top"},
		{"array result", `{"result":[{"sys_id":"arr"}]}`, "arr"},
		{"nested json string", `{"result":"{\"sysId\":\"nested\"}"}`, "nested"},
		{"deep fallback", `{"outer":{"inner":{"sysid":"deep"}}}`, "deep"},
		{"empty", ``, ""},
		{"malformed", `{not json`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRichWebMessageSysID([]byte(tt.body)); got != tt.want {
				t.Fatalf("sysID = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRichMessageContentJSONStringAcceptsArbitraryContent(t *testing.T) {
	got, err := richMessageContentJSONString(map[string]interface{}{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"text":"hello"}` {
		t.Fatalf("map content = %q", got)
	}

	got, err = richMessageContentJSONString(`{"text":"already-json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"text":"already-json"}` {
		t.Fatalf("string content = %q", got)
	}
}

func escapeJSONStringForTest(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw[1 : len(raw)-1])
}
