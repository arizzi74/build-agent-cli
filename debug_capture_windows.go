//go:build windows

package main

import (
	"io"
	"os"
	"strings"
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
	setDebugTraceWriter(&lockedWriter{w: file})
	cleanup := func() error {
		setDebugTraceWriter(io.Discard)
		return file.Close()
	}
	return cleanup, file, nil
}
