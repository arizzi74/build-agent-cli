package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestFluentTopicsListFallsBackToRootStateForNowConfig(t *testing.T) {
	const appID = "appsysid123"
	rootURI := "now-file:/" + appID
	entries := []gliderChangeEntry{
		{URI: rootURI + "/now.config.json", Type: "file", Checksum: "cfg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/package.json", Type: "file", Checksum: "pkg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/fluent-overview.md", Type: "file", Checksum: "overview"},
	}
	contents := map[string]string{
		rootURI + "/now.config.json":                                      `{"scope":"x_snc_demo"}`,
		rootURI + "/node_modules/@servicenow/sdk/package.json":            `{"name":"@servicenow/sdk"}`,
		rootURI + "/node_modules/@servicenow/sdk/docs/fluent-overview.md": "# Fluent Overview\n\nOverview summary.",
	}
	srv := httptest.NewServer(fluentTopicExactNowConfigMissHandler(t, rootURI, entries, contents))
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
	if len(topics) != 1 || topics[0].Name != "fluent-overview" {
		t.Fatalf("topics = %#v", topics)
	}
}

func TestFluentTopicsListReturnsNoProjectWhenNoApp(t *testing.T) {
	c := &Client{}
	result, status := c.answerFluentTopicsList(context.Background(), nil)
	if status != "error" || stringify(result["code"]) != "NO_FLUENT_PROJECT" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
}

func TestScanLocalFluentDocTopics(t *testing.T) {
	docsDir := filepath.Join(t.TempDir(), "node_modules", "@servicenow", "sdk", "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"table-guide.md":        "# Table Guide\n\nUse tables locally.\nMore detail.\n\n## API\nStop.",
		"fluent-overview.md":    "# Fluent Overview\n\nLocal overview.",
		"internal-reference.md": "# Internal\n\nNot included.",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(docsDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := scanLocalFluentDocTopics(docsDir)
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.OK || len(catalog.Topics) != 2 {
		t.Fatalf("catalog = %#v", catalog)
	}
	if catalog.Topics[0].Name != "fluent-overview" || catalog.Topics[0].Summary != "Local overview." {
		t.Fatalf("overview topic = %#v", catalog.Topics[0])
	}
	if catalog.Topics[1].Name != "table-guide" || catalog.Topics[1].Summary != "Use tables locally. More detail." {
		t.Fatalf("table topic = %#v", catalog.Topics[1])
	}
}

func fluentTopicExactNowConfigMissHandler(t *testing.T, rootURI string, entries []gliderChangeEntry, contents map[string]string) http.HandlerFunc {
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
				if uri == rootURI+"/now.config.json" {
					continue
				}
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
