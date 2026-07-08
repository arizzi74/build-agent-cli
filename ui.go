package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

var stdinState = struct {
	sync.Mutex
	reader *bufio.Reader
}{reader: bufio.NewReader(os.Stdin)}

var commandHistory = struct {
	sync.Mutex
	items []string
}{}

var pendingCommandInput = struct {
	sync.Mutex
	line      string
	submitted bool
}{}

var processingInputState = struct {
	sync.Mutex
	active bool
}{}

var terminalAppScreenState = struct {
	sync.Mutex
	active bool
}{}

func promptLine(prompt string) (string, error) {
	stdinState.Lock()
	defer stdinState.Unlock()
	fmt.Fprint(os.Stderr, prompt)
	line, err := stdinState.reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func promptPassword(prompt string) (string, error) {
	stdinState.Lock()
	defer stdinState.Unlock()
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(password)), err
	}
	line, err := stdinState.reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func promptCommandLine(prompt string, status *statusBarState) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		line, err := promptLine(prompt)
		if err == nil {
			rememberCommand(line)
		}
		return line, err
	}

	stdinState.Lock()
	defer stdinState.Unlock()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprint(os.Stderr, prompt)
		line, readErr := stdinState.reader.ReadString('\n')
		if readErr != nil && len(line) == 0 {
			return "", readErr
		}
		line = strings.TrimSpace(line)
		rememberCommand(line)
		return line, nil
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	layout := newFixedPromptLayout(status)
	defer layout.clear()

	snapshot := commandHistorySnapshot()
	historyIndex := len(snapshot)
	initialLine, initialSubmitted := takePendingCommandInput()
	line := []rune(initialLine)
	cursor := len(line)
	if initialSubmitted {
		result := strings.TrimSpace(initialLine)
		if layout.enabled {
			layout.submit(prompt, result)
		} else {
			fmt.Fprint(os.Stderr, "\r\n")
		}
		rememberCommand(result)
		return result, nil
	}
	menuOpen := false
	menu := []SlashCommandSuggestion{}
	selected := 0
	lastCtrlD := time.Time{}
	var ctrlDMu sync.Mutex

	refreshMenu := func() {
		if !menuOpen {
			menu = nil
			selected = 0
			return
		}
		menu = filterSlashSuggestions(string(line))
		if selected >= len(menu) {
			selected = len(menu) - 1
		}
		if selected < 0 {
			selected = 0
		}
	}

	redraw := func() {
		if layout.enabled {
			layout.redraw(prompt, string(line), cursor, slashMenuLines(menuOpen, menu, selected))
			return
		}
		fmt.Fprintf(os.Stderr, "\r\x1b[2K%s", commandInputLine(prompt, string(line)))
		fmt.Fprint(os.Stderr, "\x1b[J")
		lines := slashMenuLines(menuOpen, menu, selected)
		for _, menuLine := range lines {
			fmt.Fprintf(os.Stderr, "\r\n%s", menuLine)
		}
		if len(lines) > 0 {
			fmt.Fprintf(os.Stderr, "\x1b[%dA\r", len(lines))
			col := len([]rune(prompt)) + cursor
			if terminalStatusANSIEnabled() {
				col++
			}
			if col > 0 {
				fmt.Fprintf(os.Stderr, "\x1b[%dC", col)
			}
		} else if tail := len(line) - cursor; tail > 0 {
			fmt.Fprintf(os.Stderr, "\x1b[%dD", tail)
		}
	}

	if layout.enabled {
		layout.redraw(prompt, string(line), cursor, nil)
	} else {
		fmt.Fprint(os.Stderr, commandInputLine(prompt, string(line)))
	}
	resizeSignals := make(chan os.Signal, 1)
	resizeDone := make(chan struct{})
	signal.Notify(resizeSignals, syscall.SIGWINCH)
	defer func() {
		signal.Stop(resizeSignals)
		close(resizeDone)
	}()
	go func() {
		var timer *time.Timer
		var timerC <-chan time.Time
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		for {
			select {
			case <-resizeDone:
				return
			case <-resizeSignals:
				if timer == nil {
					timer = time.NewTimer(350 * time.Millisecond)
					timerC = timer.C
					continue
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(350 * time.Millisecond)
			case <-timerC:
				timerC = nil
				timer = nil
				if layout.enabled {
					redraw()
				}
			}
		}
	}()
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			return "", err
		}
		switch b := buf[0]; b {
		case '\r', '\n':
			if menuOpen && len(menu) > 0 {
				choice := slashMenuChoice(menu, selected)
				line = []rune(slashMenuInsertText(choice))
				cursor = len(line)
				menuOpen = false
				refreshMenu()
				if slashMenuEnterSubmits(choice) {
					result := strings.TrimSpace(string(line))
					if layout.enabled {
						layout.submit(prompt, result)
					} else {
						fmt.Fprint(os.Stderr, "\r\n")
					}
					rememberCommand(result)
					return result, nil
				}
				redraw()
				continue
			}
			result := strings.TrimSpace(string(line))
			if layout.enabled {
				layout.submit(prompt, result)
			} else {
				fmt.Fprint(os.Stderr, "\r\n")
			}
			rememberCommand(result)
			return result, nil
		case '\t':
			if menuOpen && len(menu) > 0 {
				choice := slashMenuChoice(menu, selected)
				line = []rune(slashMenuInsertText(choice))
				cursor = len(line)
				menuOpen = false
				refreshMenu()
				redraw()
			}
		case 3: // Ctrl-C
			if layout.enabled {
				layout.abort("^C")
			} else {
				fmt.Fprint(os.Stderr, "^C\r\n")
			}
			return "", errors.New("interrupted")
		case 4: // Ctrl-D
			now := time.Now()
			ctrlDMu.Lock()
			if !lastCtrlD.IsZero() && now.Sub(lastCtrlD) <= 2*time.Second {
				ctrlDMu.Unlock()
				if layout.enabled {
					layout.abort("")
				} else {
					fmt.Fprint(os.Stderr, "\r\n")
				}
				return "", errors.New("EOF")
			}
			lastCtrlD = now
			ctrlDMu.Unlock()
			if layout.enabled {
				layout.drawTempMessage("Press Ctrl-D again to exit ....")
			} else {
				fmt.Fprint(os.Stderr, "Press Ctrl-D again to exit ....\r")
			}
			time.AfterFunc(2*time.Second, func() {
				ctrlDMu.Lock()
				if lastCtrlD.Equal(now) {
					lastCtrlD = time.Time{}
					ctrlDMu.Unlock()
					if layout.enabled {
						layout.clearTempMessage()
					}
					return
				}
				ctrlDMu.Unlock()
			})
			continue
		case 127, 8: // Backspace
			if cursor > 0 {
				line = append(line[:cursor-1], line[cursor:]...)
				cursor--
				if menuOpen {
					if len(line) == 0 || line[0] != '/' {
						menuOpen = false
					}
					refreshMenu()
				}
				redraw()
			}
		case 27: // Escape key or escape sequence.
			seq := readPendingEscapeSequence()
			if len(seq) == 0 {
				if menuOpen {
					menuOpen = false
					line = nil
					cursor = 0
					refreshMenu()
					redraw()
				}
				continue
			}
			if seq[0] != '[' {
				if menuOpen {
					menuOpen = false
					refreshMenu()
					redraw()
				}
				continue
			}
			if len(seq) < 2 {
				continue
			}
			switch seq[1] {
			case 'A': // Up
				if menuOpen {
					if len(menu) > 0 {
						selected--
						if selected < 0 {
							selected = len(menu) - 1
						}
						redraw()
					}
					continue
				}
				if len(snapshot) > 0 && historyIndex > 0 {
					historyIndex--
					line = []rune(snapshot[historyIndex])
					cursor = len(line)
					redraw()
				}
			case 'B': // Down
				if menuOpen {
					if len(menu) > 0 {
						selected++
						if selected >= len(menu) {
							selected = 0
						}
						redraw()
					}
					continue
				}
				if historyIndex < len(snapshot)-1 {
					historyIndex++
					line = []rune(snapshot[historyIndex])
				} else {
					historyIndex = len(snapshot)
					line = nil
				}
				cursor = len(line)
				redraw()
			case 'C': // Right
				if cursor < len(line) {
					cursor++
					fmt.Fprint(os.Stderr, "\x1b[C")
				}
			case 'D': // Left
				if cursor > 0 {
					cursor--
					fmt.Fprint(os.Stderr, "\x1b[D")
				}
			}
		default:
			if b >= 32 {
				r := rune(b)
				line = append(line[:cursor], append([]rune{r}, line[cursor:]...)...)
				cursor++
				if len(line) == 1 && line[0] == '/' {
					menuOpen = true
					selected = 0
				}
				if menuOpen {
					refreshMenu()
				}
				redraw()
			}
		}
	}
}

type fixedPromptLayout struct {
	enabled       bool
	width         int
	height        int
	scrollBottom  int
	turnTop       int
	promptRow     int
	promptTop     int
	promptBottom  int
	statusRow     int
	tempRow       int
	drawnMenuRows int
	status        *statusBarState
	altScreen     bool
}

func newFixedPromptLayout(status *statusBarState) *fixedPromptLayout {
	l := &fixedPromptLayout{status: status}
	l.refresh()
	return l
}

func (l *fixedPromptLayout) refresh() {
	if l == nil || l.status == nil {
		return
	}
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return
	}
	l.enabled = true
	l.applyMetrics(metrics)

}

func (l *fixedPromptLayout) applyMetrics(metrics terminalFooterMetrics) {
	l.width = metrics.Width
	l.height = metrics.Height
	l.scrollBottom = metrics.ScrollBottom
	l.turnTop = metrics.TurnTop
	l.promptTop = metrics.PromptTop
	l.promptRow = metrics.PromptRow
	l.promptBottom = metrics.PromptBottom
	l.statusRow = metrics.StatusRow
	l.tempRow = metrics.TempRow
}

func (l *fixedPromptLayout) redraw(prompt, line string, cursor int, menuLines []string) {
	if l == nil || !l.enabled {
		return
	}
	oldWidth, oldHeight := l.width, l.height
	oldTurnTop, oldTempRow := l.turnTop, l.tempRow
	oldMenuOpen := l.drawnMenuRows > 0
	newMenuOpen := len(menuLines) > 0
	l.refresh()
	if !l.enabled {
		return
	}
	resized := oldHeight > 0 && (oldWidth != l.width || oldHeight != l.height)
	replay := resized || (oldHeight > 0 && oldMenuOpen != newMenuOpen)
	if replay {
		var metrics terminalFooterMetrics
		var ok bool
		if resized {
			metrics, ok = terminalReplayManagedViewportWithScrollback(*l.status)
		} else {
			metrics, ok = terminalReplayManagedViewport(*l.status)
		}
		if ok {
			l.applyMetrics(metrics)
		}
	} else if metrics, ok := activateTerminalFooter(*l.status); ok {
		l.applyMetrics(metrics)
	}
	if !replay && oldHeight > 0 && (oldHeight != l.height || oldTempRow != l.tempRow) {
		l.clearResizedFooterRows(oldTurnTop, oldTempRow)
	}
	menuBottom := maxInt(l.promptTop-1, 0)
	maxMenuRows := menuBottom
	if len(menuLines) > maxMenuRows {
		menuLines = menuLines[:maxMenuRows]
	}
	if !replay {
		l.clearMenu(maxInt(l.drawnMenuRows, len(menuLines)))
	}
	menuTop := l.promptTop - len(menuLines)
	for i, menuLine := range menuLines {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K%s", menuTop+i, fitPromptLine(menuLine, l.width))
	}
	l.drawPromptWithSeparator(prompt, line, cursor, len(menuLines) == 0)
	l.drawnMenuRows = len(menuLines)
}

func (l *fixedPromptLayout) clearResizedFooterRows(oldTurnTop, oldTempRow int) {
	if l == nil || !l.enabled {
		return
	}
	top := minInt(oldTurnTop, l.turnTop)
	bottom := maxInt(oldTempRow, l.tempRow)
	if top <= 0 {
		top = maxInt(l.height-8, 1)
	}
	if bottom > maxInt(oldTempRow, l.tempRow) {
		bottom = maxInt(oldTempRow, l.tempRow)
	}
	if bottom <= 0 {
		bottom = l.tempRow
	}
	fmt.Fprint(os.Stderr, "\x1b[s")
	for row := top; row <= bottom; row++ {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K", row)
	}
	fmt.Fprint(os.Stderr, "\x1b[u")
}

func (l *fixedPromptLayout) drawPrompt(prompt, line string, cursor int) {
	l.drawPromptWithSeparator(prompt, line, cursor, true)
}

func (l *fixedPromptLayout) drawPromptWithSeparator(prompt, line string, cursor int, clearSeparator bool) {
	// Keep one guaranteed blank separator between the transcript/last startup
	// output and the gray input band. Without this, text printed before the
	// footer is activated can sit directly against the prompt on first launch.
	// When a slash menu is open, that row belongs to the menu; clearing it here
	// would erase the only matching command for filtered menus such as `/w`.
	if clearSeparator && l.promptTop > 1 {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K", l.promptTop-1)
	}
	rows := commandInputBandRows(prompt, line, l.width)
	for i, rowText := range rows {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K%s", l.promptTop+i, rowText)
	}
	col := commandInputCursorColumn(prompt, cursor)
	if col > l.width {
		col = l.width
	}
	fmt.Fprintf(os.Stderr, "\x1b[%d;%dH", l.promptRow, col)
}

func (l *fixedPromptLayout) drawStatus() {
	if l.status == nil {
		return
	}
	drawTerminalFooterStatusAt(terminalFooterMetrics{Width: l.width, Height: l.height, ScrollBottom: l.scrollBottom, TurnTop: l.turnTop, TurnRow: l.turnTop + 1, TurnBottom: l.turnTop + 2, PromptTop: l.promptTop, PromptRow: l.promptRow, PromptBottom: l.promptBottom, StatusRow: l.statusRow, TempRow: l.tempRow}, *l.status)
}

func (l *fixedPromptLayout) drawTempMessage(message string) {
	if l == nil || !l.enabled {
		return
	}
	if l.status != nil && showTerminalFooterTempMessage(*l.status, message, 2*time.Second) {
		return
	}
	l.refresh()
	if !l.enabled {
		return
	}
	metrics, ok := terminalFooterMetricsForTTY()
	if ok {
		l.applyMetrics(metrics)
	}
	line := fitStatusBarText(" "+strings.TrimSpace(message)+" ", l.width)
	if terminalStatusANSIEnabled() {
		line = style(line, ansiYellow+ansiBold, true)
	}
	fmt.Fprintf(os.Stderr, "\x1b[s\x1b[%d;1H\x1b[2K%s\x1b[u", l.tempRow, line)
}

func (l *fixedPromptLayout) clearTempMessage() {
	if l == nil || !l.enabled {
		return
	}
	clearTerminalFooterTempMessage()
	l.refresh()
	if !l.enabled {
		return
	}
	fmt.Fprintf(os.Stderr, "\x1b[s\x1b[%d;1H\x1b[2K\x1b[u", l.tempRow)
}

func (l *fixedPromptLayout) clearMenu(rows int) {
	if rows <= 0 || l.promptTop <= 1 {
		return
	}
	if rows > l.promptTop-1 {
		rows = l.promptTop - 1
	}
	top := l.promptTop - rows
	for row := top; row < l.promptTop; row++ {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K", row)
	}
}

func (l *fixedPromptLayout) submit(prompt, _ string) {
	if l == nil || !l.enabled {
		return
	}
	if l.altScreen {
		leaveAlternatePickerScreen()
		l.altScreen = false
	}
	if l.drawnMenuRows > 0 {
		if metrics, ok := terminalReplayManagedViewport(*l.status); ok {
			l.applyMetrics(metrics)
		}
	} else {
		l.clearMenu(l.drawnMenuRows)
		l.drawStatus()
	}
	l.drawnMenuRows = 0
	// The submitted text is copied into the managed transcript by the REPL
	// immediately after Enter. Keep the footer prompt ready for the next input
	// while `Working ...` and the assistant response render above it.
	l.drawPrompt(prompt, "", 0)
	placeTerminalFooterPromptCursor(prompt, 0)
}

func (l *fixedPromptLayout) abort(marker string) {
	if l == nil || !l.enabled {
		return
	}
	if l.altScreen {
		leaveAlternatePickerScreen()
		l.altScreen = false
	}
	if l.drawnMenuRows > 0 {
		if metrics, ok := terminalReplayManagedViewport(*l.status); ok {
			l.applyMetrics(metrics)
		}
	} else {
		l.clearMenu(l.drawnMenuRows)
		l.drawStatus()
	}
	l.drawnMenuRows = 0
	if marker != "" {
		l.drawPrompt("", marker, len([]rune(marker)))
		return
	}
	placeTerminalFooterPromptCursor("ba> ", 0)
}

func (l *fixedPromptLayout) clear() {
	if l == nil || !l.enabled {
		return
	}
	if l.altScreen {
		leaveAlternatePickerScreen()
		l.altScreen = false
	}
	if l.drawnMenuRows > 0 {
		if metrics, ok := terminalReplayManagedViewport(*l.status); ok {
			l.applyMetrics(metrics)
		}
	} else {
		l.clearMenu(l.drawnMenuRows)
		l.drawStatus()
	}
	l.drawnMenuRows = 0
}

func drawTerminalFooterPrompt(prompt, line string, cursor int, status statusBarState) bool {
	metrics, ok := activateTerminalFooter(status)
	if !ok {
		return false
	}
	rows := commandInputBandRows(prompt, line, metrics.Width)
	for i, rowText := range rows {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[2K%s", metrics.PromptTop+i, rowText)
	}
	col := commandInputCursorColumn(prompt, cursor)
	if col > metrics.Width {
		col = metrics.Width
	}
	fmt.Fprintf(os.Stderr, "\x1b[%d;%dH", metrics.PromptRow, col)
	return true
}

func commandInputCursorColumn(prompt string, cursor int) int {
	col := len([]rune(prompt)) + cursor + 1
	if terminalStatusANSIEnabled() {
		col++ // Leading padding in the gray prompt band.
	}
	return maxInt(col, 1)
}

func fitPromptLine(line string, width int) string {
	if width <= 0 || runeLen(line) <= width {
		return line
	}
	plain := []rune(stripANSI(line))
	if len(plain) <= width {
		return line
	}
	if width == 1 {
		return "…"
	}
	return string(plain[:width-1]) + "…"
}

func commandInputLine(prompt, line string) string {
	return formatCommandInputLine(prompt, line, terminalStatusANSIEnabled(), 0)
}

func commandInputBand(prompt, line string, width int) string {
	return formatCommandInputLine(prompt, line, terminalStatusANSIEnabled(), width)
}

func commandInputBandRows(prompt, line string, width int) []string {
	return formatCommandInputBandRows(prompt, line, terminalStatusANSIEnabled(), width)
}

func formatCommandInputBandRows(prompt, line string, color bool, width int) []string {
	if !color {
		return []string{formatCommandInputLine(prompt, line, false, 0)}
	}
	blank := ansiUserBG + fitPromptBandText("", width) + ansiReset
	middle := formatCommandInputLine(prompt, line, true, width)
	return []string{blank, middle, blank}
}

func formatCommandInputLine(prompt, line string, color bool, width int) string {
	visible := prompt + line
	if !color {
		return visible
	}
	text := " " + visible
	if width > 0 {
		text = fitPromptBandText(text, width)
	}
	return ansiUserBG + ansiGrayFG + text + ansiReset
}

func fitPromptBandText(text string, width int) string {
	if width <= 0 {
		return text
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

func filterSlashSuggestions(prefix string) []SlashCommandSuggestion {
	if !strings.HasPrefix(prefix, "/") {
		return nil
	}
	suggestions := slashCommandSuggestions()
	if prefix == "/" {
		return suggestions
	}
	filtered := make([]SlashCommandSuggestion, 0, len(suggestions))
	for _, s := range suggestions {
		if strings.HasPrefix(s.Text, prefix) || strings.HasPrefix(strings.TrimSpace(s.Text), prefix) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func slashMenuLines(open bool, suggestions []SlashCommandSuggestion, selected int) []string {
	if !open {
		return nil
	}
	if len(suggestions) == 0 {
		return []string{"  no matching commands"}
	}
	lines := make([]string, 0, len(suggestions)+1)
	lines = append(lines, "  ↑/↓ choose · Enter run · Tab insert")
	for i, s := range suggestions {
		marker := " "
		if i == selected {
			marker = "›"
		}
		lines = append(lines, fmt.Sprintf("%s %-24s %s", marker, strings.TrimRight(s.Text, " "), s.Description))
	}
	return lines
}

func slashMenuChoice(suggestions []SlashCommandSuggestion, selected int) SlashCommandSuggestion {
	if len(suggestions) == 0 {
		return SlashCommandSuggestion{}
	}
	if selected < 0 {
		selected = 0
	}
	if selected >= len(suggestions) {
		selected = len(suggestions) - 1
	}
	return suggestions[selected]
}

func slashMenuInsertText(s SlashCommandSuggestion) string {
	return s.Text
}

func slashMenuEnterSubmits(s SlashCommandSuggestion) bool {
	return !strings.HasSuffix(s.Text, " ")
}

func rememberCommand(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	commandHistory.Lock()
	defer commandHistory.Unlock()
	if len(commandHistory.items) > 0 && commandHistory.items[len(commandHistory.items)-1] == line {
		return
	}
	commandHistory.items = append(commandHistory.items, line)
	if len(commandHistory.items) > 200 {
		commandHistory.items = commandHistory.items[len(commandHistory.items)-200:]
	}
}

func commandHistorySnapshot() []string {
	commandHistory.Lock()
	defer commandHistory.Unlock()
	out := make([]string, len(commandHistory.items))
	copy(out, commandHistory.items)
	return out
}

func promptYesNo(prompt string, defaultYes bool) (bool, error) {
	suffix := " [y/N]: "
	if defaultYes {
		suffix = " [Y/n]: "
	}
	for {
		answer, err := promptLine(prompt + suffix)
		if err != nil {
			return false, err
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "" {
			return defaultYes, nil
		}
		switch answer {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(os.Stderr, "please answer y or n")
		}
	}
}

type processingInputCapture struct {
	stop chan struct{}
	done chan struct{}
}

func startProcessingInputCapture(prompt string, status *statusBarState) *processingInputCapture {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) || status == nil {
		return nil
	}
	capture := &processingInputCapture{stop: make(chan struct{}), done: make(chan struct{})}
	go capture.run(prompt, *status)
	return capture
}

func (c *processingInputCapture) Stop() {
	if c == nil {
		return
	}
	close(c.stop)
	<-c.done
}

func (c *processingInputCapture) run(prompt string, status statusBarState) {
	defer close(c.done)
	setProcessingInputCaptureActive(true)
	defer setProcessingInputCaptureActive(false)
	stdinState.Lock()
	defer stdinState.Unlock()
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	fd := int(os.Stdin.Fd())
	_ = syscall.SetNonblock(fd, true)
	defer func() { _ = syscall.SetNonblock(fd, false) }()

	line, submitted := peekPendingCommandInput()
	buf := make([]byte, 32)
	draw := func() {
		drawTerminalFooterPrompt(prompt, line, len([]rune(line)), status)
	}
	draw()
	for {
		select {
		case <-c.stop:
			setPendingCommandInput(line, submitted)
			draw()
			return
		default:
		}
		n, err := syscall.Read(fd, buf)
		if n > 0 {
			changed := false
			for _, b := range buf[:n] {
				switch b {
				case '\r', '\n':
					submitted = true
					changed = true
				case 127, 8:
					if len(line) > 0 && !submitted {
						r := []rune(line)
						line = string(r[:len(r)-1])
						changed = true
					}
				case 3:
					line = ""
					submitted = false
					changed = true
				default:
					if b >= 32 && !submitted {
						line += string(rune(b))
						changed = true
					}
				}
			}
			if changed {
				setPendingCommandInput(line, submitted)
				draw()
			}
			continue
		}
		if err != nil && err != syscall.EAGAIN && err != syscall.EWOULDBLOCK {
			setPendingCommandInput(line, submitted)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func setProcessingInputCaptureActive(active bool) {
	processingInputState.Lock()
	processingInputState.active = active
	processingInputState.Unlock()
}

func processingInputCaptureActive() bool {
	processingInputState.Lock()
	defer processingInputState.Unlock()
	return processingInputState.active
}

func setPendingCommandInput(line string, submitted bool) {
	pendingCommandInput.Lock()
	pendingCommandInput.line = line
	pendingCommandInput.submitted = submitted
	pendingCommandInput.Unlock()
}

func peekPendingCommandInput() (string, bool) {
	pendingCommandInput.Lock()
	defer pendingCommandInput.Unlock()
	return pendingCommandInput.line, pendingCommandInput.submitted
}

func takePendingCommandInput() (string, bool) {
	pendingCommandInput.Lock()
	defer pendingCommandInput.Unlock()
	line, submitted := pendingCommandInput.line, pendingCommandInput.submitted
	pendingCommandInput.line = ""
	pendingCommandInput.submitted = false
	return line, submitted
}

type conversationPickerOption struct {
	Value   string
	Label   string
	Current bool
}

func promptInstanceSelection(instances []ProfileInfo, currentProfile string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		if currentProfile != "" {
			return currentProfile, nil
		}
		if len(instances) > 0 {
			return instances[0].Name, nil
		}
		return "", errors.New("no instances configured")
	}

	options := make([]conversationPickerOption, 0, len(instances)+1)
	selected := 0
	for _, instance := range instances {
		if instance.Name == currentProfile {
			selected = len(options)
		}
		cred := []string{}
		if instance.HasToken {
			cred = append(cred, "oauth")
		}
		if instance.HasSession {
			cred = append(cred, "web-session")
		}
		credText := "no credentials"
		if len(cred) > 0 {
			credText = strings.Join(cred, ", ")
		}
		options = append(options, conversationPickerOption{
			Value:   instance.Name,
			Label:   fmt.Sprintf("%-22s %s  [%s]", instance.Name, instance.InstanceURL, credText),
			Current: instance.Name == currentProfile,
		})
	}
	options = append(options, conversationPickerOption{Value: "__cancel__", Label: "Cancel"})
	if selected >= len(options) {
		selected = 0
	}

	stdinState.Lock()
	defer stdinState.Unlock()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprint(os.Stderr, "Select instance number/profile: ")
		line, readErr := stdinState.reader.ReadString('\n')
		if readErr != nil && len(line) == 0 {
			return "", readErr
		}
		return strings.TrimSpace(line), nil
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	enterAlternatePickerScreen()
	defer leaveAlternatePickerScreen()

	redraw := func() {
		fmt.Fprint(os.Stderr, "\x1b[H\x1b[2J")
		lines := instancePickerLines(options, selected)
		_, height, sizeErr := term.GetSize(int(os.Stderr.Fd()))
		maxLines := len(lines)
		if sizeErr == nil && height > 2 && maxLines > height-1 {
			maxLines = height - 1
		}
		for _, line := range lines[:maxLines] {
			fmt.Fprintf(os.Stderr, "%s\r\n", line)
		}
	}
	redraw()
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			return "", err
		}
		switch b := buf[0]; b {
		case '\r', '\n':
			return options[selected].Value, nil
		case '\t':
			selected = (selected + 1) % len(options)
			redraw()
		case 'q', 'Q':
			return "__cancel__", nil
		case 3:
			fmt.Fprint(os.Stderr, "^C\r\n")
			return "", errors.New("interrupted")
		case 4:
			return "__cancel__", nil
		case 27:
			seq := readPendingEscapeSequence()
			if len(seq) == 0 {
				return "__cancel__", nil
			}
			if seq[0] != '[' || len(seq) < 2 {
				continue
			}
			switch seq[1] {
			case 'A':
				selected--
				if selected < 0 {
					selected = len(options) - 1
				}
				redraw()
			case 'B':
				selected++
				if selected >= len(options) {
					selected = 0
				}
				redraw()
			}
		}
	}
}

func instancePickerLines(options []conversationPickerOption, selected int) []string {
	color := pickerColorEnabled()
	lines := []string{pickerHeader("Build Agent instances", "↑/↓ choose · Enter select · q cancel", color)}
	for i, option := range options {
		lines = append(lines, pickerOptionLine(option, i == selected, color))
	}
	return lines
}

func promptConversationSelection(conversations []WebConversation, currentID string, allowNew bool) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		printConversationPicker(conversations, currentID)
		return promptLine("Select conversation number/id, or n for new [n]: ")
	}

	options, selected := conversationPickerOptions(conversations, currentID, allowNew)

	stdinState.Lock()
	defer stdinState.Unlock()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		printConversationPicker(conversations, currentID)
		fmt.Fprint(os.Stderr, "Select conversation number/id, or n for new [n]: ")
		line, readErr := stdinState.reader.ReadString('\n')
		if readErr != nil && len(line) == 0 {
			return "", readErr
		}
		return strings.TrimSpace(line), nil
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	enterAlternatePickerScreen()
	defer leaveAlternatePickerScreen()

	redraw := func() {
		fmt.Fprint(os.Stderr, "\x1b[H\x1b[2J")
		lines := conversationPickerLines(options, selected)
		_, height, sizeErr := term.GetSize(int(os.Stderr.Fd()))
		maxLines := len(lines)
		if sizeErr == nil && height > 2 && maxLines > height-1 {
			maxLines = height - 1
		}
		for _, line := range lines[:maxLines] {
			fmt.Fprintf(os.Stderr, "%s\r\n", line)
		}
		if maxLines < len(lines) {
			fmt.Fprintf(os.Stderr, "  … %d more conversations not shown\r\n", len(lines)-maxLines)
		}
	}

	redraw()
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			return "", err
		}
		switch b := buf[0]; b {
		case '\r', '\n':
			return options[selected].Value, nil
		case '\t':
			selected++
			if selected >= len(options) {
				selected = 0
			}
			redraw()
		case 'n', 'N':
			if allowNew {
				return "__new__", nil
			}
		case 'q', 'Q':
			return "__cancel__", nil
		case 3: // Ctrl-C
			fmt.Fprint(os.Stderr, "^C\r\n")
			return "", errors.New("interrupted")
		case 4: // Ctrl-D
			return "__cancel__", nil
		case 27: // Escape closes; escape sequences handle arrows.
			seq := readPendingEscapeSequence()
			if len(seq) == 0 {
				return "__cancel__", nil
			}
			if seq[0] != '[' {
				continue
			}
			if len(seq) < 2 {
				continue
			}
			switch seq[1] {
			case 'A': // Up
				selected--
				if selected < 0 {
					selected = len(options) - 1
				}
				redraw()
			case 'B': // Down
				selected++
				if selected >= len(options) {
					selected = 0
				}
				redraw()
			}
		}
	}
}

func conversationPickerOptions(conversations []WebConversation, currentID string, allowNew bool) ([]conversationPickerOption, int) {
	options := make([]conversationPickerOption, 0, len(conversations)+2)
	selected := 0
	currentTrimmed := strings.TrimSpace(currentID)
	currentNormalized := normalizeGatewayConversationID(currentID)
	isCurrentConversation := func(id string) bool {
		id = strings.TrimSpace(id)
		if id == "" || currentTrimmed == "" {
			return false
		}
		if id == currentTrimmed {
			return true
		}
		if currentNormalized == "" {
			return false
		}
		return normalizeGatewayConversationID(id) == currentNormalized
	}
	for _, conv := range conversations {
		current := isCurrentConversation(conv.ID)
		if current {
			selected = len(options)
		}
		options = append(options, conversationPickerOption{
			Value:   conv.ID,
			Label:   conversationLabel(conv),
			Current: current,
		})
	}
	if allowNew {
		if len(options) == 0 {
			selected = len(options)
		}
		options = append(options, conversationPickerOption{Value: "__new__", Label: "New conversation"})
	}
	options = append(options, conversationPickerOption{Value: "__cancel__", Label: "Cancel"})
	if selected >= len(options) {
		selected = 0
	}
	return options, selected
}

func promptWorkspaceSelection(choices []WorkspaceChoice) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		printWorkspacePicker(choices)
		return promptLine("Select workspace number/name, or q to cancel: ")
	}

	options := make([]conversationPickerOption, 0, len(choices)+1)
	selected := 0
	for _, choice := range choices {
		if choice.Current {
			selected = len(options)
		}
		options = append(options, conversationPickerOption{
			Value:   choice.Value,
			Label:   choice.Label,
			Current: choice.Current,
		})
	}
	options = append(options, conversationPickerOption{Value: "__cancel__", Label: "Cancel"})
	if selected >= len(options) {
		selected = 0
	}

	stdinState.Lock()
	defer stdinState.Unlock()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		printWorkspacePicker(choices)
		fmt.Fprint(os.Stderr, "Select workspace number/name, or q to cancel: ")
		line, readErr := stdinState.reader.ReadString('\n')
		if readErr != nil && len(line) == 0 {
			return "", readErr
		}
		return strings.TrimSpace(line), nil
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	enterAlternatePickerScreen()
	defer leaveAlternatePickerScreen()

	redraw := func() {
		fmt.Fprint(os.Stderr, "\x1b[H\x1b[2J")
		lines := workspacePickerLines(options, selected)
		_, height, sizeErr := term.GetSize(int(os.Stderr.Fd()))
		maxLines := len(lines)
		if sizeErr == nil && height > 2 && maxLines > height-1 {
			maxLines = height - 1
		}
		for _, line := range lines[:maxLines] {
			fmt.Fprintf(os.Stderr, "%s\r\n", line)
		}
		if maxLines < len(lines) {
			fmt.Fprintf(os.Stderr, "  … %d more workspaces not shown\r\n", len(lines)-maxLines)
		}
	}

	redraw()
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			return "", err
		}
		switch b := buf[0]; b {
		case '\r', '\n':
			return options[selected].Value, nil
		case '\t':
			selected++
			if selected >= len(options) {
				selected = 0
			}
			redraw()
		case 'q', 'Q':
			return "__cancel__", nil
		case 3: // Ctrl-C
			fmt.Fprint(os.Stderr, "^C\r\n")
			return "", errors.New("interrupted")
		case 4: // Ctrl-D
			return "__cancel__", nil
		case 27: // Escape closes; escape sequences handle arrows.
			seq := readPendingEscapeSequence()
			if len(seq) == 0 {
				return "__cancel__", nil
			}
			if seq[0] != '[' || len(seq) < 2 {
				continue
			}
			switch seq[1] {
			case 'A': // Up
				selected--
				if selected < 0 {
					selected = len(options) - 1
				}
				redraw()
			case 'B': // Down
				selected++
				if selected >= len(options) {
					selected = 0
				}
				redraw()
			}
		}
	}
}

func promptAppSelection(choices []AppChoice) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		printAppPicker(choices)
		return promptLine("Select app number/name, or q to cancel: ")
	}

	options := make([]conversationPickerOption, 0, len(choices)+1)
	selected := 0
	for _, choice := range choices {
		if choice.Current {
			selected = len(options)
		}
		options = append(options, conversationPickerOption{
			Value:   choice.Value,
			Label:   choice.Label,
			Current: choice.Current,
		})
	}
	options = append(options, conversationPickerOption{Value: "__cancel__", Label: "Cancel"})
	if selected >= len(options) {
		selected = 0
	}

	stdinState.Lock()
	defer stdinState.Unlock()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		printAppPicker(choices)
		fmt.Fprint(os.Stderr, "Select app number/name, or q to cancel: ")
		line, readErr := stdinState.reader.ReadString('\n')
		if readErr != nil && len(line) == 0 {
			return "", readErr
		}
		return strings.TrimSpace(line), nil
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	enterAlternatePickerScreen()
	defer leaveAlternatePickerScreen()

	redraw := func() {
		fmt.Fprint(os.Stderr, "\x1b[H\x1b[2J")
		lines := appPickerLines(options, selected)
		_, height, sizeErr := term.GetSize(int(os.Stderr.Fd()))
		maxLines := len(lines)
		if sizeErr == nil && height > 2 && maxLines > height-1 {
			maxLines = height - 1
		}
		for _, line := range lines[:maxLines] {
			fmt.Fprintf(os.Stderr, "%s\r\n", line)
		}
		if maxLines < len(lines) {
			fmt.Fprintf(os.Stderr, "  … %d more apps not shown\r\n", len(lines)-maxLines)
		}
	}

	redraw()
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			return "", err
		}
		switch b := buf[0]; b {
		case '\r', '\n':
			return options[selected].Value, nil
		case '\t':
			selected++
			if selected >= len(options) {
				selected = 0
			}
			redraw()
		case 'q', 'Q':
			return "__cancel__", nil
		case 3: // Ctrl-C
			fmt.Fprint(os.Stderr, "^C\r\n")
			return "", errors.New("interrupted")
		case 4: // Ctrl-D
			return "__cancel__", nil
		case 27: // Escape closes; escape sequences handle arrows.
			seq := readPendingEscapeSequence()
			if len(seq) == 0 {
				return "__cancel__", nil
			}
			if seq[0] != '[' || len(seq) < 2 {
				continue
			}
			switch seq[1] {
			case 'A': // Up
				selected--
				if selected < 0 {
					selected = len(options) - 1
				}
				redraw()
			case 'B': // Down
				selected++
				if selected >= len(options) {
					selected = 0
				}
				redraw()
			}
		}
	}
}

func enterTerminalAppScreen() bool {
	if !interactiveTerminalUIEnabled() {
		return false
	}
	terminalAppScreenState.Lock()
	defer terminalAppScreenState.Unlock()
	if terminalAppScreenState.active {
		return false
	}
	terminalAppScreenState.active = true
	// Save the user's current terminal screen and run the interactive REPL inside
	// the alternate screen. On exit, the original shell screen is restored. Do not
	// clear alternate-screen scrollback by default: terminals that expose it should
	// still let the user scroll through the Build Agent transcript.
	fmt.Fprint(os.Stderr, "\x1b[?1049h\x1b[r\x1b[H\x1b[2J")
	return true
}

func leaveTerminalAppScreen() {
	terminalAppScreenState.Lock()
	if !terminalAppScreenState.active {
		terminalAppScreenState.Unlock()
		return
	}
	terminalAppScreenState.active = false
	terminalAppScreenState.Unlock()
	fmt.Fprint(os.Stderr, "\x1b[r\x1b[?1049l")
}

func terminalAppScreenActive() bool {
	terminalAppScreenState.Lock()
	defer terminalAppScreenState.Unlock()
	return terminalAppScreenState.active
}

func clearTerminalAppScrollback() {
	if terminalAppScreenActive() && os.Getenv("BA_CLI_CLEAR_ALT_SCROLLBACK") == "1" {
		fmt.Fprint(os.Stderr, "\x1b[3J")
	}
}

func enterAlternatePickerScreen() {
	// Conversation selection is an overlay-style UI. Outside the REPL app screen,
	// use the terminal alternate screen so closing/canceling restores the previous
	// scrollback. Inside the app alternate screen, do not nest 1049 screens;
	// clear the app screen and let leaveAlternatePickerScreen replay the app UI.
	if terminalAppScreenActive() {
		fmt.Fprint(os.Stderr, "\x1b[r\x1b[H\x1b[2J")
		return
	}
	fmt.Fprint(os.Stderr, "\x1b[?1049h\x1b[r\x1b[H\x1b[2J")
}

func leaveAlternatePickerScreen() {
	if terminalAppScreenActive() {
		if !replayManagedViewportFromLastStatus() {
			fmt.Fprint(os.Stderr, "\x1b[r\x1b[H\x1b[2J")
		}
		return
	}
	fmt.Fprint(os.Stderr, "\x1b[?1049l")
}

func readPendingEscapeSequence() []byte {
	// In raw mode, arrow keys arrive as ESC-prefixed byte sequences, while a
	// plain Escape key arrives as a lone 0x1b. A blocking read for the next byte
	// makes Escape feel hung. Briefly switch stdin non-blocking so a standalone
	// Escape can close menus immediately, while still accepting already-arrived
	// arrow-key sequences such as "[A" / "[B".
	time.Sleep(10 * time.Millisecond)
	fd := int(os.Stdin.Fd())
	_ = syscall.SetNonblock(fd, true)
	defer func() { _ = syscall.SetNonblock(fd, false) }()
	buf := make([]byte, 8)
	seq := make([]byte, 0, len(buf))
	for len(seq) < cap(seq) {
		n, err := syscall.Read(fd, buf[len(seq):cap(seq)])
		if n > 0 {
			seq = append(seq, buf[len(seq):len(seq)+n]...)
			continue
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK || err == nil {
			break
		}
		break
	}
	return seq
}

func conversationPickerLines(options []conversationPickerOption, selected int) []string {
	color := pickerColorEnabled()
	lines := []string{pickerHeader("Build Agent conversations", "↑/↓ choose · Enter open · n new · q cancel", color)}
	if len(options) == 0 {
		return append(lines, style("  <none found>", ansiDim, color))
	}
	for i, option := range options {
		lines = append(lines, pickerOptionLine(option, i == selected, color))
	}
	return lines
}

func printWorkspacePicker(choices []WorkspaceChoice) {
	fmt.Fprintln(os.Stderr, "Build Agent workspaces:")
	if len(choices) == 0 {
		fmt.Fprintln(os.Stderr, "  <none found>")
		return
	}
	for i, choice := range choices {
		marker := " "
		if choice.Current {
			marker = "*"
		}
		fmt.Fprintf(os.Stderr, "%s %2d) %s\n", marker, i+1, choice.Label)
	}
}

func workspacePickerLines(options []conversationPickerOption, selected int) []string {
	color := pickerColorEnabled()
	lines := []string{pickerHeader("Build Agent workspaces", "↑/↓ choose · Enter open · q cancel", color)}
	if len(options) == 0 {
		return append(lines, style("  <none found>", ansiDim, color))
	}
	for i, option := range options {
		lines = append(lines, pickerOptionLine(option, i == selected, color))
	}
	return lines
}

func printAppPicker(choices []AppChoice) {
	fmt.Fprintln(os.Stderr, "Build Agent workspace apps:")
	if len(choices) == 0 {
		fmt.Fprintln(os.Stderr, "  <none found>")
		return
	}
	for i, choice := range choices {
		marker := " "
		if choice.Current {
			marker = "*"
		}
		fmt.Fprintf(os.Stderr, "%s %2d) %s\n", marker, i+1, choice.Label)
	}
}

func appPickerLines(options []conversationPickerOption, selected int) []string {
	color := pickerColorEnabled()
	lines := []string{pickerHeader("Build Agent workspace apps", "↑/↓ choose · Enter select · q cancel", color)}
	if len(options) == 0 {
		return append(lines, style("  <none found>", ansiDim, color))
	}
	for i, option := range options {
		lines = append(lines, pickerOptionLine(option, i == selected, color))
	}
	return lines
}

func pickerColorEnabled() bool {
	return terminalStatusANSIEnabled()
}

func pickerHeader(title, hint string, color bool) string {
	if !color {
		return title + "  " + hint
	}
	return style(title, ansiBold+ansiCyan, true) + style("  "+hint, ansiDim, true)
}

func pickerOptionLine(option conversationPickerOption, selected bool, color bool) string {
	selector := " "
	if selected {
		selector = style("›", ansiWasabiGreen, color)
	}
	current := " "
	if option.Current {
		current = style("*", ansiYellow+ansiBold, color)
	}
	label := colorPickerLabel(option, color)
	if selected {
		label = style(label, ansiBold, color)
	}
	return fmt.Sprintf("%s %s %s", selector, current, label)
}

func colorPickerLabel(option conversationPickerOption, color bool) string {
	label := option.Label
	if !color {
		return label
	}
	switch option.Value {
	case "__cancel__":
		return style(label, ansiRed, true)
	case "__new__":
		return style(label, ansiWasabiGreen, true)
	}
	return colorPickerURL(colorPickerBracketTags(label))
}

func colorPickerURL(label string) string {
	idx := strings.Index(label, "https://")
	if idx < 0 {
		idx = strings.Index(label, "http://")
	}
	if idx < 0 {
		return label
	}
	end := idx
	for end < len(label) && label[end] != ' ' && label[end] != '\t' {
		end++
	}
	return label[:idx] + style(label[idx:end], ansiCyan, true) + label[end:]
}

func colorPickerBracketTags(label string) string {
	var out strings.Builder
	for {
		start := strings.Index(label, "[")
		if start < 0 {
			out.WriteString(label)
			return out.String()
		}
		end := strings.Index(label[start:], "]")
		if end < 0 {
			out.WriteString(label)
			return out.String()
		}
		end += start
		out.WriteString(label[:start])
		tag := label[start : end+1]
		lower := strings.ToLower(tag)
		code := ansiMagenta + ansiBold
		switch {
		case strings.Contains(lower, "no credentials") || strings.Contains(lower, "closed") || strings.Contains(lower, "error"):
			code = ansiRed
		case strings.Contains(lower, "open") || strings.Contains(lower, "oauth") || strings.Contains(lower, "web-session"):
			code = ansiWasabiGreen
		case strings.Contains(lower, "new"):
			code = ansiGreen + ansiBold
		}
		out.WriteString(style(tag, code, true))
		label = label[end+1:]
	}
}
