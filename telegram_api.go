package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	telegramMaxResponseBytes      = 2 << 20
	telegramTextChunkUTF16Units   = 4000
	telegramMaxDirectTextMessages = 10
	telegramDocumentPartBytes     = 8 << 20
	telegramDirectMessageInterval = time.Second
)

type telegramAPI struct {
	baseURL          string
	token            string
	client           *http.Client
	messageInterval  time.Duration
	documentPartSize int
}

type telegramAPIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *telegramAPIError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("Telegram %s failed (%d): %s", e.Method, e.Code, e.Description)
	}
	return fmt.Sprintf("Telegram %s failed (%d)", e.Method, e.Code)
}

type telegramEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramMessage struct {
	MessageID int64         `json:"message_id"`
	From      *telegramUser `json:"from"`
	Chat      telegramChat  `json:"chat"`
	Date      int64         `json:"date"`
	Text      string        `json:"text"`
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

func newTelegramAPI(token string) *telegramAPI {
	return &telegramAPI{
		baseURL:         "https://api.telegram.org/bot" + token + "/",
		token:           token,
		messageInterval: telegramDirectMessageInterval,
		client: &http.Client{
			Timeout: 65 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (api *telegramAPI) call(ctx context.Context, method string, payload interface{}, result interface{}) error {
	return api.callWithRetry(ctx, func() error {
		return api.callOnce(ctx, method, payload, result)
	})
}

func (api *telegramAPI) callWithRetry(ctx context.Context, callAttempt func() error) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		err := callAttempt()
		if err == nil {
			return nil
		}
		last = err
		var apiErr *telegramAPIError
		if !errors.As(err, &apiErr) || apiErr.Code == http.StatusUnauthorized || apiErr.Code == http.StatusForbidden || apiErr.Code == http.StatusConflict {
			return err
		}
		delay := apiErr.RetryAfter
		if delay <= 0 {
			if apiErr.Code != 0 && apiErr.Code != http.StatusRequestTimeout && apiErr.Code < 500 {
				return err
			}
			delay = time.Duration(attempt+1) * time.Second
		}
		if delay > 60*time.Second {
			delay = 60 * time.Second
		}
		if err := waitTelegramRetry(ctx, delay); err != nil {
			return err
		}
	}
	return last
}

func (api *telegramAPI) callOnce(ctx context.Context, method string, payload interface{}, result interface{}) error {
	if !validTelegramAPIMethod(method) {
		return errors.New("invalid Telegram API method")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("Telegram %s request could not be encoded", method)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.baseURL+method, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("Telegram %s request could not be created", method)
	}
	req.Header.Set("Content-Type", "application/json")
	return api.doRequest(ctx, method, req, result)
}

func (api *telegramAPI) callDocument(ctx context.Context, chatID, filename, caption string, data []byte) error {
	return api.callWithRetry(ctx, func() error {
		return api.callDocumentOnce(ctx, chatID, filename, caption, data)
	})
}

func (api *telegramAPI) callDocumentOnce(ctx context.Context, chatID, filename, caption string, data []byte) error {
	const method = "sendDocument"
	if !validTelegramAPIMethod(method) {
		return errors.New("invalid Telegram API method")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", chatID); err != nil {
		return errors.New("Telegram sendDocument request could not be encoded")
	}
	if caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return errors.New("Telegram sendDocument request could not be encoded")
		}
	}
	if err := writer.WriteField("disable_content_type_detection", "true"); err != nil {
		return errors.New("Telegram sendDocument request could not be encoded")
	}
	part, err := writer.CreateFormFile("document", filename)
	if err != nil {
		return errors.New("Telegram sendDocument request could not be encoded")
	}
	if _, err := part.Write(data); err != nil {
		return errors.New("Telegram sendDocument request could not be encoded")
	}
	if err := writer.Close(); err != nil {
		return errors.New("Telegram sendDocument request could not be encoded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.baseURL+method, bytes.NewReader(body.Bytes()))
	if err != nil {
		return errors.New("Telegram sendDocument request could not be created")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return api.doRequest(ctx, method, req, nil)
}

func (api *telegramAPI) doRequest(ctx context.Context, method string, req *http.Request, result interface{}) error {
	response, err := api.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// net/http errors often contain req.URL, which embeds the bot token.
		return &telegramAPIError{Method: method, Code: 0, Description: "network request failed"}
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, telegramMaxResponseBytes+1))
	if readErr != nil {
		return &telegramAPIError{Method: method, Code: response.StatusCode, Description: "response could not be read"}
	}
	if len(body) > telegramMaxResponseBytes {
		return &telegramAPIError{Method: method, Code: response.StatusCode, Description: "response exceeded the size limit"}
	}
	var envelope telegramEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return &telegramAPIError{Method: method, Code: response.StatusCode, Description: "invalid response"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.OK {
		code := envelope.ErrorCode
		if code == 0 {
			code = response.StatusCode
		}
		description := envelope.Description
		if api.token != "" {
			description = strings.ReplaceAll(description, api.token, "[REDACTED]")
		}
		description = singleLineLabel(description)
		descriptionRunes := []rune(description)
		if len(descriptionRunes) > 300 {
			description = string(descriptionRunes[:300])
		}
		return &telegramAPIError{
			Method: method, Code: code, Description: description,
			RetryAfter: time.Duration(envelope.Parameters.RetryAfter) * time.Second,
		}
	}
	if result == nil || len(envelope.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return &telegramAPIError{Method: method, Code: response.StatusCode, Description: "invalid result"}
	}
	return nil
}

func validTelegramAPIMethod(method string) bool {
	if method == "" || len(method) > 64 {
		return false
	}
	for _, r := range method {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return false
		}
	}
	return true
}

func waitTelegramRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (api *telegramAPI) getMe(ctx context.Context) (telegramUser, error) {
	var user telegramUser
	err := api.call(ctx, "getMe", struct{}{}, &user)
	return user, err
}

func (api *telegramAPI) deleteWebhook(ctx context.Context) error {
	return api.call(ctx, "deleteWebhook", map[string]interface{}{"drop_pending_updates": false}, nil)
}

func (api *telegramAPI) setMyCommands(ctx context.Context) error {
	published := telegramPublishedCommands()
	if err := validateTelegramPublishedCommands(published); err != nil {
		return err
	}
	commands := make([]map[string]string, 0, len(published))
	for _, command := range published {
		commands = append(commands, map[string]string{"command": command.Name, "description": command.Description})
	}
	if err := api.call(ctx, "setMyCommands", map[string]interface{}{
		"commands": commands,
		"scope":    map[string]string{"type": "all_private_chats"},
	}, nil); err != nil {
		return err
	}
	// Older bacli builds registered a default-scope subset. Remove that stale
	// menu so commands cannot continue to appear in groups after the new private
	// scope is installed.
	return api.call(ctx, "deleteMyCommands", map[string]interface{}{
		"scope": map[string]string{"type": "default"},
	}, nil)
}

func (api *telegramAPI) getUpdates(ctx context.Context, offset int64, timeout int) ([]telegramUpdate, error) {
	var updates []telegramUpdate
	payload := map[string]interface{}{
		"offset": offset, "timeout": timeout, "allowed_updates": []string{"message"},
	}
	err := api.callOnce(ctx, "getUpdates", payload, &updates)
	return updates, err
}

func (api *telegramAPI) sendText(ctx context.Context, chatID, text string) error {
	parts, overflow := splitTelegramTextLimited(text, telegramTextChunkUTF16Units, telegramMaxDirectTextMessages)
	if overflow {
		return api.sendTextDocuments(ctx, chatID, text)
	}
	for i, part := range parts {
		if err := api.call(ctx, "sendMessage", map[string]interface{}{
			"chat_id": chatID, "text": part, "disable_web_page_preview": true,
		}, nil); err != nil {
			return fmt.Errorf("Telegram text chunk %d of %d could not be sent: %w", i+1, len(parts), err)
		}
		if i+1 < len(parts) && api.messageInterval > 0 {
			if err := waitTelegramRetry(ctx, api.messageInterval); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitTelegramText(text string, limit int) []string {
	parts, _ := splitTelegramTextLimited(text, limit, 0)
	return parts
}

func splitTelegramTextLimited(text string, limit, maxParts int) ([]string, bool) {
	if limit <= 0 {
		return []string{"(no output)"}, false
	}
	if strings.TrimSpace(text) == "" {
		return []string{"(no output)"}, false
	}
	runes := []rune(text)
	parts := make([]string, 0)
	for len(runes) > 0 {
		if maxParts > 0 && len(parts) >= maxParts {
			return parts, true
		}
		cut := telegramUTF16Cut(runes, limit)
		if cut >= len(runes) {
			parts = append(parts, string(runes))
			break
		}
		newline := -1
		for i := cut; i > 0; i-- {
			if runes[i-1] == '\n' {
				newline = i
				break
			}
		}
		if newline > 0 && telegramUTF16Units(runes[:newline]) >= limit/2 {
			cut = newline
		}
		parts = append(parts, string(runes[:cut]))
		runes = runes[cut:]
	}
	return parts, false
}

func (api *telegramAPI) sendTextDocuments(ctx context.Context, chatID, text string) error {
	text = strings.ToValidUTF8(text, "\ufffd")
	partSize := api.documentPartSize
	if partSize < utf8.UTFMax {
		partSize = telegramDocumentPartBytes
	}
	parts := splitTelegramDocumentText(text, partSize)
	for i, part := range parts {
		filename := telegramDocumentFilename(i+1, len(parts))
		caption := telegramDocumentCaption(i+1, len(parts))
		if err := api.callDocument(ctx, chatID, filename, caption, []byte(part)); err != nil {
			return fmt.Errorf("Telegram document part %d of %d could not be sent: %w", i+1, len(parts), err)
		}
		if i+1 < len(parts) && api.messageInterval > 0 {
			if err := waitTelegramRetry(ctx, api.messageInterval); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitTelegramDocumentText(text string, limit int) []string {
	if text == "" {
		return []string{""}
	}
	if limit < utf8.UTFMax {
		limit = utf8.UTFMax
	}
	parts := make([]string, 0, (len(text)+limit-1)/limit)
	for start := 0; start < len(text); {
		end := start + limit
		if end >= len(text) {
			parts = append(parts, text[start:])
			break
		}
		for end > start && !utf8.RuneStart(text[end]) {
			end--
		}
		if end == start {
			_, size := utf8.DecodeRuneInString(text[start:])
			end = start + size
		}
		if newline := strings.LastIndexByte(text[start:end], '\n'); newline >= (end-start)/2 {
			end = start + newline + 1
		}
		parts = append(parts, text[start:end])
		start = end
	}
	return parts
}

func telegramDocumentFilename(part, total int) string {
	if total <= 1 {
		return "bacli-response.txt"
	}
	width := len(strconv.Itoa(total))
	if width < 3 {
		width = 3
	}
	return fmt.Sprintf("bacli-response-%0*d-of-%0*d.txt", width, part, width, total)
}

func telegramDocumentCaption(part, total int) string {
	if total <= 1 {
		return "BACLI response attached as UTF-8 text."
	}
	return fmt.Sprintf("BACLI response part %d of %d (UTF-8 text).", part, total)
}

func telegramUTF16Units(runes []rune) int {
	units := 0
	for _, r := range runes {
		units++
		if r > 0xffff {
			units++
		}
	}
	return units
}

func telegramUTF16Cut(runes []rune, limit int) int {
	units := 0
	for i, r := range runes {
		next := 1
		if r > 0xffff {
			next = 2
		}
		if units+next > limit {
			if i == 0 {
				return 1
			}
			return i
		}
		units += next
	}
	return len(runes)
}

func telegramID(value int64) string { return strconv.FormatInt(value, 10) }
