package main

import (
	"strings"
	"testing"
	"time"
)

func TestSuspendProcessingInputCaptureStopsRegisteredCapture(t *testing.T) {
	capture := &processingInputCapture{stop: make(chan struct{}), done: make(chan struct{})}
	setProcessingInputCapture(capture)

	stopped := make(chan struct{})
	go func() {
		<-capture.stop
		clearProcessingInputCapture(capture)
		close(capture.done)
		close(stopped)
	}()

	if !processingInputCaptureActive() {
		t.Fatal("processing input capture should be active after registration")
	}
	if !suspendProcessingInputCapture() {
		t.Fatal("suspendProcessingInputCapture returned false for active capture")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("registered capture was not stopped")
	}
	if processingInputCaptureActive() {
		t.Fatal("processing input capture should be inactive after suspend")
	}

	// Main's deferred cleanup calls Stop after runPrompt returns. If approval
	// already suspended the capture, that second Stop must be a no-op, not a
	// close-of-closed-channel panic.
	capture.Stop()
	if suspendProcessingInputCapture() {
		t.Fatal("suspendProcessingInputCapture returned true after capture was cleared")
	}
}

func TestTerminalPickerStateTracksOverlayLifetime(t *testing.T) {
	terminalPickerState.Lock()
	previous := terminalPickerState.active
	terminalPickerState.active = false
	terminalPickerState.Unlock()
	defer func() {
		terminalPickerState.Lock()
		terminalPickerState.active = previous
		terminalPickerState.Unlock()
	}()

	if terminalPickerActive() {
		t.Fatal("picker should start inactive")
	}
	terminalPickerState.Lock()
	terminalPickerState.active = true
	terminalPickerState.Unlock()
	if !terminalPickerActive() {
		t.Fatal("picker should report active while overlay owns the terminal")
	}
}

func TestBoundedSlashMenuLinesKeepsSelectionVisibleWithHeader(t *testing.T) {
	suggestions := make([]SlashCommandSuggestion, 8)
	for i := range suggestions {
		suggestions[i] = SlashCommandSuggestion{Text: "/command-" + string(rune('a'+i))}
	}

	lines := slashMenuLines(true, suggestions, 6)
	visible := boundedSlashMenuLines(lines, 3)
	if len(visible) != 3 {
		t.Fatalf("visible rows = %d, want 3: %#v", len(visible), visible)
	}
	if visible[0] != "  ↑/↓ choose · Enter run · Tab insert" {
		t.Fatalf("help row was not preserved: %#v", visible)
	}
	if !strings.Contains(visible[2], "› /command-g") {
		t.Fatalf("selected command is not visible: %#v", visible)
	}
	if !strings.Contains(visible[1], "/command-f") {
		t.Fatalf("command window should be contiguous before selection: %#v", visible)
	}
}

func TestBoundedSlashMenuLinesHandlesWrapResizeAndTinyTerminals(t *testing.T) {
	suggestions := make([]SlashCommandSuggestion, 6)
	for i := range suggestions {
		suggestions[i] = SlashCommandSuggestion{Text: "/command-" + string(rune('a'+i))}
	}

	// After wrapping from the first item to the last, a resized two-row menu
	// must still show its help and the selected command.
	wrapped := boundedSlashMenuLines(slashMenuLines(true, suggestions, len(suggestions)-1), 2)
	if len(wrapped) != 2 || !strings.HasPrefix(wrapped[1], "›") || !strings.Contains(wrapped[1], "/command-f") {
		t.Fatalf("wrapped selection was clipped: %#v", wrapped)
	}

	// Wrapping back to the first selection must reset the viewport instead of
	// retaining a stale offset from the previous selection.
	first := boundedSlashMenuLines(slashMenuLines(true, suggestions, 0), 2)
	if len(first) != 2 || !strings.Contains(first[1], "› /command-a") {
		t.Fatalf("first selection after wrap was not visible: %#v", first)
	}

	tiny := boundedSlashMenuLines(slashMenuLines(true, suggestions, 4), 1)
	if len(tiny) != 1 || !strings.Contains(tiny[0], "› /command-e") {
		t.Fatalf("one-row menu must prioritize selection: %#v", tiny)
	}
	if got := boundedSlashMenuLines(slashMenuLines(true, suggestions, 4), 0); got != nil {
		t.Fatalf("zero-row menu = %#v, want nil", got)
	}
}

func TestBoundedSlashMenuLinesKeepsNoMatchesWithinRenderingBoundary(t *testing.T) {
	lines := slashMenuLines(true, nil, 0)
	if got := boundedSlashMenuLines(lines, 1); len(got) != 1 || got[0] != "  no matching commands" {
		t.Fatalf("no-match menu = %#v", got)
	}
	if got := boundedSlashMenuLines(lines, -1); len(got) != 1 || got[0] != "  no matching commands" {
		t.Fatalf("unbounded no-match menu = %#v", got)
	}
}
