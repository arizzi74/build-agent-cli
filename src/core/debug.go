package core

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
)

const redactedDebugValue = "[REDACTED]"
const redactedDebugAttachmentValue = "[REDACTED ATTACHMENT]"

// Also recognize text attachment bodies embedded in JSON strings or partial
// debug frames, where normal map-key redaction cannot inspect the contents.
var inlineAttachmentTextKeyPattern = regexp.MustCompile(`(?i)\\?"textcontent\\?"\s*:`)

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
		if containsDebugAttachmentData(string(raw)) {
			return []byte(redactedDebugAttachmentValue)
		}
		return raw
	}
	if text, ok := v.(string); ok && containsDebugAttachmentData(text) {
		v = redactedDebugAttachmentValue
	}
	redactDebugValue(v, "")
	redacted, err := json.Marshal(v)
	if err != nil {
		if containsDebugAttachmentData(string(raw)) {
			return []byte(redactedDebugAttachmentValue)
		}
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
			if isAttachmentDebugKey(k) {
				typed[k] = redactedDebugAttachmentValue
				continue
			}
			if text, ok := child.(string); ok && containsDebugAttachmentData(text) {
				typed[k] = redactedDebugAttachmentValue
				continue
			}
			redactDebugValue(child, k)
		}
	case []interface{}:
		for i, child := range typed {
			if text, ok := child.(string); ok && containsDebugAttachmentData(text) {
				typed[i] = redactedDebugAttachmentValue
				continue
			}
			redactDebugValue(child, key)
		}
	}
}

func isAttachmentDebugKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "attachment", "attachments", "images", "image_data", "file_data", "clipboard_data", "textcontent":
		return true
	default:
		return false
	}
}

func isInlineAttachmentData(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(strings.ToLower(value), "data:") && strings.Contains(value, ";base64,")
}

func containsInlineAttachmentData(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return isInlineAttachmentData(lower) || (strings.Contains(lower, "data:") && strings.Contains(lower, ";base64,"))
}

func containsDebugAttachmentData(value string) bool {
	return containsInlineAttachmentData(value) || inlineAttachmentTextKeyPattern.MatchString(value)
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
