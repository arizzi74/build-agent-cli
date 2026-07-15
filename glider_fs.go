package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	posixpath "path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

func (c *Client) withGliderFSTimeout(fn func(context.Context) (map[string]interface{}, string)) (map[string]interface{}, string) {
	ctx, cancel := context.WithTimeout(c.activeTurnContext(), 90*time.Second)
	defer cancel()
	return fn(ctx)
}

func (c *Client) answerGliderFSReadDirectory(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	uri, rel, err := c.resolveGliderPath(payloadPath(payload))
	if err != nil {
		return fsToolError(err), "error"
	}
	entries, err := c.gliderDirectoryEntries(ctx, uri, rel)
	if err != nil {
		return fsToolError(err), "error"
	}
	return map[string]interface{}{"entries": entries, "ideContext": c.currentIDEContext()}, "complete"
}

func (c *Client) answerGliderFSReadFile(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	uri, _, err := c.resolveGliderPath(payloadPath(payload))
	if err != nil {
		return fsToolError(err), "error"
	}
	contents, err := c.fetchV2SyncFiles(ctx, []string{uri})
	if err != nil {
		return fsToolError(err), "error"
	}
	for _, content := range contents {
		return map[string]interface{}{"content": string(content), "ideContext": c.currentIDEContext()}, "complete"
	}
	return fsToolError(fmt.Errorf("file not found: %s", payloadPath(payload))), "error"
}

func (c *Client) answerGliderFSWriteFile(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	filePath := payloadPath(payload)
	data := stringify(payload["data"])
	if data == "" {
		data = stringify(payload["content"])
	}
	if strings.HasSuffix(strings.ToLower(filePath), ".xml") {
		return map[string]interface{}{"error": "Operations on XML files are not allowed", "code": "XML_BLOCKED"}, "error"
	}
	uri, rel, err := c.resolveGliderPath(filePath)
	if err != nil {
		return fsToolError(err), "error"
	}
	if rel == "" {
		return fsToolError(errors.New("cannot write app root as a file")), "error"
	}
	content := []byte(data)
	nowMS := time.Now().UnixMilli()
	entry := gliderChangeEntry{Checksum: sha1Hex(content), CTime: nowMS, MTime: nowMS, Size: len(content), Type: "file", URI: uri}
	files := []gliderFileBlob{{Path: entry.Checksum, Checksum: entry.Checksum, Content: content}}
	createDirs, existing := c.gliderMissingParentDirs(ctx, rel)
	var create []gliderChangeEntry
	var update []gliderChangeEntry
	create = append(create, createDirs...)
	if existing[uri] {
		update = append(update, entry)
	} else {
		create = append(create, entry)
	}
	if err := c.applyGliderChanges(ctx, create, update, nil, files); err != nil {
		if c.gliderFileContentMatches(ctx, uri, content) {
			return map[string]interface{}{
				"message":    fmt.Sprintf("Successfully wrote to %s", filePath),
				"path":       filePath,
				"uri":        uri,
				"warning":    "Glider sync returned an error after persisting the file; verified remote content matches.",
				"syncError":  err.Error(),
				"ideContext": c.currentIDEContext(),
			}, "complete"
		}
		return fsToolError(err), "error"
	}
	return map[string]interface{}{"message": fmt.Sprintf("Successfully wrote to %s", filePath), "path": filePath, "uri": uri, "ideContext": c.currentIDEContext()}, "complete"
}

func (c *Client) answerGliderFSCreateDirectory(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	uri, rel, err := c.resolveGliderPath(payloadPath(payload))
	if err != nil {
		return fsToolError(err), "error"
	}
	nowMS := time.Now().UnixMilli()
	createDirs, _ := c.gliderMissingParentDirs(ctx, rel)
	entry := gliderChangeEntry{CTime: nowMS, MTime: nowMS, Size: 0, Type: "dir", URI: uri}
	createDirs = append(createDirs, entry)
	if err := c.applyGliderChanges(ctx, createDirs, nil, nil, nil); err != nil {
		if c.gliderDirectoryExists(ctx, uri) {
			return map[string]interface{}{
				"message":    fmt.Sprintf("Successfully created directory %s", payloadPath(payload)),
				"path":       payloadPath(payload),
				"uri":        uri,
				"warning":    "Glider sync returned an error after persisting the directory; verified remote state contains it.",
				"syncError":  err.Error(),
				"ideContext": c.currentIDEContext(),
			}, "complete"
		}
		return fsToolError(err), "error"
	}
	return map[string]interface{}{"message": fmt.Sprintf("Successfully created directory %s", payloadPath(payload)), "path": payloadPath(payload), "uri": uri, "ideContext": c.currentIDEContext()}, "complete"
}

// answerGliderFSDelete removes a file or a directory subtree from the active
// Glider VFS. It never touches the local persistent checkout.
func (c *Client) answerGliderFSDelete(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	filePath := payloadPath(payload)
	if strings.HasSuffix(strings.ToLower(filePath), ".xml") {
		return map[string]interface{}{"error": "Operations on XML files are not allowed", "code": "XML_BLOCKED"}, "error"
	}
	uri, rel, err := c.resolveGliderPath(filePath)
	if err != nil {
		return fsToolError(err), "error"
	}
	if rel == "" {
		return fsToolError(errors.New("cannot delete app root")), "error"
	}
	entries, err := c.fetchGliderState(ctx, c.activeAppRootURI())
	if err != nil {
		return fsToolError(err), "error"
	}
	prefix := strings.TrimRight(uri, "/") + "/"
	remove := make([]gliderChangeEntry, 0)
	for _, entry := range entries {
		if strings.EqualFold(entry.URI, uri) || strings.HasPrefix(entry.URI, prefix) {
			if strings.HasSuffix(strings.ToLower(entry.URI), ".xml") {
				return map[string]interface{}{"error": "Operations on XML files are not allowed", "code": "XML_BLOCKED"}, "error"
			}
			remove = append(remove, entry)
		}
	}
	if len(remove) == 0 {
		// Deletion is deliberately idempotent: remote absence is the requested
		// postcondition and no mutation request is necessary.
		return map[string]interface{}{"message": fmt.Sprintf("Path already absent: %s", filePath), "path": filePath, "uri": uri, "deleted": false, "ideContext": c.currentIDEContext()}, "complete"
	}
	sort.Slice(remove, func(i, j int) bool {
		// Children must precede their parent directories; make equal-depth order
		// stable and deterministic for reproducible multipart payloads.
		di, dj := strings.Count(strings.Trim(remove[i].URI, "/"), "/"), strings.Count(strings.Trim(remove[j].URI, "/"), "/")
		if di != dj {
			return di > dj
		}
		return remove[i].URI < remove[j].URI
	})
	if err := c.applyGliderChanges(ctx, nil, nil, remove, nil); err != nil {
		if c.gliderPathAbsent(ctx, uri) {
			return map[string]interface{}{
				"message": fmt.Sprintf("Successfully deleted: %s", rel), "path": filePath, "uri": uri, "deleted": true,
				"warning": "Glider sync returned an error after deleting the path; verified remote path is absent.", "syncError": err.Error(), "ideContext": c.currentIDEContext(),
			}, "complete"
		}
		return fsToolError(err), "error"
	}
	return map[string]interface{}{"message": fmt.Sprintf("Successfully deleted: %s", rel), "path": filePath, "uri": uri, "deleted": true, "removed": len(remove), "ideContext": c.currentIDEContext()}, "complete"
}

func (c *Client) answerGliderFSTree(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	uri, rel, err := c.resolveGliderPath(payloadPath(payload))
	if err != nil {
		return fsToolError(err), "error"
	}
	maxDepth := intFromPayload(payload["maxDepth"], 64)
	entries, err := c.fetchGliderState(ctx, uri)
	if err != nil {
		return fsToolError(err), "error"
	}
	tree := gliderTreeFromEntries(uri, rel, entries, maxDepth)
	return map[string]interface{}{"tree": tree, "ideContext": c.currentIDEContext()}, "complete"
}

func (c *Client) answerGliderFSStat(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	uri, _, err := c.resolveGliderPath(payloadPath(payload))
	if err != nil {
		return fsToolError(err), "error"
	}
	entries, err := c.fetchGliderState(ctx, c.activeAppRootURI())
	if err != nil {
		return fsToolError(err), "error"
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.URI, uri) {
			return map[string]interface{}{"type": entry.Type, "size": entry.Size, "checksum": entry.Checksum, "uri": entry.URI, "ideContext": c.currentIDEContext()}, "complete"
		}
	}
	prefix := strings.TrimRight(uri, "/") + "/"
	for _, entry := range entries {
		if strings.HasPrefix(entry.URI, prefix) {
			return map[string]interface{}{"type": "dir", "size": 0, "uri": uri, "ideContext": c.currentIDEContext()}, "complete"
		}
	}
	return fsToolError(fmt.Errorf("path not found: %s", payloadPath(payload))), "error"
}

func (c *Client) answerGliderFSGlob(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	pattern := strings.TrimSpace(firstString(payload, "pattern", "glob"))
	if pattern == "" {
		return fsToolError(errors.New("fs_glob requires pattern")), "error"
	}
	entries, err := c.fetchGliderState(ctx, c.activeAppRootURI())
	if err != nil {
		return fsToolError(err), "error"
	}
	matches := make([]string, 0)
	for _, entry := range entries {
		if entry.Type != "file" {
			continue
		}
		rel := c.relFromGliderURI(entry.URI)
		ok, _ := posixpath.Match(pattern, rel)
		if !ok && !strings.Contains(pattern, "/") {
			ok, _ = posixpath.Match(pattern, posixpath.Base(rel))
		}
		if ok {
			matches = append(matches, rel)
		}
	}
	sort.Strings(matches)
	return map[string]interface{}{"matches": matches, "files": matches, "ideContext": c.currentIDEContext()}, "complete"
}

const (
	fsGrepMaxFiles     = 200
	fsGrepMaxMatches   = 1000
	fsGrepMaxFileBytes = 1 << 20
	fsGrepMaxLineBytes = 4096
	fsGrepFetchBatch   = 20
)

// answerGliderFSGrep searches the selected app directory without modifying it.
// Match locations are one-based; column is counted in UTF-8 characters.
func (c *Client) answerGliderFSGrep(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	pattern := firstString(payload, "pattern", "query", "search_string")
	if pattern == "" {
		return fsToolError(errors.New("fs_grep requires pattern")), "error"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fsToolError(fmt.Errorf("invalid fs_grep regex %q: %w", pattern, err)), "error"
	}

	rootURI, _, err := c.resolveGliderPath(payloadPath(payload))
	if err != nil {
		return fsToolError(err), "error"
	}
	glob := strings.TrimSpace(firstString(payload, "glob"))
	if glob == "" {
		glob = "**/*"
	}
	globRE, err := compileGliderGlob(glob)
	if err != nil {
		return fsToolError(fmt.Errorf("invalid fs_grep glob %q: %w", glob, err)), "error"
	}
	if err := ctx.Err(); err != nil {
		return fsToolError(err), "error"
	}
	entries, err := c.fetchGliderState(ctx, rootURI)
	if err != nil {
		return fsToolError(err), "error"
	}

	type candidate struct{ uri, rel, scopedRel string }
	candidates := make([]candidate, 0)
	prefix := strings.TrimRight(rootURI, "/") + "/"
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return fsToolError(err), "error"
		}
		if entry.Type != "file" || entry.Size > fsGrepMaxFileBytes || !searchableGliderFile(entry.URI) {
			continue
		}
		if !strings.HasPrefix(entry.URI, prefix) {
			continue
		}
		rel := c.relFromGliderURI(entry.URI)
		scopedRel := strings.TrimPrefix(entry.URI, prefix)
		if globRE.MatchString(rel) || globRE.MatchString(scopedRel) {
			candidates = append(candidates, candidate{uri: entry.URI, rel: rel, scopedRel: scopedRel})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].rel < candidates[j].rel })
	if len(candidates) > fsGrepMaxFiles {
		candidates = candidates[:fsGrepMaxFiles]
	}

	matches := make([]map[string]interface{}, 0)
	fileSet := make(map[string]struct{})
	for start := 0; start < len(candidates) && len(matches) < fsGrepMaxMatches; start += fsGrepFetchBatch {
		if err := ctx.Err(); err != nil {
			return fsToolError(err), "error"
		}
		end := start + fsGrepFetchBatch
		if end > len(candidates) {
			end = len(candidates)
		}
		uris := make([]string, 0, end-start)
		for _, candidate := range candidates[start:end] {
			uris = append(uris, candidate.uri)
		}
		contents, err := c.fetchV2SyncFiles(ctx, uris)
		if err != nil {
			return fsToolError(err), "error"
		}
		for _, candidate := range candidates[start:end] {
			if err := ctx.Err(); err != nil {
				return fsToolError(err), "error"
			}
			content, ok := contents[candidate.uri]
			if !ok || len(content) > fsGrepMaxFileBytes || !gliderSearchableText(content) {
				continue
			}
			for _, index := range re.FindAllIndex(content, -1) {
				if len(matches) == fsGrepMaxMatches {
					break
				}
				line, column, text := gliderGrepLocation(content, index[0])
				matches = append(matches, map[string]interface{}{"file": candidate.rel, "line": line, "column": column, "text": text})
				fileSet[candidate.rel] = struct{}{}
			}
		}
	}
	files := make([]string, 0, len(fileSet))
	for file := range fileSet {
		files = append(files, file)
	}
	sort.Strings(files)
	return map[string]interface{}{"matches": matches, "files": files, "ideContext": c.currentIDEContext()}, "complete"
}

// compileGliderGlob supports path-segment globs and **, where ** matches zero
// or more directories (so **/*.ts also matches a.ts at the search root).
func compileGliderGlob(glob string) (*regexp.Regexp, error) {
	glob = strings.TrimSpace(strings.ReplaceAll(glob, "\\", "/"))
	if glob == "" {
		return nil, errors.New("glob cannot be empty")
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		switch glob[i] {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i += 2
				if i < len(glob) && glob[i] == '/' {
					b.WriteString("(?:.*/)?")
					i++
				} else {
					b.WriteString(".*")
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := i + 1
			if end < len(glob) && (glob[end] == '!' || glob[end] == '^') {
				end++
			}
			if end < len(glob) && glob[end] == ']' {
				end++
			}
			for end < len(glob) && glob[end] != ']' {
				end++
			}
			if end == len(glob) {
				return nil, errors.New("unterminated character class")
			}
			class := glob[i+1 : end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteByte('[')
			b.WriteString(class)
			b.WriteByte(']')
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
		i++
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func gliderSearchableText(content []byte) bool {
	return !bytes.Contains(content, []byte{0}) && utf8.Valid(content)
}

func gliderGrepLocation(content []byte, offset int) (line, column int, text string) {
	if offset < 0 {
		offset = 0
	}
	before := content[:offset]
	line = bytes.Count(before, []byte{'\n'}) + 1
	lineStart := bytes.LastIndexByte(before, '\n') + 1
	lineEnd := bytes.IndexByte(content[offset:], '\n')
	if lineEnd < 0 {
		lineEnd = len(content)
	} else {
		lineEnd += offset
	}
	lineBytes := content[lineStart:lineEnd]
	column = utf8.RuneCount(lineBytes[:offset-lineStart]) + 1
	text = string(lineBytes)
	if len(text) > fsGrepMaxLineBytes {
		budget := fsGrepMaxLineBytes - len("…")
		if offset-lineStart >= budget {
			text = "…" + string(gliderGrepUTF8Tail(lineBytes, budget))
		} else {
			text = string(gliderGrepUTF8Prefix(lineBytes, budget)) + "…"
		}
	}
	return line, column, text
}

func gliderGrepUTF8Prefix(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	end := max
	for end > 0 && b[end]&0xc0 == 0x80 {
		end--
	}
	return b[:end]
}

func gliderGrepUTF8Tail(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	start := len(b) - max
	for start < len(b) && b[start]&0xc0 == 0x80 {
		start++
	}
	return b[start:]
}

func (c *Client) answerGliderLocalSearch(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	needle := strings.ToLower(strings.TrimSpace(firstString(payload, "search_string", "query", "pattern")))
	if needle == "" {
		return fsToolError(errors.New("local_search requires search_string")), "error"
	}
	entries, err := c.fetchGliderState(ctx, c.activeAppRootURI())
	if err != nil {
		return fsToolError(err), "error"
	}
	files := make([]string, 0)
	for _, entry := range entries {
		if entry.Type != "file" || !searchableGliderFile(entry.URI) {
			continue
		}
		contents, err := c.fetchV2SyncFiles(ctx, []string{entry.URI})
		if err != nil {
			continue
		}
		for _, content := range contents {
			if strings.Contains(strings.ToLower(string(content)), needle) {
				files = append(files, c.relFromGliderURI(entry.URI))
			}
			break
		}
	}
	sort.Strings(files)
	return map[string]interface{}{"files": files, "ideContext": c.currentIDEContext()}, "complete"
}

func (c *Client) answerGliderBuild(ctx context.Context, action string, payload map[string]interface{}) (map[string]interface{}, string) {
	return c.answerBuildInstallParity(ctx, action, payload)
}

func (c *Client) activeAppRootURI() string {
	appID := c.activeAppSysID()
	if appID == "" {
		return ""
	}
	return "now-file:/" + appID
}

func (c *Client) activeAppSysID() string {
	if c.currentApp != nil {
		if id := strings.TrimSpace(c.currentApp.AppSysID); id != "" {
			return id
		}
		if id := strings.TrimSpace(c.currentApp.ScopeID); id != "" {
			return id
		}
	}
	if app := appFromPayload(asMap(c.appScope)); app != nil {
		if id := strings.TrimSpace(app.AppSysID); id != "" {
			return id
		}
		return strings.TrimSpace(app.ScopeID)
	}
	return strings.TrimSpace(stringify(c.appScope))
}

func (c *Client) resolveGliderPath(rawPath string) (uri string, rel string, err error) {
	appID := c.activeAppSysID()
	if appID == "" {
		return "", "", errors.New("Tool requires an application to have been created or selected")
	}
	p := strings.TrimSpace(strings.ReplaceAll(rawPath, "\\", "/"))
	if p == "" || p == "." {
		return "now-file:/" + appID, "", nil
	}
	if strings.HasPrefix(p, "now-file:") {
		trimmed := strings.TrimPrefix(p, "now-file:")
		trimmed = strings.TrimLeft(trimmed, "/")
		if trimmed == appID {
			return "now-file:/" + appID, "", nil
		}
		if strings.HasPrefix(trimmed, appID+"/") {
			rel = strings.TrimPrefix(trimmed, appID+"/")
			return "now-file:/" + appID + "/" + rel, rel, nil
		}
		return "", "", fmt.Errorf("path %q is outside active app %s", rawPath, appID)
	}
	p = strings.TrimLeft(p, "/")
	p = posixpath.Clean(p)
	if p == "." {
		return "now-file:/" + appID, "", nil
	}
	if p == appID {
		return "now-file:/" + appID, "", nil
	}
	if strings.HasPrefix(p, appID+"/") {
		p = strings.TrimPrefix(p, appID+"/")
	}
	if strings.HasPrefix(p, "../") || p == ".." || strings.Contains(p, "/../") {
		return "", "", fmt.Errorf("path escapes active app: %s", rawPath)
	}
	return "now-file:/" + appID + "/" + p, p, nil
}

func (c *Client) fetchGliderState(ctx context.Context, uri string) ([]gliderChangeEntry, error) {
	return c.fetchGliderStateForURIs(ctx, []string{uri})
}

func (c *Client) fetchGliderStateForURIs(ctx context.Context, uris []string) ([]gliderChangeEntry, error) {
	clean := make([]string, 0, len(uris))
	seen := map[string]struct{}{}
	for _, uri := range uris {
		uri = strings.TrimSpace(uri)
		if uri == "" {
			continue
		}
		key := strings.ToLower(uri)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		clean = append(clean, uri)
	}
	if len(clean) == 0 {
		return nil, errors.New("no active app uri")
	}
	body, _, err := c.postWebJSON(ctx, strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/sn_glider/v2/sync/state", map[string]interface{}{"uris": clean})
	if err != nil {
		return nil, err
	}
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("Glider sync/state returned a non-JSON response: %s", trimBody(body))
	}
	files := syncStateFiles(root)
	entries := make([]gliderChangeEntry, 0, len(files))
	for _, raw := range files {
		m := asMap(raw)
		if m == nil {
			continue
		}
		uri := strings.TrimSpace(firstString(m, "uri"))
		if uri == "" {
			continue
		}
		entries = append(entries, gliderChangeEntry{
			Checksum: firstString(m, "checksum"),
			MTime:    int64FromInterface(m["mtime"]),
			Size:     int(int64FromInterface(m["size"])),
			Type:     strings.ToLower(strings.TrimSpace(firstString(m, "type"))),
			URI:      uri,
		})
	}
	return entries, nil
}

func (c *Client) gliderDirectoryEntries(ctx context.Context, uri, rel string) ([]map[string]interface{}, error) {
	entries, err := c.fetchGliderState(ctx, uri)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimRight(uri, "/") + "/"
	typeByName := map[string]string{}
	for _, entry := range entries {
		if strings.EqualFold(entry.URI, uri) {
			continue
		}
		if !strings.HasPrefix(entry.URI, prefix) {
			continue
		}
		child := strings.TrimPrefix(entry.URI, prefix)
		if child == "" {
			continue
		}
		name := child
		typeName := strings.ToUpper(entry.Type)
		if slash := strings.Index(name, "/"); slash >= 0 {
			name = name[:slash]
			typeName = "DIR"
		}
		if typeName == "DIRECTORY" {
			typeName = "DIR"
		}
		if typeName == "" {
			typeName = "FILE"
		}
		if prev := typeByName[name]; prev == "DIR" {
			continue
		}
		typeByName[name] = typeName
	}
	names := make([]string, 0, len(typeByName))
	for name := range typeByName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]interface{}{"name": name, "type": typeByName[name]})
	}
	_ = rel
	return out, nil
}

func (c *Client) gliderMissingParentDirs(ctx context.Context, rel string) ([]gliderChangeEntry, map[string]bool) {
	root := c.activeAppRootURI()
	parentURIs := []string{root}
	dir := posixpath.Dir(rel)
	if dir != "." && dir != "/" && dir != "" {
		current := root
		for _, part := range strings.Split(dir, "/") {
			if part == "" || part == "." {
				continue
			}
			current += "/" + part
			parentURIs = append(parentURIs, current)
		}
	}
	entries, _ := c.fetchGliderStateForURIs(ctx, parentURIs)
	existing := map[string]bool{root: true}
	for _, entry := range entries {
		existing[entry.URI] = true
		for _, parent := range parentURIs {
			if strings.HasPrefix(entry.URI, strings.TrimRight(parent, "/")+"/") {
				existing[parent] = true
			}
		}
	}
	nowMS := time.Now().UnixMilli()
	var create []gliderChangeEntry
	if dir == "." || dir == "/" || dir == "" {
		return create, existing
	}
	parts := strings.Split(dir, "/")
	current := root
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current += "/" + part
		if existing[current] {
			continue
		}
		existing[current] = true
		create = append(create, gliderChangeEntry{CTime: nowMS, MTime: nowMS, Size: 0, Type: "dir", URI: current})
	}
	return create, existing
}

func gliderTreeFromEntries(rootURI, rootRel string, entries []gliderChangeEntry, maxDepth int) map[string]interface{} {
	name := posixpath.Base(strings.TrimRight(rootURI, "/"))
	if rootRel != "" {
		name = posixpath.Base(rootRel)
	}
	root := map[string]interface{}{"name": name, "type": "directory", "children": []interface{}{}}
	type node map[string]interface{}
	childrenOf := func(n node) []interface{} {
		children, _ := n["children"].([]interface{})
		return children
	}
	nodes := map[string]node{"": root}
	prefix := strings.TrimRight(rootURI, "/") + "/"
	for _, entry := range entries {
		if !strings.HasPrefix(entry.URI, prefix) {
			continue
		}
		rel := strings.TrimPrefix(entry.URI, prefix)
		if rel == "" {
			continue
		}
		parts := strings.Split(rel, "/")
		if maxDepth >= 0 && len(parts) > maxDepth+1 {
			parts = parts[:maxDepth+1]
		}
		parentKey := ""
		for i, part := range parts {
			key := strings.Join(parts[:i+1], "/")
			if _, ok := nodes[key]; ok {
				parentKey = key
				continue
			}
			typeName := "directory"
			if i == len(parts)-1 && entry.Type == "file" {
				typeName = "file"
			}
			n := node{"name": part, "type": typeName}
			if typeName == "directory" {
				n["children"] = []interface{}{}
			}
			parent := nodes[parentKey]
			parent["children"] = append(childrenOf(parent), n)
			nodes[key] = n
			parentKey = key
		}
	}
	sortGliderTree(root)
	return root
}

func sortGliderTree(n map[string]interface{}) {
	children, _ := n["children"].([]interface{})
	sort.SliceStable(children, func(i, j int) bool {
		mi := asMap(children[i])
		mj := asMap(children[j])
		if mi == nil || mj == nil {
			return i < j
		}
		di := stringify(mi["type"]) == "directory"
		dj := stringify(mj["type"]) == "directory"
		if di != dj {
			return di
		}
		return strings.ToLower(stringify(mi["name"])) < strings.ToLower(stringify(mj["name"]))
	})
	n["children"] = children
	for _, child := range children {
		if m := asMap(child); m != nil {
			sortGliderTree(m)
		}
	}
}

func (c *Client) relFromGliderURI(uri string) string {
	root := strings.TrimRight(c.activeAppRootURI(), "/") + "/"
	return strings.TrimPrefix(uri, root)
}

func (c *Client) gliderFileContentMatches(ctx context.Context, uri string, content []byte) bool {
	contents, err := c.fetchV2SyncFiles(ctx, []string{uri})
	if err == nil {
		for _, remote := range contents {
			if string(remote) == string(content) {
				return true
			}
		}
	}
	checksum := sha1Hex(content)
	entries, err := c.fetchGliderStateForURIs(ctx, []string{uri, c.activeAppRootURI()})
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.URI, uri) && strings.EqualFold(entry.Checksum, checksum) && entry.Size == len(content) {
			return true
		}
	}
	return false
}

func (c *Client) gliderDirectoryExists(ctx context.Context, uri string) bool {
	uri = strings.TrimRight(strings.TrimSpace(uri), "/")
	if uri == "" {
		return false
	}
	entries, err := c.fetchGliderStateForURIs(ctx, []string{uri, c.activeAppRootURI()})
	if err != nil {
		return false
	}
	prefix := uri + "/"
	for _, entry := range entries {
		entryURI := strings.TrimRight(entry.URI, "/")
		if strings.EqualFold(entryURI, uri) && (entry.Type == "dir" || entry.Type == "directory" || entry.Type == "") {
			return true
		}
		if strings.HasPrefix(entry.URI, prefix) {
			return true
		}
	}
	return false
}

func (c *Client) gliderPathAbsent(ctx context.Context, uri string) bool {
	entries, err := c.fetchGliderState(ctx, c.activeAppRootURI())
	if err != nil {
		return false
	}
	prefix := strings.TrimRight(uri, "/") + "/"
	for _, entry := range entries {
		if strings.EqualFold(entry.URI, uri) || strings.HasPrefix(entry.URI, prefix) {
			return false
		}
	}
	return true
}

func payloadPath(payload map[string]interface{}) string {
	return strings.TrimSpace(firstString(payload, "path", "filePath", "filename"))
}

func (c *Client) currentIDEContext() map[string]interface{} {
	ctx := emptyIDEContext()
	appID := c.activeAppSysID()
	if appID == "" {
		return ctx
	}
	// The Nirvana schema mirrors the Web IDE extension's getIDEContext(), where
	// workspaceFolders is an array of URI paths (strings), not folder objects.
	// For now-file:/<app_sys_id>/... the VS Code URI path is /<app_sys_id>/...
	appPath := "/" + strings.TrimLeft(appID, "/")
	ctx["workspaceFolders"] = []string{appPath}
	ctx["currentDir"] = appPath
	if scope := c.activeAppScopeName(); scope != "" {
		ctx["scopeName"] = scope
	}
	return ctx
}

func (c *Client) activeAppScopeName() string {
	if c.currentApp != nil {
		if scope := serviceNowScopeFromApp(*c.currentApp); scope != "" {
			return scope
		}
	}
	if app := appFromPayload(asMap(c.appScope)); app != nil {
		return serviceNowScopeFromApp(*app)
	}
	return ""
}

func serviceNowScopeFromApp(app AppScope) string {
	if scope := strings.TrimSpace(app.Scope); scope != "" {
		return scope
	}
	if looksLikeServiceNowScope(app.ScopeName) {
		return strings.TrimSpace(app.ScopeName)
	}
	return ""
}

func looksLikeServiceNowScope(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "global" || strings.HasPrefix(value, "x_")
}

func (c *Client) ensureActiveAppMetadata(ctx context.Context) error {
	if c.currentApp == nil || strings.TrimSpace(c.currentApp.Scope) != "" {
		return nil
	}
	appID := c.activeAppSysID()
	if appID == "" {
		return nil
	}
	if err := c.ensureConversationHTTPClient(); err != nil {
		return err
	}
	if app, err := c.fetchServiceNowAppBySysID(ctx, appID); err == nil {
		c.mergeActiveAppMetadata(app)
		return nil
	} else if c.debug {
		c.debugf("warning: could not resolve sys_app metadata for %s: %v\n", shortConversationID(appID), err)
	}
	if scope, err := c.fetchNowConfigScope(ctx, appID); err == nil && scope != "" {
		c.mergeActiveAppMetadata(AppScope{Scope: scope})
		return nil
	} else if err != nil && c.debug {
		c.debugf("warning: could not resolve now.config scope for %s: %v\n", shortConversationID(appID), err)
	}
	return nil
}

func (c *Client) mergeActiveAppMetadata(update AppScope) {
	if c.currentApp == nil {
		return
	}
	if strings.TrimSpace(update.Scope) != "" {
		c.currentApp.Scope = strings.TrimSpace(update.Scope)
	}
	if strings.TrimSpace(update.ScopeName) != "" && c.currentApp.ScopeName == c.currentApp.ScopeID {
		c.currentApp.ScopeName = strings.TrimSpace(update.ScopeName)
	}
	if strings.TrimSpace(update.AppSysID) != "" {
		c.currentApp.AppSysID = strings.TrimSpace(update.AppSysID)
	}
	if strings.TrimSpace(c.currentApp.AppSysID) == "" {
		c.currentApp.AppSysID = c.currentApp.ScopeID
	}
	_ = saveActiveApp(c.opts.Profile, *c.currentApp)
}

func (c *Client) fetchServiceNowAppBySysID(ctx context.Context, appID string) (AppScope, error) {
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/now/table/sys_app/" + urlPathEscape(appID) + "?sysparm_fields=sys_id,name,scope&sysparm_display_value=true"
	body, _, err := c.getJSON(ctx, endpoint)
	if err != nil {
		return AppScope{}, err
	}
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return AppScope{}, err
	}
	for _, m := range flattenMaps(root) {
		sysID := firstString(m, "sys_id", "sysId")
		if sysID != "" && !strings.EqualFold(sysID, appID) {
			continue
		}
		scope := serviceNowFieldString(m["scope"])
		name := serviceNowFieldString(m["name"])
		if scope != "" || name != "" || sysID != "" {
			if sysID == "" {
				sysID = appID
			}
			return AppScope{ScopeID: sysID, AppSysID: sysID, ScopeName: name, Scope: scope}, nil
		}
	}
	return AppScope{}, fmt.Errorf("sys_app %s did not include name/scope metadata", appID)
}

func (c *Client) fetchNowConfigScope(ctx context.Context, appID string) (string, error) {
	contents, err := c.fetchV2SyncFiles(ctx, []string{"now-file:/" + strings.TrimSpace(appID) + "/now.config.json"})
	if err != nil {
		return "", err
	}
	for _, raw := range contents {
		var cfg map[string]interface{}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			continue
		}
		if scope := strings.TrimSpace(stringify(cfg["scope"])); scope != "" {
			return scope, nil
		}
	}
	return "", errors.New("now.config.json scope not found")
}

func serviceNowFieldString(v interface{}) string {
	if m := asMap(v); m != nil {
		if s := firstString(m, "display_value", "displayValue", "value"); s != "" {
			return s
		}
	}
	return strings.TrimSpace(stringify(v))
}

func urlPathEscape(value string) string {
	return url.PathEscape(strings.TrimSpace(value))
}

func fsToolError(err error) map[string]interface{} {
	msg := err.Error()
	return map[string]interface{}{"content": msg, "error": msg, "code": "TOOL_ERROR"}
}

func searchableGliderFile(uri string) bool {
	lower := strings.ToLower(uri)
	for _, suffix := range []string{".ts", ".tsx", ".js", ".jsx", ".json", ".md", ".txt", ".css", ".scss", ".html"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func intFromPayload(v interface{}, fallback int) int {
	if n := int64FromInterface(v); n > 0 {
		return int(n)
	}
	return fallback
}

func int64FromInterface(v interface{}) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case int32:
		return int64(t)
	case float64:
		return int64(t)
	case float32:
		return int64(t)
	case jsonNumber:
		n, _ := t.Int64()
		return n
	case string:
		var n int64
		_, _ = fmt.Sscanf(t, "%d", &n)
		return n
	default:
		return 0
	}
}

type jsonNumber interface {
	Int64() (int64, error)
}
