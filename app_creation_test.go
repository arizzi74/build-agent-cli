package main

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
)

func TestScopeCandidatesMatchObservedWebUIRetryShape(t *testing.T) {
	got := scopeCandidatesForAppName("My Test App")
	wantPrefix := []string{"x_snc_my_test_app", "x_snc_my_test_ap_1", "x_snc_my_test_ap_2"}
	if len(got) < len(wantPrefix) {
		t.Fatalf("scope candidates too short: %#v", got)
	}
	for i, want := range wantPrefix {
		if got[i] != want {
			t.Fatalf("candidate %d = %q, want %q (all=%#v)", i, got[i], want, got)
		}
		if len(got[i]) > serviceNowScopeMaxLength {
			t.Fatalf("candidate %q exceeds max length %d", got[i], serviceNowScopeMaxLength)
		}
	}
}

func TestCreateServiceNowAppLikeWebUIOrchestratesREST(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspaceURI := "settings:/users/user123/workspaces/Default - admin.code-workspace"
	workspaceChecksum := "workspace-checksum"
	workspaceBody := []byte(`{
  "settings": {"workbench.editor.untitled.hint": "hidden"},
  "folders": [{"name":"Existing App","uri":"now-file:/existingappsysid"}]
}`)

	var templateScopes []string
	var syncStateURIs []string
	var patchPayloads []map[string]interface{}
	var titlePayload map[string]interface{}
	var createEntries []gliderChangeEntry
	var updateEntries []gliderChangeEntry
	fileParts := map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sn_glider/v2/sync/files":
			writeMultipartSyncFilesResponse(t, w, workspaceChecksum, workspaceBody)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/sn_build_agent/build_agent_api/runQuery/table/sys_app/query/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"query_results":"No matching records found.","num_results":0}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/now/templates":
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("template payload decode: %v", err)
			}
			vars := asMap(payload["variables"])
			templateScopes = append(templateScopes, stringify(vars["scope_id"]))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":"template-` + string(rune('0'+len(templateScopes))) + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/now/templates/status":
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("template_instance_id") == "template-1" {
				_, _ = w.Write([]byte(`{"result":{"outputs":[{"name":"app_sys_id","value":""},{"name":"error_message","value":"Error: Ensure scope is unique on instance and app repo."},{"name":"has_error","value":"true"}],"status":"COMPLETE"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"outputs":[{"name":"app_sys_id","value":"newappsysid1234567890"},{"name":"error_message","value":""},{"name":"has_error","value":""}],"status":"COMPLETE"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/sn_glider/v2/sync/state":
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("sync/state payload decode: %v", err)
			}
			for _, raw := range payload["uris"].([]interface{}) {
				syncStateURIs = append(syncStateURIs, stringify(raw))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/sn_glider/v2/sync/changes/apply":
			parts := readMultipartRequestParts(t, r)
			if err := json.Unmarshal([]byte(parts["create"]), &createEntries); err != nil {
				t.Fatalf("create part decode: %v\n%s", err, parts["create"])
			}
			if err := json.Unmarshal([]byte(parts["update"]), &updateEntries); err != nil {
				t.Fatalf("update part decode: %v\n%s", err, parts["update"])
			}
			for name, content := range parts {
				if name != "create" && name != "update" && name != "remove" {
					fileParts[name] = content
				}
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/sn_build_agent/build_agent_api/conversations/conv123":
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("conversation patch decode: %v", err)
			}
			patchPayloads = append(patchPayloads, payload)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"sys_id":"conv123"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/sn_build_agent/build_agent_api/conversations/conv123/title":
			if err := json.NewDecoder(r.Body).Decode(&titlePayload); err != nil {
				t.Fatalf("conversation title decode: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"title":"My Test App: create application TRACE_CLI"}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()

	client := &Client{
		cfg:               CLIConfig{InstanceURL: srv.URL},
		opts:              Options{Profile: "default", Nirvana: true},
		httpClient:        srv.Client(),
		conversationID:    "conv123",
		conversationTitle: "create application TRACE_CLI",
		workspaceName:     "Default - admin",
		workspaceURI:      workspaceURI,
		workspaceChecksum: workspaceChecksum,
		workspaceFolders:  []WebWorkspaceFolder{{Name: "Existing App", URI: "now-file:/existingappsysid"}},
	}
	app, err := client.createServiceNowAppLikeWebUI(context.Background(), appCreationRequest{AppName: "My Test App", AppDescription: "A test application for ServiceNow development."})
	if err != nil {
		t.Fatal(err)
	}

	if app.Scope != "x_snc_my_test_ap_1" || app.ScopeID != "newappsysid1234567890" || app.Name != "My Test App" {
		t.Fatalf("unexpected created app: %#v", app)
	}
	if strings.Join(templateScopes, ",") != "x_snc_my_test_app,x_snc_my_test_ap_1" {
		t.Fatalf("template scopes = %#v", templateScopes)
	}
	if !containsString(syncStateURIs, "now-file:/existingappsysid") || !containsString(syncStateURIs, "now-file:/newappsysid1234567890") || !containsString(syncStateURIs, "settings:/users/user123") {
		t.Fatalf("sync/state uris = %#v", syncStateURIs)
	}
	if len(patchPayloads) != 2 || stringify(patchPayloads[0]["applicationId"]) != "newappsysid1234567890" {
		t.Fatalf("conversation patches = %#v", patchPayloads)
	}
	if _, ok := patchPayloads[1]["workingSet"].([]interface{}); !ok {
		t.Fatalf("second conversation patch should clear workingSet: %#v", patchPayloads)
	}
	if stringify(titlePayload["title"]) != "My Test App: create application TRACE_CLI" {
		t.Fatalf("conversation title payload = %#v", titlePayload)
	}
	if !hasGliderEntry(createEntries, "now-file:/newappsysid1234567890/package.json", "file") || !hasGliderEntry(createEntries, "now-file:/newappsysid1234567890/src/server/tsconfig.json", "file") {
		t.Fatalf("create entries missing app files: %#v", createEntries)
	}
	if len(updateEntries) != 1 || updateEntries[0].URI != workspaceURI || updateEntries[0].Checksum == workspaceChecksum {
		t.Fatalf("workspace update entry = %#v", updateEntries)
	}
	workspaceUpdated := false
	packageFound := false
	for _, content := range fileParts {
		if strings.Contains(content, `"name": "My Test App"`) && strings.Contains(content, `"uri": "now-file:/newappsysid1234567890"`) {
			workspaceUpdated = true
		}
		if strings.Contains(content, `"@servicenow/sdk": "4.8.1"`) && strings.Contains(content, `"name": "x-snc-my-test-app"`) {
			packageFound = true
		}
	}
	if !workspaceUpdated {
		t.Fatalf("updated workspace file part not found in %#v", fileParts)
	}
	if !packageFound {
		t.Fatalf("package.json file part not found in %#v", fileParts)
	}
	if client.CurrentApp() == nil || client.CurrentApp().ScopeID != "newappsysid1234567890" || client.statusBarAppName() != "My Test App" {
		t.Fatalf("current app not updated: %#v", client.CurrentApp())
	}
}

func TestServiceNowAppCreationResultMatchesWebSocketShape(t *testing.T) {
	result, err := serviceNowAppCreationResult(createdServiceNowApp{Name: "My Test App", Scope: "x_snc_my_test_ap_1", ScopeID: "newappsysid1234567890"})
	if err != nil {
		t.Fatal(err)
	}
	content := stringify(result["content"])
	if !strings.Contains(content, `"scope":"x_snc_my_test_ap_1"`) || !strings.Contains(content, `"tsconfigPath":"./src/server/tsconfig.json"`) {
		t.Fatalf("content JSON = %s", content)
	}
	ctx := asMap(result["ideContext"])
	if ctx == nil || stringify(ctx["fluentVersion"]) != fluentSDKVersion || stringify(ctx["scopeName"]) != "x_snc_my_test_ap_1" {
		t.Fatalf("ideContext = %#v", result["ideContext"])
	}
	folders, ok := ctx["workspaceFolders"].([]string)
	if !ok || len(folders) != 1 || folders[0] != "/newappsysid1234567890" {
		t.Fatalf("workspaceFolders = %#v", ctx["workspaceFolders"])
	}
}

func TestApplyGliderChangesSendsEmptyArraysForNilChangeLists(t *testing.T) {
	var parts map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sn_glider/v2/sync/changes/apply" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		parts = readMultipartRequestParts(t, r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &Client{cfg: CLIConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
	if err := client.applyGliderChanges(context.Background(), nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"create", "update", "remove"} {
		if parts[name] != "[]" {
			t.Fatalf("%s part = %q, want [] (all parts %#v)", name, parts[name], parts)
		}
	}
}

func writeMultipartSyncFilesResponse(t *testing.T, w http.ResponseWriter, checksum string, content []byte) {
	t.Helper()
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", mw.FormDataContentType())
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="`+checksum+`"; filename="`+checksum+`"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(content)
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
}

func readMultipartRequestParts(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		t.Fatalf("not multipart: %q err=%v", r.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	out := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		name := part.FormName()
		if name == "" {
			name = part.FileName()
		}
		out[name] = string(body)
	}
	return out
}

func hasGliderEntry(entries []gliderChangeEntry, uri, typ string) bool {
	for _, entry := range entries {
		if entry.URI == uri && entry.Type == typ {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
