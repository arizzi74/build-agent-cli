package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	telegramTurnRelayLimit      = 128
	telegramSendTimeout         = 45 * time.Second
	telegramProgressSendTimeout = 15 * time.Second
	telegramRelayFinishTimeout  = 20 * time.Second
	telegramTypingRefresh       = 4 * time.Second
	telegramTypingSendTimeout   = 5 * time.Second
	telegramTypingFinalWait     = time.Second
)

type telegramActiveTurn struct {
	chatID string
	userID string
	cancel context.CancelFunc
}

type telegramPendingInteraction struct {
	id      string
	chatID  string
	userID  string
	kind    string
	ready   bool
	replies chan string
}

type telegramRelayItem struct {
	text         string
	preformatted bool
	ack          chan error
}

type telegramTypingIndicator struct {
	cancel  context.CancelFunc
	done    chan struct{}
	ready   chan struct{}
	refresh chan chan struct{}
	once    sync.Once
}

func startTelegramTypingIndicator(parent context.Context, api *telegramAPI, chatID string, refresh, sendTimeout time.Duration, warn func(string)) *telegramTypingIndicator {
	ctx, cancel := context.WithCancel(parent)
	indicator := &telegramTypingIndicator{
		cancel:  cancel,
		done:    make(chan struct{}),
		ready:   make(chan struct{}),
		refresh: make(chan chan struct{}, 1),
	}
	if api == nil || !validTelegramNumericID(strings.TrimSpace(chatID)) {
		close(indicator.ready)
		close(indicator.done)
		return indicator
	}
	if refresh <= 0 {
		refresh = telegramTypingRefresh
	}
	if sendTimeout <= 0 {
		sendTimeout = telegramTypingSendTimeout
	}
	go func() {
		defer close(indicator.done)
		ticker := time.NewTicker(refresh)
		defer ticker.Stop()
		warned := false
		initial := true
		var acknowledge chan struct{}
		for {
			sendCtx, sendCancel := context.WithTimeout(ctx, sendTimeout)
			err := api.sendChatAction(sendCtx, chatID, "typing")
			sendCancel()
			if initial {
				close(indicator.ready)
				initial = false
			}
			if err != nil && ctx.Err() == nil && !warned {
				warned = true
				if warn != nil {
					warn("Telegram typing indicator unavailable: " + err.Error())
				}
			}
			if acknowledge != nil {
				close(acknowledge)
				acknowledge = nil
			}
			select {
			case <-ctx.Done():
				return
			case acknowledge = <-indicator.refresh:
			case <-ticker.C:
			}
		}
	}()
	return indicator
}

// WaitReady bounds the initial ordering barrier: the first chat-action request
// has completed (successfully or not) before the first progress message can
// clear it. A failed action never blocks the actual Build Agent turn.
func (indicator *telegramTypingIndicator) WaitReady(ctx context.Context) {
	if indicator == nil {
		return
	}
	select {
	case <-indicator.ready:
	case <-ctx.Done():
	}
}

// Refresh requests an immediate reassertion after a progress send. Requests
// are deliberately coalesced: one action after the newest message is enough,
// and transport callbacks must not block on cosmetic Telegram state.
func (indicator *telegramTypingIndicator) Refresh() {
	if indicator == nil {
		return
	}
	select {
	case indicator.refresh <- nil:
	default:
	}
}

// RefreshAndWait is used only at the final relay boundary. It guarantees that
// the most recent progress message is followed by a chat action before the
// final response is handed back, while still respecting turn cancellation.
func (indicator *telegramTypingIndicator) RefreshAndWait(ctx context.Context) {
	if indicator == nil {
		return
	}
	acknowledge := make(chan struct{})
	select {
	case indicator.refresh <- acknowledge:
	case <-indicator.done:
		return
	case <-ctx.Done():
		return
	}
	select {
	case <-acknowledge:
	case <-indicator.done:
	case <-ctx.Done():
	}
}

func (indicator *telegramTypingIndicator) Stop() {
	if indicator == nil {
		return
	}
	indicator.once.Do(func() {
		indicator.cancel()
		<-indicator.done
	})
}

// telegramTurnRelay keeps transport callbacks non-blocking while preserving
// their order for Telegram. Network I/O happens on its own goroutine; finish
// drains that queue before the final assistant response is sent.
type telegramTurnRelay struct {
	service *telegramService
	chatID  string
	ctx     context.Context
	cancel  context.CancelFunc

	mu      sync.Mutex
	queue   []telegramRelayItem
	closed  bool
	stopped bool
	dropped int
	wake    chan struct{}
	done    chan struct{}
	err     error
	typing  *telegramTypingIndicator
}

func newTelegramTurnRelay(service *telegramService, chatID string) *telegramTurnRelay {
	ctx, cancel := context.WithCancel(service.serviceContext())
	relay := &telegramTurnRelay{service: service, chatID: chatID, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go relay.run()
	return relay
}

func (r *telegramTurnRelay) setTypingIndicator(indicator *telegramTypingIndicator) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.typing = indicator
	r.mu.Unlock()
}

func (r *telegramTurnRelay) refreshTyping() {
	if r == nil {
		return
	}
	r.mu.Lock()
	indicator := r.typing
	r.mu.Unlock()
	indicator.Refresh()
}

func (r *telegramTurnRelay) add(text string) {
	r.enqueue(text, false)
}

func (r *telegramTurnRelay) addPreformatted(text string) {
	r.enqueue(text, true)
}

func (r *telegramTurnRelay) enqueue(text string, preformatted bool) {
	if r == nil {
		return
	}
	text = telegramSafeRemoteText(text, 0)
	if text == "" {
		return
	}
	r.mu.Lock()
	if r.closed || r.stopped {
		r.mu.Unlock()
		return
	}
	if len(r.queue) >= telegramTurnRelayLimit {
		r.dropped++
		r.mu.Unlock()
		return
	}
	r.queue = append(r.queue, telegramRelayItem{text: text, preformatted: preformatted})
	r.mu.Unlock()
	r.wakeSender()
}

func (r *telegramTurnRelay) sendRequired(ctx context.Context, text string) error {
	if r == nil {
		return errors.New("Telegram progress relay is unavailable")
	}
	text = telegramSafeRemoteText(text, 0)
	if text == "" {
		return errors.New("Telegram interaction message is empty")
	}
	item := telegramRelayItem{text: text, ack: make(chan error, 1)}
	r.mu.Lock()
	if r.closed || r.stopped {
		err := r.err
		if err == nil {
			err = errors.New("Telegram progress relay is closed")
		}
		r.mu.Unlock()
		return err
	}
	if len(r.queue) >= telegramTurnRelayLimit {
		removed := -1
		for i := len(r.queue) - 1; i >= 0; i-- {
			if r.queue[i].ack == nil {
				removed = i
				break
			}
		}
		if removed < 0 {
			r.mu.Unlock()
			return errors.New("Telegram interaction queue is full")
		}
		copy(r.queue[removed:], r.queue[removed+1:])
		r.queue[len(r.queue)-1] = telegramRelayItem{}
		r.queue = r.queue[:len(r.queue)-1]
		r.dropped++
	}
	// Required interactions may authorize host mutations. Put them ahead of
	// ordinary cosmetic progress so delivery acknowledgement cannot sit behind
	// an arbitrarily long progress backlog.
	r.queue = append(r.queue, telegramRelayItem{})
	copy(r.queue[1:], r.queue[:len(r.queue)-1])
	r.queue[0] = item
	r.mu.Unlock()
	r.wakeSender()
	select {
	case err := <-item.ack:
		return err
	case <-ctx.Done():
		// The required item uses the relay context for network I/O. Stop that
		// relay as well so a command timeout cannot deliver a stale approval
		// prompt after its pending interaction has already been cleared.
		r.cancel()
		return ctx.Err()
	case <-r.ctx.Done():
		if err := r.relayError(); err != nil {
			return err
		}
		return r.ctx.Err()
	}
}

func (r *telegramTurnRelay) wakeSender() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *telegramTurnRelay) relayError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *telegramTurnRelay) presentation(event turnPresentationEvent) {
	if text := telegramPresentationText(event); text != "" {
		if telegramPresentationUsesTerminalStyle(event.Kind) {
			r.addPreformatted(telegramTerminalPresentationText(event, text))
			return
		}
		r.add(text)
	}
}

func telegramPresentationUsesTerminalStyle(kind string) bool {
	switch kind {
	case turnPresentationToolStarted, turnPresentationToolCompleted, turnPresentationToolWarning, turnPresentationBuildProgress:
		return true
	default:
		return false
	}
}

// telegramTerminalPresentationText keeps tool progress visually close to the
// terminal TUI. Telegram has no ANSI/CSS colour support, so coloured emoji
// markers convey running, success, failure, warning, and build states inside
// the fixed-width block without relying on an unsupported parse mode.
func telegramTerminalPresentationText(event turnPresentationEvent, plain string) string {
	switch event.Kind {
	case turnPresentationToolStarted:
		return "🟢 " + strings.TrimPrefix(plain, "▶ ")
	case turnPresentationToolCompleted:
		if event.Success {
			return "✅ " + strings.TrimPrefix(plain, "✓ ")
		}
		return "❌ " + strings.TrimPrefix(plain, "✗ ")
	case turnPresentationToolWarning:
		return "⚠️ " + strings.TrimPrefix(plain, "⚠ ")
	case turnPresentationBuildProgress:
		return "🛠️ " + plain
	default:
		return plain
	}
}

func telegramPresentationText(event turnPresentationEvent) string {
	name := telegramSafeRemoteText(event.Name, 160)
	text := telegramSafeRemoteText(event.Text, 1000)
	suffix := ""
	if text != "" {
		suffix = "\n" + text
	}
	switch event.Kind {
	case turnPresentationToolStarted:
		if name == "" {
			name = "tool"
		}
		return "▶ Tool started: " + name
	case turnPresentationToolCompleted:
		if name == "" {
			name = "tool"
		}
		marker := "✓"
		state := "completed"
		if !event.Success {
			marker, state = "✗", "failed"
		}
		return fmt.Sprintf("%s Tool %s: %s%s", marker, state, name, suffix)
	case turnPresentationToolWarning:
		return "⚠ Tool warning: " + name + suffix
	case turnPresentationBuildProgress:
		return "Build: " + text
	case turnPresentationSummary:
		return "Summary: " + text
	case turnPresentationSubAgentStart:
		return "↳ Sub-agent started: " + name
	case turnPresentationSubAgentEnd:
		return "↳ Sub-agent completed: " + name
	case turnPresentationRuntimeError:
		return "Error: " + text
	case turnPresentationRetry, turnPresentationFallback:
		return text
	case turnPresentationUsage:
		return text
	default:
		return ""
	}
}

func (r *telegramTurnRelay) run() {
	defer close(r.done)
	for {
		r.mu.Lock()
		if len(r.queue) > 0 {
			item := r.queue[0]
			copy(r.queue, r.queue[1:])
			r.queue[len(r.queue)-1] = telegramRelayItem{}
			r.queue = r.queue[:len(r.queue)-1]
			r.mu.Unlock()
			sendCtx, cancel := context.WithTimeout(r.ctx, telegramProgressSendTimeout)
			var err error
			if item.preformatted {
				err = r.service.sendPreformattedText(sendCtx, r.chatID, item.text)
			} else {
				err = r.service.sendText(sendCtx, r.chatID, item.text)
			}
			cancel()
			if item.ack != nil {
				item.ack <- err
			}
			if err != nil {
				r.stop(err)
				return
			}
			r.refreshTyping()
			continue
		}
		if r.closed {
			if r.dropped > 0 {
				dropped := r.dropped
				r.dropped = 0
				r.mu.Unlock()
				sendCtx, cancel := context.WithTimeout(r.ctx, telegramProgressSendTimeout)
				err := r.service.sendText(sendCtx, r.chatID, fmt.Sprintf("%d additional progress updates were compacted.", dropped))
				cancel()
				if err != nil {
					r.stop(err)
				} else {
					r.refreshTyping()
				}
				return
			}
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		select {
		case <-r.wake:
		case <-r.ctx.Done():
			r.stop(r.ctx.Err())
			return
		}
	}
}

func (r *telegramTurnRelay) stop(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.err == nil && err != nil {
		r.err = err
	}
	r.stopped = true
	pending := r.queue
	r.queue = nil
	r.mu.Unlock()
	for _, item := range pending {
		if item.ack != nil {
			item.ack <- err
		}
	}
}

func (r *telegramTurnRelay) finish() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.wakeSender()
	timer := time.NewTimer(telegramRelayFinishTimeout)
	defer timer.Stop()
	select {
	case <-r.done:
	case <-timer.C:
		r.cancel()
		return errors.New("Telegram progress flush timed out; final response delivery continued")
	}
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (s *telegramService) serviceContext() context.Context {
	if s != nil && s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *telegramService) setActiveTurn(chatID, userID string, cancel context.CancelFunc) {
	s.turnControlMu.Lock()
	s.activeTurn = telegramActiveTurn{chatID: chatID, userID: userID, cancel: cancel}
	s.turnControlMu.Unlock()
}

func (s *telegramService) clearActiveTurn(chatID, userID string) {
	s.turnControlMu.Lock()
	if s.activeTurn.chatID == chatID && s.activeTurn.userID == userID {
		s.activeTurn = telegramActiveTurn{}
	}
	s.turnControlMu.Unlock()
}

func (s *telegramService) cancelOwnedTurn(chatID, userID string) (cancelled bool, ownedByOther bool) {
	s.turnControlMu.Lock()
	active := s.activeTurn
	s.turnControlMu.Unlock()
	if active.chatID == "" {
		return false, false
	}
	if active.chatID != chatID || active.userID != userID {
		return false, true
	}
	if active.cancel != nil {
		active.cancel()
		return true, false
	}
	return false, false
}

func (s *telegramService) deliverInteractionReply(chatID, userID, text string) bool {
	s.turnControlMu.Lock()
	pending := s.pendingInteraction
	if pending == nil || pending.chatID != chatID || pending.userID != userID {
		s.turnControlMu.Unlock()
		return false
	}
	if !pending.ready {
		s.turnControlMu.Unlock()
		// The explicit ready acknowledgement is the authorization boundary.
		// Ignore early guesses/replies so they cannot race ahead of complete
		// delivery (and cannot appear after the ready message out of order).
		return true
	}
	text = strings.TrimSpace(text)
	command, argument := telegramCommandParts(text)
	switch command {
	case "":
		s.turnControlMu.Unlock()
		return false
	case "/answer":
		fields := strings.Fields(argument)
		if len(fields) < 2 || !strings.EqualFold(fields[0], pending.id) {
			expected := pending.id
			s.turnControlMu.Unlock()
			s.sendAsyncMirrored(chatID, "That interaction reply does not match the active request. Use /answer "+expected+" <answer>.")
			return true
		}
		text = strings.TrimSpace(strings.TrimPrefix(argument, fields[0]))
	case "/turn_approve":
		text = "Approve"
	case "/turn_reject":
		text = "Reject"
	default:
		if strings.HasPrefix(command, "/") {
			s.turnControlMu.Unlock()
			return false
		}
	}
	if strings.TrimSpace(text) == "" {
		s.turnControlMu.Unlock()
		s.sendAsyncMirrored(chatID, "The interaction reply cannot be empty.")
		return true
	}
	if pending.kind == "approval" {
		if _, err := parseTurnApprovalAnswer(text); err != nil {
			s.turnControlMu.Unlock()
			s.sendAsyncMirrored(chatID, "Reply Approve or Reject for the active request.")
			return true
		}
	}
	select {
	case pending.replies <- text:
	default:
	}
	if s.pendingInteraction == pending {
		s.pendingInteraction = nil
	}
	s.turnControlMu.Unlock()
	return true
}

func (s *telegramService) requestInteraction(ctx context.Context, chatID, userID string, relay *telegramTurnRelay, request turnInteractionRequest) (string, error) {
	pending := &telegramPendingInteraction{id: strings.ToUpper(uuidV4Compact()[:6]), chatID: chatID, userID: userID, kind: request.Kind, replies: make(chan string, 1)}
	s.turnControlMu.Lock()
	if s.pendingInteraction != nil {
		s.turnControlMu.Unlock()
		return "", errors.New("another Telegram interaction is already pending")
	}
	s.pendingInteraction = pending
	s.turnControlMu.Unlock()
	defer func() {
		s.turnControlMu.Lock()
		if s.pendingInteraction == pending {
			s.pendingInteraction = nil
		}
		s.turnControlMu.Unlock()
	}()
	requestMessage := formatTelegramInteraction(pending.id, request)
	if err := relay.sendRequired(ctx, requestMessage); err != nil {
		return "", fmt.Errorf("Telegram could not deliver the interaction request: %w", err)
	}
	if client, pinned := s.pinnedClient(); pinned {
		mirrorTelegramSystemToTerminal(client, requestMessage)
	}
	readyMessage := fmt.Sprintf("Input required [%s] is ready. Reply now", pending.id)
	if request.Kind == "approval" {
		readyMessage += " with Approve or Reject."
	} else {
		readyMessage += fmt.Sprintf(" directly, or use /answer %s <answer> when the answer starts with /.", pending.id)
	}
	if err := relay.sendRequired(ctx, readyMessage); err != nil {
		return "", fmt.Errorf("Telegram could not confirm the interaction request: %w", err)
	}
	if client, pinned := s.pinnedClient(); pinned {
		mirrorTelegramSystemToTerminal(client, readyMessage)
	}
	s.turnControlMu.Lock()
	if s.pendingInteraction == pending {
		pending.ready = true
	}
	s.turnControlMu.Unlock()
	select {
	case answer := <-pending.replies:
		if strings.TrimSpace(answer) == "" {
			return "", errors.New("Telegram reply cannot be empty")
		}
		return answer, nil
	case <-ctx.Done():
		return s.finishInteractionWait(pending, ctx.Err())
	case <-s.serviceContext().Done():
		return s.finishInteractionWait(pending, s.serviceContext().Err())
	}
}

func (s *telegramService) finishInteractionWait(pending *telegramPendingInteraction, waitErr error) (string, error) {
	s.turnControlMu.Lock()
	if s.pendingInteraction == pending {
		s.pendingInteraction = nil
		s.turnControlMu.Unlock()
		return "", waitErr
	}
	// A valid delivery atomically removes the pending request only after putting
	// its answer in the buffered channel, so a reply that races cancellation is
	// never silently swallowed.
	answer := <-pending.replies
	s.turnControlMu.Unlock()
	return answer, nil
}

func formatTelegramInteraction(id string, request turnInteractionRequest) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Input required [%s]", id)
	if prompt := telegramSafeRemoteText(request.Prompt, 0); prompt != "" {
		out.WriteString("\n")
		out.WriteString(prompt)
	}
	for _, row := range request.Rows {
		key := telegramSafeRemoteText(row[0], 0)
		value := telegramSafeRemoteText(row[1], 0)
		if key != "" || value != "" {
			fmt.Fprintf(&out, "\n%s: %s", key, value)
		}
	}
	for i, option := range request.Options {
		fmt.Fprintf(&out, "\n%d. %s", i+1, telegramSafeRemoteText(option, 0))
	}
	if request.Kind == "approval" {
		out.WriteString("\nReply Approve or Reject (or /turn_approve / /turn_reject). Use /cancel to cancel the turn.")
	} else {
		fmt.Fprintf(&out, "\nReply with your answer. If it starts with /, use /answer %s <answer>. Use /cancel to cancel the turn.", id)
	}
	return out.String()
}

func (s *telegramService) runTelegramPrompt(in telegramQueuedCommand, prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "Send a text message to start a Build Agent turn."
	}
	bacliActionMu.Lock()
	client, pinned := s.pinnedClient()
	if !pinned {
		bacliActionMu.Unlock()
		return "The bacli instance changed. Restart bacli to bind Telegram to the new client securely."
	}
	if !in.inputMirrored {
		mirrorTelegramInputToTerminal(client, in.text)
	}
	if err := client.requirePendingAttachmentOwner(telegramPendingAttachmentOwner(in.chatID, in.userID)); err != nil {
		bacliActionMu.Unlock()
		return "Build Agent turn not started: " + err.Error()
	}
	turnBase, turnCancel := context.WithCancel(s.serviceContext())
	turnCtx := withTelegramCommandContext(turnBase, in.chatID, in.userID)
	promptCtx, promptCancel := context.WithCancel(turnCtx)
	s.setActiveTurn(in.chatID, in.userID, promptCancel)
	defer turnCancel()
	defer s.clearActiveTurn(in.chatID, in.userID)
	defer promptCancel()
	typing := startTelegramTypingIndicator(turnCtx, s.api, in.chatID, telegramTypingRefresh, telegramTypingSendTimeout, s.warn)
	defer typing.Stop()
	typing.WaitReady(promptCtx)
	relay := newTelegramTurnRelay(s, in.chatID)
	relay.setTypingIndicator(typing)
	relay.add("Building… Use /cancel to interrupt.")
	restore := client.installTurnFrontend(func(event turnPresentationEvent) {
		relay.presentation(event)
		mirrorTelegramPresentationToTerminal(client, event)
	}, func(ctx context.Context, request turnInteractionRequest) (string, error) {
		return s.requestInteraction(ctx, in.chatID, in.userID, relay, request)
	})
	response, err := runPromptForResponseLocked(promptCtx, client, prompt, s.timeout)
	s.clearActiveTurn(in.chatID, in.userID)
	restore()
	// The terminal and Telegram command paths may proceed once client state is
	// stable. Slow progress delivery must not retain the global action lock.
	bacliActionMu.Unlock()
	if sendErr := relay.finish(); sendErr != nil && s.serviceContext().Err() == nil {
		s.warn("Telegram progress send failed: " + sendErr.Error())
	}
	typingFinalCtx, typingFinalCancel := context.WithTimeout(turnCtx, telegramTypingFinalWait)
	typing.RefreshAndWait(typingFinalCtx)
	typingFinalCancel()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "Build Agent turn cancelled."
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "Build Agent turn timed out."
		}
		return "Build Agent turn failed: " + telegramSafeRemoteText(err.Error(), 1000)
	}
	if strings.TrimSpace(response) == "" {
		return "Build Agent completed without a text response."
	}
	return response
}
