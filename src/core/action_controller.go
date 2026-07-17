package core

import (
	"context"
	"strings"
	"sync"
	"time"
)

// bacliActionMu serializes stateful client operations from every front end.
// Cancellation deliberately bypasses it so a remote /cancel can interrupt a turn.
var bacliActionMu sync.Mutex

type activeClientRef struct {
	mu     sync.RWMutex
	client *Client
}

func newActiveClientRef(client *Client) *activeClientRef { return &activeClientRef{client: client} }

func (r *activeClientRef) Get() *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.client
}

func (r *activeClientRef) Set(client *Client) {
	r.mu.Lock()
	r.client = client
	r.mu.Unlock()
}

func (c *Client) resetTurnFinalText() {
	c.turnResultMu.Lock()
	c.turnResultGeneration++
	c.turnFinalText = ""
	c.turnFinalRendered = false
	c.turnRenderedAssistantText = ""
	c.turnResultMu.Unlock()
}

func (c *Client) setTurnFinalText(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	c.turnResultMu.Lock()
	c.turnFinalText = text
	c.turnFinalRendered = c.turnRenderedAssistantText == text
	c.turnResultMu.Unlock()
}

func (c *Client) noteTurnAssistantRendered(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	c.turnResultMu.Lock()
	c.turnRenderedAssistantText = text
	if c.turnFinalText == text {
		c.turnFinalRendered = true
	}
	c.turnResultMu.Unlock()
}

func (c *Client) getTurnFinalText() string {
	c.turnResultMu.Lock()
	defer c.turnResultMu.Unlock()
	return c.turnFinalText
}

func (c *Client) turnFinalResult() (text string, rendered bool, generation uint64) {
	c.turnResultMu.Lock()
	defer c.turnResultMu.Unlock()
	return c.turnFinalText, c.turnFinalRendered, c.turnResultGeneration
}

func runPromptForResponse(ctx context.Context, client *Client, prompt string, timeout time.Duration) (string, error) {
	bacliActionMu.Lock()
	defer bacliActionMu.Unlock()
	return runPromptForResponseLocked(ctx, client, prompt, timeout)
}

func runPromptForActiveClient(ctx context.Context, clients *activeClientRef, prompt string, timeout time.Duration) (*Client, error) {
	bacliActionMu.Lock()
	defer bacliActionMu.Unlock()
	client := clients.Get()
	if client == nil {
		return nil, context.Canceled
	}
	_, err := runPromptForResponseLocked(ctx, client, prompt, timeout)
	return client, err
}

func runPromptForResponseLocked(ctx context.Context, client *Client, prompt string, timeout time.Duration) (string, error) {
	attachmentOwner := "local"
	if source, ok := telegramCommandSourceFromContext(ctx); ok {
		attachmentOwner = telegramPendingAttachmentOwner(source.ChatID, source.UserID)
	}
	if err := client.requirePendingAttachmentOwner(attachmentOwner); err != nil {
		return "", err
	}
	client.resetTurnFinalText()
	turnCtx := client.beginActiveTurn(ctx)
	defer client.endActiveTurn()
	if err := client.SendMessage(turnCtx, prompt); err != nil {
		return "", err
	}
	waitCtx, cancel := context.WithTimeout(turnCtx, timeout)
	defer cancel()
	if err := waitPromptTurn(client, waitCtx); err != nil {
		return "", err
	}
	return client.getTurnFinalText(), nil
}

func waitPromptTurn(client *Client, ctx context.Context) error {
	err := client.WaitTurn(ctx)
	// Transport closure can race the closed channel without clearing processing.
	// Do not relabel an already-terminal server error as cancellation, but always
	// stop a non-terminal turn before endActiveTurn removes its cancel handle.
	if err != nil && client.processing {
		client.cancelActiveTurn()
	}
	return err
}
