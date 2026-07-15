package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchFluentDocsRanksAndReportsMatches(t *testing.T) {
	const appID = "appsysid123"
	rootURI := "now-file:/" + appID
	entries := []gliderChangeEntry{
		{URI: rootURI + "/now.config.json", Type: "file", Checksum: "cfg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/package.json", Type: "file", Checksum: "pkg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/fluent-overview.md", Type: "file", Checksum: "overview"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/table-guide.md", Type: "file", Checksum: "table"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/query-guide.md", Type: "file", Checksum: "query"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/api/record-api.md", Type: "file", Checksum: "record"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/fluent/data-helpers-guide.md", Type: "file", Checksum: "helpers"},
	}
	contents := map[string]string{
		rootURI + "/now.config.json":                                                `{}`,
		rootURI + "/node_modules/@servicenow/sdk/package.json":                      `{}`,
		rootURI + "/node_modules/@servicenow/sdk/docs/fluent-overview.md":           "# Fluent Overview\n\nStart with tables and queries.",
		rootURI + "/node_modules/@servicenow/sdk/docs/table-guide.md":               "# Table Guide\n\nTables are the primary record API.",
		rootURI + "/node_modules/@servicenow/sdk/docs/query-guide.md":               "# Query Guide\n\nFilter table records.",
		rootURI + "/node_modules/@servicenow/sdk/docs/api/record-api.md":            "---\ntags: [record]\n---\n# Record API\n\nAccess entries.",
		rootURI + "/node_modules/@servicenow/sdk/docs/fluent/data-helpers-guide.md": "---\ntags:\n  - record\n  - data\n---\n# Data Helpers Guide\n\nUseful helpers.",
	}
	srv := httptest.NewServer(fluentTopicMockHandler(t, entries, contents))
	defer srv.Close()

	c := &Client{cfg: CLIConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
	result, status := c.answerSearchFluentDocs(context.Background(), map[string]interface{}{"appId": appID, "query": "table"})
	if status != "complete" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	var response fluentDocSearchResponse
	if err := json.Unmarshal([]byte(stringify(result["content"])), &response); err != nil {
		t.Fatal(err)
	}
	if response.Query != "table" || len(response.Matches) != 3 {
		t.Fatalf("response=%#v", response)
	}
	if response.Matches[0].Name != "table-guide" || !sameStrings(response.Matches[0].MatchedFields, []string{"name", "summary"}) {
		t.Fatalf("top match=%#v", response.Matches[0])
	}
	if response.Matches[1].Name != "fluent-overview" || !sameStrings(response.Matches[1].MatchedFields, []string{"summary"}) {
		t.Fatalf("second match=%#v", response.Matches[1])
	}
	if response.Matches[2].Name != "query-guide" || !sameStrings(response.Matches[2].MatchedFields, []string{"summary"}) {
		t.Fatalf("third match=%#v", response.Matches[2])
	}
	result, status = c.answerSearchFluentDocs(context.Background(), map[string]interface{}{"appId": appID, "query": "record"})
	if status != "complete" {
		t.Fatalf("nested search status=%q result=%#v", status, result)
	}
	if err := json.Unmarshal([]byte(stringify(result["content"])), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Matches) == 0 || response.Matches[0].Name != "record-api" || !sameStrings(response.Matches[0].MatchedFields, []string{"name", "tags"}) {
		t.Fatalf("nested search response=%#v", response)
	}
	result, status = c.answerSearchFluentDocs(context.Background(), map[string]interface{}{"appId": appID, "query": "record data"})
	if status != "complete" {
		t.Fatalf("tag search status=%q result=%#v", status, result)
	}
	if err := json.Unmarshal([]byte(stringify(result["content"])), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Matches) == 0 || response.Matches[0].Name != "data-helpers-guide" || !containsFluentDocField(response.Matches[0].MatchedFields, "tags") {
		t.Fatalf("tag search response=%#v", response)
	}
}

func TestSearchFluentDocsNoAppOrDocs(t *testing.T) {
	c := &Client{}
	result, status := c.answerSearchFluentDocs(context.Background(), map[string]interface{}{"query": "table"})
	if status != "error" || stringify(result["code"]) != "NO_FLUENT_PROJECT" {
		t.Fatalf("no app: status=%q result=%#v", status, result)
	}

	const appID = "appsysid123"
	rootURI := "now-file:/" + appID
	entries := []gliderChangeEntry{
		{URI: rootURI + "/now.config.json", Type: "file", Checksum: "cfg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/package.json", Type: "file", Checksum: "pkg"},
	}
	srv := httptest.NewServer(fluentTopicMockHandler(t, entries, map[string]string{rootURI + "/now.config.json": `{}`}))
	defer srv.Close()
	c = &Client{cfg: CLIConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
	result, status = c.answerSearchFluentDocs(context.Background(), map[string]interface{}{"appId": appID, "query": "table"})
	if status != "error" || stringify(result["code"]) != "SDK_VERSION_TOO_OLD" {
		t.Fatalf("no docs: status=%q result=%#v", status, result)
	}
}

func TestSearchFluentDocTopicsBoundsAndAttributesTermMatches(t *testing.T) {
	topics := make([]fluentTopic, 0, fluentDocsSearchLimit+2)
	for i := fluentDocsSearchLimit + 1; i >= 0; i-- {
		topics = append(topics, fluentTopic{Name: fmt.Sprintf("topic-%02d-guide", i), Summary: "Beta documentation."})
	}
	matches := searchFluentDocTopics(topics, "missing beta")
	if len(matches) != fluentDocsSearchLimit {
		t.Fatalf("matches=%d want=%d", len(matches), fluentDocsSearchLimit)
	}
	if matches[0].Name != "topic-00-guide" || !sameStrings(matches[0].MatchedFields, []string{"summary"}) {
		t.Fatalf("first match=%#v", matches[0])
	}
}

func TestExplainFluentDocReturnsExactTopicOnly(t *testing.T) {
	const appID = "appsysid123"
	rootURI := "now-file:/" + appID
	table := "# Table Guide\n\nUse tables.\n\n## API\nMore."
	entries := []gliderChangeEntry{
		{URI: rootURI + "/now.config.json", Type: "file", Checksum: "cfg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/package.json", Type: "file", Checksum: "pkg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/table-guide.md", Type: "file", Checksum: "table"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/api/record-api.md", Type: "file", Checksum: "record"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/docs/internal-reference.md", Type: "file", Checksum: "internal"},
	}
	contents := map[string]string{
		rootURI + "/now.config.json":                                         `{}`,
		rootURI + "/node_modules/@servicenow/sdk/package.json":               `{}`,
		rootURI + "/node_modules/@servicenow/sdk/docs/table-guide.md":        table,
		rootURI + "/node_modules/@servicenow/sdk/docs/api/record-api.md":     "# Record API\n\nNested record documentation.",
		rootURI + "/node_modules/@servicenow/sdk/docs/internal-reference.md": "# Internal\n\nNo.",
	}
	srv := httptest.NewServer(fluentTopicMockHandler(t, entries, contents))
	defer srv.Close()

	c := &Client{cfg: CLIConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
	result, status := c.answerExplainFluentDoc(context.Background(), map[string]interface{}{"appId": appID, "name": "table-guide"})
	if status != "complete" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	var response fluentDocExplainResponse
	if err := json.Unmarshal([]byte(stringify(result["content"])), &response); err != nil {
		t.Fatal(err)
	}
	if response.Name != "table-guide" || response.Content != table || response.Truncated {
		t.Fatalf("response=%#v", response)
	}

	result, status = c.answerExplainFluentDoc(context.Background(), map[string]interface{}{"appId": appID, "name": "record-api"})
	if status != "complete" {
		t.Fatalf("nested explain status=%q result=%#v", status, result)
	}
	if err := json.Unmarshal([]byte(stringify(result["content"])), &response); err != nil {
		t.Fatal(err)
	}
	if response.Content != "# Record API\n\nNested record documentation." {
		t.Fatalf("nested explain response=%#v", response)
	}

	for _, name := range []string{"../table-guide", "table-guide.md", "missing-guide"} {
		result, status = c.answerExplainFluentDoc(context.Background(), map[string]interface{}{"appId": appID, "name": name})
		if status != "error" {
			t.Fatalf("name=%q status=%q result=%#v", name, status, result)
		}
		wantCode := "FLUENT_DOC_NOT_FOUND"
		if strings.Contains(name, "/") || strings.HasSuffix(name, ".md") {
			wantCode = "INVALID_FLUENT_DOC_NAME"
		}
		if stringify(result["code"]) != wantCode {
			t.Fatalf("name=%q code=%q want=%q", name, stringify(result["code"]), wantCode)
		}
	}
}

func TestExplainFluentDocUsesCurrentLocalBuildSDKDocsWithoutHTTP(t *testing.T) {
	const appID = "appsysid123"
	project := t.TempDir()
	docs := filepath.Join(project, "node_modules", "@servicenow", "sdk", "docs", "nested")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "table-guide.md"), []byte("# Table Guide\n\nRead from current build."), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Client{
		cfg:             CLIConfig{InstanceURL: "https://must-not-connect.example"},
		httpClient:      &http.Client{Transport: failRoundTripper{t: t}},
		lastGliderBuild: &gliderBuildState{AppID: appID, TempDir: project},
	}
	result, status := c.answerExplainFluentDoc(context.Background(), map[string]interface{}{"appId": appID, "name": "table-guide"})
	if status != "complete" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	var response fluentDocExplainResponse
	if err := json.Unmarshal([]byte(stringify(result["content"])), &response); err != nil {
		t.Fatal(err)
	}
	if response.Content != "# Table Guide\n\nRead from current build." {
		t.Fatalf("response=%#v", response)
	}
}

func TestSearchFluentDocsUsesCanonicalCheckoutReadOnlyWithoutHTTP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const appID = "appsysid123"
	const profile = "fluent-docs-test"
	instance := "https://example.test"
	project := t.TempDir()
	docs := filepath.Join(project, "node_modules", "@servicenow", "sdk", "docs", "guides")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "table-guide.md"), []byte("# Table Guide\n\nCanonical checkout docs."), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := persistentSyncManifest{Version: 2, InstanceURL: instance, AppID: appID, RootURI: "now-file:/" + appID, CheckoutID: "checkout-1", Files: map[string]string{}}
	if err := savePersistentSyncManifest(project, manifest); err != nil {
		t.Fatal(err)
	}
	registry := projectRegistry{Version: projectRegistryVersion, Projects: map[string]registeredProject{
		projectRegistryKey(instance, appID): {InstanceURL: instance, AppID: appID, PrimaryCheckoutID: "checkout-1", Checkouts: []projectCheckout{{ID: "checkout-1", Path: project}}},
	}}
	if err := saveProjectRegistry(profile, registry); err != nil {
		t.Fatal(err)
	}
	registryPath := projectRegistryPath(profile)
	before, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}

	invalidOverride := t.TempDir()
	c := &Client{cfg: CLIConfig{InstanceURL: instance}, opts: Options{Profile: profile}, httpClient: &http.Client{Transport: failRoundTripper{t: t}}, localProjectOverride: invalidOverride}
	result, status := c.answerSearchFluentDocs(context.Background(), map[string]interface{}{"appId": appID, "query": "canonical"})
	if status != "complete" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	after, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("read-only Fluent docs lookup modified the project registry")
	}
	if c.localProjectOverride != invalidOverride {
		t.Fatal("read-only Fluent docs lookup modified the process-local checkout override")
	}
	var response fluentDocSearchResponse
	if err := json.Unmarshal([]byte(stringify(result["content"])), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Matches) != 1 || response.Matches[0].Name != "table-guide" {
		t.Fatalf("response=%#v", response)
	}
}

func TestFluentDocsRejectDuplicateNestedBasenames(t *testing.T) {
	const appID = "appsysid123"
	project := t.TempDir()
	for _, path := range []string{
		filepath.Join(project, "node_modules", "@servicenow", "sdk", "docs", "api", "record-api.md"),
		filepath.Join(project, "node_modules", "@servicenow", "sdk", "docs", "helpers", "record-api.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# Record API\n\nDuplicate."), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := &Client{cfg: CLIConfig{InstanceURL: "https://must-not-connect.example"}, httpClient: &http.Client{Transport: failRoundTripper{t: t}}, lastGliderBuild: &gliderBuildState{AppID: appID, TempDir: project}}
	result, status := c.answerExplainFluentDoc(context.Background(), map[string]interface{}{"appId": appID, "name": "record-api"})
	if status != "error" || stringify(result["code"]) != "FLUENT_DOCS_ERROR" || !strings.Contains(stringify(result["error"]), "duplicate doc topic name") {
		t.Fatalf("status=%q result=%#v", status, result)
	}
}

func TestMissingLocalDocsNeverInstallsOrSyncs(t *testing.T) {
	const appID = "appsysid123"
	rootURI := "now-file:/" + appID
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "node_modules", "@servicenow", "sdk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"dependencies":{"@servicenow/sdk":"1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []gliderChangeEntry{
		{URI: rootURI + "/now.config.json", Type: "file", Checksum: "cfg"},
		{URI: rootURI + "/node_modules/@servicenow/sdk/package.json", Type: "file", Checksum: "pkg"},
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || (r.URL.Path != "/api/sn_glider/v2/sync/state" && r.URL.Path != "/api/sn_glider/v2/sync/files") {
			t.Fatalf("unexpected mutation/network request: %s %s", r.Method, r.URL.Path)
		}
		fluentTopicMockHandler(t, entries, map[string]string{rootURI + "/now.config.json": `{}`})(w, r)
	}))
	defer srv.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: srv.URL}, httpClient: srv.Client(), lastGliderBuild: &gliderBuildState{AppID: appID, TempDir: project}}
	result, status := c.answerSearchFluentDocs(context.Background(), map[string]interface{}{"appId": appID, "query": "table"})
	if status != "error" || stringify(result["code"]) != "SDK_VERSION_TOO_OLD" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
	if calls == 0 {
		t.Fatal("expected only read-only remote fallback calls")
	}
	if _, err := os.Stat(filepath.Join(project, "node_modules", "@servicenow", "sdk", "docs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local docs directory changed: %v", err)
	}
}

func TestSearchFluentDocsHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Client{}
	result, status := c.answerSearchFluentDocs(ctx, map[string]interface{}{"appId": "appsysid123", "query": "table"})
	if status != "error" || stringify(result["code"]) != "FLUENT_DOCS_CANCELLED" {
		t.Fatalf("status=%q result=%#v", status, result)
	}
}

func TestTruncateFluentDocContentPreservesUTF8(t *testing.T) {
	content, truncated := truncateFluentDocContent("αβγ", 3)
	if !truncated || !strings.HasPrefix(content, "α") || !strings.Contains(content, "Documentation truncated") {
		t.Fatalf("content=%q truncated=%v", content, truncated)
	}
}

type failRoundTripper struct{ t *testing.T }

func (rt failRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.t.Helper()
	rt.t.Fatal("unexpected HTTP request while using local Fluent documentation")
	return nil, errors.New("unexpected HTTP request")
}

func containsFluentDocField(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
