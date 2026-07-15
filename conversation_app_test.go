package main

import (
	"context"
	"testing"
)

func TestApplyWebConversationAppUsesMatchingWorkspaceApp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c.workspaceFolders = []WebWorkspaceFolder{
		{Name: "Demo App", URI: "now-file:/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{Name: "Other App", URI: "now-file:/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}
	statusChecks := 0
	c.postSelectionStatus = func(context.Context) error { statusChecks++; return nil }
	conv := WebConversation{
		ID:              "cccccccccccccccccccccccccccccccc",
		Title:           "Conversation for demo",
		ApplicationID:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ApplicationName: "Raw Conversation App Name",
	}
	if err := c.applyWebConversationApp(context.Background(), conv); err != nil {
		t.Fatalf("applyWebConversationApp returned error: %v", err)
	}
	if c.CurrentApp() == nil || c.CurrentApp().AppSysID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || c.CurrentApp().ScopeName != "Demo App" {
		t.Fatalf("conversation app did not use matching workspace app: %#v", c.CurrentApp())
	}
	if c.appScope != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("appScope = %#v", c.appScope)
	}
	if saved, ok := loadActiveApp(c.opts.Profile); !ok || saved.AppSysID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || saved.ScopeName != "Demo App" {
		t.Fatalf("active app was not persisted from conversation: %#v ok=%v", saved, ok)
	}
	if statusChecks != 1 {
		t.Fatalf("post-selection status checks = %d, want 1", statusChecks)
	}
}

func TestApplyWebConversationAppFallsBackToConversationApplicationName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	conv := WebConversation{
		ID:              "cccccccccccccccccccccccccccccccc",
		Title:           "Conversation for raw app",
		ApplicationID:   "dddddddddddddddddddddddddddddddd",
		ApplicationName: "Conversation App",
	}
	statusChecks := 0
	c.postSelectionStatus = func(context.Context) error { statusChecks++; return nil }
	if err := c.applyWebConversationApp(context.Background(), conv); err != nil {
		t.Fatalf("applyWebConversationApp returned error: %v", err)
	}
	if c.CurrentApp() == nil || c.CurrentApp().AppSysID != "dddddddddddddddddddddddddddddddd" || c.CurrentApp().ScopeName != "Conversation App" {
		t.Fatalf("conversation app fallback mismatch: %#v", c.CurrentApp())
	}
	if statusChecks != 1 {
		t.Fatalf("post-selection status checks = %d, want 1", statusChecks)
	}
}

func TestApplyWebConversationAppClearsWhenConversationHasNoApp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c.currentApp = &AppScope{ScopeID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ScopeName: "Demo App", AppSysID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	c.appScope = c.currentApp.ScopeID
	if err := saveActiveApp(c.opts.Profile, *c.currentApp); err != nil {
		t.Fatal(err)
	}
	conv := WebConversation{
		ID:    "cccccccccccccccccccccccccccccccc",
		Title: "Conversation without app",
	}
	if err := c.applyWebConversationApp(context.Background(), conv); err != nil {
		t.Fatalf("applyWebConversationApp returned error: %v", err)
	}
	if c.CurrentApp() != nil || c.appScope != nil {
		t.Fatalf("conversation without app should clear app, got app=%#v appScope=%#v", c.CurrentApp(), c.appScope)
	}
	if saved, ok := loadActiveApp(c.opts.Profile); ok || saved != nil {
		t.Fatalf("active app should be deleted, got %#v ok=%v", saved, ok)
	}
	if state := c.statusBarState(); state.App != "" {
		t.Fatalf("status app should be empty before formatter displays <none>, got %q", state.App)
	}
}
