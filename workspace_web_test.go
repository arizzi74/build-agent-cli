package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseWebWorkspaceStateAllowsWebUIDisplayNames(t *testing.T) {
	body := []byte(`{
		"timeStamp": 1783488590459,
		"files": [
			{"uri":"settings:/users/user123/workspaces/Test Workspace.code-workspace","type":"file","checksum":"testsum","size":"134"},
			{"uri":"settings:/users/user123/workspaces/Default - admin.code-workspace","type":"file","checksum":"defaultsum","size":"250"},
			{"uri":"settings:/users/user123/settings.json","type":"file","checksum":"settingssum"},
			{"uri":"settings:/users/user123/workspaces","type":"dir"}
		]
	}`)
	workspaces, err := parseWebWorkspaceState(body)
	if err != nil {
		t.Fatalf("parseWebWorkspaceState returned error: %v", err)
	}
	if len(workspaces) != 2 {
		t.Fatalf("workspace count = %d, want 2: %#v", len(workspaces), workspaces)
	}
	if workspaces[0].Name != "Default - admin" || workspaces[1].Name != "Test Workspace" {
		t.Fatalf("workspace names/order mismatch: %#v", workspaces)
	}
}

func TestWorkspaceListUsesWebUISyncStateWhenAuthenticated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := workspaceTestServer(t)
	defer server.Close()

	c, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c.gatewayAuth = authModeBasic
	c.basicUser = "admin"
	c.basicPass = "pw"
	c.httpClient = server.Client()

	var output strings.Builder
	handled, err := withSlashCommandOutput(&output, func() (bool, error) {
		return handleSlashCommand(context.Background(), c, "/workspace list")
	})
	if err != nil {
		t.Fatalf("/workspace list returned error: %v", err)
	}
	if !handled {
		t.Fatal("/workspace list was not handled")
	}
	got := output.String()
	if !strings.Contains(got, "workspaces:") || !strings.Contains(got, "Default - admin") || !strings.Contains(got, "Test Workspace") {
		t.Fatalf("web workspace list output mismatch: %q", got)
	}
}

func TestWorkspaceUseSwitchesToWebUIWorkspaceAndLoadsFolders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := workspaceTestServer(t)
	defer server.Close()

	c, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c.gatewayAuth = authModeBasic
	c.basicUser = "admin"
	c.basicPass = "pw"
	c.httpClient = server.Client()
	lastConversationID := "506fe6bc3b91c350d6531d9c73e45acc"
	saved := newWorkspaceState("Default - admin")
	saved.ConversationID = lastConversationID
	saved.ConversationTitle = "Last workspace chat"
	saved.ServerConversation = true
	if err := saveWorkspace(c.opts.Profile, saved); err != nil {
		t.Fatal(err)
	}

	var output strings.Builder
	handled, err := withSlashCommandOutput(&output, func() (bool, error) {
		return handleSlashCommand(context.Background(), c, "/workspace use Default - admin")
	})
	if err != nil {
		t.Fatalf("/workspace use returned error: %v", err)
	}
	if !handled {
		t.Fatal("/workspace use was not handled")
	}
	if c.workspaceName != "Default - admin" {
		t.Fatalf("workspaceName = %q", c.workspaceName)
	}
	if c.workspaceURI != "settings:/users/user123/workspaces/Default - admin.code-workspace" {
		t.Fatalf("workspaceURI = %q", c.workspaceURI)
	}
	if c.workspaceDescription != "Admin default workspace" {
		t.Fatalf("workspaceDescription = %q", c.workspaceDescription)
	}
	if c.currentApp == nil || c.currentApp.AppSysID != "c97c188e73654c3cbf3e72e2f5339622" {
		t.Fatalf("current app not loaded from workspace folders: %#v", c.currentApp)
	}
	if c.conversationID != lastConversationID {
		t.Fatalf("last workspace conversation was not restored: %q", c.conversationID)
	}
	if _, ok := loadWorkspace(c.opts.Profile, "Default - admin"); !ok {
		t.Fatal("remote workspace state was not persisted locally")
	}
	got := output.String()
	if !strings.Contains(got, "workspace: Default - admin") || !strings.Contains(got, "app sys_id: c97c188e73654c3cbf3e72e2f5339622") {
		t.Fatalf("workspace use output mismatch: %q", got)
	}
}

func TestWorkspaceChoicesPreferWebUIAndResolveByName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := workspaceTestServer(t)
	defer server.Close()

	c, err := NewClient(CLIConfig{InstanceURL: server.URL}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c.gatewayAuth = authModeBasic
	c.basicUser = "admin"
	c.basicPass = "pw"
	c.httpClient = server.Client()
	c.workspaceURI = "settings:/users/user123/workspaces/Default - admin.code-workspace"

	choices, err := c.ListWorkspaceChoices(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaceChoices returned error: %v", err)
	}
	if len(choices) != 2 || choices[0].Source != "web" || !choices[0].Current || !strings.Contains(choices[0].Label, "[web]") {
		t.Fatalf("unexpected web workspace choices: %#v", choices)
	}
	choice, ok := workspaceChoiceBySelection(choices, "Default")
	if !ok || choice.Name != "Default - admin" {
		t.Fatalf("workspaceChoiceBySelection(Default) = %#v, %v", choice, ok)
	}
}

func workspaceTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	state := map[string]interface{}{
		"timeStamp": 1783488590459,
		"files": []map[string]interface{}{
			{
				"uri":      "settings:/users/user123/workspaces/Test Workspace.code-workspace",
				"type":     "file",
				"checksum": "testsum",
				"size":     "134",
			},
			{
				"uri":      "settings:/users/user123/workspaces/Default - admin.code-workspace",
				"type":     "file",
				"checksum": "defaultsum",
				"size":     "250",
			},
		},
	}
	workspaceContent := []byte(`{
		"folders": [
			{"name":"Demo App","uri":"now-file:/c97c188e73654c3cbf3e72e2f5339622"}
		],
		"settings": {},
		"description": "Admin default workspace"
	}`)

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sn_glider_app/ide.do":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<script>window.sn_glider = { user: { userId: 'user123' } };</script>`))
		case "/api/sn_glider/v2/sync/state":
			var payload struct {
				URIs []string `json:"uris"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("sync/state request JSON decode failed: %v", err)
			}
			if len(payload.URIs) != 1 || payload.URIs[0] != "settings:/users/user123" {
				t.Fatalf("sync/state URIs = %#v", payload.URIs)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state)
		case "/api/sn_glider/v2/sync/files":
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				t.Fatalf("sync/files content-type = %q", r.Header.Get("Content-Type"))
			}
			var buf bytes.Buffer
			writer := multipart.NewWriter(&buf)
			part, err := writer.CreateFormFile("defaultsum", "defaultsum")
			if err != nil {
				t.Fatalf("CreateFormFile failed: %v", err)
			}
			_, _ = part.Write(workspaceContent)
			if err := writer.Close(); err != nil {
				t.Fatalf("multipart writer close failed: %v", err)
			}
			w.Header().Set("Content-Type", writer.FormDataContentType())
			_, _ = w.Write(buf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
}
