package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBuildDependenciesCombinesAndIgnoresESLint(t *testing.T) {
	pkg := packageJSONInfo{
		Dependencies: map[string]string{
			"alpha":  "1.0.0",
			"eslint": "latest",
		},
		DevDependencies: map[string]string{
			"@servicenow/sdk": "4.8.1",
			"alpha":           "2.0.0",
		},
		OptionalDependencies: map[string]string{
			"zulu": "1.0.0",
		},
	}
	got := buildDependencies(pkg)
	want := []string{"alpha", "@servicenow/sdk", "zulu"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildDependencies() = %#v, want %#v", got, want)
	}
}

func TestMissingNodeDependenciesHandlesScopedPackages(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "node_modules", "@servicenow", "sdk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "node_modules", "@servicenow", "sdk", "package.json"), []byte(`{"name":"@servicenow/sdk"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := missingNodeDependencies(tmp, []string{"@servicenow/sdk", "@servicenow/glide"})
	want := []string{"@servicenow/glide"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missingNodeDependencies() = %#v, want %#v", got, want)
	}
}

func TestGeneratedDirRelDefaultsAndCustomPaths(t *testing.T) {
	if got := generatedDirRel(map[string]interface{}{}); got != "src/fluent/generated" {
		t.Fatalf("default generated dir = %q", got)
	}
	cfg := map[string]interface{}{"fluentDir": "fluent", "generatedDir": "fluent/gen"}
	if got := generatedDirRel(cfg); got != "fluent/gen" {
		t.Fatalf("custom generated dir = %q", got)
	}
}

func TestParseUploadScopedAppPackageResponse(t *testing.T) {
	tracker, rollback := parseUploadScopedAppPackageResponse([]byte(`{"result":{"executionTracker":"track1","rollbackContext":"roll1"}}`))
	if tracker != "track1" || rollback != "roll1" {
		t.Fatalf("tracker=%q rollback=%q", tracker, rollback)
	}
}

func TestParseInstallProgressResponseSuccessAndFailure(t *testing.T) {
	success := parseInstallProgressResponse([]byte(`{"result":{"status":"2","status_label":"Successful","status_message":"Application installed successfully","percent_complete":100}}`))
	if !success.Successful || success.Failed || success.displayMessage() != "Application installed successfully" {
		t.Fatalf("unexpected success parse: %#v", success)
	}
	failure := parseInstallProgressResponse([]byte(`{"result":{"status_label":"Failed","status_message":"Boom","error":"bad"}}`))
	if !failure.Failed || failure.Successful {
		t.Fatalf("unexpected failure parse: %#v", failure)
	}
}

func TestUpgradeHistoryHelpers(t *testing.T) {
	body := []byte(`{"result":[{"sys_id":"hist1","upgrade_finished":"2026-07-09 09:21:27"}]}`)
	if got := upgradeHistorySysID(body); got != "hist1" {
		t.Fatalf("upgradeHistorySysID() = %q", got)
	}
	if !upgradeHistoryFinished(body) {
		t.Fatalf("upgradeHistoryFinished() = false, want true")
	}
}

func TestFindPackageZipUsesConfiguredPackOutputDir(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "custom"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldZip := filepath.Join(tmp, "target", "old.zip")
	newZip := filepath.Join(tmp, "custom", "new.zip")
	if err := os.WriteFile(oldZip, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newZip, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour)
	newTime := time.Now()
	_ = os.Chtimes(oldZip, oldTime, oldTime)
	_ = os.Chtimes(newZip, newTime, newTime)
	got, err := findPackageZip(tmp, map[string]interface{}{"packOutputDir": "custom"})
	if err != nil {
		t.Fatal(err)
	}
	if got != newZip {
		t.Fatalf("findPackageZip() = %q, want %q", got, newZip)
	}
}

func TestRunNowSDKPackUsesAppLocalSDK(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake now-sdk is Unix-only")
	}
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeSDK := filepath.Join(binDir, nowSDKBinName())
	script := `#!/bin/sh
set -eu
if [ "${1:-}" != "pack" ]; then
  echo "unexpected command: ${1:-}" >&2
  exit 12
fi
mkdir -p target
printf 'zip' > target/app.zip
`
	if err := os.WriteFile(fakeSDK, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&Client{}).runNowSDKPack(context.Background(), tmp); err != nil {
		t.Fatalf("runNowSDKPack() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "target", "app.zip")); err != nil {
		t.Fatalf("expected fake pack to create target/app.zip: %v", err)
	}
}

func TestArtifactLinksFromRunQuery(t *testing.T) {
	body := []byte(`{"result":[{"name":"x_snc_lima_cars","label":"Cars"}]}`)
	got := artifactLinksFromRunQuery("https://example.service-now.com", "sys_db_object", body)
	want := []string{"https://example.service-now.com/x_snc_lima_cars_list.do?sysparm_clear_stack=true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("artifactLinksFromRunQuery() = %#v, want %#v", got, want)
	}
}

func TestBuildPathSafetyAndSkipsGeneratedHeavyDirs(t *testing.T) {
	for _, rel := range []string{"node_modules/@servicenow/sdk/package.json", "dist/app/file.xml", "target/app.zip", ".git/config"} {
		if includeBuildProjectFile(rel) {
			t.Fatalf("includeBuildProjectFile(%q) = true, want false", rel)
		}
	}
	if !includeBuildProjectFile("src/fluent/table.now.ts") {
		t.Fatalf("expected source file to be included")
	}
	if _, err := safeBuildLocalPath(t.TempDir(), "../escape"); err == nil {
		t.Fatalf("expected traversal path to fail")
	}
}

func TestPrepareBuildProjectBlocksPendingLocalChangesBeforeNPMOrUpload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	oldWD, _ := os.Getwd()
	defer os.Chdir(oldWD)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{
		"now.config.json": []byte(`{"scope":"x_demo"}`),
		"package.json":    []byte(`{"name":"demo","version":"1.0.0","dependencies":{"missing":"1.0.0"}}`),
		"src/a.ts":        []byte("remote"),
	}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)

	_, project, cleanup, err := c.prepareBuildProject(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if err := os.WriteFile(project.Files["src/a.ts"], []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	npmMarker := filepath.Join(bin, "npm-ran")
	if err := os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\ntouch '"+npmMarker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	result, status := c.answerBuildParity(context.Background(), nil)
	if status != "error" || result["code"] != "SYNC_PUSH_REQUIRED" || !strings.Contains(result["error"].(string), "/sync push") {
		t.Fatalf("result=%#v status=%q", result, status)
	}
	if _, err := os.Stat(npmMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("npm ran despite pending local edit: %v", err)
	}
	if got := string(remote.files["src/a.ts"]); got != "remote" {
		t.Fatalf("remote file was uploaded: %q", got)
	}
}

func TestPrepareBuildProjectPullsRemoteOnlyChangesAndUsesSyncedSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	oldWD, _ := os.Getwd()
	defer os.Chdir(oldWD)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{
		"now.config.json": []byte(`{"scope":"x_demo"}`),
		"package.json":    []byte(`{"name":"demo","version":"1.0.0"}`),
		"src/a.ts":        []byte("one"),
	}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	_, project, cleanup, err := c.prepareBuildProject(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	remote.files["src/a.ts"] = []byte("two")
	remote.files["package.json"] = []byte(`{"name":"demo","version":"2.0.0","dependencies":{"remote-only":"1.0.0"}}`)

	ctxInfo, project, cleanup, err := c.prepareBuildProject(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got := string(mustReadBuildFile(t, project.Files["src/a.ts"])); got != "two" {
		t.Fatalf("local remote-only update = %q", got)
	}
	if ctxInfo.Version != "2.0.0" || ctxInfo.Package.Dependencies["remote-only"] != "1.0.0" {
		t.Fatalf("context was not refreshed from synced checkout: %#v", ctxInfo)
	}
}

func TestPrepareBuildProjectFreshCheckoutAndAlwaysReleasesLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	oldWD, _ := os.Getwd()
	defer os.Chdir(oldWD)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{
		"now.config.json": []byte(`{"scope":"x_demo"}`),
		"package.json":    []byte(`{"name":"demo"}`),
	}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	_, project, cleanup, err := c.prepareBuildProject(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(project.Files["now.config.json"]); err != nil {
		t.Fatalf("fresh checkout missing now.config.json: %v", err)
	}
	cleanup()
	assertBuildProjectLockAvailable(t, project.Dir)

	delete(remote.files, "now.config.json")
	_, _, _, err = c.prepareBuildProject(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "now.config.json") {
		t.Fatalf("err=%v, want missing config", err)
	}
	assertBuildProjectLockAvailable(t, project.Dir)
}

func TestPrepareBuildProjectRecoversApprovedCanonicalCollisionWithoutUpload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	remote := &syncTestRemote{files: map[string][]byte{
		"now.config.json": []byte(`{"scope":"x_demo"}`), "package.json": []byte(`{"name":"demo"}`), "src/web.ts": []byte("web"),
	}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	target := filepath.Join(home, "BA", "Bocota")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "legacy.ts"), []byte("local-only"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	c.projectRecoveryApproval = func(rows [][2]string, message string) (bool, error) {
		called = true
		if !strings.Contains(message, "local-only") || !strings.Contains(message, "Web files") {
			t.Fatalf("diagnostics missing: %s", message)
		}
		return true, nil
	}
	_, project, cleanup, err := c.prepareBuildProject(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !called {
		t.Fatal("recovery approval was not requested")
	}
	if got := string(mustReadBuildFile(t, project.Files["src/web.ts"])); got != "web" {
		t.Fatalf("rehydrated=%q", got)
	}
	if _, err := os.Stat(filepath.Join(target, "legacy.ts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy remained in new checkout: %v", err)
	}
	matches, err := filepath.Glob(target + ".bacli-backup-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("backup=%v err=%v", matches, err)
	}
	if got := string(mustReadBuildFile(t, filepath.Join(matches[0], "legacy.ts"))); got != "local-only" {
		t.Fatalf("backup=%q", got)
	}
	if _, ok := remote.files["legacy.ts"]; ok {
		t.Fatal("collision content was uploaded")
	}
	manifest, ok, err := loadPersistentSyncManifest(target)
	if err != nil || !ok || manifest.Version != 2 || manifest.AppID != "app" || normalizedProjectInstance(manifest.InstanceURL) != normalizedProjectInstance(server.URL) {
		t.Fatalf("recreated manifest=%+v ok=%v err=%v", manifest, ok, err)
	}
	if _, registered := c.registeredPersistentProjectCheckout(target, "app"); !registered {
		t.Fatal("recreated checkout was not registered")
	}
}

func TestPrepareBuildProjectDeclinedCanonicalRecoveryDoesNotMutate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	remote := &syncTestRemote{files: map[string][]byte{"now.config.json": []byte(`{"scope":"x_demo"}`), "package.json": []byte(`{"name":"demo"}`)}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	target := filepath.Join(home, "BA", "Bocota")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep.ts"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.projectRecoveryApproval = func([][2]string, string) (bool, error) { return false, nil }
	_, _, _, err := c.prepareBuildProject(context.Background(), nil)
	if err == nil || buildInstallErrorCode(err) != "PROJECT_RECOVERY_DECLINED" {
		t.Fatalf("err=%v", err)
	}
	if got := string(mustReadBuildFile(t, filepath.Join(target, "keep.ts"))); got != "keep" {
		t.Fatalf("target changed=%q", got)
	}
	if matches, _ := filepath.Glob(target + ".bacli-backup-*"); len(matches) != 0 {
		t.Fatalf("unexpected backups=%v", matches)
	}
}

func TestRecoverCanonicalBuildProjectAutoApproveCannotBypassNoninteractiveConfirmation(t *testing.T) {
	target := filepath.Join(t.TempDir(), "Earth")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep.ts"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{"now.config.json": []byte(`{"scope":"x_demo"}`)}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	c.opts.AutoApprove = true
	_, err := c.recoverCanonicalBuildProject(context.Background(), &canonicalProjectCollisionError{Target: target, Reason: "invalid sync manifest", Recoverable: true}, buildInstallContext{RootURI: "now-file:/app"})
	if err == nil || buildInstallErrorCode(err) != "PROJECT_RECOVERY_NONINTERACTIVE" {
		t.Fatalf("err=%v", err)
	}
	if got := string(mustReadBuildFile(t, filepath.Join(target, "keep.ts"))); got != "keep" {
		t.Fatalf("target changed=%q", got)
	}
}

func TestPrepareBuildProjectRecoveryRetryFailureRestoresOriginalAndNeverUploads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	remote := &syncTestRemote{files: map[string][]byte{
		"now.config.json": []byte(`{"scope":"x_demo"}`),
		"package.json":    []byte(`{"name":"demo"}`),
		"src/web.ts":      []byte("web"),
	}}
	base := newSyncTestServer(t, remote)
	defer base.Close()
	c := testSyncClient(base)
	baseTransport := c.httpClient.Transport
	stateCalls := 0
	c.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/sn_glider/v2/sync/state" {
			stateCalls++
			if stateCalls >= 2 {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("offline")), Request: req}, nil
			}
		}
		return baseTransport.RoundTrip(req)
	})}
	target := filepath.Join(home, "BA", "Bocota")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "legacy.ts"), []byte("local-only"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.projectRecoveryApproval = func([][2]string, string) (bool, error) { return true, nil }
	_, _, _, err := c.prepareBuildProject(context.Background(), nil)
	if err == nil || buildInstallErrorCode(err) != "PROJECT_RECOVERY_RECREATE_FAILED" {
		t.Fatalf("err=%v", err)
	}
	if got := string(mustReadBuildFile(t, filepath.Join(target, "legacy.ts"))); got != "local-only" {
		t.Fatalf("original was not restored: %q", got)
	}
	if _, ok := remote.files["legacy.ts"]; ok {
		t.Fatal("collision content was uploaded")
	}
	if backups, _ := filepath.Glob(target + ".bacli-backup-*"); len(backups) != 0 {
		t.Fatalf("restored backup should no longer remain separately: %v", backups)
	}
	if partials, _ := filepath.Glob(target + ".bacli-recovery-failed-*"); len(partials) != 1 {
		t.Fatalf("partial checkout evidence=%v", partials)
	}
}

func TestProjectRecoveryDiagnosticsNewestLocalExcludesMetadataAndBuildOutputs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".ba-cli-sync"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for path, content := range map[string]string{"src/source.ts": "source", ".ba-cli-sync/manifest.json": "metadata", "node_modules/pkg/index.js": "generated"} {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if path == "src/source.ts" {
			if err := os.Chtimes(full, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	report, err := projectRecoveryDiagnostics(dir, map[string]gliderChangeEntry{}, map[string][]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if report.localCount != 1 || !strings.HasSuffix(report.localNewest, " src/source.ts") {
		t.Fatalf("report=%+v", report)
	}
}

func TestProjectRecoveryDiagnosticsDoesNotReportUnixEpochWhenWebTimestampsAreMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "local.ts"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := projectRecoveryDiagnostics(dir,
		map[string]gliderChangeEntry{"web.ts": {URI: "now-file:/app/web.ts", Type: "file"}},
		map[string][]byte{"web.ts": []byte("web")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.remoteNewest != "unavailable (Web timestamps not provided)" || strings.Contains(report.remoteNewest, "1970") {
		t.Fatalf("remote newest=%q", report.remoteNewest)
	}
}

func TestPrepareBuildProjectDoesNotPromptForUnsafeCanonicalSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	remote := &syncTestRemote{files: map[string][]byte{"now.config.json": []byte(`{"scope":"x_demo"}`), "package.json": []byte(`{"name":"demo"}`)}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	target := filepath.Join(home, "BA", "Bocota")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), target); err != nil {
		t.Fatal(err)
	}
	prompted := false
	c.projectRecoveryApproval = func([][2]string, string) (bool, error) { prompted = true; return true, nil }
	_, _, _, err := c.prepareBuildProject(context.Background(), nil)
	if err == nil || prompted {
		t.Fatalf("err=%v prompted=%v", err, prompted)
	}
}

func TestRecoverCanonicalBuildProjectPromptErrorLeavesTargetUntouched(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Earth")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep.ts"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{"now.config.json": []byte(`{"scope":"x_demo"}`)}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	c.projectRecoveryApproval = func([][2]string, string) (bool, error) { return false, errors.New("stdin failed") }
	_, err := c.recoverCanonicalBuildProject(context.Background(), &canonicalProjectCollisionError{Target: target, Reason: "invalid sync manifest", Recoverable: true}, buildInstallContext{RootURI: "now-file:/app"})
	if err == nil || buildInstallErrorCode(err) != "PROJECT_RECOVERY_PROMPT_FAILED" {
		t.Fatalf("err=%v", err)
	}
	if got := string(mustReadBuildFile(t, filepath.Join(target, "keep.ts"))); got != "keep" {
		t.Fatalf("target changed=%q", got)
	}
}

func TestRecoverCanonicalBuildProjectRemoteFailureDoesNotPrompt(t *testing.T) {
	target := filepath.Join(t.TempDir(), "Earth")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline", http.StatusServiceUnavailable) }))
	defer server.Close()
	c := testSyncClient(server)
	prompted := false
	c.projectRecoveryApproval = func([][2]string, string) (bool, error) { prompted = true; return true, nil }
	_, err := c.recoverCanonicalBuildProject(context.Background(), &canonicalProjectCollisionError{Target: target, Reason: "invalid sync manifest", Recoverable: true}, buildInstallContext{RootURI: "now-file:/app"})
	if err == nil || buildInstallErrorCode(err) != "PROJECT_RECOVERY_REMOTE_UNAVAILABLE" || prompted {
		t.Fatalf("err=%v prompted=%v", err, prompted)
	}
}

func TestDependencyInstallReleasesBuildProjectLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	oldWD, _ := os.Getwd()
	defer os.Chdir(oldWD)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{
		"now.config.json": []byte(`{"scope":"x_demo"}`),
		"package.json":    []byte(`{"name":"demo","dependencies":{"missing":"1.0.0"}}`),
	}}
	server := newSyncTestServer(t, remote)
	defer server.Close()
	c := testSyncClient(server)
	bin := t.TempDir()
	marker := filepath.Join(bin, "npm-ran")
	argsMarker := filepath.Join(bin, "npm-args")
	for name, script := range map[string]string{
		"node": "#!/bin/sh\nexit 0\n",
		"npm":  "#!/bin/sh\ntouch '" + marker + "'\nprintf '%s\\n' \"$@\" > '" + argsMarker + "'\ncase \" $* \" in *' --package-lock=false '*) ;; *) echo generated > package-lock.json ;; esac\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	result, status := c.answerInstallDependenciesParity(context.Background(), nil)
	if status != "complete" || result["success"] != true {
		t.Fatalf("result=%#v status=%q", result, status)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("npm did not run: %v", err)
	}
	args, err := os.ReadFile(argsMarker)
	if err != nil || !strings.Contains(string(args), "--package-lock=false") {
		t.Fatalf("npm args = %q, err=%v", args, err)
	}
	if _, err := os.Stat(filepath.Join(c.lastGliderBuild.TempDir, "package-lock.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dependency install created package-lock.json: %v", err)
	}
	assertBuildProjectLockAvailable(t, c.lastGliderBuild.TempDir)
}

func TestAcquirePersistentProjectLockHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	release, err := acquirePersistentProjectLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err = acquirePersistentProjectLock(ctx, dir)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline exceeded", err)
	}
}

func mustReadBuildFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func assertBuildProjectLockAvailable(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := acquirePersistentProjectLock(ctx, dir)
	if err != nil {
		t.Fatalf("build project lock was not released: %v", err)
	}
	release()
}
