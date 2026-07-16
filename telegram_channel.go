package main

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
	chatID string
	userID string
	text   string
}

type telegramService struct {
	profile string
	cfg     telegramConfig
	api     *telegramAPI
	clients *activeClientRef
	client  *Client
	timeout time.Duration

	ctx      context.Context
	cancel   context.CancelFunc
	commands chan telegramQueuedCommand
	fatalErr chan error
	wg       sync.WaitGroup
	lockFile *os.File
}

func maybeStartTelegramService(parent context.Context, profile string, clients *activeClientRef, timeout time.Duration, environmentToken string) (*telegramService, error) {
	if !isValidProfile(profile) {
		return nil, fmt.Errorf("invalid profile %q", profile)
	}
	pinnedClient := clients.Get()
	if pinnedClient == nil || pinnedClient.opts.Profile != profile {
		return nil, errors.New("Telegram channel could not pin the active profile client")
	}
	cfg, _, err := loadTelegramConfig(profile)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled || cfg.DMPolicy == telegramPolicyDisabled {
		return nil, nil
	}
	token, _, err := resolveTelegramTokenWithEnvironment(profile, cfg, environmentToken)
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
	state, err := bindTelegramState(profile, botID, fingerprint)
	if err != nil {
		return nil, err
	}
	if err := api.deleteWebhook(startupCtx); err != nil {
		return nil, fmt.Errorf("Telegram webhook cleanup failed: %w", err)
	}
	if err := api.setMyCommands(startupCtx); err != nil {
		return nil, fmt.Errorf("Telegram command registration failed: %w", err)
	}

	ctx, stop := context.WithCancel(parent)
	service := &telegramService{
		profile: profile, cfg: cfg, api: api, clients: clients, client: pinnedClient, timeout: timeout,
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
	if message == "" {
		return
	}
	if client := s.clients.Get(); client != nil {
		client.printRuntimeError(message)
	}
}

func (s *telegramService) pinnedClient() (*Client, bool) {
	current := s.clients.Get()
	return current, current != nil && current == s.client && current.opts.Profile == s.profile
}

func (s *telegramService) authorization(userID string) (telegramConfig, bool, error) {
	cfg, _, err := loadTelegramConfig(s.profile)
	if err != nil {
		return telegramConfig{}, false, err
	}
	if !cfg.Enabled || cfg.DMPolicy == telegramPolicyDisabled {
		return cfg, false, nil
	}
	state, err := readTelegramState(s.profile)
	if err != nil {
		return cfg, false, err
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
			if err := recordTelegramUpdate(s.profile, update.UpdateID); err != nil {
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
		s.sendAsync(chatID, "Your numeric Telegram user ID is "+userID+".")
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
		client, pinned := s.pinnedClient()
		if !pinned {
			s.sendAsync(chatID, "The bacli instance changed. Restart bacli before using /cancel.")
		} else if client.cancelActiveTurn() {
			s.sendAsync(chatID, "Cancellation requested.")
		} else {
			s.sendAsync(chatID, "No Build Agent turn is currently running.")
		}
		return
	}
	if text == "" || !strings.HasPrefix(text, "/") {
		s.sendAsync(chatID, "This bot accepts commands only. Use /ask <prompt> or /help.")
		return
	}
	if !s.enqueue(telegramQueuedCommand{chatID: chatID, userID: userID, text: text}) && s.ctx.Err() == nil {
		s.sendAsync(chatID, "The bacli command queue is full. Use /cancel for the active turn or try again later.")
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
	code, created, err := createTelegramPairing(s.profile, userID, chatID, label, time.Now().UTC())
	if err != nil {
		s.sendAsync(chatID, "Pairing could not be created: "+err.Error())
		return
	}
	if !created {
		s.sendAsync(chatID, "A pairing request is already pending. Ask the bacli operator to inspect `bacli --telegram-status`.")
		return
	}
	messageText := fmt.Sprintf("Pairing required. Your one-hour code is %s. Ask the bacli operator to run locally: bacli --profile %s --telegram-approve %s", code, s.profile, code)
	s.sendAsync(chatID, messageText)
}

func (s *telegramService) sendAsync(chatID, text string) {
	ctx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
	defer cancel()
	if err := s.api.sendText(ctx, chatID, text); err != nil && s.ctx.Err() == nil {
		s.warn("Telegram send failed: " + err.Error())
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
			if _, pinned := s.pinnedClient(); !pinned {
				s.sendAsync(command.chatID, "The bacli instance changed. Restart bacli to bind Telegram to the new client securely.")
				continue
			}
			_, authorized, err := s.authorization(command.userID)
			if err != nil {
				s.warn("Telegram queued command denied: authorization state could not be read safely")
				continue
			}
			if !authorized {
				s.sendAsync(command.chatID, "This Telegram user is no longer authorized.")
				continue
			}
			response := s.dispatchCommand(command)
			s.sendAsync(command.chatID, response)
		}
	}
}

func (s *telegramService) dispatchCommand(in telegramQueuedCommand) string {
	command, argument := telegramCommandParts(in.text)
	switch command {
	case "/start", "/help":
		return telegramRemoteHelp()
	case "/whoami":
		return "Your numeric Telegram user ID is " + in.userID + "."
	case "/ask":
		if strings.TrimSpace(argument) == "" {
			return "Usage: /ask <prompt>"
		}
		bacliActionMu.Lock()
		defer bacliActionMu.Unlock()
		client, pinned := s.pinnedClient()
		if !pinned {
			return "The bacli instance changed. Restart bacli to bind Telegram to the new client securely."
		}
		response, err := runPromptForResponseLocked(s.ctx, client, argument, s.timeout)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return "Build Agent turn cancelled."
			}
			return "Build Agent turn failed: " + err.Error()
		}
		if strings.TrimSpace(response) == "" {
			return "Build Agent completed without a text response."
		}
		return response
	case "/cancel":
		return "No Build Agent turn is currently running."
	}
	line, ok := telegramSafeSlashLine(command, argument)
	if !ok {
		return "That command is not available through Telegram. Use /help."
	}
	if interactiveTerminalUIEnabled() && telegramSlashMutatesContext(command, argument) {
		return "That context-changing command is local-only while the interactive terminal UI is running. Use it in bacli or restart with --telegram-only."
	}
	return s.executeSlash(line)
}

func (s *telegramService) executeSlash(line string) string {
	bacliActionMu.Lock()
	defer bacliActionMu.Unlock()
	client, pinned := s.pinnedClient()
	if !pinned {
		return "The bacli instance changed. Restart bacli to bind Telegram to the new client securely."
	}
	var output strings.Builder
	handled, err := withSlashCommandOutput(&output, func() (bool, error) {
		return handleSlashCommand(s.ctx, client, line)
	})
	if err != nil {
		if output.Len() > 0 {
			return strings.TrimSpace(output.String()) + "\nError: " + err.Error()
		}
		return "Command failed: " + err.Error()
	}
	if !handled {
		return "Command is not available through Telegram."
	}
	return strings.TrimSpace(output.String())
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
	args := strings.Fields(argument)
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}
	safe := false
	switch command {
	case "/status", "/turn", "/approvals":
		safe = argument == "" || argument == "--json"
	case "/mcp":
		safe = sub == "list" && len(args) == 1
	case "/conversation":
		safe = sub == "current" || sub == "show" || sub == "new" || sub == "create" || ((sub == "use" || sub == "open" || sub == "switch") && len(args) == 2)
	case "/workspace":
		safe = sub == "current" || sub == "show" || sub == "list" || sub == "ls" || ((sub == "use" || sub == "switch") && len(args) >= 2)
	case "/app":
		safe = sub == "current" || sub == "show" || sub == "clear" || sub == "unset" || ((sub == "use" || sub == "set") && len(args) >= 2)
	case "/sync":
		safe = sub == "status" && len(args) == 1
	case "/project":
		safe = (sub == "current" || sub == "list") && len(args) == 1
	case "/approve", "/reject":
		safe = len(args) == 1
	}
	if !safe {
		return "", false
	}
	line := command
	if argument != "" {
		line += " " + argument
	}
	return line, true
}

func telegramRemoteHelp() string {
	return strings.TrimSpace(`bacli Telegram commands:
/ask <prompt> - run one Build Agent turn
/cancel - cancel the active turn immediately
/status, /turn - inspect runtime state
/conversation current|new|use <id> (changes require --telegram-only)
/workspace current|list|use <name> (changes require --telegram-only)
/app current|use <scopeId> [name]|clear (changes require --telegram-only)
/sync status
/project current|list
/mcp list
/approvals, /approve <id>, /reject <id> (decisions require --telegram-only)
/whoami - show your numeric Telegram user ID

Only private command messages are accepted. Interactive pickers, instance switching, attachment paths, debug/export/support, and destructive sync operations stay local. After a local /instance switch, restart bacli so Telegram can bind to the new profile/client.`)
}

func telegramPendingSummary(profile string, now time.Time) (telegramState, error) {
	state, err := readTelegramState(profile)
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
