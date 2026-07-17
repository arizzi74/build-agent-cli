//go:build !windows

package core

import (
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

func openDebugOutputLog(path string) (func() error, *os.File, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		setDebugTraceWriter(io.Discard)
		return nil, nil, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, err
	}
	lockedLog := &lockedWriter{w: file}
	setDebugTraceWriter(lockedLog)

	stdoutOrigFD, err := unix.Dup(int(os.Stdout.Fd()))
	if err != nil {
		_ = file.Close()
		setDebugTraceWriter(io.Discard)
		return nil, nil, err
	}
	stderrOrigFD, err := unix.Dup(int(os.Stderr.Fd()))
	if err != nil {
		_ = unix.Close(stdoutOrigFD)
		_ = file.Close()
		setDebugTraceWriter(io.Discard)
		return nil, nil, err
	}
	stdoutOrig := os.NewFile(uintptr(stdoutOrigFD), "debug-terminal-stdout")
	stderrOrig := os.NewFile(uintptr(stderrOrigFD), "debug-terminal-stderr")
	setTerminalOutputFiles(stdoutOrig, stderrOrig)

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdoutOrig.Close()
		_ = stderrOrig.Close()
		_ = file.Close()
		setTerminalOutputFiles(nil, nil)
		setDebugTraceWriter(io.Discard)
		return nil, nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stdoutOrig.Close()
		_ = stderrOrig.Close()
		_ = file.Close()
		setTerminalOutputFiles(nil, nil)
		setDebugTraceWriter(io.Discard)
		return nil, nil, err
	}
	if err := unix.Dup2(int(stdoutW.Fd()), int(os.Stdout.Fd())); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		_ = stdoutOrig.Close()
		_ = stderrOrig.Close()
		_ = file.Close()
		setTerminalOutputFiles(nil, nil)
		setDebugTraceWriter(io.Discard)
		return nil, nil, err
	}
	if err := unix.Dup2(int(stderrW.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = unix.Dup2(stdoutOrigFD, int(os.Stdout.Fd()))
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		_ = stdoutOrig.Close()
		_ = stderrOrig.Close()
		_ = file.Close()
		setTerminalOutputFiles(nil, nil)
		setDebugTraceWriter(io.Discard)
		return nil, nil, err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(stdoutOrig, lockedLog), stdoutR)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(stderrOrig, lockedLog), stderrR)
	}()

	cleanup := func() error {
		var firstErr error
		if err := unix.Dup2(stdoutOrigFD, int(os.Stdout.Fd())); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := unix.Dup2(stderrOrigFD, int(os.Stderr.Fd())); err != nil && firstErr == nil {
			firstErr = err
		}
		_ = stdoutW.Close()
		_ = stderrW.Close()
		wg.Wait()
		_ = stdoutR.Close()
		_ = stderrR.Close()
		setTerminalOutputFiles(nil, nil)
		setDebugTraceWriter(io.Discard)
		if err := stdoutOrig.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := stderrOrig.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := file.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	}
	return cleanup, file, nil
}
