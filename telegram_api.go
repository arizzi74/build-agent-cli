package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const telegramMaxResponseBytes = 2 << 20

type telegramAPI struct {
	baseURL string
	token   string
	client  *http.Client
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
		baseURL: "https://api.telegram.org/bot" + token + "/",
		token:   token,
		client: &http.Client{
			Timeout: 65 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (api *telegramAPI) call(ctx context.Context, method string, payload interface{}, result interface{}) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		err := api.callOnce(ctx, method, payload, result)
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
	commands := []map[string]string{
		{"command": "ask", "description": "Send a prompt to Build Agent"},
		{"command": "cancel", "description": "Cancel the active Build Agent turn"},
		{"command": "status", "description": "Show Build Agent status"},
		{"command": "conversation", "description": "Manage the active conversation"},
		{"command": "workspace", "description": "Inspect or select a workspace"},
		{"command": "app", "description": "Inspect or select an app"},
		{"command": "sync", "description": "Show safe sync status"},
		{"command": "project", "description": "Inspect local projects"},
		{"command": "help", "description": "Show Telegram commands"},
		{"command": "whoami", "description": "Show your numeric Telegram user ID"},
	}
	return api.call(ctx, "setMyCommands", map[string]interface{}{"commands": commands}, nil)
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
	for _, part := range splitTelegramText(text, 4000) {
		if err := api.call(ctx, "sendMessage", map[string]interface{}{
			"chat_id": chatID, "text": part, "disable_web_page_preview": true,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

func splitTelegramText(text string, limit int) []string {
	if limit <= 0 {
		return []string{"(no output)"}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{"(no output)"}
	}
	runes := []rune(text)
	parts := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i
				break
			}
		}
		parts = append(parts, strings.TrimSpace(string(runes[:cut])))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		parts = append(parts, strings.TrimSpace(string(runes)))
	}
	return parts
}

func telegramID(value int64) string { return strconv.FormatInt(value, 10) }
