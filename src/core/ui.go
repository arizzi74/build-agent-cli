package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

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
	cursor    int
	submitted bool
}{}

var processingInputState = struct {
	sync.Mutex
	active  bool
	capture *processingInputCapture
}{}

var terminalAppScreenState = struct {
	sync.Mutex
	active bool
}{}

var terminalPickerState = struct {
	sync.Mutex
	active         bool
	replayDeferred bool
}{}

var terminalActivePromptView = struct {
	sync.Mutex
	layout    *fixedPromptLayout
	prompt    string
	line      string
	cursor    int
	menuLines []string
}{}

func updateTerminalActivePromptView(layout *fixedPromptLayout, prompt, line string, cursor int, menuLines []string) {
	terminalActivePromptView.Lock()
	terminalActivePromptView.layout = layout
	terminalActivePromptView.prompt = prompt
	terminalActivePromptView.line = line
	terminalActivePromptView.cursor = cursor
	terminalActivePromptView.menuLines = append(terminalActivePromptView.menuLines[:0], menuLines...)
	terminalActivePromptView.Unlock()
}

func clearTerminalActivePromptView(layout *fixedPromptLayout) {
	terminalActivePromptView.Lock()
	if terminalActivePromptView.layout == layout {
		terminalActivePromptView.layout = nil
		terminalActivePromptView.prompt = ""
		terminalActivePromptView.line = ""
		terminalActivePromptView.cursor = 0
		terminalActivePromptView.menuLines = nil
	}
	terminalActivePromptView.Unlock()
}

// redrawTerminalActivePromptUnlocked is called only while terminalRenderMu is
// held. The immutable snapshot avoids taking the command editor's state lock in
// the opposite order from asynchronous transcript rendering.
func redrawTerminalActivePromptUnlocked() bool {
	terminalActivePromptView.Lock()
	layout := terminalActivePromptView.layout
	prompt := terminalActivePromptView.prompt
	line := terminalActivePromptView.line
	cursor := terminalActivePromptView.cursor
	menuLines := append([]string(nil), terminalActivePromptView.menuLines...)
	terminalActivePromptView.Unlock()
	if layout == nil {
		return false
	}
	layout.redrawUnlocked(prompt, line, cursor, menuLines)
	return layout.enabled
}

func terminalPickerActive() bool {
	terminalPickerState.Lock()
	defer terminalPickerState.Unlock()
	return terminalPickerState.active
}

// deferTerminalReplayWhilePickerActive atomically records that semantic
// transcript state changed while a modal picker owned the physical display.
// Picker teardown consumes the flag only after it has released screen
// ownership, so no remote output can overwrite the picker.
func deferTerminalReplayWhilePickerActive() bool {
	terminalPickerState.Lock()
	defer terminalPickerState.Unlock()
	if !terminalPickerState.active {
		return false
	}
	terminalPickerState.replayDeferred = true
	return true
}

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
	return promptCommandLineForClient(prompt, status, nil)
}

func promptCommandLineForClient(prompt string, status *statusBarState, client *Client) (string, error) {
	return promptCommandLineForClientProvider(prompt, status, func() *Client { return client })
}

func promptCommandLineForActiveClient(prompt string, clients *activeClientRef) (string, error) {
	if clients == nil {
		return "", errors.New("active client is unavailable")
	}
	client := clients.Get()
	if client == nil {
		return "", errors.New("active client is unavailable")
	}
	status := client.statusBarState()
	return promptCommandLineForClientProvider(prompt, &status, clients.Get)
}

func promptCommandLineForClientProvider(prompt string, status *statusBarState, clientProvider func() *Client) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(terminalStderrFD()) {
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
	terminalControls := !strings.EqualFold(os.Getenv("TERM"), "dumb")
	if terminalControls {
		terminalRenderMu.Lock()
		_ = writeTerminalString(os.Stderr, "\x1b[?2004h"+ansiCursorShow)
		terminalRenderMu.Unlock()
	}
	defer func() {
		if terminalControls {
			terminalRenderMu.Lock()
			_ = writeTerminalString(os.Stderr, "\x1b[?2004l"+ansiReset+ansiCursorShow)
			terminalRenderMu.Unlock()
		}
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}()

	layout := newFixedPromptLayout(status)
	defer layout.clear()
	defer clearTerminalActivePromptView(layout)

	history := commandHistorySnapshot()
	historyIndex := len(history)
	initialLine, initialCursor, initialSubmitted := takePendingCommandInputAt()
	editor := newTerminalComposerAt(initialLine, initialCursor)
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

	type promptSnapshot struct {
		line     string
		cursor   int
		menuOpen bool
		menu     []SlashCommandSuggestion
		selected int
	}

	var stateMu sync.Mutex
	var redrawMu sync.Mutex
	menuOpen := false
	menu := []SlashCommandSuggestion{}
	selected := 0
	draftLine, draftCursor := editor.Text(), editor.CursorByte()
	draftSaved := false
	lastCtrlD := time.Time{}
	var ctrlDMu sync.Mutex
	var ctrlDTimer *time.Timer
	promptDone := make(chan struct{})
	currentClient := func() *Client {
		if clientProvider == nil {
			return nil
		}
		return clientProvider()
	}
	promptLabel := func() string {
		if client := currentClient(); client != nil {
			if count := client.pendingAttachmentCount(); count > 0 {
				return fmt.Sprintf("ba[attach:%d]> ", count)
			}
		}
		return prompt
	}
	defer func() {
		close(promptDone)
		ctrlDMu.Lock()
		if ctrlDTimer != nil {
			ctrlDTimer.Stop()
		}
		ctrlDMu.Unlock()
	}()

	refreshMenuLocked := func() {
		if !menuOpen {
			menu = nil
			selected = 0
			return
		}
		if !strings.HasPrefix(editor.Text(), "/") || strings.ContainsAny(editor.Text(), "\r\n") {
			menuOpen = false
			menu = nil
			selected = 0
			return
		}
		menu = filterSlashSuggestionsForClient(editor.Text(), currentClient())
		if selected >= len(menu) {
			selected = len(menu) - 1
		}
		if selected < 0 {
			selected = 0
		}
	}

	snapshotLocked := func() promptSnapshot {
		return promptSnapshot{
			line:     editor.Text(),
			cursor:   editor.CursorByte(),
			menuOpen: menuOpen,
			menu:     append([]SlashCommandSuggestion(nil), menu...),
			selected: selected,
		}
	}

	redrawSnapshot := func(snapshot promptSnapshot) {
		livePrompt := promptLabel()
		menuLines := slashMenuLines(snapshot.menuOpen, snapshot.menu, snapshot.selected)
		updateTerminalActivePromptView(layout, livePrompt, snapshot.line, snapshot.cursor, menuLines)
		layout.redraw(livePrompt, snapshot.line, snapshot.cursor, menuLines)
		if layout.enabled {
			return
		}

		// Tiny terminals use a compact, one-row fallback until the next resize can
		// activate the fixed footer. Newlines are made visible without allowing a
		// draft to move the physical terminal cursor unpredictably.
		displayPrefix := strings.ReplaceAll(strings.ReplaceAll(snapshot.line[:snapshot.cursor], "\n", "↵ "), "\t", "    ")
		displaySuffix := strings.ReplaceAll(strings.ReplaceAll(snapshot.line[snapshot.cursor:], "\n", "↵ "), "\t", "    ")
		width, _, sizeErr := term.GetSize(terminalStderrFD())
		if sizeErr != nil || width <= 0 {
			width = 80
		}
		leadingCells := 0
		if terminalStatusANSIEnabled() {
			leadingCells = 1
		}
		available := maxInt(1, width-terminalDisplayWidth(livePrompt)-leadingCells-1)
		visiblePrefix := displayPrefix
		leftMarker := ""
		if terminalDisplayWidth(visiblePrefix) > available {
			leftMarker = "…"
			visiblePrefix = terminalTailCells(visiblePrefix, maxInt(available-1, 0))
		}
		cursorCells := terminalDisplayWidth(leftMarker + visiblePrefix)
		displayLine := leftMarker + visiblePrefix + terminalFitCells(displaySuffix, maxInt(available-cursorCells, 0))
		terminalRenderMu.Lock()
		beginTerminalFrame()
		fmt.Fprintf(os.Stderr, "\r%s%s", ansiEraseLine, commandInputLine(livePrompt, displayLine))
		fmt.Fprintf(os.Stderr, "%s\x1b[J", ansiReset)
		lines := boundedSlashMenuLines(menuLines, terminalSlashMenuMaxRows())
		for _, menuLine := range lines {
			fmt.Fprintf(os.Stderr, "\r\n%s", fitPromptLine(menuLine, maxInt(width-1, 1)))
		}
		if len(lines) > 0 {
			fmt.Fprintf(os.Stderr, "\x1b[%dA\r", len(lines))
		} else {
			fmt.Fprint(os.Stderr, "\r")
		}
		col := terminalDisplayWidth(livePrompt) + leadingCells + cursorCells
		if col > 0 {
			fmt.Fprintf(os.Stderr, "\x1b[%dC", col)
		}
		endTerminalFrame()
		terminalRenderMu.Unlock()
	}

	redraw := func() {
		redrawMu.Lock()
		defer redrawMu.Unlock()
		stateMu.Lock()
		snapshot := snapshotLocked()
		stateMu.Unlock()
		redrawSnapshot(snapshot)
	}

	finishPrompt := func(result string, abort bool, marker string) {
		redrawMu.Lock()
		defer redrawMu.Unlock()
		if layout.enabled {
			if abort {
				layout.abort(marker)
			} else {
				layout.submit(promptLabel(), result)
			}
			return
		}
		terminalRenderMu.Lock()
		beginTerminalFrame()
		if marker != "" {
			fmt.Fprintf(os.Stderr, "\r%s%s\r\n", ansiEraseLine, marker)
		} else {
			fmt.Fprint(os.Stderr, "\r\n")
		}
		endTerminalFrame()
		terminalRenderMu.Unlock()
	}

	showCtrlDMessage := func(message string) {
		redrawMu.Lock()
		defer redrawMu.Unlock()
		if layout.enabled {
			layout.drawTempMessage(message)
			return
		}
		terminalRenderMu.Lock()
		beginTerminalFrame()
		width, _, sizeErr := term.GetSize(terminalStderrFD())
		if sizeErr != nil || width <= 0 {
			width = 80
		}
		fmt.Fprint(os.Stderr, "\r"+ansiEraseLine+fitPromptLine(message, maxInt(width-1, 1))+"\r")
		endTerminalFrame()
		terminalRenderMu.Unlock()
	}

	clearCtrlDMessage := func() {
		redrawMu.Lock()
		defer redrawMu.Unlock()
		if layout.enabled {
			layout.clearTempMessage()
			return
		}
		stateMu.Lock()
		snapshot := snapshotLocked()
		stateMu.Unlock()
		redrawSnapshot(snapshot)
	}

	redraw()
	stopResizeNotifications := startTerminalResizeNotifications(func() {
		redraw()
	})
	defer stopResizeNotifications()

	contentWidth := func() int {
		width, _, sizeErr := term.GetSize(terminalStderrFD())
		if sizeErr != nil || width <= 0 {
			width = terminalStatusWidth()
		}
		return maxInt(1, width-terminalDisplayWidth(promptLabel())-2)
	}

	for {
		event, readErr := readTerminalInputEvent(int(os.Stdin.Fd()))
		if readErr != nil {
			return "", readErr
		}

		changed := false
		redrawNeeded := false
		warning := ""
		stateMu.Lock()
		switch event.Kind {
		case terminalInputEnter:
			if menuOpen && len(menu) > 0 {
				choice := slashMenuChoice(menu, selected)
				inserted := slashMenuInsertText(choice)
				editor.Reset(inserted, len(inserted))
				menuOpen = false
				refreshMenuLocked()
				if slashMenuEnterSubmits(choice) {
					result := strings.TrimSpace(editor.Text())
					stateMu.Unlock()
					finishPrompt(result, false, "")
					rememberCommand(result)
					return result, nil
				}
				redrawNeeded = true
				break
			}
			result := strings.TrimSpace(editor.Text())
			stateMu.Unlock()
			finishPrompt(result, false, "")
			rememberCommand(result)
			return result, nil

		case terminalInputNewline:
			changed = editor.Insert("\n")

		case terminalInputTab:
			if menuOpen && len(menu) > 0 {
				choice := slashMenuChoice(menu, selected)
				inserted := slashMenuInsertText(choice)
				editor.Reset(inserted, len(inserted))
				menuOpen = false
				refreshMenuLocked()
				redrawNeeded = true
			}

		case terminalInputCtrlC:
			stateMu.Unlock()
			finishPrompt("", true, "^C")
			return "", errors.New("interrupted")

		case terminalInputCtrlD:
			stateMu.Unlock()
			now := time.Now()
			ctrlDMu.Lock()
			if !lastCtrlD.IsZero() && now.Sub(lastCtrlD) <= 2*time.Second {
				lastCtrlD = time.Time{}
				if ctrlDTimer != nil {
					ctrlDTimer.Stop()
					ctrlDTimer = nil
				}
				ctrlDMu.Unlock()
				finishPrompt("", true, "")
				return "", errors.New("EOF")
			}
			lastCtrlD = now
			if ctrlDTimer != nil {
				ctrlDTimer.Stop()
			}
			ctrlDTimer = time.AfterFunc(2*time.Second, func() {
				select {
				case <-promptDone:
					return
				default:
				}
				ctrlDMu.Lock()
				if !lastCtrlD.Equal(now) {
					ctrlDMu.Unlock()
					return
				}
				lastCtrlD = time.Time{}
				ctrlDTimer = nil
				ctrlDMu.Unlock()
				select {
				case <-promptDone:
					return
				default:
				}
				clearCtrlDMessage()
			})
			ctrlDMu.Unlock()
			showCtrlDMessage("Press Ctrl-D again to exit ....")
			continue

		case terminalInputBackspace:
			changed = editor.BackspaceGrapheme()

		case terminalInputDelete:
			changed = editor.DeleteGrapheme()

		case terminalInputEscape:
			if menuOpen {
				menuOpen = false
				editor.Reset("", 0)
				refreshMenuLocked()
				redrawNeeded = true
			} else if client := currentClient(); client != nil && client.cancelActiveTurn() {
				warning = "Cancellation requested"
				redrawNeeded = true
			}

		case terminalInputLeft, terminalInputCtrlB:
			if event.Kind == terminalInputLeft && event.Modifiers&(terminalInputModifierAlt|terminalInputModifierCtrl) != 0 {
				changed = editor.MoveWordLeft()
			} else {
				changed = editor.MoveGraphemeLeft()
			}

		case terminalInputRight, terminalInputCtrlF:
			if event.Kind == terminalInputRight && event.Modifiers&(terminalInputModifierAlt|terminalInputModifierCtrl) != 0 {
				changed = editor.MoveWordRight()
			} else {
				changed = editor.MoveGraphemeRight()
			}

		case terminalInputHome, terminalInputCtrlA:
			changed = editor.MoveLogicalHome()

		case terminalInputEnd, terminalInputCtrlE:
			changed = editor.MoveLogicalEnd()

		case terminalInputCtrlW:
			changed = editor.DeleteWordBackward()

		case terminalInputCtrlU:
			end := editor.CursorByte()
			if editor.MoveLogicalHome() {
				changed = true
				for editor.CursorByte() < end {
					before := len(editor.Text())
					if !editor.DeleteGrapheme() {
						break
					}
					end -= before - len(editor.Text())
				}
			}

		case terminalInputCtrlV:
			client := currentClient()
			if client == nil {
				warning = "Clipboard image capture is unavailable"
				break
			}
			clipboardCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			name, mediaType, data, clipboardErr := readClipboardAttachment(clipboardCtx)
			cancel()
			if clipboardErr != nil {
				warning = "Attachment not added: " + clipboardErr.Error()
				break
			}
			summary, attachmentErr := client.addPendingAttachmentBytes(name, mediaType, data)
			if attachmentErr != nil {
				warning = "Attachment not added: " + attachmentErr.Error()
				break
			}
			warning = fmt.Sprintf("Attached %s (%s)", summary.Name, formatAttachmentBytes(summary.Size))
			redrawNeeded = true

		case terminalInputCtrlK:
			probe := editor
			probe.MoveLogicalEnd()
			end := probe.CursorByte()
			for editor.CursorByte() < end {
				before := len(editor.Text())
				if !editor.DeleteGrapheme() {
					break
				}
				end -= before - len(editor.Text())
				changed = true
			}

		case terminalInputUp:
			if menuOpen {
				if len(menu) > 0 {
					selected--
					if selected < 0 {
						selected = len(menu) - 1
					}
					redrawNeeded = true
				}
				break
			}
			if editor.MoveVisualUp(contentWidth()) {
				changed = true
				break
			}
			if len(history) > 0 && historyIndex > 0 {
				if historyIndex == len(history) && !draftSaved {
					draftLine, draftCursor = editor.Text(), editor.CursorByte()
					draftSaved = true
				}
				historyIndex--
				editor.Reset(history[historyIndex], len(history[historyIndex]))
				changed = true
			}

		case terminalInputDown:
			if menuOpen {
				if len(menu) > 0 {
					selected++
					if selected >= len(menu) {
						selected = 0
					}
					redrawNeeded = true
				}
				break
			}
			if editor.MoveVisualDown(contentWidth()) {
				changed = true
				break
			}
			if historyIndex < len(history)-1 {
				historyIndex++
				editor.Reset(history[historyIndex], len(history[historyIndex]))
				changed = true
			} else if historyIndex < len(history) {
				historyIndex = len(history)
				if draftSaved {
					editor.Reset(draftLine, draftCursor)
				} else {
					editor.Reset("", 0)
				}
				draftSaved = false
				changed = true
			}

		case terminalInputPaste:
			if event.Truncated {
				warning = "Paste rejected: input exceeds 1 MiB"
			} else {
				changed = editor.Insert(event.Text)
			}

		case terminalInputText:
			if !unicode.IsControl(event.Rune) {
				wasEmpty := editor.Text() == ""
				changed = editor.Insert(string(event.Rune))
				if changed && wasEmpty && editor.Text() == "/" {
					menuOpen = true
					selected = 0
				}
			}
		}

		if changed {
			refreshMenuLocked()
			redrawNeeded = true
		}
		stateMu.Unlock()
		if warning != "" {
			showCtrlDMessage(warning)
			time.AfterFunc(2*time.Second, func() {
				select {
				case <-promptDone:
					return
				default:
				}
				clearCtrlDMessage()
			})
		}
		if redrawNeeded {
			redraw()
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
	promptRows    int
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

func (l *fixedPromptLayout) currentStatus() statusBarState {
	lastTerminalFooterStatus.Lock()
	if lastTerminalFooterStatus.set {
		status := lastTerminalFooterStatus.state
		lastTerminalFooterStatus.Unlock()
		return status
	}
	lastTerminalFooterStatus.Unlock()
	if l == nil || l.status == nil {
		return statusBarState{}
	}
	return *l.status
}

func (l *fixedPromptLayout) refresh() {
	if l == nil || l.status == nil {
		return
	}
	l.enabled = false
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
	l.promptRows = metrics.PromptRows
	l.statusRow = metrics.StatusRow
	l.tempRow = metrics.TempRow
}

func (l *fixedPromptLayout) redraw(prompt, line string, cursor int, menuLines []string) {
	if l == nil {
		return
	}
	terminalRenderMu.Lock()
	defer terminalRenderMu.Unlock()
	beginTerminalFrame()
	defer endTerminalFrame()
	l.redrawUnlocked(prompt, line, cursor, menuLines)
}

func (l *fixedPromptLayout) redrawUnlocked(prompt, line string, cursor int, menuLines []string) {
	if l == nil {
		return
	}
	oldWidth, oldHeight := l.width, l.height
	oldTurnTop, oldTempRow := l.turnTop, l.tempRow
	oldPromptRows := l.promptRows
	oldEnabled := l.enabled
	oldMenuOpen := l.drawnMenuRows > 0
	newMenuOpen := len(menuLines) > 0
	l.refresh()
	if !l.enabled {
		if oldEnabled {
			deactivateTerminalFooterForFallback(oldTurnTop)
		}
		return
	}
	composerLayout := terminalComposerLayoutForSize(prompt, line, cursor, l.width, l.height)
	setTerminalPromptRows(len(composerLayout.Rows))
	l.refresh()
	resized := oldHeight > 0 && (oldWidth != l.width || oldHeight != l.height)
	replay := resized || (oldHeight > 0 && (oldMenuOpen != newMenuOpen || oldPromptRows != l.promptRows))
	if replay {
		var metrics terminalFooterMetrics
		var ok bool
		if resized {
			metrics, ok = terminalReplayManagedViewportWithScrollbackUnlocked(l.currentStatus())
		} else {
			metrics, ok = terminalReplayManagedViewportUnlocked(l.currentStatus())
		}
		if ok {
			l.applyMetrics(metrics)
		}
	} else if metrics, ok := activateTerminalFooter(l.currentStatus()); ok {
		l.applyMetrics(metrics)
	}
	if !replay && oldHeight > 0 && (oldHeight != l.height || oldTempRow != l.tempRow) {
		l.clearResizedFooterRows(oldTurnTop, oldTempRow)
	}
	menuBottom := maxInt(l.promptTop-1, 0)
	maxMenuRows := menuBottom
	menuLines = boundedSlashMenuLines(menuLines, maxMenuRows)
	if !replay {
		l.clearMenu(maxInt(l.drawnMenuRows, len(menuLines)))
	}
	menuTop := l.promptTop - len(menuLines)
	for i, menuLine := range menuLines {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H%s%s", menuTop+i, ansiEraseLine, fitPromptLine(menuLine, l.width))
	}
	l.drawPromptLayoutWithSeparator(composerLayout, len(menuLines) == 0)
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
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H%s", row, ansiEraseLine)
	}
	fmt.Fprint(os.Stderr, "\x1b[u")
}

func (l *fixedPromptLayout) drawPrompt(prompt, line string, cursor int) {
	l.drawPromptWithSeparator(prompt, line, cursor, true)
}

func (l *fixedPromptLayout) drawPromptWithSeparator(prompt, line string, cursor int, clearSeparator bool) {
	l.refresh()
	if !l.enabled {
		return
	}
	layout := terminalComposerLayoutForSize(prompt, line, cursor, l.width, l.height)
	rowsChanged := setTerminalPromptRows(len(layout.Rows))
	var metrics terminalFooterMetrics
	var ok bool
	if rowsChanged {
		metrics, ok = terminalReplayManagedViewportUnlocked(l.currentStatus())
	} else {
		metrics, ok = activateTerminalFooter(l.currentStatus())
	}
	if ok {
		l.applyMetrics(metrics)
	}
	l.drawPromptLayoutWithSeparator(layout, clearSeparator)
}

func (l *fixedPromptLayout) drawPromptLayoutWithSeparator(layout terminalComposerLayout, clearSeparator bool) {
	// Keep one guaranteed blank separator between the transcript/last startup
	// output and the gray input band. Without this, text printed before the
	// footer is activated can sit directly against the prompt on first launch.
	// When a slash menu is open, that row belongs to the menu; clearing it here
	// would erase the only matching command for filtered menus such as `/w`.
	if clearSeparator && l.promptTop > 1 {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H%s", l.promptTop-1, ansiEraseLine)
	}
	rows := terminalComposerBandRows(layout, terminalStatusANSIEnabled(), l.width)
	for i, rowText := range rows {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H%s%s", l.promptTop+i, ansiEraseLine, rowText)
	}
	row := l.promptTop + 1 + layout.CursorRow
	col := minInt(layout.CursorColumn, l.width)
	fmt.Fprintf(os.Stderr, "\x1b[%d;%dH", row, col)
}

func (l *fixedPromptLayout) drawStatus() {
	if l.status == nil {
		return
	}
	drawTerminalFooterStatusAt(terminalFooterMetrics{Width: l.width, Height: l.height, ScrollBottom: l.scrollBottom, TurnTop: l.turnTop, TurnRow: l.turnTop + 1, TurnBottom: l.turnTop + 2, PromptTop: l.promptTop, PromptRow: l.promptRow, PromptBottom: l.promptBottom, PromptRows: l.promptRows, StatusRow: l.statusRow, TempRow: l.tempRow}, l.currentStatus())
}

func (l *fixedPromptLayout) drawTempMessage(message string) {
	if l == nil || !l.enabled {
		return
	}
	if l.status != nil && showTerminalFooterTempMessage(l.currentStatus(), message, 2*time.Second) {
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
	fmt.Fprintf(os.Stderr, "\x1b[s\x1b[%d;1H%s%s\x1b[u", l.tempRow, ansiEraseLine, line)
}

func (l *fixedPromptLayout) clearTempMessage() {
	if l == nil || !l.enabled {
		return
	}
	clearTerminalFooterTempMessage()
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
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H%s", row, ansiEraseLine)
	}
}

func (l *fixedPromptLayout) submit(prompt, _ string) {
	if l == nil || !l.enabled {
		return
	}
	terminalRenderMu.Lock()
	defer terminalRenderMu.Unlock()
	beginTerminalFrame()
	defer endTerminalFrame()
	if l.altScreen {
		leaveAlternatePickerScreen()
		l.altScreen = false
	}
	if l.drawnMenuRows > 0 {
		if metrics, ok := terminalReplayManagedViewportUnlocked(l.currentStatus()); ok {
			l.applyMetrics(metrics)
		}
	} else {
		l.clearMenu(l.drawnMenuRows)
		l.drawStatus()
	}
	l.drawnMenuRows = 0
	// The submitted text is copied into the managed transcript by the REPL
	// immediately after Enter. Keep the footer prompt ready for the next input
	// while `Building` and the assistant response render above it.
	l.drawPrompt(prompt, "", 0)
	placeTerminalFooterComposerCursorUnlocked(prompt, "", 0)
}

func (l *fixedPromptLayout) abort(marker string) {
	if l == nil || !l.enabled {
		return
	}
	terminalRenderMu.Lock()
	defer terminalRenderMu.Unlock()
	beginTerminalFrame()
	defer endTerminalFrame()
	if l.altScreen {
		leaveAlternatePickerScreen()
		l.altScreen = false
	}
	if l.drawnMenuRows > 0 {
		if metrics, ok := terminalReplayManagedViewportUnlocked(l.currentStatus()); ok {
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
	placeTerminalFooterComposerCursorUnlocked("ba> ", "", 0)
}

func (l *fixedPromptLayout) clear() {
	if l == nil || !l.enabled {
		return
	}
	terminalRenderMu.Lock()
	defer terminalRenderMu.Unlock()
	beginTerminalFrame()
	defer endTerminalFrame()
	if l.altScreen {
		leaveAlternatePickerScreen()
		l.altScreen = false
	}
	if l.drawnMenuRows > 0 {
		if metrics, ok := terminalReplayManagedViewportUnlocked(l.currentStatus()); ok {
			l.applyMetrics(metrics)
		}
	} else {
		l.clearMenu(l.drawnMenuRows)
		l.drawStatus()
	}
	l.drawnMenuRows = 0
}

func drawTerminalFooterPrompt(prompt, line string, cursor int, status statusBarState) bool {
	if terminalPickerActive() {
		return false
	}
	terminalRenderMu.Lock()
	defer terminalRenderMu.Unlock()
	beginTerminalFrame()
	defer endTerminalFrame()
	return drawTerminalFooterPromptUnlocked(prompt, line, cursor, status)
}

func drawTerminalFooterPromptUnlocked(prompt, line string, cursor int, status statusBarState) bool {
	metrics, ok := terminalFooterMetricsForTTY()
	if !ok {
		return false
	}
	layout := terminalComposerLayoutForSize(prompt, line, cursor, metrics.Width, metrics.Height)
	rowsChanged := setTerminalPromptRows(len(layout.Rows))
	if rowsChanged {
		metrics, ok = terminalReplayManagedViewportUnlocked(status)
	} else {
		metrics, ok = activateTerminalFooter(status)
	}
	if !ok {
		return false
	}
	rows := terminalComposerBandRows(layout, terminalStatusANSIEnabled(), metrics.Width)
	for i, rowText := range rows {
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H%s%s", metrics.PromptTop+i, ansiEraseLine, rowText)
	}
	row := metrics.PromptTop + 1 + layout.CursorRow
	col := minInt(layout.CursorColumn, metrics.Width)
	fmt.Fprintf(os.Stderr, "\x1b[%d;%dH", row, col)
	return true
}

func commandInputCursorColumn(prompt string, cursor int) int {
	col := terminalDisplayWidth(prompt) + cursor + 1
	if terminalStatusANSIEnabled() {
		col++ // Leading padding in the gray prompt band.
	}
	return maxInt(col, 1)
}

func fitPromptLine(line string, width int) string {
	if width <= 0 || terminalDisplayWidth(stripANSI(line)) <= width {
		return line
	}
	plain := stripANSI(line)
	if width == 1 {
		return "…"
	}
	return terminalFitCells(plain, width-1) + "…"
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

func terminalComposerBandRows(layout terminalComposerLayout, color bool, width int) []string {
	rows := make([]string, 0, len(layout.Rows)+2)
	if color {
		rows = append(rows, formatPromptBandRow("", "", width))
		for _, row := range layout.Rows {
			rows = append(rows, formatPromptBandRow(" "+row, ansiGrayFG, width))
		}
		return append(rows, formatPromptBandRow("", "", width))
	}
	rows = append(rows, "")
	for _, row := range layout.Rows {
		rows = append(rows, " "+terminalFitCells(row, maxInt(width-1, 0)))
	}
	return append(rows, "")
}

func formatCommandInputBandRows(prompt, line string, color bool, width int) []string {
	if !color {
		return []string{formatCommandInputLine(prompt, line, false, 0)}
	}
	blank := formatPromptBandRow("", "", width)
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
		return formatPromptBandRow(text, ansiGrayFG, width)
	}
	return ansiUserBG + ansiGrayFG + text + ansiReset
}

func fitPromptBandText(text string, width int) string {
	if width <= 0 {
		return text
	}
	plain := stripANSI(text)
	cellWidth := terminalDisplayWidth(plain)
	if cellWidth > width {
		if width == 1 {
			return "…"
		}
		return terminalFitCells(plain, width-1) + "…"
	}
	return plain + strings.Repeat(" ", width-cellWidth)
}

func filterSlashSuggestions(prefix string) []SlashCommandSuggestion {
	return filterSlashSuggestionsForClient(prefix, nil)
}

func filterSlashSuggestionsForClient(prefix string, client *Client) []SlashCommandSuggestion {
	if !strings.HasPrefix(prefix, "/") {
		return nil
	}
	suggestions := slashCommandSuggestionsForClient(client)
	if prefix == "/" {
		return suggestions
	}
	filtered := make([]SlashCommandSuggestion, 0, len(suggestions))
	for _, s := range suggestions {
		if strings.HasPrefix(s.Text, prefix) || strings.HasPrefix(strings.TrimSpace(s.Text), prefix) || slashSuggestionHasAliasPrefix(s, prefix) {
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

// boundedSlashMenuLines returns a contiguous command window that fits maxRows.
// When room permits, the help header stays visible; otherwise the selected row
// takes priority. A zero bound deliberately renders no menu, which keeps
// callers safe on terminals with no usable space above the prompt. A negative
// bound means the caller has no terminal-size information and keeps all rows.
func boundedSlashMenuLines(lines []string, maxRows int) []string {
	if len(lines) == 0 || maxRows == 0 {
		return nil
	}
	if maxRows < 0 || len(lines) <= maxRows {
		return lines
	}

	selected := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "›") {
			selected = i
			break
		}
	}
	if selected <= 0 {
		return lines[:maxRows]
	}
	if maxRows == 1 {
		return lines[selected : selected+1]
	}

	// Reserve one row for help and fill the remaining rows with a contiguous
	// command range that contains the selection. This naturally recalculates
	// after filtering, wrap-around navigation, or a terminal resize.
	commandRows := maxRows - 1
	start := selected - commandRows + 1
	if start < 1 {
		start = 1
	}
	end := start + commandRows
	if end > len(lines) {
		end = len(lines)
		start = maxInt(1, end-commandRows)
	}
	visible := make([]string, 0, maxRows)
	visible = append(visible, lines[0])
	visible = append(visible, lines[start:end]...)
	return visible
}

// terminalSlashMenuMaxRows bounds the non-fixed fallback renderer when the
// terminal reports a size. The fixed footer uses its precise prompt boundary.
// Unknown terminal sizes retain the historical unbounded behavior.
func terminalSlashMenuMaxRows() int {
	_, height, err := term.GetSize(terminalStderrFD())
	if err != nil || height <= 0 {
		return -1
	}
	return maxInt(height-2, 1)
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
	return s.Behavior != SlashCommandTemplate && !strings.HasSuffix(s.Text, " ")
}

func slashSuggestionHasAliasPrefix(s SlashCommandSuggestion, prefix string) bool {
	for _, alias := range s.Aliases {
		if strings.HasPrefix(alias, prefix) {
			return true
		}
	}
	return false
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

func promptApprovalTable(rows [][2]string, message string, defaultYes bool) (bool, error) {
	// During an active turn the terminal prompt runs a background typeahead
	// capture that owns stdin until the turn finishes. Approval is itself part of
	// the turn, so leaving that capture active deadlocks: the approval UI waits
	// for stdin, while the turn waits for the approval response. Stop it first;
	// Stop preserves any partially typed command in pendingCommandInput.
	resumeInputCapture := pauseProcessingInputCapture()
	defer resumeInputCapture()
	if strings.TrimSpace(message) != "" {
		rows = append([][2]string{{"Question", singleLineLabel(message)}}, rows...)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(terminalStderrFD()) {
		printApprovalTable(rows, -1)
		return promptYesNo("Approve", defaultYes)
	}

	selected := 1
	if defaultYes {
		selected = 0
	}
	stdinState.Lock()
	defer stdinState.Unlock()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		printApprovalTable(rows, -1)
		return promptYesNo("Approve", defaultYes)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	enterAlternatePickerScreen()
	defer leaveAlternatePickerScreen()

	redraw := func() {
		fmt.Fprint(os.Stderr, ansiReset+"\x1b[H\x1b[2J")
		printApprovalTable(rows, selected)
	}
	redraw()
	for {
		event, err := readTerminalInputEvent(int(os.Stdin.Fd()))
		if err != nil {
			return false, err
		}
		switch event.Kind {
		case terminalInputEnter, terminalInputNewline:
			return selected == 0, nil
		case terminalInputText:
			switch event.Rune {
			case 'y', 'Y':
				return true, nil
			case 'n', 'N', 'q', 'Q':
				return false, nil
			}
		case terminalInputPaste:
			preserveTerminalPasteForNextPrompt(event)
		case terminalInputCtrlC:
			fmt.Fprint(os.Stderr, "^C\r\n")
			return false, errors.New("interrupted")
		case terminalInputCtrlD, terminalInputEscape:
			return false, nil
		case terminalInputTab:
			selected = 1 - selected
			redraw()
		case terminalInputUp, terminalInputDown:
			selected = 1 - selected
			redraw()
		}
	}
}

func printApprovalTable(rows [][2]string, selected int) {
	width, _, err := term.GetSize(terminalStderrFD())
	if err != nil || width <= 0 {
		width = 100
	}
	if width < 60 {
		width = 60
	}
	color := pickerColorEnabled()
	fmt.Fprintf(os.Stderr, "%s\r\n", pickerHeader("Approval required", "↑/↓ choose · Enter confirm · y/n shortcut · q reject", color))
	fmt.Fprintf(os.Stderr, "%s\r\n", approvalRule(width, color))
	keyWidth := 14
	valueWidth := width - keyWidth - 7
	for _, row := range rows {
		key := approvalClip(singleLineLabel(row[0]), keyWidth)
		value := singleLineLabel(row[1])
		if value == "" {
			value = "-"
		}
		wrapped := wrapReplayLine(value, valueWidth)
		if len(wrapped) == 0 {
			wrapped = []string{""}
		}
		for i, part := range wrapped {
			left := ""
			if i == 0 {
				left = key
			}
			fmt.Fprintf(os.Stderr, "│ %-*s │ %-*s │\r\n", keyWidth, approvalClip(left, keyWidth), valueWidth, approvalClip(part, valueWidth))
		}
	}
	fmt.Fprintf(os.Stderr, "%s\r\n\r\n", approvalRule(width, color))
	choices := []struct {
		label string
		ok    bool
	}{
		{label: "Approve", ok: true},
		{label: "Reject", ok: false},
	}
	for i, choice := range choices {
		selector := " "
		if selected == i {
			selector = style("›", ansiWasabiGreen, color)
		}
		label := choice.label
		if choice.ok {
			label = "✓ " + label
			label = style(label, ansiWasabiGreen+ansiBold, color)
		} else {
			label = "✗ " + label
			label = style(label, ansiRed+ansiBold, color)
		}
		fmt.Fprintf(os.Stderr, "%s %s\r\n", selector, label)
	}
}

func approvalRule(width int, color bool) string {
	count := width - 1
	if count < 0 {
		count = 0
	}
	line := strings.Repeat("─", count)
	return style(line, ansiDim, color)
}

func approvalClip(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(stripANSI(s))
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

type processingInputCapture struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	cancel   func() bool
	prompt   string
	status   *statusBarState
}

type connectingInputCapture struct {
	stop        chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
	cleanupOnce sync.Once
	cancel      func()
	fd          int
	oldState    *term.State
}

func startConnectingInputCapture(cancel func()) func() {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(terminalStderrFD()) || cancel == nil {
		return func() {}
	}
	// Prepare raw input synchronously so the first rendered frame
	// never advertises Esc cancellation before stdin is actually ready.
	stdinState.Lock()
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		stdinState.Unlock()
		return func() {}
	}
	capture := &connectingInputCapture{stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel, fd: fd, oldState: oldState}
	go capture.run()
	return capture.Stop
}

func (c *connectingInputCapture) Stop() {
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.done
	c.cleanupOnce.Do(func() {
		_ = term.Restore(c.fd, c.oldState)
		stdinState.Unlock()
	})
}

func (c *connectingInputCapture) run() {
	defer close(c.done)
	buf := make([]byte, 16)
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		n, err := readTerminalFD(c.fd, buf)
		if n > 0 && connectingCancelRequested(buf[:n]) {
			c.cancel()
			return
		}
		if err != nil && !terminalReadWouldBlock(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func connectingCancelRequested(input []byte) bool {
	for _, b := range input {
		if b == 27 || b == 3 {
			return true
		}
	}
	return false
}

func startProcessingInputCapture(prompt string, status *statusBarState, cancel func() bool) *processingInputCapture {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(terminalStderrFD()) || status == nil {
		return nil
	}
	capture := &processingInputCapture{stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel, prompt: prompt, status: status}
	setProcessingInputCapture(capture)
	go capture.run(prompt, *status)
	return capture
}

func (c *processingInputCapture) handleTurnInterruptInput(kind terminalInputEventKind) bool {
	if kind != terminalInputEscape && kind != terminalInputCtrlC {
		return false
	}
	if c != nil && c.cancel != nil {
		c.cancel()
	}
	return true
}

func (c *processingInputCapture) Stop() {
	if c == nil {
		return
	}
	processingInputState.Lock()
	current := processingInputState.capture
	processingInputState.Unlock()
	if current != nil && current != c {
		current.Stop()
		return
	}
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.done
}

func (c *processingInputCapture) run(prompt string, status statusBarState) {
	defer close(c.done)
	defer clearProcessingInputCapture(c)
	stdinState.Lock()
	defer stdinState.Unlock()
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return
	}
	terminalControls := !strings.EqualFold(os.Getenv("TERM"), "dumb")
	if terminalControls {
		terminalRenderMu.Lock()
		_ = writeTerminalString(os.Stderr, "\x1b[?2004h"+ansiCursorShow)
		terminalRenderMu.Unlock()
	}
	defer func() {
		if terminalControls {
			terminalRenderMu.Lock()
			_ = writeTerminalString(os.Stderr, "\x1b[?2004l"+ansiReset+ansiCursorShow)
			terminalRenderMu.Unlock()
		}
		_ = term.Restore(fd, oldState)
	}()

	line, cursor, submitted := peekPendingCommandInputAt()
	composer := newTerminalComposerAt(line, cursor)
	history := commandHistorySnapshot()
	historyIndex := len(history)
	draftLine, draftCursor := composer.Text(), composer.CursorByte()
	draftSaved := false
	currentStatus := func() statusBarState {
		lastTerminalFooterStatus.Lock()
		defer lastTerminalFooterStatus.Unlock()
		if lastTerminalFooterStatus.set {
			return lastTerminalFooterStatus.state
		}
		return status
	}
	draw := func() {
		terminalRenderMu.Lock()
		defer terminalRenderMu.Unlock()
		beginTerminalFrame()
		defer endTerminalFrame()
		drawTerminalFooterPromptUnlocked(prompt, composer.Text(), composer.CursorByte(), currentStatus())
	}
	persist := func() {
		setPendingCommandInputAt(composer.Text(), composer.CursorByte(), submitted)
	}
	contentWidth := func() int {
		width, _, sizeErr := term.GetSize(terminalStderrFD())
		if sizeErr != nil || width <= 0 {
			width = terminalStatusWidth()
		}
		return maxInt(1, width-terminalDisplayWidth(prompt)-2)
	}
	deleteToLogicalEnd := func() bool {
		text := composer.Text()
		cursor := composer.CursorByte()
		if cursor >= len(text) {
			return false
		}
		end := len(text)
		if newline := strings.IndexByte(text[cursor:], '\n'); newline >= 0 {
			end = cursor + newline
		}
		if end == cursor {
			return false
		}
		composer.Reset(text[:cursor]+text[end:], cursor)
		return true
	}
	deleteToLogicalStart := func() bool {
		text := composer.Text()
		cursor := composer.CursorByte()
		start := 0
		if newline := strings.LastIndexByte(text[:cursor], '\n'); newline >= 0 {
			start = newline + 1
		}
		if start == cursor {
			return false
		}
		composer.Reset(text[:start]+text[cursor:], start)
		return true
	}

	draw()
	stopResizeNotifications := startTerminalResizeNotifications(func() {
		terminalRenderMu.Lock()
		defer terminalRenderMu.Unlock()
		beginTerminalFrame()
		defer endTerminalFrame()
		pendingLine, pendingCursor, _ := peekPendingCommandInputAt()
		drawTerminalFooterPromptUnlocked(prompt, pendingLine, pendingCursor, currentStatus())
	})
	defer stopResizeNotifications()
	for {
		event, readErr := readTerminalInputEventUntil(fd, c.stop)
		if readErr != nil {
			if errors.Is(readErr, errTerminalInputCanceled) {
				persist()
				draw()
				return
			}
			if terminalReadWouldBlock(readErr) {
				continue
			}
			persist()
			return
		}

		if c.handleTurnInterruptInput(event.Kind) {
			continue
		}
		if submitted {
			continue
		}

		changed := false
		switch event.Kind {
		case terminalInputText:
			if !unicode.IsControl(event.Rune) {
				changed = composer.Insert(string(event.Rune))
			}
		case terminalInputPaste:
			if event.Truncated {
				showTerminalFooterTempMessageWithStyle(currentStatus(), "Paste rejected: input exceeds 1 MiB", 2*time.Second, ansiYellow+ansiBold)
			} else {
				changed = composer.Insert(event.Text)
			}
		case terminalInputEnter:
			submitted = true
			changed = true
		case terminalInputNewline:
			changed = composer.Insert("\n")
		case terminalInputBackspace:
			changed = composer.BackspaceGrapheme()
		case terminalInputDelete:
			changed = composer.DeleteGrapheme()
		case terminalInputLeft:
			if event.Modifiers&(terminalInputModifierAlt|terminalInputModifierCtrl) != 0 {
				changed = composer.MoveWordLeft()
			} else {
				changed = composer.MoveGraphemeLeft()
			}
		case terminalInputRight:
			if event.Modifiers&(terminalInputModifierAlt|terminalInputModifierCtrl) != 0 {
				changed = composer.MoveWordRight()
			} else {
				changed = composer.MoveGraphemeRight()
			}
		case terminalInputUp:
			if composer.MoveVisualUp(contentWidth()) {
				changed = true
				break
			}
			if len(history) > 0 && historyIndex > 0 {
				if historyIndex == len(history) && !draftSaved {
					draftLine, draftCursor = composer.Text(), composer.CursorByte()
					draftSaved = true
				}
				historyIndex--
				composer.Reset(history[historyIndex], len(history[historyIndex]))
				changed = true
			}
		case terminalInputDown:
			if composer.MoveVisualDown(contentWidth()) {
				changed = true
				break
			}
			if historyIndex < len(history)-1 {
				historyIndex++
				composer.Reset(history[historyIndex], len(history[historyIndex]))
				changed = true
			} else if historyIndex < len(history) {
				historyIndex = len(history)
				if draftSaved {
					composer.Reset(draftLine, draftCursor)
				} else {
					composer.Reset("", 0)
				}
				draftSaved = false
				changed = true
			}
		case terminalInputHome, terminalInputCtrlA:
			changed = composer.MoveLogicalHome()
		case terminalInputEnd, terminalInputCtrlE:
			changed = composer.MoveLogicalEnd()
		case terminalInputCtrlB:
			changed = composer.MoveGraphemeLeft()
		case terminalInputCtrlF:
			changed = composer.MoveGraphemeRight()
		case terminalInputCtrlK:
			changed = deleteToLogicalEnd()
		case terminalInputCtrlU:
			changed = deleteToLogicalStart()
		case terminalInputCtrlW:
			changed = composer.DeleteWordBackward()
		}
		if changed {
			persist()
			draw()
		}
	}
}

func setProcessingInputCapture(capture *processingInputCapture) {
	processingInputState.Lock()
	processingInputState.active = capture != nil
	processingInputState.capture = capture
	processingInputState.Unlock()
}

func clearProcessingInputCapture(capture *processingInputCapture) {
	processingInputState.Lock()
	if processingInputState.capture == capture {
		processingInputState.active = false
		processingInputState.capture = nil
	}
	processingInputState.Unlock()
}

func processingInputCaptureActive() bool {
	processingInputState.Lock()
	defer processingInputState.Unlock()
	return processingInputState.active
}

func suspendProcessingInputCapture() bool {
	processingInputState.Lock()
	capture := processingInputState.capture
	processingInputState.Unlock()
	if capture == nil {
		return false
	}
	capture.Stop()
	return true
}

func pauseProcessingInputCapture() func() {
	processingInputState.Lock()
	capture := processingInputState.capture
	processingInputState.Unlock()
	if capture == nil {
		return func() {}
	}
	prompt, status, cancel := capture.prompt, capture.status, capture.cancel
	capture.Stop()
	return func() {
		if status != nil {
			startProcessingInputCapture(prompt, status, cancel)
		}
	}
}

func setPendingCommandInput(line string, submitted bool) {
	setPendingCommandInputAt(line, len(line), submitted)
}

func setPendingCommandInputAt(line string, cursor int, submitted bool) {
	pendingCommandInput.Lock()
	pendingCommandInput.line = line
	pendingCommandInput.cursor = maxInt(0, minInt(cursor, len(line)))
	pendingCommandInput.submitted = submitted
	pendingCommandInput.Unlock()
}

func peekPendingCommandInput() (string, bool) {
	line, _, submitted := peekPendingCommandInputAt()
	return line, submitted
}

func peekPendingCommandInputAt() (string, int, bool) {
	pendingCommandInput.Lock()
	defer pendingCommandInput.Unlock()
	return pendingCommandInput.line, pendingCommandInput.cursor, pendingCommandInput.submitted
}

// preserveTerminalPasteForNextPrompt keeps bracketed paste from being lost if
// it straddles the transition from background typeahead capture to an approval
// or picker. Modal screens never interpret pasted text as navigation or an
// approval response; the next command editor receives it verbatim instead.
func preserveTerminalPasteForNextPrompt(event terminalInputEvent) {
	if event.Kind != terminalInputPaste || event.Truncated || event.Text == "" {
		return
	}
	line, cursor, submitted := peekPendingCommandInputAt()
	if submitted {
		return
	}
	composer := newTerminalComposerAt(line, cursor)
	if composer.Insert(event.Text) {
		setPendingCommandInputAt(composer.Text(), composer.CursorByte(), false)
	}
}

func takePendingCommandInput() (string, bool) {
	line, _, submitted := takePendingCommandInputAt()
	return line, submitted
}

func takePendingCommandInputAt() (string, int, bool) {
	pendingCommandInput.Lock()
	defer pendingCommandInput.Unlock()
	line, cursor, submitted := pendingCommandInput.line, pendingCommandInput.cursor, pendingCommandInput.submitted
	pendingCommandInput.line = ""
	pendingCommandInput.cursor = 0
	pendingCommandInput.submitted = false
	return line, cursor, submitted
}

type conversationPickerOption struct {
	Value   string
	Label   string
	Current bool
	Global  bool
}

func promptInstanceSelection(instances []ProfileInfo, currentProfile string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(terminalStderrFD()) {
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
		if instance.HasLogin {
			cred = append(cred, "stored-login")
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
		fmt.Fprint(os.Stderr, ansiReset+"\x1b[H\x1b[2J")
		lines := instancePickerLines(options, selected)
		_, height, sizeErr := term.GetSize(terminalStderrFD())
		maxLines := len(lines)
		if sizeErr == nil && height > 2 && maxLines > height-1 {
			maxLines = height - 1
		}
		for _, line := range lines[:maxLines] {
			fmt.Fprintf(os.Stderr, "%s\r\n", line)
		}
	}
	redraw()
	for {
		event, err := readTerminalInputEvent(int(os.Stdin.Fd()))
		if err != nil {
			return "", err
		}
		switch event.Kind {
		case terminalInputEnter, terminalInputNewline:
			return options[selected].Value, nil
		case terminalInputTab:
			selected = (selected + 1) % len(options)
			redraw()
		case terminalInputText:
			if event.Rune == 'q' || event.Rune == 'Q' {
				return "__cancel__", nil
			}
		case terminalInputPaste:
			preserveTerminalPasteForNextPrompt(event)
		case terminalInputCtrlC:
			fmt.Fprint(os.Stderr, "^C\r\n")
			return "", errors.New("interrupted")
		case terminalInputCtrlD, terminalInputEscape:
			return "__cancel__", nil
		case terminalInputUp:
			selected--
			if selected < 0 {
				selected = len(options) - 1
			}
			redraw()
		case terminalInputDown:
			selected++
			if selected >= len(options) {
				selected = 0
			}
			redraw()
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
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(terminalStderrFD()) {
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
		fmt.Fprint(os.Stderr, ansiReset+"\x1b[H\x1b[2J")
		lines := conversationPickerLines(options, selected)
		_, height, sizeErr := term.GetSize(terminalStderrFD())
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
	for {
		event, err := readTerminalInputEvent(int(os.Stdin.Fd()))
		if err != nil {
			return "", err
		}
		switch event.Kind {
		case terminalInputEnter, terminalInputNewline:
			return options[selected].Value, nil
		case terminalInputTab:
			selected++
			if selected >= len(options) {
				selected = 0
			}
			redraw()
		case terminalInputText:
			if (event.Rune == 'n' || event.Rune == 'N') && allowNew {
				return "__new__", nil
			}
			if event.Rune == 'q' || event.Rune == 'Q' {
				return "__cancel__", nil
			}
		case terminalInputPaste:
			preserveTerminalPasteForNextPrompt(event)
		case terminalInputCtrlC:
			fmt.Fprint(os.Stderr, "^C\r\n")
			return "", errors.New("interrupted")
		case terminalInputCtrlD, terminalInputEscape:
			return "__cancel__", nil
		case terminalInputUp:
			selected--
			if selected < 0 {
				selected = len(options) - 1
			}
			redraw()
		case terminalInputDown:
			selected++
			if selected >= len(options) {
				selected = 0
			}
			redraw()
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
			Label:   conversationLabelWithCurrent(conv, current),
			Current: current,
			Global:  strings.TrimSpace(conv.ApplicationID) == "",
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
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(terminalStderrFD()) {
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
		fmt.Fprint(os.Stderr, ansiReset+"\x1b[H\x1b[2J")
		lines := workspacePickerLines(options, selected)
		_, height, sizeErr := term.GetSize(terminalStderrFD())
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
	for {
		event, err := readTerminalInputEvent(int(os.Stdin.Fd()))
		if err != nil {
			return "", err
		}
		switch event.Kind {
		case terminalInputEnter, terminalInputNewline:
			return options[selected].Value, nil
		case terminalInputTab:
			selected++
			if selected >= len(options) {
				selected = 0
			}
			redraw()
		case terminalInputText:
			if event.Rune == 'q' || event.Rune == 'Q' {
				return "__cancel__", nil
			}
		case terminalInputPaste:
			preserveTerminalPasteForNextPrompt(event)
		case terminalInputCtrlC:
			fmt.Fprint(os.Stderr, "^C\r\n")
			return "", errors.New("interrupted")
		case terminalInputCtrlD, terminalInputEscape:
			return "__cancel__", nil
		case terminalInputUp:
			selected--
			if selected < 0 {
				selected = len(options) - 1
			}
			redraw()
		case terminalInputDown:
			selected++
			if selected >= len(options) {
				selected = 0
			}
			redraw()
		}
	}
}

func promptAppSelection(choices []AppChoice) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(terminalStderrFD()) {
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
		fmt.Fprint(os.Stderr, ansiReset+"\x1b[H\x1b[2J")
		lines := appPickerLines(options, selected)
		_, height, sizeErr := term.GetSize(terminalStderrFD())
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
	for {
		event, err := readTerminalInputEvent(int(os.Stdin.Fd()))
		if err != nil {
			return "", err
		}
		switch event.Kind {
		case terminalInputEnter, terminalInputNewline:
			return options[selected].Value, nil
		case terminalInputTab:
			selected++
			if selected >= len(options) {
				selected = 0
			}
			redraw()
		case terminalInputText:
			if event.Rune == 'q' || event.Rune == 'Q' {
				return "__cancel__", nil
			}
		case terminalInputPaste:
			preserveTerminalPasteForNextPrompt(event)
		case terminalInputCtrlC:
			fmt.Fprint(os.Stderr, "^C\r\n")
			return "", errors.New("interrupted")
		case terminalInputCtrlD, terminalInputEscape:
			return "__cancel__", nil
		case terminalInputUp:
			selected--
			if selected < 0 {
				selected = len(options) - 1
			}
			redraw()
		case terminalInputDown:
			selected++
			if selected >= len(options) {
				selected = 0
			}
			redraw()
		}
	}
}

func enterTerminalAppScreen() bool {
	if !interactiveTerminalUIEnabled() {
		return false
	}
	terminalAppScreenState.Lock()
	if terminalAppScreenState.active {
		terminalAppScreenState.Unlock()
		return false
	}
	terminalAppScreenState.active = true
	terminalAppScreenState.Unlock()
	// Save the user's current terminal screen and run the interactive REPL inside
	// the alternate screen. On exit, the original shell screen is restored. Do not
	// clear alternate-screen scrollback by default: terminals that expose it should
	// still let the user scroll through the Build Agent transcript. Reset the
	// scrolling region and autowrap only after entering 1049, so iTerm2 cannot
	// inherit a stale main-screen margin/pending-wrap state into the logo frame.
	terminalRenderMu.Lock()
	fmt.Fprintf(os.Stderr, "\x1b[?1049h\x1b[?7h\x1b[?25h\x1b[r%s", terminalFullScreenClearAndPurgeHistorySequence())
	terminalRenderMu.Unlock()
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
	terminalRenderMu.Lock()
	fmt.Fprint(os.Stderr, "\x1b[?2004l\x1b[?7h\x1b[?25h\x1b[?2026l\x1b[r\x1b[?1049l")
	terminalRenderMu.Unlock()
}

func terminalAppScreenActive() bool {
	terminalAppScreenState.Lock()
	defer terminalAppScreenState.Unlock()
	return terminalAppScreenState.active
}

func clearTerminalAppScrollback() {
	if terminalAppScreenActive() && os.Getenv("BA_CLI_CLEAR_ALT_SCROLLBACK") == "1" {
		fmt.Fprint(os.Stderr, ansiReset+"\x1b[3J")
	}
}

func enterAlternatePickerScreen() {
	terminalRenderMu.Lock()
	defer terminalRenderMu.Unlock()
	terminalPickerState.Lock()
	terminalPickerState.active = true
	terminalPickerState.replayDeferred = false
	terminalPickerState.Unlock()
	// Conversation selection is an overlay-style UI. Outside the REPL app screen,
	// use the terminal alternate screen so closing/canceling restores the previous
	// scrollback. Inside the app alternate screen, do not nest 1049 screens;
	// clear the app screen and let leaveAlternatePickerScreen replay the app UI.
	if terminalAppScreenActive() {
		fmt.Fprint(os.Stderr, ansiReset+"\x1b[r\x1b[H\x1b[2J")
		return
	}
	fmt.Fprint(os.Stderr, ansiReset+"\x1b[?1049h\x1b[r\x1b[H\x1b[2J")
}

func leaveAlternatePickerScreen() {
	// Keep teardown in the same render -> picker lock order as enter and async
	// append. In particular, do not publish active=false while the alternate
	// picker screen still owns the terminal: a remote append in that gap would be
	// written into the picker and then discarded by 1049l.
	terminalRenderMu.Lock()
	terminalPickerState.Lock()
	terminalPickerState.active = false
	replayDeferred := terminalPickerState.replayDeferred
	terminalPickerState.replayDeferred = false
	terminalPickerState.Unlock()
	if terminalAppScreenActive() {
		beginTerminalFrame()
		lastTerminalFooterStatus.Lock()
		status, statusSet := lastTerminalFooterStatus.state, lastTerminalFooterStatus.set
		lastTerminalFooterStatus.Unlock()
		replayed := false
		if statusSet {
			_, replayed = terminalReplayManagedViewportUnlocked(status)
		}
		if replayed {
			if !redrawTerminalActivePromptUnlocked() {
				redrawPendingFooterPromptFromStateUnlocked()
			}
		} else {
			fmt.Fprint(os.Stderr, ansiReset+"\x1b[r\x1b[H\x1b[2J")
		}
		endTerminalFrame()
		terminalRenderMu.Unlock()
		return
	}
	fmt.Fprint(os.Stderr, "\x1b[?1049l")
	terminalRenderMu.Unlock()
	if replayDeferred {
		replayManagedViewportFromLastStatus()
	}
}

func conversationPickerLines(options []conversationPickerOption, selected int) []string {
	color := pickerColorEnabled()
	lines := []string{pickerHeader("Build Agent conversations", "↑/↓ choose · Enter open · n new · q cancel", color)}
	if len(options) == 0 {
		return append(lines, style("  <none found>", ansiDim, color))
	}
	for _, option := range options {
		if option.Global {
			lines = append(lines, style("  🌐 Global / no app conversations are available across workspaces.", ansiDim, color))
			break
		}
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
