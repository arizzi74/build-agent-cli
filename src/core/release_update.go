package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var (
	cliVersion       = "dev"
	cliUpdateBaseURL = "https://nowdemo.it/bacli"
)

const releaseManifestSchemaVersion = 1

type releaseManifest struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Version       string                 `json:"version"`
	Files         map[string]releaseFile `json:"files"`
}

type releaseFile struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

func releaseTarget(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "linux/arm64":
		return "linux-arm64"
	case "linux/amd64":
		return "linux-amd64"
	case "darwin/amd64":
		return "darwin-amd64"
	case "darwin/arm64":
		return "darwin-arm64"
	case "windows/amd64":
		return "windows-amd64"
	case "windows/arm64":
		return "windows-arm64"
	default:
		return ""
	}
}

func checkForSelfUpdate() (bool, string) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("BACLI_NO_UPDATE")), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("BACLI_NO_UPDATE")), "true") {
		return false, ""
	}
	current := strings.TrimSpace(cliVersion)
	if current == "" || strings.EqualFold(current, "dev") {
		return false, ""
	}
	target := releaseTarget(runtime.GOOS, runtime.GOARCH)
	if target == "" {
		return false, ""
	}
	base := strings.TrimRight(strings.TrimSpace(cliUpdateBaseURL), "/")
	if override := strings.TrimSpace(os.Getenv("BACLI_UPDATE_BASE_URL")); override != "" {
		base = strings.TrimRight(override, "/")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	updated, version, err := selfUpdateFromManifest(ctx, http.DefaultClient, base, current, target)
	if err != nil || !updated {
		return false, ""
	}
	return true, fmt.Sprintf("bacli updated to %s. Relaunch bacli to use the new version.", version)
}

func selfUpdateFromManifest(ctx context.Context, client *http.Client, baseURL, currentVersion, target string) (bool, string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	manifestURL := strings.TrimRight(baseURL, "/") + "/version.json"
	raw, err := downloadReleaseFile(ctx, client, manifestURL, 1<<20)
	if err != nil {
		return false, "", err
	}
	var manifest releaseManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false, "", fmt.Errorf("invalid release manifest: %w", err)
	}
	if manifest.SchemaVersion != releaseManifestSchemaVersion || strings.TrimSpace(manifest.Version) == "" {
		return false, "", errors.New("unsupported release manifest")
	}
	if compareReleaseVersions(manifest.Version, currentVersion) <= 0 {
		return false, manifest.Version, nil
	}
	artifact, ok := manifest.Files[target]
	if !ok || strings.TrimSpace(artifact.URL) == "" || !validSHA256(artifact.SHA256) {
		return false, "", fmt.Errorf("release manifest has no valid artifact for %s", target)
	}
	artifactURL, err := resolveReleaseURL(manifestURL, artifact.URL)
	if err != nil {
		return false, "", err
	}
	binary, err := downloadReleaseFile(ctx, client, artifactURL, 128<<20)
	if err != nil {
		return false, "", err
	}
	if !strings.EqualFold(sha256Hex(binary), strings.TrimSpace(artifact.SHA256)) {
		return false, "", errors.New("downloaded update checksum mismatch")
	}
	executable, err := os.Executable()
	if err != nil {
		return false, "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return false, "", err
	}
	if err := installSelfUpdate(executable, binary); err != nil {
		return false, "", err
	}
	return true, manifest.Version, nil
}

func installSelfUpdate(executable string, binary []byte) error {
	dir := filepath.Dir(executable)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".bacli-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(binary); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Rename(tmpPath, executable); err != nil {
			return err
		}
		cleanup = false
		return nil
	}
	staged := executable + ".new"
	_ = os.Remove(staged)
	if err := os.Rename(tmpPath, staged); err != nil {
		return err
	}
	cleanup = false
	script := executable + ".update.cmd"
	scriptBody := "@echo off\r\n:retry\r\nmove /Y \"" + staged + "\" \"" + executable + "\" >nul 2>&1\r\nif errorlevel 1 (\r\n  timeout /t 1 /nobreak >nul\r\n  goto retry\r\n)\r\ndel \"%~f0\"\r\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o600); err != nil {
		_ = os.Remove(staged)
		return err
	}
	cmd := exec.Command("cmd.exe", "/C", "start", "", "/B", script)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(script)
		_ = os.Remove(staged)
		return err
	}
	return nil
}

func downloadReleaseFile(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release download returned HTTP %d", res.StatusCode)
	}
	limited := io.LimitReader(res.Body, maxBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, errors.New("release download exceeded size limit")
	}
	return raw, nil
}

func resolveReleaseURL(manifestURL, artifactURL string) (string, error) {
	base, err := url.Parse(manifestURL)
	if err != nil {
		return "", err
	}
	rel, err := url.Parse(strings.TrimSpace(artifactURL))
	if err != nil {
		return "", err
	}
	resolved := base.ResolveReference(rel)
	if resolved.Scheme != "https" && resolved.Scheme != "http" {
		return "", errors.New("unsupported release URL scheme")
	}
	return resolved.String(), nil
}

func compareReleaseVersions(a, b string) int {
	ap, apre := parseReleaseVersion(a)
	bp, bpre := parseReleaseVersion(b)
	width := len(ap)
	if len(bp) > width {
		width = len(bp)
	}
	for i := 0; i < width; i++ {
		av, bv := 0, 0
		if i < len(ap) {
			av = ap[i]
		}
		if i < len(bp) {
			bv = bp[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	if apre == bpre {
		return 0
	}
	if apre == "" {
		return 1
	}
	if bpre == "" {
		return -1
	}
	return strings.Compare(apre, bpre)
}

func parseReleaseVersion(raw string) ([]int, string) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "v")
	core := raw
	pre := ""
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		pre = core[i+1:]
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	out := make([]int, len(parts))
	for i := range parts {
		out[i], _ = strconv.Atoi(parts[i])
	}
	return out, pre
}

func validSHA256(raw string) bool {
	raw = strings.TrimSpace(raw)
	if len(raw) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(raw)
	return err == nil
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
