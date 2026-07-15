package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const projectCommandUsage = "usage: /project <help|current|list|use <path|checkout-id>|clone-here [directory-name]|primary <path|checkout-id>|forget <path|checkout-id>>"

func projectCommandIdentity(c *Client) (appID, appName, rootURI string, err error) {
	appID = strings.TrimSpace(c.activeAppSysID())
	if appID == "" {
		return "", "", "", errors.New("/project requires an active app; choose one with /app")
	}
	return appID, c.persistentAppDisplayName(appID), "now-file:/" + strings.TrimLeft(appID, "/"), nil
}

func (c *Client) projectPrimaryCheckoutID(appID string) (string, error) {
	instanceURL, appID := c.projectRegistryIdentity(appID)
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		return "", err
	}
	project, ok := registryProject(registry, instanceURL, appID)
	if !ok {
		return "", nil
	}
	return project.PrimaryCheckoutID, nil
}

func projectPathMarkers(c *Client, appID, rootURI string) (string, string) {
	override := strings.TrimSpace(c.localProjectOverride)
	current := ""
	if dir, _, ok := matchingManifestedAncestor(c.cfg.InstanceURL, appID, rootURI); ok {
		current = dir
	}
	return override, current
}

func printProjectCheckout(checkout projectCheckout, primaryID, override, current string) {
	markers := make([]string, 0, 3)
	if strings.EqualFold(checkout.ID, primaryID) {
		markers = append(markers, "primary")
	}
	if checkout.Path == override {
		markers = append(markers, "current-process")
	}
	if checkout.Path == current {
		markers = append(markers, "current-directory")
	}
	line := fmt.Sprintf("%s  %s", checkout.ID, checkout.Path)
	if len(markers) > 0 {
		line += "  [" + strings.Join(markers, ", ") + "]"
	}
	slashCommandPrintln(line)
}

func (c *Client) projectSelectOrRegister(appID, rootURI, selector string) (projectCheckout, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return projectCheckout{}, errors.New(projectCommandUsage)
	}
	if checkout, err := c.lookupPersistentProjectCheckout(appID, rootURI, selector); err == nil {
		return checkout, nil
	} else if !errors.Is(err, errRegisteredCheckoutNotFound) {
		return projectCheckout{}, err
	}
	path, err := canonicalProjectPath(selector)
	if err != nil {
		return projectCheckout{}, err
	}
	if _, valid := validManifestedProject(path, c.cfg.InstanceURL, appID, rootURI); !valid {
		return projectCheckout{}, errors.New("project path does not contain a matching local sync manifest")
	}
	return c.registerPersistentProjectSecondary(path, appID, rootURI, "")
}

func projectCloneDestination(appName, appID, requested string) (string, error) {
	name := strings.TrimSpace(requested)
	if name == "" {
		var err error
		name, err = persistentAppDirName(appName, appID)
		if err != nil {
			return "", err
		}
	}
	if filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return "", errors.New("clone directory must be a single safe directory name")
	}
	if sanitized, err := persistentAppDirName(name, ""); err != nil || sanitized != name {
		return "", errors.New("clone directory must be a safe unchanged directory name")
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, name), nil
}

func (c *Client) clonePersistentProjectHere(ctx context.Context, appID, appName, rootURI, requested string) (projectCheckout, error) {
	destination, err := projectCloneDestination(appName, appID, requested)
	if err != nil {
		return projectCheckout{}, err
	}
	if strings.TrimSpace(requested) == "" {
		primaryID, err := c.projectPrimaryCheckoutID(appID)
		if err != nil {
			return projectCheckout{}, err
		}
		if primaryID != "" {
			if primary, err := c.lookupPersistentProjectCheckout(appID, rootURI, primaryID); err == nil && filepath.Clean(primary.Path) == filepath.Clean(destination) {
				return projectCheckout{}, errors.New("default clone directory is already the primary checkout here; specify a different directory name")
			}
		}
	}
	if info, err := os.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return projectCheckout{}, fmt.Errorf("clone destination %q is not a safe directory", destination)
		}
		entries, err := os.ReadDir(destination)
		if err != nil {
			return projectCheckout{}, err
		}
		if len(entries) != 0 {
			return projectCheckout{}, fmt.Errorf("clone destination %q is not empty", destination)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return projectCheckout{}, err
	}
	if err := ensurePersistentProjectRoot(destination); err != nil {
		return projectCheckout{}, err
	}
	unlock, err := acquirePersistentProjectLock(ctx, destination)
	if err != nil {
		return projectCheckout{}, err
	}
	defer unlock()
	_, remoteFiles, err := c.fetchPersistentRemoteFiles(ctx, rootURI)
	if err != nil {
		return projectCheckout{}, err
	}
	for _, rel := range sortedContentKeys(remoteFiles) {
		path, err := persistentSafeLocalWritePath(destination, rel)
		if err != nil {
			return projectCheckout{}, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return projectCheckout{}, err
		}
		if err := os.WriteFile(path, remoteFiles[rel], 0o644); err != nil {
			return projectCheckout{}, err
		}
	}
	if err := savePersistentSyncManifest(destination, persistentSyncManifest{
		InstanceURL: c.cfg.InstanceURL,
		AppID:       appID,
		Scope:       c.activeAppScopeName(),
		RootURI:     rootURI,
		Files:       fileChecksums(remoteFiles),
	}); err != nil {
		return projectCheckout{}, err
	}
	manifest, ok, err := loadPersistentSyncManifest(destination)
	if err != nil || !ok {
		return projectCheckout{}, firstNonNil(err, errors.New("clone local sync manifest was not written"))
	}
	return c.registerPersistentProjectSecondary(destination, appID, rootURI, manifest.CheckoutID)
}

func handleProjectCommand(ctx context.Context, c *Client, args []string) error {
	if len(args) == 0 || strings.EqualFold(args[0], "help") {
		slashCommandPrintln(projectCommandUsage)
		slashCommandPrintln("/project use selects only this process; /project primary changes the persisted default.")
		return nil
	}
	appID, appName, rootURI, err := projectCommandIdentity(c)
	if err != nil {
		return err
	}
	subcommand := strings.ToLower(strings.TrimSpace(args[0]))
	switch subcommand {
	case "current":
		if len(args) != 1 {
			return errors.New(projectCommandUsage)
		}
		resolution, resolveErr := c.resolveExistingPersistentProject(appID, appName, rootURI)
		primaryID, err := c.projectPrimaryCheckoutID(appID)
		if err != nil {
			return err
		}
		slashCommandPrintf("app: %s\ninstance: %s\n", appID, normalizedProjectInstance(c.cfg.InstanceURL))
		if resolveErr != nil {
			if errors.Is(resolveErr, os.ErrNotExist) {
				slashCommandPrintln("current: none")
				return nil
			}
			return resolveErr
		}
		marker := ""
		if strings.EqualFold(resolution.CheckoutID, primaryID) {
			marker = " [primary]"
		}
		slashCommandPrintf("current: %s\nsource: %s\ncheckout: %s%s\n", resolution.Dir, resolution.Source, resolution.CheckoutID, marker)
		return nil
	case "list":
		if len(args) != 1 {
			return errors.New(projectCommandUsage)
		}
		checkouts, err := c.listPersistentProjectCheckouts(appID, rootURI)
		if err != nil {
			return err
		}
		if len(checkouts) == 0 {
			slashCommandPrintln("no registered checkouts")
			return nil
		}
		sort.Slice(checkouts, func(i, j int) bool { return checkouts[i].ID < checkouts[j].ID })
		primaryID, err := c.projectPrimaryCheckoutID(appID)
		if err != nil {
			return err
		}
		override, current := projectPathMarkers(c, appID, rootURI)
		for _, checkout := range checkouts {
			printProjectCheckout(checkout, primaryID, override, current)
		}
		return nil
	case "use":
		if len(args) != 2 {
			return errors.New(projectCommandUsage)
		}
		checkout, err := c.projectSelectOrRegister(appID, rootURI, args[1])
		if err != nil {
			return err
		}
		c.localProjectOverride = checkout.Path
		c.refreshStatusProjectPath(appID, appName, rootURI)
		slashCommandPrintf("using checkout %s: %s (current process only)\n", checkout.ID, checkout.Path)
		return nil
	case "clone-here":
		if len(args) > 2 {
			return errors.New(projectCommandUsage)
		}
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		checkout, err := c.clonePersistentProjectHere(ctx, appID, appName, rootURI, name)
		if err != nil {
			return err
		}
		c.localProjectOverride = checkout.Path
		c.refreshStatusProjectPath(appID, appName, rootURI)
		slashCommandPrintf("cloned checkout %s: %s (current process only)\n", checkout.ID, checkout.Path)
		return nil
	case "primary":
		if len(args) != 2 {
			return errors.New(projectCommandUsage)
		}
		checkout, err := c.setPersistentProjectPrimary(appID, rootURI, args[1])
		if err != nil {
			return err
		}
		c.localProjectOverride = checkout.Path
		c.refreshStatusProjectPath(appID, appName, rootURI)
		slashCommandPrintf("primary checkout %s: %s\n", checkout.ID, checkout.Path)
		return nil
	case "forget":
		if len(args) != 2 {
			return errors.New(projectCommandUsage)
		}
		checkout, err := c.lookupPersistentProjectCheckout(appID, rootURI, args[1])
		if err != nil {
			return err
		}
		if err := c.forgetPersistentProjectCheckout(appID, rootURI, args[1]); err != nil {
			return err
		}
		if filepath.Clean(c.localProjectOverride) == filepath.Clean(checkout.Path) {
			c.localProjectOverride = ""
		}
		c.refreshStatusProjectPath(appID, appName, rootURI)
		primaryID, err := c.projectPrimaryCheckoutID(appID)
		if err != nil {
			return err
		}
		if primaryID == "" {
			slashCommandPrintf("forgot checkout %s\n", checkout.ID)
			return nil
		}
		slashCommandPrintf("forgot checkout %s; promoted %s to primary\n", checkout.ID, primaryID)
		return nil
	default:
		return errors.New(projectCommandUsage)
	}
}
