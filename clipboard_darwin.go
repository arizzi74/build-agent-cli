//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const clipboardJXA = `
ObjC.import('AppKit');
function run() {
  const pasteboard = $.NSPasteboard.generalPasteboard;
  const candidates = [
    ['public.png', 'image/png', 'clipboard.png'],
    ['public.jpeg', 'image/jpeg', 'clipboard.jpg'],
    ['public.tiff', 'image/tiff', 'clipboard.tiff']
  ];
  for (const candidate of candidates) {
    const data = pasteboard.dataForType(candidate[0]);
    if (data) {
      const encoded = ObjC.unwrap(data.base64EncodedStringWithOptions(0));
      return JSON.stringify({name: candidate[2], type: candidate[1], data: encoded});
    }
  }
  return '';
}
run();`

type clipboardJXAResult struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Data string `json:"data"`
}

func readClipboardAttachment(ctx context.Context) (string, string, []byte, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", clipboardJXA)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", "", nil, fmt.Errorf("could not read the macOS clipboard: %w", err)
	}
	maxEncoded := int64(base64.StdEncoding.EncodedLen(attachmentMaxFileBytes) + 4096)
	raw, readErr := io.ReadAll(io.LimitReader(stdout, maxEncoded+1))
	tooLarge := int64(len(raw)) > maxEncoded
	if readErr != nil || tooLarge {
		// A producer whose output exceeds the limit may still be blocked writing
		// to the pipe. Stop it before Wait so a very large clipboard cannot hang
		// the terminal indefinitely.
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return "", "", nil, readErr
	}
	if tooLarge {
		return "", "", nil, fmt.Errorf("clipboard image exceeds the %s per-file limit", formatAttachmentBytes(attachmentMaxFileBytes))
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = waitErr.Error()
		}
		return "", "", nil, fmt.Errorf("could not read the macOS clipboard: %s", message)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "", "", nil, errors.New("the macOS clipboard does not contain an image")
	}
	var result clipboardJXAResult
	if err := json.Unmarshal(bytes.TrimSpace(raw), &result); err != nil {
		return "", "", nil, errors.New("macOS returned an invalid clipboard image")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(result.Data))
	if err != nil {
		return "", "", nil, errors.New("macOS returned invalid clipboard image data")
	}
	if len(data) > attachmentMaxFileBytes {
		return "", "", nil, fmt.Errorf("clipboard image exceeds the %s per-file limit", formatAttachmentBytes(attachmentMaxFileBytes))
	}
	if result.Type == "image/tiff" {
		converted, err := convertClipboardTIFFToPNG(ctx, data)
		if err != nil {
			return "", "", nil, err
		}
		return "clipboard.png", "image/png", converted, nil
	}
	return safeAttachmentName(result.Name), result.Type, data, nil
}

func convertClipboardTIFFToPNG(ctx context.Context, data []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "bacli-clipboard-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "clipboard.tiff")
	output := filepath.Join(dir, "clipboard.png")
	if err := os.WriteFile(input, data, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/sips", "-s", "format", "png", input, "--out", output)
	if commandOutput, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("could not convert the clipboard TIFF to PNG: %s", strings.TrimSpace(string(commandOutput)))
	}
	converted, err := os.ReadFile(output)
	if err != nil {
		return nil, err
	}
	if len(converted) == 0 || len(converted) > attachmentMaxFileBytes {
		return nil, errors.New("converted clipboard image is empty or too large")
	}
	return converted, nil
}
