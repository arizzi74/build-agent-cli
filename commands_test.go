package main

import (
	"context"
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
		"/workspace current",
		"/workspace list",
		"/workspace new ",
		"/workspace use ",
		"/workspace reset",
		"/workspace delete ",
		"/app current",
		"/app use ",
		"/app clear",
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
	} {
		if texts[forbidden] {
			t.Fatalf("slash picker should not advertise parameterized conversation command %q", forbidden)
		}
	}
}

func TestSlashMenuEnterRunsCompleteCommandsButNotArgumentTemplates(t *testing.T) {
	if !slashMenuEnterSubmits(SlashCommandSuggestion{Text: "/workspace list"}) {
		t.Fatal("complete slash command should run on Enter")
	}
	if !slashMenuEnterSubmits(SlashCommandSuggestion{Text: "/conversation"}) {
		t.Fatal("/conversation should run on Enter so it opens the conversation picker")
	}
	if slashMenuEnterSubmits(SlashCommandSuggestion{Text: "/workspace use "}) {
		t.Fatal("argument template should be inserted, not run, on Enter")
	}
	if got := slashMenuInsertText(SlashCommandSuggestion{Text: "/workspace use "}); got != "/workspace use " {
		t.Fatalf("slash menu insert text should preserve argument placeholder spacing, got %q", got)
	}
}

func TestSlashMenuChoiceIsStableAfterMenuClose(t *testing.T) {
	suggestions := []SlashCommandSuggestion{
		{Text: "/workspace current"},
		{Text: "/workspace list"},
	}
	choice := slashMenuChoice(suggestions, 1)
	if choice.Text != "/workspace list" {
		t.Fatalf("selected choice mismatch: %#v", choice)
	}
	if !slashMenuEnterSubmits(choice) {
		t.Fatalf("selected complete command should submit after menu close: %#v", choice)
	}
	if got := slashMenuChoice(suggestions, 99).Text; got != "/workspace list" {
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
