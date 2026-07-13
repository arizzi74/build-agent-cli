package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	persistentProjectsDirName  = ".build-agent"
	persistentSyncDirName      = ".ba-cli-sync"
	persistentSyncManifestFile = "manifest.json"
)

type persistentSyncManifest struct {
	Version int               `json:"version"`
	AppID   string            `json:"appId"`
	RootURI string            `json:"rootUri"`
	Files   map[string]string `json:"files"`
}

type persistentSyncMode string

const (
	persistentSyncAuto   persistentSyncMode = "sync"
	persistentSyncPull   persistentSyncMode = "pull"
	persistentSyncPush   persistentSyncMode = "push"
	persistentSyncStatus persistentSyncMode = "status"
)

type persistentSyncResult struct {
	LocalDir  string
	Pulled    []string
	Pushed    []string
	Deleted   []string
	Conflicts []string
	Unchanged int
}

func persistentProjectDir(instanceURL, workspaceName, appID string) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	instance := safePersistentPathPart(persistentInstanceHost(instanceURL))
	if instance == "" {
		instance = "instance"
	}
	workspace := strings.TrimSpace(workspaceName)
	if workspace == "" {
		workspace = "default"
	}
	workspace = safePersistentPathPart(workspace) + "-" + shortStableHash(workspace)
	appID = safePersistentPathPart(appID)
	if appID == "" {
		return "", errors.New("active app has no safe id for persistent local project")
	}
	return filepath.Join(wd, persistentProjectsDirName, instance, workspace, appID), nil
}

func persistentInstanceHost(instanceURL string) string {
	value := strings.TrimSpace(instanceURL)
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	value = strings.Split(value, "/")[0]
	return value
}

func safePersistentPathPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), ".-")
}

func shortStableHash(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}

func persistentSyncManifestPath(projectDir string) string {
	return filepath.Join(projectDir, persistentSyncDirName, persistentSyncManifestFile)
}

func loadPersistentSyncManifest(projectDir string) (persistentSyncManifest, bool, error) {
	path := persistentSyncManifestPath(projectDir)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return persistentSyncManifest{}, false, nil
	}
	if err != nil {
		return persistentSyncManifest{}, false, err
	}
	var manifest persistentSyncManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return persistentSyncManifest{}, false, fmt.Errorf("invalid local sync manifest: %w", err)
	}
	if manifest.Version != 1 || strings.TrimSpace(manifest.AppID) == "" || strings.TrimSpace(manifest.RootURI) == "" {
		return persistentSyncManifest{}, false, errors.New("invalid local sync manifest version or identity")
	}
	if manifest.Files == nil {
		manifest.Files = map[string]string{}
	}
	return manifest, true, nil
}

func savePersistentSyncManifest(projectDir string, manifest persistentSyncManifest) error {
	manifest.Version = 1
	if manifest.Files == nil {
		manifest.Files = map[string]string{}
	}
	path := persistentSyncManifestPath(projectDir)
	if err := ensurePersistentProjectRoot(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return errors.New("unsafe local sync manifest path")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func isPersistentSyncFile(rel string) bool {
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" || strings.HasPrefix(rel, "../") || rel == ".." || strings.Contains(rel, "/../") {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		switch strings.ToLower(part) {
		case "node_modules", "dist", "target", ".git", ".jest_cache", ".cache", ".turbo", ".next", ".ba-cli-sync":
			return false
		}
	}
	return true
}

func localPersistentFiles(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !isPersistentSyncFile(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = content
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return files, nil
	}
	return files, err
}

func fileChecksums(files map[string][]byte) map[string]string {
	out := make(map[string]string, len(files))
	for rel, content := range files {
		out[rel] = sha1Hex(content)
	}
	return out
}

func refreshPersistentSyncManifest(projectDir, appID, rootURI string) error {
	files, err := localPersistentFiles(projectDir)
	if err != nil {
		return err
	}
	return savePersistentSyncManifest(projectDir, persistentSyncManifest{
		AppID:   strings.TrimSpace(appID),
		RootURI: strings.TrimRight(strings.TrimSpace(rootURI), "/"),
		Files:   fileChecksums(files),
	})
}

func persistentSafeLocalPath(root, rel string) (string, error) {
	if !isPersistentSyncFile(rel) {
		return "", fmt.Errorf("unsafe or excluded sync path %q", rel)
	}
	return safeBuildLocalPath(root, rel)
}

func (c *Client) persistentProjectDirForApp(appID string) (string, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return "", errors.New("persistent local project requires an app id")
	}
	return persistentProjectDir(c.cfg.InstanceURL, c.workspaceName, appID)
}

func (c *Client) persistentProjectDirForActiveApp() (string, error) {
	appID := c.activeAppSysID()
	if appID == "" {
		return "", errors.New("/sync requires an active app")
	}
	return c.persistentProjectDirForApp(appID)
}

func (c *Client) fetchPersistentRemoteFiles(ctx context.Context, rootURI string) (map[string]gliderChangeEntry, map[string][]byte, error) {
	entries, err := c.fetchGliderState(ctx, rootURI)
	if err != nil {
		return nil, nil, err
	}
	remoteEntries := map[string]gliderChangeEntry{}
	prefix := strings.TrimRight(rootURI, "/") + "/"
	for _, entry := range entries {
		if !gliderStateEntryIsFile(entry) || !strings.HasPrefix(entry.URI, prefix) {
			continue
		}
		rel := strings.TrimPrefix(entry.URI, prefix)
		if !isPersistentSyncFile(rel) {
			continue
		}
		remoteEntries[rel] = entry
	}
	uris := make([]string, 0, len(remoteEntries))
	for _, entry := range remoteEntries {
		uris = append(uris, entry.URI)
	}
	sort.Strings(uris)
	contents := map[string][]byte{}
	const batchSize = 60
	for start := 0; start < len(uris); start += batchSize {
		end := start + batchSize
		if end > len(uris) {
			end = len(uris)
		}
		fetched, err := c.fetchV2SyncFiles(ctx, uris[start:end])
		if err != nil {
			return nil, nil, err
		}
		for _, uri := range uris[start:end] {
			rel := strings.TrimPrefix(uri, prefix)
			entry := remoteEntries[rel]
			content, ok := gliderFetchedContentForEntry(fetched, entry)
			if !ok {
				// Some Glider versions omit one file from a multi-URI multipart
				// response while returning it correctly when requested alone.
				one, fetchErr := c.fetchV2SyncFiles(ctx, []string{uri})
				if fetchErr != nil {
					return nil, nil, fetchErr
				}
				content, ok = gliderFetchedContentForEntry(one, entry)
				if !ok && len(one) == 1 {
					for _, only := range one {
						content, ok = only, true
					}
				}
			}
			if !ok {
				return nil, nil, fmt.Errorf("Glider did not return content for %s", rel)
			}
			contents[rel] = content
		}
	}
	return remoteEntries, contents, nil
}

func syncChanged(current, baseline map[string]string, rel string) bool {
	return current[rel] != baseline[rel]
}

func syncPaths(maps ...map[string]string) []string {
	seen := map[string]bool{}
	for _, m := range maps {
		for key := range m {
			seen[key] = true
		}
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (c *Client) syncPersistentApp(ctx context.Context, mode persistentSyncMode) (persistentSyncResult, error) {
	if c.processing {
		return persistentSyncResult{}, errors.New("cannot run /sync while a turn is processing")
	}
	appID := c.activeAppSysID()
	if appID == "" {
		return persistentSyncResult{}, errors.New("/sync requires an active app; choose one with /app")
	}
	return c.syncPersistentAppForBuild(ctx, appID, "now-file:/"+strings.TrimLeft(appID, "/"), mode)
}

func (c *Client) syncPersistentAppForBuild(ctx context.Context, appID, rootURI string, mode persistentSyncMode) (persistentSyncResult, error) {
	if mode == "" {
		mode = persistentSyncAuto
	}
	if mode != persistentSyncAuto && mode != persistentSyncPull && mode != persistentSyncPush && mode != persistentSyncStatus {
		return persistentSyncResult{}, fmt.Errorf("unknown sync mode %q", mode)
	}
	appID = strings.TrimSpace(appID)
	rootURI = strings.TrimRight(strings.TrimSpace(rootURI), "/")
	if appID == "" || rootURI == "" {
		return persistentSyncResult{}, errors.New("persistent local project requires an app id and Glider root URI")
	}
	projectDir, err := c.persistentProjectDirForApp(appID)
	if err != nil {
		return persistentSyncResult{}, err
	}
	if err := ensurePersistentProjectRoot(projectDir); err != nil {
		return persistentSyncResult{}, err
	}
	manifest, haveManifest, err := loadPersistentSyncManifest(projectDir)
	if err != nil {
		return persistentSyncResult{}, err
	}
	if haveManifest && (manifest.AppID != appID || manifest.RootURI != rootURI) {
		return persistentSyncResult{}, errors.New("local sync manifest belongs to another app; choose a different launch directory or remove the stale .ba-cli-sync directory")
	}
	_, remoteFiles, err := c.fetchPersistentRemoteFiles(ctx, rootURI)
	if err != nil {
		return persistentSyncResult{}, err
	}
	localFiles, err := localPersistentFiles(projectDir)
	if err != nil {
		return persistentSyncResult{}, err
	}
	localChecks := fileChecksums(localFiles)
	remoteChecks := fileChecksums(remoteFiles)
	baseline := map[string]string{}
	if haveManifest {
		baseline = manifest.Files
	}
	result := persistentSyncResult{LocalDir: projectDir}
	if mode == persistentSyncStatus {
		for _, rel := range syncPaths(localChecks, remoteChecks, baseline) {
			if localChecks[rel] == remoteChecks[rel] {
				result.Unchanged++
				continue
			}
			if syncChanged(localChecks, baseline, rel) && syncChanged(remoteChecks, baseline, rel) {
				result.Conflicts = append(result.Conflicts, rel)
			} else if syncChanged(localChecks, baseline, rel) {
				result.Pushed = append(result.Pushed, rel)
			} else {
				result.Pulled = append(result.Pulled, rel)
			}
		}
		return result, nil
	}

	pull := map[string][]byte{}
	push := map[string][]byte{}
	remoteRemove := []string{}
	localRemove := []string{}
	for _, rel := range syncPaths(localChecks, remoteChecks, baseline) {
		local, remote, base := localChecks[rel], remoteChecks[rel], baseline[rel]
		if local == remote {
			result.Unchanged++
			continue
		}
		localChanged, remoteChanged := local != base, remote != base
		// With no baseline, a file present on only one side is a safe initial import;
		// two different files at the same path require user resolution.
		if !haveManifest && local != "" && remote != "" {
			result.Conflicts = append(result.Conflicts, rel)
			continue
		}
		if localChanged && remoteChanged {
			result.Conflicts = append(result.Conflicts, rel)
			continue
		}
		switch {
		case remoteChanged:
			if mode == persistentSyncPush {
				result.Conflicts = append(result.Conflicts, rel)
				continue
			}
			if remote == "" {
				localRemove = append(localRemove, rel)
			} else {
				pull[rel] = remoteFiles[rel]
			}
		case localChanged:
			if mode == persistentSyncPull {
				result.Conflicts = append(result.Conflicts, rel)
				continue
			}
			if local == "" {
				// A remote-only file without a baseline must never be treated as a deletion.
				if haveManifest {
					remoteRemove = append(remoteRemove, rel)
				}
			} else {
				push[rel] = localFiles[rel]
			}
		}
	}
	if len(result.Conflicts) > 0 {
		sort.Strings(result.Conflicts)
		return result, fmt.Errorf("sync conflict in %s; resolve the listed paths, then run /sync again", strings.Join(result.Conflicts, ", "))
	}
	if err := c.applyPersistentRemoteChanges(ctx, rootURI, push, remoteRemove); err != nil {
		return result, err
	}
	for _, rel := range localRemove {
		path, err := persistentSafeLocalWritePath(projectDir, rel)
		if err != nil {
			return result, err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		removeEmptyPersistentParents(projectDir, filepath.Dir(path))
		result.Deleted = append(result.Deleted, rel)
	}
	for rel, content := range pull {
		path, err := persistentSafeLocalWritePath(projectDir, rel)
		if err != nil {
			return result, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return result, err
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return result, err
		}
		result.Pulled = append(result.Pulled, rel)
	}
	for rel := range push {
		result.Pushed = append(result.Pushed, rel)
	}
	for _, rel := range remoteRemove {
		result.Deleted = append(result.Deleted, rel)
	}
	sort.Strings(result.Pulled)
	sort.Strings(result.Pushed)
	sort.Strings(result.Deleted)
	if err := refreshPersistentSyncManifest(projectDir, appID, rootURI); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Client) applyPersistentRemoteChanges(ctx context.Context, rootURI string, push map[string][]byte, remove []string) error {
	if len(push) == 0 && len(remove) == 0 {
		return nil
	}
	entries, err := c.fetchGliderState(ctx, rootURI)
	if err != nil {
		return err
	}
	existing := map[string]gliderChangeEntry{}
	for _, entry := range entries {
		existing[entry.URI] = entry
	}
	now := time.Now().UnixMilli()
	create, update := []gliderChangeEntry{}, []gliderChangeEntry{}
	files := []gliderFileBlob{}
	for _, rel := range sortedContentKeys(push) {
		content := push[rel]
		uri := strings.TrimRight(rootURI, "/") + "/" + rel
		entry := gliderChangeEntry{Checksum: sha1Hex(content), CTime: now, MTime: now, Size: len(content), Type: "file", URI: uri}
		if _, ok := existing[uri]; ok {
			update = append(update, entry)
		} else {
			create = append(create, entry)
		}
		files = append(files, gliderFileBlob{Path: entry.Checksum, Checksum: entry.Checksum, Content: content})
		for _, dir := range c.buildMissingParentDirs(ctx, rootURI, rel) {
			if _, exists := existing[dir.URI]; !exists {
				create = append(create, dir)
				existing[dir.URI] = dir
			}
		}
	}
	removeEntries := []gliderChangeEntry{}
	for _, rel := range remove {
		uri := strings.TrimRight(rootURI, "/") + "/" + rel
		if entry, ok := existing[uri]; ok {
			removeEntries = append(removeEntries, entry)
		}
	}
	return c.applyGliderChanges(ctx, create, update, removeEntries, files)
}

func ensurePersistentProjectRoot(projectDir string) error {
	clean := filepath.Clean(projectDir)
	for current := clean; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("unsafe persistent project path %q", current)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return os.MkdirAll(clean, 0o755)
}

func removeEmptyPersistentParents(root, dir string) {
	cleanRoot := filepath.Clean(root)
	for current := filepath.Clean(dir); current != cleanRoot && strings.HasPrefix(current, cleanRoot+string(os.PathSeparator)); current = filepath.Dir(current) {
		if err := os.Remove(current); err != nil {
			return
		}
	}
}

func persistentSafeLocalWritePath(root, rel string) (string, error) {
	path, err := persistentSafeLocalPath(root, rel)
	if err != nil {
		return "", err
	}
	cleanRoot := filepath.Clean(root)
	for current := filepath.Dir(path); current != cleanRoot; current = filepath.Dir(current) {
		if !strings.HasPrefix(current, cleanRoot+string(os.PathSeparator)) {
			return "", fmt.Errorf("unsafe sync path %q", rel)
		}
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("unsafe symlink or non-directory in sync path %q", rel)
			}
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
	}
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("unsafe symlink sync destination %q", rel)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	return path, nil
}

func sortedContentKeys(files map[string][]byte) []string {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func formatPersistentSyncResult(result persistentSyncResult) string {
	parts := []string{fmt.Sprintf("local project: %s", result.LocalDir)}
	parts = append(parts, fmt.Sprintf("pulled: %d", len(result.Pulled)), fmt.Sprintf("pushed: %d", len(result.Pushed)), fmt.Sprintf("deleted: %d", len(result.Deleted)), fmt.Sprintf("unchanged: %d", result.Unchanged))
	if len(result.Conflicts) > 0 {
		parts = append(parts, fmt.Sprintf("conflicts: %s", strings.Join(result.Conflicts, ", ")))
	}
	return strings.Join(parts, "\n")
}

func handleSyncCommand(ctx context.Context, c *Client, args []string) error {
	mode := persistentSyncAuto
	if len(args) > 1 {
		return errors.New("usage: /sync [status|pull|push]")
	}
	if len(args) == 1 {
		switch strings.ToLower(strings.TrimSpace(args[0])) {
		case "", "sync":
		case "status", "pull", "push":
			mode = persistentSyncMode(strings.ToLower(strings.TrimSpace(args[0])))
		case "help":
			slashCommandPrintln("usage: /sync [status|pull|push]")
			slashCommandPrintln("/sync safely merges local app source with ServiceNow Glider; conflicts are never overwritten.")
			return nil
		default:
			return errors.New("usage: /sync [status|pull|push]")
		}
	}
	result, err := c.syncPersistentApp(ctx, mode)
	if result.LocalDir != "" {
		slashCommandPrintln(formatPersistentSyncResult(result))
	}
	return err
}
