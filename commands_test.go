package main

import "testing"

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
