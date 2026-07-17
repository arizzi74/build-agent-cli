package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	posixpath "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	diagnosticsMaxFiles = 10
)

var tscDiagnosticLineRE = regexp.MustCompile(`^(.+?)\((\d+),(\d+)\):\s+error\s+(TS\d+):\s+(.*)$`)

func (c *Client) answerGliderRunDiagnostics(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	requested, err := diagnosticsFilesFromPayload(payload)
	if err != nil {
		return diagnosticsError(err.Error(), "INVALID_ARGUMENT", nil), "error"
	}
	if len(requested) > diagnosticsMaxFiles {
		return diagnosticsError(fmt.Sprintf("Error: Too many files. Requested %d, maximum allowed is %d. Use the build tool for larger sets.", len(requested), diagnosticsMaxFiles), "TOO_MANY_FILES", map[string]interface{}{"maxFiles": diagnosticsMaxFiles}), "error"
	}

	project, cleanup, err := c.syncGliderProjectToTemp(ctx)
	if err != nil {
		return diagnosticsError(fmt.Sprintf("Error while syncing Glider project for diagnostics: %v", err), "SYNC_FAILED", nil), "error"
	}
	defer cleanup()

	valid, invalid := validateDiagnosticFileRequests(requested, project)
	if len(valid) == 0 {
		reason := "No valid files to diagnose."
		if len(invalid) > 0 {
			unique := uniqueInvalidReasons(invalid)
			if len(unique) == 1 {
				reason = unique[0]
			} else {
				reason = "Multiple issues: " + strings.Join(unique, "; ")
			}
		}
		files := make([]string, 0, len(invalid))
		for _, item := range invalid {
			files = append(files, item.File)
		}
		msg := fmt.Sprintf("Error: All files invalid. %s. Files: %s", reason, strings.Join(files, ", "))
		return diagnosticsError(msg, "ALL_FILES_INVALID", map[string]interface{}{"invalidFiles": invalid, "supportedExtensions": supportedDiagnosticExtensions()}), "error"
	}

	// Diagnostics runs in an isolated, remote-sourced temp project. Reuse an
	// already-installed dependency tree only when it belongs to this exact active
	// app; diagnostics must never materialize a checkout or install dependencies.
	c.attachTrustedDiagnosticsDependencies(ctx, project)

	tscPath := findTSC(project.Dir)
	if tscPath == "" {
		msg := "TypeScript compiler 'tsc' was not found in PATH or project node_modules/.bin; local diagnostics could not run."
		fmt.Fprintf(os.Stderr, "run_diagnostics: %s\n", msg)
		return diagnosticsError(msg, "TSC_NOT_FOUND", map[string]interface{}{"syncedFiles": len(project.Files), "diagnosedFiles": valid}), "error"
	}

	args := tscDiagnosticArgs(valid)
	cmd := exec.CommandContext(ctx, tscPath, args...)
	cmd.Dir = project.Dir
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "FORCE_COLOR=0")
	output, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		msg := fmt.Sprintf("Error while running diagnostics: %v", ctx.Err())
		return diagnosticsError(msg, "DIAGNOSTICS_TIMEOUT", nil), "error"
	}

	diagnostics := parseTSCDiagnostics(output, project.Dir, valid)
	warning := diagnosticsSkippedWarning(invalid)
	if runErr == nil && len(diagnostics) == 0 {
		msg := fmt.Sprintf("%sNo errors found in %d file(s).", warning, len(valid))
		return map[string]interface{}{
			"result":      strings.TrimSpace(msg),
			"diagnostics": diagnostics,
			"warnings":    diagnosticWarningValues(invalid),
			"ideContext":  c.currentIDEContext(),
		}, "complete"
	}
	if len(diagnostics) == 0 {
		out := singleLineLabel(string(bytes.TrimSpace(output)))
		if out == "" {
			out = runErr.Error()
		}
		msg := fmt.Sprintf("Error while running diagnostics: %s", out)
		return diagnosticsError(msg, "DIAGNOSTICS_FAILED", map[string]interface{}{"output": string(bytes.TrimSpace(output)), "tool": filepath.Base(tscPath), "args": args}), "error"
	}

	result := formatDiagnosticsFailure(warning, diagnostics)
	return diagnosticsError(result, "DIAGNOSTICS_ERRORS", map[string]interface{}{"diagnostics": diagnostics, "warnings": diagnosticWarningValues(invalid), "tool": filepath.Base(tscPath)}), "error"
}

type syncedGliderProject struct {
	Dir   string
	Files map[string]string // normalized relative path -> local file path
}

func (c *Client) syncGliderProjectToTemp(ctx context.Context) (syncedGliderProject, func(), error) {
	rootURI := c.activeAppRootURI()
	if rootURI == "" {
		return syncedGliderProject{}, func() {}, errors.New("Tool requires an application to have been created or selected")
	}
	entries, err := c.fetchGliderState(ctx, rootURI)
	if err != nil {
		return syncedGliderProject{}, func() {}, err
	}
	tmp, err := os.MkdirTemp("", "ba-glider-diagnostics-*")
	if err != nil {
		return syncedGliderProject{}, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	project := syncedGliderProject{Dir: tmp, Files: map[string]string{}}

	fileEntries := make([]gliderChangeEntry, 0, len(entries))
	for _, entry := range entries {
		if !gliderStateEntryIsFile(entry) {
			continue
		}
		if _, ok := normalizeDiagnosticRel(c.relFromGliderURI(entry.URI)); !ok {
			continue
		}
		fileEntries = append(fileEntries, entry)
	}
	sort.Slice(fileEntries, func(i, j int) bool { return fileEntries[i].URI < fileEntries[j].URI })

	const batchSize = 80
	for start := 0; start < len(fileEntries); start += batchSize {
		end := start + batchSize
		if end > len(fileEntries) {
			end = len(fileEntries)
		}
		batch := fileEntries[start:end]
		uris := make([]string, 0, len(batch))
		for _, entry := range batch {
			uris = append(uris, entry.URI)
		}
		contents, err := c.fetchV2SyncFiles(ctx, uris)
		if err != nil {
			cleanup()
			return syncedGliderProject{}, func() {}, err
		}
		for _, entry := range batch {
			rel, ok := normalizeDiagnosticRel(c.relFromGliderURI(entry.URI))
			if !ok {
				continue
			}
			content, ok := gliderFetchedContentForEntry(contents, entry)
			if !ok {
				continue
			}
			localPath, err := safeDiagnosticLocalPath(tmp, rel)
			if err != nil {
				cleanup()
				return syncedGliderProject{}, func() {}, err
			}
			if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
				cleanup()
				return syncedGliderProject{}, func() {}, err
			}
			if err := os.WriteFile(localPath, content, 0o644); err != nil {
				cleanup()
				return syncedGliderProject{}, func() {}, err
			}
			project.Files[rel] = localPath
		}
	}
	return project, cleanup, nil
}

func gliderStateEntryIsFile(entry gliderChangeEntry) bool {
	typeName := strings.ToLower(strings.TrimSpace(entry.Type))
	if typeName == "file" {
		return true
	}
	if typeName == "" && entry.Checksum != "" {
		return true
	}
	return false
}

func gliderFetchedContentForEntry(contents map[string][]byte, entry gliderChangeEntry) ([]byte, bool) {
	for _, key := range []string{entry.Checksum, entry.URI, posixpath.Base(entry.URI)} {
		if key == "" {
			continue
		}
		if content, ok := contents[key]; ok {
			return content, true
		}
	}
	return nil, false
}

type invalidDiagnosticFile struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

func diagnosticsFilesFromPayload(payload map[string]interface{}) ([]string, error) {
	raw, ok := payload["files"]
	if !ok || raw == nil {
		if one := payloadPath(payload); one != "" {
			return []string{one}, nil
		}
		return nil, errors.New(`Error: Missing required parameter "files"`)
	}
	var out []string
	switch typed := raw.(type) {
	case []string:
		out = append(out, typed...)
	case []interface{}:
		for _, item := range typed {
			s, ok := item.(string)
			if !ok {
				out = append(out, fmt.Sprintf("%v", item))
				continue
			}
			out = append(out, s)
		}
	default:
		return nil, fmt.Errorf("Error: Parameter \"files\" must be an array. Received: %T", raw)
	}
	if len(out) == 0 {
		return nil, errors.New(`Error: No files provided. The "files" array is empty.`)
	}
	return out, nil
}

func validateDiagnosticFileRequests(files []string, project syncedGliderProject) ([]string, []invalidDiagnosticFile) {
	valid := make([]string, 0, len(files))
	invalid := make([]invalidDiagnosticFile, 0)
	seen := map[string]struct{}{}
	for _, file := range files {
		rel, ok := normalizeDiagnosticRel(file)
		if !ok || rel == "" {
			invalid = append(invalid, invalidDiagnosticFile{File: displayDiagnosticFile(file), Reason: "File path is empty or escapes the active app"})
			continue
		}
		if !diagnosticExtensionSupported(rel) {
			invalid = append(invalid, invalidDiagnosticFile{File: rel, Reason: "Unsupported file type. Expected: " + strings.Join(supportedDiagnosticExtensions(), ", ")})
			continue
		}
		localPath, exists := project.Files[rel]
		if !exists {
			invalid = append(invalid, invalidDiagnosticFile{File: rel, Reason: "File not found or inaccessible"})
			continue
		}
		if info, err := os.Stat(localPath); err != nil || info.IsDir() {
			invalid = append(invalid, invalidDiagnosticFile{File: rel, Reason: "Path is a directory, not a file"})
			continue
		}
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		valid = append(valid, rel)
	}
	return valid, invalid
}

func supportedDiagnosticExtensions() []string {
	return []string{".ts", ".tsx", ".js", ".jsx"}
}

func diagnosticExtensionSupported(rel string) bool {
	lower := strings.ToLower(rel)
	for _, ext := range supportedDiagnosticExtensions() {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func normalizeDiagnosticRel(path string) (string, bool) {
	p := strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	p = strings.TrimPrefix(p, "now-file:")
	p = strings.TrimLeft(p, "/")
	p = posixpath.Clean("/" + p)
	p = strings.TrimLeft(p, "/")
	if p == "." {
		p = ""
	}
	if p == "" || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || strings.Contains(p, "\x00") {
		return p, false
	}
	return p, true
}

func displayDiagnosticFile(file string) string {
	if strings.TrimSpace(file) == "" {
		return "(empty string)"
	}
	return file
}

func uniqueInvalidReasons(invalid []invalidDiagnosticFile) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, item := range invalid {
		if item.Reason == "" {
			continue
		}
		if _, ok := seen[item.Reason]; ok {
			continue
		}
		seen[item.Reason] = struct{}{}
		out = append(out, item.Reason)
	}
	sort.Strings(out)
	return out
}

func safeDiagnosticLocalPath(root, rel string) (string, error) {
	rel, ok := normalizeDiagnosticRel(rel)
	if !ok {
		return "", fmt.Errorf("unsafe diagnostic path: %s", rel)
	}
	parts := strings.Split(rel, "/")
	joined := filepath.Join(append([]string{root}, parts...)...)
	rootClean, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	joinedClean, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	if joinedClean != rootClean && !strings.HasPrefix(joinedClean, rootClean+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes diagnostic project: %s", rel)
	}
	return joinedClean, nil
}

// attachTrustedDiagnosticsDependencies makes an already-present node_modules
// available to the isolated diagnostics project. It is deliberately best-effort:
// an absent, stale, or unsupported local tree leaves diagnostics unchanged rather
// than copying packages or installing anything. The temporary project is removed
// by its caller, so the symlink cannot alter the source checkout.
func (c *Client) attachTrustedDiagnosticsDependencies(ctx context.Context, project syncedGliderProject) {
	if err := ctx.Err(); err != nil {
		return
	}
	target := filepath.Join(project.Dir, "node_modules")
	if _, err := os.Lstat(target); err == nil {
		// Remote state may exceptionally include node_modules. Never replace it.
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		return
	}

	for _, source := range c.trustedDiagnosticsDependencySources() {
		if err := ctx.Err(); err != nil {
			return
		}
		nodeModules := filepath.Join(source, "node_modules")
		if !hasDiagnosticsSDKDependency(nodeModules) {
			continue
		}
		if err := os.Symlink(nodeModules, target); err == nil {
			return
		}
		// Directory symlinks can require additional privileges on Windows. Do not
		// copy or install as a fallback: local dependencies are optional here and
		// diagnostics must stay non-materializing and read-only.
	}
}

// trustedDiagnosticsDependencySources returns only dependency trees known to
// belong to the active app. The checkout resolver is read-only: it neither
// repairs registry state nor changes the selected checkout.
func (c *Client) trustedDiagnosticsDependencySources() []string {
	appID := strings.TrimSpace(c.activeAppSysID())
	if appID == "" {
		return nil
	}
	rootURI := "now-file:/" + strings.TrimLeft(appID, "/")
	seen := map[string]struct{}{}
	var sources []string
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		clean := filepath.Clean(dir)
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		sources = append(sources, clean)
	}

	if resolution, err := c.resolveFluentDocsLocalCheckout(appID, c.persistentAppDisplayName(appID), rootURI); err == nil {
		add(resolution.Dir)
	}
	if build := c.lastGliderBuild; build != nil && build.TempDir != "" &&
		strings.EqualFold(strings.TrimSpace(build.AppID), appID) &&
		strings.EqualFold(strings.TrimSpace(build.RootURI), rootURI) {
		add(build.TempDir)
	}
	return sources
}

func hasDiagnosticsSDKDependency(nodeModules string) bool {
	info, err := os.Stat(filepath.Join(nodeModules, "@servicenow", "sdk", "package.json"))
	return err == nil && !info.IsDir()
}

func findTSC(projectDir string) string {
	local := filepath.Join(projectDir, "node_modules", ".bin", "tsc")
	if info, err := os.Stat(local); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return local
	}
	if path, err := exec.LookPath("tsc"); err == nil {
		return path
	}
	return ""
}

func tscDiagnosticArgs(files []string) []string {
	args := []string{
		"--noEmit",
		"--pretty", "false",
		"--skipLibCheck",
		"--module", "es2022",
		"--target", "es2022",
		"--moduleResolution", "bundler",
		"--allowJs",
		"--checkJs",
		"--jsx", "react-jsx",
	}
	for _, file := range files {
		args = append(args, filepath.FromSlash(file))
	}
	return args
}

func parseTSCDiagnostics(output []byte, projectDir string, requested []string) map[string][]map[string]interface{} {
	requestedSet := map[string]struct{}{}
	for _, file := range requested {
		if rel, ok := normalizeDiagnosticRel(file); ok {
			requestedSet[rel] = struct{}{}
		}
	}
	diagnostics := map[string][]map[string]interface{}{}
	for _, rawLine := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		matches := tscDiagnosticLineRE.FindStringSubmatch(line)
		if len(matches) != 6 {
			continue
		}
		rel := normalizeTSCOutputPath(matches[1], projectDir)
		if _, wanted := requestedSet[rel]; !wanted {
			continue
		}
		lineNo, _ := strconv.Atoi(matches[2])
		columnNo, _ := strconv.Atoi(matches[3])
		diagnostics[rel] = append(diagnostics[rel], map[string]interface{}{
			"line":    lineNo,
			"column":  columnNo,
			"message": strings.TrimSpace(matches[5]),
			"source":  matches[4],
		})
	}
	return diagnostics
}

func normalizeTSCOutputPath(rawPath, projectDir string) string {
	p := strings.Trim(strings.TrimSpace(rawPath), `"'`)
	p = filepath.Clean(p)
	if filepath.IsAbs(p) {
		if rel, err := filepath.Rel(projectDir, p); err == nil {
			p = rel
		}
	}
	p = filepath.ToSlash(p)
	if rel, ok := normalizeDiagnosticRel(p); ok {
		return rel
	}
	return p
}

func diagnosticsSkippedWarning(invalid []invalidDiagnosticFile) string {
	if len(invalid) == 0 {
		return ""
	}
	files := make([]string, 0, len(invalid))
	for _, item := range invalid {
		files = append(files, item.File)
	}
	return fmt.Sprintf("Warning: Skipped %d invalid file(s): %s. ", len(invalid), strings.Join(files, ", "))
}

func diagnosticWarningValues(invalid []invalidDiagnosticFile) []interface{} {
	if len(invalid) == 0 {
		return nil
	}
	warnings := make([]interface{}, 0, len(invalid))
	for _, item := range invalid {
		warnings = append(warnings, map[string]interface{}{"file": item.File, "reason": item.Reason})
	}
	return warnings
}

func formatDiagnosticsFailure(warning string, diagnostics map[string][]map[string]interface{}) string {
	files := make([]string, 0, len(diagnostics))
	for file := range diagnostics {
		files = append(files, file)
	}
	sort.Strings(files)
	summary := make([]string, 0, len(files))
	for _, file := range files {
		summary = append(summary, fmt.Sprintf("%s: %d error(s)", file, len(diagnostics[file])))
	}
	details, _ := json.MarshalIndent(diagnostics, "", "  ")
	return fmt.Sprintf("%sFound errors in %d file(s):\n  %s\n\nDetails:\n%s", warning, len(files), strings.Join(summary, "\n  "), string(details))
}

func diagnosticsError(message, code string, metadata map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"result": message,
		"error":  message,
		"code":   code,
	}
	if metadata != nil {
		out["metadata"] = metadata
	}
	return out
}
