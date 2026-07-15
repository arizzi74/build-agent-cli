package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	projectRegistryVersion = 1
	projectRegistryDirName = "projects"
	projectRegistryFile    = "registry.json"
)

type projectCheckout struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	CreatedAt string `json:"createdAt,omitempty"`
}

type registeredProject struct {
	InstanceURL       string            `json:"instanceUrl"`
	AppID             string            `json:"appId"`
	PrimaryCheckoutID string            `json:"primaryCheckoutId,omitempty"`
	Checkouts         []projectCheckout `json:"checkouts"`
}

type projectRegistry struct {
	Version  int                          `json:"version"`
	Projects map[string]registeredProject `json:"projects"`
}

type persistentProjectResolution struct {
	Dir            string
	CheckoutID     string
	Source         string
	Registered     bool
	DurablePrimary bool
}

func projectsDir(profile string) string {
	return filepath.Join(profileDir(profile), projectRegistryDirName)
}
func projectRegistryPath(profile string) string {
	return filepath.Join(projectsDir(profile), projectRegistryFile)
}

func normalizedProjectInstance(raw string) string {
	value := strings.TrimSpace(raw)
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "https://"):
		value = "https://" + value[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		value = "http://" + value[len("http://"):]
	}
	return strings.ToLower(strings.TrimRight(cleanInstanceURL(value), "/"))
}

func projectRegistryKey(instanceURL, appID string) string {
	return normalizedProjectInstance(instanceURL) + "\x00" + strings.ToLower(strings.TrimSpace(appID))
}

func loadProjectRegistry(profile string) (projectRegistry, error) {
	path := projectRegistryPath(profile)
	if err := rejectSymlinkPath(path); err != nil {
		return projectRegistry{}, err
	}
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return projectRegistry{}, fmt.Errorf("unsafe project registry path %q", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return projectRegistry{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return projectRegistry{Version: projectRegistryVersion, Projects: map[string]registeredProject{}}, nil
	}
	if err != nil {
		return projectRegistry{}, err
	}
	var registry projectRegistry
	if err := json.Unmarshal(raw, &registry); err != nil {
		return projectRegistry{}, fmt.Errorf("invalid project registry: %w", err)
	}
	if registry.Version != projectRegistryVersion {
		return projectRegistry{}, errors.New("invalid project registry version")
	}
	if registry.Projects == nil {
		registry.Projects = map[string]registeredProject{}
	}
	return registry, nil
}

func saveProjectRegistry(profile string, registry projectRegistry) error {
	registry.Version = projectRegistryVersion
	if registry.Projects == nil {
		registry.Projects = map[string]registeredProject{}
	}
	if err := ensurePrivateDir(projectsDir(profile)); err != nil {
		return err
	}
	if err := rejectSymlinkPath(projectRegistryPath(profile)); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	return durablePrivateAtomicWrite(projectRegistryPath(profile), append(raw, '\n'))
}

func durablePrivateAtomicWrite(path string, content []byte) error {
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return fmt.Errorf("unsafe path %q", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Directory fsync is unsupported on some platforms/filesystems. The file
	// rename is still atomic there, so only fail for a usable directory handle.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func randomCheckoutID() (string, error) {
	var b [12]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func canonicalProjectPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("project path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func manifestMatchesProject(manifest persistentSyncManifest, instanceURL, appID, rootURI string) bool {
	if !strings.EqualFold(strings.TrimSpace(manifest.AppID), strings.TrimSpace(appID)) || strings.TrimRight(strings.TrimSpace(manifest.RootURI), "/") != strings.TrimRight(strings.TrimSpace(rootURI), "/") {
		return false
	}
	// v1 had no instance identity. It remains compatible only after the app/root
	// identity match; the next safe mutating sync upgrades it to v2.
	if manifest.Version < 2 || strings.TrimSpace(manifest.InstanceURL) == "" {
		return true
	}
	return normalizedProjectInstance(manifest.InstanceURL) == normalizedProjectInstance(instanceURL)
}

// canonicalManifestMatchesProject is deliberately stricter than
// manifestMatchesProject. A fixed ~/BA/<app-name> path has no instance in its
// name, so it can only be claimed from a v2 manifest that records the exact
// normalized instance identity. v1 manifests remain usable at historical or
// explicitly registered paths, where the registry supplies the missing binding.
func canonicalManifestMatchesProject(manifest persistentSyncManifest, instanceURL, appID, rootURI string) bool {
	return manifest.Version == 2 && strings.TrimSpace(manifest.InstanceURL) != "" && manifestMatchesProject(manifest, instanceURL, appID, rootURI)
}

func validManifestedProject(path, instanceURL, appID, rootURI string) (persistentSyncManifest, bool) {
	path, err := canonicalProjectPath(path)
	if err != nil {
		return persistentSyncManifest{}, false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return persistentSyncManifest{}, false
	}
	manifest, ok, err := loadPersistentSyncManifest(path)
	if err != nil || !ok || !manifestMatchesProject(manifest, instanceURL, appID, rootURI) {
		return persistentSyncManifest{}, false
	}
	return manifest, true
}

func validCanonicalManifestedProject(path, instanceURL, appID, rootURI string) (persistentSyncManifest, bool) {
	path, err := canonicalProjectPath(path)
	if err != nil {
		return persistentSyncManifest{}, false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return persistentSyncManifest{}, false
	}
	manifest, ok, err := loadPersistentSyncManifest(path)
	if err != nil || !ok || !canonicalManifestMatchesProject(manifest, instanceURL, appID, rootURI) {
		return persistentSyncManifest{}, false
	}
	return manifest, true
}

func matchingManifestedAncestor(instanceURL, appID, rootURI string) (string, persistentSyncManifest, bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "", persistentSyncManifest{}, false
	}
	for dir := filepath.Clean(wd); ; dir = filepath.Dir(dir) {
		if manifest, ok := validManifestedProject(dir, instanceURL, appID, rootURI); ok {
			return dir, manifest, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", persistentSyncManifest{}, false
}

func registryProject(registry projectRegistry, instanceURL, appID string) (registeredProject, bool) {
	project, ok := registry.Projects[projectRegistryKey(instanceURL, appID)]
	return project, ok
}

func projectCheckoutByID(project registeredProject, selector string) (projectCheckout, bool) {
	selector = strings.TrimSpace(selector)
	for _, checkout := range project.Checkouts {
		if strings.EqualFold(checkout.ID, selector) {
			return checkout, true
		}
	}
	return projectCheckout{}, false
}

func chooseValidRegisteredCheckout(project registeredProject, instanceURL, appID, rootURI string, primaryOnly bool) (projectCheckout, persistentSyncManifest, bool) {
	if primaryOnly && project.PrimaryCheckoutID != "" {
		if checkout, ok := projectCheckoutByID(project, project.PrimaryCheckoutID); ok {
			if manifest, valid := validManifestedProject(checkout.Path, instanceURL, appID, rootURI); valid {
				return checkout, manifest, true
			}
		}
		return projectCheckout{}, persistentSyncManifest{}, false
	}
	for _, checkout := range project.Checkouts {
		if manifest, valid := validManifestedProject(checkout.Path, instanceURL, appID, rootURI); valid {
			return checkout, manifest, true
		}
	}
	return projectCheckout{}, persistentSyncManifest{}, false
}

func validRegisteredCheckouts(project registeredProject, instanceURL, appID, rootURI string) ([]projectCheckout, map[string]persistentSyncManifest) {
	valid := make([]projectCheckout, 0, len(project.Checkouts))
	manifests := make(map[string]persistentSyncManifest, len(project.Checkouts))
	for _, checkout := range project.Checkouts {
		manifest, ok := validManifestedProject(checkout.Path, instanceURL, appID, rootURI)
		if !ok {
			continue
		}
		if checkout.ID == "" {
			checkout.ID = manifest.CheckoutID
		}
		if checkout.ID == "" {
			continue
		}
		valid = append(valid, checkout)
		manifests[checkout.ID] = manifest
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].ID < valid[j].ID })
	return valid, manifests
}

func registeredCheckoutAtPath(project registeredProject, path string) (projectCheckout, bool) {
	path, err := canonicalProjectPath(path)
	if err != nil {
		return projectCheckout{}, false
	}
	for _, checkout := range project.Checkouts {
		if checkout.Path == path {
			return checkout, true
		}
	}
	return projectCheckout{}, false
}

func (c *Client) projectRegistryIdentity(appID string) (string, string) {
	return normalizedProjectInstance(c.cfg.InstanceURL), strings.TrimSpace(appID)
}

func (c *Client) isRegisteredPersistentProjectPath(path, appID string) bool {
	_, ok := c.registeredPersistentProjectCheckout(path, appID)
	return ok
}

func (c *Client) registeredPersistentProjectCheckout(path, appID string) (projectCheckout, bool) {
	path, err := canonicalProjectPath(path)
	if err != nil {
		return projectCheckout{}, false
	}
	instanceURL, appID := c.projectRegistryIdentity(appID)
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		return projectCheckout{}, false
	}
	project, ok := registryProject(registry, instanceURL, appID)
	if !ok {
		return projectCheckout{}, false
	}
	return registeredCheckoutAtPath(project, path)
}

func (c *Client) resolvePersistentProject(appID, appName, rootURI string, materialize bool) (persistentProjectResolution, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return persistentProjectResolution{}, errors.New("persistent local project requires an app id")
	}
	instanceURL, appID := c.projectRegistryIdentity(appID)
	if override := strings.TrimSpace(c.localProjectOverride); override != "" {
		if manifest, ok := validManifestedProject(override, instanceURL, appID, rootURI); ok {
			return persistentProjectResolution{Dir: override, CheckoutID: manifest.CheckoutID, Source: "current-process", Registered: false}, nil
		}
		c.localProjectOverride = ""
	}
	if dir, manifest, ok := matchingManifestedAncestor(instanceURL, appID, rootURI); ok {
		return persistentProjectResolution{Dir: dir, CheckoutID: manifest.CheckoutID, Source: "current directory", Registered: false}, nil
	}
	candidate, err := c.persistentProjectDirForAppNamed(appID, appName)
	if err != nil {
		return persistentProjectResolution{}, err
	}
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		return persistentProjectResolution{}, err
	}
	project, haveProject := registryProject(registry, instanceURL, appID)
	if !materialize {
		// Read-only callers must remain able to inspect a valid registered
		// checkout when the shared canonical name is stale or ambiguous. They do
		// not claim, repair, prune, or otherwise mutate either location.
		if manifest, ok := validCanonicalManifestedProject(candidate, instanceURL, appID, rootURI); ok {
			return persistentProjectResolution{Dir: candidate, CheckoutID: manifest.CheckoutID, Source: "canonical checkout"}, nil
		}
		if haveProject {
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
				return persistentProjectResolution{Dir: chosen.Path, CheckoutID: firstNonBlank(chosen.ID, manifests[chosen.ID].CheckoutID), Source: "registered checkout", Registered: true, DurablePrimary: chosen.ID == project.PrimaryCheckoutID}, nil
			}
		}
		return persistentProjectResolution{}, os.ErrNotExist
	}
	canonicalCollisionErr := canonicalProjectCollision(candidate, instanceURL, appID, rootURI)
	if canonicalCollisionErr != nil {
		// A v1 manifest at the canonical name is ambiguous because the path
		// carries no instance identity. The sole safe exception is an exact
		// current-instance registry binding; the next mutating sync pins it v2.
		if checkout, registered := registeredCheckoutAtPath(project, candidate); haveProject && registered {
			if manifest, valid := validManifestedProject(candidate, instanceURL, appID, rootURI); valid {
				return persistentProjectResolution{Dir: candidate, CheckoutID: firstNonBlank(checkout.ID, manifest.CheckoutID), Source: "registered checkout", Registered: true, DurablePrimary: checkout.ID == project.PrimaryCheckoutID}, nil
			}
		}
	}
	if canonicalCollisionErr == nil {
		if manifest, ok := validCanonicalManifestedProject(candidate, instanceURL, appID, rootURI); ok {
			return persistentProjectResolution{Dir: candidate, CheckoutID: manifest.CheckoutID, Source: "canonical checkout", Registered: false}, nil
		}
	}
	if haveProject {
		valid, manifests := validRegisteredCheckouts(project, instanceURL, appID, rootURI)
		if len(valid) == 0 {
			delete(registry.Projects, projectRegistryKey(instanceURL, appID))
			if err := saveProjectRegistry(c.opts.Profile, registry); err != nil {
				return persistentProjectResolution{}, err
			}
		} else {
			chosen := valid[0]
			if primary, found := projectCheckoutByID(project, project.PrimaryCheckoutID); found {
				for _, checkout := range valid {
					if checkout.ID == primary.ID {
						chosen = checkout
						break
					}
				}
			}
			// Persist pruning and deterministic promotion before returning, so a
			// later process cannot create a duplicate from stale registry state.
			project.Checkouts = valid
			project.PrimaryCheckoutID = chosen.ID
			registry.Projects[projectRegistryKey(instanceURL, appID)] = project
			if err := saveProjectRegistry(c.opts.Profile, registry); err != nil {
				return persistentProjectResolution{}, err
			}
			return persistentProjectResolution{Dir: chosen.Path, CheckoutID: firstNonBlank(chosen.ID, manifests[chosen.ID].CheckoutID), Source: "registered checkout", Registered: true, DurablePrimary: chosen.ID == project.PrimaryCheckoutID}, nil
		}
	}
	if canonicalCollisionErr != nil {
		return persistentProjectResolution{}, canonicalCollisionErr
	}
	// Do not claim a non-empty name collision. The caller will emit an explicit
	// error instead of creating an ambiguous second checkout.
	return persistentProjectResolution{Dir: candidate, Source: "new canonical location"}, nil
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (c *Client) registerPersistentProject(path, appID, rootURI, checkoutID string, makePrimary bool) (projectCheckout, error) {
	path, err := canonicalProjectPath(path)
	if err != nil {
		return projectCheckout{}, err
	}
	instanceURL, appID := c.projectRegistryIdentity(appID)
	manifest, valid := validManifestedProject(path, instanceURL, appID, rootURI)
	if !valid {
		return projectCheckout{}, errors.New("project path does not contain a matching local sync manifest")
	}
	if checkoutID == "" {
		checkoutID = manifest.CheckoutID
	}
	if checkoutID == "" {
		checkoutID, err = randomCheckoutID()
		if err != nil {
			return projectCheckout{}, err
		}
	}
	manifestChanged := manifest.CheckoutID != checkoutID || manifest.Version != 2 || normalizedProjectInstance(manifest.InstanceURL) != instanceURL
	if manifestChanged {
		// Pin legacy v1 manifests to the current normalized instance while
		// preserving their existing checksum algorithm and baseline.
		manifest.Version = 2
		manifest.InstanceURL = instanceURL
		manifest.CheckoutID = checkoutID
		if err := writePersistentSyncManifest(path, manifest); err != nil {
			return projectCheckout{}, err
		}
	}
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		return projectCheckout{}, err
	}
	key := projectRegistryKey(instanceURL, appID)
	project := registry.Projects[key]
	project.InstanceURL = instanceURL
	project.AppID = appID
	found := false
	for i := range project.Checkouts {
		if strings.EqualFold(project.Checkouts[i].ID, checkoutID) && project.Checkouts[i].Path != path {
			if _, stillValid := validManifestedProject(project.Checkouts[i].Path, instanceURL, appID, rootURI); stillValid {
				return projectCheckout{}, errors.New("checkout id is already registered at a different path")
			}
			project.Checkouts[i].Path = path
			found = true
			continue
		}
		if project.Checkouts[i].Path == path {
			project.Checkouts[i].Path = path
			project.Checkouts[i].ID = checkoutID
			found = true
		}
	}
	checkout := projectCheckout{ID: checkoutID, Path: path, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if !found {
		project.Checkouts = append(project.Checkouts, checkout)
	}
	if makePrimary || project.PrimaryCheckoutID == "" {
		project.PrimaryCheckoutID = checkoutID
	}
	sort.Slice(project.Checkouts, func(i, j int) bool { return project.Checkouts[i].ID < project.Checkouts[j].ID })
	registry.Projects[key] = project
	if err := saveProjectRegistry(c.opts.Profile, registry); err != nil {
		return projectCheckout{}, err
	}
	return checkout, nil
}

// registerPersistentProjectSecondary records a verified checkout. The first
// explicit checkout is also the durable primary; later explicit checkouts stay
// secondary unless /project primary changes the choice.
func (c *Client) registerPersistentProjectSecondary(path, appID, rootURI, checkoutID string) (projectCheckout, error) {
	_, normalizedAppID := c.projectRegistryIdentity(appID)
	return c.registerPersistentProject(path, normalizedAppID, rootURI, checkoutID, false)
}

// listPersistentProjectCheckouts returns only validated checkouts. It is a
// read-only command helper: current/list must not create or rewrite registry
// content merely to inspect the available checkout set.
func (c *Client) listPersistentProjectCheckouts(appID, rootURI string) ([]projectCheckout, error) {
	instanceURL, appID := c.projectRegistryIdentity(appID)
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		return nil, err
	}
	project, ok := registryProject(registry, instanceURL, appID)
	if !ok {
		return nil, nil
	}
	valid, _ := validRegisteredCheckouts(project, instanceURL, appID, rootURI)
	return valid, nil
}

var errRegisteredCheckoutNotFound = errors.New("registered checkout not found")

func (c *Client) lookupPersistentProjectCheckout(appID, rootURI, selector string) (projectCheckout, error) {
	checkouts, err := c.listPersistentProjectCheckouts(appID, rootURI)
	if err != nil {
		return projectCheckout{}, err
	}
	selector = strings.TrimSpace(selector)
	canonicalSelector, canonicalErr := canonicalProjectPath(selector)
	for _, checkout := range checkouts {
		if strings.EqualFold(checkout.ID, selector) || checkout.Path == selector || (canonicalErr == nil && checkout.Path == canonicalSelector) {
			return checkout, nil
		}
	}
	return projectCheckout{}, errRegisteredCheckoutNotFound
}

// usePersistentProjectCheckout is process-local by design; it never changes
// another shell's selected checkout or the registry primary.
func (c *Client) usePersistentProjectCheckout(appID, rootURI, selector string) (projectCheckout, error) {
	checkout, err := c.lookupPersistentProjectCheckout(appID, rootURI, selector)
	if err != nil {
		return projectCheckout{}, err
	}
	c.localProjectOverride = checkout.Path
	return checkout, nil
}

func (c *Client) setPersistentProjectPrimary(appID, rootURI, selector string) (projectCheckout, error) {
	checkout, err := c.lookupPersistentProjectCheckout(appID, rootURI, selector)
	if err != nil {
		return projectCheckout{}, err
	}
	instanceURL, appID := c.projectRegistryIdentity(appID)
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		return projectCheckout{}, err
	}
	project := registry.Projects[projectRegistryKey(instanceURL, appID)]
	project.PrimaryCheckoutID = checkout.ID
	registry.Projects[projectRegistryKey(instanceURL, appID)] = project
	if err := saveProjectRegistry(c.opts.Profile, registry); err != nil {
		return projectCheckout{}, err
	}
	return checkout, nil
}

func (c *Client) forgetPersistentProjectCheckout(appID, rootURI, selector string) error {
	checkout, err := c.lookupPersistentProjectCheckout(appID, rootURI, selector)
	if err != nil {
		return err
	}
	instanceURL, appID := c.projectRegistryIdentity(appID)
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		return err
	}
	key := projectRegistryKey(instanceURL, appID)
	project := registry.Projects[key]
	kept := make([]projectCheckout, 0, len(project.Checkouts)-1)
	for _, candidate := range project.Checkouts {
		if candidate.ID != checkout.ID {
			kept = append(kept, candidate)
		}
	}
	if len(kept) == 0 {
		delete(registry.Projects, key)
	} else {
		sort.Slice(kept, func(i, j int) bool { return kept[i].ID < kept[j].ID })
		project.Checkouts = kept
		if project.PrimaryCheckoutID == checkout.ID {
			project.PrimaryCheckoutID = kept[0].ID
		}
		registry.Projects[key] = project
	}
	return saveProjectRegistry(c.opts.Profile, registry)
}

// registerPersistentProjectClone gives a copied checkout an independent ID
// before registering it, so it cannot alias the source checkout in the
// profile registry.
func (c *Client) registerPersistentProjectClone(path, appID, rootURI string, makePrimary bool) (projectCheckout, error) {
	path, err := canonicalProjectPath(path)
	if err != nil {
		return projectCheckout{}, err
	}
	instanceURL, normalizedAppID := c.projectRegistryIdentity(appID)
	manifest, ok := validManifestedProject(path, instanceURL, normalizedAppID, rootURI)
	if !ok {
		return projectCheckout{}, errors.New("clone path does not contain a matching local sync manifest")
	}
	id, err := randomCheckoutID()
	if err != nil {
		return projectCheckout{}, err
	}
	manifest.CheckoutID = id
	if err := writePersistentSyncManifest(path, manifest); err != nil {
		return projectCheckout{}, err
	}
	return c.registerPersistentProject(path, normalizedAppID, rootURI, id, makePrimary)
}

func (c *Client) pruneProjectRegistry(appID, rootURI string) error {
	instanceURL, appID := c.projectRegistryIdentity(appID)
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		return err
	}
	key := projectRegistryKey(instanceURL, appID)
	project, ok := registry.Projects[key]
	if !ok {
		return nil
	}
	kept := make([]projectCheckout, 0, len(project.Checkouts))
	for _, checkout := range project.Checkouts {
		if _, valid := validManifestedProject(checkout.Path, instanceURL, appID, rootURI); valid {
			kept = append(kept, checkout)
		}
	}
	if len(kept) == 0 {
		delete(registry.Projects, key)
		return saveProjectRegistry(c.opts.Profile, registry)
	}
	project.Checkouts = kept
	if _, ok := projectCheckoutByID(project, project.PrimaryCheckoutID); !ok {
		project.PrimaryCheckoutID = kept[0].ID
	}
	registry.Projects[key] = project
	return saveProjectRegistry(c.opts.Profile, registry)
}

func (c *Client) activeProjectPath() string {
	if c.currentApp == nil {
		return ""
	}
	appID := c.activeAppSysID()
	if appID == "" {
		return ""
	}
	rootURI := "now-file:/" + strings.TrimLeft(appID, "/")
	resolution, err := c.resolvePersistentProject(appID, c.persistentAppDisplayName(appID), rootURI, false)
	if err != nil {
		return ""
	}
	return resolution.Dir
}
