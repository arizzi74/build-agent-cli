package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

type terminalTranscriptEntry struct {
	Role  string
	Title string
	Text  string
}

var terminalTranscript = struct {
	sync.Mutex
	entries         []terminalTranscriptEntry
	activeAssistant string
}{entries: make([]terminalTranscriptEntry, 0, 128)}

const terminalTranscriptLimit = 200

func terminalRecordUserPrompt(text string) {
	terminalRecordEntry(terminalTranscriptEntry{Role: "user", Text: text})
}

func terminalRecordUserPromptAndAppend(text string, status statusBarState) bool {
	return terminalRecordEntryAndAppend(terminalTranscriptEntry{Role: "user", Text: text}, status)
}

func terminalRecordAssistantText(text string) {
	terminalTranscript.Lock()
	terminalTranscript.activeAssistant = ""
	terminalTranscript.Unlock()
	terminalRecordEntry(terminalTranscriptEntry{Role: "assistant", Text: text})
}

func terminalRecordAssistantTextAndAppend(text string, status statusBarState) bool {
	terminalTranscript.Lock()
	terminalTranscript.activeAssistant = ""
	terminalTranscript.Unlock()
	return terminalRecordEntryAndAppend(terminalTranscriptEntry{Role: "assistant", Text: text}, status)
}

func terminalSetActiveAssistantText(text string) {
	text = strings.TrimSpace(text)
	terminalTranscript.Lock()
	terminalTranscript.activeAssistant = text
	terminalTranscript.Unlock()
}

func terminalClearActiveAssistantText() {
	terminalTranscript.Lock()
	terminalTranscript.activeAssistant = ""
	terminalTranscript.Unlock()
}

func terminalRecordSystemText(title, text string) {
	terminalRecordEntry(terminalTranscriptEntry{Role: "system", Title: title, Text: text})
}

func terminalRecordToolResult(name string, success bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	role := "tool_error"
	if success {
		role = "tool_success"
	}
	terminalRecordEntry(terminalTranscriptEntry{Role: role, Text: name})
}

func terminalRecordToolResultAndAppend(name string, success bool, status statusBarState) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	role := "tool_error"
	if success {
		role = "tool_success"
	}
	return terminalRecordEntryAndAppend(terminalTranscriptEntry{Role: role, Text: name}, status)
}

func terminalRecordConversationHistory(title string, history []interface{}) {
	terminalSetConversationHistory(title, history)
}

func terminalSetConversationHistory(title string, history []interface{}) bool {
	messages := collectTerminalHistoryMessages(history)
	title = strings.TrimSpace(title)
	terminalTranscript.Lock()
	terminalTranscript.entries = terminalTranscript.entries[:0]
	terminalTranscript.activeAssistant = ""
	if strings.TrimSpace(title) != "" {
		terminalTranscript.entries = append(terminalTranscript.entries, terminalTranscriptEntry{Role: "system", Title: "Conversation", Text: title})
	}
	for _, msg := range messages {
		terminalTranscript.entries = append(terminalTranscript.entries, terminalTranscriptEntry{Role: msg.Role, Text: msg.Content})
	}
	if extra := len(terminalTranscript.entries) - terminalTranscriptLimit; extra > 0 {
		copy(terminalTranscript.entries, terminalTranscript.entries[extra:])
		terminalTranscript.entries = terminalTranscript.entries[:terminalTranscriptLimit]
	}
	changed := title != "" || len(messages) > 0
	terminalTranscript.Unlock()
	return changed
}

func terminalRecordEntry(entry terminalTranscriptEntry) {
	_ = terminalRecordEntrySnapshot(entry)
}

func terminalRecordEntryAndAppend(entry terminalTranscriptEntry, status statusBarState) bool {
	snapshot := terminalRecordEntrySnapshot(entry)
	if snapshot.Text == "" && snapshot.Title == "" {
		return false
	}
	rows := terminalTranscriptRows([]terminalTranscriptEntry{snapshot.Entry}, terminalANSIEnabled(), terminalOutputWidth())
	rows = wrapReplayRows(rows, terminalOutputWidth())
	if snapshot.PriorEntries > 0 && len(rows) > 0 {
		rows = append([]string{""}, rows...)
	}
	return terminalAppendRowsToScrollback(rows, status)
}

type terminalRecordSnapshot struct {
	Entry        terminalTranscriptEntry
	Text         string
	Title        string
	PriorEntries int
}

func terminalRecordEntrySnapshot(entry terminalTranscriptEntry) terminalRecordSnapshot {
	entry.Text = strings.TrimSpace(entry.Text)
	entry.Title = strings.TrimSpace(entry.Title)
	if entry.Text == "" && entry.Title == "" {
		return terminalRecordSnapshot{}
	}
	terminalTranscript.Lock()
	prior := len(terminalTranscript.entries)
	terminalTranscript.entries = append(terminalTranscript.entries, entry)
	if extra := len(terminalTranscript.entries) - terminalTranscriptLimit; extra > 0 {
		copy(terminalTranscript.entries, terminalTranscript.entries[extra:])
		terminalTranscript.entries = terminalTranscript.entries[:terminalTranscriptLimit]
	}
	terminalTranscript.Unlock()
	return terminalRecordSnapshot{Entry: entry, Text: entry.Text, Title: entry.Title, PriorEntries: prior}
}

func terminalTranscriptSnapshot() []terminalTranscriptEntry {
	terminalTranscript.Lock()
	defer terminalTranscript.Unlock()
	out := make([]terminalTranscriptEntry, len(terminalTranscript.entries), len(terminalTranscript.entries)+1)
	copy(out, terminalTranscript.entries)
	if strings.TrimSpace(terminalTranscript.activeAssistant) != "" {
		out = append(out, terminalTranscriptEntry{Role: "assistant", Text: terminalTranscript.activeAssistant})
	}
	return out
}

func terminalReplayManagedViewport(status statusBarState) (terminalFooterMetrics, bool) {
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return terminalFooterMetrics{}, false
	}
	rows := terminalReplayRows(metrics.Width)
	rows = terminalReplayTail(rows, metrics.ScrollBottom)

	// Rebuild the visible managed viewport from transcript state instead of trying
	// to preserve stale pixels. This row-in-place path avoids full-screen erases so
	// routine stream/menu/final redraws do not push repeated frames into terminals
	// that expose alternate-screen history.
	fmt.Fprint(os.Stderr, "\x1b[s")
	for row := 1; row <= metrics.TempRow; row++ {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K", row)
	}
	for i, row := range rows {
		if i+1 > metrics.ScrollBottom {
			break
		}
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H%s", i+1, row)
	}
	fmt.Fprint(os.Stderr, "\x1b[u")
	lastTerminalFooterMetrics.set = false
	metrics, ok = activateTerminalFooter(status)
	return metrics, ok
}

func terminalAppendRowsToScrollback(rows []string, status statusBarState) bool {
	if len(rows) == 0 {
		return true
	}
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return false
	}
	// opencode-style commit: append only new stable rows to the terminal's real
	// scrollback, instead of replaying the whole semantic transcript. The fixed
	// footer owns mutable UI; committed transcript rows flow through the scroll
	// region naturally.
	fmt.Fprintf(os.Stderr, "\x1b[1;%dr", metrics.ScrollBottom)
	for _, row := range rows {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K%s\r\n", metrics.ScrollBottom, row)
	}
	lastTerminalFooterMetrics.set = false
	if _, ok := activateTerminalFooter(status); ok {
		redrawPendingFooterPromptFromState()
	}
	return true
}

func terminalReplayManagedViewportWithScrollback(status statusBarState) (terminalFooterMetrics, bool) {
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return terminalFooterMetrics{}, false
	}
	if !terminalAppScreenActive() {
		return terminalReplayManagedViewport(status)
	}
	rows := terminalReplayRows(metrics.Width)
	footerRows := maxInt(metrics.Height-metrics.ScrollBottom, 0)
	// Resize can leave the terminal's alternate-screen scrollback out of sync with
	// the semantic transcript: the current visible top rows may also exist just
	// above the viewport, so scrolling up shows duplicated content. On resize only,
	// rebuild alternate-screen history from the transcript: clear the alt-screen
	// scrollback, print semantic rows once with footer padding, then redraw the
	// fixed footer. This preserves useful scrollback without retaining stale repaint
	// frames from previous sizes.
	fmt.Fprint(os.Stderr, "\x1b[r\x1b[?7l\x1b[H\x1b[2J\x1b[3J")
	total := len(rows) + footerRows
	written := 0
	for _, row := range rows {
		written++
		writeReplayScrollbackLine(row, written == total)
	}
	for i := 0; i < footerRows; i++ {
		written++
		writeReplayScrollbackLine("", written == total)
	}
	fmt.Fprint(os.Stderr, "\x1b[?7h")
	lastTerminalFooterMetrics.set = false
	metrics, ok = activateTerminalFooter(status)
	return metrics, ok
}

func writeReplayScrollbackLine(row string, last bool) {
	fmt.Fprintf(os.Stderr, "\r%s\x1b[K", row)
	if !last {
		fmt.Fprint(os.Stderr, "\r\n")
	}
}

func terminalReplayRows(width int) []string {
	entries := terminalTranscriptSnapshot()
	rows := terminalTranscriptRows(entries, terminalANSIEnabled(), width)
	return wrapReplayRows(rows, width)
}

func terminalReplayTail(rows []string, maxRows int) []string {
	if maxRows > 0 && len(rows) > maxRows {
		return rows[len(rows)-maxRows:]
	}
	return rows
}

func terminalTranscriptRows(entries []terminalTranscriptEntry, color bool, width int) []string {
	if width < 20 {
		width = 20
	}
	if width > 160 {
		width = 160
	}
	rows := make([]string, 0, len(entries)*4)
	for _, entry := range entries {
		formatted := terminalFormatTranscriptEntry(entry, color, width)
		formatted = strings.TrimRight(formatted, "\n")
		if formatted == "" {
			continue
		}
		if len(rows) > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, strings.Split(formatted, "\n")...)
	}
	return rows
}

func terminalFormatTranscriptEntry(entry terminalTranscriptEntry, color bool, width int) string {
	switch entry.Role {
	case "user":
		return strings.TrimRight(formatUserPromptBlock(entry.Text, color, width), "\n")
	case "assistant":
		return strings.TrimRight(formatAssistantResponseTerminal(entry.Text, color, width), "\n")
	case "system":
		title := entry.Title
		if title == "" {
			title = "System"
		}
		return style(title, ansiBold, color) + "\n" + strings.TrimRight(formatAssistantResponseTerminal(entry.Text, color, width), "\n")
	case "tool_success":
		return formatToolResultTerminal(entry.Text, true, color)
	case "tool_error":
		return formatToolResultTerminal(entry.Text, false, color)
	default:
		return strings.TrimSpace(entry.Text)
	}
}

func formatToolResultTerminal(name string, success bool, color bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	marker := "✗"
	code := ansiRed + ansiBold
	if success {
		marker = "✓"
		code = ansiWasabiGreen
	}
	return style(marker+" "+name, code, color)
}

func wrapReplayRows(rows []string, width int) []string {
	if width <= 0 {
		return rows
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, wrapReplayLine(row, width)...)
	}
	return out
}

func wrapReplayLine(line string, width int) []string {
	if width <= 0 || runeLen(line) <= width {
		return []string{line}
	}
	var out []string
	var chunk strings.Builder
	visible := 0
	for i := 0; i < len(line); {
		if line[i] == '\x1b' && i+1 < len(line) && line[i+1] == '[' {
			start := i
			i += 2
			for i < len(line) && (line[i] < '@' || line[i] > '~') {
				i++
			}
			if i < len(line) {
				i++
			}
			chunk.WriteString(line[start:i])
			continue
		}
		_, size := utf8.DecodeRuneInString(line[i:])
		if size <= 0 {
			size = 1
		}
		if visible >= width {
			out = append(out, chunk.String())
			chunk.Reset()
			visible = 0
		}
		chunk.WriteString(line[i : i+size])
		visible++
		i += size
	}
	out = append(out, chunk.String())
	return out
}
