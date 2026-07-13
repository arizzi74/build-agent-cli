package main

import (
	"strings"
	"testing"
)

func TestFormatAssistantTerminalRendersWebLikeMarkdown(t *testing.T) {
	input := strings.TrimSpace("## Example Scripts\n\n" +
		"**Count active incidents:**\n\n" +
		"```javascript\n" +
		"var gr = new GlideRecord(\"incident\");\n" +
		"gr.addActiveQuery();\n" +
		"gr.query();\n" +
		"out(\"Active incidents: \" + gr.getRowCount());\n" +
		"```\n\n" +
		"## Limitations\n\n" +
		"| Limitation | Details |\n" +
		"| --- | --- |\n" +
		"| **Scope** | Global scope only |\n" +
		"| **Timeout** | Subject to platform script execution time limits |\n\n" +
		"Would you like me to run a script for you?")

	got := formatAssistantTerminal(input, false, 100)
	checks := []string{
		"Example Scripts",
		"Count active incidents:",
		"╭─ javascript",
		"│ var gr = new GlideRecord(\"incident\");",
		"╰",
		"┌────────────┬──────────────────────────────────────────────────┐",
		"│ Limitation │ Details                                          │",
		"│ Scope      │ Global scope only                                │",
		"Would you like me to run a script for you?",
	}
	for _, check := range checks {
		if !strings.Contains(got, check) {
			t.Fatalf("formatted output missing %q:\n%s", check, got)
		}
	}
	for _, raw := range []string{"```", "##", "**Scope**"} {
		if strings.Contains(got, raw) {
			t.Fatalf("formatted output still contains raw markdown %q:\n%s", raw, got)
		}
	}
}

func TestMarkdownTableWrapsCellsWithinWidth(t *testing.T) {
	input := strings.TrimSpace("| Name | Description |\n" +
		"| --- | --- |\n" +
		"| Alpha | This description should wrap inside the table cell instead of being clipped on the right |")
	got := formatAssistantTerminal(input, false, 42)
	assertTableLinesFit(t, got, 42)
	if !strings.Contains(got, "│ Alpha") || !strings.Contains(got, "should wrap") || !strings.Contains(got, "inside the table cell") || !strings.Contains(got, "of being clipped") {
		t.Fatalf("wrapped table missing expected cell content:\n%s", got)
	}
}

func TestColoredAssistantTableAccountsForBulletIndent(t *testing.T) {
	input := strings.TrimSpace("| Name | Description |\n" +
		"| --- | --- |\n" +
		"| Alpha | This description should wrap inside the table cell instead of being clipped on the right |")
	got := formatAssistantResponseTerminal(input, true, 42)
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if runeLen(line) > 42 {
			t.Fatalf("colored assistant table line exceeds terminal width: len=%d line=%q\n%s", runeLen(line), line, got)
		}
	}
}

func assertTableLinesFit(t *testing.T, got string, width int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasPrefix(line, "┌") || strings.HasPrefix(line, "│") || strings.HasPrefix(line, "├") || strings.HasPrefix(line, "└") {
			if len([]rune(line)) > width {
				t.Fatalf("table line exceeds width: len=%d line=%q\n%s", len([]rune(line)), line, got)
			}
		}
	}
}

func TestCollectGatewayStreamContentDoesNotPrintMarkup(t *testing.T) {
	got := collectGatewayStreamContent(`<thinking>hidden</thinking><text>Hello **there**</text><signature>x</signature>`)
	if got != "Hello **there**" {
		t.Fatalf("stream text = %q, want assistant text only", got)
	}
}

func TestAssistantResponseBulletOnlyWhenColorEnabled(t *testing.T) {
	plain := formatAssistantResponseTerminal("Hello", false, 80)
	if strings.Contains(plain, "•") || !strings.Contains(plain, "Hello") {
		t.Fatalf("plain assistant output should stay script-friendly, got %q", plain)
	}
	colored := formatAssistantResponseTerminal("Hello", true, 80)
	if !strings.Contains(colored, "• ") || !strings.Contains(colored, "Hello") {
		t.Fatalf("colored assistant output should be bullet-prefixed, got %q", colored)
	}
}

func TestPrintAssistantTextAddsRoomOnlyForInteractiveColor(t *testing.T) {
	colored := formatAssistantResponseTerminal("Hello", true, 80)
	if !strings.HasPrefix(colored, "\x1b") || !strings.Contains(colored, "• ") {
		t.Fatalf("colored assistant formatting missing bullet/color: %q", colored)
	}
}

func TestStatusBarFormattingIncludesRequestedFieldsAndFitsWidth(t *testing.T) {
	state := statusBarState{
		Model:         "claude-opus-4-6",
		InputMessages: 7,
		InputTokens:   4007,
		OutputTokens:  81,
		Workspace:     "Default - admin",
		App:           "Demo App",
		Instance:      "https://demoalectriallwfze140800.service-now.com/",
	}
	plain := formatStatusBar(state, false, 80)
	for _, check := range []string{"model=claude-opus-4-6", "input_messages=7", "workspace=Default - admin", "app=Demo App", "instance=demoalectriallwfze140800.service-now.com"} {
		if !strings.Contains(plain, check) {
			t.Fatalf("plain status missing %q: %q", check, plain)
		}
	}
	for _, forbidden := range []string{"input_tokens=4007", "output_tokens=81", "input_tokens=0", "output_tokens=0"} {
		if strings.Contains(plain, forbidden) {
			t.Fatalf("plain status should not include token field %q: %q", forbidden, plain)
		}
	}
	colored := formatStatusBar(state, true, 140)
	if strings.Contains(colored, "48;5;") || !strings.Contains(colored, "38;5;118") || !strings.Contains(colored, "34") || !strings.Contains(colored, "35") || runeLen(colored) != 140 {
		t.Fatalf("colored status should be colorful, background-free, and fit width, len=%d text=%q", runeLen(colored), colored)
	}
}

func TestUserTerminalUsesGrayBackgroundOnlyWhenColorEnabled(t *testing.T) {
	plain := formatUserTerminal("hello", false)
	if !strings.Contains(plain, "› hello") || strings.Contains(plain, "48;5;236") {
		t.Fatalf("plain user output changed unexpectedly: %q", plain)
	}
	colored := formatUserTerminal("hello", true)
	if !strings.Contains(colored, "48;5;236") || !strings.Contains(colored, "hello") {
		t.Fatalf("colored user output missing gray background: %q", colored)
	}
}

func TestCommandInputLineCanFillFullWidthGrayPromptBand(t *testing.T) {
	plain := formatCommandInputLine("ba> ", "hello", false, 20)
	if plain != "ba> hello" {
		t.Fatalf("plain command prompt changed unexpectedly: %q", plain)
	}
	colored := formatCommandInputLine("ba> ", "hello", true, 20)
	if !strings.Contains(colored, "48;5;236") || runeLen(colored) != 20 {
		t.Fatalf("colored prompt should be a full-width gray band, len=%d text=%q", runeLen(colored), colored)
	}
	if got := stripANSI(colored); !strings.HasPrefix(got, " ba> hello") || !strings.HasSuffix(got, "  ") {
		t.Fatalf("colored prompt band missing content/padding: %q", got)
	}
	rows := formatCommandInputBandRows("ba> ", "hello", true, 20)
	if len(rows) != 3 {
		t.Fatalf("prompt band should be 3 rows, got %d", len(rows))
	}
	if stripANSI(rows[0]) != strings.Repeat(" ", 20) || stripANSI(rows[2]) != strings.Repeat(" ", 20) {
		t.Fatalf("top/bottom prompt band rows should be blank gray padding: %#v", rows)
	}
	if got := stripANSI(rows[1]); !strings.HasPrefix(got, " ba> hello") {
		t.Fatalf("middle prompt row should contain command text: %q", got)
	}
}

func TestLiveUserPromptPrintsThreeRowPromptBlock(t *testing.T) {
	formatted := formatLiveUserPromptTerminal("how are you today?", true, 32)
	plain := stripANSI(formatted)
	if strings.Contains(plain, "You") || !strings.Contains(plain, " › how are you today?") {
		t.Fatalf("live user prompt should contain only highlighted prompt block, got: %q", plain)
	}
	lines := strings.Split(strings.Trim(plain, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("live user prompt should include 3-row prompt block: %#v", lines)
	}
	block := lines[len(lines)-3:]
	if block[0] != strings.Repeat(" ", 32) || block[2] != strings.Repeat(" ", 32) {
		t.Fatalf("user prompt top/bottom rows should be full-width gray padding: %#v", block)
	}
	if !strings.HasPrefix(block[1], " › how are you today?") || len([]rune(block[1])) != 32 {
		t.Fatalf("user prompt middle row should contain padded prompt text: %q", block[1])
	}
}

func TestAnimatedConnectingLogoMatchesReferenceShapeAndGlows(t *testing.T) {
	first := animatedConnectingLogo(0)
	later := animatedConnectingLogo(5)
	plain := stripANSI(first)
	rows := strings.Split(plain, "\n")
	if len(rows) != 9 {
		t.Fatalf("connecting logo rows = %d, want 9: %q", len(rows), plain)
	}
	if first == later || !strings.Contains(first, "38;5;") {
		t.Fatalf("connecting logo should animate with wasabi glow")
	}
	for _, r := range plain {
		if r > 127 {
			t.Fatalf("connecting logo must use ASCII only, found %q", r)
		}
	}
	if len([]rune(rows[0])) > 24 || !strings.Contains(rows[3], "oOO'") || !strings.Contains(rows[4], "OOO              OOO") {
		t.Fatalf("connecting logo does not preserve the narrow open-center ring silhouette: %q", plain)
	}
	details := "profile: zaiagents\ntransport: Nirvana websocket\n"
	frame := stripANSI(connectingScreenFrame(details, 2))
	profileAt := strings.Index(frame, "profile: zaiagents")
	transportAt := strings.Index(frame, "transport: Nirvana websocket")
	connectingAt := strings.Index(frame, "Connecting...")
	if !strings.HasPrefix(frame, rows[0]) || profileAt < len(rows[0]) || transportAt <= profileAt || connectingAt <= transportAt {
		t.Fatalf("connecting screen order should be logo, details, status: %q", frame)
	}
}

func TestAnimatedBuildingAndConnectingStatusBounceAndUseWasabiPalette(t *testing.T) {
	first := animatedBuildingStatus(0)
	later := animatedBuildingStatus(5)
	if first == later || !strings.Contains(first, "38;5;190") || !strings.Contains(stripANSI(later), "Building...") {
		t.Fatalf("animated building status did not vary/glow as expected: first=%q later=%q", first, later)
	}
	connecting := animatedConnectingStatus(3)
	if !strings.Contains(stripANSI(connecting), "Connecting...") || !strings.Contains(connecting, "38;5;") {
		t.Fatalf("animated connecting status missing label/glow: %q", connecting)
	}
}

func TestFormatConversationHistoryScrollback(t *testing.T) {
	history := []interface{}{
		map[string]interface{}{"role": "user", "content": "show me scripts"},
		map[string]interface{}{"role": "assistant-thinking", "content": "private reasoning"},
		map[string]interface{}{"role": "assistant", "content": "## Example\n\n```javascript\nvar x = 1;\n```"},
		map[string]interface{}{"role": "tool", "content": "tool output"},
	}
	got := formatConversationHistoryScrollback("Existing chat", history, false, 80)
	checks := []string{
		"conversation history",
		"Conversation: Existing chat",
		"› show me scripts",
		"Example",
		"╭─ javascript",
		"│ var x = 1;",
		"end history",
	}
	for _, check := range checks {
		if !strings.Contains(got, check) {
			t.Fatalf("history output missing %q:\n%s", check, got)
		}
	}
	for _, hidden := range []string{"Build Agent", "private reasoning", "tool output", "```", "##"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("history output contains hidden/raw content %q:\n%s", hidden, got)
		}
	}
}

func TestPickerFormattingCanUseColor(t *testing.T) {
	line := pickerOptionLine(conversationPickerOption{Value: "demo", Label: "demo https://demo.service-now.com [oauth, web-session]", Current: true}, true, true)
	if !strings.Contains(line, ansiWasabiGreen) || !strings.Contains(line, ansiYellow) || !strings.Contains(line, ansiCyan) {
		t.Fatalf("colored picker line missing expected ANSI accents: %q", line)
	}
	plain := stripANSI(line)
	if !strings.Contains(plain, "› * demo https://demo.service-now.com [oauth, web-session]") {
		t.Fatalf("colored picker line plain text changed: %q", plain)
	}
	cancel := pickerOptionLine(conversationPickerOption{Value: "__cancel__", Label: "Cancel"}, false, true)
	if !strings.Contains(cancel, ansiRed) || stripANSI(cancel) != "    Cancel" {
		t.Fatalf("cancel picker line should be red and plain-compatible: %q", cancel)
	}
}

func TestConversationPickerLinesMarksSelectionAndCurrent(t *testing.T) {
	lines := conversationPickerLines([]conversationPickerOption{
		{Value: "conv1", Label: "First chat", Current: true},
		{Value: "conv2", Label: "Second chat"},
		{Value: "__new__", Label: "New conversation"},
	}, 1)
	joined := strings.Join(lines, "\n")
	checks := []string{
		"↑/↓ choose",
		"  * First chat",
		"›   Second chat",
		"    New conversation",
	}
	for _, check := range checks {
		if !strings.Contains(joined, check) {
			t.Fatalf("picker output missing %q:\n%s", check, joined)
		}
	}
}

func TestConversationPickerOptionsPreselectCurrentConversation(t *testing.T) {
	currentID := "506fe6bc3b91c350d6531d9c73e45acc"
	options, selected := conversationPickerOptions([]WebConversation{
		{ID: "fc5540513b1d4750d6531d9c73e45acd", Title: "Other workspace chat"},
		{ID: currentID, Title: "Last workspace chat"},
	}, normalizeCodeAssistConversationID(currentID), true)
	if selected != 1 {
		t.Fatalf("selected = %d, want 1; options=%#v", selected, options)
	}
	if !options[1].Current || options[selected].Value != currentID {
		t.Fatalf("current conversation not marked/preselected: selected=%d options=%#v", selected, options)
	}
}

func TestConversationPickerScopeLabelsAndGlobalExplanation(t *testing.T) {
	options, selected := conversationPickerOptions([]WebConversation{
		{ID: "global", Title: "Everywhere"},
		{ID: "app", ApplicationID: "app-id", ApplicationName: "Scoped app", Title: "Only here"},
	}, "global", true)
	if selected != 0 || !options[0].Current || !options[0].Global {
		t.Fatalf("global conversation should remain current/preselected: selected=%d options=%#v", selected, options)
	}
	if !strings.Contains(options[0].Label, "🌐 Global / no app · Everywhere") || !strings.Contains(options[0].Label, "🕘 Last used") {
		t.Fatalf("global option label = %q", options[0].Label)
	}
	if !strings.Contains(options[1].Label, "📦 Scoped app · Only here") {
		t.Fatalf("app option label = %q", options[1].Label)
	}
	joined := strings.Join(conversationPickerLines(options, selected), "\n")
	if got := strings.Count(joined, "🌐 Global / no app conversations are available across workspaces."); got != 1 {
		t.Fatalf("global explanation count = %d, lines=%q", got, joined)
	}
}

func TestTerminalRowsCountsWrappingAndANSI(t *testing.T) {
	got := terminalRows("short\n\x1b[1m"+strings.Repeat("x", 101)+"\x1b[0m", 100)
	if got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}
}
