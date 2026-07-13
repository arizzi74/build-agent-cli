//go:build !windows

package main

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func setTerminalNonblock(fd int, enabled bool) error {
	return syscall.SetNonblock(fd, enabled)
}

func readTerminalFD(fd int, buffer []byte) (int, error) {
	return syscall.Read(fd, buffer)
}

func terminalReadWouldBlock(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK)
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
