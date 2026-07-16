package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	attachmentMaxCount      = 10
	attachmentMaxFileBytes  = 25 << 20
	attachmentMaxTotalBytes = 50 << 20
	attachmentMaxReplyBytes = 1 << 20
)

type pendingAttachment struct {
	ID                     string
	Name                   string
	Size                   int64
	Type                   string
	Category               string
	Data                   []byte
	SysAttachmentID        string
	UploadedConversationID string
	ServerDownloadLink     string
}

type attachmentSummary struct {
	ID       string
	Name     string
	Size     int64
	Type     string
	Category string
	Uploaded bool
}

// RichAttachment is the attachment metadata shape persisted by the Build Agent
// Web UI. The actual bytes are deliberately absent: ServiceNow stores them in
// sys_attachment, while the Nirvana turn carries a separate inline data URL.
type RichAttachment struct {
	URL             string `json:"url"`
	Name            string `json:"name"`
	Size            int64  `json:"size"`
	Type            string `json:"type"`
	Category        string `json:"category"`
	SysAttachmentID string `json:"sys_attachment_id"`
}

type nirvanaImageAttachment struct {
	URL  string `json:"url"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type nirvanaAttachment struct {
	URL      string `json:"url"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Category string `json:"category"`
}

type attachmentUploadResponse struct {
	Result struct {
		SysID        string `json:"sys_id"`
		FileName     string `json:"file_name"`
		SizeBytes    string `json:"size_bytes"`
		ContentType  string `json:"content_type"`
		State        string `json:"state"`
		TableName    string `json:"table_name"`
		TableSysID   string `json:"table_sys_id"`
		DownloadLink string `json:"download_link"`
		Hash         string `json:"hash"`
	} `json:"result"`
}

func (c *Client) addPendingAttachmentFile(path string) (attachmentSummary, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return attachmentSummary{}, errors.New("attachment path is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return attachmentSummary{}, fmt.Errorf("cannot inspect attachment: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return attachmentSummary{}, errors.New("attachment must be a regular file, not a symlink or directory")
	}
	if info.Size() <= 0 {
		return attachmentSummary{}, errors.New("attachment is empty")
	}
	if info.Size() > attachmentMaxFileBytes {
		return attachmentSummary{}, fmt.Errorf("attachment exceeds the %s per-file limit", formatAttachmentBytes(attachmentMaxFileBytes))
	}
	f, err := os.Open(path)
	if err != nil {
		return attachmentSummary{}, fmt.Errorf("cannot open attachment: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, attachmentMaxFileBytes+1))
	if err != nil {
		return attachmentSummary{}, fmt.Errorf("cannot read attachment: %w", err)
	}
	if len(data) > attachmentMaxFileBytes {
		return attachmentSummary{}, fmt.Errorf("attachment exceeds the %s per-file limit", formatAttachmentBytes(attachmentMaxFileBytes))
	}
	return c.addPendingAttachmentBytes(filepath.Base(path), mime.TypeByExtension(strings.ToLower(filepath.Ext(path))), data)
}

func (c *Client) addPendingAttachmentBytes(name, mediaType string, data []byte) (attachmentSummary, error) {
	if c == nil {
		return attachmentSummary{}, errors.New("attachment client is unavailable")
	}
	if len(data) == 0 {
		return attachmentSummary{}, errors.New("attachment is empty")
	}
	if len(data) > attachmentMaxFileBytes {
		return attachmentSummary{}, fmt.Errorf("attachment exceeds the %s per-file limit", formatAttachmentBytes(attachmentMaxFileBytes))
	}
	name = safeAttachmentName(name)
	mediaType = normalizedAttachmentMediaType(mediaType, name, data)
	category := "file"
	if strings.HasPrefix(mediaType, "image/") {
		category = "image"
	}
	item := pendingAttachment{
		ID:       "att-" + uuidV4Compact(),
		Name:     name,
		Size:     int64(len(data)),
		Type:     mediaType,
		Category: category,
		Data:     append([]byte(nil), data...),
	}
	c.attachmentsMu.Lock()
	defer c.attachmentsMu.Unlock()
	if len(c.pendingAttachments) >= attachmentMaxCount {
		return attachmentSummary{}, fmt.Errorf("attachment count exceeds the limit of %d", attachmentMaxCount)
	}
	total := int64(len(data))
	for _, existing := range c.pendingAttachments {
		total += existing.Size
	}
	if total > attachmentMaxTotalBytes {
		return attachmentSummary{}, fmt.Errorf("attachments exceed the %s total limit", formatAttachmentBytes(attachmentMaxTotalBytes))
	}
	c.pendingAttachments = append(c.pendingAttachments, item)
	return summarizePendingAttachment(item), nil
}

func normalizedAttachmentMediaType(provided, name string, data []byte) string {
	provided = strings.TrimSpace(strings.Split(provided, ";")[0])
	if provided != "" {
		if parsed, _, err := mime.ParseMediaType(provided); err == nil {
			provided = strings.ToLower(parsed)
		} else {
			provided = ""
		}
	}
	if provided == "" {
		if byExtension := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); byExtension != "" {
			provided = strings.ToLower(strings.TrimSpace(strings.Split(byExtension, ";")[0]))
		}
	}
	if provided == "" || provided == "application/octet-stream" {
		provided = strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data[:minInt(len(data), 512)]), ";")[0]))
	}
	if provided == "" {
		return "application/octet-stream"
	}
	return provided
}

func safeAttachmentName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u007f' {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" || name == "." || name == ".." {
		return "attachment.bin"
	}
	return name
}

func summarizePendingAttachment(item pendingAttachment) attachmentSummary {
	return attachmentSummary{ID: item.ID, Name: item.Name, Size: item.Size, Type: item.Type, Category: item.Category, Uploaded: item.SysAttachmentID != ""}
}

func (c *Client) pendingAttachmentSummaries() []attachmentSummary {
	if c == nil {
		return nil
	}
	c.attachmentsMu.Lock()
	defer c.attachmentsMu.Unlock()
	out := make([]attachmentSummary, len(c.pendingAttachments))
	for i, item := range c.pendingAttachments {
		out[i] = summarizePendingAttachment(item)
	}
	return out
}

func (c *Client) pendingAttachmentCount() int {
	if c == nil {
		return 0
	}
	c.attachmentsMu.Lock()
	defer c.attachmentsMu.Unlock()
	return len(c.pendingAttachments)
}

func (c *Client) removePendingAttachment(ctx context.Context, selector string) (attachmentSummary, error) {
	_ = ctx // Local detach deliberately does not call an undocumented remote DELETE API.
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return attachmentSummary{}, errors.New("attachment number, id, or filename is required")
	}
	c.attachmentsMu.Lock()
	defer c.attachmentsMu.Unlock()
	index, err := selectPendingAttachment(c.pendingAttachments, selector)
	if err != nil {
		return attachmentSummary{}, err
	}
	item := c.pendingAttachments[index]
	copy(c.pendingAttachments[index:], c.pendingAttachments[index+1:])
	last := len(c.pendingAttachments) - 1
	c.pendingAttachments[last] = pendingAttachment{}
	c.pendingAttachments = c.pendingAttachments[:last]
	// Removing an item explicitly abandons any partially persisted attachment
	// turn locally. The remote row/attachment may remain because the HAR does
	// not establish safe delete semantics.
	c.pendingAttachmentMessageKey = ""
	c.pendingAttachmentMessageSysID = ""
	c.pendingAttachmentMessageContent = ""
	c.pendingAttachmentMessageRichID = ""
	return summarizePendingAttachment(item), nil
}

func selectPendingAttachment(items []pendingAttachment, selector string) (int, error) {
	if n, err := strconv.Atoi(selector); err == nil {
		if n < 1 || n > len(items) {
			return -1, fmt.Errorf("attachment number must be between 1 and %d", len(items))
		}
		return n - 1, nil
	}
	found := -1
	for i, item := range items {
		if item.ID == selector || strings.HasPrefix(item.ID, selector) || strings.EqualFold(item.Name, selector) {
			if found >= 0 {
				return -1, errors.New("attachment selector is ambiguous")
			}
			found = i
		}
	}
	if found < 0 {
		return -1, errors.New("attachment was not found")
	}
	return found, nil
}

func (c *Client) clearPendingAttachments(ctx context.Context) (int, error) {
	items := c.pendingAttachmentSummaries()
	removed := 0
	var failures []string
	for _, item := range items {
		if _, err := c.removePendingAttachment(ctx, item.ID); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", item.Name, err))
			continue
		}
		removed++
	}
	if len(failures) > 0 {
		return removed, errors.New(strings.Join(failures, "; "))
	}
	return removed, nil
}

func (c *Client) preparePendingAttachments(ctx context.Context) ([]pendingAttachment, error) {
	if c == nil {
		return nil, errors.New("attachment client is unavailable")
	}
	c.attachmentsMu.Lock()
	items := clonePendingAttachments(c.pendingAttachments)
	c.attachmentsMu.Unlock()
	if len(items) == 0 {
		return nil, nil
	}
	if !c.opts.Nirvana {
		return nil, errors.New("attachments require the default Nirvana transport")
	}
	conversationID := strings.TrimSpace(c.conversationID)
	if conversationID == "" {
		return nil, errors.New("cannot upload attachments before the conversation exists")
	}
	for i := range items {
		if items[i].SysAttachmentID != "" {
			if uploadedConversationID := strings.TrimSpace(items[i].UploadedConversationID); uploadedConversationID != "" && uploadedConversationID != conversationID {
				return nil, fmt.Errorf("attachment %s was uploaded for a different conversation; clear and add it again", items[i].Name)
			}
			continue
		}
		uploaded, err := c.uploadConversationAttachment(ctx, conversationID, items[i])
		if err != nil {
			return nil, fmt.Errorf("attachment %s upload failed: %w", items[i].Name, err)
		}
		items[i].SysAttachmentID = uploaded.Result.SysID
		items[i].UploadedConversationID = conversationID
		items[i].ServerDownloadLink = uploaded.Result.DownloadLink
		c.attachmentsMu.Lock()
		updated := false
		for j := range c.pendingAttachments {
			if c.pendingAttachments[j].ID == items[i].ID {
				c.pendingAttachments[j].SysAttachmentID = items[i].SysAttachmentID
				c.pendingAttachments[j].UploadedConversationID = conversationID
				c.pendingAttachments[j].ServerDownloadLink = items[i].ServerDownloadLink
				updated = true
				break
			}
		}
		c.attachmentsMu.Unlock()
		if !updated {
			return nil, errors.New("attachment queue changed while uploading; the uploaded remote attachment was not deleted")
		}
	}
	return items, nil
}

func clonePendingAttachments(items []pendingAttachment) []pendingAttachment {
	out := make([]pendingAttachment, len(items))
	for i, item := range items {
		out[i] = item
		out[i].Data = append([]byte(nil), item.Data...)
	}
	return out
}

func (c *Client) pendingAttachmentSnapshot() []pendingAttachment {
	if c == nil {
		return nil
	}
	c.attachmentsMu.Lock()
	defer c.attachmentsMu.Unlock()
	return clonePendingAttachments(c.pendingAttachments)
}

func attachmentPayloads(items []pendingAttachment) ([]nirvanaImageAttachment, []nirvanaAttachment, []RichAttachment) {
	images := make([]nirvanaImageAttachment, 0, len(items))
	attachments := make([]nirvanaAttachment, 0, len(items))
	rich := make([]RichAttachment, 0, len(items))
	for _, item := range items {
		dataURL := "data:" + item.Type + ";base64," + base64.StdEncoding.EncodeToString(item.Data)
		if item.Category == "image" {
			images = append(images, nirvanaImageAttachment{URL: dataURL, Name: item.Name, Type: item.Type})
		}
		attachments = append(attachments, nirvanaAttachment{URL: dataURL, Name: item.Name, Type: item.Type, Category: item.Category})
		rich = append(rich, RichAttachment{URL: "", Name: item.Name, Size: item.Size, Type: item.Type, Category: item.Category, SysAttachmentID: item.SysAttachmentID})
	}
	return images, attachments, rich
}

func cloneRichAttachments(items []RichAttachment) []RichAttachment {
	if len(items) == 0 {
		return nil
	}
	out := make([]RichAttachment, len(items))
	copy(out, items)
	for i := range out {
		// Inline/local URLs are never part of local history or durable state.
		out[i].URL = ""
	}
	return out
}

func richAttachmentsFromMessageContentValue(content interface{}) []RichAttachment {
	var decoded map[string]interface{}
	switch value := content.(type) {
	case string:
		value = strings.TrimSpace(value)
		if value == "" || !strings.HasPrefix(value, "{") || json.Unmarshal([]byte(value), &decoded) != nil {
			return nil
		}
	default:
		decoded = asMap(value)
		if decoded == nil {
			return nil
		}
	}
	return richAttachmentsFromValue(decoded["attachments"])
}

func richAttachmentsFromValue(value interface{}) []RichAttachment {
	var raw []interface{}
	switch typed := value.(type) {
	case nil:
		return nil
	case []RichAttachment:
		out := make([]RichAttachment, 0, len(typed))
		for _, item := range typed {
			if strings.TrimSpace(item.Name) == "" && strings.TrimSpace(item.SysAttachmentID) == "" {
				continue
			}
			out = append(out, normalizedRichAttachment(item))
		}
		return out
	case []interface{}:
		raw = typed
	default:
		return nil
	}
	out := make([]RichAttachment, 0, len(raw))
	for _, item := range raw {
		m := asMap(item)
		if m == nil {
			continue
		}
		name := firstString(m, "name", "file_name", "fileName")
		sysAttachmentID := firstString(m, "sys_attachment_id", "sysAttachmentId", "sys_id", "sysId")
		if strings.TrimSpace(name) == "" && strings.TrimSpace(sysAttachmentID) == "" {
			continue
		}
		attachment := normalizedRichAttachment(RichAttachment{
			Name:            name,
			Size:            int64FromInterface(m["size"]),
			Type:            firstString(m, "type", "content_type", "contentType"),
			Category:        firstString(m, "category"),
			SysAttachmentID: sysAttachmentID,
		})
		out = append(out, attachment)
	}
	return out
}

func normalizedRichAttachment(item RichAttachment) RichAttachment {
	item.URL = ""
	item.Name = safeAttachmentName(item.Name)
	if item.Size < 0 {
		item.Size = 0
	}
	provided := strings.TrimSpace(item.Type)
	if parsed, _, err := mime.ParseMediaType(provided); err == nil {
		item.Type = strings.ToLower(parsed)
	} else if byExtension := mime.TypeByExtension(strings.ToLower(filepath.Ext(item.Name))); byExtension != "" {
		item.Type = strings.ToLower(strings.TrimSpace(strings.Split(byExtension, ";")[0]))
	} else {
		item.Type = "application/octet-stream"
	}
	item.Category = strings.ToLower(singleLineLabel(item.Category))
	if item.Category == "" {
		item.Category = "file"
		if strings.HasPrefix(item.Type, "image/") {
			item.Category = "image"
		}
	}
	item.SysAttachmentID = strings.TrimSpace(item.SysAttachmentID)
	return item
}

func attachmentSummariesFromValue(value interface{}) []attachmentSummary {
	items := richAttachmentsFromValue(value)
	out := make([]attachmentSummary, len(items))
	for i, item := range items {
		out[i] = attachmentSummary{
			Name: item.Name, Size: item.Size, Type: item.Type, Category: item.Category,
			Uploaded: item.SysAttachmentID != "",
		}
	}
	return out
}

func historyUserMessage(content string, attachments []RichAttachment) map[string]interface{} {
	message := map[string]interface{}{"role": "user", "content": content}
	if len(attachments) > 0 {
		message["attachments"] = cloneRichAttachments(attachments)
	}
	return message
}

func sanitizeAttachmentHistory(history []interface{}) []interface{} {
	if history == nil {
		return nil
	}
	out := make([]interface{}, len(history))
	for i, value := range history {
		out[i] = sanitizeAttachmentHistoryValue(value)
	}
	return out
}

func sanitizeAttachmentHistoryValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case RichAttachment:
		return normalizedRichAttachment(typed)
	case []RichAttachment:
		return cloneRichAttachments(typed)
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, child := range typed {
			out[i] = sanitizeAttachmentHistoryValue(child)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			lower := strings.ToLower(strings.TrimSpace(key))
			if lower == "attachments" || lower == "images" {
				if attachments := richAttachmentsFromValue(child); len(attachments) > 0 {
					out[key] = cloneRichAttachments(attachments)
					continue
				}
			}
			if lower == "url" {
				if rawURL, ok := child.(string); ok && containsInlineAttachmentData(rawURL) {
					out[key] = ""
					continue
				}
			}
			out[key] = sanitizeAttachmentHistoryValue(child)
		}
		return out
	case string:
		if !containsInlineAttachmentData(typed) {
			return typed
		}
		trimmed := strings.TrimSpace(typed)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var decoded interface{}
			if json.Unmarshal([]byte(trimmed), &decoded) == nil {
				if encoded, err := json.Marshal(sanitizeAttachmentHistoryValue(decoded)); err == nil {
					return string(encoded)
				}
			}
		}
		return "[attachment data omitted]"
	default:
		return value
	}
}

func preservePendingAttachmentsInHistory(history []interface{}, content string, attachments []RichAttachment) []interface{} {
	history = sanitizeAttachmentHistory(history)
	if len(attachments) == 0 {
		return history
	}
	out := history
	wanted := strings.TrimSpace(content)
	for i := len(out) - 1; i >= 0; i-- {
		message := asMap(out[i])
		if message == nil || strings.ToLower(strings.TrimSpace(firstString(message, "role", "author", "sender", "type"))) != "user" {
			continue
		}
		text := strings.TrimSpace(historyMessageText(message))
		if (wanted == "" && text != "") || (wanted != "" && text != wanted) {
			continue
		}
		cloned := make(map[string]interface{}, len(message)+1)
		for key, value := range message {
			cloned[key] = value
		}
		cloned["attachments"] = cloneRichAttachments(attachments)
		out[i] = cloned
		return out
	}
	return out
}

func (c *Client) consumePendingAttachments(items []pendingAttachment) {
	if c == nil || len(items) == 0 {
		return
	}
	consumed := make(map[string]bool, len(items))
	for _, item := range items {
		consumed[item.ID] = true
	}
	c.attachmentsMu.Lock()
	defer c.attachmentsMu.Unlock()
	all := c.pendingAttachments
	kept := all[:0]
	for _, item := range c.pendingAttachments {
		if consumed[item.ID] {
			continue
		}
		kept = append(kept, item)
	}
	for i := len(kept); i < len(all); i++ {
		// Drop byte-slice references promptly after a successful send instead
		// of retaining image contents in the queue's unused capacity.
		all[i] = pendingAttachment{}
	}
	c.pendingAttachments = kept
	c.pendingAttachmentMessageKey = ""
	c.pendingAttachmentMessageSysID = ""
	c.pendingAttachmentMessageContent = ""
	c.pendingAttachmentMessageRichID = ""
}

func attachmentConversationTitle(content string, items []attachmentSummary) string {
	if title := strings.TrimSpace(content); title != "" {
		return title
	}
	if len(items) == 0 {
		return ""
	}
	if len(items) == 1 {
		return "Attachment: " + items[0].Name
	}
	return fmt.Sprintf("Attachments: %s and %d more", items[0].Name, len(items)-1)
}

func pendingAttachmentTurnKey(conversationID, content string, items []pendingAttachment) string {
	type keyAttachment struct {
		ID                     string `json:"id"`
		Name                   string `json:"name"`
		Size                   int64  `json:"size"`
		Type                   string `json:"type"`
		Category               string `json:"category"`
		SysAttachmentID        string `json:"sysAttachmentId"`
		UploadedConversationID string `json:"uploadedConversationId"`
	}
	payload := struct {
		ConversationID string          `json:"conversationId"`
		Content        string          `json:"content"`
		Attachments    []keyAttachment `json:"attachments"`
	}{
		ConversationID: strings.TrimSpace(conversationID),
		Content:        content,
		Attachments:    make([]keyAttachment, len(items)),
	}
	for i, item := range items {
		payload.Attachments[i] = keyAttachment{
			ID: item.ID, Name: item.Name, Size: item.Size, Type: item.Type, Category: item.Category,
			SysAttachmentID: item.SysAttachmentID, UploadedConversationID: item.UploadedConversationID,
		}
	}
	raw, _ := json.Marshal(payload)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (c *Client) persistedPendingAttachmentMessage(key string) (string, string, error) {
	c.attachmentsMu.Lock()
	defer c.attachmentsMu.Unlock()
	if c.pendingAttachmentMessageSysID == "" {
		return "", "", nil
	}
	if c.pendingAttachmentMessageKey != key {
		return "", "", fmt.Errorf("the previous attachment message was already persisted with different prompt text or attachments; retry the exact original prompt %q or use /attach clear to abandon it locally", c.pendingAttachmentMessageContent)
	}
	return c.pendingAttachmentMessageSysID, c.pendingAttachmentMessageRichID, nil
}

func (c *Client) rememberPersistedPendingAttachmentMessage(key, content, sysID, richID string) {
	c.attachmentsMu.Lock()
	defer c.attachmentsMu.Unlock()
	c.pendingAttachmentMessageKey = key
	c.pendingAttachmentMessageContent = content
	c.pendingAttachmentMessageSysID = strings.TrimSpace(sysID)
	c.pendingAttachmentMessageRichID = strings.TrimSpace(richID)
}

func (c *Client) uploadConversationAttachment(ctx context.Context, conversationID string, item pendingAttachment) (attachmentUploadResponse, error) {
	var decoded attachmentUploadResponse
	if c.httpClient == nil {
		return decoded, errors.New("HTTP client is unavailable")
	}
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/now/attachment/upload"
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	writeDone := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if closeErr := multipartWriter.Close(); err == nil {
				err = closeErr
			}
			_ = writer.CloseWithError(err)
			writeDone <- err
		}()
		if err = multipartWriter.WriteField("table_name", "sn_build_agent_conversation"); err != nil {
			return
		}
		if err = multipartWriter.WriteField("table_sys_id", conversationID); err != nil {
			return
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="uploadFile"; filename="%s"`, escapeAttachmentHeaderValue(item.Name)))
		header.Set("Content-Type", item.Type)
		var part io.Writer
		part, err = multipartWriter.CreatePart(header)
		if err != nil {
			return
		}
		_, err = part.Write(item.Data)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeDone
		return decoded, err
	}
	c.setGliderWebHeaders(req)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeDone
		return decoded, err
	}
	writeErr := <-writeDone
	defer res.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(res.Body, attachmentMaxReplyBytes+1))
	if writeErr != nil {
		return decoded, writeErr
	}
	if readErr != nil {
		return decoded, readErr
	}
	if len(body) > attachmentMaxReplyBytes {
		return decoded, errors.New("attachment upload response exceeded 1 MiB")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return decoded, fmt.Errorf("attachment upload failed (%d): %s", res.StatusCode, trimBody(body))
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return decoded, fmt.Errorf("attachment upload response was invalid: %w", err)
	}
	decoded.Result.SysID = strings.TrimSpace(decoded.Result.SysID)
	if decoded.Result.SysID == "" {
		return decoded, errors.New("attachment upload response did not include a sys_id")
	}
	if normalizeGatewayConversationID(decoded.Result.SysID) == "" {
		return decoded, errors.New("attachment upload response included an invalid sys_id")
	}
	if table := strings.TrimSpace(decoded.Result.TableName); table != "" && table != "sn_build_agent_conversation" {
		return decoded, fmt.Errorf("attachment upload returned unexpected table %q", table)
	}
	if record := strings.TrimSpace(decoded.Result.TableSysID); record != "" && record != conversationID {
		return decoded, errors.New("attachment upload returned a different conversation id")
	}
	if sizeText := strings.TrimSpace(decoded.Result.SizeBytes); sizeText != "" {
		size, parseErr := strconv.ParseInt(sizeText, 10, 64)
		if parseErr != nil || size != item.Size {
			return decoded, errors.New("attachment upload returned a different file size")
		}
	}
	if responseType := strings.TrimSpace(decoded.Result.ContentType); responseType != "" {
		parsed, _, parseErr := mime.ParseMediaType(responseType)
		if parseErr != nil || !strings.EqualFold(parsed, item.Type) {
			return decoded, errors.New("attachment upload returned a different content type")
		}
	}
	return decoded, nil
}

func escapeAttachmentHeaderValue(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "_", "\n", "_").Replace(value)
}

func formatAttachmentBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	units := []string{"KiB", "MiB", "GiB"}
	for _, suffix := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TiB", value/unit)
}

func attachmentDisplayContent(content string, items []attachmentSummary) string {
	content = strings.TrimSpace(content)
	if len(items) == 0 {
		return content
	}
	var out strings.Builder
	if content != "" {
		out.WriteString(content)
		out.WriteString("\n\n")
	}
	for i, item := range items {
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "[Attachment %d: %s · %s · %s]", i+1, item.Name, item.Type, formatAttachmentBytes(item.Size))
	}
	return out.String()
}

func handleAttachmentCommand(ctx context.Context, c *Client, args []string) error {
	if c == nil {
		return errors.New("attachment client is unavailable")
	}
	if len(args) == 0 || strings.EqualFold(args[0], "list") {
		items := c.pendingAttachmentSummaries()
		if len(items) == 0 {
			slashCommandPrintln("attachments: none")
			return nil
		}
		slashCommandPrintln("pending attachments:")
		for i, item := range items {
			state := "local"
			if item.Uploaded {
				state = "uploaded; pending send"
			}
			slashCommandPrintf("%d. %s  %s  %s  %s\n", i+1, item.Name, item.Type, formatAttachmentBytes(item.Size), state)
		}
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "add":
		if len(args) < 2 {
			return errors.New("usage: /attach add <path>")
		}
		item, err := c.addPendingAttachmentFile(attachmentCommandPath(args[1:]))
		if err != nil {
			return err
		}
		slashCommandPrintf("attached: %s (%s, %s)\n", item.Name, item.Type, formatAttachmentBytes(item.Size))
		return nil
	case "paste":
		clipboardCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		name, mediaType, data, err := readClipboardAttachment(clipboardCtx)
		cancel()
		if err != nil {
			return err
		}
		item, err := c.addPendingAttachmentBytes(name, mediaType, data)
		if err != nil {
			return err
		}
		slashCommandPrintf("attached from clipboard: %s (%s, %s)\n", item.Name, item.Type, formatAttachmentBytes(item.Size))
		return nil
	case "remove", "rm":
		if len(args) != 2 {
			return errors.New("usage: /attach remove <number|id|filename>")
		}
		item, err := c.removePendingAttachment(ctx, args[1])
		if err != nil {
			return err
		}
		slashCommandPrintf("removed attachment: %s\n", item.Name)
		if item.Uploaded {
			slashCommandPrintln("warning: it was already uploaded; the remote attachment was not deleted")
		}
		return nil
	case "clear":
		if len(args) != 1 {
			return errors.New("usage: /attach clear")
		}
		items := c.pendingAttachmentSummaries()
		removed, err := c.clearPendingAttachments(ctx)
		if err != nil {
			return err
		}
		slashCommandPrintf("cleared attachments: %d\n", removed)
		for _, item := range items {
			if item.Uploaded {
				slashCommandPrintln("warning: one or more items were already uploaded; remote attachments were not deleted")
				break
			}
		}
		return nil
	case "help":
		slashCommandPrintln("usage: /attach [list|add <path>|paste|remove <number|id|filename>|clear]")
		slashCommandPrintln("On macOS, Ctrl-V or /attach paste reads an image from the local clipboard.")
		return nil
	default:
		return errors.New("usage: /attach [list|add <path>|paste|remove <number|id|filename>|clear]")
	}
}

func attachmentCommandPath(parts []string) string {
	path := strings.TrimSpace(strings.Join(parts, " "))
	if len(path) >= 2 {
		first, last := path[0], path[len(path)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			path = path[1 : len(path)-1]
		}
	}
	return path
}
