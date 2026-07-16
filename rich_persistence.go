package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const richWebMessageEndpointPrefix = "/api/sn_build_agent/build_agent_api/conversations/"

// RichAssistantTextContent is the Web UI conversation content shape observed for
// completed assistant text rows and assistant-thinking rows. Field order is
// intentionally kept in the HAR-observed order so marshaled content matches the
// browser payload byte-for-byte for deterministic inputs.
type RichAssistantTextContent struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Complete  bool   `json:"complete"`
	Duration  int64  `json:"duration"`
	Sender    string `json:"sender"`
	StartTime string `json:"startTime"`
}

type RichUserContent struct {
	ID             string                  `json:"id"`
	Sender         string                  `json:"sender"`
	Text           string                  `json:"text"`
	HasCheckpoints bool                    `json:"hasCheckpoints"`
	Attachments    []RichAttachment        `json:"attachments,omitempty"`
	Checkpoints    []RichMessageCheckpoint `json:"checkpoints,omitempty"`
}

type RichMessageCheckpoint struct {
	ID     string `json:"id"`
	AppDir string `json:"appDir"`
}

type RichStopContent struct {
	ID     string `json:"id"`
	Sender string `json:"sender"`
	Text   string `json:"text"`
}

func NewRichStopContent() RichStopContent {
	return RichStopContent{ID: uuidV4(), Sender: "stop", Text: "Processing was stopped. You can send a new message to continue."}
}

func NewRichUserContent(id, text string) RichUserContent {
	return RichUserContent{ID: defaultRichMessageID(id), Sender: "user", Text: text, HasCheckpoints: false}
}

// RichAssistantToolContent is the Web UI conversation content shape observed for
// completed assistant-tool rows. Field order is intentionally kept in the
// HAR-observed order.
type RichAssistantToolContent struct {
	ID                string      `json:"id"`
	Sender            string      `json:"sender"`
	ToolName          string      `json:"toolName"`
	ToolActualName    string      `json:"toolActualName"`
	Complete          bool        `json:"complete"`
	Duration          int64       `json:"duration"`
	ToolUseID         string      `json:"toolUseId"`
	StartTime         string      `json:"startTime"`
	RequiresApproval  bool        `json:"requiresApproval"`
	RequiresSelection bool        `json:"requiresSelection"`
	ToolInput         interface{} `json:"toolInput"`
	Success           bool        `json:"success"`
	Result            interface{} `json:"result"`
}

// RichAssistantToolContentOptions contains the values needed to construct the
// exact completed assistant-tool row shape used by the Web UI.
type RichAssistantToolContentOptions struct {
	ID                string
	ToolUseID         string
	ToolName          string
	ToolActualName    string
	ToolInput         interface{}
	Success           bool
	Result            interface{}
	RequiresApproval  bool
	RequiresSelection bool
	Duration          time.Duration
	DurationMS        int64
	StartTime         time.Time
	StartTimeString   string
}

func NewRichAssistantThinkingContent(id, text string, duration time.Duration, startTime time.Time) RichAssistantTextContent {
	return newRichAssistantTextContent(id, "assistant-thinking", text, duration, 0, startTime, "")
}

func NewRichAssistantThinkingContentMS(id, text string, durationMS int64, startTime string) RichAssistantTextContent {
	return newRichAssistantTextContent(id, "assistant-thinking", text, 0, durationMS, time.Time{}, startTime)
}

func NewRichAssistantFinalContent(id, text string, duration time.Duration, startTime time.Time) RichAssistantTextContent {
	return newRichAssistantTextContent(id, "assistant", text, duration, 0, startTime, "")
}

func NewRichAssistantFinalContentMS(id, text string, durationMS int64, startTime string) RichAssistantTextContent {
	return newRichAssistantTextContent(id, "assistant", text, 0, durationMS, time.Time{}, startTime)
}

func NewRichAssistantToolContent(opts RichAssistantToolContentOptions) RichAssistantToolContent {
	durationMS := opts.DurationMS
	if durationMS == 0 && opts.Duration != 0 {
		durationMS = opts.Duration.Milliseconds()
	}
	return RichAssistantToolContent{
		ID:                defaultRichMessageID(opts.ID),
		Sender:            "assistant-tool",
		ToolName:          opts.ToolName,
		ToolActualName:    opts.ToolActualName,
		Complete:          true,
		Duration:          durationMS,
		ToolUseID:         opts.ToolUseID,
		StartTime:         richStartTimeString(opts.StartTime, opts.StartTimeString),
		RequiresApproval:  opts.RequiresApproval,
		RequiresSelection: opts.RequiresSelection,
		ToolInput:         opts.ToolInput,
		Success:           opts.Success,
		Result:            opts.Result,
	}
}

func newRichAssistantTextContent(id, sender, text string, duration time.Duration, durationMS int64, startTime time.Time, startTimeString string) RichAssistantTextContent {
	if durationMS == 0 && duration != 0 {
		durationMS = duration.Milliseconds()
	}
	return RichAssistantTextContent{
		ID:        defaultRichMessageID(id),
		Text:      text,
		Complete:  true,
		Duration:  durationMS,
		Sender:    sender,
		StartTime: richStartTimeString(startTime, startTimeString),
	}
}

func defaultRichMessageID(id string) string {
	id = strings.TrimSpace(id)
	if id != "" {
		return id
	}
	return uuidV4()
}

func richStartTimeString(t time.Time, explicit string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit
	}
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func (c *Client) PersistRichWebMessageContent(ctx context.Context, conversationID string, content interface{}) (string, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		conversationID = strings.TrimSpace(c.conversationID)
	}
	if conversationID == "" {
		return "", fmt.Errorf("Build Agent conversation id is required")
	}
	contentJSON, err := richMessageContentJSONString(content)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(contentJSON) == "" {
		return "", nil
	}
	if err := c.ensureConversationHTTPClient(); err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + richWebMessageEndpointPrefix + url.PathEscape(conversationID) + "/messages"
	body, _, err := c.postJSON(ctx, endpoint, map[string]interface{}{"content": contentJSON})
	if err != nil {
		return "", err
	}
	return parseRichWebMessageSysID(body), nil
}

func (c *Client) PersistRichAssistantThinkingMessage(ctx context.Context, conversationID, id, text string, duration time.Duration, startTime time.Time) (string, error) {
	return c.PersistRichWebMessageContent(ctx, conversationID, NewRichAssistantThinkingContent(id, text, duration, startTime))
}

func (c *Client) PersistRichAssistantToolMessage(ctx context.Context, conversationID string, opts RichAssistantToolContentOptions) (string, error) {
	return c.PersistRichWebMessageContent(ctx, conversationID, NewRichAssistantToolContent(opts))
}

func (c *Client) PersistRichAssistantFinalMessage(ctx context.Context, conversationID, id, text string, duration time.Duration, startTime time.Time) (string, error) {
	return c.PersistRichWebMessageContent(ctx, conversationID, NewRichAssistantFinalContent(id, text, duration, startTime))
}

func (c *Client) BestEffortPersistRichWebMessageContent(ctx context.Context, conversationID string, content interface{}) string {
	sysID, err := c.PersistRichWebMessageContent(ctx, conversationID, content)
	if err != nil && c != nil && c.debug {
		c.debugf("warning: could not persist rich Web UI message: %v\n", err)
	}
	return sysID
}

func (c *Client) BestEffortPersistRichAssistantThinkingMessage(ctx context.Context, conversationID, id, text string, duration time.Duration, startTime time.Time) string {
	return c.BestEffortPersistRichWebMessageContent(ctx, conversationID, NewRichAssistantThinkingContent(id, text, duration, startTime))
}

func (c *Client) BestEffortPersistRichAssistantToolMessage(ctx context.Context, conversationID string, opts RichAssistantToolContentOptions) string {
	return c.BestEffortPersistRichWebMessageContent(ctx, conversationID, NewRichAssistantToolContent(opts))
}

func (c *Client) BestEffortPersistRichAssistantFinalMessage(ctx context.Context, conversationID, id, text string, duration time.Duration, startTime time.Time) string {
	return c.BestEffortPersistRichWebMessageContent(ctx, conversationID, NewRichAssistantFinalContent(id, text, duration, startTime))
}

func richMessageContentJSONString(content interface{}) (string, error) {
	switch typed := content.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	case []byte:
		return string(typed), nil
	case json.RawMessage:
		return string(typed), nil
	default:
		raw, err := json.Marshal(content)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
}

func parseRichWebMessageSysID(body []byte) string {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return ""
	}
	var decoded interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ""
	}
	return firstRichWebMessageSysID(decoded)
}

func firstRichWebMessageSysID(v interface{}) string {
	switch typed := v.(type) {
	case map[string]interface{}:
		for _, key := range []string{"sysId", "sys_id", "sysID", "sysid"} {
			if id := strings.TrimSpace(stringify(typed[key])); id != "" {
				return id
			}
		}
		for _, key := range []string{"result", "record", "message", "data"} {
			if id := firstRichWebMessageSysID(typed[key]); id != "" {
				return id
			}
		}
		for _, value := range typed {
			if id := firstRichWebMessageSysID(value); id != "" {
				return id
			}
		}
	case []interface{}:
		for _, item := range typed {
			if id := firstRichWebMessageSysID(item); id != "" {
				return id
			}
		}
	case string:
		text := strings.TrimSpace(typed)
		if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
			var nested interface{}
			if err := json.Unmarshal([]byte(text), &nested); err == nil {
				return firstRichWebMessageSysID(nested)
			}
		}
	}
	return ""
}
