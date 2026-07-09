package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDiagnosticsReportsMissingTSCOnTerminal(t *testing.T) {
	content := `import { Table } from "@servicenow/sdk/core";
export const demo = true;
`
	server := newGliderProjectServer(t, map[string]string{"src/fluent/demo.now.ts": content})
	defer server.Close()

	t.Setenv("PATH", t.TempDir())
	c := &Client{
		cfg:        CLIConfig{InstanceURL: server.URL},
		httpClient: server.Client(),
		currentApp: &AppScope{ScopeID: "appsysid123", AppSysID: "appsysid123", ScopeName: "Demo", Scope: "x_snc_demo"},
	}
	var result map[string]interface{}
	var status string
	stderr := captureStderr(t, func() {
		result, status = c.answerGliderRunDiagnostics(context.Background(), map[string]interface{}{"files": []interface{}{"src/fluent/demo.now.ts"}})
	})
	if status != "error" || stringify(result["code"]) != "TSC_NOT_FOUND" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	if !strings.Contains(stderr, "run_diagnostics: TypeScript compiler 'tsc' was not found") {
		t.Fatalf("stderr missing tsc notice: %q", stderr)
	}
}

func TestRunDiagnosticsRunsLocalTSCAndReturnsErrors(t *testing.T) {
	content := `export const answer: string = 42;
`
	server := newGliderProjectServer(t, map[string]string{"src/fluent/demo.now.ts": content, "package.json": `{"name":"demo"}`})
	defer server.Close()

	binDir := t.TempDir()
	fakeTSC := filepath.Join(binDir, "tsc")
	if err := os.WriteFile(fakeTSC, []byte("#!/bin/sh\necho 'src/fluent/demo.now.ts(1,30): error TS2322: Type '\"'number'\"' is not assignable to type '\"'string'\"'.' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	c := &Client{
		cfg:        CLIConfig{InstanceURL: server.URL},
		httpClient: server.Client(),
		currentApp: &AppScope{ScopeID: "appsysid123", AppSysID: "appsysid123", ScopeName: "Demo", Scope: "x_snc_demo"},
	}
	result, status := c.answerGliderRunDiagnostics(context.Background(), map[string]interface{}{"files": []interface{}{"src/fluent/demo.now.ts"}})
	if status != "error" || stringify(result["code"]) != "DIAGNOSTICS_ERRORS" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	if !strings.Contains(stringify(result["result"]), "Found errors in 1 file(s)") {
		t.Fatalf("result text = %q", stringify(result["result"]))
	}
	metadata := asMap(result["metadata"])
	if metadata == nil {
		t.Fatalf("metadata missing: %#v", result)
	}
	diagnostics, ok := metadata["diagnostics"].(map[string][]map[string]interface{})
	if !ok || len(diagnostics["src/fluent/demo.now.ts"]) == 0 {
		t.Fatalf("diagnostics missing file: %#v", metadata["diagnostics"])
	}
}

func newGliderProjectServer(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	type fileEntry struct {
		URI      string
		Rel      string
		Checksum string
		Content  string
	}
	entries := make([]fileEntry, 0, len(files))
	for rel, content := range files {
		checksum := sha1Hex([]byte(content))
		entries = append(entries, fileEntry{URI: "now-file:/appsysid123/" + rel, Rel: rel, Checksum: checksum, Content: content})
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"files":[`)
			for i, entry := range entries {
				if i > 0 {
					_, _ = fmt.Fprint(w, ",")
				}
				_, _ = fmt.Fprintf(w, `{"uri":%q,"checksum":%q,"type":"file","size":%d}`, entry.URI, entry.Checksum, len(entry.Content))
			}
			_, _ = fmt.Fprint(w, `]}`)
		case "/api/sn_glider/v2/sync/files":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"files":[`)
			for i, entry := range entries {
				if i > 0 {
					_, _ = fmt.Fprint(w, ",")
				}
				_, _ = fmt.Fprintf(w, `{"checksum":%q,"content":%q}`, entry.Checksum, entry.Content)
			}
			_, _ = fmt.Fprint(w, `]}`)
		default:
			http.NotFound(w, r)
		}
	}))
}
