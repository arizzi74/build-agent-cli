package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func projectTestClient(t *testing.T, instance string) *Client {
	t.Helper()
	return &Client{
		cfg:        CLIConfig{InstanceURL: instance},
		opts:       Options{Profile: "project-test"},
		currentApp: &AppScope{AppSysID: "app", ScopeID: "app", ScopeName: "Bocota"},
	}
}

func writeProjectTestManifest(t *testing.T, c *Client, dir, appID string) persistentSyncManifest {
	t.Helper()
	if err := ensurePersistentProjectRoot(dir); err != nil {
		t.Fatal(err)
	}
	if err := savePersistentSyncManifest(dir, persistentSyncManifest{InstanceURL: c.cfg.InstanceURL, AppID: appID, RootURI: "now-file:/" + appID, Files: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	manifest, ok, err := loadPersistentSyncManifest(dir)
	if err != nil || !ok {
		t.Fatalf("manifest = %#v, %v, %v", manifest, ok, err)
	}
	return manifest
}

func projectCommandOutput(t *testing.T, c *Client, line string) (string, error) {
	t.Helper()
	var out strings.Builder
	_, err := withSlashCommandOutput(&out, func() (bool, error) { return handleSlashCommand(context.Background(), c, line) })
	return out.String(), err
}

func TestProjectCommandsUseListCurrentPrimaryAndForget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	c := projectTestClient(t, "https://example.test")
	one := filepath.Join(wd, "one")
	two := filepath.Join(wd, "two")
	oneManifest := writeProjectTestManifest(t, c, one, "app")
	twoManifest := writeProjectTestManifest(t, c, two, "app")
	if _, err := c.registerPersistentProject(one, "app", "now-file:/app", oneManifest.CheckoutID, true); err != nil {
		t.Fatal(err)
	}
	if out, err := projectCommandOutput(t, c, "/projects use two"); err != nil || !strings.Contains(out, "current process only") || c.localProjectOverride != two {
		t.Fatalf("use out=%q err=%v override=%q", out, err, c.localProjectOverride)
	}
	out, err := projectCommandOutput(t, c, "/project list")
	if err != nil || !strings.Contains(out, oneManifest.CheckoutID+"  "+one+"  [primary]") || !strings.Contains(out, twoManifest.CheckoutID+"  "+two+"  [current-process]") {
		t.Fatalf("list out=%q err=%v", out, err)
	}
	out, err = projectCommandOutput(t, c, "/project current")
	if err != nil || !strings.Contains(out, "current: "+two) || !strings.Contains(out, "source: current-process") {
		t.Fatalf("current out=%q err=%v", out, err)
	}
	if out, err = projectCommandOutput(t, c, "/project primary "+twoManifest.CheckoutID); err != nil || !strings.Contains(out, "primary checkout "+twoManifest.CheckoutID) {
		t.Fatalf("primary out=%q err=%v", out, err)
	}
	if out, err = projectCommandOutput(t, c, "/project forget "+twoManifest.CheckoutID); err != nil || !strings.Contains(out, "promoted "+oneManifest.CheckoutID) || c.localProjectOverride != "" {
		t.Fatalf("forget out=%q err=%v override=%q", out, err, c.localProjectOverride)
	}
	checkouts, err := c.listPersistentProjectCheckouts("app", "now-file:/app")
	if err != nil || len(checkouts) != 1 || checkouts[0].ID != oneManifest.CheckoutID {
		t.Fatalf("remaining=%#v err=%v", checkouts, err)
	}
}

func TestProjectUseRegistersValidPathAndRejectsMismatchedManifest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	c := projectTestClient(t, "https://example.test")
	valid := filepath.Join(wd, "valid")
	manifest := writeProjectTestManifest(t, c, valid, "app")
	if _, err := projectCommandOutput(t, c, "/project use valid"); err != nil || c.localProjectOverride != valid {
		t.Fatalf("valid use err=%v override=%q", err, c.localProjectOverride)
	}
	checkouts, err := c.listPersistentProjectCheckouts("app", "now-file:/app")
	if err != nil || len(checkouts) != 1 || checkouts[0].ID != manifest.CheckoutID {
		t.Fatalf("checkouts=%#v err=%v", checkouts, err)
	}
	if primary, err := c.projectPrimaryCheckoutID("app"); err != nil || primary != manifest.CheckoutID {
		t.Fatalf("first explicit use primary=%q err=%v, want %q", primary, err, manifest.CheckoutID)
	}
	wrong := filepath.Join(wd, "wrong")
	writeProjectTestManifest(t, c, wrong, "other")
	if _, err := projectCommandOutput(t, c, "/project use wrong"); err == nil || !strings.Contains(err.Error(), "matching") {
		t.Fatalf("mismatched manifest err=%v", err)
	}
}

func newProjectCloneServer(t *testing.T, applyCalls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sn_glider/v2/sync/state":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": []map[string]interface{}{{"uri": "now-file:/app/src/a.ts", "checksum": sha1Hex([]byte("remote")), "type": "file", "size": 6}}})
		case "/api/sn_glider/v2/sync/files":
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": []map[string]string{{"checksum": sha1Hex([]byte("remote")), "content": "remote"}}})
		case "/api/sn_glider/v2/sync/changes/apply":
			*applyCalls++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestProjectCloneHereDefaultCustomAndSafety(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(wd)
	applyCalls := 0
	server := newProjectCloneServer(t, &applyCalls)
	defer server.Close()
	c := projectTestClient(t, server.URL)
	c.httpClient = server.Client()
	out, err := projectCommandOutput(t, c, "/project clone-here")
	defaultDir := filepath.Join(wd, "Bocota")
	if err != nil || !strings.Contains(out, "cloned checkout") || c.localProjectOverride != defaultDir {
		t.Fatalf("default clone out=%q err=%v override=%q", out, err, c.localProjectOverride)
	}
	defaultManifest, ok, err := loadPersistentSyncManifest(defaultDir)
	if err != nil || !ok || defaultManifest.Version != 2 {
		t.Fatalf("default manifest=%#v ok=%v err=%v", defaultManifest, ok, err)
	}
	if content, err := os.ReadFile(filepath.Join(defaultDir, "src", "a.ts")); err != nil || string(content) != "remote" {
		t.Fatalf("cloned content=%q err=%v", content, err)
	}
	if applyCalls != 0 {
		t.Fatalf("clone made upload requests: %d", applyCalls)
	}
	if primary, err := c.projectPrimaryCheckoutID("app"); err != nil || primary != defaultManifest.CheckoutID {
		t.Fatalf("first explicit clone should become primary: primary=%q err=%v want=%q", primary, err, defaultManifest.CheckoutID)
	}
	if _, err := projectCommandOutput(t, c, "/project clone-here ../escape"); err == nil {
		t.Fatal("path traversal clone was accepted")
	}
	if _, err := projectCommandOutput(t, c, "/project clone-here second"); err != nil {
		t.Fatal(err)
	}
	secondManifest, ok, err := loadPersistentSyncManifest(filepath.Join(wd, "second"))
	if err != nil || !ok || secondManifest.CheckoutID == defaultManifest.CheckoutID {
		t.Fatalf("independent clone ids default=%q second=%q ok=%v err=%v", defaultManifest.CheckoutID, secondManifest.CheckoutID, ok, err)
	}
	if primary, err := c.projectPrimaryCheckoutID("app"); err != nil || primary != defaultManifest.CheckoutID {
		t.Fatalf("later clone changed primary: primary=%q err=%v want=%q", primary, err, defaultManifest.CheckoutID)
	}
	if _, err := projectCommandOutput(t, c, "/project clone-here second"); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("collision err=%v", err)
	}
}

func TestProjectUseOverrideWinsOverManifestedWorkingDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	c := projectTestClient(t, "https://example.test")
	one := filepath.Join(wd, "one")
	two := filepath.Join(wd, "two")
	oneManifest := writeProjectTestManifest(t, c, one, "app")
	twoManifest := writeProjectTestManifest(t, c, two, "app")
	if _, err := c.registerPersistentProject(one, "app", "now-file:/app", oneManifest.CheckoutID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.registerPersistentProject(two, "app", "now-file:/app", twoManifest.CheckoutID, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(one); err != nil {
		t.Fatal(err)
	}
	if _, err := projectCommandOutput(t, c, "/project use "+twoManifest.CheckoutID); err != nil {
		t.Fatal(err)
	}
	out, err := projectCommandOutput(t, c, "/project current")
	if err != nil || !strings.Contains(out, "current: "+two) || !strings.Contains(out, "source: current-process") {
		t.Fatalf("current out=%q err=%v", out, err)
	}
}

func TestProjectCloneDestinationRejectsSanitizedOrReservedName(t *testing.T) {
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CON", "name.", "a:b", "../escape"} {
		if _, err := projectCloneDestination("App", "app", name); err == nil {
			t.Fatalf("unsafe clone name %q accepted", name)
		}
	}
	if got, err := projectCloneDestination("App", "app", "safe-name"); err != nil || got != filepath.Join(wd, "safe-name") {
		t.Fatalf("safe destination = %q, %v", got, err)
	}
}

func TestProjectRegistrationPinsLegacyManifestToInstanceWithoutChangingBaseline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := projectTestClient(t, "HTTPS://Example.Test/")
	dir := t.TempDir()
	legacy := persistentSyncManifest{
		Version: 1,
		AppID:   "app",
		RootURI: "now-file:/app",
		Files:   map[string]string{"src/a.ts": "0123456789012345678901234567890123456789"},
	}
	if err := writePersistentSyncManifest(dir, legacy); err != nil {
		t.Fatal(err)
	}
	checkout, err := c.registerPersistentProjectSecondary(dir, "app", "now-file:/app", "")
	if err != nil {
		t.Fatal(err)
	}
	manifest, ok, err := loadPersistentSyncManifest(dir)
	if err != nil || !ok {
		t.Fatalf("manifest ok=%v err=%v", ok, err)
	}
	if manifest.Version != 2 || manifest.InstanceURL != "https://example.test" || manifest.CheckoutID != checkout.ID {
		t.Fatalf("pinned manifest = %#v", manifest)
	}
	if manifest.ChecksumAlgorithm != "sha1" || manifest.Files["src/a.ts"] != legacy.Files["src/a.ts"] {
		t.Fatalf("legacy baseline changed: %#v", manifest)
	}
}
