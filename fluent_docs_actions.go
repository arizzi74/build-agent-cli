package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	posixpath "path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	fluentDocsSearchLimit     = 20
	fluentDocsMaxQueryBytes   = 512
	fluentDocsMaxContentBytes = 48 * 1024
)

type fluentDocSearchMatch struct {
	Name          string   `json:"name"`
	Summary       string   `json:"summary"`
	MatchedFields []string `json:"matchedFields"`
	rank          int
}

type fluentDocSearchResponse struct {
	Query   string                 `json:"query"`
	Matches []fluentDocSearchMatch `json:"matches"`
}

type fluentDocExplainResponse struct {
	Name      string `json:"name"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// fluentDocsSource identifies a documentation source already present locally
// or remotely. It is deliberately read-only: no sync, install, checkout, or
// registry operation is permitted on this code path.
type fluentDocsSource struct {
	Catalog      fluentCatalogResult
	LocalDocPath map[string]string
	RemoteRoot   string
}

// answerSearchFluentDocs searches the active project's bundled SDK Fluent
// documentation. It first uses an existing local build/checkout and only then
// reads already-present remote Glider SDK docs. It never uploads or materializes
// a project, installs dependencies, or changes local project state.
func (c *Client) answerSearchFluentDocs(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	query, err := fluentDocsQuery(payload)
	if err != nil {
		return fluentDocsError("INVALID_FLUENT_DOC_QUERY", err.Error()), "error"
	}
	source, result, status := c.loadActiveFluentDocs(ctx, payload)
	if status != "complete" {
		return result, status
	}
	response, err := json.Marshal(fluentDocSearchResponse{Query: query, Matches: searchFluentDocTopics(source.Catalog.Topics, query)})
	if err != nil {
		return fluentDocsError("FLUENT_DOCS_ERROR", err.Error()), "error"
	}
	return map[string]interface{}{"content": string(response)}, "complete"
}

// answerExplainFluentDoc returns one exact, safe Fluent documentation topic.
// Topic names resolve solely through an already-scanned catalog, never as an
// arbitrary path supplied by Forge.
func (c *Client) answerExplainFluentDoc(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	name, err := fluentDocsTopicName(payload)
	if err != nil {
		return fluentDocsError("INVALID_FLUENT_DOC_NAME", err.Error()), "error"
	}
	source, result, status := c.loadActiveFluentDocs(ctx, payload)
	if status != "complete" {
		return result, status
	}
	if !fluentCatalogContainsTopic(source.Catalog.Topics, name) {
		return fluentDocsError("FLUENT_DOC_NOT_FOUND", fmt.Sprintf("Fluent documentation topic %q is not available in this project's SDK.", name)), "error"
	}
	content, err := c.readFluentDocFromSource(ctx, source, name)
	if err != nil {
		if errors.Is(err, errFluentDocNotFound) {
			return fluentDocsError("FLUENT_DOC_NOT_FOUND", fmt.Sprintf("Fluent documentation topic %q is not available in this project's SDK.", name)), "error"
		}
		return fluentDocsContextError(err)
	}
	content, truncated := truncateFluentDocContent(content, fluentDocsMaxContentBytes)
	response, err := json.Marshal(fluentDocExplainResponse{Name: name, Content: content, Truncated: truncated})
	if err != nil {
		return fluentDocsError("FLUENT_DOCS_ERROR", err.Error()), "error"
	}
	return map[string]interface{}{"content": string(response)}, "complete"
}

func (c *Client) loadActiveFluentDocs(ctx context.Context, payload map[string]interface{}) (fluentDocsSource, map[string]interface{}, string) {
	if err := ctx.Err(); err != nil {
		return fluentDocsSource{}, fluentDocsError("FLUENT_DOCS_CANCELLED", "Fluent docs operation cancelled."), "error"
	}
	appID := fluentTopicsAppID(payload, c.activeAppSysID())
	if appID == "" {
		return fluentDocsSource{}, fluentTopicsError("NO_FLUENT_PROJECT", "No active fluent project scope. Documentation will be available once an app is selected or created."), "error"
	}
	rootURI := "now-file:/" + appID

	// The local source must be attempted before obtaining an HTTP client so an
	// existing canonical checkout works completely offline.
	if source, ok, err := c.loadReadOnlyLocalFluentDocs(ctx, appID, rootURI); err != nil {
		result, status := fluentDocsContextError(err)
		return fluentDocsSource{}, result, status
	} else if ok {
		return source, nil, "complete"
	}

	// Do not initialize HTTP/auth here: that can load and apply a saved session.
	// Remote fallback is available only through the already-active Glider client.
	if c.httpClient == nil {
		return fluentDocsSource{}, fluentDocsError("FLUENT_DOCS_ERROR", "Fluent docs are not available locally and no active Glider connection is available."), "error"
	}
	source, reason, err := c.loadRemoteFluentDocs(ctx, rootURI)
	if err != nil {
		result, status := fluentDocsContextError(err)
		return fluentDocsSource{}, result, status
	}
	if source.Catalog.OK {
		return source, nil, "complete"
	}
	if reason == "sdk_version_too_old" {
		return fluentDocsSource{}, fluentTopicsError("SDK_VERSION_TOO_OLD", "Fluent docs not bundled in installed SDK; project SDK predates docs bundling."), "error"
	}
	return fluentDocsSource{}, fluentTopicsError("NO_FLUENT_PROJECT", "No fluent project detected in this workspace."), "error"
}

// loadReadOnlyLocalFluentDocs considers only existing sources. In particular,
// it never calls syncGliderBuildProjectToTemp, installProjectDependencies, or
// resolvePersistentProject (the mutating project resolver).
func (c *Client) loadReadOnlyLocalFluentDocs(ctx context.Context, appID, rootURI string) (fluentDocsSource, bool, error) {
	candidates := make([]string, 0, 2)
	if build := c.lastGliderBuild; build != nil && build.TempDir != "" && strings.EqualFold(strings.TrimSpace(build.AppID), appID) {
		candidates = append(candidates, build.TempDir)
	}
	resolution, err := c.resolveFluentDocsLocalCheckout(appID, c.persistentAppDisplayName(appID), rootURI)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fluentDocsSource{}, false, err
	}
	if err == nil && resolution.Dir != "" {
		duplicate := false
		for _, candidate := range candidates {
			if filepath.Clean(candidate) == filepath.Clean(resolution.Dir) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			candidates = append(candidates, resolution.Dir)
		}
	}
	for _, projectDir := range candidates {
		source, present, err := scanReadOnlyLocalFluentDocs(ctx, filepath.Join(projectDir, "node_modules", "@servicenow", "sdk", "docs"))
		if err != nil {
			return fluentDocsSource{}, false, err
		}
		if source.Catalog.OK {
			return source, true, nil
		}
		// A stale/incomplete local checkout must not block the remote, read-only
		// source, which may have the current SDK docs.
		_ = present
	}
	return fluentDocsSource{}, false, nil
}

// resolveFluentDocsLocalCheckout mirrors the persistent checkout lookup without
// the resolver's invalid-override cleanup. Docs lookup must not mutate even
// process-local selection state.
func (c *Client) resolveFluentDocsLocalCheckout(appID, appName, rootURI string) (persistentProjectResolution, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return persistentProjectResolution{}, errors.New("persistent local project requires an app id")
	}
	instanceURL, appID := c.projectRegistryIdentity(appID)
	if override := strings.TrimSpace(c.localProjectOverride); override != "" {
		if manifest, ok := validManifestedProject(override, instanceURL, appID, rootURI); ok {
			return persistentProjectResolution{Dir: override, CheckoutID: manifest.CheckoutID, Source: "current-process"}, nil
		}
	}
	if dir, manifest, ok := matchingManifestedAncestor(instanceURL, appID, rootURI); ok {
		return persistentProjectResolution{Dir: dir, CheckoutID: manifest.CheckoutID, Source: "current directory"}, nil
	}
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		return persistentProjectResolution{}, err
	}
	if project, ok := registryProject(registry, instanceURL, appID); ok {
		valid, manifests := validRegisteredCheckouts(project, instanceURL, appID, rootURI)
		if len(valid) > 0 {
			chosen := valid[0]
			if primary, found := projectCheckoutByID(project, project.PrimaryCheckoutID); found {
				for _, checkout := range valid {
					if checkout.ID == primary.ID {
						chosen = checkout
						break
					}
				}
			}
			return persistentProjectResolution{Dir: chosen.Path, CheckoutID: firstNonBlank(chosen.ID, manifests[chosen.ID].CheckoutID), Source: "registered checkout", Registered: true}, nil
		}
	}
	candidate, err := c.persistentProjectDirForAppNamed(appID, appName)
	if err != nil {
		return persistentProjectResolution{}, err
	}
	if manifest, ok := validCanonicalManifestedProject(candidate, instanceURL, appID, rootURI); ok {
		return persistentProjectResolution{Dir: candidate, CheckoutID: manifest.CheckoutID, Source: "canonical checkout"}, nil
	}
	return persistentProjectResolution{}, os.ErrNotExist
}

func (c *Client) loadRemoteFluentDocs(ctx context.Context, rootURI string) (fluentDocsSource, string, error) {
	if err := ctx.Err(); err != nil {
		return fluentDocsSource{}, "", err
	}
	exists, err := c.gliderFileExists(ctx, rootURI+"/now.config.json")
	if err != nil {
		return fluentDocsSource{}, "", err
	}
	if !exists {
		return fluentDocsSource{}, "no_fluent_project", nil
	}
	sdkURI := rootURI + "/node_modules/@servicenow/sdk"
	entries, err := c.fetchGliderState(ctx, sdkURI)
	if err != nil {
		return fluentDocsSource{}, "", err
	}
	if len(entries) == 0 {
		return fluentDocsSource{}, "no_fluent_project", nil
	}
	docEntries := fluentDocMarkdownEntries(entries, sdkURI+"/docs")
	if len(docEntries) == 0 {
		return fluentDocsSource{}, "sdk_version_too_old", nil
	}
	topics, err := c.scanRemoteFluentDocs(ctx, docEntries)
	if err != nil {
		return fluentDocsSource{}, "", err
	}
	return fluentDocsSource{Catalog: fluentCatalogResult{OK: true, Topics: topics}, RemoteRoot: rootURI}, "", nil
}

func scanReadOnlyLocalFluentDocs(ctx context.Context, docsDir string) (fluentDocsSource, bool, error) {
	docsDir = filepath.Clean(strings.TrimSpace(docsDir))
	if docsDir == "." || docsDir == "" {
		return fluentDocsSource{}, false, nil
	}
	info, err := os.Lstat(docsDir)
	if errors.Is(err, os.ErrNotExist) {
		return fluentDocsSource{}, false, nil
	}
	if err != nil {
		return fluentDocsSource{}, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fluentDocsSource{}, true, nil
	}

	paths := make(map[string]string)
	topics := make([]fluentTopic, 0)
	err = filepath.WalkDir(docsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 || d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		name := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
		if prior, exists := paths[name]; exists {
			return fmt.Errorf("duplicate doc topic name %q — both %s and %s resolve to it", name, prior, path)
		}
		rel, err := filepath.Rel(docsDir, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe local Fluent documentation path %q", path)
		}
		paths[name] = path
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		topics = append(topics, fluentTopic{Name: name, Summary: fluentDocSummary(string(content)), Tags: fluentDocTags(string(content))})
		return nil
	})
	if err != nil {
		return fluentDocsSource{}, true, err
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].Name < topics[j].Name })
	return fluentDocsSource{Catalog: fluentCatalogResult{OK: true, Topics: topics}, LocalDocPath: paths}, true, nil
}

// scanRemoteFluentDocs indexes every safe Markdown topic provided by the
// bundled SDK docs tree, including nested API references. Topic identity is
// basename-only for Forge compatibility, so duplicate basenames are rejected.
func (c *Client) scanRemoteFluentDocs(ctx context.Context, entries []gliderChangeEntry) ([]fluentTopic, error) {
	seen := make(map[string]string)
	topics := make([]fluentTopic, 0, len(entries))
	const batchSize = 80
	for start := 0; start < len(entries); start += batchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
			if prior, exists := seen[name]; exists {
				return nil, fmt.Errorf("duplicate doc topic name %q — both %s and %s resolve to it", name, prior, entry.URI)
			}
			seen[name] = entry.URI
			content, ok := gliderFetchedContentForEntry(contents, entry)
			if !ok {
				return nil, fmt.Errorf("doc content was not returned for %s", entry.URI)
			}
			topics = append(topics, fluentTopic{Name: name, Summary: fluentDocSummary(string(content)), Tags: fluentDocTags(string(content))})
		}
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].Name < topics[j].Name })
	return topics, nil
}

func (c *Client) readFluentDocFromSource(ctx context.Context, source fluentDocsSource, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if path, ok := source.LocalDocPath[name]; ok {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) || (err == nil && info.Mode()&os.ModeSymlink != 0) {
			return "", errFluentDocNotFound
		}
		if err != nil {
			return "", err
		}
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return "", errFluentDocNotFound
		}
		return string(content), err
	}
	if source.RemoteRoot == "" {
		return "", errFluentDocNotFound
	}
	sdkURI := source.RemoteRoot + "/node_modules/@servicenow/sdk"
	entries, err := c.fetchGliderState(ctx, sdkURI)
	if err != nil {
		return "", err
	}
	for _, entry := range fluentDocMarkdownEntries(entries, sdkURI+"/docs") {
		if strings.TrimSuffix(posixpath.Base(entry.URI), ".md") != name {
			continue
		}
		contents, err := c.fetchV2SyncFiles(ctx, []string{entry.URI})
		if err != nil {
			return "", err
		}
		content, ok := gliderFetchedContentForEntry(contents, entry)
		if !ok {
			return "", errFluentDocNotFound
		}
		return string(content), nil
	}
	return "", errFluentDocNotFound
}

func fluentDocTags(markdown string) []string {
	markdown = strings.ReplaceAll(strings.ReplaceAll(markdown, "\r\n", "\n"), "\r", "\n")
	if !strings.HasPrefix(markdown, "---\n") {
		return nil
	}
	end := strings.Index(markdown[len("---\n"):], "\n---\n")
	if end < 0 {
		return nil
	}
	frontmatter := markdown[len("---\n") : len("---\n")+end]
	values := make([]string, 0)
	inTags := false
	for _, line := range strings.Split(frontmatter, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), "tags:") {
			inTags = true
			if rest := strings.TrimSpace(trimmed[len("tags:"):]); rest != "" {
				values = append(values, fluentDocTagValues(rest)...)
			}
			continue
		}
		if inTags && strings.HasPrefix(trimmed, "-") {
			values = append(values, fluentDocTagValues(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")))...)
			continue
		}
		if trimmed != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inTags = false
		}
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.Trim(strings.TrimSpace(value), `"'`))
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func fluentDocTagValues(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	return strings.Split(raw, ",")
}

func fluentDocsQuery(payload map[string]interface{}) (string, error) {
	query := strings.Join(strings.Fields(firstString(payload, "query")), " ")
	if query == "" {
		return "", errors.New("A non-empty Fluent documentation search query is required.")
	}
	if len(query) > fluentDocsMaxQueryBytes {
		return "", fmt.Errorf("Fluent documentation search query exceeds the %d-byte limit.", fluentDocsMaxQueryBytes)
	}
	return query, nil
}

func fluentDocsTopicName(payload map[string]interface{}) (string, error) {
	name := strings.TrimSpace(firstString(payload, "name"))
	if name == "" {
		return "", errors.New("A Fluent documentation topic name is required.")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `\\/:`) || name == "." || name == ".." || strings.HasSuffix(strings.ToLower(name), ".md") {
		return "", errors.New("Fluent documentation topic name must be an exact catalog topic name, without a path or extension.")
	}
	return name, nil
}

func searchFluentDocTopics(topics []fluentTopic, query string) []fluentDocSearchMatch {
	needle := strings.ToLower(query)
	terms := strings.Fields(needle)
	matches := make([]fluentDocSearchMatch, 0)
	for _, topic := range topics {
		name := strings.ToLower(topic.Name)
		summary := strings.ToLower(topic.Summary)
		tags := strings.ToLower(strings.Join(topic.Tags, " "))
		fields := make([]string, 0, 3)
		rank := 0
		if name == needle {
			fields = append(fields, "name")
			rank += 1000
		} else if strings.HasPrefix(name, needle) {
			fields = append(fields, "name")
			rank += 900
		} else if strings.Contains(name, needle) {
			fields = append(fields, "name")
			rank += 800
		}
		if strings.Contains(summary, needle) {
			fields = append(fields, "summary")
			rank += 700
		}
		if strings.Contains(tags, needle) {
			fields = append(fields, "tags")
			rank += 750
		}
		termHits, nameTermMatch, summaryTermMatch, tagsTermMatch := 0, false, false, false
		for _, term := range terms {
			inName, inSummary, inTags := strings.Contains(name, term), strings.Contains(summary, term), strings.Contains(tags, term)
			if inName || inSummary || inTags {
				termHits++
			}
			nameTermMatch, summaryTermMatch, tagsTermMatch = nameTermMatch || inName, summaryTermMatch || inSummary, tagsTermMatch || inTags
		}
		if rank == 0 && termHits == 0 {
			continue
		}
		if nameTermMatch && !fluentDocMatchedField(fields, "name") {
			fields = append(fields, "name")
		}
		if summaryTermMatch && !fluentDocMatchedField(fields, "summary") {
			fields = append(fields, "summary")
		}
		if tagsTermMatch && !fluentDocMatchedField(fields, "tags") {
			fields = append(fields, "tags")
		}
		matches = append(matches, fluentDocSearchMatch{Name: topic.Name, Summary: topic.Summary, MatchedFields: fields, rank: rank + termHits*10})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank > matches[j].rank
		}
		return matches[i].Name < matches[j].Name
	})
	if len(matches) > fluentDocsSearchLimit {
		matches = matches[:fluentDocsSearchLimit]
	}
	return matches
}

func fluentDocMatchedField(fields []string, field string) bool {
	for _, candidate := range fields {
		if candidate == field {
			return true
		}
	}
	return false
}

var errFluentDocNotFound = errors.New("fluent documentation topic not found")

func fluentCatalogContainsTopic(topics []fluentTopic, name string) bool {
	for _, topic := range topics {
		if topic.Name == name {
			return true
		}
	}
	return false
}

func truncateFluentDocContent(content string, limit int) (string, bool) {
	if len(content) <= limit {
		return content, false
	}
	if limit <= 0 {
		return "", true
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut] + "\n\n[Documentation truncated by bacli.]", true
}

func fluentDocsContextError(err error) (map[string]interface{}, string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fluentDocsError("FLUENT_DOCS_CANCELLED", "Fluent docs operation cancelled."), "error"
	}
	return fluentDocsError("FLUENT_DOCS_ERROR", err.Error()), "error"
}

func fluentDocsError(code, message string) map[string]interface{} {
	return fluentTopicsError(code, message)
}
