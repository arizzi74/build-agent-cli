//go:build !darwin

package core

import (
	"context"
	"errors"
)

func readClipboardAttachment(context.Context) (string, string, []byte, error) {
	return "", "", nil, errors.New("clipboard image capture is available when bacli runs on macOS; use /attach add <path> on this host")
}
