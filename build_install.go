package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"os"
	"os/exec"
	posixpath "path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

const (
	buildInstallPollInterval = time.Second
	buildInstallPollMax      = 10 * time.Minute
)

type gliderBuildState struct {
	AppID          string
	RootURI        string
	Scope          string
	AppName        string
	Version        string
	TempDir        string
	PackageZip     string
	Persistent     bool
	BuildSucceeded bool
	BuiltAt        time.Time
}

type buildInstallContext struct {
	AppID     string
	RootURI   string
	Scope     string
	ScopeID   string
	AppName   string
	Version   string
	NowConfig map[string]interface{}
	Package   packageJSONInfo
}

type packageJSONInfo struct {
	Name                 string
	Version              string
	Dependencies         map[string]string
	DevDependencies      map[string]string
	OptionalDependencies map[string]string
	Scripts              map[string]string
}

type syncedBuildProject struct {
	Dir         string
	RootURI     string
	Entries     map[string]gliderChangeEntry
	Files       map[string]string
	Original    map[string][]byte
	PendingPush []string
}

type installProgressResult struct {
	Status        string
	StatusLabel   string
	StatusMessage string
	StatusDetail  string
	Error         string
	Percent       int64
	Successful    bool
	Failed        bool
}

func (c *Client) buildInstallProgress(format string, args ...interface{}) {
	if c.activeTurnCancelled() {
		return
	}
	message := strings.TrimSpace(fmt.Sprintf(format, args...))
	if message == "" {
		return
	}
	c.flushActiveStreamForTerminalInterruption()
	if terminalRecordSystemTextAndAppend("Build", message, c.statusBarState()) {
		return
	}
	fmt.Fprintln(os.Stderr, message)
}

func (c *Client) answerBuildInstallParity(ctx context.Context, action string, payload map[string]interface{}) (map[string]interface{}, string) {
	if err := ctx.Err(); err != nil {
		return map[string]interface{}{"error": err.Error(), "code": "CANCELLED"}, "error"
	}
	action = strings.ToLower(strings.TrimSpace(action))
	switch action {
	case "install_dependencies":
		return c.answerInstallDependenciesParity(ctx, payload)
	case "build":
		return c.answerBuildParity(ctx, payload)
	case "install":
		return c.answerInstallParity(ctx, payload)
	case "build_install":
		result, status := c.answerBuildParity(ctx, payload)
		if status != "complete" {
			return result, status
		}
		return c.answerInstallParity(ctx, payload)
	default:
		return buildInstallError(fmt.Sprintf("Unsupported build/install action %q", action), "UNSUPPORTED_BUILD_ACTION", nil), "error"
	}
}

func (c *Client) answerInstallDependenciesParity(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	ctxInfo, project, cleanup, err := c.prepareBuildProject(ctx, payload)
	if err != nil {
		return buildInstallError(err.Error(), buildInstallErrorCode(err), nil), "error"
	}
	defer cleanup()

	missing := missingNodeDependencies(project.Dir, buildDependencies(ctxInfo.Package))
	if len(missing) == 0 {
		msg := fmt.Sprintf("Dependencies already available for ServiceNow application %s.", ctxInfo.displayName())
		return buildInstallSuccess(msg, map[string]interface{}{"path": payloadPathOrDot(payload), "missingDependencies": []string{}, "ideContext": c.currentIDEContext()}), "complete"
	}
	if err := c.installProjectDependencies(ctx, project.Dir, missing); err != nil {
		return buildInstallError(err.Error(), buildInstallErrorCode(err), map[string]interface{}{"missingDependencies": missing}), "error"
	}
	c.replaceLastGliderBuild(&gliderBuildState{
		AppID:          ctxInfo.AppID,
		RootURI:        ctxInfo.RootURI,
		Scope:          ctxInfo.Scope,
		AppName:        ctxInfo.AppName,
		Version:        ctxInfo.Version,
		TempDir:        project.Dir,
		Persistent:     true,
		BuildSucceeded: false,
		BuiltAt:        time.Now(),
	})
	msg := fmt.Sprintf("Dependencies installed for ServiceNow application %s.", ctxInfo.displayName())
	return buildInstallSuccess(msg, map[string]interface{}{"path": payloadPathOrDot(payload), "installedDependencies": missing, "ideContext": c.currentIDEContext()}), "complete"
}

func (c *Client) answerBuildParity(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	c.buildOperationMu.Lock()
	defer c.buildOperationMu.Unlock()
	c.invalidateLastGliderBuild()
	ctxInfo, project, cleanup, err := c.prepareBuildProject(ctx, payload)
	if err != nil {
		return buildInstallError(err.Error(), buildInstallErrorCode(err), nil), "error"
	}
	defer cleanup()

	missing := missingNodeDependencies(project.Dir, buildDependencies(ctxInfo.Package))
	if len(missing) > 0 {
		if err := c.installProjectDependencies(ctx, project.Dir, missing); err != nil {
			return buildInstallError(err.Error(), buildInstallErrorCode(err), map[string]interface{}{"missingDependencies": missing}), "error"
		}
	} else {
		c.buildInstallProgress("build: dependencies already available")
	}

	metadataSync, err := c.syncMetadataBeforeBuild(ctx, project)
	if err != nil {
		return buildInstallError(err.Error(), buildInstallErrorCode(err), nil), "error"
	}

	if err := c.runNowSDKBuild(ctx, project.Dir); err != nil {
		return buildInstallError(err.Error(), buildInstallErrorCode(err), nil), "error"
	}
	c.buildInstallProgress("build: SDK build completed")

	warnings, err := c.persistGeneratedBuildFiles(ctx, project, ctxInfo)
	if err != nil {
		return buildInstallError(err.Error(), "GLIDER_WRITEBACK_FAILED", nil), "error"
	}
	if len(warnings) == 0 {
		c.buildInstallProgress("build: generated files synced to Glider")
	} else {
		c.buildInstallProgress("build: generated files synced to Glider with warning: %s", warnings[0])
	}

	if err := c.runNowSDKPack(ctx, project.Dir); err != nil {
		return buildInstallError(err.Error(), buildInstallErrorCode(err), nil), "error"
	}
	c.buildInstallProgress("build: SDK package artifact generated")

	zipPath, err := findPackageZip(project.Dir, ctxInfo.NowConfig)
	if err != nil {
		return buildInstallError(err.Error(), "PACKAGE_ZIP_NOT_FOUND", nil), "error"
	}
	c.buildInstallProgress("build: package artifact ready at %s", zipPath)

	c.replaceLastGliderBuild(&gliderBuildState{
		AppID:          ctxInfo.AppID,
		RootURI:        ctxInfo.RootURI,
		Scope:          ctxInfo.Scope,
		AppName:        ctxInfo.AppName,
		Version:        ctxInfo.Version,
		TempDir:        project.Dir,
		PackageZip:     zipPath,
		Persistent:     true,
		BuildSucceeded: true,
		BuiltAt:        time.Now(),
	})
	if warnings == nil {
		warnings = []interface{}{}
	}
	msg := fmt.Sprintf("ServiceNow application %s built successfully!", ctxInfo.displayName())
	result := map[string]interface{}{
		"path":          payloadPathOrDot(payload),
		"packageZip":    zipPath,
		"warnings":      warnings,
		"errors":        []interface{}{},
		"ideContext":    c.currentIDEContext(),
		"nowConfigJson": ctxInfo.NowConfig,
		"metadataSync":  metadataSync,
	}
	return buildInstallSuccess(msg, result), "complete"
}

func (c *Client) answerInstallParity(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	ctxInfo, err := c.resolveBuildInstallContext(ctx, payload)
	if err != nil {
		return buildInstallError(err.Error(), buildInstallErrorCode(err), nil), "error"
	}
	build := c.lastGliderBuild
	if build == nil || !build.BuildSucceeded || build.AppID != ctxInfo.AppID || strings.TrimSpace(build.PackageZip) == "" {
		msg := "Install requires a successful build for the active app in this CLI session. Run the build tool first."
		c.buildInstallProgress("install: %s", msg)
		return buildInstallError(msg, "BUILD_REQUIRED", map[string]interface{}{"appId": ctxInfo.AppID}), "error"
	}
	if _, err := os.Stat(build.PackageZip); err != nil {
		msg := fmt.Sprintf("Build package artifact is missing: %s", build.PackageZip)
		c.buildInstallProgress("install: %s", msg)
		return buildInstallError(msg, "PACKAGE_ZIP_NOT_FOUND", nil), "error"
	}
	if err := checkMetadataSyncState(build.TempDir); err != nil {
		c.buildInstallProgress("install: %s", err.Error())
		return buildInstallError(err.Error(), buildInstallErrorCode(err), nil), "error"
	}

	priorUpgradeID, err := c.queryPriorUpgradeHistory(ctx, ctxInfo.Scope)
	if err != nil {
		c.buildInstallProgress("install: warning: could not query prior upgrade history: %v", err)
	}
	scopeInfo, err := c.queryInstallScopeInfo(ctx, ctxInfo.AppID, ctxInfo.Scope)
	if err != nil {
		return buildInstallError(err.Error(), "SCOPE_LOOKUP_FAILED", nil), "error"
	}
	if name := strings.TrimSpace(firstString(scopeInfo, "name", "scope_name", "scopeName")); name != "" && ctxInfo.AppName == "" {
		ctxInfo.AppName = serviceNowFieldString(scopeInfo["name"])
	}

	tracker, rollback, err := c.uploadScopedAppPackage(ctx, build.PackageZip, ctxInfo)
	if err != nil {
		return buildInstallError(err.Error(), buildInstallErrorCode(err), nil), "error"
	}
	c.buildInstallProgress("install: package uploaded")

	var progress installProgressResult
	if tracker != "" {
		progress, err = c.pollInstallProgress(ctx, tracker)
	} else {
		progress, err = c.pollUpgradeHistoryFallback(ctx, ctxInfo.Scope, priorUpgradeID)
	}
	if err != nil {
		return buildInstallError(err.Error(), buildInstallErrorCode(err), map[string]interface{}{"executionTracker": tracker, "rollbackContext": rollback}), "error"
	}
	c.buildInstallProgress("install: %s", progress.displayMessage())

	links := c.discoverInstalledArtifactLinks(ctx, ctxInfo.AppID)
	c.buildInstallProgress("install: discovered %d installed artifact link(s)", len(links))

	msg := fmt.Sprintf("%s installed successfully!", ctxInfo.displayName())
	if len(links) > 0 {
		msg += "\n\n" + strings.Join(links, "\n")
	}
	return buildInstallSuccess(msg, map[string]interface{}{
		"path":             payloadPathOrDot(payload),
		"executionTracker": tracker,
		"rollbackContext":  rollback,
		"progress":         progress.toMap(),
		"links":            links,
		"ideContext":       c.currentIDEContext(),
		"nowConfigJson":    ctxInfo.NowConfig,
	}), "complete"
}

func (c *Client) prepareBuildProject(ctx context.Context, payload map[string]interface{}) (buildInstallContext, syncedBuildProject, func(), error) {
	ctxInfo, err := c.resolveBuildInstallContext(ctx, payload)
	if err != nil {
		return buildInstallContext{}, syncedBuildProject{}, func() {}, err
	}
	c.buildInstallProgress("build: synchronizing persistent local project for %s", ctxInfo.displayName())
	project, cleanup, err := c.syncGliderBuildProjectToPersistentNamed(ctx, ctxInfo.AppID, ctxInfo.AppName, ctxInfo.RootURI)
	if err != nil {
		var collision *canonicalProjectCollisionError
		if !errors.As(err, &collision) || !collision.Recoverable {
			return buildInstallContext{}, syncedBuildProject{}, func() {}, fmt.Errorf("Error while syncing Glider project for build: %w", err)
		}
		backup, err := c.recoverCanonicalBuildProject(ctx, collision, ctxInfo)
		if err != nil {
			return buildInstallContext{}, syncedBuildProject{}, func() {}, err
		}
		project, cleanup, err = c.syncGliderBuildProjectToPersistentNamed(ctx, ctxInfo.AppID, ctxInfo.AppName, ctxInfo.RootURI)
		if err != nil {
			partial := collision.Target + ".bacli-recovery-failed-" + time.Now().UTC().Format("20060102T150405.000000000Z")
			if moveErr := os.Rename(collision.Target, partial); moveErr == nil {
				restored, restoreErr := restoreCanonicalProject(backup, collision.Target)
				return buildInstallContext{}, syncedBuildProject{}, func() {}, codedError{Code: "PROJECT_RECOVERY_RECREATE_FAILED", Message: projectRecoveryRollbackMessage(err, backup, partial, restored, restoreErr)}
			}
			return buildInstallContext{}, syncedBuildProject{}, func() {}, codedError{Code: "PROJECT_RECOVERY_RECREATE_FAILED", Message: fmt.Sprintf("Recovered checkout sync failed (%v); original backup retained at %s and partial checkout remains at %s.", err, backup, collision.Target)}
		}
	}
	if len(project.PendingPush) > 0 {
		cleanup()
		return buildInstallContext{}, syncedBuildProject{}, func() {}, codedError{
			Code:    "SYNC_PUSH_REQUIRED",
			Message: fmt.Sprintf("Local project changes are pending for %s (%s). Run /sync push before building; the build will not upload pre-existing local edits.", ctxInfo.displayName(), strings.Join(project.PendingPush, ", ")),
		}
	}
	if _, ok := project.Files["now.config.json"]; !ok {
		cleanup()
		return buildInstallContext{}, syncedBuildProject{}, func() {}, errors.New("now.config.json was not found in the active Glider project")
	}
	if err := refreshBuildInstallContextFromProject(&ctxInfo, project); err != nil {
		cleanup()
		return buildInstallContext{}, syncedBuildProject{}, func() {}, err
	}
	return ctxInfo, project, cleanup, nil
}

const projectRecoveryPathLimit = 50

// recoverCanonicalBuildProject is deliberately reachable only from mutating
// build preparation. It first reads Web state, then asks for a non-bypassable
// confirmation before moving (never deleting) a conflicting checkout.
func (c *Client) recoverCanonicalBuildProject(ctx context.Context, collision *canonicalProjectCollisionError, info buildInstallContext) (string, error) {
	entries, remote, err := c.fetchPersistentRemoteFiles(ctx, info.RootURI)
	if err != nil {
		return "", codedError{Code: "PROJECT_RECOVERY_REMOTE_UNAVAILABLE", Message: fmt.Sprintf("Cannot offer local-project recovery for %s because the current Web project could not be read: %v", collision.Target, err)}
	}
	diagnostics, err := projectRecoveryDiagnostics(collision.Target, entries, remote)
	if err != nil {
		return "", codedError{Code: "PROJECT_RECOVERY_DIAGNOSTICS_FAILED", Message: fmt.Sprintf("Cannot inspect local-project collision at %s: %v", collision.Target, err)}
	}
	message := fmt.Sprintf("The canonical local project %s is ambiguous (%s). Recreate it strictly from the current Web project? The existing directory will be moved to a sibling backup and retained.\n%s", collision.Target, collision.Reason, diagnostics.summary())
	rows := diagnostics.rows()
	var approved bool
	if c.projectRecoveryApproval != nil {
		approved, err = c.projectRecoveryApproval(rows, message)
	} else {
		if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(terminalStderrFD()) {
			return "", codedError{Code: "PROJECT_RECOVERY_NONINTERACTIVE", Message: fmt.Sprintf("Local-project recovery requires an interactive explicit confirmation; nothing was changed. %s", diagnostics.summary())}
		}
		// This is intentionally not answerApproval and therefore AutoApprove
		// cannot bypass a local replacement confirmation.
		approved, err = promptApprovalTable(rows, message, false)
	}
	if err != nil {
		return "", codedError{Code: "PROJECT_RECOVERY_PROMPT_FAILED", Message: fmt.Sprintf("Local-project recovery was not performed: %v. %s", err, diagnostics.summary())}
	}
	if !approved {
		return "", codedError{Code: "PROJECT_RECOVERY_DECLINED", Message: fmt.Sprintf("Local-project recovery was declined; nothing was changed. %s", diagnostics.summary())}
	}
	backup, err := moveCanonicalProjectToBackup(collision.Target)
	if err != nil {
		return "", codedError{Code: "PROJECT_RECOVERY_BACKUP_FAILED", Message: fmt.Sprintf("Could not preserve conflicting local project before recovery: %v", err)}
	}
	if err := os.MkdirAll(collision.Target, 0o755); err != nil {
		restored, restoreErr := restoreCanonicalProject(backup, collision.Target)
		return "", codedError{Code: "PROJECT_RECOVERY_RECREATE_FAILED", Message: projectRecoveryRollbackMessage(err, backup, "", restored, restoreErr)}
	}
	// The caller retries normal sync from an empty, unclaimed directory. That
	// path is pull-only on first materialization; no collision contents are ever
	// included in a push.
	return backup, nil
}

type projectRecoveryReport struct {
	localCount, remoteCount          int
	localNewest, remoteNewest        string
	differing, localOnly, remoteOnly []string
}

func projectRecoveryDiagnostics(dir string, entries map[string]gliderChangeEntry, remote map[string][]byte) (projectRecoveryReport, error) {
	report := projectRecoveryReport{remoteCount: len(remote)}
	local, err := localPersistentFiles(dir)
	if err != nil {
		return report, err
	}
	report.localCount = len(local)
	var newest time.Time
	for rel := range local {
		path, err := persistentSafeLocalWritePath(dir, rel)
		if err != nil {
			return report, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return report, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().After(newest) || (info.ModTime().Equal(newest) && rel < report.localNewest) {
			newest, report.localNewest = info.ModTime(), rel
		}
	}
	if !newest.IsZero() {
		report.localNewest = newest.UTC().Format(time.RFC3339) + " " + report.localNewest
	}
	var remoteLatest int64
	var remoteLatestPath string
	for path, content := range remote {
		entry := entries[path]
		if entry.MTime > remoteLatest || (entry.MTime == remoteLatest && (remoteLatestPath == "" || path < remoteLatestPath)) {
			remoteLatest, remoteLatestPath = entry.MTime, path
		}
		if localContent, ok := local[path]; !ok {
			report.remoteOnly = append(report.remoteOnly, path)
		} else if !bytes.Equal(localContent, content) {
			report.differing = append(report.differing, path)
		}
	}
	for path := range local {
		if _, ok := remote[path]; !ok {
			report.localOnly = append(report.localOnly, path)
		}
	}
	if remoteLatestPath != "" {
		if remoteLatest > 0 {
			report.remoteNewest = time.UnixMilli(remoteLatest).UTC().Format(time.RFC3339) + " " + remoteLatestPath
		} else {
			report.remoteNewest = "unavailable (Web timestamps not provided)"
		}
	}
	for _, paths := range [][]string{report.differing, report.localOnly, report.remoteOnly} {
		sort.Strings(paths)
	}
	return report, nil
}

func boundedRecoveryPaths(paths []string) string {
	if len(paths) == 0 {
		return "none"
	}
	if len(paths) > projectRecoveryPathLimit {
		return strings.Join(paths[:projectRecoveryPathLimit], ", ") + fmt.Sprintf(" (+%d more)", len(paths)-projectRecoveryPathLimit)
	}
	return strings.Join(paths, ", ")
}
func (r projectRecoveryReport) summary() string {
	return fmt.Sprintf("Local files: %d (newest: %s); Web files: %d (newest: %s); differing: %s; local-only: %s; Web-only: %s.", r.localCount, firstNonBlank(r.localNewest, "none"), r.remoteCount, firstNonBlank(r.remoteNewest, "none"), boundedRecoveryPaths(r.differing), boundedRecoveryPaths(r.localOnly), boundedRecoveryPaths(r.remoteOnly))
}
func (r projectRecoveryReport) rows() [][2]string {
	return [][2]string{{"Local files", fmt.Sprint(r.localCount)}, {"Web files", fmt.Sprint(r.remoteCount)}, {"Newest local", firstNonBlank(r.localNewest, "none")}, {"Newest Web", firstNonBlank(r.remoteNewest, "none")}, {"Differing", boundedRecoveryPaths(r.differing)}, {"Local-only", boundedRecoveryPaths(r.localOnly)}, {"Web-only", boundedRecoveryPaths(r.remoteOnly)}}
}

func moveCanonicalProjectToBackup(target string) (string, error) {
	for n := 0; ; n++ {
		suffix := time.Now().UTC().Format("20060102T150405.000000000Z")
		if n > 0 {
			suffix += fmt.Sprintf("-%d", n)
		}
		backup := target + ".bacli-backup-" + suffix
		if _, err := os.Lstat(backup); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(target, backup); err != nil {
				return "", err
			}
			return backup, nil
		} else if err != nil {
			return "", err
		}
	}
}
func restoreCanonicalProject(backup, target string) (bool, error) {
	if err := os.Rename(backup, target); err != nil {
		return false, err
	}
	return true, nil
}
func projectRecoveryRollbackMessage(cause error, backup, partial string, restored bool, restoreErr error) string {
	if restored {
		return fmt.Sprintf("Recovery recreation failed (%v); the original local project was restored from %s.", cause, backup)
	}
	return fmt.Sprintf("Recovery recreation failed (%v); backup retained at %s, partial checkout at %s, restore error: %v.", cause, backup, partial, restoreErr)
}

// refreshBuildInstallContextFromProject makes dependency and packaging decisions
// from the locked checkout after sync has pulled remote-only changes.
func refreshBuildInstallContextFromProject(ctxInfo *buildInstallContext, project syncedBuildProject) error {
	nowPath, ok := project.Files["now.config.json"]
	if !ok {
		return errors.New("now.config.json was not found in the active Glider project")
	}
	nowRaw, err := os.ReadFile(nowPath)
	if err != nil {
		return fmt.Errorf("failed to read synced now.config.json: %w", err)
	}
	var nowCfg map[string]interface{}
	if err := json.Unmarshal(nowRaw, &nowCfg); err != nil {
		return fmt.Errorf("failed to parse synced now.config.json: %w", err)
	}
	ctxInfo.NowConfig = nowCfg
	if scope := strings.TrimSpace(stringify(nowCfg["scope"])); scope != "" {
		ctxInfo.Scope = scope
	}
	if scopeID := strings.TrimSpace(stringify(nowCfg["scopeId"])); scopeID != "" {
		ctxInfo.ScopeID = scopeID
	}
	if appName := strings.TrimSpace(stringify(nowCfg["name"])); appName != "" {
		ctxInfo.AppName = appName
	}
	if packagePath, ok := project.Files["package.json"]; ok {
		packageRaw, err := os.ReadFile(packagePath)
		if err != nil {
			return fmt.Errorf("failed to read synced package.json: %w", err)
		}
		ctxInfo.Package = parsePackageJSONInfo(packageRaw)
		if version := strings.TrimSpace(ctxInfo.Package.Version); version != "" {
			ctxInfo.Version = version
		}
	}
	return nil
}

func (c *Client) resolveBuildInstallContext(ctx context.Context, payload map[string]interface{}) (buildInstallContext, error) {
	appID := strings.TrimSpace(firstString(payload, "appId", "appID", "scopeId", "applicationId", "application_id"))
	if appID == "" {
		appID = c.activeAppSysID()
	}
	if appID == "" {
		return buildInstallContext{}, errors.New("Tool requires an application to have been created or selected")
	}
	rootURI := "now-file:/" + strings.TrimLeft(appID, "/")
	contents, err := c.fetchV2SyncFiles(ctx, []string{rootURI + "/now.config.json", rootURI + "/package.json"})
	if err != nil {
		return buildInstallContext{}, fmt.Errorf("failed to read project config from Glider: %w", err)
	}
	nowRaw, ok := gliderFetchedContentForEntry(contents, gliderChangeEntry{URI: rootURI + "/now.config.json", Checksum: "now.config.json"})
	if !ok {
		// sync/files usually keys multipart parts by checksum, so fall back to the only JSON-looking
		// response body when direct URI lookup is unavailable.
		nowRaw = firstJSONFileContent(contents, "scopeId", "scope")
		ok = len(nowRaw) > 0
	}
	if !ok {
		return buildInstallContext{}, errors.New("now.config.json was not found in the active Glider project")
	}
	var nowCfg map[string]interface{}
	if err := json.Unmarshal(nowRaw, &nowCfg); err != nil {
		return buildInstallContext{}, fmt.Errorf("failed to parse now.config.json: %w", err)
	}

	pkgRaw, ok := gliderFetchedContentForEntry(contents, gliderChangeEntry{URI: rootURI + "/package.json", Checksum: "package.json"})
	if !ok {
		pkgRaw = firstJSONFileContent(contents, "dependencies", "devDependencies", "scripts")
	}
	pkg := packageJSONInfo{}
	if len(pkgRaw) > 0 {
		pkg = parsePackageJSONInfo(pkgRaw)
	}

	if err := c.ensureActiveAppMetadata(ctx); err != nil && c.debug {
		c.debugf("warning: could not refresh active app metadata before build/install: %v\n", err)
	}
	scope := strings.TrimSpace(stringify(nowCfg["scope"]))
	if scope == "" {
		scope = c.activeAppScopeName()
	}
	scopeID := strings.TrimSpace(stringify(nowCfg["scopeId"]))
	if scopeID == "" {
		scopeID = appID
	}
	appName := strings.TrimSpace(stringify(nowCfg["name"]))
	if appName == "" && c.currentApp != nil && c.currentApp.ScopeName != c.currentApp.ScopeID {
		appName = c.currentApp.ScopeName
	}
	version := strings.TrimSpace(pkg.Version)
	if version == "" {
		version = "0.0.1"
	}
	return buildInstallContext{AppID: appID, RootURI: rootURI, Scope: scope, ScopeID: scopeID, AppName: appName, Version: version, NowConfig: nowCfg, Package: pkg}, nil
}

func firstJSONFileContent(files map[string][]byte, requiredKeys ...string) []byte {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		content := bytes.TrimSpace(files[key])
		if !bytes.HasPrefix(content, []byte("{")) {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal(content, &m); err != nil {
			continue
		}
		match := false
		for _, required := range requiredKeys {
			if _, ok := m[required]; ok {
				match = true
				break
			}
		}
		if match {
			return files[key]
		}
	}
	return nil
}

func (c *Client) syncGliderBuildProjectToTemp(ctx context.Context, rootURI string) (syncedBuildProject, func(), error) {
	return c.syncGliderBuildProjectToPersistent(ctx, c.activeAppSysID(), rootURI)
}

func (c *Client) syncGliderBuildProjectToPersistent(ctx context.Context, appID, rootURI string) (syncedBuildProject, func(), error) {
	return c.syncGliderBuildProjectToPersistentNamed(ctx, appID, c.persistentAppDisplayName(appID), rootURI)
}

func (c *Client) syncGliderBuildProjectToPersistentNamed(ctx context.Context, appID, appName, rootURI string) (syncedBuildProject, func(), error) {
	// Kept under the historical wrapper name for compatibility, but projects now
	// live persistently under the process launch directory. Internal build tools
	// may run while a turn is processing, so they bypass only the interactive
	// slash-command availability guard while retaining conflict detection.
	result, cleanup, err := c.syncPersistentAppForBuildNamedLocked(ctx, appID, appName, rootURI, persistentSyncAuto)
	if err != nil {
		return syncedBuildProject{}, func() {}, err
	}
	entries, remoteFiles, err := c.fetchPersistentRemoteFiles(ctx, rootURI)
	if err != nil {
		cleanup()
		return syncedBuildProject{}, func() {}, err
	}
	project := syncedBuildProject{Dir: result.LocalDir, RootURI: rootURI, Entries: map[string]gliderChangeEntry{}, Files: map[string]string{}, Original: map[string][]byte{}, PendingPush: append([]string(nil), result.PendingPush...)}
	for rel, entry := range entries {
		if !includeBuildProjectFile(rel) {
			continue
		}
		localPath, err := safeBuildLocalPath(project.Dir, rel)
		if err != nil {
			cleanup()
			return syncedBuildProject{}, func() {}, err
		}
		project.Entries[entry.URI] = entry
		project.Files[rel] = localPath
		project.Original[rel] = append([]byte(nil), remoteFiles[rel]...)
	}
	return project, cleanup, nil
}

func includeBuildProjectFile(rel string) bool {
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, dir := range []string{"node_modules", "dist", "target", ".git", ".jest_cache"} {
		if parts[0] == dir {
			return false
		}
	}
	return true
}

func safeBuildLocalPath(root, rel string) (string, error) {
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" || strings.HasPrefix(rel, "../") || rel == ".." || strings.Contains(rel, "/../") {
		return "", fmt.Errorf("unsafe Glider path %q", rel)
	}
	local := filepath.Join(append([]string{root}, strings.Split(rel, "/")...)...)
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	cleanLocal, err := filepath.Abs(local)
	if err != nil {
		return "", err
	}
	if cleanLocal != cleanRoot && !strings.HasPrefix(cleanLocal, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe Glider path %q", rel)
	}
	return cleanLocal, nil
}

func gliderRelFromRootURI(rootURI, uri string) string {
	root := strings.TrimRight(rootURI, "/") + "/"
	return strings.Trim(strings.TrimPrefix(uri, root), "/")
}

func parsePackageJSONInfo(raw []byte) packageJSONInfo {
	var root map[string]interface{}
	_ = json.Unmarshal(raw, &root)
	return packageJSONInfo{
		Name:                 strings.TrimSpace(stringify(root["name"])),
		Version:              strings.TrimSpace(stringify(root["version"])),
		Dependencies:         stringMap(root["dependencies"]),
		DevDependencies:      stringMap(root["devDependencies"]),
		OptionalDependencies: stringMap(root["optionalDependencies"]),
		Scripts:              stringMap(root["scripts"]),
	}
}

func stringMap(v interface{}) map[string]string {
	out := map[string]string{}
	if m := asMap(v); m != nil {
		for key, value := range m {
			out[key] = strings.TrimSpace(stringify(value))
		}
	}
	return out
}

func buildDependencies(pkg packageJSONInfo) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || name == "eslint" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies, pkg.OptionalDependencies} {
		keys := make([]string, 0, len(deps))
		for key := range deps {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			add(key)
		}
	}
	return out
}

func missingNodeDependencies(projectDir string, deps []string) []string {
	var missing []string
	for _, dep := range deps {
		if _, err := os.Stat(filepath.Join(projectDir, "node_modules", filepath.FromSlash(dep), "package.json")); err == nil {
			continue
		}
		missing = append(missing, dep)
	}
	return missing
}

func (c *Client) installProjectDependencies(ctx context.Context, projectDir string, missing []string) error {
	if _, err := exec.LookPath("node"); err != nil {
		msg := "Node.js executable 'node' was not found in PATH; local Fluent build dependencies cannot be installed."
		c.buildInstallProgress("build: %s", msg)
		return codedError{Code: "NODE_NOT_FOUND", Message: msg}
	}
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		msg := "npm executable was not found in PATH; local Fluent build dependencies cannot be installed."
		c.buildInstallProgress("build: %s", msg)
		return codedError{Code: "NPM_NOT_FOUND", Message: msg}
	}
	c.buildInstallProgress("build: installing %d missing dependency package(s): %s", len(missing), strings.Join(missing, ", "))
	// package-lock.json is derived local dependency state for bacli builds, not
	// application source. Prevent npm from creating/updating it and forcing the
	// next conservative sync preflight into a local-change conflict.
	output, err := runLocalCommand(ctx, projectDir, npmPath, []string{"install", "--no-audit", "--no-fund", "--package-lock=false"})
	if err != nil {
		return codedError{Code: "DEPENDENCY_INSTALL_FAILED", Message: fmt.Sprintf("Dependency installation failed: %s", commandErrorSummary(err, output))}
	}
	c.buildInstallProgress("build: dependency installation completed")
	return nil
}

func (c *Client) runNowSDKBuild(ctx context.Context, projectDir string) error {
	nowSDK := filepath.Join(projectDir, "node_modules", ".bin", nowSDKBinName())
	if _, err := os.Stat(nowSDK); err != nil {
		msg := "app-local now-sdk executable was not found at node_modules/.bin/now-sdk after dependency installation."
		c.buildInstallProgress("build: %s", msg)
		return codedError{Code: "NOW_SDK_NOT_FOUND", Message: msg}
	}
	c.buildInstallProgress("build: running now-sdk build")
	output, err := runLocalCommand(ctx, projectDir, nowSDK, []string{"build"})
	if err != nil {
		return codedError{Code: "NOW_SDK_BUILD_FAILED", Message: fmt.Sprintf("now-sdk build failed: %s", commandErrorSummary(err, output))}
	}
	return nil
}

func (c *Client) runNowSDKPack(ctx context.Context, projectDir string) error {
	nowSDK := filepath.Join(projectDir, "node_modules", ".bin", nowSDKBinName())
	if _, err := os.Stat(nowSDK); err != nil {
		msg := "app-local now-sdk executable was not found at node_modules/.bin/now-sdk after dependency installation."
		c.buildInstallProgress("build: %s", msg)
		return codedError{Code: "NOW_SDK_NOT_FOUND", Message: msg}
	}
	c.buildInstallProgress("build: running now-sdk pack")
	output, err := runLocalCommand(ctx, projectDir, nowSDK, []string{"pack"})
	if err != nil {
		return codedError{Code: "NOW_SDK_PACK_FAILED", Message: fmt.Sprintf("now-sdk pack failed: %s", commandErrorSummary(err, output))}
	}
	return nil
}

func nowSDKBinName() string {
	if runtime.GOOS == "windows" {
		return "now-sdk.cmd"
	}
	return "now-sdk"
}

func runLocalCommand(ctx context.Context, dir, name string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	localBin := filepath.Join(dir, "node_modules", ".bin")
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "FORCE_COLOR=0", "PATH="+localBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return output, ctx.Err()
	}
	return output, err
}

func commandErrorSummary(err error, output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" && err != nil {
		text = err.Error()
	}
	if text == "" {
		text = "unknown error"
	}
	return truncateToolSummary(singleLineLabel(text), 500)
}

func (c *Client) persistGeneratedBuildFiles(ctx context.Context, project syncedBuildProject, ctxInfo buildInstallContext) ([]interface{}, error) {
	generatedRel := generatedDirRel(ctxInfo.NowConfig)
	generatedAbs, err := safeBuildLocalPath(project.Dir, generatedRel)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(generatedAbs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	changed := map[string][]byte{}
	if err := filepath.WalkDir(generatedAbs, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relLocal, err := filepath.Rel(project.Dir, path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relLocal)
		if bytes.Equal(project.Original[rel], content) {
			return nil
		}
		changed[rel] = content
		return nil
	}); err != nil {
		return nil, err
	}
	if len(changed) == 0 {
		return nil, nil
	}

	nowMS := time.Now().UnixMilli()
	var create []gliderChangeEntry
	var update []gliderChangeEntry
	var files []gliderFileBlob
	dirCreatesByURI := map[string]gliderChangeEntry{}
	rels := make([]string, 0, len(changed))
	for rel := range changed {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		content := changed[rel]
		uri := strings.TrimRight(project.RootURI, "/") + "/" + rel
		checksum := sha1Hex(content)
		entry := gliderChangeEntry{Checksum: checksum, CTime: nowMS, MTime: nowMS, Size: len(content), Type: "file", URI: uri}
		for _, dirEntry := range c.buildMissingParentDirs(ctx, project.RootURI, rel) {
			dirCreatesByURI[dirEntry.URI] = dirEntry
		}
		if _, exists := project.Entries[uri]; exists {
			update = append(update, entry)
		} else {
			create = append(create, entry)
		}
		files = append(files, gliderFileBlob{Path: checksum, Checksum: checksum, Content: content})
	}
	for _, dirEntry := range dirCreatesByURI {
		create = append(create, dirEntry)
	}
	sort.Slice(create, func(i, j int) bool { return create[i].URI < create[j].URI })
	sort.Slice(update, func(i, j int) bool { return update[i].URI < update[j].URI })
	if err := c.applyGliderChanges(ctx, create, update, nil, files); err != nil {
		allMatch := true
		for _, rel := range rels {
			uri := strings.TrimRight(project.RootURI, "/") + "/" + rel
			if !c.gliderFileContentMatches(ctx, uri, changed[rel]) {
				allMatch = false
				break
			}
		}
		if !allMatch {
			return nil, err
		}
		if manifestErr := c.refreshPersistentSyncManifest(project.Dir, ctxInfo.AppID, project.RootURI); manifestErr != nil {
			return nil, manifestErr
		}
		return []interface{}{"Glider sync returned an error after persisting generated files; verified remote content matches."}, nil
	}
	if err := c.refreshPersistentSyncManifest(project.Dir, ctxInfo.AppID, project.RootURI); err != nil {
		return nil, err
	}
	return nil, nil
}

func (c *Client) buildMissingParentDirs(ctx context.Context, rootURI, rel string) []gliderChangeEntry {
	rootURI = strings.TrimRight(strings.TrimSpace(rootURI), "/")
	if rootURI == "" {
		return nil
	}
	parentURIs := []string{rootURI}
	dir := posixpath.Dir(rel)
	if dir != "." && dir != "/" && dir != "" {
		current := rootURI
		for _, part := range strings.Split(dir, "/") {
			if part == "" || part == "." {
				continue
			}
			current += "/" + part
			parentURIs = append(parentURIs, current)
		}
	}
	entries, _ := c.fetchGliderStateForURIs(ctx, parentURIs)
	existing := map[string]bool{rootURI: true}
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
		return create
	}
	current := rootURI
	for _, part := range strings.Split(dir, "/") {
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
	return create
}

func generatedDirRel(nowCfg map[string]interface{}) string {
	fluentDir := strings.Trim(strings.ReplaceAll(stringify(nowCfg["fluentDir"]), "\\", "/"), "/")
	if fluentDir == "" {
		fluentDir = "src/fluent"
	}
	generatedDir := strings.Trim(strings.ReplaceAll(stringify(nowCfg["generatedDir"]), "\\", "/"), "/")
	if generatedDir == "" {
		generatedDir = "generated"
	}
	if strings.HasPrefix(generatedDir, fluentDir+"/") || generatedDir == fluentDir {
		return posixpath.Clean(generatedDir)
	}
	return posixpath.Clean(fluentDir + "/" + generatedDir)
}

func findPackageZip(projectDir string, nowCfg map[string]interface{}) (string, error) {
	candidates := []string{}
	if pack := strings.TrimSpace(stringify(nowCfg["packOutputDir"])); pack != "" {
		candidates = append(candidates, pack)
	}
	candidates = append(candidates, "target", "dist")
	seen := map[string]struct{}{}
	type zipCandidate struct {
		Path    string
		ModTime time.Time
	}
	var zips []zipCandidate
	for _, rel := range candidates {
		rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
		if rel == "" {
			continue
		}
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		root := filepath.Join(projectDir, filepath.FromSlash(rel))
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if !strings.EqualFold(filepath.Ext(path), ".zip") {
				return nil
			}
			info, statErr := d.Info()
			if statErr != nil {
				return nil
			}
			zips = append(zips, zipCandidate{Path: path, ModTime: info.ModTime()})
			return nil
		})
	}
	if len(zips) == 0 {
		return "", errors.New("now-sdk pack completed but no package ZIP was found under target/ or configured packOutputDir")
	}
	sort.Slice(zips, func(i, j int) bool { return zips[i].ModTime.After(zips[j].ModTime) })
	return zips[0].Path, nil
}

func checkMetadataSyncState(projectDir string) error {
	state, err := readMetadataSyncState(projectDir)
	if err != nil {
		return err
	}
	if state.Needed {
		return codedError{Code: "METADATA_SYNC_REQUIRED", Message: "Metadata synchronization is still required. Re-run build so the CLI can synchronize instance metadata before packaging."}
	}
	return nil
}

func boolValue(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return parseBoolish(t)
	default:
		return false
	}
}

func (c *Client) queryPriorUpgradeHistory(ctx context.Context, scope string) (string, error) {
	if strings.TrimSpace(scope) == "" {
		return "", nil
	}
	body, err := c.fetchLatestUpgradeHistory(ctx, scope)
	if err != nil {
		return "", err
	}
	return upgradeHistorySysID(body), nil
}

func (c *Client) fetchLatestUpgradeHistory(ctx context.Context, scope string) ([]byte, error) {
	values := url.Values{}
	values.Set("sysparm_query", "to_version="+scope+"^ORDERBYDESCupgrade_started")
	values.Set("sysparm_fields", "upgrade_finished,sys_id")
	values.Set("sysparm_limit", "1")
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/now/table/sys_upgrade_history?" + values.Encode()
	body, _, err := c.getJSON(ctx, endpoint)
	return body, err
}

func upgradeHistorySysID(body []byte) string {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	for _, m := range flattenMaps(root) {
		if id := serviceNowFieldString(m["sys_id"]); id != "" {
			return id
		}
	}
	return ""
}

func upgradeHistoryFinished(body []byte) bool {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return false
	}
	for _, m := range flattenMaps(root) {
		if finished := serviceNowFieldString(m["upgrade_finished"]); finished != "" {
			return true
		}
	}
	return false
}

func (c *Client) queryInstallScopeInfo(ctx context.Context, appID, scope string) (map[string]interface{}, error) {
	values := url.Values{}
	query := "sys_id=" + appID
	if strings.TrimSpace(scope) != "" {
		query = "scope=" + scope + "^" + query
	}
	values.Set("sysparm_query", query)
	values.Set("sysparm_fields", "sys_id,sys_class_name,active,scope,name,short_description")
	values.Set("sysparm_limit", "2")
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/now/table/sys_scope?" + values.Encode()
	body, _, err := c.getJSON(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	for _, m := range flattenMaps(root) {
		if sysID := serviceNowFieldString(m["sys_id"]); sysID != "" && !strings.EqualFold(sysID, appID) {
			continue
		}
		if activeRaw, ok := m["active"]; ok && !boolValue(activeRaw) && !strings.EqualFold(strings.TrimSpace(stringify(activeRaw)), "true") {
			return nil, fmt.Errorf("ServiceNow scope %s is not active", appID)
		}
		if serviceNowFieldString(m["sys_id"]) != "" || serviceNowFieldString(m["scope"]) != "" {
			return m, nil
		}
	}
	return nil, fmt.Errorf("ServiceNow scope %s was not found or is not accessible", appID)
}

func (c *Client) uploadScopedAppPackage(ctx context.Context, zipPath string, ctxInfo buildInstallContext) (string, string, error) {
	ck := strings.TrimSpace(c.userToken)
	if ck == "" {
		ck = c.fetchUserToken(ctx)
		c.userToken = ck
	}
	if ck == "" {
		msg := "ServiceNow sysparm_ck / X-UserToken was not available; upload processor requires a browser web session. Re-authenticate with --auth cookie or --auth form."
		c.buildInstallProgress("install: %s", msg)
		return "", "", codedError{Code: "CSRF_TOKEN_NOT_FOUND", Message: msg}
	}
	file, err := os.Open(zipPath)
	if err != nil {
		return "", "", err
	}
	defer file.Close()

	values := url.Values{}
	values.Set("sysparm_track_fluent_install", "true")
	values.Set("sysparm_async_fluent_install", "false")
	values.Set("sysparm_fluent_scope_id", ctxInfo.AppID)
	values.Set("sysparm_fluent_scope_name", ctxInfo.Scope)
	values.Set("sysparm_fluent_app_version", ctxInfo.Version)
	values.Set("sysparm_request_type", "custom_app")
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + "/sn_appclient_upload_processor.do?" + values.Encode()
	c.buildInstallProgress("install: uploading package to ServiceNow")
	body, _, _, err := c.postBrowserSessionMultipart(ctx, endpoint, func(writer *multipart.Writer) error {
		if err := writer.WriteField("upload_type", "file"); err != nil {
			return err
		}
		if err := writer.WriteField("load_demo", "true"); err != nil {
			return err
		}
		if err := writer.WriteField("sysparm_ck", ck); err != nil {
			return err
		}
		part, err := writer.CreateFormFile("attachFile", "blob")
		if err != nil {
			return err
		}
		_, err = io.Copy(part, file)
		return err
	})
	if err != nil {
		return "", "", codedError{Code: "PACKAGE_UPLOAD_FAILED", Message: err.Error()}
	}
	tracker, rollback := parseUploadScopedAppPackageResponse(body)
	return tracker, rollback, nil
}

func parseUploadScopedAppPackageResponse(body []byte) (tracker, rollback string) {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return "", ""
	}
	for _, m := range flattenMaps(root) {
		if tracker == "" {
			tracker = firstString(m, "executionTracker", "execution_tracker", "tracker", "trackerId")
		}
		if rollback == "" {
			rollback = firstString(m, "rollbackContext", "rollback_context", "rollbackContextId")
		}
	}
	return tracker, rollback
}

func (c *Client) pollInstallProgress(ctx context.Context, tracker string) (installProgressResult, error) {
	if tracker == "" {
		return installProgressResult{}, codedError{Code: "EXECUTION_TRACKER_MISSING", Message: "Cannot poll install progress without an execution tracker"}
	}
	deadline := time.Now().Add(buildInstallPollMax)
	for attempt := 0; ; attempt++ {
		endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/sn_cicd/progress/" + url.PathEscape(tracker)
		body, _, err := c.getJSON(ctx, endpoint)
		if err != nil {
			return installProgressResult{}, codedError{Code: "PROGRESS_POLL_FAILED", Message: err.Error()}
		}
		progress := parseInstallProgressResponse(body)
		if progress.Successful {
			return progress, nil
		}
		if progress.Failed {
			msg := progress.displayMessage()
			if msg == "" {
				msg = "Application install failed"
			}
			return progress, codedError{Code: "INSTALL_FAILED", Message: msg}
		}
		if attempt%30 == 0 {
			c.buildInstallProgress("install: app install pending (%s)", progress.displayMessage())
		}
		if time.Now().After(deadline) {
			return progress, codedError{Code: "INSTALL_TIMEOUT", Message: "Timed out waiting for ServiceNow install progress"}
		}
		select {
		case <-ctx.Done():
			return progress, codedError{Code: "INSTALL_TIMEOUT", Message: ctx.Err().Error()}
		case <-time.After(buildInstallPollInterval):
		}
	}
}

func (c *Client) pollUpgradeHistoryFallback(ctx context.Context, scope, priorID string) (installProgressResult, error) {
	if strings.TrimSpace(scope) == "" {
		return installProgressResult{}, codedError{Code: "EXECUTION_TRACKER_MISSING", Message: "Upload response did not include executionTracker and scope is unavailable for sys_upgrade_history fallback polling"}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		body, err := c.fetchLatestUpgradeHistory(ctx, scope)
		if err != nil {
			return installProgressResult{}, codedError{Code: "UPGRADE_HISTORY_POLL_FAILED", Message: err.Error()}
		}
		currentID := upgradeHistorySysID(body)
		if currentID != "" && !strings.EqualFold(currentID, priorID) && upgradeHistoryFinished(body) {
			return installProgressResult{Status: "success", StatusLabel: "Successful", StatusMessage: "Application installed successfully", Percent: 100, Successful: true}, nil
		}
		if time.Now().After(deadline) {
			return installProgressResult{}, codedError{Code: "INSTALL_TIMEOUT", Message: "Timed out waiting for sys_upgrade_history fallback install completion"}
		}
		select {
		case <-ctx.Done():
			return installProgressResult{}, codedError{Code: "INSTALL_TIMEOUT", Message: ctx.Err().Error()}
		case <-time.After(buildInstallPollInterval):
		}
	}
}

func parseInstallProgressResponse(body []byte) installProgressResult {
	var root interface{}
	_ = json.Unmarshal(body, &root)
	var best map[string]interface{}
	for _, m := range flattenMaps(root) {
		if firstString(m, "status", "status_label", "statusLabel", "status_message", "statusMessage") != "" {
			best = m
		}
	}
	if best == nil {
		return installProgressResult{}
	}
	progress := installProgressResult{
		Status:        firstString(best, "status"),
		StatusLabel:   firstString(best, "status_label", "statusLabel"),
		StatusMessage: firstString(best, "status_message", "statusMessage", "message"),
		StatusDetail:  firstString(best, "status_detail", "statusDetail"),
		Error:         firstString(best, "error"),
		Percent:       int64FromInterface(best["percent_complete"]),
	}
	lower := strings.ToLower(strings.Join([]string{progress.Status, progress.StatusLabel, progress.StatusMessage}, " "))
	progress.Successful = progress.Status == "2" || strings.Contains(lower, "successful") || strings.Contains(lower, "success")
	progress.Failed = progress.Error != "" || strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "failure")
	return progress
}

func (p installProgressResult) displayMessage() string {
	for _, value := range []string{p.StatusMessage, p.StatusLabel, p.StatusDetail, p.Error} {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if p.Percent > 0 {
		return fmt.Sprintf("%d%% complete", p.Percent)
	}
	return "pending"
}

func (p installProgressResult) toMap() map[string]interface{} {
	return map[string]interface{}{
		"status":          p.Status,
		"statusLabel":     p.StatusLabel,
		"statusMessage":   p.StatusMessage,
		"statusDetail":    p.StatusDetail,
		"error":           p.Error,
		"percentComplete": p.Percent,
	}
}

func (c *Client) discoverInstalledArtifactLinks(ctx context.Context, appID string) []string {
	var links []string
	base := strings.TrimRight(c.cfg.InstanceURL, "/")
	for _, table := range []string{"sys_ui_page", "sys_db_object"} {
		endpoint := fmt.Sprintf("%s/api/sn_build_agent/build_agent_api/runQuery/table/%s/query/sys_scope=%s", base, table, url.PathEscape(appID))
		body, _, err := c.getJSON(ctx, endpoint)
		if err != nil {
			c.buildInstallProgress("install: warning: artifact query for %s failed: %v", table, err)
			continue
		}
		links = append(links, artifactLinksFromRunQuery(base, table, body)...)
	}
	return uniqueStrings(links)
}

func artifactLinksFromRunQuery(base, table string, body []byte) []string {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	var links []string
	for _, m := range flattenMaps(root) {
		switch table {
		case "sys_db_object":
			name := serviceNowFieldString(m["name"])
			if name == "" || !looksLikeServiceNowScope(name) && !strings.Contains(name, "_") {
				continue
			}
			links = append(links, fmt.Sprintf("%s/%s_list.do?sysparm_clear_stack=true", base, url.PathEscape(name)))
		case "sys_ui_page":
			endpoint := serviceNowFieldString(m["endpoint"])
			if endpoint == "" {
				endpoint = serviceNowFieldString(m["name"])
			}
			endpoint = strings.TrimLeft(endpoint, "/")
			if endpoint == "" {
				continue
			}
			links = append(links, base+"/"+endpoint)
		}
	}
	return links
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func (c *Client) replaceLastGliderBuild(next *gliderBuildState) {
	if c.lastGliderBuild != nil && !c.lastGliderBuild.Persistent && c.lastGliderBuild.TempDir != "" && (next == nil || c.lastGliderBuild.TempDir != next.TempDir) {
		_ = os.RemoveAll(c.lastGliderBuild.TempDir)
	}
	c.lastGliderBuild = next
}

func (c *Client) invalidateLastGliderBuild() { c.replaceLastGliderBuild(nil) }

func (b buildInstallContext) displayName() string {
	for _, value := range []string{b.AppName, b.Scope, b.AppID} {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "app"
}

func payloadPathOrDot(payload map[string]interface{}) string {
	path := payloadPath(payload)
	if path == "" {
		return "."
	}
	return path
}

type codedError struct {
	Code    string
	Message string
}

func (e codedError) Error() string { return e.Message }

func buildInstallErrorCode(err error) string {
	var coded codedError
	if errors.As(err, &coded) && coded.Code != "" {
		return coded.Code
	}
	return "BUILD_INSTALL_FAILED"
}

func buildInstallError(message, code string, details map[string]interface{}) map[string]interface{} {
	if code == "" {
		code = "BUILD_INSTALL_FAILED"
	}
	out := map[string]interface{}{"content": message, "error": message, "code": code}
	for key, value := range details {
		out[key] = value
	}
	return out
}

func buildInstallSuccess(message string, extra map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{"success": true, "content": message, "message": message}
	for key, value := range extra {
		out[key] = value
	}
	return out
}
