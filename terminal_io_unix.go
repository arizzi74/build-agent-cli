//go:build !windows

package main

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func readTerminalFD(fd int, buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	// Do not use F_SETFL(O_NONBLOCK) on stdin. In a PTY, stdin, stdout, and
	// stderr are commonly duplicates of the same open-file description, so
	// changing fd 0 also makes terminal output nonblocking. Darwin's smaller
	// PTY output queue then exposes partial ANSI frames that Linux often masks.
	// Polling provides the nonblocking input semantics the UI needs without
	// changing any shared descriptor flags.
	ready, err := unix.Poll([]unix.PollFd{{
		Fd:     int32(fd),
		Events: unix.POLLIN,
	}}, 0)
	if err != nil {
		return 0, err
	}
	if ready == 0 {
		return 0, syscall.EAGAIN
	}
	return syscall.Read(fd, buffer)
}

func terminalReadWouldBlock(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR)
}

func startTerminalResizeNotifications(redraw func()) func() {
	resizeSignals := make(chan os.Signal, 1)
	resizeDone := make(chan struct{})
	signal.Notify(resizeSignals, syscall.SIGWINCH)
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
				redraw()
			}
		}
	}()
	return func() {
		signal.Stop(resizeSignals)
		close(resizeDone)
	}
}
