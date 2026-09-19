package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	fsEditMaxBytes = 1 << 20
	fsCopyMaxFiles = 200
	fsCopyMaxBytes = 8 << 20
)

type gliderCopyFile struct {
	entry   gliderChangeEntry
	content []byte
}

// answerGliderFSCopy and answerGliderFSMove perform a fully preflighted single
// Glider-VFS mutation. They never use a persistent checkout or local disk.
func (c *Client) answerGliderFSCopy(ctx context.Context, payload map[string]interface{}, move bool) (map[string]interface{}, string) {
	sourcePath := strings.TrimSpace(firstString(payload, "sourcePath", "oldPath", "source", "from"))
	destinationPath := strings.TrimSpace(firstString(payload, "destinationPath", "newPath", "destination", "to"))
	verb, past := "copy", "copied"
	if move {
		verb, past = "move", "moved"
	}
	if sourcePath == "" || destinationPath == "" {
		return fsToolError(fmt.Errorf("fs_%s requires source and destination paths", verb)), "error"
	}
	if strings.HasSuffix(strings.ToLower(sourcePath), ".xml") || strings.HasSuffix(strings.ToLower(destinationPath), ".xml") {
		return xmlBlocked(), "error"
	}
	sourceURI, sourceRel, err := c.resolveGliderPath(sourcePath)
	if err != nil {
		return fsToolError(err), "error"
	}
	destinationURI, destinationRel, err := c.resolveGliderPath(destinationPath)
	if err != nil {
		return fsToolError(err), "error"
	}
	if sourceRel == "" || destinationRel == "" {
		return fsToolError(errors.New("cannot copy or move the app root")), "error"
	}
	if sourceURI == destinationURI {
		return map[string]interface{}{"message": fmt.Sprintf("Source and destination are identical: %s", sourcePath), "sourcePath": sourcePath, "destinationPath": destinationPath, "noOp": true, "ideContext": c.currentIDEContext()}, "complete"
	}
	if err := ctx.Err(); err != nil {
		return fsToolError(err), "error"
	}
	entries, err := c.fetchGliderState(ctx, c.activeAppRootURI())
	if err != nil {
		return fsToolError(err), "error"
	}
	sourcePrefix := strings.TrimRight(sourceURI, "/") + "/"
	destinationPrefix := strings.TrimRight(destinationURI, "/") + "/"
	source := make([]gliderChangeEntry, 0)
	for _, entry := range entries {
		if strings.EqualFold(entry.URI, sourceURI) || strings.HasPrefix(entry.URI, sourcePrefix) {
			if strings.HasSuffix(strings.ToLower(entry.URI), ".xml") {
				return xmlBlocked(), "error"
			}
			source = append(source, entry)
		}
	}
	if len(source) == 0 {
		return fsToolError(fmt.Errorf("source path not found: %s", sourcePath)), "error"
	}
	isDirectory := false
	for _, entry := range source {
		if entry.URI != sourceURI || entry.Type == "dir" || entry.Type == "directory" {
			isDirectory = true
		}
	}
	// Sources may never be copied/moved onto an ancestor or descendant. This is
	// unsafe for files as well as recursive trees: a file can otherwise replace
	// its own parent directory or be written below itself.
	if strings.HasPrefix(destinationURI, sourcePrefix) || strings.HasPrefix(sourceURI, destinationPrefix) {
		return fsToolError(errors.New("source and destination paths overlap")), "error"
	}
	if err := gliderDestinationParentsAreDirectories(entries, c.activeAppRootURI(), destinationURI); err != nil {
		return fsToolError(err), "error"
	}

	existingDestination := make([]gliderChangeEntry, 0)
	for _, entry := range entries {
		if strings.EqualFold(entry.URI, destinationURI) || strings.HasPrefix(entry.URI, destinationPrefix) {
			if strings.HasSuffix(strings.ToLower(entry.URI), ".xml") {
				return xmlBlocked(), "error"
			}
			existingDestination = append(existingDestination, entry)
		}
	}
	overwrite, _ := payload["overwrite"].(bool)
	if len(existingDestination) > 0 && !overwrite {
		return fsToolError(fmt.Errorf("destination already exists: %s", destinationPath)), "error"
	}

	fileEntries := make([]gliderChangeEntry, 0)
	uris := make([]string, 0)
	for _, entry := range source {
		if entry.Type == "file" {
			fileEntries = append(fileEntries, entry)
			uris = append(uris, entry.URI)
		}
	}
	if len(fileEntries) > fsCopyMaxFiles {
		return fsToolError(fmt.Errorf("copy source exceeds %d files", fsCopyMaxFiles)), "error"
	}
	contents, err := c.fetchV2SyncFiles(ctx, uris)
	if err != nil {
		return fsToolError(err), "error"
	}
	files := make([]gliderCopyFile, 0, len(fileEntries))
	totalBytes := 0
	for _, entry := range fileEntries {
		content, ok := gliderEntryContent(contents, entry)
		if !ok {
			return fsToolError(fmt.Errorf("could not read source content: %s", c.relFromGliderURI(entry.URI))), "error"
		}
		if entry.Checksum != "" && !strings.EqualFold(sha1Hex(content), entry.Checksum) {
			return fsToolError(fmt.Errorf("source content checksum mismatch: %s", c.relFromGliderURI(entry.URI))), "error"
		}
		if len(content) > fsEditMaxBytes || totalBytes > fsCopyMaxBytes-len(content) {
			return fsToolError(errors.New("copy source exceeds safe content limit")), "error"
		}
		totalBytes += len(content)
		files = append(files, gliderCopyFile{entry: entry, content: content})
	}
	if err := ctx.Err(); err != nil {
		return fsToolError(err), "error"
	}

	now := time.Now().UnixMilli()
	desired := make([]gliderChangeEntry, 0, len(source)+8)
	expectedFiles := make(map[string][]byte, len(files))
	expectedDirs := map[string]bool{}
	if isDirectory {
		expectedDirs[destinationURI] = true
	}
	blobsByChecksum := map[string]gliderFileBlob{}
	for _, entry := range source {
		targetURI := destinationURI + strings.TrimPrefix(entry.URI, sourceURI)
		if entry.Type == "file" {
			var content []byte
			for _, file := range files {
				if file.entry.URI == entry.URI {
					content = file.content
					break
				}
			}
			if content == nil {
				return fsToolError(fmt.Errorf("source content disappeared: %s", c.relFromGliderURI(entry.URI))), "error"
			}
			checksum := sha1Hex(content)
			desired = append(desired, gliderChangeEntry{Checksum: checksum, CTime: now, MTime: now, Size: len(content), Type: "file", URI: targetURI})
			expectedFiles[targetURI] = content
			blobsByChecksum[checksum] = gliderFileBlob{Path: checksum, Checksum: checksum, Content: content}
		} else {
			expectedDirs[targetURI] = true
		}
		for parent := gliderURIParent(targetURI); parent != destinationURI && strings.HasPrefix(parent, destinationPrefix); parent = gliderURIParent(parent) {
			expectedDirs[parent] = true
		}
	}
	for dir := range expectedDirs {
		desired = append(desired, gliderChangeEntry{CTime: now, MTime: now, Size: 0, Type: "dir", URI: dir})
	}
	// State was fetched above. Deriving parents from that same snapshot avoids
	// treating a swallowed sync/state error as authority for a mutation decision.
	desired = append(desired, c.gliderMissingParentDirsFromState(entries, destinationURI)...)
	desired = uniqueGliderEntries(desired)
	existingByURI := make(map[string]gliderChangeEntry, len(entries))
	for _, entry := range entries {
		existingByURI[entry.URI] = entry
	}
	desiredByURI := make(map[string]gliderChangeEntry, len(desired))
	for _, entry := range desired {
		desiredByURI[entry.URI] = entry
	}
	create := make([]gliderChangeEntry, 0, len(desired))
	update := make([]gliderChangeEntry, 0, len(desired))
	destinationRemove := make([]gliderChangeEntry, 0, len(existingDestination))
	for _, entry := range desired {
		existing, exists := existingByURI[entry.URI]
		if !exists {
			create = append(create, entry)
			continue
		}
		if gliderEntryTypesMatch(existing, entry) {
			if entry.Type == "file" {
				update = append(update, entry)
			}
			continue // matching directories are retained, never remove+create.
		}
		// A type conflict must be removed before its replacement is created.
		// Keep them in separate apply calls so a multipart payload never names
		// one destination URI in both create and remove.
		destinationRemove = append(destinationRemove, existing)
		create = append(create, entry)
	}
	// Any existing destination descendant not represented by the source tree is
	// stale under overwrite and must be removed deepest-first.
	for _, existing := range existingDestination {
		if _, wanted := desiredByURI[existing.URI]; !wanted {
			destinationRemove = append(destinationRemove, existing)
		}
	}
	create = uniqueGliderEntries(create)
	update = uniqueGliderEntries(update)
	destinationRemove = uniqueGliderEntries(destinationRemove)
	sourceRemove := uniqueGliderEntries(append([]gliderChangeEntry(nil), source...))
	sort.Slice(create, func(i, j int) bool { return create[i].URI < create[j].URI })
	sort.Slice(update, func(i, j int) bool { return update[i].URI < update[j].URI })
	sort.Slice(destinationRemove, func(i, j int) bool {
		di, dj := strings.Count(destinationRemove[i].URI, "/"), strings.Count(destinationRemove[j].URI, "/")
		if di != dj {
			return di > dj
		}
		return destinationRemove[i].URI < destinationRemove[j].URI
	})
	sort.Slice(sourceRemove, func(i, j int) bool {
		di, dj := strings.Count(sourceRemove[i].URI, "/"), strings.Count(sourceRemove[j].URI, "/")
		if di != dj {
			return di > dj
		}
		return sourceRemove[i].URI < sourceRemove[j].URI
	})
	blobs := make([]gliderFileBlob, 0, len(blobsByChecksum))
	for _, blob := range blobsByChecksum {
		blobs = append(blobs, blob)
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].Checksum < blobs[j].Checksum })
	// Remove stale/type-conflicting destination paths first. Do not remove the
	// move source here: a subsequent destination write failure must leave it
	// intact.
	if len(destinationRemove) > 0 {
		if err := c.applyGliderChanges(ctx, nil, nil, destinationRemove, nil); err != nil {
			return fsToolError(err), "error"
		}
	}
	if len(create) == 0 && len(update) == 0 {
		if !c.gliderCopyTreeVerified(ctx, sourceURI, destinationURI, expectedFiles, expectedDirs, existingDestination, false) {
			return fsToolError(errors.New("Glider apply completed but remote copy verification failed")), "error"
		}
		if move {
			// No destination write is required. Remove a source only after the
			// destination is known to be a complete exact copy.
			if err := c.applyGliderChanges(ctx, nil, nil, sourceRemove, nil); err != nil {
				if c.gliderCopyTreeVerified(ctx, sourceURI, destinationURI, expectedFiles, expectedDirs, existingDestination, true) {
					return c.fsCopySuccess(past, sourcePath, destinationPath, true), "complete"
				}
				return fsToolError(err), "error"
			}
		}
		if !c.gliderCopyTreeVerified(ctx, sourceURI, destinationURI, expectedFiles, expectedDirs, existingDestination, move) {
			return fsToolError(errors.New("Glider apply completed but remote copy verification failed")), "error"
		}
		return c.fsCopySuccess(past, sourcePath, destinationPath, false), "complete"
	}
	// Source and destination are preflighted as disjoint, so a move can remove
	// its source in the same apply as the successful destination write.
	applyRemove := []gliderChangeEntry(nil)
	if move {
		applyRemove = sourceRemove
	}
	if err := c.applyGliderChanges(ctx, create, update, applyRemove, blobs); err != nil {
		if c.gliderCopyTreeVerified(ctx, sourceURI, destinationURI, expectedFiles, expectedDirs, existingDestination, move) {
			return c.fsCopySuccess(past, sourcePath, destinationPath, true), "complete"
		}
		return fsToolError(err), "error"
	}
	if !c.gliderCopyTreeVerified(ctx, sourceURI, destinationURI, expectedFiles, expectedDirs, existingDestination, move) {
		return fsToolError(errors.New("Glider apply completed but remote copy verification failed")), "error"
	}
	return c.fsCopySuccess(past, sourcePath, destinationPath, false), "complete"
}

// gliderDestinationParentsAreDirectories rejects writes below an existing file.
// A missing parent or a parent implied only by descendants is a directory; an
// exact file entry is not.
func gliderDestinationParentsAreDirectories(entries []gliderChangeEntry, rootURI, destinationURI string) error {
	rootURI = strings.TrimRight(rootURI, "/")
	for parent := gliderURIParent(destinationURI); parent != "" && parent != rootURI; parent = gliderURIParent(parent) {
		for _, entry := range entries {
			if strings.EqualFold(entry.URI, parent) && entry.Type == "file" {
				return fmt.Errorf("destination parent is a file: %s", parent)
			}
		}
	}
	return nil
}

func gliderURIParent(uri string) string {
	trimmed := strings.TrimRight(uri, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}

func (c *Client) gliderMissingParentDirsFromState(entries []gliderChangeEntry, destinationURI string) []gliderChangeEntry {
	root := strings.TrimRight(c.activeAppRootURI(), "/")
	parents := make([]string, 0)
	for parent := gliderURIParent(destinationURI); parent != "" && parent != root; parent = gliderURIParent(parent) {
		parents = append(parents, parent)
	}
	// Build roots first so the multipart payload is deterministic and valid for
	// remote implementations that require a parent before its child.
	sort.Strings(parents)
	now := time.Now().UnixMilli()
	missing := make([]gliderChangeEntry, 0, len(parents))
	for _, parent := range parents {
		exists := false
		prefix := strings.TrimRight(parent, "/") + "/"
		for _, entry := range entries {
			if strings.EqualFold(entry.URI, parent) || strings.HasPrefix(entry.URI, prefix) {
				exists = true
				break
			}
		}
		if !exists {
			missing = append(missing, gliderChangeEntry{CTime: now, MTime: now, Size: 0, Type: "dir", URI: parent})
		}
	}
	return missing
}

func gliderEntryTypesMatch(existing, desired gliderChangeEntry) bool {
	if desired.Type == "file" {
		return existing.Type == "file"
	}
	return existing.Type == "dir" || existing.Type == "directory"
}

func uniqueGliderEntries(entries []gliderChangeEntry) []gliderChangeEntry {
	seen := map[string]bool{}
	out := make([]gliderChangeEntry, 0, len(entries))
	for _, entry := range entries {
		if !seen[entry.URI] {
			seen[entry.URI] = true
			out = append(out, entry)
		}
	}
	return out
}

func xmlBlocked() map[string]interface{} {
	return map[string]interface{}{"error": "Operations on XML files are not allowed", "code": "XML_BLOCKED"}
}

func (c *Client) fsCopySuccess(past, source, destination string, recovered bool) map[string]interface{} {
	result := map[string]interface{}{"message": fmt.Sprintf("Successfully %s %s to %s", past, source, destination), "sourcePath": source, "destinationPath": destination, "ideContext": c.currentIDEContext()}
	if recovered {
		result["warning"] = "Glider sync returned an error after mutation; verified remote state matches."
	}
	return result
}

func gliderEntryContent(contents map[string][]byte, entry gliderChangeEntry) ([]byte, bool) {
	if content, ok := contents[entry.URI]; ok {
		return content, true
	}
	// sync/files is normally keyed by the entry checksum. Do not fall back to
	// an arbitrary singleton: that could silently substitute another source
	// file when a requested response entry is missing.
	content, ok := contents[entry.Checksum]
	return content, ok
}

func (c *Client) gliderCopyTreeVerified(ctx context.Context, sourceURI, destinationURI string, expectedFiles map[string][]byte, expectedDirs map[string]bool, oldDestination []gliderChangeEntry, moved bool) bool {
	entries, err := c.fetchGliderState(ctx, c.activeAppRootURI())
	if err != nil {
		return false
	}
	actual := map[string]gliderChangeEntry{}
	for _, entry := range entries {
		actual[entry.URI] = entry
	}
	for uri, content := range expectedFiles {
		entry, exists := actual[uri]
		if !exists || entry.Type != "file" {
			return false
		}
		if !c.gliderFileContentMatches(ctx, uri, content) {
			return false
		}
	}
	for dir := range expectedDirs {
		entry, exists := actual[dir]
		if !exists || (entry.Type != "dir" && entry.Type != "directory") {
			return false
		}
	}
	for _, stale := range oldDestination {
		if _, expectedFile := expectedFiles[stale.URI]; expectedFile {
			continue
		}
		if expectedDirs[stale.URI] {
			continue
		}
		if _, stillPresent := actual[stale.URI]; stillPresent {
			return false
		}
	}
	return !moved || c.gliderPathAbsent(ctx, sourceURI)
}

func (c *Client) answerGliderFSFindAndReplace(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	path := payloadPath(payload)
	search, searchOK := payload["search"].(string)
	replace, replaceOK := payload["replace"].(string)
	if path == "" || !searchOK || !replaceOK {
		return fsToolError(errors.New("fs_find_and_replace requires path, search, and replace strings")), "error"
	}
	if strings.HasSuffix(strings.ToLower(path), ".xml") {
		return xmlBlocked(), "error"
	}
	if search == "" { // whitespace-only values are valid exact searches in Web.
		return fsToolError(errors.New("search string cannot be empty")), "error"
	}
	uri, _, err := c.resolveGliderPath(path)
	if err != nil {
		return fsToolError(err), "error"
	}
	contents, err := c.fetchV2SyncFiles(ctx, []string{uri})
	if err != nil {
		return fsToolError(fmt.Errorf("file not found: %s", path)), "error"
	}
	old, ok := contents[uri]
	if !ok && len(contents) == 1 {
		for _, content := range contents {
			old, ok = content, true
		}
	}
	if !ok {
		return fsToolError(fmt.Errorf("file not found: %s", path)), "error"
	}
	if len(old) > fsEditMaxBytes {
		return fsToolError(errors.New("file exceeds safe find-and-replace input limit")), "error"
	}
	replaceAll, _ := payload["replaceAll"].(bool)
	caseSensitive := true
	if v, ok := payload["caseSensitive"].(bool); ok {
		caseSensitive = v
	}
	useRegex, _ := payload["useRegex"].(bool)
	pattern := search
	if !useRegex {
		pattern = regexp.QuoteMeta(pattern)
	}
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fsToolError(fmt.Errorf("invalid search regex: %w", err)), "error"
	}
	matches := re.FindAllIndex(old, -1)
	if len(matches) == 0 {
		return fsToolError(fmt.Errorf("No matches found for %q in %s", search, path)), "error"
	}
	count := len(matches)
	if !replaceAll {
		count = 1
		matches = matches[:1]
	}
	var updated []byte
	if useRegex {
		// Web's regex String.replace expands $&, while Go calls the full match ${0}.
		replacement := []byte(strings.ReplaceAll(replace, "$&", "${0}"))
		if replaceAll {
			updated = re.ReplaceAll(old, replacement)
		} else {
			updated = append(updated, old[:matches[0][0]]...)
			updated = re.Expand(updated, replacement, old, matches[0])
			updated = append(updated, old[matches[0][1]:]...)
		}
	} else {
		// Exact replacement is literal: no capture expansion can reinterpret $1.
		updated = replaceGliderLiteralMatches(old, matches, []byte(replace))
	}
	if len(updated) > fsCopyMaxBytes {
		return fsToolError(errors.New("replacement output exceeds safe size limit")), "error"
	}
	result, status := c.answerGliderFSWriteFile(ctx, map[string]interface{}{"path": path, "data": string(updated)})
	if status != "complete" {
		return result, status
	}
	if !c.gliderFileContentMatches(ctx, uri, updated) {
		return fsToolError(errors.New("remote write verification failed")), "error"
	}
	result["message"] = fmt.Sprintf("Successfully replaced %d occurrence%s in %s", count, plural(count), path)
	result["replacements"] = count
	return result, "complete"
}

func replaceGliderLiteralMatches(old []byte, matches [][]int, replacement []byte) []byte {
	out := make([]byte, 0, len(old)+len(matches)*len(replacement))
	last := 0
	for _, match := range matches {
		out = append(out, old[last:match[0]]...)
		out = append(out, replacement...)
		last = match[1]
	}
	return append(out, old[last:]...)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (c *Client) answerOpenApp(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string, error) {
	id := strings.TrimSpace(firstString(payload, "appId", "appSysId", "app_sys_id", "scopeId"))
	if id == "" {
		return map[string]interface{}{"error": "open_app requires appId", "code": "NO_APP_ID"}, "error", nil
	}
	// Match Web's sys_app validation before touching selected-app persistence.
	app, err := c.fetchServiceNowAppBySysID(ctx, id)
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("Failed to open app %s: %v", id, err), "code": "APP_LOOKUP_FAILED"}, "error", nil
	}
	if strings.TrimSpace(app.AppSysID) == "" {
		app.AppSysID = id
	}
	if strings.TrimSpace(app.ScopeID) == "" {
		app.ScopeID = app.AppSysID
	}
	previousApp := cloneAppScope(c.currentApp)
	previousAppScope := c.appScope
	previousStatusProjectPath := c.cachedStatusProjectPath()
	if err := c.SetApp(app); err != nil {
		c.currentApp = previousApp
		c.appScope = previousAppScope
		c.setStatusProjectPath(previousStatusProjectPath)
		return nil, "error", err
	}
	return map[string]interface{}{"message": fmt.Sprintf("%s selected as the active bacli application.", app.ScopeName), "appId": app.AppSysID, "appName": app.ScopeName, "workspaceOpened": false, "conversionPerformed": false, "ideContext": c.currentIDEContext()}, "complete", nil
}

func cloneAppScope(app *AppScope) *AppScope {
	if app == nil {
		return nil
	}
	clone := *app
	return &clone
}

func (c *Client) answerUIDiagnostics(payload map[string]interface{}) (map[string]interface{}, string) {
	path, ok := payload["path"].(string)
	path = strings.TrimSpace(path)
	if !ok || path == "" {
		return fsToolError(errors.New("ui_diagnostics requires path")), "error"
	}
	return map[string]interface{}{"path": path, "success": false, "previewAvailable": false, "terminalClient": true, "consoleMessages": []interface{}{}, "networkRequests": []interface{}{}, "error": "UI preview diagnostics are unavailable in the bacli terminal client; no browser panel was opened.", "code": "UI_DIAGNOSTICS_UNAVAILABLE"}, "error"
}

func (c *Client) answerMCPManagement(ctx context.Context, action string, payload map[string]interface{}) (map[string]interface{}, string) {
	// Connect/disconnect are stable terminal-client limitations. Validate their
	// payloads before any server discovery so they fail quickly offline.
	if action == "connect_to_mcp_server" || action == "disconnect_from_mcp_server" {
		id := strings.TrimSpace(firstString(payload, "serverId", "server_id"))
		if id == "" {
			return map[string]interface{}{"error": fmt.Sprintf("%s requires serverId", action), "code": "MISSING_SERVER_ID"}, "error"
		}
		if action == "connect_to_mcp_server" {
			for _, key := range []string{"name", "url", "transport"} {
				if strings.TrimSpace(stringify(payload[key])) == "" {
					return map[string]interface{}{"error": fmt.Sprintf("connect_to_mcp_server requires %s", key), "code": "MISSING_MCP_CONFIGURATION"}, "error"
				}
			}
			transport := strings.ToLower(strings.TrimSpace(stringify(payload["transport"])))
			if transport != "sse" && transport != "streamable-http" && transport != "pseudo" {
				return map[string]interface{}{"error": "Invalid transport type: " + transport, "code": "INVALID_MCP_TRANSPORT"}, "error"
			}
		}
		return map[string]interface{}{"error": "bacli does not own MCP client transports; connection state is configured and managed by the ServiceNow backend.", "code": "MCP_CLIENT_TRANSPORT_UNAVAILABLE", "serverId": id, "managedBy": "ServiceNow backend/Nirvana handshake"}, "error"
	}

	servers := c.nirvanaMCPServerPayload(ctx)
	if action == "list_mcp_servers" {
		return mcpServerListing(servers), "complete"
	}
	if action == "list_mcp_tools" {
		return c.answerMCPToolsList(ctx, servers, payload)
	}
	return map[string]interface{}{"error": fmt.Sprintf("unsupported MCP management action: %s", action), "code": "UNEXPECTED_CLIENT_ACTION"}, "error"
}
