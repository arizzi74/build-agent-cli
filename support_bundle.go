package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// SupportBundleSchemaVersion is the stable schema for the local-only support archive.
const SupportBundleSchemaVersion = 1

const (
	supportBundleEventLimit = 20
	supportBundleMaxBytes   = 1 << 20
)

var supportBundlePathComponentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type supportBundleMember struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type supportBundleManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Format        string                `json:"format"`
	GeneratedAt   string                `json:"generatedAt"`
	Build         supportBundleBuild    `json:"build"`
	Profile       string                `json:"profile"`
	InstanceHost  string                `json:"instanceHost"`
	Transport     string                `json:"transport"`
	Members       []supportBundleMember `json:"members"`
	Safety        string                `json:"safety"`
}

type supportBundleBuild struct {
	CLIVersion string `json:"cliVersion"`
	BuildSHA   string `json:"buildSha,omitempty"`
}

type supportBundleResult struct {
	SchemaVersion int    `json:"schemaVersion"`
	Format        string `json:"format"`
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	MemberCount   int    `json:"memberCount"`
}

// handleSupportBundleCommand creates a strictly local, allowlisted diagnostic archive.
func handleSupportBundleCommand(_ context.Context, c *Client, args []string) (bool, error) {
	jsonOutput := false
	pathArg := ""
	for _, arg := range args {
		switch arg {
		case "--json":
			if jsonOutput {
				return true, fmt.Errorf("usage: /support-bundle [path] [--json]")
			}
			jsonOutput = true
		default:
			if pathArg != "" {
				return true, fmt.Errorf("usage: /support-bundle [path] [--json]")
			}
			pathArg = arg
		}
	}
	result, err := c.createSupportBundle(pathArg)
	if err != nil {
		return true, err
	}
	if jsonOutput {
		raw, err := json.Marshal(result)
		if err != nil {
			return true, err
		}
		slashCommandPrintln(string(raw))
		return true, nil
	}
	slashCommandPrintf("support bundle: %s (%d bytes, %d members)\n", result.Path, result.Bytes, result.MemberCount)
	return true, nil
}

func (c *Client) createSupportBundle(pathArg string) (supportBundleResult, error) {
	now := statusNow().UTC()
	payloads, manifest, err := c.supportBundlePayloads(now)
	if err != nil {
		return supportBundleResult{}, err
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		return supportBundleResult{}, err
	}
	payloads["manifest.json"] = manifestRaw
	path, profileLocal, err := supportBundleDestination(c.opts.Profile, pathArg, now)
	if err != nil {
		return supportBundleResult{}, err
	}
	if profileLocal {
		if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
			return supportBundleResult{}, err
		}
	}
	if err := rejectUnsafeBundleDestination(path); err != nil {
		return supportBundleResult{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".support-bundle-*")
	if err != nil {
		return supportBundleResult{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return supportBundleResult{}, err
	}
	if err := writeDeterministicSupportZIP(tmp, payloads, now); err != nil {
		tmp.Close()
		return supportBundleResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return supportBundleResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return supportBundleResult{}, err
	}
	// Link is same-filesystem and atomically fails if destination exists. Unlike
	// Rename on Unix it cannot replace a concurrent creator's archive.
	if err := os.Link(tmpName, path); err != nil {
		if os.IsExist(err) {
			return supportBundleResult{}, fmt.Errorf("support bundle destination already exists")
		}
		return supportBundleResult{}, err
	}
	if err := os.Remove(tmpName); err != nil {
		return supportBundleResult{}, err
	}
	if err := syncSupportBundleDir(filepath.Dir(path)); err != nil {
		return supportBundleResult{}, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return supportBundleResult{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return supportBundleResult{}, err
	}
	return supportBundleResult{SchemaVersion: SupportBundleSchemaVersion, Format: "zip", Path: supportBundleDisplayPath(path, profileLocal), Bytes: info.Size(), MemberCount: len(payloads)}, nil
}

func (c *Client) supportBundlePayloads(now time.Time) (map[string][]byte, supportBundleManifest, error) {
	status := c.statusDocument()
	turn := c.turnDocument()
	entries, journal := readTurnJournal(c.opts.Profile, c.workspaceName, c.semanticJournalSequence)
	events := make([]debugEvent, 0, supportBundleEventLimit)
	if journal.Health == "healthy" {
		start := len(entries) - supportBundleEventLimit
		if start < 0 {
			start = 0
		}
		for _, entry := range entries[start:] {
			events = append(events, redactedDebugEvent(entry))
		}
	}
	inventory := struct {
		SchemaVersion int            `json:"schemaVersion"`
		Servers       []statusServer `json:"servers"`
		Generation    string         `json:"generation,omitempty"`
		Hash          string         `json:"hash,omitempty"`
		WorkingSet    string         `json:"workingSetHash,omitempty"`
	}{SupportBundleSchemaVersion, status.Servers.Inventory, status.Servers.Generation, status.Servers.Hash, status.WorkingSet.Hash}
	config := struct {
		SchemaVersion int    `json:"schemaVersion"`
		Profile       string `json:"profile"`
		Transport     string `json:"transport"`
		AuthMode      string `json:"authMode"`
		Nirvana       bool   `json:"nirvana"`
		CodeAssistWS  bool   `json:"codeAssistWebSocket"`
	}{SupportBundleSchemaVersion, status.Profile.Name, status.Transport.Mode, status.Transport.AuthMode, c.opts.Nirvana, c.opts.CodeAssistWS}
	environment := struct {
		SchemaVersion int    `json:"schemaVersion"`
		GoVersion     string `json:"goVersion"`
		GOOS          string `json:"goos"`
		GOARCH        string `json:"goarch"`
	}{SupportBundleSchemaVersion, runtime.Version(), runtime.GOOS, runtime.GOARCH}
	journalSummary := struct {
		SchemaVersion int             `json:"schemaVersion"`
		Health        turnJournalInfo `json:"health"`
		Included      int             `json:"includedEvents"`
		Limit         int             `json:"eventLimit"`
	}{SupportBundleSchemaVersion, journal, len(events), supportBundleEventLimit}
	values := map[string]interface{}{
		"config.json":      config,
		"environment.json": environment,
		"events.json":      events,
		"inventory.json":   inventory,
		"journal.json":     journalSummary,
		"status.json":      status,
		"turn.json":        turn,
	}
	payloads := make(map[string][]byte, len(values)+1)
	members := make([]supportBundleMember, 0, len(values))
	for name, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, supportBundleManifest{}, err
		}
		if err := supportBundleSafeBytes(name, raw); err != nil {
			return nil, supportBundleManifest{}, err
		}
		payloads[name] = raw
		sum := sha256.Sum256(raw)
		members = append(members, supportBundleMember{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(raw))})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	if totalPayloadBytes(payloads) > supportBundleMaxBytes {
		return nil, supportBundleManifest{}, fmt.Errorf("support bundle exceeds local size limit")
	}
	manifest := supportBundleManifest{SchemaVersion: SupportBundleSchemaVersion, Format: "zip", GeneratedAt: now.Format(time.RFC3339Nano), Build: supportBundleBuild{CLIVersion: turnRuntimeCLIVersion, BuildSHA: safeSnapshotString(turnRuntimeBuildSHA)}, Profile: safeStatusString(status.Profile.Name), InstanceHost: safeStatusString(status.Profile.InstanceHost), Transport: safeStatusString(status.Transport.Mode), Members: members, Safety: "allowlisted_redacted_local_only"}
	return payloads, manifest, nil
}

func supportBundleDestination(profile, arg string, now time.Time) (string, bool, error) {
	if arg == "" {
		base := filepath.Join(profileDir(profile), "support")
		name := "support-" + now.Format("20060102T150405.000000000Z") + ".zip"
		for i := 0; ; i++ {
			candidate := filepath.Join(base, name)
			if i > 0 {
				candidate = filepath.Join(base, strings.TrimSuffix(name, ".zip")+fmt.Sprintf("-%d.zip", i))
			}
			if _, err := os.Lstat(candidate); os.IsNotExist(err) {
				return candidate, true, nil
			} else if err != nil {
				return "", false, err
			}
		}
	}
	if filepath.IsAbs(arg) || strings.Contains(arg, "\x00") {
		return "", false, fmt.Errorf("support bundle path must be a safe relative .zip path")
	}
	clean := filepath.Clean(arg)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || filepath.Ext(clean) != ".zip" {
		return "", false, fmt.Errorf("support bundle path must be a safe relative .zip path")
	}
	for _, component := range strings.Split(filepath.ToSlash(clean), "/") {
		if component == "." || !supportBundlePathComponentRE.MatchString(component) || !supportBundlePathComponentSafe(component) {
			return "", false, fmt.Errorf("support bundle path must be a safe relative .zip path")
		}
	}
	return filepath.Join(".", clean), false, nil
}

func rejectUnsafeBundleDestination(path string) error {
	parent := filepath.Clean(filepath.Dir(path))
	abs, err := filepath.Abs(parent)
	if err != nil {
		return fmt.Errorf("support bundle parent directory is unsafe")
	}
	volume := filepath.VolumeName(abs)
	current := volume + string(os.PathSeparator)
	for _, component := range strings.Split(strings.TrimPrefix(abs, current), string(os.PathSeparator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("support bundle parent directory is unsafe")
		}
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o600 == 0) {
		return fmt.Errorf("support bundle destination is unsafe")
	}
	return nil
}

func supportBundlePathComponentSafe(component string) bool {
	lower := strings.ToLower(component)
	for _, word := range []string{"token", "bearer", "cookie", "password", "secret", "authorization", "auth", "g_ck", "session"} {
		if strings.Contains(lower, word) {
			return false
		}
	}
	return true
}

func syncSupportBundleDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func supportBundleDisplayPath(path string, profileLocal bool) string {
	if profileLocal {
		return filepath.ToSlash(filepath.Join("support", filepath.Base(path)))
	}
	return filepath.ToSlash(path)
}

func writeDeterministicSupportZIP(w io.Writer, payloads map[string][]byte, when time.Time) error {
	names := make([]string, 0, len(payloads))
	for name := range payloads {
		if name != "manifest.json" && !strings.HasSuffix(name, ".json") {
			return fmt.Errorf("unsupported support bundle member")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if totalPayloadBytes(payloads) > supportBundleMaxBytes {
		return fmt.Errorf("support bundle exceeds local size limit")
	}
	zw := zip.NewWriter(w)
	for _, name := range names {
		raw := payloads[name]
		if err := supportBundleSafeBytes(name, raw); err != nil {
			zw.Close()
			return err
		}
		h := &zip.FileHeader{Name: name, Method: zip.Store, Modified: when.UTC()}
		h.SetMode(0o600)
		entry, err := zw.CreateHeader(h)
		if err != nil {
			zw.Close()
			return err
		}
		if _, err := entry.Write(raw); err != nil {
			zw.Close()
			return err
		}
	}
	return zw.Close()
}

func totalPayloadBytes(payloads map[string][]byte) int {
	total := 0
	for _, raw := range payloads {
		total += len(raw)
	}
	return total
}

// This is a final defensive canary check. Inputs are already built from safe
// diagnostic documents; this rejects accidental future additions that contain
// credentials, raw prompts, or path-like private data.
func supportBundleSafeBytes(name string, raw []byte) error {
	if name == "" || strings.Contains(name, "/") || !strings.HasSuffix(name, ".json") {
		return fmt.Errorf("unsafe support bundle member")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("unsafe support bundle member")
	}
	if err := validateSupportBundleValue(value, ""); err != nil {
		return err
	}
	lower := strings.ToLower(string(raw))
	for _, marker := range []string{"oauth-token", "session.json", "full prompt", "assistant content", "raw frame", "<html"} {
		if strings.Contains(lower, marker) {
			return fmt.Errorf("unsafe diagnostic material omitted")
		}
	}
	return nil
}

func validateSupportBundleValue(value any, key string) error {
	switch typed := value.(type) {
	case map[string]any:
		for nestedKey, nested := range typed {
			lower := strings.ToLower(nestedKey)
			if supportBundleUnsafeKey(lower) {
				return fmt.Errorf("unsafe diagnostic material omitted")
			}
			if err := validateSupportBundleValue(nested, lower); err != nil {
				return err
			}
		}
	case []any:
		for _, nested := range typed {
			if err := validateSupportBundleValue(nested, key); err != nil {
				return err
			}
		}
	case string:
		if snapshotCredentialPattern.MatchString(typed) || credentialValuePattern.MatchString(typed) {
			return fmt.Errorf("unsafe diagnostic material omitted")
		}
	}
	return nil
}

func supportBundleUnsafeKey(key string) bool {
	if key == "authmode" || key == "authcapabilities" || key == "inputtokens" || key == "outputtokens" || key == "thinkingtokens" {
		return false
	}
	return unsafeJournalKey(key)
}
