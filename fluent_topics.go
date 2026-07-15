package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	posixpath "path"
	"path/filepath"
	"sort"
	"strings"
)

type fluentTopic struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Tags    []string `json:"tags,omitempty"`
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
	uri = strings.TrimRight(strings.TrimSpace(uri), "/")
	if uri == "" {
		return false, nil
	}
	var stateErr error
	if exists, err := c.gliderFileExistsInState(ctx, []string{uri}, uri); err != nil {
		stateErr = err
	} else if exists {
		return true, nil
	}
	// ServiceNow Glider can return an empty state result for an exact file URI even
	// when root workspace state shows the file. Mirror the Web extension's VFS
	// behavior by also checking the app root, then fall back to a direct file fetch.
	if rootURI := gliderRootURIFromFileURI(uri); rootURI != "" && !strings.EqualFold(rootURI, uri) {
		if exists, err := c.gliderFileExistsInState(ctx, []string{rootURI}, uri); err != nil {
			stateErr = err
		} else if exists {
			return true, nil
		}
	}
	contents, fetchErr := c.fetchV2SyncFiles(ctx, []string{uri})
	if fetchErr == nil {
		for _, content := range contents {
			if len(bytes.TrimSpace(content)) > 0 {
				return true, nil
			}
		}
	}
	if stateErr != nil {
		return false, stateErr
	}
	if fetchErr != nil {
		return false, fetchErr
	}
	return false, nil
}

func (c *Client) gliderFileExistsInState(ctx context.Context, stateURIs []string, targetURI string) (bool, error) {
	entries, err := c.fetchGliderStateForURIs(ctx, stateURIs)
	if err != nil {
		return false, err
	}
	targetURI = strings.TrimRight(strings.TrimSpace(targetURI), "/")
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimRight(entry.URI, "/"), targetURI) && gliderStateEntryIsFile(entry) {
			return true, nil
		}
	}
	return false, nil
}

func gliderRootURIFromFileURI(uri string) string {
	uri = strings.TrimRight(strings.TrimSpace(uri), "/")
	if !strings.HasPrefix(uri, "now-file:/") {
		return ""
	}
	rel := strings.TrimPrefix(uri, "now-file:/")
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return ""
	}
	return "now-file:/" + strings.TrimSpace(parts[0])
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
		return c.loadLocalFluentTopicCatalog(ctx, workspaceURI)
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

func (c *Client) loadLocalFluentTopicCatalog(ctx context.Context, workspaceURI string) (fluentCatalogResult, error) {
	appID := fluentTopicsWorkspaceAppID(workspaceURI)
	if build := c.lastGliderBuild; build != nil && build.TempDir != "" && strings.EqualFold(strings.TrimSpace(build.AppID), appID) {
		catalog, err := scanLocalFluentDocTopics(filepath.Join(build.TempDir, "node_modules", "@servicenow", "sdk", "docs"))
		if err != nil || catalog.OK {
			return catalog, err
		}
	}

	project, cleanup, err := c.syncGliderBuildProjectToTemp(ctx, workspaceURI)
	if err != nil {
		return fluentCatalogResult{}, err
	}
	defer cleanup()

	pkgPath := project.Files["package.json"]
	if pkgPath == "" {
		return fluentCatalogResult{OK: false, Reason: "no_fluent_project"}, nil
	}
	pkgRaw, err := os.ReadFile(pkgPath)
	if err != nil {
		return fluentCatalogResult{}, err
	}
	pkg := parsePackageJSONInfo(pkgRaw)
	if !packageHasDependency(pkg, "@servicenow/sdk") {
		return fluentCatalogResult{OK: false, Reason: "no_fluent_project"}, nil
	}
	if missing := missingNodeDependencies(project.Dir, []string{"@servicenow/sdk"}); len(missing) > 0 {
		if err := c.installProjectDependencies(ctx, project.Dir, missing); err != nil {
			return fluentCatalogResult{}, err
		}
	}
	return scanLocalFluentDocTopics(filepath.Join(project.Dir, "node_modules", "@servicenow", "sdk", "docs"))
}

func packageHasDependency(pkg packageJSONInfo, name string) bool {
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies, pkg.OptionalDependencies} {
		if _, ok := deps[name]; ok {
			return true
		}
	}
	return false
}

func scanLocalFluentDocTopics(docsDir string) (fluentCatalogResult, error) {
	docsDir = filepath.Clean(strings.TrimSpace(docsDir))
	if docsDir == "." || docsDir == "" {
		return fluentCatalogResult{OK: false, Reason: "no_fluent_project"}, nil
	}
	if info, err := os.Stat(docsDir); err != nil {
		sdkPackage := filepath.Join(filepath.Dir(docsDir), "package.json")
		if _, pkgErr := os.Stat(sdkPackage); pkgErr == nil {
			return fluentCatalogResult{OK: false, Reason: "sdk_version_too_old"}, nil
		}
		if os.IsNotExist(err) {
			return fluentCatalogResult{OK: false, Reason: "no_fluent_project"}, nil
		}
		return fluentCatalogResult{}, err
	} else if !info.IsDir() {
		return fluentCatalogResult{OK: false, Reason: "sdk_version_too_old"}, nil
	}

	seen := map[string]string{}
	topics := []fluentTopic{}
	if err := filepath.WalkDir(docsDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		name := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
		if previous, ok := seen[name]; ok {
			return fmt.Errorf("duplicate doc topic name %q — both %s and %s resolve to it", name, previous, path)
		}
		seen[name] = path
		if !fluentTopicIncluded(name) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		topics = append(topics, fluentTopic{Name: name, Summary: fluentDocSummary(string(content))})
		return nil
	}); err != nil {
		return fluentCatalogResult{}, err
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].Name < topics[j].Name })
	return fluentCatalogResult{OK: true, Topics: topics}, nil
}

func fluentTopicsWorkspaceAppID(workspaceURI string) string {
	workspaceURI = strings.TrimRight(strings.TrimSpace(workspaceURI), "/")
	workspaceURI = strings.TrimPrefix(workspaceURI, "now-file:")
	workspaceURI = strings.Trim(workspaceURI, "/")
	if slash := strings.Index(workspaceURI, "/"); slash >= 0 {
		workspaceURI = workspaceURI[:slash]
	}
	return workspaceURI
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
