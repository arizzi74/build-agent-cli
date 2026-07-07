package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/term"
)

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiBlue    = "\x1b[34m"
	ansiRed     = "\x1b[31m"
	ansiCyan    = "\x1b[36m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiMagenta = "\x1b[35m"
	ansiGrayFG  = "\x1b[38;5;250m"
	ansiUserBG  = "\x1b[48;5;236m"
	// Bright wasabi-ish green for transient status text.
	ansiWasabiGreen = "\x1b[1;38;5;118m"
)

func printAssistantText(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if terminalRecordAssistantTextAndAppend(text, lastKnownStatusBarState()) {
		return
	}
	color := terminalANSIEnabled()
	if interactiveTerminalUIEnabled() {
		prepareTerminalScrollbackOutput()
	}
	formatted := formatAssistantResponseTerminal(text, color, terminalOutputWidth())
	if color {
		fmt.Println()
	}
	fmt.Print(formatted)
	if !strings.HasSuffix(formatted, "\n") {
		fmt.Println()
	}
	if color {
		fmt.Println()
	}
	redrawPendingFooterPromptFromState()
}

func terminalLiveStreamEnabled() bool {
	// Real TTYs use the retained live Markdown repaint path by default. The
	// finalized assistant text is still recorded separately for resize replay.
	// Set BA_CLI_LIVE_REPAINT=0 to force append-only/plain streaming locally.
	if os.Getenv("BA_CLI_LIVE_REPAINT") == "0" {
		return false
	}
	return terminalANSIEnabled()
}

func renderAssistantStreamLive(text string, previousRows int) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return previousRows
	}
	width := terminalOutputWidth()
	color := terminalANSIEnabled()
	formatted := formatAssistantResponseTerminal(text, color, width)
	if previousRows > 0 {
		fmt.Printf("\x1b[%dA\r\x1b[J", previousRows)
	}
	fmt.Print(formatted)
	redrawPendingFooterPromptFromState()
	return terminalRows(formatted, width)
}

func printConversationHistoryScrollback(title string, history []interface{}) {
	printConversationHistoryScrollbackWithStatus(title, history, statusBarState{})
}

func printConversationHistoryScrollbackWithStatus(title string, history []interface{}, status statusBarState) {
	terminalSetConversationHistory(title, history)
	if replayConversationHistoryFromTranscript(status) {
		return
	}
	formatted := formatConversationHistoryScrollback(title, history, terminalANSIEnabled(), terminalOutputWidth())
	if strings.TrimSpace(formatted) == "" {
		return
	}
	fmt.Print(formatted)
	if !strings.HasSuffix(formatted, "\n") {
		fmt.Println()
	}
}

func replayConversationHistoryFromTranscript(status statusBarState) bool {
	if !interactiveTerminalUIEnabled() {
		return false
	}
	lastTerminalFooterStatus.Lock()
	footerActive := lastTerminalFooterStatus.set
	lastTerminalFooterStatus.Unlock()
	if !footerActive {
		return false
	}
	_, replayed := terminalReplayManagedViewportWithScrollback(status)
	if replayed {
		redrawPendingFooterPromptFromState()
	}
	return replayed
}

type terminalHistoryMessage struct {
	Role    string
	Content string
}

func formatConversationHistoryScrollback(title string, history []interface{}, color bool, width int) string {
	messages := collectTerminalHistoryMessages(history)
	if len(messages) == 0 {
		return ""
	}
	if width < 50 {
		width = 100
	}
	if width > 140 {
		width = 140
	}
	var out strings.Builder
	writeLine(&out, style(centeredDivider("conversation history", width), ansiDim, color))
	if title = strings.TrimSpace(title); title != "" {
		writeLine(&out, style("Conversation: "+title, ansiDim, color))
		writeBlankLine(&out)
	}
	for i, msg := range messages {
		if i > 0 {
			if color {
				writeBlankLines(&out, 2)
			} else {
				writeBlankLine(&out)
			}
		}
		switch msg.Role {
		case "user":
			if color && i > 0 {
				writeBlankLine(&out)
			}
			out.WriteString(formatUserPromptBlock(msg.Content, color, width))
		case "assistant":
			if color && i > 0 {
				writeBlankLine(&out)
			}
			out.WriteString(formatAssistantResponseTerminal(msg.Content, color, width))
		case "system":
			if color {
				writeBlankLine(&out)
			}
			writeLine(&out, style("System", ansiBold, color))
			if color {
				writeBlankLine(&out)
			}
			out.WriteString(formatAssistantResponseTerminal(msg.Content, color, width))
		}
	}
	writeBlankLine(&out)
	writeLine(&out, style(centeredDivider("end history", width), ansiDim, color))
	return strings.TrimRight(out.String(), "\n") + "\n"
}

func collectTerminalHistoryMessages(history []interface{}) []terminalHistoryMessage {
	out := make([]terminalHistoryMessage, 0, len(history))
	for _, raw := range history {
		msg := asMap(raw)
		if msg == nil {
			continue
		}
		role := terminalHistoryRole(firstString(msg, "role", "author", "sender", "type"))
		text := strings.TrimSpace(firstString(msg, "content", "text", "message"))
		if body := asMap(msg["body"]); body != nil {
			if role == "" {
				role = terminalHistoryRole(firstString(body, "author_type", "author_id", "role", "sender", "type"))
			}
			if text == "" {
				text = strings.TrimSpace(firstString(body, "text", "message", "content", "label"))
			}
		}
		if role == "" || text == "" {
			continue
		}
		out = append(out, terminalHistoryMessage{Role: role, Content: text})
	}
	return out
}

func terminalHistoryRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	switch role {
	case "user", "system":
		return role
	case "assistant":
		return "assistant"
	case "error":
		return "assistant"
	case "assistant-thinking", "thinking", "assistant-tool", "tool", "remote_tool", "loading":
		return ""
	default:
		if strings.Contains(role, "tool") || strings.Contains(role, "thinking") || strings.Contains(role, "loading") {
			return ""
		}
		if strings.Contains(role, "assistant") {
			return "assistant"
		}
		return ""
	}
}

func formatUserTerminal(input string, color bool) string {
	input = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(input, "\r\n", "\n"), "\r", "\n"))
	if input == "" {
		return ""
	}
	var out strings.Builder
	for _, line := range strings.Split(input, "\n") {
		if !color {
			writeLine(&out, style("› ", ansiDim, color)+renderInline(strings.TrimRight(line, " \t"), color))
			continue
		}
		text := "  " + strings.TrimRight(line, " \t") + "  "
		text = ansiUserBG + ansiGrayFG + text + ansiReset
		writeLine(&out, text)
	}
	return out.String()
}

func printLiveUserPrompt(input string) {
	if terminalRecordUserPromptAndAppend(input, lastKnownStatusBarState()) {
		return
	}
	formatted := formatLiveUserPromptTerminal(input, terminalANSIEnabled(), terminalOutputWidth())
	if strings.TrimSpace(stripANSI(formatted)) == "" {
		return
	}
	prepareTerminalScrollbackOutput()
	fmt.Print(formatted)
	redrawPendingFooterPromptFromState()
}

func formatLiveUserPromptTerminal(input string, color bool, width int) string {
	input = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(input, "\r\n", "\n"), "\r", "\n"))
	if input == "" {
		return ""
	}
	if width <= 0 {
		width = 100
	}
	var out strings.Builder
	if color {
		out.WriteByte('\n')
	}
	out.WriteString(formatUserPromptBlock(input, color, width))
	writeBlankLine(&out)
	return out.String()
}

func formatUserPromptBlock(input string, color bool, width int) string {
	input = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(input, "\r\n", "\n"), "\r", "\n"))
	if input == "" {
		return ""
	}
	if !color {
		return formatUserTerminal(input, false)
	}
	if width <= 0 {
		width = 100
	}
	var out strings.Builder
	for i, line := range strings.Split(input, "\n") {
		if i > 0 {
			writeBlankLine(&out)
		}
		middle := " › " + strings.TrimRight(line, " \t")
		writeLine(&out, ansiUserBG+fitPromptBandText("", width)+ansiReset)
		writeLine(&out, ansiUserBG+ansiGrayFG+fitPromptBandText(middle, width)+ansiReset)
		writeLine(&out, ansiUserBG+fitPromptBandText("", width)+ansiReset)
	}
	return out.String()
}

func formatAssistantResponseTerminal(input string, color bool, width int) string {
	contentWidth := width
	if color && contentWidth > 2 {
		// Colored interactive responses add a two-column bullet/indent prefix. Render
		// Markdown, especially tables, inside the remaining content width so the
		// prefix does not push table borders past the terminal edge and trigger
		// terminal auto-wrap.
		contentWidth -= 2
	}
	formatted := formatAssistantTerminal(input, color, contentWidth)
	if !color {
		return formatted
	}
	return bulletTerminalBlock(formatted, color)
}

func bulletTerminalBlock(formatted string, color bool) string {
	formatted = strings.TrimRight(formatted, "\n")
	if formatted == "" {
		return ""
	}
	lines := strings.Split(formatted, "\n")
	bullet := style("• ", ansiWasabiGreen, color)
	var out strings.Builder
	for i, line := range lines {
		if i == 0 {
			writeLine(&out, bullet+line)
			continue
		}
		writeLine(&out, "  "+line)
	}
	return out.String()
}

func centeredDivider(label string, width int) string {
	label = " " + strings.TrimSpace(label) + " "
	if width < len(label)+2 {
		return strings.Repeat("─", maxInt(width, 1))
	}
	left := (width - len(label)) / 2
	right := width - len(label) - left
	return strings.Repeat("─", left) + label + strings.Repeat("─", right)
}

func terminalANSIEnabled() bool {
	if os.Getenv("NO_COLOR") != "" || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func terminalStatusANSIEnabled() bool {
	if os.Getenv("NO_COLOR") != "" || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}

func interactiveTerminalUIEnabled() bool {
	return terminalStatusANSIEnabled() && term.IsTerminal(int(os.Stdin.Fd()))
}

func terminalStatusWidth() int {
	width, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || width < 50 {
		return 100
	}
	if width > 160 {
		return 160
	}
	return width
}

type terminalFooterMetrics struct {
	Width        int
	Height       int
	ScrollBottom int
	TurnTop      int
	TurnRow      int
	TurnBottom   int
	PromptTop    int
	PromptRow    int
	PromptBottom int
	StatusRow    int
	TempRow      int
}

var lastTerminalFooterMetrics struct {
	set     bool
	metrics terminalFooterMetrics
}

var lastTerminalFooterStatus struct {
	sync.Mutex
	set   bool
	state statusBarState
}

var terminalFooterTempStatus struct {
	sync.Mutex
	message string
	code    string
	expires time.Time
	seq     uint64
}

func terminalFooterMetricsForTTY() (terminalFooterMetrics, bool) {
	if !interactiveTerminalUIEnabled() {
		return terminalFooterMetrics{}, false
	}
	width, height, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || width < 20 || height < 9 {
		return terminalFooterMetrics{}, false
	}
	promptTop := height - 4
	return terminalFooterMetrics{
		Width:        width,
		Height:       height,
		ScrollBottom: height - 8,
		TurnTop:      height - 7,
		TurnRow:      height - 6,
		TurnBottom:   height - 5,
		PromptTop:    promptTop,
		PromptRow:    promptTop + 1,
		PromptBottom: promptTop + 2,
		StatusRow:    height - 1,
		TempRow:      height,
	}, true
}

func activateTerminalFooter(status statusBarState) (terminalFooterMetrics, bool) {
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return terminalFooterMetrics{}, false
	}
	clearStaleFooterAfterResize(metrics)
	// Keep the working spacer/status/spacer, 3-row prompt band, status row, and
	// bottom temporary-message row outside the scrolling region. Normal output then scrolls above the
	// footer instead of overwriting it. DECSTBM resets the cursor to the home
	// position in many terminals, so save/restore around both scroll-region setup
	// and status redraw; otherwise the next assistant output can start at row 1.
	fmt.Fprintf(os.Stderr, "\x1b[s\x1b[1;%dr", metrics.ScrollBottom)
	drawTerminalFooterStatusLine(metrics, status)
	message, code := currentTerminalFooterTempMessage()
	drawTerminalFooterTempLine(metrics, message, code)
	fmt.Fprint(os.Stderr, "\x1b[u")
	lastTerminalFooterMetrics.set = true
	lastTerminalFooterMetrics.metrics = metrics
	lastTerminalFooterStatus.Lock()
	lastTerminalFooterStatus.set = true
	lastTerminalFooterStatus.state = status
	lastTerminalFooterStatus.Unlock()
	clearTerminalAppScrollback()
	return metrics, true
}

func clearStaleFooterAfterResize(metrics terminalFooterMetrics) {
	if !lastTerminalFooterMetrics.set {
		return
	}
	old := lastTerminalFooterMetrics.metrics
	if old.Height == metrics.Height && old.Width == metrics.Width && old.StatusRow == metrics.StatusRow && old.TempRow == metrics.TempRow {
		return
	}
	top := minInt(old.ScrollBottom, metrics.ScrollBottom)
	bottom := maxInt(old.TempRow, metrics.TempRow)
	if top <= 0 {
		top = 1
	}
	if bottom <= 0 {
		bottom = metrics.StatusRow
	}
	fmt.Fprint(os.Stderr, "\x1b[s")
	for row := top; row <= bottom; row++ {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K", row)
	}
	fmt.Fprint(os.Stderr, "\x1b[u")
}

func drawTerminalFooterStatus(status statusBarState) bool {
	metrics, ok := activateTerminalFooter(status)
	if !ok {
		return false
	}
	_ = metrics
	return true
}

func drawTerminalFooterStatusAt(metrics terminalFooterMetrics, status statusBarState) {
	fmt.Fprint(os.Stderr, "\x1b[s")
	drawTerminalFooterStatusLine(metrics, status)
	fmt.Fprint(os.Stderr, "\x1b[u")
}

func drawTerminalFooterStatusLine(metrics terminalFooterMetrics, status statusBarState) {
	line := formatStatusBar(status, true, metrics.Width)
	fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K%s", metrics.StatusRow, line)
}

func drawTerminalFooterTempLine(metrics terminalFooterMetrics, message, code string) {
	line := fitStatusBarText(" "+strings.TrimSpace(message)+" ", metrics.Width)
	if strings.TrimSpace(message) == "" {
		line = strings.Repeat(" ", maxInt(metrics.Width, 0))
	} else if code != "" {
		line = style(line, code, true)
	}
	fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K%s", metrics.TempRow, line)
}

func currentTerminalFooterTempMessage() (string, string) {
	terminalFooterTempStatus.Lock()
	defer terminalFooterTempStatus.Unlock()
	if strings.TrimSpace(terminalFooterTempStatus.message) == "" {
		return "", ""
	}
	if !terminalFooterTempStatus.expires.IsZero() && time.Now().After(terminalFooterTempStatus.expires) {
		terminalFooterTempStatus.message = ""
		terminalFooterTempStatus.code = ""
		terminalFooterTempStatus.expires = time.Time{}
		return "", ""
	}
	return terminalFooterTempStatus.message, terminalFooterTempStatus.code
}

func showTerminalFooterTempMessage(status statusBarState, message string, duration time.Duration) bool {
	return showTerminalFooterTempMessageWithStyle(status, message, duration, ansiYellow+ansiBold)
}

func showTerminalFooterTempMessageWithStyle(status statusBarState, message string, duration time.Duration, code string) bool {
	message = strings.TrimSpace(message)
	if message == "" || !interactiveTerminalUIEnabled() {
		return false
	}
	expires := time.Time{}
	if duration > 0 {
		expires = time.Now().Add(duration)
	}
	terminalFooterTempStatus.Lock()
	terminalFooterTempStatus.seq++
	seq := terminalFooterTempStatus.seq
	terminalFooterTempStatus.message = message
	terminalFooterTempStatus.code = code
	terminalFooterTempStatus.expires = expires
	terminalFooterTempStatus.Unlock()
	metrics, ok := activateTerminalFooter(status)
	if ok {
		fmt.Fprint(os.Stderr, "\x1b[s")
		drawTerminalFooterTempLine(metrics, message, code)
		fmt.Fprint(os.Stderr, "\x1b[u")
	}
	if duration > 0 {
		time.AfterFunc(duration, func() {
			terminalFooterTempStatus.Lock()
			if terminalFooterTempStatus.seq != seq {
				terminalFooterTempStatus.Unlock()
				return
			}
			terminalFooterTempStatus.message = ""
			terminalFooterTempStatus.code = ""
			terminalFooterTempStatus.expires = time.Time{}
			terminalFooterTempStatus.Unlock()
			metrics, ok := terminalFooterMetricsForTTY()
			if !ok {
				return
			}
			fmt.Fprint(os.Stderr, "\x1b[s")
			drawTerminalFooterTempLine(metrics, "", "")
			fmt.Fprint(os.Stderr, "\x1b[u")
		})
	}
	return ok
}

func clearTerminalFooterTempMessage() {
	terminalFooterTempStatus.Lock()
	terminalFooterTempStatus.seq++
	terminalFooterTempStatus.message = ""
	terminalFooterTempStatus.code = ""
	terminalFooterTempStatus.expires = time.Time{}
	terminalFooterTempStatus.Unlock()
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return
	}
	fmt.Fprint(os.Stderr, "\x1b[s")
	drawTerminalFooterTempLine(metrics, "", "")
	fmt.Fprint(os.Stderr, "\x1b[u")
}

func replayManagedViewportFromLastStatus() bool {
	return replayManagedViewportFromLastStatusWith(false)
}

func lastKnownStatusBarState() statusBarState {
	lastTerminalFooterStatus.Lock()
	defer lastTerminalFooterStatus.Unlock()
	return lastTerminalFooterStatus.state
}

func replayManagedViewportWithScrollbackFromLastStatus() bool {
	return replayManagedViewportFromLastStatusWith(true)
}

func replayManagedViewportFromLastStatusWith(rebuildScrollback bool) bool {
	if !interactiveTerminalUIEnabled() {
		return false
	}
	lastTerminalFooterStatus.Lock()
	status, ok := lastTerminalFooterStatus.state, lastTerminalFooterStatus.set
	lastTerminalFooterStatus.Unlock()
	if !ok {
		return false
	}
	var replayed bool
	if rebuildScrollback {
		_, replayed = terminalReplayManagedViewportWithScrollback(status)
	} else {
		_, replayed = terminalReplayManagedViewport(status)
	}
	if replayed {
		redrawPendingFooterPromptFromState()
	}
	return replayed
}

func redrawPendingFooterPromptFromState() bool {
	if !interactiveTerminalUIEnabled() {
		return false
	}
	line, _ := peekPendingCommandInput()
	lastTerminalFooterStatus.Lock()
	status, ok := lastTerminalFooterStatus.state, lastTerminalFooterStatus.set
	lastTerminalFooterStatus.Unlock()
	if !ok {
		return placeTerminalFooterPromptCursor("ba> ", len([]rune(line)))
	}
	return drawTerminalFooterPrompt("ba> ", line, len([]rune(line)), status)
}

func placeTerminalFooterPromptCursor(prompt string, cursor int) bool {
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return false
	}
	col := commandInputCursorColumn(prompt, cursor)
	if col > metrics.Width {
		col = metrics.Width
	}
	fmt.Fprintf(os.Stderr, "\x1b[%d;%dH", metrics.PromptRow, col)
	return true
}

func prepareTerminalScrollbackOutput() bool {
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return false
	}
	// Position normal stdout output at the bottom of the active scroll region.
	// This keeps transcript turns above the reserved footer instead of letting
	// terminal state from the prompt/status rows leak into assistant output.
	fmt.Fprintf(os.Stderr, "\x1b[1;%dr\x1b[%d;1H\x1b[2K", metrics.ScrollBottom, metrics.ScrollBottom)
	return true
}

func drawTerminalFooterWorkingLine(text string, status statusBarState) bool {
	metrics, ok := activateTerminalFooter(status)
	if !ok {
		return false
	}
	fmt.Fprintf(os.Stderr, "\x1b[s")
	fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K", metrics.TurnTop)
	fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K%s", metrics.TurnRow, fitPromptLine(text, metrics.Width))
	fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K", metrics.TurnBottom)
	fmt.Fprintf(os.Stderr, "\x1b[u")
	return true
}

func clearTerminalFooterWorkingLine(status statusBarState) bool {
	metrics, ok := activateTerminalFooter(status)
	if !ok {
		return false
	}
	fmt.Fprintf(os.Stderr, "\x1b[s")
	for row := metrics.TurnTop; row <= metrics.TurnBottom; row++ {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K", row)
	}
	fmt.Fprintf(os.Stderr, "\x1b[u")
	return true
}

func restoreTerminalFooter() {
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return
	}
	fmt.Fprint(os.Stderr, "\x1b[r")
	lastTerminalFooterMetrics.set = false
	lastTerminalFooterStatus.Lock()
	lastTerminalFooterStatus.set = false
	lastTerminalFooterStatus.Unlock()
	for row := metrics.TurnTop; row <= metrics.TempRow; row++ {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K", row)
	}
	fmt.Fprintf(os.Stderr, "\x1b[%d;1H", metrics.TempRow)
}

type statusBarState struct {
	Model         string
	InputMessages int
	InputTokens   int64
	OutputTokens  int64
	Instance      string
}

func formatStatusBar(state statusBarState, color bool, width int) string {
	model := strings.TrimSpace(state.Model)
	if model == "" {
		model = "unknown"
	}
	instance := compactStatusInstance(state.Instance)
	if instance == "" {
		instance = "unknown-instance"
	}
	text := fmt.Sprintf(" model=%s  input_messages=%d  input_tokens=%d  output_tokens=%d  instance=%s ", model, state.InputMessages, state.InputTokens, state.OutputTokens, instance)
	if !color {
		return strings.TrimSpace(text)
	}
	if width > 0 && runeLen(text) > width {
		return style(fitStatusBarText(text, width), ansiCyan+ansiBold, true)
	}
	segments := []statusBarSegment{
		{" model=", ansiDim}, {model, ansiWasabiGreen},
		{"  input_messages=", ansiDim}, {fmt.Sprintf("%d", state.InputMessages), ansiYellow + ansiBold},
		{"  input_tokens=", ansiDim}, {fmt.Sprintf("%d", state.InputTokens), ansiBlue + ansiBold},
		{"  output_tokens=", ansiDim}, {fmt.Sprintf("%d", state.OutputTokens), ansiMagenta + ansiBold},
		{"  instance=", ansiDim}, {instance, ansiGreen + ansiBold},
		{" ", ""},
	}
	return colorStatusSegments(segments, width)
}

type statusBarSegment struct {
	Text string
	Code string
}

func colorStatusSegments(segments []statusBarSegment, width int) string {
	var out strings.Builder
	plainLen := 0
	for _, segment := range segments {
		out.WriteString(style(segment.Text, segment.Code, segment.Code != ""))
		plainLen += runeLen(segment.Text)
	}
	if width > plainLen {
		out.WriteString(strings.Repeat(" ", width-plainLen))
	}
	return out.String()
}

func compactStatusInstance(raw string) string {
	instance := strings.TrimSpace(raw)
	if instance == "" {
		return ""
	}
	if !strings.Contains(instance, "://") {
		instance = "https://" + instance
	}
	if u, err := url.Parse(instance); err == nil && u.Host != "" {
		return u.Hostname()
	}
	instance = strings.TrimRight(strings.TrimSpace(raw), "/")
	instance = strings.TrimPrefix(instance, "https://")
	instance = strings.TrimPrefix(instance, "http://")
	if slash := strings.Index(instance, "/"); slash >= 0 {
		instance = instance[:slash]
	}
	if host, _, ok := strings.Cut(instance, ":"); ok {
		return host
	}
	return instance
}

func fitStatusBarText(text string, width int) string {
	if width <= 0 {
		return strings.TrimSpace(text)
	}
	runes := []rune(stripANSI(text))
	if len(runes) > width {
		if width == 1 {
			return "…"
		}
		return string(runes[:width-1]) + "…"
	}
	return string(runes) + strings.Repeat(" ", width-len(runes))
}

func workingStatusText(color bool) string {
	return style("Working ...", ansiWasabiGreen, color)
}

func animatedWorkingStatus(frame int) string {
	text := []rune("Working ...")
	if len(text) == 0 {
		return ""
	}
	span := len(text)*2 - 2
	if span <= 0 {
		span = 1
	}
	pos := frame % span
	if pos >= len(text) {
		pos = span - pos
	}
	colors := []int{28, 34, 40, 46, 82, 118, 154, 190}
	var out strings.Builder
	for i, r := range text {
		dist := absInt(i - pos)
		level := len(colors) - 1 - minInt(dist, len(colors)-1)
		fmt.Fprintf(&out, "\x1b[1;38;5;%dm%c%s", colors[level], r, ansiReset)
	}
	return out.String()
}

func terminalOutputWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width < 50 {
		return 100
	}
	if width > 140 {
		return 140
	}
	return width
}

func formatAssistantTerminal(input string, color bool, width int) string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.ReplaceAll(input, "\r", "\n")
	lines := strings.Split(input, "\n")
	var out strings.Builder
	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			writeBlankLine(&out)
			i++
			continue
		}
		if lang, ok := parseCodeFence(trimmed); ok {
			var codeLines []string
			i++
			for i < len(lines) {
				if _, end := parseCodeFence(strings.TrimSpace(lines[i])); end {
					i++
					break
				}
				codeLines = append(codeLines, lines[i])
				i++
			}
			writeCodeBlock(&out, lang, codeLines, color)
			continue
		}
		if isTableStart(lines, i) {
			start := i
			i += 2
			for i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != "" {
				i++
			}
			writeMarkdownTable(&out, lines[start:i], color, width)
			continue
		}
		if heading, ok := parseHeading(trimmed); ok {
			writeLine(&out, style(heading, ansiBold, color))
			writeBlankLine(&out)
			i++
			continue
		}
		if isHorizontalRule(trimmed) {
			writeLine(&out, style(strings.Repeat("─", minInt(width, 80)), ansiDim, color))
			writeBlankLine(&out)
			i++
			continue
		}
		if prefix, body, ok := parseListItem(line); ok {
			writeLine(&out, prefix+renderInline(strings.TrimSpace(body), color))
			i++
			continue
		}
		writeLine(&out, renderInline(strings.TrimRight(line, " \t"), color))
		i++
	}
	return strings.TrimRight(out.String(), "\n") + "\n"
}

func writeLine(out *strings.Builder, line string) {
	out.WriteString(line)
	out.WriteByte('\n')
}

func writeBlankLine(out *strings.Builder) {
	s := out.String()
	if s == "" || strings.HasSuffix(s, "\n\n") {
		return
	}
	out.WriteByte('\n')
}

func writeBlankLines(out *strings.Builder, n int) {
	if n <= 0 || out.Len() == 0 {
		return
	}
	for !strings.HasSuffix(out.String(), strings.Repeat("\n", n+1)) {
		out.WriteByte('\n')
	}
}

func parseCodeFence(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "```") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "```")), true
}

func parseHeading(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	count := 0
	for count < len(trimmed) && trimmed[count] == '#' {
		count++
	}
	if count == 0 || count > 6 || count >= len(trimmed) || trimmed[count] != ' ' {
		return "", false
	}
	return renderInline(strings.TrimSpace(trimmed[count:]), false), true
}

func isHorizontalRule(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	for _, ch := range trimmed {
		if ch != '-' && ch != '_' && ch != '*' && !unicode.IsSpace(ch) {
			return false
		}
	}
	return true
}

func parseListItem(line string) (string, string, bool) {
	trimLeft := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(trimLeft)]
	if strings.HasPrefix(trimLeft, "- ") || strings.HasPrefix(trimLeft, "* ") || strings.HasPrefix(trimLeft, "+ ") {
		return indent + "• ", trimLeft[2:], true
	}
	for i := 0; i < len(trimLeft); i++ {
		if trimLeft[i] < '0' || trimLeft[i] > '9' {
			if i > 0 && i+1 < len(trimLeft) && trimLeft[i] == '.' && trimLeft[i+1] == ' ' {
				return indent + trimLeft[:i+2], trimLeft[i+2:], true
			}
			break
		}
	}
	return "", "", false
}

func writeCodeBlock(out *strings.Builder, lang string, codeLines []string, color bool) {
	label := "code"
	if strings.TrimSpace(lang) != "" {
		label = strings.TrimSpace(lang)
	}
	writeLine(out, style("╭─ "+label, ansiDim, color))
	if len(codeLines) == 0 {
		writeLine(out, style("│", ansiDim, color))
	}
	for _, line := range codeLines {
		writeLine(out, style("│ ", ansiDim, color)+highlightCodeLine(line, lang, color))
	}
	writeLine(out, style("╰", ansiDim, color))
	writeBlankLine(out)
}

func highlightCodeLine(line, lang string, color bool) string {
	if !color {
		return line
	}
	keywords := map[string]bool{
		"var": true, "let": true, "const": true, "new": true, "while": true, "for": true, "if": true, "else": true,
		"return": true, "function": true, "class": true, "try": true, "catch": true, "true": true, "false": true, "null": true,
	}
	_ = lang
	var out strings.Builder
	for i := 0; i < len(line); {
		ch := rune(line[i])
		if i+1 < len(line) && line[i] == '/' && line[i+1] == '/' {
			out.WriteString(style(line[i:], ansiDim, color))
			break
		}
		if line[i] == '\'' || line[i] == '"' || line[i] == '`' {
			quote := line[i]
			j := i + 1
			for j < len(line) {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == quote {
					j++
					break
				}
				j++
			}
			out.WriteString(style(line[i:j], ansiRed, color))
			i = j
			continue
		}
		if isIdentStart(ch) {
			j := i + 1
			for j < len(line) && isIdentPart(rune(line[j])) {
				j++
			}
			word := line[i:j]
			if keywords[word] {
				out.WriteString(style(word, ansiBlue+ansiBold, color))
			} else {
				out.WriteString(word)
			}
			i = j
			continue
		}
		out.WriteByte(line[i])
		i++
	}
	return out.String()
}

func isIdentStart(ch rune) bool {
	return unicode.IsLetter(ch) || ch == '_'
}

func isIdentPart(ch rune) bool {
	return unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_'
}

func isTableStart(lines []string, i int) bool {
	if i+1 >= len(lines) {
		return false
	}
	return strings.Contains(lines[i], "|") && isMarkdownTableSeparator(lines[i+1])
}

func isMarkdownTableSeparator(line string) bool {
	cells := splitMarkdownTableRow(line)
	if len(cells) < 2 {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		if cell == "" {
			return false
		}
		seenDash := false
		for _, ch := range cell {
			switch ch {
			case '-':
				seenDash = true
			case ':', ' ':
			default:
				return false
			}
		}
		if !seenDash {
			return false
		}
	}
	return true
}

func splitMarkdownTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func writeMarkdownTable(out *strings.Builder, lines []string, color bool, width int) {
	if len(lines) < 2 {
		return
	}
	headers := splitMarkdownTableRow(lines[0])
	var rows [][]string
	for _, line := range lines[2:] {
		cells := splitMarkdownTableRow(line)
		if len(cells) == 0 {
			continue
		}
		for len(cells) < len(headers) {
			cells = append(cells, "")
		}
		if len(cells) > len(headers) {
			cells = cells[:len(headers)]
		}
		rows = append(rows, cells)
	}
	widths := tableColumnWidths(headers, rows, width)
	writeTableBorder(out, "┌", "┬", "┐", widths, color)
	writeTableRow(out, headers, widths, color, true)
	writeTableBorder(out, "├", "┼", "┤", widths, color)
	for _, row := range rows {
		writeTableRow(out, row, widths, color, false)
	}
	writeTableBorder(out, "└", "┴", "┘", widths, color)
	writeBlankLine(out)
}

func tableColumnWidths(headers []string, rows [][]string, terminalWidth int) []int {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = maxInt(1, runeLen(renderInline(h, false)))
	}
	for _, row := range rows {
		for i, cell := range row {
			if n := runeLen(renderInline(cell, false)); n > widths[i] {
				widths[i] = n
			}
		}
	}
	if terminalWidth <= 0 || len(widths) == 0 {
		return widths
	}
	available := terminalWidth - (3*len(widths) + 1)
	if available < len(widths) {
		available = len(widths)
	}
	for sumInts(widths) > available {
		largest := 0
		for i := range widths {
			if widths[i] > widths[largest] {
				largest = i
			}
		}
		if widths[largest] <= 1 {
			break
		}
		widths[largest]--
	}
	return widths
}

func sumInts(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

func writeTableBorder(out *strings.Builder, left, mid, right string, widths []int, color bool) {
	var line strings.Builder
	line.WriteString(left)
	for i, width := range widths {
		line.WriteString(strings.Repeat("─", width+2))
		if i == len(widths)-1 {
			line.WriteString(right)
		} else {
			line.WriteString(mid)
		}
	}
	writeLine(out, style(line.String(), ansiDim, color))
}

func writeTableRow(out *strings.Builder, cells []string, widths []int, color bool, header bool) {
	wrapped := make([][]string, len(widths))
	rowHeight := 1
	for i, width := range widths {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		plain := renderInline(cell, false)
		wrapped[i] = wrapTableCell(plain, width)
		if len(wrapped[i]) > rowHeight {
			rowHeight = len(wrapped[i])
		}
	}
	for rowLine := 0; rowLine < rowHeight; rowLine++ {
		var line strings.Builder
		line.WriteString(style("│", ansiDim, color))
		for i, width := range widths {
			plain := ""
			if rowLine < len(wrapped[i]) {
				plain = wrapped[i][rowLine]
			}
			styled := renderInline(plain, color)
			if header {
				styled = style(plain, ansiBold, color)
			}
			line.WriteByte(' ')
			line.WriteString(styled)
			line.WriteString(strings.Repeat(" ", maxInt(0, width-runeLen(plain)+1)))
			line.WriteString(style("│", ansiDim, color))
		}
		writeLine(out, line.String())
	}
}

func wrapTableCell(text string, width int) []string {
	text = strings.TrimSpace(text)
	if width <= 0 {
		width = 1
	}
	if text == "" {
		return []string{""}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	current := ""
	for _, word := range words {
		for runeLen(word) > width {
			if current != "" {
				lines = append(lines, current)
				current = ""
			}
			runes := []rune(word)
			lines = append(lines, string(runes[:width]))
			word = string(runes[width:])
		}
		if current == "" {
			current = word
			continue
		}
		if runeLen(current)+1+runeLen(word) <= width {
			current += " " + word
			continue
		}
		lines = append(lines, current)
		current = word
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func renderInline(s string, color bool) string {
	s = renderMarkdownLinks(s)
	var out strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "**") {
			if end := strings.Index(s[i+2:], "**"); end >= 0 {
				inner := renderInline(s[i+2:i+2+end], color)
				out.WriteString(style(inner, ansiBold, color))
				i += 2 + end + 2
				continue
			}
		}
		if strings.HasPrefix(s[i:], "__") {
			if end := strings.Index(s[i+2:], "__"); end >= 0 {
				inner := renderInline(s[i+2:i+2+end], color)
				out.WriteString(style(inner, ansiBold, color))
				i += 2 + end + 2
				continue
			}
		}
		if s[i] == '`' {
			if end := strings.IndexByte(s[i+1:], '`'); end >= 0 {
				inner := s[i+1 : i+1+end]
				out.WriteString(style(inner, ansiCyan, color))
				i += 1 + end + 1
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func renderMarkdownLinks(s string) string {
	for {
		open := strings.Index(s, "[")
		if open < 0 {
			return s
		}
		close := strings.Index(s[open:], "](")
		if close < 0 {
			return s
		}
		close += open
		end := strings.Index(s[close+2:], ")")
		if end < 0 {
			return s
		}
		end += close + 2
		label := s[open+1 : close]
		url := s[close+2 : end]
		replacement := label
		if strings.TrimSpace(url) != "" && strings.TrimSpace(url) != strings.TrimSpace(label) {
			replacement = label + " (" + url + ")"
		}
		s = s[:open] + replacement + s[end+1:]
	}
}

func style(s, code string, enabled bool) string {
	if !enabled || s == "" {
		return s
	}
	return code + s + ansiReset
}

func runeLen(s string) int {
	return len([]rune(stripANSI(s)))
}

func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < '@' || s[i] > '~') {
				i++
			}
			if i < len(s) {
				i++
			}
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func terminalRows(s string, width int) int {
	if width <= 0 {
		width = 80
	}
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	rows := 0
	for _, line := range strings.Split(s, "\n") {
		length := runeLen(line)
		if length == 0 {
			rows++
			continue
		}
		rows += (length + width - 1) / width
	}
	return rows
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
