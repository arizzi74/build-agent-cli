package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCompareReleaseVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.2", 1},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.3-beta", "1.2.3", -1},
		{"2.0.0", "10.0.0", -1},
		{"2026.07.13.2", "2026.07.13.1", 1},
	} {
		got := compareReleaseVersions(tc.a, tc.b)
		if got < 0 {
			got = -1
		} else if got > 0 {
			got = 1
		}
		if got != tc.want {
			t.Fatalf("compareReleaseVersions(%q,%q)=%d want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestReleaseTarget(t *testing.T) {
	cases := map[string]string{
		"linux/arm64":   "linux-arm64",
		"linux/amd64":   "linux-amd64",
		"darwin/amd64":  "darwin-amd64",
		"darwin/arm64":  "darwin-arm64",
		"windows/amd64": "windows-amd64",
		"linux/386":     "",
	}
	for raw, want := range cases {
		var goos, goarch string
		for i, r := range raw {
			if r == '/' {
				goos, goarch = raw[:i], raw[i+1:]
				break
			}
		}
		if got := releaseTarget(goos, goarch); got != want {
			t.Fatalf("releaseTarget(%s)=%q want %q", raw, got, want)
		}
	}
}

func TestSelfUpdateFromManifestSkipsCurrentVersion(t *testing.T) {
	manifest := releaseManifest{SchemaVersion: 1, Version: "1.2.3", Files: map[string]releaseFile{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(manifest)
	}))
	defer server.Close()
	updated, version, err := selfUpdateFromManifest(context.Background(), server.Client(), server.URL, "1.2.3", "linux-arm64")
	if err != nil || updated || version != "1.2.3" {
		t.Fatalf("updated=%v version=%q err=%v", updated, version, err)
	}
}

func TestSelfUpdateRejectsChecksumMismatchBeforeInstall(t *testing.T) {
	binary := []byte("new binary")
	manifest := releaseManifest{SchemaVersion: 1, Version: "1.2.4", Files: map[string]releaseFile{
		"linux-arm64": {URL: "bacli-linux-arm64", SHA256: string(make([]byte, 64))},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version.json" {
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		_, _ = w.Write(binary)
	}))
	defer server.Close()
	if _, _, err := selfUpdateFromManifest(context.Background(), server.Client(), server.URL, "1.2.3", "linux-arm64"); err == nil {
		t.Fatal("expected checksum mismatch")
	}
}

func TestInstallSelfUpdateAtomicallyReplacesUnixExecutable(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix replacement test")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bacli")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installSelfUpdate(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "new" {
		t.Fatalf("content=%q", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("replacement is not executable: %v", info.Mode())
	}
}

func TestReleaseManifestChecksumFormat(t *testing.T) {
	sum := sha256.Sum256([]byte("artifact"))
	if !validSHA256(hex.EncodeToString(sum[:])) || validSHA256("nope") {
		t.Fatal("sha256 validation mismatch")
	}
}
