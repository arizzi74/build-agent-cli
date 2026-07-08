package main

import (
	"context"
	"strings"
	"testing"
)

func TestSlashCommandSuggestionsAdvertiseOtherOptionsButNoConversationSubcommands(t *testing.T) {
	suggestions := slashCommandSuggestions()
	texts := map[string]bool{}
	for _, suggestion := range suggestions {
		texts[suggestion.Text] = true
	}
	for _, want := range []string{
		"/help",
		"/conversation",
		"/mcp list",
		"/workspace",
		"/app",
		"/exit",
		"/quit",
	} {
		if !texts[want] {
			t.Fatalf("slash picker missing %q in %#v", want, suggestions)
		}
	}
	for _, forbidden := range []string{
		"/conversation current",
		"/conversation list",
		"/conversation new",
		"/conversation use ",
		"/workspace current",
		"/workspace list",
		"/workspace new ",
		"/workspace use ",
		"/workspace reset",
		"/workspace delete ",
		"/app current",
		"/app use ",
		"/app clear",
	} {
		if texts[forbidden] {
			t.Fatalf("slash picker should not advertise parameterized picker command %q", forbidden)
		}
	}
}

func TestSlashMenuEnterRunsCompleteCommandsButNotArgumentTemplates(t *testing.T) {
	if !slashMenuEnterSubmits(SlashCommandSuggestion{Text: "/workspace"}) {
		t.Fatal("/workspace should run on Enter so it opens the workspace picker")
	}
	if !slashMenuEnterSubmits(SlashCommandSuggestion{Text: "/conversation"}) {
		t.Fatal("/conversation should run on Enter so it opens the conversation picker")
	}
	if !slashMenuEnterSubmits(SlashCommandSuggestion{Text: "/app"}) {
		t.Fatal("/app should run on Enter so it opens the app picker")
	}
}

func TestSlashMenuFiltersSingleWorkspaceCommand(t *testing.T) {
	suggestions := filterSlashSuggestions("/w")
	if len(suggestions) != 1 || suggestions[0].Text != "/workspace" {
		t.Fatalf("/w suggestions = %#v, want only /workspace", suggestions)
	}
}

func TestSlashMenuChoiceIsStableAfterMenuClose(t *testing.T) {
	suggestions := []SlashCommandSuggestion{
		{Text: "/conversation"},
		{Text: "/workspace"},
	}
	choice := slashMenuChoice(suggestions, 1)
	if choice.Text != "/workspace" {
		t.Fatalf("selected choice mismatch: %#v", choice)
	}
	if !slashMenuEnterSubmits(choice) {
		t.Fatalf("selected complete command should submit after menu close: %#v", choice)
	}
	if got := slashMenuChoice(suggestions, 99).Text; got != "/workspace" {
		t.Fatalf("out-of-range selected index should clamp to last suggestion, got %q", got)
	}
}

func TestWorkspaceListSlashCommandIsHandled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	handled, err := handleSlashCommand(context.Background(), c, "/workspace list")
	if err != nil {
		t.Fatalf("/workspace list returned error: %v", err)
	}
	if !handled {
		t.Fatal("/workspace list was not handled")
	}
}

func TestSlashCommandOutputCanBeCapturedForWorkspaceList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	handled, err := withSlashCommandOutput(&output, func() (bool, error) {
		return handleSlashCommand(context.Background(), c, "/workspace list")
	})
	if err != nil {
		t.Fatalf("/workspace list returned error: %v", err)
	}
	if !handled {
		t.Fatal("/workspace list was not handled")
	}
	got := output.String()
	if !strings.Contains(got, "workspaces:") || !strings.Contains(got, "default") {
		t.Fatalf("captured workspace list output mismatch: %q", got)
	}
}

func TestWorkspaceResetSlashCommandPrintsConfirmation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c.conversationID = "old-conversation"
	var output strings.Builder
	handled, err := withSlashCommandOutput(&output, func() (bool, error) {
		return handleSlashCommand(context.Background(), c, "/workspace reset")
	})
	if err != nil {
		t.Fatalf("/workspace reset returned error: %v", err)
	}
	if !handled {
		t.Fatal("/workspace reset was not handled")
	}
	if c.conversationID != "" {
		t.Fatalf("workspace reset did not clear conversation id: %q", c.conversationID)
	}
	if got := output.String(); !strings.Contains(got, "workspace reset: default") {
		t.Fatalf("captured workspace reset confirmation mismatch: %q", got)
	}
}

func TestSlashCommandOutputCapturePolicy(t *testing.T) {
	if !slashCommandOutputCanBeCaptured("/workspace list") {
		t.Fatal("legacy workspace list output should still be captured for managed terminal replay")
	}
	if slashCommandOutputCanBeCaptured("/workspace") {
		t.Fatal("bare /workspace opens terminal UI and should not be captured")
	}
	if slashCommandOutputCanBeCaptured("/workspace select") {
		t.Fatal("/workspace select opens terminal UI and should not be captured")
	}
	if slashCommandOutputCanBeCaptured("/app") {
		t.Fatal("bare /app opens terminal UI and should not be captured")
	}
	if slashCommandOutputCanBeCaptured("/app list") {
		t.Fatal("/app list opens terminal UI and should not be captured")
	}
	if slashCommandOutputCanBeCaptured("/conversation") {
		t.Fatal("bare /conversation opens terminal UI and should not be captured")
	}
	if slashCommandOutputCanBeCaptured("/conversation list") {
		t.Fatal("/conversation list opens terminal UI in TTY and should not be captured")
	}
}
