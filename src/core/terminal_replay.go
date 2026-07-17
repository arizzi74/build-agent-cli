package core

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

type terminalTranscriptEntry struct {
	Role       string
	Title      string
	Text       string
	Persistent bool
}

var terminalTranscript = struct {
	sync.Mutex
	entries           []terminalTranscriptEntry
	persistentEntries []terminalTranscriptEntry
	activeAssistant   string
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

func terminalRecordSystemTextAndAppend(title, text string, status statusBarState) bool {
	return terminalRecordEntryAndAppend(terminalTranscriptEntry{Role: "system", Title: title, Text: text}, status)
}

func terminalRecordPersistentWarningTextAndAppend(title, text string, status statusBarState) bool {
	entry := terminalTranscriptEntry{Role: "warning", Title: strings.TrimSpace(title), Text: strings.TrimSpace(text), Persistent: true}
	if entry.Title == "" && entry.Text == "" {
		return false
	}
	terminalTranscript.Lock()
	replaced := false
	for i := range terminalTranscript.persistentEntries {
		if terminalTranscript.persistentEntries[i].Role == entry.Role && terminalTranscript.persistentEntries[i].Title == entry.Title {
			terminalTranscript.persistentEntries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		terminalTranscript.persistentEntries = append(terminalTranscript.persistentEntries, entry)
	}
	active := terminalTranscript.entries[:0]
	for _, existing := range terminalTranscript.entries {
		if existing.Persistent && existing.Role == entry.Role && existing.Title == entry.Title {
			continue
		}
		active = append(active, existing)
	}
	terminalTranscript.entries = active
	prior := len(terminalTranscript.entries)
	terminalTranscript.entries = append(terminalTranscript.entries, entry)
	terminalTrimEntriesLocked()
	terminalTranscript.Unlock()
	return terminalAppendRecordSnapshot(terminalRecordSnapshot{Entry: entry, Text: entry.Text, Title: entry.Title, PriorEntries: prior}, status)
}

// terminalRecordRuntimeErrorAndAppend keeps application diagnostics inside the
// managed transcript while the interactive footer owns stderr. Renderer escape
// sequences intentionally continue to write directly to stderr.
func terminalRecordRuntimeErrorAndAppend(text string, status statusBarState) bool {
	return terminalRecordEntryAndAppend(terminalTranscriptEntry{Role: "error", Title: "Error", Text: text}, status)
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

func terminalRecordToolWarningAndAppend(name string, status statusBarState) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	return terminalRecordEntryAndAppend(terminalTranscriptEntry{Role: "tool_warning", Text: name}, status)
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
	terminalTranscript.entries = append(terminalTranscript.entries, terminalTranscript.persistentEntries...)
	terminalTrimEntriesLocked()
	changed := title != "" || len(messages) > 0 || len(terminalTranscript.persistentEntries) > 0
	terminalTranscript.Unlock()
	return changed
}

func terminalRecordEntry(entry terminalTranscriptEntry) {
	_ = terminalRecordEntrySnapshot(entry)
}

func terminalRecordEntryAndAppend(entry terminalTranscriptEntry, status statusBarState) bool {
	return terminalAppendRecordSnapshot(terminalRecordEntrySnapshot(entry), status)
}

func terminalAppendRecordSnapshot(snapshot terminalRecordSnapshot, status statusBarState) bool {
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
	terminalTrimEntriesLocked()
	terminalTranscript.Unlock()
	return terminalRecordSnapshot{Entry: entry, Text: entry.Text, Title: entry.Title, PriorEntries: prior}
}

func terminalTrimEntriesLocked() {
	extra := len(terminalTranscript.entries) - terminalTranscriptLimit
	if extra <= 0 {
		return
	}
	kept := terminalTranscript.entries[:0]
	for _, entry := range terminalTranscript.entries {
		if extra > 0 && !entry.Persistent {
			extra--
			continue
		}
		kept = append(kept, entry)
	}
	if extra > 0 {
		copy(kept, kept[extra:])
		kept = kept[:len(kept)-extra]
	}
	terminalTranscript.entries = kept
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
	terminalRenderMu.Lock()
	defer terminalRenderMu.Unlock()
	beginTerminalFrame()
	defer endTerminalFrame()
	return terminalReplayManagedViewportUnlocked(status)
}

func terminalReplayManagedViewportUnlocked(status statusBarState) (terminalFooterMetrics, bool) {
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return terminalFooterMetrics{}, false
	}
	rows := terminalReplayRows(metrics.Width)
	rows = terminalReplayTail(rows, metrics.ScrollBottom)

	// Rebuild the visible managed viewport from transcript state instead of trying
	// to preserve stale pixels. This row-in-place path avoids full-screen erases so
	// routine stream/menu/final redraws do not push repeated frames into terminals
	// that expose alternate-screen history. Disable delayed autowrap while writing
	// exact-width transcript rows: otherwise iTerm2 may consume the next cursor
	// address as a wrap/scroll, which smears a prompt background into later rows.
	fmt.Fprint(os.Stderr, "\x1b[s\x1b[?7l")
	for row := 1; row <= metrics.TempRow; row++ {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H%s", row, ansiEraseLine)
	}
	for i, row := range rows {
		if i+1 > metrics.ScrollBottom {
			break
		}
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H%s", i+1, row)
	}
	fmt.Fprint(os.Stderr, "\x1b[?7h\x1b[u")
	lastTerminalFooterMetrics.set = false
	metrics, ok = activateTerminalFooter(status)
	return metrics, ok
}

func terminalAppendRowsToScrollback(rows []string, status statusBarState) bool {
	if len(rows) == 0 {
		return true
	}
	terminalRenderMu.Lock()
	defer terminalRenderMu.Unlock()
	// Modal pickers own the whole physical screen. The semantic entry has already
	// been recorded above; leave it untouched here and let picker teardown replay
	// the managed transcript once the overlay is gone.
	if deferTerminalReplayWhilePickerActive() {
		return true
	}
	beginTerminalFrame()
	defer endTerminalFrame()
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return false
	}
	// opencode-style commit: append only new stable rows to the terminal's real
	// scrollback, instead of replaying the whole semantic transcript. The fixed
	// footer owns mutable UI; committed transcript rows flow through the scroll
	// region naturally. Keep DECAWM off for the transaction: a full-width gray
	// prompt row otherwise leaves iTerm2 in pending-wrap and its CR/LF can scroll
	// twice, producing repeated/corrupted background bands.
	fmt.Fprintf(os.Stderr, "\x1b[?7l\x1b[1;%dr", metrics.ScrollBottom)
	for _, row := range rows {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H%s%s\r\n", metrics.ScrollBottom, ansiEraseLine, row)
	}
	fmt.Fprint(os.Stderr, "\x1b[?7h")
	lastTerminalFooterMetrics.set = false
	if _, ok := activateTerminalFooter(status); ok {
		if !redrawTerminalActivePromptUnlocked() {
			redrawPendingFooterPromptFromStateUnlocked()
		}
	}
	return true
}

func terminalReplayManagedViewportWithScrollback(status statusBarState) (terminalFooterMetrics, bool) {
	terminalRenderMu.Lock()
	defer terminalRenderMu.Unlock()
	beginTerminalFrame()
	defer endTerminalFrame()
	return terminalReplayManagedViewportWithScrollbackUnlocked(status)
}

func terminalReplayManagedViewportWithScrollbackUnlocked(status statusBarState) (terminalFooterMetrics, bool) {
	// Semantic content may already exist when this path is reached (for example
	// after resize). Do not use ED2/ED3 here: those are intentionally reserved
	// for blank-screen transitions, before the first transcript row. Replaying
	// the current viewport row-by-row preserves the transcript and keeps iTerm2
	// alternate-screen history free of destructive repaint frames.
	return terminalReplayManagedViewportUnlocked(status)
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
	case "error":
		title := entry.Title
		if title == "" {
			title = "Error"
		}
		return style(title, ansiRed+ansiBold, color) + "\n" + style(strings.TrimSpace(entry.Text), ansiRed, color)
	case "warning":
		title := entry.Title
		if title == "" {
			title = "Warning"
		}
		body := strings.TrimRight(formatAssistantTerminal(entry.Text, color, width), "\n")
		if body == "" {
			return style(title, ansiYellow+ansiBold, color)
		}
		return style(title, ansiYellow+ansiBold, color) + "\n" + body
	case "tool_success":
		return formatToolResultTerminal(entry.Text, true, color)
	case "tool_error":
		return formatToolResultTerminal(entry.Text, false, color)
	case "tool_warning":
		return formatToolWarningTerminal(entry.Text, color)
	default:
		return strings.TrimSpace(entry.Text)
	}
}

func formatToolResultTerminal(name string, success bool, color bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	lines := strings.Split(name, "\n")
	toolName := strings.TrimSpace(lines[0])
	if toolName == "" {
		toolName = "tool"
	}
	marker := "✗"
	code := ansiRed + ansiBold
	if success {
		marker = "✓"
		code = ansiWasabiGreen
	}
	out := style(marker+" "+toolName, code, color)
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out += "\n" + style("  "+line, ansiGrayFG, color)
	}
	return out
}

func formatToolWarningTerminal(name string, color bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	lines := strings.Split(name, "\n")
	toolName := strings.TrimSpace(lines[0])
	if toolName == "" {
		toolName = "tool"
	}
	out := style("⚠ "+toolName, ansiYellow+ansiBold, color)
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out += "\n" + style("  "+line, ansiYellow, color)
	}
	return out
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
