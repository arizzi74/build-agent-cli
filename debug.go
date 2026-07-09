package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const redactedDebugValue = "[REDACTED]"

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

var debugTrace = struct {
	sync.Mutex
	writer io.Writer
}{writer: io.Discard}

var terminalOutput = struct {
	sync.RWMutex
	stdout *os.File
	stderr *os.File
}{stdout: os.Stdout, stderr: os.Stderr}

var debugOutputCleanup = struct {
	sync.Mutex
	fn func() error
}{}

func terminalStdoutFD() int {
	terminalOutput.RLock()
	defer terminalOutput.RUnlock()
	if terminalOutput.stdout != nil {
		return int(terminalOutput.stdout.Fd())
	}
	return int(os.Stdout.Fd())
}

func terminalStderrFD() int {
	terminalOutput.RLock()
	defer terminalOutput.RUnlock()
	if terminalOutput.stderr != nil {
		return int(terminalOutput.stderr.Fd())
	}
	return int(os.Stderr.Fd())
}

func setTerminalOutputFiles(stdout, stderr *os.File) {
	terminalOutput.Lock()
	defer terminalOutput.Unlock()
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	terminalOutput.stdout = stdout
	terminalOutput.stderr = stderr
}

func setDebugTraceWriter(writer io.Writer) {
	debugTrace.Lock()
	defer debugTrace.Unlock()
	if writer == nil {
		writer = io.Discard
	}
	debugTrace.writer = writer
}

func openDebugTraceFile(path string) (*os.File, error) {
	cleanup, file, err := openDebugOutputLog(path)
	if err != nil {
		return nil, err
	}
	setDebugOutputCleanup(cleanup)
	return file, nil
}

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

func setDebugOutputCleanup(fn func() error) {
	debugOutputCleanup.Lock()
	defer debugOutputCleanup.Unlock()
	debugOutputCleanup.fn = fn
}

func closeDebugOutputLog() error {
	debugOutputCleanup.Lock()
	fn := debugOutputCleanup.fn
	debugOutputCleanup.fn = nil
	debugOutputCleanup.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}

func debugTracef(format string, args ...interface{}) {
	debugTrace.Lock()
	writer := debugTrace.writer
	debugTrace.Unlock()
	_, _ = fmt.Fprintf(writer, format, args...)
}

func (c *Client) debugf(format string, args ...interface{}) {
	if c == nil || !c.debug {
		return
	}
	debugTracef(format, args...)
}

func redactDebugJSON(v interface{}) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("<unmarshalable debug payload>")
	}
	return redactDebugJSONBytes(raw)
}

func redactDebugJSONBytes(raw []byte) []byte {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	redactDebugValue(v, "")
	redacted, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return redacted
}

func redactDebugValue(v interface{}, key string) {
	switch typed := v.(type) {
	case map[string]interface{}:
		for k, child := range typed {
			if isSensitiveDebugKey(k) {
				typed[k] = redactedDebugValue
				continue
			}
			redactDebugValue(child, k)
		}
	case []interface{}:
		for _, child := range typed {
			redactDebugValue(child, key)
		}
	}
}

func isSensitiveDebugKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.ReplaceAll(k, "-", "_")
	k = strings.ReplaceAll(k, " ", "_")
	if isNonSecretTokenMetric(k) {
		return false
	}
	if strings.Contains(k, "token") {
		return true
	}
	switch k {
	case "authorization", "cookie", "cookies", "cookie_header", "cookieheader", "password", "secret", "g_ck", "glide_ck":
		return true
	default:
		return false
	}
}

func isNonSecretTokenMetric(normalizedKey string) bool {
	switch normalizedKey {
	case "maxoutputtokens", "max_output_tokens", "outputtokens", "output_tokens", "requesttokens", "request_tokens", "thinkingtokens", "thinking_tokens":
		return true
	default:
		return false
	}
}
