//go:build windows

package main

import (
	"errors"
	"os"
)

var errWindowsTerminalWouldBlock = errors.New("terminal read would block")

func setTerminalNonblock(fd int, enabled bool) error {
	return nil
}

func readTerminalFD(fd int, buffer []byte) (int, error) {
	file := os.NewFile(uintptr(fd), "terminal-input")
	if file == nil {
		return 0, errors.New("invalid terminal handle")
	}
	return file.Read(buffer)
}

func terminalReadWouldBlock(err error) bool {
	return errors.Is(err, errWindowsTerminalWouldBlock)
}

func startTerminalResizeNotifications(redraw func()) func() {
	return func() {}
}
