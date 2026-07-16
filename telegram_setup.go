package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/term"
)

type telegramSetupWizardIO struct {
	Interactive func() bool
	Line        func(string) (string, error)
	Secret      func(string) (string, error)
	Confirm     func(string, bool) (bool, error)
	VerifyBot   func(context.Context, string) (telegramUser, error)
	Output      io.Writer
}

var errTelegramSetupCanceled = errors.New("Telegram setup canceled")
var errTelegramSetupBack = errors.New("return to Telegram policy selection")

type telegramSetupSnapshot struct {
	Config       telegramConfig
	ConfigExists bool
	Token        string
	State        telegramState
	Revision     [sha256.Size]byte
}

func defaultTelegramSetupWizardIO(ctx context.Context) telegramSetupWizardIO {
	return telegramSetupWizardIO{
		Interactive: func() bool {
			return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(terminalStderrFD())
		},
		Line: func(prompt string) (string, error) {
			return promptTelegramWizardTTY(ctx, prompt, false, 4096)
		},
		Secret: func(prompt string) (string, error) {
			return promptTelegramWizardTTY(ctx, prompt, true, telegramMaxTokenLength)
		},
		Confirm: func(prompt string, defaultYes bool) (bool, error) {
			return confirmTelegramWizardTTY(ctx, prompt, defaultYes)
		},
		VerifyBot: func(ctx context.Context, token string) (telegramUser, error) {
			verifyCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			return newTelegramAPI(token).getMe(verifyCtx)
		},
		Output: os.Stderr,
	}
}

func runTelegramSetup(ctx context.Context, modal bool) error {
	wizard := defaultTelegramSetupWizardIO(ctx)
	if wizard.Interactive == nil || !wizard.Interactive() {
		return errors.New("Telegram setup requires an interactive terminal; run bacli --telegram-setup in a terminal. Tokens are never accepted on the command line")
	}
	if modal {
		enterAlternatePickerScreen()
		defer leaveAlternatePickerScreen()
	}
	err := runTelegramSetupWizard(ctx, wizard)
	if errors.Is(err, errTelegramSetupCanceled) {
		fmt.Fprintln(wizard.Output, "Telegram setup canceled; no configuration was changed.")
		return nil
	}
	if modal && err == nil {
		_, _ = wizard.Line("Press Enter to return to bacli: ")
	}
	return err
}

func promptTelegramWizardTTY(ctx context.Context, prompt string, secret bool, maxBytes int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	stdinState.Lock()
	defer stdinState.Unlock()

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer func() { _ = term.Restore(fd, oldState) }()

	terminalRenderMu.Lock()
	fmt.Fprint(os.Stderr, "\x1b[?2004h"+prompt)
	terminalRenderMu.Unlock()
	defer func() {
		terminalRenderMu.Lock()
		fmt.Fprint(os.Stderr, "\x1b[?2004l")
		terminalRenderMu.Unlock()
	}()

	value := make([]rune, 0, 64)
	redraw := func() {
		terminalRenderMu.Lock()
		defer terminalRenderMu.Unlock()
		fmt.Fprint(os.Stderr, "\r"+ansiEraseLine+prompt)
		if !secret {
			fmt.Fprint(os.Stderr, string(value))
		}
	}
	finish := func() {
		terminalRenderMu.Lock()
		fmt.Fprint(os.Stderr, "\r\n")
		terminalRenderMu.Unlock()
	}
	for {
		event, readErr := readTerminalInputEventUntil(fd, ctx.Done())
		if readErr != nil {
			finish()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", ctxErr
			}
			if errors.Is(readErr, errTerminalInputCanceled) {
				return "", errTelegramSetupCanceled
			}
			return "", readErr
		}
		switch event.Kind {
		case terminalInputEnter, terminalInputNewline:
			finish()
			return strings.TrimSpace(string(value)), nil
		case terminalInputCtrlC, terminalInputCtrlD, terminalInputEscape:
			finish()
			return "", errTelegramSetupCanceled
		case terminalInputBackspace:
			if len(value) > 0 {
				value = value[:len(value)-1]
				if !secret {
					redraw()
				}
			}
		case terminalInputCtrlU:
			value = value[:0]
			if !secret {
				redraw()
			}
		case terminalInputText:
			if unicode.IsControl(event.Rune) {
				continue
			}
			candidate := append(value, event.Rune)
			if len([]byte(string(candidate))) <= maxBytes {
				value = candidate
				if !secret {
					redraw()
				}
			}
		case terminalInputPaste:
			if event.Truncated {
				finish()
				return "", errors.New("pasted Telegram setup value exceeds the size limit")
			}
			pasted := event.Text
			if secret {
				pasted = strings.TrimSpace(pasted)
				if strings.ContainsAny(pasted, "\r\n\t") {
					finish()
					return "", errors.New("pasted Telegram token contains invalid control characters")
				}
			} else {
				pasted = strings.Map(func(r rune) rune {
					if unicode.IsSpace(r) {
						return ' '
					}
					if unicode.IsControl(r) {
						return -1
					}
					return r
				}, pasted)
			}
			candidate := append(value, []rune(pasted)...)
			if len([]byte(string(candidate))) > maxBytes {
				finish()
				return "", errors.New("pasted Telegram setup value exceeds the size limit")
			}
			value = candidate
			if !secret {
				redraw()
			}
		}
	}
}

func confirmTelegramWizardTTY(ctx context.Context, prompt string, defaultYes bool) (bool, error) {
	suffix := " [y/N]: "
	if defaultYes {
		suffix = " [Y/n]: "
	}
	for {
		answer, err := promptTelegramWizardTTY(ctx, prompt+suffix, false, 16)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "":
			return defaultYes, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(os.Stderr, "please answer y or n")
		}
	}
}

func runTelegramSetupWizard(ctx context.Context, wizard telegramSetupWizardIO) error {
	if wizard.Line == nil || wizard.Secret == nil || wizard.Confirm == nil || wizard.VerifyBot == nil {
		return errors.New("Telegram setup is unavailable")
	}
	out := wizard.Output
	if out == nil {
		out = io.Discard
	}
	wizard.Output = out
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Telegram setup canceled: %w", err)
	}

	existing, existingConfig, err := loadTelegramConfig()
	if err != nil {
		return fmt.Errorf("existing global Telegram configuration cannot be read safely: %w", err)
	}
	if _, _, err := resolveTelegramTokenWithEnvironment(existing, ""); err != nil {
		return fmt.Errorf("existing global Telegram token cannot be read safely: %w", err)
	}
	if _, err := readTelegramState(); err != nil {
		return fmt.Errorf("existing global Telegram authorization state cannot be read safely: %w", err)
	}

	fmt.Fprintln(out, "Telegram setup — global private command channel")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "This bot configuration, its allowlist, pairings, and update offset are shared by all bacli profiles.")
	fmt.Fprintln(out, "The ServiceNow --profile chosen when bacli starts remains the runtime context the bot controls.")
	fmt.Fprintln(out, "Use a dedicated bot: open Telegram's verified @BotFather, send /newbot, choose a name and a username ending in bot, then copy its token.")
	if existingConfig {
		fmt.Fprintln(out, "An existing global Telegram configuration was found. Nothing will change until you verify and confirm a replacement.")
	}
	if legacy := legacyTelegramProfiles(); len(legacy) > 0 {
		fmt.Fprintf(out, "Legacy profile-local Telegram files were found for: %s. They will not be read, merged, or deleted.\n", strings.Join(legacy, ", "))
	}
	fmt.Fprintln(out, "")
	ready, err := wizard.Line("Press Enter when the BotFather token is ready, or type q to cancel: ")
	if err != nil {
		return fmt.Errorf("Telegram setup canceled: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(ready), "q") || strings.EqualFold(strings.TrimSpace(ready), "quit") {
		fmt.Fprintln(out, "Telegram setup canceled; no configuration was changed.")
		return nil
	}

	var token string
	var bot telegramUser
	for {
		token, err = wizard.Secret("Paste bot token (input hidden): ")
		if err != nil {
			return fmt.Errorf("Telegram setup canceled: %w", err)
		}
		token = strings.TrimSpace(token)
		if err := validateTelegramToken(token); err != nil {
			fmt.Fprintln(out, "That value is not a valid Telegram bot token format.")
			retry, confirmErr := wizard.Confirm("Try another token?", true)
			if confirmErr != nil {
				return fmt.Errorf("Telegram setup canceled: %w", confirmErr)
			}
			if !retry {
				fmt.Fprintln(out, "Telegram setup canceled; no configuration was changed.")
				return nil
			}
			continue
		}
		bot, err = wizard.VerifyBot(ctx, token)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("Telegram setup canceled: %w", ctxErr)
		}
		if err == nil && bot.IsBot && validTelegramNumericID(telegramID(bot.ID)) && strings.HasPrefix(token, telegramID(bot.ID)+":") {
			break
		}
		fmt.Fprintln(out, telegramVerificationFailureMessage(err, bot))
		retry, confirmErr := wizard.Confirm("Try another token?", true)
		if confirmErr != nil {
			return fmt.Errorf("Telegram setup canceled: %w", confirmErr)
		}
		if !retry {
			fmt.Fprintln(out, "Telegram setup canceled; no configuration was changed.")
			return nil
		}
	}

	botID := telegramID(bot.ID)
	botLabel := telegramSetupBotLabel(bot)
	if strings.Contains(botLabel, token) {
		botLabel = "Telegram bot"
	}
	fmt.Fprintf(out, "Verified bot: %s (numeric bot ID %s)\n", botLabel, botID)

	policy := telegramPolicyPairing
	var allowFrom []string
	for {
		answer, lineErr := wizard.Line("Access policy [pairing/allowlist] (pairing): ")
		if lineErr != nil {
			return fmt.Errorf("Telegram setup canceled: %w", lineErr)
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "" || answer == telegramPolicyPairing || answer == "p" {
			policy = telegramPolicyPairing
			break
		}
		if answer == telegramPolicyAllowlist || answer == "a" {
			policy = telegramPolicyAllowlist
			allowFrom, err = promptTelegramAllowlist(wizard)
			if err != nil {
				if errors.Is(err, errTelegramSetupBack) {
					continue
				}
				if errors.Is(err, errTelegramSetupCanceled) {
					fmt.Fprintln(out, "Telegram setup canceled; no configuration was changed.")
					return nil
				}
				return err
			}
			break
		}
		if answer == "q" || answer == "quit" {
			fmt.Fprintln(out, "Telegram setup canceled; no configuration was changed.")
			return nil
		}
		fmt.Fprintln(out, "Choose pairing (recommended) or allowlist.")
	}

	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Review:")
	fmt.Fprintf(out, "  bot: %s (ID %s)\n", botLabel, botID)
	fmt.Fprintln(out, "  scope: global across all bacli profiles")
	fmt.Fprintf(out, "  private-DM policy: %s\n", policy)
	if policy == telegramPolicyAllowlist {
		fmt.Fprintf(out, "  allowed numeric user IDs: %s\n", strings.Join(allowFrom, ", "))
	}
	fmt.Fprintln(out, "  token storage: private global file (token value is never displayed)")
	fmt.Fprintln(out, "  startup effect: bacli removes any existing Telegram webhook; use a dedicated bot")
	fmt.Fprintln(out, "  routing: run only one poller; its selected --profile is the ServiceNow context the bot controls")
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Telegram setup canceled: %w", err)
	}
	confirmed, err := wizard.Confirm("Save this global Telegram configuration?", false)
	if err != nil {
		return fmt.Errorf("Telegram setup canceled: %w", err)
	}
	if !confirmed {
		fmt.Fprintln(out, "Telegram setup canceled; no configuration was changed.")
		return nil
	}

	cfg := defaultTelegramConfig()
	cfg.Enabled = true
	cfg.DMPolicy = policy
	cfg.AllowFrom = allowFrom
	cfg.TokenFile = ""
	if err := validateTelegramConfig(&cfg); err != nil {
		return err
	}
	snapshot, err := readTelegramSetupSnapshot()
	if err != nil {
		return err
	}
	fingerprint := telegramTokenFingerprint(token)
	locks, err := acquireTelegramSetupPollerLocks(snapshot.Token, snapshot.State.TokenFingerprint, token)
	if err != nil {
		return fmt.Errorf("Telegram setup cannot replace a bot while a bacli poller is running; stop it and rerun bacli --telegram-setup: %w", err)
	}
	defer releaseTelegramSetupPollerLocks(locks)
	if err := commitTelegramSetup(cfg, token, botID, fingerprint, snapshot.Revision); err != nil {
		return err
	}

	fmt.Fprintf(out, "Telegram configured globally for %s.\n", botLabel)
	fmt.Fprintln(out, "Start the bot bridge: bacli --telegram-only --profile <ServiceNow-profile>")
	if policy == telegramPolicyPairing {
		fmt.Fprintln(out, "In Telegram send /start, then approve its one-hour code locally: bacli --telegram-approve <code>")
	}
	fmt.Fprintln(out, "Inspect the global channel at any time: bacli --telegram-status")
	return nil
}

func readTelegramSetupSnapshot() (telegramSetupSnapshot, error) {
	var snapshot telegramSetupSnapshot
	err := withTelegramLock(func() error {
		var err error
		snapshot.Config, snapshot.ConfigExists, err = loadTelegramConfig()
		if err != nil {
			return fmt.Errorf("existing global Telegram configuration cannot be read safely: %w", err)
		}
		snapshot.Token, _, err = resolveTelegramTokenWithEnvironment(snapshot.Config, "")
		if err != nil {
			return fmt.Errorf("existing global Telegram token cannot be read safely: %w", err)
		}
		snapshot.State, err = readTelegramState()
		if err != nil {
			return fmt.Errorf("existing global Telegram authorization state cannot be read safely: %w", err)
		}
		snapshot.Revision, err = telegramSetupStorageRevision()
		return err
	})
	return snapshot, err
}

func promptTelegramAllowlist(wizard telegramSetupWizardIO) ([]string, error) {
	for {
		line, err := wizard.Line("Numeric Telegram user IDs (comma or space separated; type back to choose pairing first): ")
		if err != nil {
			return nil, fmt.Errorf("Telegram setup canceled: %w", err)
		}
		if strings.EqualFold(strings.TrimSpace(line), "q") || strings.EqualFold(strings.TrimSpace(line), "quit") {
			return nil, errTelegramSetupCanceled
		}
		if strings.EqualFold(strings.TrimSpace(line), "back") {
			return nil, errTelegramSetupBack
		}
		parts := strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t'
		})
		seen := make(map[string]bool)
		ids := make([]string, 0, len(parts))
		valid := len(parts) > 0
		for _, id := range parts {
			id = strings.TrimSpace(id)
			if !validTelegramNumericID(id) {
				valid = false
				break
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if valid && len(ids) > 0 {
			sort.Strings(ids)
			return ids, nil
		}
		fmt.Fprintln(wizard.Output, "Enter at least one positive numeric Telegram user ID.")
	}
}

func telegramVerificationFailureMessage(err error, bot telegramUser) string {
	if err == nil && (!bot.IsBot || !validTelegramNumericID(telegramID(bot.ID))) {
		return "Telegram verified the account, but it is not a valid bot identity. Nothing was saved."
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "Telegram bot verification timed out or was canceled. Nothing was saved."
	}
	var apiErr *telegramAPIError
	if errors.As(err, &apiErr) && (apiErr.Code == 401 || apiErr.Code == 403) {
		return "Telegram rejected that token. Copy the current token from BotFather and try again; nothing was saved."
	}
	return "Telegram could not verify that bot. Check the network and token, then try again; nothing was saved."
}

func telegramSetupBotLabel(bot telegramUser) string {
	username := safeTelegramBotUsername(bot.Username)
	if username != "" {
		return "@" + username
	}
	name := safeTelegramPairingLabel(strings.TrimSpace(bot.FirstName + " " + bot.LastName))
	if name == "" {
		return "Telegram bot"
	}
	return name
}

func safeTelegramBotUsername(username string) string {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if len(username) > 64 {
		return ""
	}
	for _, r := range username {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return ""
	}
	return username
}

func acquireTelegramSetupPollerLocks(existingToken, existingFingerprint, replacementToken string) ([]*os.File, error) {
	fingerprints := make(map[string]bool)
	if len(existingFingerprint) == sha256.Size*2 {
		fingerprints[existingFingerprint] = true
	}
	for _, token := range []string{existingToken, replacementToken} {
		if token != "" {
			fingerprints[telegramTokenFingerprint(token)] = true
		}
	}
	ordered := make([]string, 0, len(fingerprints))
	for fingerprint := range fingerprints {
		ordered = append(ordered, fingerprint)
	}
	sort.Strings(ordered)
	locks := make([]*os.File, 0, len(ordered))
	for _, fingerprint := range ordered {
		lock, err := acquireTelegramPollerLock(fingerprint)
		if err != nil {
			releaseTelegramSetupPollerLocks(locks)
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

func releaseTelegramSetupPollerLocks(locks []*os.File) {
	for i := len(locks) - 1; i >= 0; i-- {
		unlockProfileFile(locks[i])
		_ = locks[i].Close()
	}
}

func commitTelegramSetup(cfg telegramConfig, token, botID, fingerprint string, baseline [sha256.Size]byte) error {
	return withTelegramLock(func() error {
		currentRevision, err := telegramSetupStorageRevision()
		if err != nil {
			return err
		}
		if currentRevision != baseline {
			return errors.New("global Telegram configuration changed while the wizard was open; rerun bacli --telegram-setup")
		}
		state, err := readTelegramState()
		if err != nil {
			return err
		}
		if state.BotID != botID {
			state = defaultTelegramState()
			state.BotID = botID
			state.TokenFingerprint = fingerprint
		} else {
			state.TokenFingerprint = fingerprint
			state.Pending = nil
		}
		if err := validateTelegramState(&state); err != nil {
			return err
		}

		// Publish a disabled configuration first. If the machine loses power in
		// the small multi-file commit window, the next bacli process fails closed
		// instead of starting a partially replaced bot.
		staging := cfg
		staging.Enabled = false
		staging.DMPolicy = telegramPolicyDisabled
		if err := privateAtomicWrite(telegramConfigFile(), staging); err != nil {
			return privateFileError(telegramConfigFile())
		}
		if err := privateAtomicWrite(telegramStateFile(), state); err != nil {
			return privateFileError(telegramStateFile())
		}
		if err := writePrivateFile(defaultTelegramTokenFile(), []byte(token+"\n")); err != nil {
			return privateFileError(defaultTelegramTokenFile())
		}
		if err := privateAtomicWrite(telegramConfigFile(), cfg); err != nil {
			return privateFileError(telegramConfigFile())
		}

		saved, exists, err := loadTelegramConfig()
		if err != nil || !exists || !saved.Enabled || saved.DMPolicy != cfg.DMPolicy {
			return errors.New("Telegram configuration could not be verified after saving")
		}
		savedToken, _, err := resolveTelegramTokenWithEnvironment(saved, "")
		if err != nil || telegramTokenFingerprint(savedToken) != fingerprint {
			return errors.New("Telegram token could not be verified after saving")
		}
		savedState, err := readTelegramState()
		if err != nil || savedState.BotID != botID || savedState.TokenFingerprint != fingerprint {
			return errors.New("Telegram authorization state could not be verified after saving")
		}
		return nil
	})
}

func telegramSetupStorageRevision() ([sha256.Size]byte, error) {
	h := sha256.New()
	files := []struct {
		path string
		max  int64
	}{
		{telegramConfigFile(), telegramMaxConfigBytes},
		{defaultTelegramTokenFile(), telegramMaxTokenBytes},
		{telegramStateFile(), telegramMaxStateBytes},
	}
	for _, file := range files {
		if err := addTelegramRevisionFile(h, file.path, file.max); err != nil {
			return [sha256.Size]byte{}, err
		}
	}
	// A saved configuration may deliberately point at a private absolute token
	// file. Include the resolved value in the optimistic revision so changing
	// that external file while the wizard is open cannot bypass the conflict
	// check. The value is hashed only and is never rendered.
	cfg, _, err := loadTelegramConfig()
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	resolvedToken, _, err := resolveTelegramTokenWithEnvironment(cfg, "")
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	_, _ = h.Write([]byte("resolved-token\n"))
	_, _ = h.Write([]byte(resolvedToken))
	var revision [sha256.Size]byte
	copy(revision[:], h.Sum(nil))
	return revision, nil
}

func addTelegramRevisionFile(h hash.Hash, path string, max int64) error {
	if err := rejectSymlinkPath(path); err != nil {
		return errors.New("unsafe Telegram storage path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		_, _ = h.Write([]byte("missing:" + filepath.Base(path) + "\n"))
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > max {
		return errors.New("Telegram storage contains an unsafe file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("Telegram storage could not be inspected")
	}
	_, _ = h.Write([]byte("file:" + filepath.Base(path) + "\n"))
	_, _ = h.Write(raw)
	_, _ = h.Write([]byte("\n"))
	return nil
}

func legacyTelegramProfiles() []string {
	entries, err := os.ReadDir(filepath.Join(stateDir(), "profiles"))
	if err != nil {
		return nil
	}
	profiles := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !isValidProfile(entry.Name()) {
			continue
		}
		dir := profileDir(entry.Name())
		found := false
		for _, name := range []string{"telegram.json", "telegram.token", "telegram-state.json"} {
			if info, statErr := os.Lstat(filepath.Join(dir, name)); statErr == nil && info.Mode().IsRegular() {
				found = true
				break
			}
		}
		if found {
			profiles = append(profiles, entry.Name())
		}
	}
	sort.Strings(profiles)
	return profiles
}
