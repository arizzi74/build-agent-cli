package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
