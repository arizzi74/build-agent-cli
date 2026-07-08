package main

import (
	"context"
	"strings"
	"testing"
)

func TestWorkspaceAppChoicesComeFromNowFileFolders(t *testing.T) {
	c := &Client{
		workspaceFolders: []WebWorkspaceFolder{
			{Name: "Zeta App", URI: "now-file:/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			{Name: "Alpha App", URI: "now-file:/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{Name: "Ignored File", URI: "settings:/users/user123/settings.json"},
			{Name: "Duplicate Alpha", URI: "now-file:/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
		currentApp: &AppScope{ScopeID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AppSysID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ScopeName: "Alpha App"},
	}
	choices, err := c.ListWorkspaceAppChoices(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaceAppChoices returned error: %v", err)
	}
	if len(choices) != 2 {
		t.Fatalf("choices length = %d, want 2: %#v", len(choices), choices)
	}
	if choices[0].App.ScopeName != "Alpha App" || !choices[0].Current || !strings.Contains(choices[0].Label, "[workspace]") {
		t.Fatalf("first choice mismatch: %#v", choices[0])
	}
	if choices[1].App.ScopeName != "Zeta App" {
		t.Fatalf("choices not sorted by app name: %#v", choices)
	}
	choice, ok := appChoiceBySelection(choices, "Zeta")
	if !ok || choice.App.AppSysID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("appChoiceBySelection(Zeta) = %#v, %v", choice, ok)
	}
}

func TestUseAppChoicePersistsSelectedWorkspaceApp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	choice := AppChoice{App: AppScope{ScopeID: "appsysid123", ScopeName: "Demo App", AppSysID: "appsysid123"}}
	var output strings.Builder
	_, err = withSlashCommandOutput(&output, func() (bool, error) {
		return true, c.UseAppChoice(context.Background(), choice)
	})
	if err != nil {
		t.Fatalf("UseAppChoice returned error: %v", err)
	}
	if c.CurrentApp() == nil || c.CurrentApp().AppSysID != "appsysid123" || c.appScope != "appsysid123" {
		t.Fatalf("current app mismatch: %#v appScope=%#v", c.CurrentApp(), c.appScope)
	}
	if got := output.String(); !strings.Contains(got, "app set: appsysid123") || !strings.Contains(got, "app name: Demo App") {
		t.Fatalf("app selection output mismatch: %q", got)
	}
	ws, ok := loadWorkspace(c.opts.Profile, c.workspaceName)
	if !ok || ws.App == nil || ws.App.AppSysID != "appsysid123" {
		t.Fatalf("selected app not persisted in workspace: %#v ok=%v", ws, ok)
	}
}

func TestWorkspaceAppChoicesCanFallbackToWorkingSet(t *testing.T) {
	c := &Client{
		workingSet: []interface{}{
			map[string]interface{}{"name": "Working Set App", "uri": "now-file:/cccccccccccccccccccccccccccccccc"},
		},
	}
	choices, err := c.ListWorkspaceAppChoices(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaceAppChoices returned error: %v", err)
	}
	if len(choices) != 1 || choices[0].App.ScopeName != "Working Set App" || choices[0].App.AppSysID != "cccccccccccccccccccccccccccccccc" {
		t.Fatalf("unexpected choices from workingSet: %#v", choices)
	}
}
