package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newPackageDocsClient(t *testing.T) (*Client, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	project := filepath.Join(t.TempDir(), "project")
	c := &Client{
		cfg:        CLIConfig{InstanceURL: "https://example.test"},
		opts:       Options{Profile: "package-docs-test"},
		currentApp: &AppScope{AppSysID: "app", ScopeID: "app", ScopeName: "Demo"},
	}
	if err := ensurePersistentProjectRoot(project); err != nil {
		t.Fatal(err)
	}
	if err := savePersistentSyncManifest(project, persistentSyncManifest{
		InstanceURL: c.cfg.InstanceURL,
		AppID:       "app",
		RootURI:     "now-file:/app",
		Files:       map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	c.localProjectOverride = project
	return c, project
}

func writePackageDocsPackage(t *testing.T, project, name, manifest, readme string) string {
	t.Helper()
	dir := filepath.Join(project, "node_modules", filepath.FromSlash(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if readme != "" {
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAnswerPackageDocsSupportsScopedAndUnscopedPackages(t *testing.T) {
	c, project := newPackageDocsClient(t)
	writePackageDocsPackage(t, project, "@servicenow/sdk", `{
		"name":"@servicenow/sdk", "version":"2.0.0", "description":"SDK", "license":"Apache-2.0",
		"exports":{".":"./index.js", "./server":{"default":"./server.js"}}
	}`, "# SDK\nUseful package docs.")
	writePackageDocsPackage(t, project, "left-pad", `{"name":"left-pad","version":"1.3.0"}`, "# left-pad")

	for _, tc := range []struct {
		name string
		path string
	}{
		{"@servicenow/sdk", "node_modules/@servicenow/sdk"},
		{"left-pad", "node_modules/left-pad"},
	} {
		result, status := c.answerPackageDocs(context.Background(), map[string]interface{}{"packageName": tc.name})
		if status != "complete" {
			t.Fatalf("%s: status=%q result=%#v", tc.name, status, result)
		}
		if result["packageName"] != tc.name || result["packagePath"] != tc.path {
			t.Fatalf("%s: result=%#v", tc.name, result)
		}
		if !result["readmeFound"].(bool) || !result["documentationAvailable"].(bool) {
			t.Fatalf("%s: expected README flags in %#v", tc.name, result)
		}
	}
	result, _ := c.answerPackageDocs(context.Background(), map[string]interface{}{"packageName": "@servicenow/sdk"})
	exports := result["exports"].([]string)
	if strings.Join(exports, ",") != ".,./server" {
		t.Fatalf("exports=%#v", exports)
	}
}

func TestAnswerPackageDocsMissingProjectPackageAndDocs(t *testing.T) {
	c := &Client{
		cfg:        CLIConfig{InstanceURL: "https://example.test"},
		opts:       Options{Profile: "package-docs-missing"},
		currentApp: &AppScope{AppSysID: "app", ScopeID: "app"},
	}
	result, status := c.answerPackageDocs(context.Background(), map[string]interface{}{"packageName": "present"})
	if status != "error" || result["code"] != "LOCAL_PROJECT_NOT_FOUND" {
		t.Fatalf("missing project: status=%q result=%#v", status, result)
	}

	c, project := newPackageDocsClient(t)
	result, status = c.answerPackageDocs(context.Background(), map[string]interface{}{"packageName": "absent"})
	if status != "error" || result["code"] != "PACKAGE_NOT_INSTALLED" {
		t.Fatalf("missing package: status=%q result=%#v", status, result)
	}
	writePackageDocsPackage(t, project, "present", `{"name":"present","version":"1.0.0","description":"metadata only"}`, "")
	result, status = c.answerPackageDocs(context.Background(), map[string]interface{}{"packageName": "present"})
	if status != "complete" || result["readmeFound"] != false || result["documentationAvailable"] != false || result["readme"] != "" {
		t.Fatalf("missing README: status=%q result=%#v", status, result)
	}
	if !strings.Contains(result["message"].(string), "README not found") {
		t.Fatalf("missing README message=%#v", result)
	}
}

func TestAnswerPackageDocsRejectsTraversalAndSymlinkEscape(t *testing.T) {
	c, project := newPackageDocsClient(t)
	for _, name := range []string{"../outside", "@scope/../outside", "@scope", "foo/bar"} {
		result, status := c.answerPackageDocs(context.Background(), map[string]interface{}{"packageName": name})
		if status != "error" || result["code"] != "INVALID_PACKAGE_NAME" {
			t.Fatalf("traversal %q: status=%q result=%#v", name, status, result)
		}
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "package.json"), []byte(`{"name":"escape"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "node_modules", "escape")); err != nil {
		t.Fatal(err)
	}
	result, status := c.answerPackageDocs(context.Background(), map[string]interface{}{"packageName": "escape"})
	if status != "error" || result["code"] != "UNSAFE_PACKAGE_PATH" {
		t.Fatalf("symlink escape: status=%q result=%#v", status, result)
	}
}

func TestAnswerPackageDocsBoundsOutputAndHandlesCancellation(t *testing.T) {
	c, project := newPackageDocsClient(t)
	longREADME := strings.Repeat("x", packageDocsMaxREADMEBytes+100)
	keywords := make([]string, 0, 2)
	keywords = append(keywords, "zeta", "alpha")
	manifest, err := json.Marshal(map[string]interface{}{
		"name": "large", "description": strings.Repeat("d", packageDocsMaxStringBytes+50), "keywords": keywords,
		"exports": map[string]string{"./z": "./z.js", "./a": "./a.js"},
	})
	if err != nil {
		t.Fatal(err)
	}
	writePackageDocsPackage(t, project, "large", string(manifest), longREADME)
	result, status := c.answerPackageDocs(context.Background(), map[string]interface{}{"packageName": "large"})
	if status != "complete" || !result["truncated"].(bool) || len(result["readme"].(string)) != packageDocsMaxREADMEBytes {
		t.Fatalf("bounded README: status=%q result sizes=(%v,%d)", status, result["truncated"], len(result["readme"].(string)))
	}
	metadata := result["metadata"].(map[string]interface{})
	if len(metadata["description"].(string)) != packageDocsMaxStringBytes {
		t.Fatalf("bounded metadata=%#v", metadata)
	}
	if got := strings.Join(result["exports"].([]string), ","); got != "./a,./z" {
		t.Fatalf("deterministic exports=%q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, status = c.answerPackageDocs(ctx, map[string]interface{}{"packageName": "large"})
	if status != "error" || result["code"] != "PACKAGE_DOCS_CANCELLED" {
		t.Fatalf("cancelled: status=%q result=%#v", status, result)
	}
}
