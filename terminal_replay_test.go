package main

import (
	"strings"
	"testing"
)

func TestTerminalTranscriptRowsRenderStructuredTurns(t *testing.T) {
	rows := terminalTranscriptRows([]terminalTranscriptEntry{
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: "# Answer\n\n- one"},
	}, false, 80)
	joined := strings.Join(rows, "\n")
	if strings.Contains(joined, "You") {
		t.Fatalf("replayed user transcript should not include label:\n%s", joined)
	}
	if strings.Contains(joined, "Build Agent") {
		t.Fatalf("replayed assistant transcript should not include label:\n%s", joined)
	}
	for _, want := range []string{"› hello", "Answer", "• one"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("replayed transcript missing %q in:\n%s", want, joined)
		}
	}
}

func TestTerminalTranscriptSnapshotIncludesActiveAssistant(t *testing.T) {
	terminalTranscript.Lock()
	previousEntries := terminalTranscript.entries
	previousActive := terminalTranscript.activeAssistant
	terminalTranscript.entries = nil
	terminalTranscript.activeAssistant = ""
	terminalTranscript.Unlock()
	defer func() {
		terminalTranscript.Lock()
		terminalTranscript.entries = previousEntries
		terminalTranscript.activeAssistant = previousActive
		terminalTranscript.Unlock()
	}()

	terminalRecordUserPrompt("question")
	terminalSetActiveAssistantText("partial answer")
	joined := strings.Join(terminalTranscriptRows(terminalTranscriptSnapshot(), false, 80), "\n")
	if !strings.Contains(joined, "› question") || !strings.Contains(joined, "partial answer") {
		t.Fatalf("active assistant was not included in replay rows:\n%s", joined)
	}
	terminalRecordAssistantText("final answer")
	joined = strings.Join(terminalTranscriptRows(terminalTranscriptSnapshot(), false, 80), "\n")
	if strings.Contains(joined, "partial answer") || !strings.Contains(joined, "final answer") {
		t.Fatalf("final assistant did not replace active assistant:\n%s", joined)
	}
}

func TestTerminalSetConversationHistoryReplacesPriorTranscript(t *testing.T) {
	terminalTranscript.Lock()
	previousEntries := terminalTranscript.entries
	previousActive := terminalTranscript.activeAssistant
	terminalTranscript.entries = nil
	terminalTranscript.activeAssistant = "old partial"
	terminalTranscript.Unlock()
	defer func() {
		terminalTranscript.Lock()
		terminalTranscript.entries = previousEntries
		terminalTranscript.activeAssistant = previousActive
		terminalTranscript.Unlock()
	}()

	terminalRecordUserPrompt("old conversation prompt")
	terminalSetConversationHistory("New chat", []interface{}{
		map[string]interface{}{"role": "user", "content": "new prompt"},
		map[string]interface{}{"role": "assistant", "content": "new answer"},
	})
	joined := strings.Join(terminalTranscriptRows(terminalTranscriptSnapshot(), false, 80), "\n")
	for _, want := range []string{"Conversation", "New chat", "› new prompt", "new answer"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("conversation history replay missing %q:\n%s", want, joined)
		}
	}
	for _, old := range []string{"old conversation prompt", "old partial"} {
		if strings.Contains(joined, old) {
			t.Fatalf("conversation history replay still contains old transcript %q:\n%s", old, joined)
		}
	}
}

func TestTerminalTranscriptRowsFormatsToolResults(t *testing.T) {
	rows := terminalTranscriptRows([]terminalTranscriptEntry{
		{Role: "tool_success", Text: "list_properties\nListed 12 properties"},
		{Role: "tool_error", Text: "run_script\nScript failed validation"},
	}, false, 80)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "✓ list_properties\n  Listed 12 properties") || !strings.Contains(joined, "✗ run_script\n  Script failed validation") {
		t.Fatalf("tool result rows missing markers:\n%s", joined)
	}
}

func TestColoredToolResultDescriptionUsesTableTextColorNotSuccessColor(t *testing.T) {
	formatted := formatToolResultTerminal("fs_write_file\nSuccessfully wrote file", true, true)
	if !strings.Contains(formatted, ansiWasabiGreen+"✓ fs_write_file") {
		t.Fatalf("tool name should be success colored: %q", formatted)
	}
	if !strings.Contains(formatted, "\n"+ansiGrayFG+"  Successfully wrote file") {
		t.Fatalf("description should be on next row in table-text color: %q", formatted)
	}
}

func TestWrapReplayRowsWrapsInsteadOfTruncating(t *testing.T) {
	rows := wrapReplayRows([]string{"abcdefghijklmnopqrstuvwxyz"}, 10)
	if got, want := strings.Join(rows, "|"), "abcdefghij|klmnopqrst|uvwxyz"; got != want {
		t.Fatalf("wrapped rows mismatch: got %q want %q", got, want)
	}
}

func TestWrapReplayRowsIgnoresANSIForWidth(t *testing.T) {
	rows := wrapReplayRows([]string{ansiBold + "abcdefghij" + ansiReset}, 5)
	plain := []string{stripANSI(rows[0]), stripANSI(rows[1])}
	if strings.Join(plain, "|") != "abcde|fghij" {
		t.Fatalf("ANSI wrapped rows mismatch: %#v plain=%#v", rows, plain)
	}
}

func TestTerminalFormatterUsesNarrowWidth(t *testing.T) {
	line := strings.TrimSpace(formatAssistantTerminal("---", false, 30))
	if len([]rune(line)) != 30 {
		t.Fatalf("horizontal rule should use actual narrow width, got len=%d line=%q", len([]rune(line)), line)
	}
}

func TestTerminalTranscriptLimit(t *testing.T) {
	terminalTranscript.Lock()
	previous := terminalTranscript.entries
	terminalTranscript.entries = nil
	terminalTranscript.Unlock()
	defer func() {
		terminalTranscript.Lock()
		terminalTranscript.entries = previous
		terminalTranscript.Unlock()
	}()

	for i := 0; i < terminalTranscriptLimit+5; i++ {
		terminalRecordAssistantText("entry")
	}
	got := terminalTranscriptSnapshot()
	if len(got) != terminalTranscriptLimit {
		t.Fatalf("expected transcript capped at %d entries, got %d", terminalTranscriptLimit, len(got))
	}
}
