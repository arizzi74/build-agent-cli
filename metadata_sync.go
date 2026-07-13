package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const metadataSyncAppDataRel = ".now/.app-data.json"

type metadataSyncState struct {
	Needed   bool
	Status   string
	LastSync int64
	Raw      map[string]json.RawMessage
	Original []byte
}

type metadataSyncTransformResult struct {
	ChangedFiles  []string `json:"changedFiles"`
	HandledPaths  []string `json:"handledPaths"`
	CompletedAtMS int64    `json:"completedAtMs"`
}

type projectGliderDiff struct {
	Create map[string][]byte
	Update map[string][]byte
	Remove []string
}

var metadataSyncRunCommand = runMetadataSyncNodeCommand

func readMetadataSyncState(projectDir string) (metadataSyncState, error) {
	path := filepath.Join(projectDir, filepath.FromSlash(metadataSyncAppDataRel))
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return metadataSyncState{}, nil
		}
		return metadataSyncState{}, err
	}
	data := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return metadataSyncState{}, fmt.Errorf("parse %s: %w", metadataSyncAppDataRel, err)
	}
	var syncNeeded bool
	_ = json.Unmarshal(data["syncNeeded"], &syncNeeded)
	var syncStatus string
	_ = json.Unmarshal(data["syncStatus"], &syncStatus)
	var lastSync int64
	if len(data["lastSync"]) > 0 {
		if err := json.Unmarshal(data["lastSync"], &lastSync); err != nil {
			var lastSyncFloat float64
			if json.Unmarshal(data["lastSync"], &lastSyncFloat) == nil {
				lastSync = int64(lastSyncFloat)
			}
		}
	}
	return metadataSyncState{
		Needed:   syncNeeded && !strings.EqualFold(strings.TrimSpace(syncStatus), "InProgress"),
		Status:   strings.TrimSpace(syncStatus),
		LastSync: lastSync,
		Raw:      data,
		Original: append([]byte(nil), raw...),
	}, nil
}

func metadataSyncLastPull(lastSync int64) (string, error) {
	if lastSync <= 0 {
		return "", codedError{Code: "METADATA_SYNC_TIMESTAMP_MISSING", Message: "Metadata sync is required but .now/.app-data.json has no valid lastSync timestamp."}
	}
	return time.UnixMilli(lastSync).UTC().Format("2006-01-02 15:04:05"), nil
}

func updateMetadataSyncCompleted(projectDir string, completedAt time.Time) error {
	state, err := readMetadataSyncState(projectDir)
	if err != nil {
		return err
	}
	if state.Raw == nil {
		state.Raw = map[string]json.RawMessage{}
	}
	state.Raw["syncNeeded"] = json.RawMessage("false")
	state.Raw["syncStatus"] = json.RawMessage(`"completed"`)
	state.Raw["lastSync"] = json.RawMessage(fmt.Sprintf("%d", completedAt.UnixMilli()))
	raw, err := json.Marshal(state.Raw)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	path := filepath.Join(projectDir, filepath.FromSlash(metadataSyncAppDataRel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".app-data-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (c *Client) runMetadataIncrementalTransform(ctx context.Context, projectDir, lastPull string) (metadataSyncTransformResult, error) {
	token := strings.TrimSpace(c.nirvanaRESTAccessToken())
	var authErr error
	oauthCfg := oauthConfig(c.cfg)
	if oauthCfg.ClientID == "" {
		oauthCfg.ClientID = defaultOAuthClientID
	}
	if oauthCfg.RedirectURI == "" {
		oauthCfg.RedirectURI = defaultOAuthRedirectURI
	}
	if tok, err := getAccessToken(ctx, oauthCfg, c.opts.Profile, c.cfg.InstanceURL, c.opts.NoOpen, true); err == nil && tok.AccessToken != "" {
		token = tok.AccessToken
		c.oauthAccessToken = token
	} else if err != nil && token == "" {
		authErr = err
	}
	if token == "" || authErr != nil {
		message := "Metadata sync requires a valid refreshable OAuth profile. Reconnect with OAuth, then rebuild."
		if authErr != nil {
			message += " " + authErr.Error()
		}
		return metadataSyncTransformResult{}, codedError{Code: "METADATA_SYNC_AUTH_REQUIRED", Message: message}
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		return metadataSyncTransformResult{}, codedError{Code: "NODE_NOT_FOUND", Message: "Node.js is required for ServiceNow metadata synchronization."}
	}
	helperPath := filepath.Join(projectDir, ".now", ".ba-metadata-sync.cjs")
	if err := os.MkdirAll(filepath.Dir(helperPath), 0o755); err != nil {
		return metadataSyncTransformResult{}, err
	}
	if err := os.WriteFile(helperPath, []byte(metadataSyncNodeHelper), 0o600); err != nil {
		return metadataSyncTransformResult{}, err
	}
	defer os.Remove(helperPath)
	request := map[string]string{
		"instanceURL": strings.TrimRight(c.cfg.InstanceURL, "/"),
		"projectDir":  projectDir,
		"lastPull":    lastPull,
		"token":       token,
	}
	stdin, err := json.Marshal(request)
	if err != nil {
		return metadataSyncTransformResult{}, err
	}
	output, err := metadataSyncRunCommand(ctx, projectDir, nodePath, helperPath, stdin)
	for i := range stdin {
		stdin[i] = 0
	}
	if ctx.Err() != nil {
		return metadataSyncTransformResult{}, ctx.Err()
	}
	if err != nil {
		return metadataSyncTransformResult{}, codedError{Code: "METADATA_SYNC_FAILED", Message: fmt.Sprintf("ServiceNow metadata sync failed: %s", commandErrorSummary(err, redactMetadataSyncOutput(output, token)))}
	}
	var result metadataSyncTransformResult
	if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
		return metadataSyncTransformResult{}, codedError{Code: "METADATA_SYNC_FAILED", Message: fmt.Sprintf("ServiceNow metadata sync returned invalid output: %s", truncateToolSummary(string(redactMetadataSyncOutput(output, token)), 500))}
	}
	return result, nil
}

func runMetadataSyncNodeCommand(ctx context.Context, projectDir, nodePath, helperPath string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, nodePath, helperPath)
	cmd.Dir = projectDir
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	localBin := filepath.Join(projectDir, "node_modules", ".bin")
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "FORCE_COLOR=0", "PATH="+localBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := cmd.Run()
	if err != nil && stderr.Len() > 0 {
		return append(append([]byte(nil), stdout.Bytes()...), stderr.Bytes()...), err
	}
	return stdout.Bytes(), err
}

func redactMetadataSyncOutput(output []byte, secrets ...string) []byte {
	text := string(output)
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	return []byte(text)
}

func diffProjectForGlider(project syncedBuildProject) (projectGliderDiff, error) {
	diff := projectGliderDiff{Create: map[string][]byte{}, Update: map[string][]byte{}}
	current := map[string][]byte{}
	err := filepath.WalkDir(project.Dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			rel, err := filepath.Rel(project.Dir, path)
			if err != nil {
				return err
			}
			if path != project.Dir && !includeBuildProjectFile(filepath.ToSlash(rel)+"/placeholder") {
				return filepath.SkipDir
			}
			return nil
		}
		relLocal, err := filepath.Rel(project.Dir, path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relLocal)
		if !includeBuildProjectFile(rel) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		current[rel] = content
		original, existed := project.Original[rel]
		if !existed {
			diff.Create[rel] = content
		} else if !bytes.Equal(original, content) {
			diff.Update[rel] = content
		}
		return nil
	})
	if err != nil {
		return projectGliderDiff{}, err
	}
	for rel := range project.Original {
		if !includeBuildProjectFile(rel) {
			continue
		}
		if _, exists := current[rel]; !exists {
			diff.Remove = append(diff.Remove, rel)
		}
	}
	sort.Strings(diff.Remove)
	return diff, nil
}

func (c *Client) persistProjectGliderDiff(ctx context.Context, project syncedBuildProject, diff projectGliderDiff) error {
	if err := c.verifyMetadataSyncBaseline(ctx, project, diff); err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	create := []gliderChangeEntry{}
	update := []gliderChangeEntry{}
	remove := []gliderChangeEntry{}
	files := []gliderFileBlob{}
	dirs := map[string]gliderChangeEntry{}
	appendFiles := func(changes map[string][]byte, target *[]gliderChangeEntry) {
		rels := make([]string, 0, len(changes))
		for rel := range changes {
			rels = append(rels, rel)
		}
		sort.Strings(rels)
		for _, rel := range rels {
			content := changes[rel]
			uri := strings.TrimRight(project.RootURI, "/") + "/" + rel
			checksum := sha1Hex(content)
			*target = append(*target, gliderChangeEntry{Checksum: checksum, CTime: nowMS, MTime: nowMS, Size: len(content), Type: "file", URI: uri})
			files = append(files, gliderFileBlob{Path: checksum, Checksum: checksum, Content: content})
			for _, dir := range c.buildMissingParentDirs(ctx, project.RootURI, rel) {
				dirs[dir.URI] = dir
			}
		}
	}
	appendFiles(diff.Create, &create)
	appendFiles(diff.Update, &update)
	for _, rel := range diff.Remove {
		uri := strings.TrimRight(project.RootURI, "/") + "/" + rel
		entry := project.Entries[uri]
		if entry.URI == "" {
			entry = gliderChangeEntry{Type: "file", URI: uri}
		}
		entry.DTime = nowMS
		remove = append(remove, entry)
	}
	for _, dir := range dirs {
		create = append(create, dir)
	}
	sort.Slice(create, func(i, j int) bool { return create[i].URI < create[j].URI })
	sort.Slice(update, func(i, j int) bool { return update[i].URI < update[j].URI })
	sort.Slice(remove, func(i, j int) bool { return remove[i].URI < remove[j].URI })
	if len(create) == 0 && len(update) == 0 && len(remove) == 0 {
		return nil
	}
	if err := c.applyGliderChanges(ctx, create, update, remove, files); err != nil {
		return err
	}
	return c.verifyMetadataSyncWriteback(ctx, project, diff)
}

func (c *Client) verifyMetadataSyncBaseline(ctx context.Context, project syncedBuildProject, diff projectGliderDiff) error {
	var uris []string
	for rel := range diff.Update {
		uris = append(uris, strings.TrimRight(project.RootURI, "/")+"/"+rel)
	}
	for _, rel := range diff.Remove {
		uris = append(uris, strings.TrimRight(project.RootURI, "/")+"/"+rel)
	}
	if len(uris) == 0 {
		return nil
	}
	remote, err := c.fetchGliderStateForURIs(ctx, uris)
	if err != nil {
		return err
	}
	byURI := map[string]gliderChangeEntry{}
	for _, entry := range remote {
		byURI[entry.URI] = entry
	}
	for _, uri := range uris {
		original, ok := project.Entries[uri]
		if !ok {
			continue
		}
		current, exists := byURI[uri]
		if !exists {
			contents, fetchErr := c.fetchV2SyncFiles(ctx, []string{uri})
			if fetchErr != nil {
				return fetchErr
			}
			rel := gliderRelFromRootURI(project.RootURI, uri)
			content, found := onlySyncFileContent(contents)
			if !found || !bytes.Equal(content, project.Original[rel]) {
				return codedError{Code: "METADATA_SYNC_CONFLICT", Message: fmt.Sprintf("Glider file %s disappeared or changed while metadata synchronization was running; rebuild and retry.", rel)}
			}
			continue
		}
		// Some Glider state responses compute/normalize checksums differently
		// when querying a root versus exact file URIs. Confirm the actual file
		// bytes before declaring a conflict.
		if original.Checksum != "" && current.Checksum != original.Checksum {
			contents, fetchErr := c.fetchV2SyncFiles(ctx, []string{uri})
			if fetchErr != nil {
				return fetchErr
			}
			content, found := gliderFetchedContentForEntry(contents, current)
			rel := gliderRelFromRootURI(project.RootURI, uri)
			if !found || !bytes.Equal(content, project.Original[rel]) {
				return codedError{Code: "METADATA_SYNC_CONFLICT", Message: fmt.Sprintf("Glider file %s changed while metadata synchronization was running (snapshot=%s current=%s bytes=%t); rebuild and retry.", rel, shortChecksum(original.Checksum), shortChecksum(current.Checksum), found && bytes.Equal(content, project.Original[rel]))}
			}
		}
	}
	return nil
}

func shortChecksum(value string) string {
	if len(value) > 10 {
		return value[:10]
	}
	return value
}

func (c *Client) verifyMetadataSyncWriteback(ctx context.Context, project syncedBuildProject, diff projectGliderDiff) error {
	var uris []string
	expected := map[string]string{}
	for rel, content := range diff.Create {
		uri := strings.TrimRight(project.RootURI, "/") + "/" + rel
		uris = append(uris, uri)
		expected[uri] = sha1Hex(content)
	}
	for rel, content := range diff.Update {
		uri := strings.TrimRight(project.RootURI, "/") + "/" + rel
		uris = append(uris, uri)
		expected[uri] = sha1Hex(content)
	}
	for _, rel := range diff.Remove {
		uris = append(uris, strings.TrimRight(project.RootURI, "/")+"/"+rel)
	}
	if len(uris) == 0 {
		return nil
	}
	remote, err := c.fetchGliderStateForURIs(ctx, uris)
	if err != nil {
		return err
	}
	byURI := map[string]gliderChangeEntry{}
	for _, entry := range remote {
		byURI[entry.URI] = entry
	}
	for uri, checksum := range expected {
		if entry, exists := byURI[uri]; !exists || entry.Checksum != checksum {
			contents, fetchErr := c.fetchV2SyncFiles(ctx, []string{uri})
			if fetchErr != nil {
				return fetchErr
			}
			content, found := onlySyncFileContent(contents)
			if !found || sha1Hex(content) != checksum {
				return codedError{Code: "METADATA_SYNC_WRITEBACK_FAILED", Message: "Glider did not confirm all synchronized metadata files; install is blocked."}
			}
		}
	}
	for _, rel := range diff.Remove {
		uri := strings.TrimRight(project.RootURI, "/") + "/" + rel
		if _, exists := byURI[uri]; exists {
			return codedError{Code: "METADATA_SYNC_WRITEBACK_FAILED", Message: "Glider did not confirm synchronized metadata deletions; install is blocked."}
		}
	}
	return nil
}

func onlySyncFileContent(contents map[string][]byte) ([]byte, bool) {
	if len(contents) != 1 {
		return nil, false
	}
	for _, content := range contents {
		return content, true
	}
	return nil, false
}

func (c *Client) syncMetadataBeforeBuild(ctx context.Context, project syncedBuildProject) (metadataSyncTransformResult, error) {
	c.metadataSyncMu.Lock()
	defer c.metadataSyncMu.Unlock()
	state, err := readMetadataSyncState(project.Dir)
	if err != nil || !state.Needed {
		return metadataSyncTransformResult{}, err
	}
	lastPull, err := metadataSyncLastPull(state.LastSync)
	if err != nil {
		return metadataSyncTransformResult{}, err
	}
	c.buildInstallProgress("build: synchronizing ServiceNow metadata changed since %s UTC", lastPull)
	result, err := c.runMetadataIncrementalTransform(ctx, project.Dir, lastPull)
	if err != nil {
		return metadataSyncTransformResult{}, err
	}
	completedAt := time.UnixMilli(result.CompletedAtMS)
	if result.CompletedAtMS <= 0 {
		completedAt = time.Now()
	}
	if err := updateMetadataSyncCompleted(project.Dir, completedAt); err != nil {
		return metadataSyncTransformResult{}, err
	}
	diff, err := diffProjectForGlider(project)
	if err != nil {
		return metadataSyncTransformResult{}, err
	}
	if err := c.persistProjectGliderDiff(ctx, project, diff); err != nil {
		return metadataSyncTransformResult{}, codedError{Code: "METADATA_SYNC_WRITEBACK_FAILED", Message: fmt.Sprintf("Metadata sync succeeded locally but Glider writeback failed: %v", err)}
	}
	for rel, content := range diff.Create {
		project.Original[rel] = append([]byte(nil), content...)
		uri := strings.TrimRight(project.RootURI, "/") + "/" + rel
		project.Entries[uri] = gliderChangeEntry{Checksum: sha1Hex(content), CTime: time.Now().UnixMilli(), MTime: time.Now().UnixMilli(), Size: len(content), Type: "file", URI: uri}
	}
	for rel, content := range diff.Update {
		project.Original[rel] = append([]byte(nil), content...)
		uri := strings.TrimRight(project.RootURI, "/") + "/" + rel
		entry := project.Entries[uri]
		entry.Checksum, entry.MTime, entry.Size, entry.Type, entry.URI = sha1Hex(content), time.Now().UnixMilli(), len(content), "file", uri
		project.Entries[uri] = entry
	}
	for _, rel := range diff.Remove {
		delete(project.Original, rel)
		delete(project.Entries, strings.TrimRight(project.RootURI, "/")+"/"+rel)
	}
	c.buildInstallProgress("build: metadata synchronized to Glider (%d changed file(s))", len(result.ChangedFiles))
	return result, nil
}

const metadataSyncNodeHelper = `
const fs = require('fs');
const path = require('path');
async function readStdin() { const chunks=[]; for await (const c of process.stdin) chunks.push(c); return Buffer.concat(chunks).toString('utf8'); }
(async () => {
  const request = JSON.parse(await readStdin());
  const sdk = require(path.join(request.projectDir, 'node_modules', '@servicenow', 'sdk-api'));
  const core = require(path.join(request.projectDir, 'node_modules', '@servicenow', 'sdk-build-core'));
  const logger = { trace(){}, debug(){}, info(){}, warn(){}, error(){} };
  const config = core.NowConfig.parseFromDirectory(request.projectDir, fs, logger);
  const project = new sdk.Project({ config, fileSystem: fs, rootDir: request.projectDir, logger });
  const credential = new sdk.LazyCredential(new URL(request.instanceURL), () => ({ type: 'oauth', token: request.token }));
  const orchestrator = new sdk.Orchestrator(project, credential);
  const syncStartedAt = Date.now();
  const result = await orchestrator.transform({ method: 'incremental', lastPull: request.lastPull, format: true });
  const changedFiles = (result.changedFiles || []).map((f) => typeof f.getPath === 'function' ? f.getPath() : String(f));
  process.stdout.write(JSON.stringify({ changedFiles, handledPaths: result.handledPaths || [], completedAtMs: syncStartedAt }));
})().catch((error) => { process.stderr.write(String(error && (error.stack || error.message) || error)); process.exit(1); });
`
