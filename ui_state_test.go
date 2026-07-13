package main

import (
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
