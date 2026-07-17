package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type telegramQueuedCommand struct {
	chatID        string
	userID        string
	text          string
	inputMirrored bool
}

type telegramService struct {
	profile     string
	cfg         telegramConfig
	api         *telegramAPI
	botID       string
	fingerprint string
	clients     *activeClientRef
	client      *Client
	timeout     time.Duration

	ctx      context.Context
	cancel   context.CancelFunc
	commands chan telegramQueuedCommand
	fatalErr chan error
	wg       sync.WaitGroup
	lockFile *os.File

	clientMu           sync.RWMutex
	outboundMu         sync.Mutex
	outboundLast       time.Time
	turnControlMu      sync.Mutex
	activeTurn         telegramActiveTurn
	pendingInteraction *telegramPendingInteraction
}

func maybeStartTelegramService(parent context.Context, profile string, clients *activeClientRef, timeout time.Duration, environmentToken string) (*telegramService, error) {
	if !isValidProfile(profile) {
		return nil, fmt.Errorf("invalid profile %q", profile)
	}
	pinnedClient := clients.Get()
	if pinnedClient == nil || pinnedClient.opts.Profile != profile {
		return nil, errors.New("Telegram channel could not pin the active profile client")
	}
	cfg, _, err := loadTelegramConfig()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled || cfg.DMPolicy == telegramPolicyDisabled {
		return nil, nil
	}
	token, _, err := resolveTelegramTokenWithEnvironment(cfg, environmentToken)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, nil
	}
	fingerprint := telegramTokenFingerprint(token)
	lockFile, err := acquireTelegramPollerLock(fingerprint)
	if err != nil {
		return nil, err
	}
	closeLock := true
	defer func() {
		if closeLock {
			unlockProfileFile(lockFile)
			_ = lockFile.Close()
		}
	}()

	api := newTelegramAPI(token)
	startupCtx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	bot, err := api.getMe(startupCtx)
	if err != nil {
		return nil, err
	}
	botID := telegramID(bot.ID)
	if !bot.IsBot || !validTelegramNumericID(botID) {
		return nil, errors.New("Telegram getMe did not return a valid bot identity")
	}
	state, err := bindTelegramState(botID, fingerprint)
	if err != nil {
		return nil, err
	}
	if err := api.deleteWebhook(startupCtx); err != nil {
		return nil, fmt.Errorf("Telegram webhook cleanup failed: %w", err)
	}
	if err := validateTelegramPublishedCommands(telegramPublishedCommands()); err != nil {
		return nil, fmt.Errorf("Telegram command catalog is invalid: %w", err)
	}
	if err := api.setMyCommands(startupCtx); err != nil {
		// Command-menu publication is useful discovery metadata, but it is not
		// part of the bot's authorization or polling safety boundary. Keep the
		// authenticated channel available when Telegram temporarily rejects a
		// menu refresh (including legacy default-scope cleanup).
		pinnedClient.printRuntimeError("warning: Telegram command menu could not be refreshed: " + err.Error())
	}

	ctx, stop := context.WithCancel(parent)
	service := &telegramService{
		profile: profile, cfg: cfg, api: api, botID: botID, fingerprint: fingerprint, clients: clients, client: pinnedClient, timeout: timeout,
		ctx: ctx, cancel: stop, commands: make(chan telegramQueuedCommand, 64), fatalErr: make(chan error, 1), lockFile: lockFile,
	}
	service.wg.Add(2)
	go service.poll(state.LastUpdateID + 1)
	go service.runCommands()
	closeLock = false
	return service, nil
}

func acquireTelegramPollerLock(fingerprint string) (*os.File, error) {
	dir := filepath.Join(stateDir(), "telegram-pollers")
	path := filepath.Join(dir, fingerprint+".lock")
	if err := rejectSymlinkPath(path); err != nil {
		return nil, errors.New("unsafe Telegram poller lock path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("could not create Telegram poller lock directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, errors.New("could not secure Telegram poller lock directory")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("could not open Telegram poller lock")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, errors.New("could not secure Telegram poller lock")
	}
	locked, err := tryLockProfileFile(file)
	if err != nil {
		_ = file.Close()
		return nil, errors.New("could not lock Telegram poller")
	}
	if !locked {
		_ = file.Close()
		return nil, errors.New("another bacli process is already polling this Telegram bot")
	}
	return file, nil
}

func (s *telegramService) Close() {
	if s == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
	if s.lockFile != nil {
		unlockProfileFile(s.lockFile)
		_ = s.lockFile.Close()
		s.lockFile = nil
	}
}

func (s *telegramService) Done() <-chan struct{} { return s.ctx.Done() }

func (s *telegramService) Fatal() <-chan error { return s.fatalErr }

func (s *telegramService) fail(err error) {
	select {
	case s.fatalErr <- err:
	default:
	}
	s.cancel()
}

func (s *telegramService) warn(message string) {
	message = singleLineLabel(message)
	if message == "" || s == nil || s.clients == nil {
		return
	}
	if client := s.clients.Get(); client != nil {
		client.printRuntimeError(message)
	}
}

func (s *telegramService) pinnedClient() (*Client, bool) {
	if s == nil || s.clients == nil {
		return nil, false
	}
	s.clientMu.RLock()
	pinned := s.client
	profile := s.profile
	s.clientMu.RUnlock()
	current := s.clients.Get()
	return current, current != nil && current == pinned && current.opts.Profile == profile
}

func (s *telegramService) adoptClientAfterRemoteSwitch(previous *Client) bool {
	current := s.clients.Get()
	if current == nil || current == previous {
		return false
	}
	s.clientMu.Lock()
	s.client = current
	s.profile = current.opts.Profile
	s.clientMu.Unlock()
	return true
}

func (s *telegramService) authorization(userID string) (telegramConfig, bool, error) {
	var cfg telegramConfig
	var state telegramState
	err := withTelegramLock(func() error {
		var err error
		cfg, _, err = loadTelegramConfig()
		if err != nil {
			return err
		}
		state, err = readTelegramState()
		return err
	})
	if err != nil {
		return cfg, false, err
	}
	if !cfg.Enabled || cfg.DMPolicy == telegramPolicyDisabled {
		return cfg, false, nil
	}
	if state.BotID != s.botID || state.TokenFingerprint != s.fingerprint {
		return cfg, false, errors.New("Telegram bot identity changed; restart bacli")
	}
	return cfg, telegramUserAuthorized(cfg, state, userID), nil
}

func (s *telegramService) poll(offset int64) {
	defer s.wg.Done()
	backoff := time.Second
	for s.ctx.Err() == nil {
		updates, err := s.api.getUpdates(s.ctx, offset, s.cfg.PollTimeout)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			delay, fatal := telegramPollRetry(err, backoff)
			if fatal {
				s.warn("Telegram channel stopped: " + err.Error())
				s.fail(err)
				return
			}
			if delay > backoff {
				backoff = delay
			} else if backoff < 30*time.Second {
				backoff *= 2
			}
			if err := waitTelegramRetry(s.ctx, delay); err != nil {
				return
			}
			continue
		}
		backoff = time.Second
		for _, update := range updates {
			if update.UpdateID < offset {
				continue
			}
			// Persist before any side effect. A crash can drop an accepted command,
			// but it cannot replay a mutating command after restart.
			if err := recordTelegramUpdate(s.botID, s.fingerprint, update.UpdateID); err != nil {
				err = errors.New("Telegram channel could not persist its update offset")
				s.warn(err.Error())
				s.fail(err)
				return
			}
			offset = update.UpdateID + 1
			s.acceptUpdate(update)
		}
	}
}

func telegramPollRetry(err error, fallback time.Duration) (time.Duration, bool) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, true
	}
	var apiErr *telegramAPIError
	if !errors.As(err, &apiErr) {
		return clampTelegramDelay(fallback), false
	}
	switch apiErr.Code {
	case 401, 403:
		return 0, true
	case 409:
		return 10 * time.Second, false
	case 429:
		if apiErr.RetryAfter > 0 {
			return clampTelegramDelay(apiErr.RetryAfter), false
		}
		return clampTelegramDelay(fallback), false
	case 0, 408:
		return clampTelegramDelay(fallback), false
	default:
		if apiErr.Code >= 500 {
			return clampTelegramDelay(fallback), false
		}
		return 10 * time.Second, false
	}
}

func clampTelegramDelay(delay time.Duration) time.Duration {
	if delay < time.Second {
		return time.Second
	}
	if delay > 60*time.Second {
		return 60 * time.Second
	}
	return delay
}

func (s *telegramService) acceptUpdate(update telegramUpdate) {
	message := update.Message
	if message == nil || message.From == nil || message.From.IsBot || message.Chat.Type != "private" {
		return
	}
	userID := telegramID(message.From.ID)
	chatID := telegramID(message.Chat.ID)
	if !validTelegramNumericID(userID) || !validTelegramNumericID(chatID) || userID != chatID {
		return
	}
	text := strings.TrimSpace(message.Text)
	command, _ := telegramCommandParts(text)
	if command == "/whoami" {
		response := "Your numeric Telegram user ID is " + userID + "."
		// /whoami remains available before pairing, but only an already
		// authorized sender may write into the managed terminal transcript.
		if _, pinned := s.pinnedClient(); pinned {
			if _, authorized, authErr := s.authorization(userID); authErr == nil && authorized {
				client := s.currentTerminalClient()
				mirrorTelegramInputToTerminal(client, text)
				mirrorTelegramSystemToTerminal(client, response)
			}
		}
		s.sendAsync(chatID, response)
		return
	}
	if _, pinned := s.pinnedClient(); !pinned {
		s.sendAsync(chatID, "The bacli instance changed. Restart bacli to bind Telegram to the new client securely.")
		return
	}
	cfg, authorized, err := s.authorization(userID)
	if err != nil {
		s.warn("Telegram message denied: state could not be read safely")
		return
	}
	if !authorized {
		s.handleUnknownUser(cfg, message, userID, chatID)
		return
	}
	if command == "/cancel" {
		mirrorTelegramInputToTerminal(s.currentTerminalClient(), text)
		s.cancelAndRespond(chatID, userID)
		return
	}
	inputMirrored := false
	if s.telegramInteractionConsumesInput(chatID, userID, text) {
		mirrorTelegramInputToTerminal(s.currentTerminalClient(), text)
		inputMirrored = true
	}
	if s.deliverInteractionReply(chatID, userID, text) {
		return
	}
	if text == "" {
		s.sendAsyncMirrored(chatID, "Send a text message to start a Build Agent turn, or use /help.")
		return
	}
	if !s.enqueue(telegramQueuedCommand{chatID: chatID, userID: userID, text: text, inputMirrored: inputMirrored}) && s.ctx.Err() == nil {
		if !inputMirrored {
			mirrorTelegramInputToTerminal(s.currentTerminalClient(), text)
		}
		s.sendAsyncMirrored(chatID, "The bacli command queue is full. Use /cancel for the active turn or try again later.")
	}
}

func (s *telegramService) telegramInteractionConsumesInput(chatID, userID, text string) bool {
	if s == nil {
		return false
	}
	s.turnControlMu.Lock()
	pending := s.pendingInteraction
	if pending == nil || pending.chatID != chatID || pending.userID != userID {
		s.turnControlMu.Unlock()
		return false
	}
	ready := pending.ready
	s.turnControlMu.Unlock()
	if !ready {
		return true
	}
	command, _ := telegramCommandParts(strings.TrimSpace(text))
	if command == "" {
		return false
	}
	switch command {
	case "/answer", "/turn_approve", "/turn_reject":
		return true
	default:
		return !strings.HasPrefix(command, "/")
	}
}

func (s *telegramService) enqueue(command telegramQueuedCommand) bool {
	select {
	case s.commands <- command:
		return true
	case <-s.ctx.Done():
		return false
	default:
		return false
	}
}

func (s *telegramService) handleUnknownUser(cfg telegramConfig, message *telegramMessage, userID, chatID string) {
	if !cfg.Enabled || cfg.DMPolicy != telegramPolicyPairing {
		s.sendAsync(chatID, "This Telegram user is not authorized. Use /whoami and ask the bacli operator to add the numeric user ID.")
		return
	}
	label := strings.TrimSpace(strings.Join([]string{message.From.FirstName, message.From.LastName}, " "))
	if message.From.Username != "" {
		label = strings.TrimSpace(label + " (@" + message.From.Username + ")")
	}
	label = safeTelegramPairingLabel(label)
	code, created, err := createTelegramPairing(s.botID, s.fingerprint, userID, chatID, label, time.Now().UTC())
	if err != nil {
		s.sendAsync(chatID, "Pairing could not be created: "+err.Error())
		return
	}
	if !created {
		s.sendAsync(chatID, "A pairing request is already pending. Ask the bacli operator to inspect `bacli --telegram-status`.")
		return
	}
	messageText := fmt.Sprintf("Pairing required. Your one-hour code is %s. Ask the bacli operator to run locally: bacli --telegram-approve %s", code, code)
	s.sendAsync(chatID, messageText)
}

func (s *telegramService) sendAsync(chatID, text string) {
	ctx, cancel := context.WithTimeout(s.serviceContext(), telegramSendTimeout)
	defer cancel()
	if err := s.sendText(ctx, chatID, text); err != nil && s.serviceContext().Err() == nil {
		s.warn("Telegram send failed: " + err.Error())
	}
}

func (s *telegramService) sendAsyncMirrored(chatID, text string) {
	if client := s.currentTerminalClient(); client != nil {
		mirrorTelegramSystemToTerminal(client, text)
	}
	s.sendAsync(chatID, text)
}

func (s *telegramService) currentTerminalClient() *Client {
	if s == nil || s.clients == nil {
		return nil
	}
	return s.clients.Get()
}

func (s *telegramService) sendText(ctx context.Context, chatID, text string) error {
	if s == nil || s.api == nil {
		return errors.New("Telegram API is unavailable")
	}
	s.outboundMu.Lock()
	defer s.outboundMu.Unlock()
	return s.sendTextLocked(ctx, chatID, text)
}

func (s *telegramService) sendPreformattedText(ctx context.Context, chatID, text string) error {
	if s == nil || s.api == nil {
		return errors.New("Telegram API is unavailable")
	}
	s.outboundMu.Lock()
	defer s.outboundMu.Unlock()
	return s.sendPreformattedTextLocked(ctx, chatID, text)
}

func (s *telegramService) sendTextLocked(ctx context.Context, chatID, text string) error {
	if interval := s.api.messageInterval; interval > 0 && !s.outboundLast.IsZero() {
		if delay := time.Until(s.outboundLast.Add(interval)); delay > 0 {
			if err := waitTelegramRetry(ctx, delay); err != nil {
				return err
			}
		}
	}
	err := s.api.sendText(ctx, chatID, text)
	if err == nil {
		s.outboundLast = time.Now()
	}
	return err
}

func (s *telegramService) sendPreformattedTextLocked(ctx context.Context, chatID, text string) error {
	if interval := s.api.messageInterval; interval > 0 && !s.outboundLast.IsZero() {
		if delay := time.Until(s.outboundLast.Add(interval)); delay > 0 {
			if err := waitTelegramRetry(ctx, delay); err != nil {
				return err
			}
		}
	}
	err := s.api.sendPreformattedText(ctx, chatID, text)
	if err == nil {
		s.outboundLast = time.Now()
	}
	return err
}

// cancelAndRespond takes the outbound sequencing lock before cancelling the
// exact stored command context. Progress already in flight completes first;
// the acknowledgement is then guaranteed to precede the command's final
// cancellation response.
func (s *telegramService) cancelAndRespond(chatID, userID string) {
	s.outboundMu.Lock()
	message := ""
	if _, pinned := s.pinnedClient(); !pinned {
		message = "The bacli instance changed. Restart bacli before using /cancel."
	} else if cancelled, ownedByOther := s.cancelOwnedTurn(chatID, userID); ownedByOther {
		message = "Another authorized user owns the active turn; only its originator can cancel it."
	} else if cancelled {
		message = "Cancellation requested."
	} else {
		message = "No Build Agent turn is currently running."
	}
	ctx, cancel := context.WithTimeout(s.serviceContext(), telegramSendTimeout)
	err := s.sendTextLocked(ctx, chatID, message)
	cancel()
	s.outboundMu.Unlock()
	if client := s.currentTerminalClient(); client != nil {
		mirrorTelegramSystemToTerminal(client, message)
	}
	if err != nil && s.serviceContext().Err() == nil {
		s.warn("Telegram cancellation acknowledgement failed: " + err.Error())
	}
}

func safeTelegramPairingLabel(label string) string {
	label = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return ' '
		}
		return r
	}, label)
	label = singleLineLabel(label)
	runes := []rune(label)
	if len(runes) > 80 {
		label = string(runes[:80])
	}
	return label
}

func (s *telegramService) runCommands() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case command := <-s.commands:
			if s.ctx.Err() != nil {
				return
			}
			terminalClient := s.currentTerminalClient()
			client, pinned := s.pinnedClient()
			if !pinned {
				if !command.inputMirrored {
					mirrorTelegramInputToTerminal(terminalClient, command.text)
				}
				message := "The bacli instance changed. Restart bacli to bind Telegram to the new client securely."
				mirrorTelegramSystemToTerminal(terminalClient, message)
				s.sendAsync(command.chatID, message)
				continue
			}
			_, authorized, err := s.authorization(command.userID)
			if err != nil {
				s.warn("Telegram queued command denied: authorization state could not be read safely")
				continue
			}
			if !authorized {
				if !command.inputMirrored {
					mirrorTelegramInputToTerminal(terminalClient, command.text)
				}
				message := "This Telegram user is no longer authorized."
				mirrorTelegramSystemToTerminal(terminalClient, message)
				s.sendAsync(command.chatID, message)
				continue
			}
			_, _, turnGeneration := client.turnFinalResult()
			response := s.dispatchCommand(command)
			// The command is bound to the pinned client for execution, but the
			// visible response belongs on whichever TUI is safe and current when
			// execution finishes. Force rendering after an instance switch because
			// the old client's render history says nothing about the new viewport.
			mirrorClient := s.currentTerminalClient()
			mirrorGeneration := turnGeneration
			if mirrorClient != nil && mirrorClient != client {
				_, _, mirrorGeneration = mirrorClient.turnFinalResult()
			}
			mirrorTelegramResponseToTerminal(mirrorClient, command.text, response, mirrorGeneration)
			s.sendAsync(command.chatID, response)
		}
	}
}

func (s *telegramService) dispatchCommand(in telegramQueuedCommand) string {
	if !strings.HasPrefix(strings.TrimSpace(in.text), "/") {
		return s.runTelegramPrompt(in, in.text)
	}
	command, argument := telegramCommandParts(in.text)
	command = telegramCanonicalCommand(command)
	switch command {
	case "/start", "/help":
		client, _ := s.pinnedClient()
		if !in.inputMirrored {
			mirrorTelegramInputToTerminal(client, in.text)
		}
		return telegramRemoteHelp(client)
	case "/whoami":
		if !in.inputMirrored {
			mirrorTelegramInputToTerminal(s.currentTerminalClient(), in.text)
		}
		return "Your numeric Telegram user ID is " + in.userID + "."
	case "/cancel":
		if !in.inputMirrored {
			mirrorTelegramInputToTerminal(s.currentTerminalClient(), in.text)
		}
		return "No Build Agent turn is currently running."
	}
	line, ok := telegramRemoteSlashLine(command, argument)
	if !ok {
		if !in.inputMirrored {
			mirrorTelegramInputToTerminal(s.currentTerminalClient(), in.text)
		}
		return "Unknown command. Use /help to see every published BACLI command."
	}
	return s.executeSlash(in, line)
}

func (s *telegramService) executeSlash(in telegramQueuedCommand, line string) string {
	bacliActionMu.Lock()
	client, pinned := s.pinnedClient()
	if !pinned {
		bacliActionMu.Unlock()
		return "The bacli instance changed. Restart bacli to bind Telegram to the new client securely."
	}
	if !in.inputMirrored {
		mirrorTelegramInputToTerminal(client, in.text)
	}
	commandTimeout := s.timeout
	if commandTimeout <= 0 {
		commandTimeout = 30 * time.Minute
	}
	commandBase, commandCancel := context.WithTimeout(s.serviceContext(), commandTimeout)
	commandCtx := withTelegramCommandContext(commandBase, in.chatID, in.userID)
	restoreActionCancel := client.installFrontendActionCancel(commandCancel)
	relay := newTelegramTurnRelay(s, in.chatID)
	restore := client.installTurnFrontend(func(event turnPresentationEvent) {
		relay.presentation(event)
		mirrorTelegramPresentationToTerminal(client, event)
	}, func(ctx context.Context, request turnInteractionRequest) (string, error) {
		return s.requestInteraction(ctx, in.chatID, in.userID, relay, request)
	})
	s.setActiveTurn(in.chatID, in.userID, commandCancel)
	var output strings.Builder
	handled, err := withSlashCommandOutput(&output, func() (bool, error) {
		return handleSlashCommand(commandCtx, client, line)
	})
	response := ""
	if err != nil {
		if output.Len() > 0 {
			response = telegramSafeRemoteText(strings.TrimSpace(output.String())+"\nError: "+err.Error(), 0)
		} else {
			response = "Command failed: " + telegramSafeRemoteText(err.Error(), 1000)
		}
	} else if !handled {
		response = "The command is published for BACLI parity but cannot stop this host process remotely."
	} else {
		command, _ := telegramCommandParts(line)
		if command == "/instance" {
			s.adoptClientAfterRemoteSwitch(client)
		}
		if text := telegramSafeRemoteText(output.String(), 0); text != "" {
			response = text
		} else {
			response = "Command completed."
		}
	}
	if err == nil && handled && telegramSlashRefreshesTerminalTranscript(line) {
		if active := s.currentTerminalClient(); active != nil {
			active.replaceTerminalConversationTranscript(active.statusBarState())
		}
	}
	s.clearActiveTurn(in.chatID, in.userID)
	restoreActionCancel()
	commandCancel()
	restore()
	// Do not retain bacli's global state lock while Telegram drains progress.
	bacliActionMu.Unlock()
	if sendErr := relay.finish(); sendErr != nil && s.serviceContext().Err() == nil {
		s.warn("Telegram command progress send failed: " + sendErr.Error())
	}
	return response
}

func mirrorTelegramInputToTerminal(client *Client, text string) {
	text = telegramTerminalDisplayText(text)
	if client == nil || text == "" || !interactiveTerminalUIEnabled() {
		return
	}
	printLiveUserPrompt(text)
}

func telegramTerminalDisplayText(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, text)
	return strings.TrimSpace(text)
}

func mirrorTelegramSystemToTerminal(client *Client, text string) {
	text = telegramSafeRemoteText(text, 0)
	if client == nil || text == "" || !interactiveTerminalUIEnabled() {
		return
	}
	if !terminalRecordSystemTextAndAppend("Telegram", text, client.statusBarState()) {
		fmt.Fprintf(os.Stderr, "\n[Telegram]\n%s\n", text)
	}
}

func mirrorTelegramResponseToTerminal(client *Client, input, response string, priorTurnGeneration uint64) {
	displayResponse := telegramSafeRemoteText(response, 0)
	if client == nil || displayResponse == "" || !interactiveTerminalUIEnabled() {
		return
	}
	if strings.HasPrefix(strings.TrimSpace(input), "/") {
		mirrorTelegramSystemToTerminal(client, displayResponse)
		return
	}
	if telegramTurnFinalAlreadyRendered(client, response, priorTurnGeneration) {
		return
	}
	printAssistantText(displayResponse)
}

func telegramTurnFinalAlreadyRendered(client *Client, response string, priorTurnGeneration uint64) bool {
	if client == nil {
		return false
	}
	finalText, rendered, generation := client.turnFinalResult()
	// Compare the raw response, not its terminal-safe projection. Distinct
	// responses can sanitize to the same display text and must not suppress one
	// another. Generation also binds the evidence to this dispatched turn.
	return generation != priorTurnGeneration && rendered && finalText == response
}

func mirrorTelegramPresentationToTerminal(client *Client, event turnPresentationEvent) {
	if client == nil || !interactiveTerminalUIEnabled() {
		return
	}
	switch event.Kind {
	case turnPresentationToolStarted, turnPresentationSubAgentStart, turnPresentationSubAgentEnd,
		turnPresentationRetry, turnPresentationFallback, turnPresentationUsage:
		mirrorTelegramSystemToTerminal(client, telegramPresentationText(event))
	case turnPresentationToolCompleted:
		if client.opts.CodeAssistWS {
			mirrorTelegramSystemToTerminal(client, telegramPresentationText(event))
		}
	}
}

func telegramSlashRefreshesTerminalTranscript(line string) bool {
	command, argument := telegramCommandParts(line)
	sub := ""
	if fields := strings.Fields(argument); len(fields) > 0 {
		sub = strings.ToLower(fields[0])
	}
	switch telegramCanonicalCommand(command) {
	case "/conversation":
		return sub == "new" || sub == "create" || sub == "use" || sub == "open" || sub == "switch"
	case "/workspace":
		return sub == "use" || sub == "switch"
	default:
		return false
	}
}

func telegramSlashMutatesContext(command, argument string) bool {
	args := strings.Fields(argument)
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}
	switch command {
	case "/conversation":
		return sub == "new" || sub == "create" || sub == "use" || sub == "open" || sub == "switch"
	case "/workspace":
		return sub == "use" || sub == "switch"
	case "/app":
		return sub == "use" || sub == "set" || sub == "clear" || sub == "unset"
	case "/approve", "/reject":
		return true
	default:
		return false
	}
}

func telegramCommandParts(text string) (string, string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	boundary := strings.IndexFunc(text, unicode.IsSpace)
	commandText := text
	argument := ""
	if boundary >= 0 {
		commandText = text[:boundary]
		argument = strings.TrimSpace(text[boundary:])
	}
	command := strings.ToLower(commandText)
	if at := strings.IndexByte(command, '@'); at >= 0 {
		command = command[:at]
	}
	return command, argument
}

func telegramSafeSlashLine(command, argument string) (string, bool) {
	return telegramRemoteSlashLine(command, argument)
}

func telegramPendingSummary(now time.Time) (telegramState, error) {
	state, err := readTelegramState()
	if err != nil {
		return telegramState{}, err
	}
	pruneTelegramPairings(&state, now)
	return state, nil
}

func telegramFormatExpiry(created time.Time, now time.Time) string {
	remaining := telegramPairingTTL - now.Sub(created)
	if remaining < 0 {
		remaining = 0
	}
	return strconv.FormatInt(int64(remaining.Round(time.Minute)/time.Minute), 10) + "m"
}
