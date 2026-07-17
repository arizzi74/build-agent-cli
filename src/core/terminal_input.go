package core

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// terminalInputEventKind is the semantic input understood by the terminal UI.
// Text and paste payloads are carried by terminalInputEvent.Rune and
// terminalInputEvent.Text respectively. Directional keys keep their modifiers
// in terminalInputEvent.Modifiers so callers can distinguish character and
// word movement without interpreting escape sequences themselves.
type terminalInputEventKind uint8

const (
	terminalInputUnknown terminalInputEventKind = iota
	terminalInputText
	terminalInputPaste
	terminalInputEnter
	terminalInputNewline
	terminalInputTab
	terminalInputEscape
	terminalInputBackspace
	terminalInputDelete
	terminalInputUp
	terminalInputDown
	terminalInputLeft
	terminalInputRight
	terminalInputHome
	terminalInputEnd
	terminalInputCtrlA
	terminalInputCtrlB
	terminalInputCtrlC
	terminalInputCtrlD
	terminalInputCtrlE
	terminalInputCtrlF
	terminalInputCtrlK
	terminalInputCtrlU
	terminalInputCtrlV
	terminalInputCtrlW
)

type terminalInputModifiers uint8

const (
	terminalInputModifierShift terminalInputModifiers = 1 << iota
	terminalInputModifierAlt
	terminalInputModifierCtrl
)

type terminalInputEvent struct {
	Kind      terminalInputEventKind
	Modifiers terminalInputModifiers
	Rune      rune
	Text      string
	Raw       []byte
	Truncated bool
}

const (
	terminalInputReadSize          = 1
	terminalPasteReadSize          = 256
	terminalInputPollInterval      = time.Millisecond
	terminalEscapeContinuationWait = 10 * time.Millisecond
	terminalPasteMaxBytes          = 1 << 20
)

var (
	terminalInputStatesMu    sync.Mutex
	terminalInputStates      = make(map[int]*terminalInputState)
	errTerminalInputCanceled = errors.New("terminal input canceled")
)

type terminalInputState struct {
	mu                   sync.Mutex
	pending              []byte
	resumePasteTruncated bool
}

func terminalInputStateForFD(fd int) *terminalInputState {
	terminalInputStatesMu.Lock()
	defer terminalInputStatesMu.Unlock()
	state := terminalInputStates[fd]
	if state == nil {
		state = &terminalInputState{}
		terminalInputStates[fd] = state
	}
	return state
}

// readTerminalInputEvent blocks until one complete semantic terminal event is
// available. It is intended for a terminal in raw mode. Reads go exclusively
// through readTerminalFD, so decoding never changes the descriptor's file
// status flags. The per-fd pending buffer also means parsing an escape sequence
// cannot discard printable input already returned by the same kernel read.
func readTerminalInputEvent(fd int) (terminalInputEvent, error) {
	return readTerminalInputEventUntil(fd, nil)
}

func readTerminalInputEventUntil(fd int, stop <-chan struct{}) (terminalInputEvent, error) {
	state := terminalInputStateForFD(fd)
	state.mu.Lock()
	defer state.mu.Unlock()

	if len(state.pending) == 0 {
		if err := state.readMoreBlocking(fd, stop); err != nil {
			return terminalInputEvent{}, err
		}
	}

	first := state.pending[0]
	switch first {
	case 0x01:
		return state.takeControl(terminalInputCtrlA), nil
	case 0x02:
		return state.takeControl(terminalInputCtrlB), nil
	case 0x03:
		return state.takeControl(terminalInputCtrlC), nil
	case 0x04:
		return state.takeControl(terminalInputCtrlD), nil
	case 0x05:
		return state.takeControl(terminalInputCtrlE), nil
	case 0x06:
		return state.takeControl(terminalInputCtrlF), nil
	case 0x08, 0x7f:
		return state.takeControl(terminalInputBackspace), nil
	case '\t':
		return state.takeControl(terminalInputTab), nil
	case '\n':
		return state.takeControl(terminalInputNewline), nil
	case 0x0b:
		return state.takeControl(terminalInputCtrlK), nil
	case '\r':
		return state.takeControl(terminalInputEnter), nil
	case 0x15:
		return state.takeControl(terminalInputCtrlU), nil
	case 0x16:
		return state.takeControl(terminalInputCtrlV), nil
	case 0x17:
		return state.takeControl(terminalInputCtrlW), nil
	case 0x1b:
		return state.readEscapeEvent(fd, stop)
	}

	if first < utf8.RuneSelf {
		state.pending = state.pending[1:]
		return terminalInputEvent{Kind: terminalInputText, Rune: rune(first)}, nil
	}

	for !utf8.FullRune(state.pending) {
		if len(state.pending) >= utf8.UTFMax {
			break
		}
		if err := state.readMoreBlocking(fd, stop); err != nil {
			if err == io.EOF {
				return terminalInputEvent{}, io.ErrUnexpectedEOF
			}
			return terminalInputEvent{}, err
		}
	}
	r, size := utf8.DecodeRune(state.pending)
	state.pending = state.pending[size:]
	return terminalInputEvent{Kind: terminalInputText, Rune: r}, nil
}

func (state *terminalInputState) takeControl(kind terminalInputEventKind) terminalInputEvent {
	raw := []byte{state.pending[0]}
	state.pending = state.pending[1:]
	return terminalInputEvent{Kind: kind, Raw: raw}
}

func (state *terminalInputState) readEscapeEvent(fd int, stop <-chan struct{}) (terminalInputEvent, error) {
	if len(state.pending) == 1 {
		if err := state.fillEscapeContinuation(fd, stop); err != nil {
			return terminalInputEvent{}, err
		}
	}
	if len(state.pending) == 1 {
		return state.takeEscape(1, terminalInputEscape, 0), nil
	}

	switch state.pending[1] {
	case 0x08, 0x7f:
		return state.takeEscape(2, terminalInputCtrlW, terminalInputModifierAlt), nil
	case 'b':
		return state.takeEscape(2, terminalInputLeft, terminalInputModifierAlt), nil
	case 'f':
		return state.takeEscape(2, terminalInputRight, terminalInputModifierAlt), nil
	case '[':
		return state.readCSIEvent(fd, stop)
	case 'O':
		return state.readSS3Event(fd, stop)
	default:
		// macOS terminals commonly encode Option+key as ESC followed by a
		// printable byte. Keep that distinct from a bare Escape so an unsupported
		// Option shortcut cannot accidentally interrupt an active turn.
		if state.pending[1] >= 0x20 && state.pending[1] < 0x7f {
			return state.takeEscape(2, terminalInputUnknown, terminalInputModifierAlt), nil
		}
		return state.takeEscape(1, terminalInputEscape, 0), nil
	}
}

func (state *terminalInputState) readCSIEvent(fd int, stop <-chan struct{}) (terminalInputEvent, error) {
	for {
		if end := terminalCSIEnd(state.pending); end >= 0 {
			raw := append([]byte(nil), state.pending[:end+1]...)
			body := state.pending[2:end]
			final := state.pending[end]
			if final == '~' && bytes.Equal(body, []byte("200")) {
				state.pending = state.pending[end+1:]
				return state.readBracketedPaste(fd, stop)
			}
			event, ok := decodeTerminalCSI(body, final)
			state.pending = state.pending[end+1:]
			if !ok {
				return terminalInputEvent{Kind: terminalInputUnknown, Raw: raw}, nil
			}
			event.Raw = raw
			return event, nil
		}
		before := len(state.pending)
		if err := state.fillEscapeContinuation(fd, stop); err != nil {
			return terminalInputEvent{}, err
		}
		if len(state.pending) == before {
			// The sequence did not finish inside the short escape window. Treat
			// the ESC as standalone and preserve every following byte.
			return state.takeEscape(1, terminalInputEscape, 0), nil
		}
	}
}

func (state *terminalInputState) readSS3Event(fd int, stop <-chan struct{}) (terminalInputEvent, error) {
	if len(state.pending) < 3 {
		if err := state.fillEscapeContinuation(fd, stop); err != nil {
			return terminalInputEvent{}, err
		}
	}
	if len(state.pending) < 3 {
		return state.takeEscape(1, terminalInputEscape, 0), nil
	}

	kind, ok := map[byte]terminalInputEventKind{
		'A': terminalInputUp,
		'B': terminalInputDown,
		'C': terminalInputRight,
		'D': terminalInputLeft,
		'H': terminalInputHome,
		'F': terminalInputEnd,
	}[state.pending[2]]
	if !ok {
		raw := append([]byte(nil), state.pending[:3]...)
		state.pending = state.pending[3:]
		return terminalInputEvent{Kind: terminalInputUnknown, Raw: raw}, nil
	}
	return state.takeEscape(3, kind, 0), nil
}

func (state *terminalInputState) takeEscape(
	count int,
	kind terminalInputEventKind,
	modifiers terminalInputModifiers,
) terminalInputEvent {
	raw := append([]byte(nil), state.pending[:count]...)
	state.pending = state.pending[count:]
	return terminalInputEvent{Kind: kind, Modifiers: modifiers, Raw: raw}
}

func terminalCSIEnd(input []byte) int {
	if len(input) < 3 || input[0] != 0x1b || input[1] != '[' {
		return -1
	}
	for i := 2; i < len(input); i++ {
		if input[i] >= 0x40 && input[i] <= 0x7e {
			return i
		}
	}
	return -1
}

func decodeTerminalCSI(body []byte, final byte) (terminalInputEvent, bool) {
	params := string(body)
	switch final {
	case 'A', 'B', 'C', 'D':
		modifiers, ok := terminalArrowModifiers(params)
		if !ok {
			return terminalInputEvent{}, false
		}
		kind := map[byte]terminalInputEventKind{
			'A': terminalInputUp,
			'B': terminalInputDown,
			'C': terminalInputRight,
			'D': terminalInputLeft,
		}[final]
		return terminalInputEvent{Kind: kind, Modifiers: modifiers}, true
	case 'H':
		if modifiers, ok := terminalArrowModifiers(params); ok {
			return terminalInputEvent{Kind: terminalInputHome, Modifiers: modifiers}, true
		}
	case 'F':
		if modifiers, ok := terminalArrowModifiers(params); ok {
			return terminalInputEvent{Kind: terminalInputEnd, Modifiers: modifiers}, true
		}
	case '~':
		switch params {
		case "1", "7":
			return terminalInputEvent{Kind: terminalInputHome}, true
		case "3":
			return terminalInputEvent{Kind: terminalInputDelete}, true
		case "4", "8":
			return terminalInputEvent{Kind: terminalInputEnd}, true
		}
	case 'u':
		return decodeTerminalCSIU(params)
	}
	return terminalInputEvent{}, false
}

func terminalArrowModifiers(params string) (terminalInputModifiers, bool) {
	if params == "" || params == "1" {
		return 0, true
	}
	parts := strings.Split(params, ";")
	if len(parts) != 2 || parts[0] != "1" {
		return 0, false
	}
	encoded, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	return decodeTerminalModifierParameter(encoded)
}

func decodeTerminalCSIU(params string) (terminalInputEvent, bool) {
	parts := strings.Split(params, ";")
	if len(parts) == 0 || len(parts) > 3 {
		return terminalInputEvent{}, false
	}
	codepoint, err := strconv.Atoi(terminalCSIUSubparameter(parts[0]))
	if err != nil {
		return terminalInputEvent{}, false
	}
	encodedModifiers := 1
	if len(parts) >= 2 {
		encodedModifiers, err = strconv.Atoi(terminalCSIUSubparameter(parts[1]))
		if err != nil {
			return terminalInputEvent{}, false
		}
	}
	modifiers, ok := decodeTerminalModifierParameter(encodedModifiers)
	if !ok {
		return terminalInputEvent{}, false
	}

	switch codepoint {
	case '\r':
		if modifiers&terminalInputModifierShift != 0 {
			return terminalInputEvent{Kind: terminalInputNewline, Modifiers: modifiers}, true
		}
		return terminalInputEvent{Kind: terminalInputEnter, Modifiers: modifiers}, true
	case '\n':
		return terminalInputEvent{Kind: terminalInputNewline, Modifiers: modifiers}, true
	default:
		return terminalInputEvent{}, false
	}
}

func terminalCSIUSubparameter(value string) string {
	if i := strings.IndexByte(value, ':'); i >= 0 {
		return value[:i]
	}
	return value
}

func decodeTerminalModifierParameter(encoded int) (terminalInputModifiers, bool) {
	if encoded < 1 || encoded > 8 {
		return 0, false
	}
	bits := encoded - 1
	var modifiers terminalInputModifiers
	if bits&1 != 0 {
		modifiers |= terminalInputModifierShift
	}
	if bits&2 != 0 {
		modifiers |= terminalInputModifierAlt
	}
	if bits&4 != 0 {
		modifiers |= terminalInputModifierCtrl
	}
	return modifiers, true
}

func (state *terminalInputState) readBracketedPaste(fd int, stop <-chan struct{}) (terminalInputEvent, error) {
	endMarker := []byte("\x1b[201~")
	candidate := make([]byte, 0, len(endMarker))
	payload := make([]byte, 0, terminalInputMin(terminalPasteMaxBytes, len(state.pending)))
	truncated := state.resumePasteTruncated
	state.resumePasteTruncated = false

	appendPayload := func(b byte) {
		if len(payload) < terminalPasteMaxBytes {
			payload = append(payload, b)
		} else {
			truncated = true
		}
	}

	for {
		if len(state.pending) == 0 {
			if err := state.readMoreBlockingSize(fd, stop, terminalPasteReadSize); err != nil {
				if errors.Is(err, errTerminalInputCanceled) {
					state.resumePasteTruncated = truncated
					restored := make([]byte, 0, len("\x1b[200~")+len(payload)+len(candidate)+len(state.pending))
					restored = append(restored, "\x1b[200~"...)
					restored = append(restored, payload...)
					restored = append(restored, candidate...)
					state.pending = append(restored, state.pending...)
				}
				return terminalInputEvent{}, err
			}
		}
		b := state.pending[0]
		state.pending = state.pending[1:]
		candidate = append(candidate, b)
		for len(candidate) > 0 && !bytes.HasPrefix(endMarker, candidate) {
			appendPayload(candidate[0])
			candidate = candidate[1:]
		}
		if bytes.Equal(candidate, endMarker) {
			break
		}
	}

	return terminalInputEvent{
		Kind:      terminalInputPaste,
		Text:      normalizeTerminalPaste(payload),
		Truncated: truncated,
	}, nil
}

func normalizeTerminalPaste(payload []byte) string {
	normalized := make([]byte, 0, len(payload))
	for i := 0; i < len(payload); i++ {
		if payload[i] != '\r' {
			normalized = append(normalized, payload[i])
			continue
		}
		normalized = append(normalized, '\n')
		if i+1 < len(payload) && payload[i+1] == '\n' {
			i++
		}
	}
	valid := strings.ToValidUTF8(string(normalized), "\uFFFD")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, valid)
}

func (state *terminalInputState) readMoreBlocking(fd int, stop <-chan struct{}) error {
	return state.readMoreBlockingSize(fd, stop, terminalInputReadSize)
}

func (state *terminalInputState) readMoreBlockingSize(fd int, stop <-chan struct{}, readSize int) error {
	for {
		if terminalInputStopRequested(stop) {
			return errTerminalInputCanceled
		}
		readBuffer := make([]byte, maxInt(readSize, 1))
		n, err := readTerminalFD(fd, readBuffer)
		if n > 0 {
			state.pending = append(state.pending, readBuffer[:n]...)
			return nil
		}
		if err == nil {
			return io.EOF
		}
		if !terminalReadWouldBlock(err) {
			return err
		}
		time.Sleep(terminalInputPollInterval)
	}
}

func (state *terminalInputState) fillEscapeContinuation(fd int, stop <-chan struct{}) error {
	deadline := time.Now().Add(terminalEscapeContinuationWait)
	for time.Now().Before(deadline) {
		if terminalInputStopRequested(stop) {
			return errTerminalInputCanceled
		}
		readBuffer := make([]byte, terminalInputReadSize)
		n, err := readTerminalFD(fd, readBuffer)
		if n > 0 {
			state.pending = append(state.pending, readBuffer[:n]...)
			return nil
		}
		if err == nil {
			return nil
		}
		if !terminalReadWouldBlock(err) {
			return err
		}
		time.Sleep(terminalInputPollInterval)
	}
	return nil
}

func terminalInputStopRequested(stop <-chan struct{}) bool {
	if stop == nil {
		return false
	}
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

func terminalInputMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}
