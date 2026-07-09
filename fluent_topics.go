package main

import (
	"context"
	"encoding/json"
	"fmt"
	posixpath "path"
	"sort"
	"strings"
)

type fluentTopic struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

type fluentCatalogResult struct {
	OK     bool
	Reason string
	Topics []fluentTopic
}

func (c *Client) answerFluentTopicsList(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	appID := fluentTopicsAppID(payload, c.activeAppSysID())
	if appID == "" {
		return fluentTopicsError("NO_FLUENT_PROJECT", "No active fluent project scope. Catalog will be available once an app is selected or created."), "error"
	}
	if err := c.ensureConversationHTTPClient(); err != nil {
		return fluentTopicsError("FLUENT_TOPICS_ERROR", err.Error()), "error"
	}
	rootURI := "now-file:/" + appID
	exists, err := c.gliderFileExists(ctx, rootURI+"/now.config.json")
	if err != nil {
		return fluentTopicsError("FLUENT_TOPICS_ERROR", err.Error()), "error"
	}
	if !exists {
		return fluentTopicsError("NO_FLUENT_PROJECT", "No active fluent project scope. Catalog will be available once an app is selected or created."), "error"
	}
	catalog, err := c.loadFluentTopicCatalog(ctx, rootURI)
	if err != nil {
		return fluentTopicsError("FLUENT_TOPICS_ERROR", err.Error()), "error"
	}
	if !catalog.OK {
		if catalog.Reason == "sdk_version_too_old" {
			return fluentTopicsError("SDK_VERSION_TOO_OLD", "Fluent docs not bundled in installed SDK; project SDK predates docs bundling."), "error"
		}
		return fluentTopicsError("NO_FLUENT_PROJECT", "No fluent project detected in this workspace."), "error"
	}
	raw, err := json.Marshal(catalog.Topics)
	if err != nil {
		return fluentTopicsError("FLUENT_TOPICS_ERROR", err.Error()), "error"
	}
	return map[string]interface{}{"content": string(raw)}, "complete"
}

func fluentTopicsAppID(payload map[string]interface{}, fallback string) string {
	appID := strings.TrimSpace(firstString(payload, "appId", "app_id", "appSysId", "app_sys_id", "applicationId", "application_id"))
	if appID == "" {
		appID = strings.TrimSpace(fallback)
	}
	appID = strings.TrimPrefix(appID, "now-file:")
	appID = strings.Trim(appID, "/")
	if slash := strings.Index(appID, "/"); slash >= 0 {
		appID = appID[:slash]
	}
	return appID
}

func fluentTopicsError(code, message string) map[string]interface{} {
	message = strings.TrimSpace(message)
	if message == "" {
		message = code
	}
	return map[string]interface{}{"error": message, "code": code}
}

func (c *Client) gliderFileExists(ctx context.Context, uri string) (bool, error) {
	entries, err := c.fetchGliderStateForURIs(ctx, []string{uri})
	if err != nil {
		return false, err
	}
	uri = strings.TrimRight(strings.TrimSpace(uri), "/")
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimRight(entry.URI, "/"), uri) && gliderStateEntryIsFile(entry) {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) loadFluentTopicCatalog(ctx context.Context, workspaceURI string) (fluentCatalogResult, error) {
	workspaceURI = strings.TrimRight(strings.TrimSpace(workspaceURI), "/")
	if workspaceURI == "" {
		return fluentCatalogResult{OK: false, Reason: "no_fluent_project"}, nil
	}
	sdkURI := workspaceURI + "/node_modules/@servicenow/sdk"
	entries, err := c.fetchGliderState(ctx, sdkURI)
	if err != nil {
		return fluentCatalogResult{}, err
	}
	if len(entries) == 0 {
		return fluentCatalogResult{OK: false, Reason: "no_fluent_project"}, nil
	}
	docsURI := sdkURI + "/docs"
	docEntries := fluentDocMarkdownEntries(entries, docsURI)
	if len(docEntries) == 0 {
		return fluentCatalogResult{OK: false, Reason: "sdk_version_too_old"}, nil
	}
	topics, err := c.scanFluentDocTopics(ctx, docEntries)
	if err != nil {
		return fluentCatalogResult{}, err
	}
	return fluentCatalogResult{OK: true, Topics: topics}, nil
}

func fluentDocMarkdownEntries(entries []gliderChangeEntry, docsURI string) []gliderChangeEntry {
	docsURI = strings.TrimRight(docsURI, "/")
	prefix := docsURI + "/"
	out := make([]gliderChangeEntry, 0)
	for _, entry := range entries {
		if !gliderStateEntryIsFile(entry) {
			continue
		}
		if !strings.HasPrefix(entry.URI, prefix) || !strings.HasSuffix(strings.ToLower(entry.URI), ".md") {
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out
}

func (c *Client) scanFluentDocTopics(ctx context.Context, entries []gliderChangeEntry) ([]fluentTopic, error) {
	seen := map[string]string{}
	topics := make([]fluentTopic, 0, len(entries))
	const batchSize = 80
	for start := 0; start < len(entries); start += batchSize {
		end := start + batchSize
		if end > len(entries) {
			end = len(entries)
		}
		batch := entries[start:end]
		uris := make([]string, 0, len(batch))
		for _, entry := range batch {
			uris = append(uris, entry.URI)
		}
		contents, err := c.fetchV2SyncFiles(ctx, uris)
		if err != nil {
			return nil, err
		}
		for _, entry := range batch {
			name := strings.TrimSuffix(posixpath.Base(entry.URI), ".md")
			if previous, ok := seen[name]; ok {
				return nil, fmt.Errorf("duplicate doc topic name %q — both %s and %s resolve to it", name, previous, entry.URI)
			}
			seen[name] = entry.URI
			if !fluentTopicIncluded(name) {
				continue
			}
			content, ok := gliderFetchedContentForEntry(contents, entry)
			if !ok {
				return nil, fmt.Errorf("doc content was not returned for %s", entry.URI)
			}
			topics = append(topics, fluentTopic{Name: name, Summary: fluentDocSummary(string(content))})
		}
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].Name < topics[j].Name })
	return topics, nil
}

func fluentTopicIncluded(name string) bool {
	return name == "fluent-overview" || strings.HasSuffix(name, "-guide")
}

func fluentDocSummary(markdown string) string {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	markdown = strings.ReplaceAll(markdown, "\r", "\n")
	body := fluentDocBodyWithoutFrontmatter(markdown)
	lines := strings.Split(body, "\n")
	pastFirstHeading := false
	paragraph := make([]string, 0)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !pastFirstHeading {
			if strings.HasPrefix(trimmed, "#") {
				pastFirstHeading = true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") || (len(paragraph) > 0 && trimmed == "") {
			break
		}
		if trimmed == "" {
			continue
		}
		paragraph = append(paragraph, trimmed)
	}
	if summary := strings.TrimSpace(strings.Join(paragraph, " ")); summary != "" {
		return summary
	}
	return "(no summary available)"
}

func fluentDocBodyWithoutFrontmatter(markdown string) string {
	if !strings.HasPrefix(markdown, "---\n") {
		return markdown
	}
	end := strings.Index(markdown[len("---\n"):], "\n---\n")
	if end < 0 {
		return markdown
	}
	return markdown[len("---\n")+end+len("\n---\n"):]
}
