package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
	"golang.org/x/term"
)

// Script reviews deliberately do not use diagnostic redaction or clipping:
// approving a different, incomplete view of the executable code is unsafe.
// Render control/format characters visibly so code cannot manipulate either
// terminal state or text direction. Newlines remain real review line breaks.
func scriptReviewSafeText(value string) string {
	var out strings.Builder
	for _, r := range strings.ReplaceAll(value, "\r\n", "\n") {
		if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)) {
			fmt.Fprintf(&out, "\\u%04x", r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func formatScriptReview(review turnScriptReview) string {
	var out strings.Builder
	out.WriteString("ServiceNow script review\n")
	for _, row := range [][2]string{
		{"Action", review.Action}, {"Instance", review.Instance}, {"Scope", review.Scope},
		{"Intent", review.Intent}, {"Rollback context", review.RollbackContext},
	} {
		value := row[1]
		if value == "" {
			value = "(not provided)"
		}
		fmt.Fprintf(&out, "%s: %s\n", row[0], scriptReviewSafeText(value))
	}
	out.WriteString("\nRuns on the ServiceNow instance, not your local machine.\n")
	if review.Action == "rollback_script" && review.Script == "" {
		out.WriteString("Rollback applies changes recorded in the context above; no new script is submitted.\n")
		out.WriteString("Review the target instance and rollback context before approving.")
		return out.String()
	}
	out.WriteString("Review the complete script. Control/format characters are shown as \\u escapes.\n")
	out.WriteString("--- BEGIN SCRIPT ---\n")
	out.WriteString(scriptReviewSafeText(review.Script))
	out.WriteString("\n--- END SCRIPT ---")
	return out.String()
}

// scriptReviewLines wraps by terminal cells, retaining every grapheme and
// whitespace. Ordinary approval tables collapse lines and cannot review code.
func scriptReviewLines(text string, width int) []string {
	width = maxInt(width, 2)
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		var part strings.Builder
		cells := 0
		g := uniseg.NewGraphemes(line)
		for g.Next() {
			cluster := g.Str()
			n := uniseg.StringWidth(cluster)
			if cells+n > width && part.Len() > 0 {
				lines = append(lines, part.String())
				part.Reset()
				cells = 0
			}
			part.WriteString(cluster)
			cells += n
		}
		lines = append(lines, part.String())
	}
	return lines
}

func scriptReviewInputDecision(event terminalInputEvent, reviewedEnd bool) (approved, done bool, err error) {
	switch event.Kind {
	case terminalInputCtrlC:
		return false, true, context.Canceled
	case terminalInputEnter, terminalInputNewline, terminalInputEscape, terminalInputCtrlD:
		return false, true, nil
	case terminalInputText:
		switch event.Rune {
		case 'y', 'Y':
			return reviewedEnd, reviewedEnd, nil
		case 'n', 'N', 'q', 'Q':
			return false, true, nil
		}
	}
	return false, false, nil
}

// promptScriptApproval keeps the complete review in a scrollable modal, never
// in a transient/capped progress row. Reject is always available; approving
// requires reaching the last page and pressing y explicitly (Enter rejects).
func promptScriptApproval(ctx context.Context, review turnScriptReview) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(terminalStderrFD()) {
		return false, errors.New("script approval requires an interactive terminal or Telegram; automatic approval is disabled")
	}
	resumeInputCapture := pauseProcessingInputCapture()
	defer resumeInputCapture()
	stdinState.Lock()
	defer stdinState.Unlock()
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return false, fmt.Errorf("cannot open script review: %w", err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	enterAlternatePickerScreen()
	defer leaveAlternatePickerScreen()
	text := formatScriptReview(review)
	offset, reviewedEnd := 0, false
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		width, height, err := term.GetSize(terminalStderrFD())
		if err != nil || width < 20 || height < 6 {
			return false, errors.New("terminal is too small for script review (minimum 20 columns by 6 rows)")
		}
		lines := scriptReviewLines(text, width-1)
		pageSize := height - 4
		maxOffset := maxInt(0, len(lines)-pageSize)
		if offset > maxOffset {
			offset = maxOffset
		}
		end := minInt(offset+pageSize, len(lines))
		reviewedEnd = reviewedEnd || end == len(lines)
		terminalRenderMu.Lock()
		fmt.Fprint(os.Stderr, ansiReset+"\x1b[H\x1b[2J")
		fmt.Fprint(os.Stderr, strings.Join(lines[offset:end], "\r\n"))
		position := fmt.Sprintf("Lines %d-%d/%d", offset+1, end, len(lines))
		fmt.Fprintf(os.Stderr, "\r\n\r\n%s\r\n", terminalFitCells(position, width-1))
		fmt.Fprint(os.Stderr, "Space next; b back\r\n")
		help := "n/Esc rejects"
		if reviewedEnd {
			help = "n reject; y APPROVE"
		}
		fmt.Fprint(os.Stderr, help)
		terminalRenderMu.Unlock()
		event, err := readTerminalInputEventUntil(int(os.Stdin.Fd()), ctx.Done())
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, err
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if approved, done, err := scriptReviewInputDecision(event, reviewedEnd); done {
			return approved, err
		}
		switch event.Kind {
		case terminalInputDown:
			offset = minInt(offset+1, maxOffset)
		case terminalInputUp:
			offset = maxInt(offset-1, 0)
		case terminalInputHome:
			offset = 0
		case terminalInputText:
			switch event.Rune {
			case ' ', 'f', 'F':
				offset = minInt(offset+pageSize, maxOffset)
			case 'b', 'B':
				offset = maxInt(offset-pageSize, 0)
			}
		case terminalInputPaste:
			// Never interpret a paste as authority to execute a script.
			preserveTerminalPasteForNextPrompt(event)
		}
	}
}
