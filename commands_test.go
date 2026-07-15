package main

import (
	"context"
	"strings"
	"testing"
)

func TestSlashCommandRegistryIsValidAndCanonical(t *testing.T) {
	if err := validateSlashCommandRegistry(slashCommandRegistry); err != nil {
		t.Fatalf("registry validation failed: %v", err)
	}
	for _, command := range slashCommandRegistry {
		if got, ok := findSlashCommand(command.Canonical); !ok || got.Canonical != command.Canonical {
			t.Fatalf("canonical lookup for %q = %#v, %v", command.Canonical, got, ok)
		}
		for _, alias := range command.Aliases {
			if got, ok := findSlashCommand(alias); !ok || got.Canonical != command.Canonical {
				t.Fatalf("alias lookup for %q = %#v, %v", alias, got, ok)
			}
		}
	}
}

func TestSlashCommandRegistryRejectsInvalidDefinitions(t *testing.T) {
	duplicateAlias := append([]SlashCommandDefinition(nil), slashCommandRegistry[:2]...)
	duplicateAlias[1].Aliases = append(duplicateAlias[1].Aliases, "/help")
	if err := validateSlashCommandRegistry(duplicateAlias); err == nil {
		t.Fatal("registry validation accepted a duplicate alias")
	}
	outOfOrder := append([]SlashCommandDefinition(nil), slashCommandRegistry[:2]...)
	outOfOrder[1].Order = outOfOrder[0].Order
	if err := validateSlashCommandRegistry(outOfOrder); err == nil {
		t.Fatal("registry validation accepted duplicate presentation order")
	}
	badSuggestion := append([]SlashCommandDefinition(nil), slashCommandRegistry[:2]...)
	badSuggestion[1].Suggestions = []string{"/help"}
	if err := validateSlashCommandRegistry(badSuggestion); err == nil {
		t.Fatal("registry validation accepted a suggestion for another command")
	}
}

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

func TestSlashCommandAvailabilityFiltersRuntimeAndProcessing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test", Nirvana: false})
	if err != nil {
		t.Fatal(err)
	}
	for _, suggestion := range slashCommandSuggestionsForClient(c) {
		if suggestion.Text == "/mcp list" {
			t.Fatal("web gateway suggestions must not include Nirvana-only /mcp")
		}
	}
	if _, err := handleSlashCommand(context.Background(), c, "/mcp list"); err == nil || !strings.Contains(err.Error(), "--nirvana") {
		t.Fatalf("web gateway /mcp dispatch error = %v", err)
	}
	var runtimeHelp strings.Builder
	if _, err := withSlashCommandOutput(&runtimeHelp, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/help") }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(runtimeHelp.String(), "/mcp") {
		t.Fatalf("web gateway help must hide Nirvana-only /mcp: %q", runtimeHelp.String())
	}
	c.opts.Nirvana = true
	c.processing = true
	for _, suggestion := range slashCommandSuggestionsForClient(c) {
		if suggestion.Text == "/workspace" || suggestion.Text == "/conversation" || suggestion.Text == "/app" {
			t.Fatalf("processing suggestions must hide unavailable command %q", suggestion.Text)
		}
	}
	if _, err := handleSlashCommand(context.Background(), c, "/workspace list"); err == nil || !strings.Contains(err.Error(), "while a turn is processing") {
		t.Fatalf("processing workspace dispatch error = %v", err)
	}
	var output strings.Builder
	if _, err := withSlashCommandOutput(&output, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/help") }); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); strings.Contains(got, "/workspace") || !strings.Contains(got, "/help") || !strings.Contains(got, "/exit") {
		t.Fatalf("processing help did not reflect availability: %q", got)
	} else if strings.Count(got, "  General:\n") != 1 {
		t.Fatalf("help must group each category exactly once: %q", got)
	}
	if handled, err := handleSlashCommand(context.Background(), c, "/quit"); handled || err != nil {
		t.Fatalf("/quit must retain exit semantics while processing: handled=%v err=%v", handled, err)
	}
}

func TestSlashMenuBehaviorUsesRegistryArgumentPolicy(t *testing.T) {
	if slashMenuEnterSubmits(SlashCommandSuggestion{Text: "/mcp", Behavior: SlashCommandTemplate}) {
		t.Fatal("template behavior must insert rather than execute")
	}
	if !slashMenuEnterSubmits(SlashCommandSuggestion{Text: "/mcp list", Behavior: SlashCommandImmediate}) {
		t.Fatal("complete required-subcommand suggestion must execute")
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
	handled, err := handleSlashCommand(context.Background(), c, "/ws list")
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

func TestSlashHelpAndSuggestionsShareRegistryBehavior(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test", Nirvana: true})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if _, err := withSlashCommandOutput(&output, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/?") }); err != nil {
		t.Fatal(err)
	}
	for _, suggestion := range slashCommandSuggestionsForClient(c) {
		command, ok := findSlashCommand(strings.Fields(suggestion.Text)[0])
		if !ok {
			t.Fatalf("suggestion %q has no registry command", suggestion.Text)
		}
		if !strings.Contains(output.String(), command.Canonical) {
			t.Fatalf("help missing suggested command %q: %q", command.Canonical, output.String())
		}
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

func TestAppUseRunsOnePostSelectionStatusCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient(CLIConfig{InstanceURL: "https://example.service-now.com"}, Options{Profile: "test"})
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	c.postSelectionStatus = func(context.Context) error { checks++; return nil }
	if _, err := withSlashCommandOutput(&strings.Builder{}, func() (bool, error) {
		return handleSlashCommand(context.Background(), c, "/app use app-id Demo")
	}); err != nil {
		t.Fatal(err)
	}
	if checks != 1 {
		t.Fatalf("post-selection status checks = %d, want 1", checks)
	}
}
