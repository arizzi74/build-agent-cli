package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPatchAppCreatedCheckpointMatchesWebUI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/sn_build_agent/build_agent_api/conversations/conv/messages/msg" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		want := `{"content":"{\"id\":\"user-id\",\"sender\":\"user\",\"text\":\"create app\",\"hasCheckpoints\":true,\"checkpoints\":[{\"id\":\"APP_CREATED:app123\",\"appDir\":\"Demo App\"}]}"}`
		if string(body) != want {
			t.Fatalf("body = %s\nwant = %s", body, want)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, conversationID: "conv", httpClient: server.Client()}
	if err := c.PatchAppCreatedCheckpoint(context.Background(), "msg", NewRichUserContent("user-id", "create app"), "app123", "Demo App"); err != nil {
		t.Fatal(err)
	}
}
