package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostAppSelectionStatusDoesNotMaterializeProjectOrRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	c := &Client{
		cfg:        CLIConfig{InstanceURL: "https://example.service-now.com"},
		opts:       Options{Profile: "test"},
		currentApp: &AppScope{ScopeID: "app-id", AppSysID: "app-id", ScopeName: "Demo App"},
	}
	var output strings.Builder
	_, err := withSlashCommandOutput(&output, func() (bool, error) {
		c.postAppSelectionStatus(context.Background())
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "BA", "Demo App")
	if !strings.Contains(output.String(), "local project: not materialized") || !strings.Contains(output.String(), want) {
		t.Fatalf("status output = %q, want non-materialized project at %q", output.String(), want)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("post-selection status created project directory: %v", err)
	}
	if _, err := os.Stat(projectRegistryPath(c.opts.Profile)); !os.IsNotExist(err) {
		t.Fatalf("post-selection status created registry: %v", err)
	}
}

func TestPostAppSelectionStatusClassifiesAndWarnsWithoutFailing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	remote := &syncTestRemote{files: map[string][]byte{"src/a.ts": []byte("base")}}
	server := newSyncTestServer(t, remote)
	c := testSyncClient(server)
	c.opts.Profile = "test"
	first, err := c.syncPersistentApp(context.Background(), persistentSyncAuto)
	if err != nil {
		t.Fatal(err)
	}
	statusText := func() string {
		var output strings.Builder
		_, err := withSlashCommandOutput(&output, func() (bool, error) {
			c.postAppSelectionStatus(context.Background())
			return true, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return output.String()
	}
	if got := statusText(); !strings.Contains(got, "status: clean") || !strings.Contains(got, first.LocalDir) {
		t.Fatalf("clean status = %q", got)
	}
	remote.files["src/a.ts"] = []byte("remote")
	if got := statusText(); !strings.Contains(got, "status: remote changes pending pull") {
		t.Fatalf("remote-pending status = %q", got)
	}
	if err := os.WriteFile(filepath.Join(first.LocalDir, "src", "a.ts"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := statusText(); !strings.Contains(got, "status: conflicts (1)") {
		t.Fatalf("conflict status = %q", got)
	}
	remote.files["src/a.ts"] = []byte("base")
	if got := statusText(); !strings.Contains(got, "status: local changes pending push") {
		t.Fatalf("local-pending status = %q", got)
	}
	server.Close()
	if got := statusText(); !strings.Contains(got, "warning: local project status unavailable:") {
		t.Fatalf("network warning = %q", got)
	}
}

func TestClassifyPersistentSyncStatus(t *testing.T) {
	for _, test := range []struct {
		result persistentSyncResult
		want   string
	}{
		{persistentSyncResult{}, "clean"},
		{persistentSyncResult{PendingPush: []string{"a"}}, "local changes pending push"},
		{persistentSyncResult{Pulled: []string{"a"}}, "remote changes pending pull"},
		{persistentSyncResult{PendingPush: []string{"a"}, Pulled: []string{"b"}}, "local changes pending push; remote changes pending pull"},
		{persistentSyncResult{PendingPush: []string{"a"}, Pulled: []string{"b"}, Conflicts: []string{"c"}}, "conflicts (1)"},
	} {
		if got := classifyPersistentSyncStatus(test.result); got != test.want {
			t.Fatalf("classifyPersistentSyncStatus(%+v) = %q, want %q", test.result, got, test.want)
		}
	}
}
