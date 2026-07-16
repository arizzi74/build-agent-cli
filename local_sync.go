package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
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
	Version           int               `json:"version"`
	InstanceURL       string            `json:"instanceUrl,omitempty"`
	AppID             string            `json:"appId"`
	Scope             string            `json:"scope,omitempty"`
	RootURI           string            `json:"rootUri"`
	CheckoutID        string            `json:"checkoutId,omitempty"`
	ChecksumAlgorithm string            `json:"checksumAlgorithm,omitempty"`
	Files             map[string]string `json:"files"`
}

type persistentSyncMode string

const (
	persistentSyncAuto   persistentSyncMode = "sync"
	persistentSyncPull   persistentSyncMode = "pull"
	persistentSyncPush   persistentSyncMode = "push"
	persistentSyncStatus persistentSyncMode = "status"
)

type persistentSyncResult struct {
	LocalDir        string
	NotMaterialized bool
	Pulled          []string
	Pushed          []string
	Deleted         []string
	Conflicts       []string
	Unchanged       int
	PendingPush     []string
}

// canonicalProjectCollisionError makes the distinction between a recoverable
// directory collision and an unsafe filesystem object explicit. Callers must
// never infer this from an error string.
type canonicalProjectCollisionError struct {
	Target      string
	Reason      string
	Recoverable bool
}

func (e *canonicalProjectCollisionError) Error() string {
	return fmt.Sprintf("local project collision at %q: %s", e.Target, e.Reason)
}

// persistentProjectDir retains the old argument shape for callers that only
// know an app ID. New code should pass a display name so the project is placed
// in the fixed, readable canonical location under ~/BA.
func persistentProjectDir(instanceURL, workspaceName, appID string) (string, error) {
	return persistentProjectDirNamed("", appID)
}

func legacyPersistentProjectDir(instanceURL, workspaceName, appID string) (string, error) {
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
		return "", errors.New("active app has no safe id for legacy persistent local project")
	}
	return filepath.Join(wd, persistentProjectsDirName, instance, workspace, appID), nil
}

// canonicalProjectRoot returns a clean absolute root without resolving or
// following symlinks. Existing components must be real directories; absent
// components are permitted so the first scaffold can create ~/BA safely.
// Only the current user's ~ spelling is expanded—never ~otheruser.
func canonicalProjectRoot(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return "", errors.New("cannot determine home directory for default project root")
		}
		value = filepath.Join(home, "BA")
	} else if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return "", errors.New("cannot expand ~ for project root")
		}
		value = filepath.Join(home, value[2:])
	} else if strings.HasPrefix(value, "~") {
		return "", errors.New("project root supports only ~ or ~/..., not another user's home")
	}
	if !filepath.IsAbs(value) {
		return "", errors.New("project root must be an absolute path or start with ~/")
	}
	root := filepath.Clean(value)
	for current := root; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("unsafe project root path %q", current)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return root, nil
}

// persistentProjectDirNamed returns the legacy default canonical checkout
// location. Client methods use the profile-configured root below. Keeping this
// helper preserves callers that do not have a client/configuration context.
func persistentProjectDirNamed(appName, fallbackID string) (string, error) {
	return persistentProjectDirNamedAtRoot("", appName, fallbackID)
}

func persistentProjectDirNamedAtRoot(root, appName, fallbackID string) (string, error) {
	root, err := canonicalProjectRoot(root)
	if err != nil {
		return "", err
	}
	dirName, err := persistentAppDirName(appName, fallbackID)
	if err != nil {
		return "", err
	}
	if dirName == "" {
		return "", errors.New("active app has no safe id for persistent local project")
	}
	return filepath.Join(root, dirName), nil
}

// persistentAppDirName preserves a human app name wherever it is safe as one
// filesystem component. Unsafe characters are deterministically replaced so
// a known display name never quietly falls back to an unrelated app ID.
func persistentAppDirName(appName, fallbackID string) (string, error) {
	if strings.TrimSpace(appName) == "" {
		return safePersistentPathPart(fallbackID), nil
	}
	name := strings.TrimSpace(appName)
	var b strings.Builder
	lastReplacement := false
	for _, r := range name {
		unsafe := r < 0x20 || r == 0x7f || strings.ContainsRune(`/\\<>:"|?*`, r)
		if unsafe {
			if !lastReplacement {
				b.WriteByte('-')
				lastReplacement = true
			}
			continue
		}
		b.WriteRune(r)
		lastReplacement = false
	}
	name = strings.Trim(b.String(), " .")
	if name == "" || name == "." || name == ".." {
		return "app-" + shortStableHash(appName), nil
	}
	// Avoid Windows device names even when the directory is first created on a
	// Unix host and later reused on Windows. Device names remain reserved when
	// followed by an extension, so inspect the portion before the first dot.
	base := strings.ToUpper(name)
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	if isWindowsReservedDirName(base) {
		name = "app-" + name
	}
	return name, nil
}

func isWindowsReservedDirName(name string) bool {
	if name == "CON" || name == "PRN" || name == "AUX" || name == "NUL" {
		return true
	}
	for _, prefix := range []string{"COM", "LPT"} {
		if len(name) == len(prefix)+1 && strings.HasPrefix(name, prefix) && name[len(prefix)] >= '1' && name[len(prefix)] <= '9' {
			return true
		}
	}
	return false
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
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return persistentSyncManifest{}, false, fmt.Errorf("unsafe local sync manifest path %q", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return persistentSyncManifest{}, false, err
	}
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
	if (manifest.Version != 1 && manifest.Version != 2) || strings.TrimSpace(manifest.AppID) == "" || strings.TrimSpace(manifest.RootURI) == "" {
		return persistentSyncManifest{}, false, errors.New("invalid local sync manifest version or identity")
	}
	if manifest.Version == 2 && strings.TrimSpace(manifest.InstanceURL) == "" {
		return persistentSyncManifest{}, false, errors.New("invalid v2 local sync manifest instance identity")
	}
	if manifest.ChecksumAlgorithm == "" {
		manifest.ChecksumAlgorithm = "sha1"
	}
	if manifest.ChecksumAlgorithm != "sha1" && manifest.ChecksumAlgorithm != "sha256" {
		return persistentSyncManifest{}, false, errors.New("invalid local sync manifest checksum algorithm")
	}
	if manifest.Files == nil {
		manifest.Files = map[string]string{}
	}
	return manifest, true, nil
}

func savePersistentSyncManifest(projectDir string, manifest persistentSyncManifest) error {
	manifest.Version = 2
	manifest.InstanceURL = normalizedProjectInstance(manifest.InstanceURL)
	if manifest.InstanceURL == "" {
		return errors.New("local sync manifest requires an instance identity")
	}
	manifest.AppID = strings.TrimSpace(manifest.AppID)
	manifest.RootURI = strings.TrimRight(strings.TrimSpace(manifest.RootURI), "/")
	manifest.ChecksumAlgorithm = "sha256"
	if manifest.CheckoutID == "" {
		id, err := randomCheckoutID()
		if err != nil {
			return err
		}
		manifest.CheckoutID = id
	}
	if manifest.Files == nil {
		manifest.Files = map[string]string{}
	}
	return writePersistentSyncManifest(projectDir, manifest)
}

// writePersistentSyncManifest preserves the supplied format. It is used for
// v1 baselines during a partial pull, where changing checksum algorithms would
// make unsynced local edits appear clean.
func writePersistentSyncManifest(projectDir string, manifest persistentSyncManifest) error {
	path := persistentSyncManifestPath(projectDir)
	if err := ensurePersistentProjectRoot(filepath.Dir(path)); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return durablePrivateAtomicWrite(path, append(raw, '\n'))
}

func isPersistentSyncFile(rel string) bool {
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" || strings.HasPrefix(rel, "../") || rel == ".." || strings.Contains(rel, "/../") {
		return false
	}
	// npm creates this derived dependency lock while preparing a local build.
	// It is deliberately local-only: build/install must not turn dependency
	// installation into an implicit source upload or a permanent sync conflict.
	if strings.EqualFold(rel, "package-lock.json") {
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

func checksumHex(algorithm string, content []byte) string {
	switch algorithm {
	case "sha1":
		return sha1Hex(content)
	default:
		sum := sha256.Sum256(content)
		return hex.EncodeToString(sum[:])
	}
}

func fileChecksumsWithAlgorithm(files map[string][]byte, algorithm string) map[string]string {
	out := make(map[string]string, len(files))
	for rel, content := range files {
		out[rel] = checksumHex(algorithm, content)
	}
	return out
}

func fileChecksums(files map[string][]byte) map[string]string {
	return fileChecksumsWithAlgorithm(files, "sha256")
}

func (c *Client) refreshPersistentSyncManifest(projectDir, appID, rootURI string) error {
	files, err := localPersistentFiles(projectDir)
	if err != nil {
		return err
	}
	return c.savePersistentSyncManifestWithFiles(projectDir, appID, rootURI, fileChecksums(files))
}

func (c *Client) savePersistentSyncManifestWithFiles(projectDir, appID, rootURI string, files map[string]string) error {
	checkoutID := ""
	if current, ok, err := loadPersistentSyncManifest(projectDir); err == nil && ok {
		checkoutID = current.CheckoutID
	}
	if err := savePersistentSyncManifest(projectDir, persistentSyncManifest{
		InstanceURL: c.cfg.InstanceURL,
		AppID:       appID,
		Scope:       c.activeAppScopeName(),
		RootURI:     rootURI,
		CheckoutID:  checkoutID,
		Files:       files,
	}); err != nil {
		return err
	}
	manifest, ok, err := loadPersistentSyncManifest(projectDir)
	if err != nil || !ok {
		return firstNonNil(err, errors.New("local sync manifest was not written"))
	}
	_, err = c.registerPersistentProject(projectDir, appID, rootURI, manifest.CheckoutID, false)
	return err
}

func firstNonNil(err error, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func persistentSafeLocalPath(root, rel string) (string, error) {
	if !isPersistentSyncFile(rel) {
		return "", fmt.Errorf("unsafe or excluded sync path %q", rel)
	}
	return safeBuildLocalPath(root, rel)
}

func (c *Client) persistentProjectDirForApp(appID string) (string, error) {
	return c.persistentProjectDirForAppNamed(appID, c.persistentAppDisplayName(appID))
}

func (c *Client) persistentProjectDirForAppNamed(appID, appName string) (string, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return "", errors.New("persistent local project requires an app id")
	}
	return persistentProjectDirNamedAtRoot(c.cfg.ProjectRoot, appName, appID)
}

func (c *Client) persistentAppDisplayName(appID string) string {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return ""
	}
	if app := c.currentApp; app != nil && appMatchesID(*app, appID) && !appNameIsID(app.ScopeName, appID, app.ScopeID, app.AppSysID) {
		return strings.TrimSpace(app.ScopeName)
	}
	for _, folder := range c.workspaceFolders {
		if workspaceFolderAppID(folder.URI) == appID && !appNameIsID(folder.Name, appID) {
			return strings.TrimSpace(folder.Name)
		}
	}
	return ""
}

func appMatchesID(app AppScope, appID string) bool {
	for _, id := range []string{app.AppSysID, app.ScopeID} {
		if strings.EqualFold(strings.TrimSpace(id), appID) {
			return true
		}
	}
	return false
}

func appNameIsID(name string, ids ...string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	for _, id := range ids {
		if strings.EqualFold(name, strings.TrimSpace(id)) {
			return true
		}
	}
	return false
}

func workspaceFolderAppID(uri string) string {
	uri = strings.TrimSpace(strings.TrimPrefix(uri, "now-file:"))
	uri = strings.Trim(uri, "/")
	return strings.Split(uri, "/")[0]
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
			if ok && !gliderEntryContentMatches(entry, content) {
				ok = false
			}
			if !ok {
				// Some Glider versions omit one file from a multi-URI multipart
				// response while returning it correctly when requested alone.
				one, fetchErr := c.fetchV2SyncFiles(ctx, []string{uri})
				if fetchErr != nil {
					return nil, nil, fetchErr
				}
				content, ok = gliderFetchedContentForEntry(one, entry)
				if ok && !gliderEntryContentMatches(entry, content) {
					ok = false
				}
				if !ok && len(one) == 1 {
					for _, only := range one {
						if gliderEntryContentMatches(entry, only) {
							content, ok = only, true
						}
					}
				}
			}
			if !ok {
				// A delete can become visible to sync/files before the recursive
				// sync/state listing catches up. Refresh state before reporting a
				// missing-content failure; an absent exact URI is a confirmed delete.
				refreshed, refreshErr := c.fetchGliderState(ctx, rootURI)
				if refreshErr != nil {
					return nil, nil, refreshErr
				}
				var current gliderChangeEntry
				for _, candidate := range refreshed {
					if gliderStateEntryIsFile(candidate) && candidate.URI == uri {
						current = candidate
						break
					}
				}
				if current.URI == "" {
					delete(remoteEntries, rel)
					continue
				}
				one, fetchErr := c.fetchV2SyncFiles(ctx, []string{uri})
				if fetchErr != nil {
					return nil, nil, fetchErr
				}
				content, ok = gliderFetchedContentForEntry(one, current)
				if ok && !gliderEntryContentMatches(current, content) {
					ok = false
				}
				if !ok && len(one) == 1 {
					for _, only := range one {
						if gliderEntryContentMatches(current, only) {
							content, ok = only, true
						}
					}
				}
				entry = current
				remoteEntries[rel] = current
			}
			if !ok {
				return nil, nil, fmt.Errorf("Glider did not return content for %s", rel)
			}
			if !gliderEntryContentMatches(entry, content) {
				return nil, nil, fmt.Errorf("Glider returned checksum-mismatched content for %s", rel)
			}
			contents[rel] = content
		}
	}
	return remoteEntries, contents, nil
}

func gliderEntryContentMatches(entry gliderChangeEntry, content []byte) bool {
	checksum := strings.ToLower(strings.TrimSpace(entry.Checksum))
	switch len(checksum) {
	case 40:
		return sha1Hex(content) == checksum
	case 64:
		return checksumHex("sha256", content) == checksum
	default:
		return true
	}
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
	return c.syncPersistentAppForBuildNamed(ctx, appID, c.persistentAppDisplayName(appID), rootURI, mode)
}

func (c *Client) syncPersistentAppForBuildNamed(ctx context.Context, appID, appName, rootURI string, mode persistentSyncMode) (persistentSyncResult, error) {
	result, cleanup, err := c.syncPersistentAppForBuildNamedLocked(ctx, appID, appName, rootURI, mode)
	if cleanup != nil {
		defer cleanup()
	}
	return result, err
}

// syncPersistentAppForBuildNamedLocked is used by build preparation so the
// project lock remains held through generated-file persistence. Ordinary
// /sync callers use the wrapper above and release it as soon as sync ends.
func (c *Client) syncPersistentAppForBuildNamedLocked(ctx context.Context, appID, appName, rootURI string, mode persistentSyncMode) (persistentSyncResult, func(), error) {
	if mode == "" {
		mode = persistentSyncAuto
	}
	if mode != persistentSyncAuto && mode != persistentSyncPull && mode != persistentSyncPush && mode != persistentSyncStatus {
		return persistentSyncResult{}, nil, fmt.Errorf("unknown sync mode %q", mode)
	}
	appID = strings.TrimSpace(appID)
	rootURI = strings.TrimRight(strings.TrimSpace(rootURI), "/")
	if appID == "" || rootURI == "" {
		return persistentSyncResult{}, nil, errors.New("persistent local project requires an app id and Glider root URI")
	}
	if mode == persistentSyncStatus {
		result, err := c.inspectPersistentAppStatus(ctx, appID, appName, rootURI)
		return result, nil, err
	}
	resolution, err := c.resolvePersistentProject(appID, appName, rootURI, true)
	if err != nil {
		return persistentSyncResult{}, nil, err
	}
	projectDir := resolution.Dir
	canonicalDir, err := c.persistentProjectDirForAppNamed(appID, appName)
	if err != nil {
		return persistentSyncResult{}, nil, err
	}
	// An automatically selected registered primary from an older layout moves
	// to the fixed canonical location only after no-clobber validation. Explicit
	// current-directory and process-override checkouts remain where chosen.
	if resolution.Registered && resolution.DurablePrimary && filepath.Clean(projectDir) != filepath.Clean(canonicalDir) {
		if err := c.migratePersistentProject(projectDir, canonicalDir, appID, rootURI); err != nil {
			return persistentSyncResult{}, nil, err
		}
		// The just-moved registered source may still carry a v1 manifest. It is
		// safe here because migration verified its exact registry binding; pin it
		// while registering the canonical primary before continuing the sync.
		if manifest, ok := validManifestedProject(canonicalDir, c.cfg.InstanceURL, appID, rootURI); ok {
			if _, err := c.registerPersistentProject(canonicalDir, appID, rootURI, manifest.CheckoutID, true); err != nil {
				return persistentSyncResult{}, nil, err
			}
			projectDir = canonicalDir
		}
	}
	if resolution.Source == "new canonical location" {
		if err := c.migrateLegacyPersistentProject(projectDir, appID, rootURI); err != nil {
			return persistentSyncResult{}, nil, err
		}
	}
	if err := ensurePersistentProjectRoot(projectDir); err != nil {
		return persistentSyncResult{}, nil, err
	}
	cleanup, err := acquirePersistentProjectLock(ctx, projectDir)
	if err != nil {
		return persistentSyncResult{}, nil, err
	}
	result, err := c.syncPersistentAppAtLocked(ctx, projectDir, appID, rootURI, mode)
	if err != nil {
		cleanup()
		return result, nil, err
	}
	if filepath.Clean(projectDir) == filepath.Clean(canonicalDir) {
		manifest, ok, manifestErr := loadPersistentSyncManifest(projectDir)
		if manifestErr != nil || !ok {
			cleanup()
			return result, nil, firstNonNil(manifestErr, errors.New("local sync manifest was not written"))
		}
		if _, err := c.registerPersistentProject(projectDir, appID, rootURI, manifest.CheckoutID, true); err != nil {
			cleanup()
			return result, nil, err
		}
	}
	return result, cleanup, nil
}

// inspectPersistentAppStatus compares an existing checkout without running any
// materializing resolver, migration, registry write, or canonical-directory
// creation. When no checkout exists, the remote tree is compared with an empty
// local baseline and the expected path is reported without claiming it.
func (c *Client) inspectPersistentAppStatus(ctx context.Context, appID, appName, rootURI string) (persistentSyncResult, error) {
	resolution, err := c.resolveExistingPersistentProject(appID, appName, rootURI)
	if err == nil {
		unlock, lockErr := acquirePersistentProjectLock(ctx, resolution.Dir)
		if lockErr != nil {
			return persistentSyncResult{}, lockErr
		}
		defer unlock()
		return c.syncPersistentAppAtLocked(ctx, resolution.Dir, appID, rootURI, persistentSyncStatus)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return persistentSyncResult{}, err
	}

	projectDir, err := c.persistentProjectDirForAppNamed(appID, appName)
	if err != nil {
		return persistentSyncResult{}, err
	}
	instanceURL, normalizedAppID := c.projectRegistryIdentity(appID)
	if err := canonicalProjectCollision(projectDir, instanceURL, normalizedAppID, rootURI); err != nil {
		return persistentSyncResult{}, err
	}
	_, remoteFiles, err := c.fetchPersistentRemoteFiles(ctx, rootURI)
	if err != nil {
		return persistentSyncResult{}, err
	}
	return persistentSyncResult{
		LocalDir:        projectDir,
		NotMaterialized: true,
		Pulled:          sortedContentKeys(remoteFiles),
	}, nil
}

func acquirePersistentProjectLock(ctx context.Context, projectDir string) (func(), error) {
	lockDir := filepath.Join(projectDir, persistentSyncDirName)
	if err := ensurePersistentProjectRoot(lockDir); err != nil {
		return nil, err
	}
	path := filepath.Join(lockDir, "project.lock")
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return nil, errors.New("unsafe local project lock path")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		locked, err := tryLockProfileFile(file)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return func() {
		unlockProfileFile(file)
		_ = file.Close()
	}, nil
}

func (c *Client) syncPersistentAppAtLocked(ctx context.Context, projectDir, appID, rootURI string, mode persistentSyncMode) (persistentSyncResult, error) {
	manifest, haveManifest, err := loadPersistentSyncManifest(projectDir)
	if err != nil {
		return persistentSyncResult{}, err
	}
	if haveManifest && !manifestMatchesProject(manifest, c.cfg.InstanceURL, appID, rootURI) {
		return persistentSyncResult{}, errors.New("local sync manifest belongs to another app; choose a different canonical app directory or remove the stale .ba-cli-sync directory")
	}
	if !haveManifest {
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			return persistentSyncResult{}, err
		}
		lockOnly, inspectErr := persistentProjectContainsOnlyOrphanLock(projectDir, entries)
		if inspectErr != nil {
			return persistentSyncResult{}, inspectErr
		}
		if !lockOnly {
			return persistentSyncResult{}, errors.New("local app folder has no sync manifest; refusing to claim an existing named folder")
		}
	}
	_, remoteFiles, err := c.fetchPersistentRemoteFiles(ctx, rootURI)
	if err != nil {
		return persistentSyncResult{}, err
	}
	localFiles, err := localPersistentFiles(projectDir)
	if err != nil {
		return persistentSyncResult{}, err
	}
	algorithm := "sha256"
	if haveManifest {
		algorithm = manifest.ChecksumAlgorithm
	}
	localChecks := fileChecksumsWithAlgorithm(localFiles, algorithm)
	remoteChecks := fileChecksumsWithAlgorithm(remoteFiles, algorithm)
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
				result.PendingPush = append(result.PendingPush, rel)
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
			if mode != persistentSyncPush {
				result.PendingPush = append(result.PendingPush, rel)
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
	sort.Strings(result.PendingPush)
	finalFiles, readErr := localPersistentFiles(projectDir)
	if readErr != nil {
		return result, readErr
	}
	if len(result.PendingPush) == 0 {
		// A clean successful operation is the only v1->v2 migration point.
		if err := c.savePersistentSyncManifestWithFiles(projectDir, appID, rootURI, fileChecksums(finalFiles)); err != nil {
			return result, err
		}
	} else if haveManifest {
		// Pulls can update unrelated paths while local edits stay pending. Write
		// only those reconciled baselines with the existing algorithm; changing
		// a v1 checksum here would make the pending paths ambiguous.
		pending := make(map[string]bool, len(result.PendingPush))
		for _, rel := range result.PendingPush {
			pending[rel] = true
		}
		next := make(map[string]string, len(baseline)+len(finalFiles))
		for rel, sum := range baseline {
			next[rel] = sum
		}
		for _, rel := range syncPaths(localChecks, remoteChecks, baseline) {
			if pending[rel] {
				continue
			}
			if content, ok := finalFiles[rel]; ok {
				next[rel] = checksumHex(manifest.ChecksumAlgorithm, content)
			} else {
				delete(next, rel)
			}
		}
		manifest.Files = next
		if err := writePersistentSyncManifest(projectDir, manifest); err != nil {
			return result, err
		}
		_, _ = c.registerPersistentProject(projectDir, appID, rootURI, manifest.CheckoutID, false)
	}
	return result, nil
}

// canonicalProjectCollision checks whether a canonical target can safely be
// created or used for this exact project. It never claims nonempty or foreign
// directories, including directories with a stale or malformed manifest. The
// sole manifestless exception is the exact lock-only residue produced by older
// `/sync status` versions before a checkout was materialized; it contains no
// project or ownership data and is revalidated after the lock is acquired.
func canonicalProjectCollision(targetDir, instanceURL, appID, rootURI string) error {
	info, err := os.Lstat(targetDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return &canonicalProjectCollisionError{Target: targetDir, Reason: "target is not a safe directory", Recoverable: false}
	}
	manifest, ok, manifestErr := loadPersistentSyncManifest(targetDir)
	if manifestErr != nil {
		return &canonicalProjectCollisionError{Target: targetDir, Reason: "invalid sync manifest", Recoverable: true}
	}
	if ok {
		if !canonicalManifestMatchesProject(manifest, instanceURL, appID, rootURI) {
			return &canonicalProjectCollisionError{Target: targetDir, Reason: "manifest belongs to another instance or app", Recoverable: true}
		}
		return nil
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		lockOnly, inspectErr := persistentProjectContainsOnlyOrphanLock(targetDir, entries)
		if inspectErr != nil {
			return inspectErr
		}
		if lockOnly {
			return nil
		}
		return &canonicalProjectCollisionError{Target: targetDir, Reason: "existing nonempty directory has no matching sync manifest", Recoverable: true}
	}
	return nil
}

func persistentProjectContainsOnlyOrphanLock(targetDir string, entries []os.DirEntry) (bool, error) {
	if len(entries) != 1 || entries[0].Name() != persistentSyncDirName || entries[0].Type()&os.ModeSymlink != 0 || !entries[0].IsDir() {
		return false, nil
	}
	metadataDir := filepath.Join(targetDir, persistentSyncDirName)
	metadataEntries, err := os.ReadDir(metadataDir)
	if err != nil {
		return false, err
	}
	if len(metadataEntries) == 0 {
		return true, nil
	}
	if len(metadataEntries) != 1 || metadataEntries[0].Name() != "project.lock" || metadataEntries[0].Type()&os.ModeSymlink != 0 {
		return false, nil
	}
	info, err := metadataEntries[0].Info()
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular() && info.Size() == 0, nil
}

// migratePersistentProject moves a verified checkout into the canonical target
// without overwriting anything. It is used for historical and registered
// primary checkouts; explicit overrides and clone-here checkouts stay put.
func (c *Client) migratePersistentProject(sourceDir, targetDir, appID, rootURI string) error {
	if filepath.Clean(sourceDir) == filepath.Clean(targetDir) {
		return nil
	}
	if persistentPathContains(sourceDir, targetDir) || persistentPathContains(targetDir, sourceDir) {
		return fmt.Errorf("cannot migrate local project between overlapping paths %q and %q", sourceDir, targetDir)
	}
	// Verify the source before changing an empty target. Historical v1 sources
	// are safe only when the current-instance registry binds that exact path.
	manifest, ok := validManifestedProject(sourceDir, c.cfg.InstanceURL, appID, rootURI)
	if !ok {
		return fmt.Errorf("cannot migrate local project %q: missing matching sync manifest", sourceDir)
	}
	registeredCheckout, registered := c.registeredPersistentProjectCheckout(sourceDir, appID)
	if !canonicalManifestMatchesProject(manifest, c.cfg.InstanceURL, appID, rootURI) && !registered {
		return fmt.Errorf("cannot migrate local project %q: legacy manifest is not registered for this instance", sourceDir)
	}
	checkoutID := firstNonBlank(manifest.CheckoutID, registeredCheckout.ID)
	if checkoutID == "" {
		return fmt.Errorf("cannot migrate local project %q: missing checkout identity", sourceDir)
	}
	if err := canonicalProjectCollision(targetDir, c.cfg.InstanceURL, appID, rootURI); err != nil {
		return err
	}
	if info, err := os.Lstat(targetDir); err == nil {
		if _, matching := validCanonicalManifestedProject(targetDir, c.cfg.InstanceURL, appID, rootURI); matching {
			// A complete canonical checkout already wins; retain the historical
			// source rather than deleting it.
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("local project collision at %q: target is not a safe directory", targetDir)
		}
		entries, readErr := os.ReadDir(targetDir)
		if readErr != nil {
			return readErr
		}
		if len(entries) != 0 {
			return fmt.Errorf("local project collision at %q: existing nonempty directory has no matching sync manifest", targetDir)
		}
		if err := os.Remove(targetDir); err != nil {
			return fmt.Errorf("remove empty canonical project directory: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensurePersistentProjectRoot(filepath.Dir(targetDir)); err != nil {
		return err
	}
	if manifest.Version != 2 || normalizedProjectInstance(manifest.InstanceURL) != normalizedProjectInstance(c.cfg.InstanceURL) || manifest.CheckoutID != checkoutID {
		manifest.Version = 2
		manifest.InstanceURL = normalizedProjectInstance(c.cfg.InstanceURL)
		manifest.CheckoutID = checkoutID
		if err := writePersistentSyncManifest(sourceDir, manifest); err != nil {
			return fmt.Errorf("pin migrated local project identity: %w", err)
		}
	}
	if err := os.Rename(sourceDir, targetDir); err != nil {
		return fmt.Errorf("migrate local project to canonical location: %w", err)
	}
	return nil
}

// migrateLegacyPersistentProject performs a one-time, no-clobber move from
// the historical .build-agent hierarchy only when its manifest proves that it
// belongs to this exact app and Glider root. Legacy directories are otherwise
// left untouched.
func persistentPathContains(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (c *Client) migrateLegacyPersistentProject(targetDir, appID, rootURI string) error {
	legacyDir, err := legacyPersistentProjectDir(c.cfg.InstanceURL, c.workspaceName, appID)
	if err != nil {
		return err
	}
	if _, ok := validManifestedProject(legacyDir, c.cfg.InstanceURL, appID, rootURI); !ok {
		return nil
	}
	return c.migratePersistentProject(legacyDir, targetDir, appID, rootURI)
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

func formatPersistentSyncResult(result persistentSyncResult, mode persistentSyncMode) string {
	localProject := result.LocalDir
	if result.NotMaterialized {
		localProject = fmt.Sprintf("not materialized (expected: %s)", result.LocalDir)
	}
	parts := []string{fmt.Sprintf("local project: %s", localProject)}
	if mode == persistentSyncStatus {
		parts = append(parts, fmt.Sprintf("pending pull: %d", len(result.Pulled)), fmt.Sprintf("pending push: %d", len(result.PendingPush)), fmt.Sprintf("unchanged: %d", result.Unchanged))
		if len(result.Conflicts) > 0 {
			parts = append(parts, fmt.Sprintf("conflicts: %s", strings.Join(result.Conflicts, ", ")))
		}
		return strings.Join(parts, "\n")
	}
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
		slashCommandPrintln(formatPersistentSyncResult(result, mode))
	}
	return err
}
