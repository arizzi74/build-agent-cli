package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFluentTopicsListScansSDKDocsFromGlider(t *testing.T) {
	const appID = "appsysid123"
	rootURI := "now-file:/" + appID
	entries := []gliderChangeEntry{
		{URI: rootURI + "/now.config.json", Type: "file", Checksum: "cfg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/package.json", Type: "file", Checksum: "pkg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/fluent-overview.md", Type: "file", Checksum: "overview"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/table-guide.md", Type: "file", Checksum: "table"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/internal-reference.md", Type: "file", Checksum: "internal"},
	}
	contents := map[string]string{
		rootURI + "/now.config.json":                                         `{"scope":"x_snc_demo"}`,
		rootURI + "/node_modules/@servicenow/sdk/package.json":               `{"name":"@servicenow/sdk"}`,
		rootURI + "/node_modules/@servicenow/sdk/docs/fluent-overview.md":    "---\ntags: [overview]\n---\n# Fluent Overview\n\nOverview summary.\n\n## Next\nMore.",
		rootURI + "/node_modules/@servicenow/sdk/docs/table-guide.md":        "# Table Guide\n\nUse tables.\nContinues here.\n\n## API\nStop.",
		rootURI + "/node_modules/@servicenow/sdk/docs/internal-reference.md": "# Internal Reference\n\nShould not be returned.",
	}
	srv := httptest.NewServer(fluentTopicMockHandler(t, entries, contents))
	defer srv.Close()

	c := &Client{cfg: CLIConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
	result, status := c.answerFluentTopicsList(context.Background(), map[string]interface{}{"appId": appID})
	if status != "complete" {
		t.Fatalf("status = %q result=%#v", status, result)
	}
	var topics []fluentTopic
	if err := json.Unmarshal([]byte(stringify(result["content"])), &topics); err != nil {
		t.Fatalf("content decode: %v content=%q", err, stringify(result["content"]))
	}
	if len(topics) != 2 {
		t.Fatalf("topics = %#v", topics)
	}
	if topics[0].Name != "fluent-overview" || topics[0].Summary != "Overview summary." {
		t.Fatalf("overview topic = %#v", topics[0])
	}
	if topics[1].Name != "table-guide" || topics[1].Summary != "Use tables. Continues here." {
		t.Fatalf("table topic = %#v", topics[1])
	}
}

func TestFluentTopicsListReturnsNoProjectWhenNoApp(t *testing.T) {
	c := &Client{}
	result, status := c.answerFluentTopicsList(context.Background(), nil)
	if status != "error" || stringify(result["code"]) != "NO_FLUENT_PROJECT" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
}

func TestFluentTopicsListReturnsOldSDKWhenDocsMissing(t *testing.T) {
	const appID = "appsysid123"
	rootURI := "now-file:/" + appID
	entries := []gliderChangeEntry{
		{URI: rootURI + "/now.config.json", Type: "file", Checksum: "cfg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/package.json", Type: "file", Checksum: "pkg"},
	}
	contents := map[string]string{rootURI + "/now.config.json": `{"scope":"x_snc_demo"}`}
	srv := httptest.NewServer(fluentTopicMockHandler(t, entries, contents))
	defer srv.Close()

	c := &Client{cfg: CLIConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
	result, status := c.answerFluentTopicsList(context.Background(), map[string]interface{}{"appId": appID})
	if status != "error" || stringify(result["code"]) != "SDK_VERSION_TOO_OLD" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	if !strings.Contains(stringify(result["error"]), "docs not bundled") {
		t.Fatalf("error = %q", stringify(result["error"]))
	}
}

func fluentTopicMockHandler(t *testing.T, entries []gliderChangeEntry, contents map[string]string) http.HandlerFunc {
	t.Helper()
	entryByURI := map[string]gliderChangeEntry{}
	for _, entry := range entries {
		entryByURI[entry.URI] = entry
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("sync/state decode: %v", err)
			}
			requested, _ := payload["uris"].([]interface{})
			files := make([]gliderChangeEntry, 0)
			for _, raw := range requested {
				uri := strings.TrimRight(stringify(raw), "/")
				for _, entry := range entries {
					entryURI := strings.TrimRight(entry.URI, "/")
					if strings.EqualFold(entryURI, uri) || strings.HasPrefix(entry.URI, uri+"/") {
						files = append(files, entry)
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": files})
		case "/api/sn_glider/v2/sync/files":
			parts := readMultipartRequestParts(t, r)
			var requested []string
			if err := json.Unmarshal([]byte(parts["uris"]), &requested); err != nil {
				t.Fatalf("sync/files uris decode: %v parts=%#v", err, parts)
			}
			files := make([]map[string]string, 0, len(requested))
			for _, uri := range requested {
				entry, ok := entryByURI[uri]
				if !ok {
					continue
				}
				content, ok := contents[uri]
				if !ok {
					continue
				}
				files = append(files, map[string]string{"checksum": entry.Checksum, "content": content})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": files})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}
}
