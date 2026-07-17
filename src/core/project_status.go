package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// postAppSelectionStatus reports the current checkout without creating or
// registering one. A selection must remain successful even when the optional
// local status probe cannot reach the instance.
func (c *Client) postAppSelectionStatus(ctx context.Context) {
	if c.currentApp == nil || strings.TrimSpace(c.activeAppSysID()) == "" {
		return
	}
	if c.postSelectionStatus != nil {
		if err := c.postSelectionStatus(ctx); err != nil {
			slashCommandPrintf("warning: local project status unavailable: %v\n", err)
		}
		c.drawPersistentStatus()
		return
	}

	appID := c.activeAppSysID()
	appName := c.persistentAppDisplayName(appID)
	rootURI := c.activeAppRootURI()
	resolution, err := c.resolveExistingPersistentProject(appID, appName, rootURI)
	if errors.Is(err, os.ErrNotExist) {
		c.setStatusProjectPath("")
		expected, pathErr := c.persistentProjectDirForAppNamed(appID, appName)
		if pathErr == nil {
			slashCommandPrintf("local project: not materialized (expected: %s)\n", expected)
		} else {
			slashCommandPrintln("local project: not materialized")
		}
		c.drawPersistentStatus()
		return
	}
	if err != nil {
		c.setStatusProjectPath("")
		slashCommandPrintf("warning: local project status unavailable: %v\n", err)
		c.drawPersistentStatus()
		return
	}
	c.setStatusProjectPath(resolution.Dir)

	unlock, err := acquirePersistentProjectLock(ctx, resolution.Dir)
	if err != nil {
		slashCommandPrintf("warning: local project status unavailable: %v\n", err)
		c.drawPersistentStatus()
		return
	}
	defer unlock()
	result, err := c.syncPersistentAppAtLocked(ctx, resolution.Dir, appID, rootURI, persistentSyncStatus)
	if err != nil {
		slashCommandPrintf("warning: local project status unavailable: %v\n", err)
		c.drawPersistentStatus()
		return
	}
	slashCommandPrintf("local project: %s\nstatus: %s\n", result.LocalDir, classifyPersistentSyncStatus(result))
	c.drawPersistentStatus()
}

// resolveExistingPersistentProject is the read-only counterpart to
// resolvePersistentProject. In particular it never prunes or saves the
// registry, creates directories, or changes the process-local override.
func (c *Client) resolveExistingPersistentProject(appID, appName, rootURI string) (persistentProjectResolution, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return persistentProjectResolution{}, errors.New("persistent local project requires an app id")
	}
	instanceURL, appID := c.projectRegistryIdentity(appID)
	if override := strings.TrimSpace(c.localProjectOverride); override != "" {
		if manifest, ok := validManifestedProject(override, instanceURL, appID, rootURI); ok {
			return persistentProjectResolution{Dir: override, CheckoutID: manifest.CheckoutID, Source: "current-process"}, nil
		}
		c.localProjectOverride = ""
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

func (c *Client) activeLocalProjectPath() string {
	return c.cachedStatusProjectPath()
}

func (c *Client) setStatusProjectPath(path string) {
	c.statusProjectMu.Lock()
	c.statusProjectPath = path
	c.statusProjectMu.Unlock()
}

func (c *Client) cachedStatusProjectPath() string {
	c.statusProjectMu.RLock()
	path := c.statusProjectPath
	c.statusProjectMu.RUnlock()
	return path
}

// refreshStatusProjectPath resolves only at explicit state-transition points;
// the terminal footer reads this cache and never touches the filesystem.
func (c *Client) refreshStatusProjectPath(appID, appName, rootURI string) {
	resolution, err := c.resolveExistingPersistentProject(appID, appName, rootURI)
	if err != nil {
		c.setStatusProjectPath("")
		return
	}
	c.setStatusProjectPath(resolution.Dir)
}

func classifyPersistentSyncStatus(result persistentSyncResult) string {
	pendingPull := result.PendingPull
	if len(pendingPull) == 0 {
		pendingPull = result.Pulled
	}
	switch {
	case len(result.Conflicts) > 0:
		return fmt.Sprintf("conflicts (%d)", len(result.Conflicts))
	case len(result.PendingPush) > 0 && len(pendingPull) > 0:
		return "local changes pending push; remote changes pending pull"
	case len(result.PendingPush) > 0:
		return "local changes pending push"
	case len(pendingPull) > 0:
		return "remote changes pending pull"
	default:
		return "clean"
	}
}
