package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGliderFSWriteFileTreatsVerifiedApply500AsSuccess(t *testing.T) {
	content := "export const ok = true\n"
	checksum := sha1Hex([]byte(content))
	var applyCalled bool
	var filesCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[]}`))
		case "/api/sn_glider/v2/sync/changes/apply":
			applyCalled = true
			http.Error(w, "", http.StatusInternalServerError)
		case "/api/sn_glider/v2/sync/files":
			filesCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"files":[{"checksum":%q,"content":%q}]}`, checksum, content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := &Client{
		cfg:        CLIConfig{InstanceURL: server.URL},
		httpClient: server.Client(),
		currentApp: &AppScope{ScopeID: "appsysid123", AppSysID: "appsysid123", ScopeName: "Demo", Scope: "x_snc_demo"},
	}
	result, status := c.answerGliderFSWriteFile(context.Background(), map[string]interface{}{"path": "src/fluent/demo.now.ts", "data": content})
	if status != "complete" {
		t.Fatalf("status = %q, result = %#v", status, result)
	}
	if !applyCalled || !filesCalled {
		t.Fatalf("applyCalled=%v filesCalled=%v, want both true", applyCalled, filesCalled)
	}
	if warning := stringify(result["warning"]); !strings.Contains(warning, "verified remote content") {
		t.Fatalf("warning = %q", warning)
	}
	if stringify(result["syncError"]) == "" {
		t.Fatalf("syncError missing: %#v", result)
	}
}

func TestEnsureActiveAppMetadataUsesSysAppScopeForIDEContextAndAppScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/now/table/sys_app/appsysid123":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"sys_id":"appsysid123","name":"Geronimo","scope":"x_snc_geronimo_2"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := &Client{
		cfg:        CLIConfig{InstanceURL: server.URL},
		httpClient: server.Client(),
		currentApp: &AppScope{ScopeID: "appsysid123", AppSysID: "appsysid123", ScopeName: "Geronimo"},
	}
	if err := c.ensureActiveAppMetadata(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.currentApp.Scope != "x_snc_geronimo_2" {
		t.Fatalf("scope = %q", c.currentApp.Scope)
	}
	ctx := c.currentIDEContext()
	if ctx["scopeName"] != "x_snc_geronimo_2" {
		t.Fatalf("ideContext scopeName = %#v", ctx["scopeName"])
	}
	appScope, ok := c.nirvanaOutboundAppScope()
	if !ok {
		t.Fatal("missing outbound appScope")
	}
	if appScope["scopeName"] != "x_snc_geronimo_2" || appScope["scope"] != "x_snc_geronimo_2" || appScope["appName"] != "Geronimo" {
		t.Fatalf("unexpected outbound appScope: %#v", appScope)
	}
}
