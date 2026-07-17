package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

func handleTelegramOfflineFlags(ctx context.Context, opts Options) (bool, error) {
	if !opts.TelegramSetup && !opts.TelegramStatus && strings.TrimSpace(opts.TelegramApprove) == "" {
		return false, nil
	}
	if opts.TelegramSetup {
		if opts.TelegramStatus || strings.TrimSpace(opts.TelegramApprove) != "" {
			return true, fmt.Errorf("--telegram-setup cannot be combined with --telegram-status or --telegram-approve")
		}
		return true, runTelegramSetup(ctx, false)
	}
	if code := strings.TrimSpace(opts.TelegramApprove); code != "" {
		userID, err := approveTelegramPairing(code, time.Now().UTC())
		if err != nil {
			return true, err
		}
		fmt.Printf("Telegram user %s approved for the global channel.\n", userID)
	}
	if opts.TelegramStatus {
		text, err := telegramOperatorStatus(time.Now().UTC())
		if err != nil {
			return true, err
		}
		fmt.Print(text)
	}
	return true, nil
}

func handleTelegramCommand(ctx context.Context, _ *Client, args []string) error {
	if len(args) == 0 || strings.EqualFold(args[0], "status") {
		text, err := telegramOperatorStatus(time.Now().UTC())
		if err != nil {
			return err
		}
		slashCommandPrintf("%s", text)
		return nil
	}
	if strings.EqualFold(args[0], "setup") {
		if len(args) != 1 {
			return fmt.Errorf("usage: /telegram setup")
		}
		if _, remote := telegramCommandSourceFromContext(ctx); remote {
			return fmt.Errorf("Telegram bot setup requires the local hidden-input wizard; run bacli --telegram-setup on the host")
		}
		return runTelegramSetup(ctx, true)
	}
	cfg, _, err := loadTelegramConfig()
	if err != nil {
		return err
	}
	switch strings.ToLower(args[0]) {
	case "help":
		printTelegramOperatorHelp()
		return nil
	case "approve":
		if len(args) != 2 {
			return fmt.Errorf("usage: /telegram approve <8-character-code>")
		}
		userID, err := approveTelegramPairing(args[1], time.Now().UTC())
		if err != nil {
			return err
		}
		slashCommandPrintf("Telegram user %s approved\n", userID)
		return nil
	case "enable":
		if _, err := updateTelegramConfig(func(latest *telegramConfig) error {
			latest.Enabled = true
			if latest.DMPolicy == telegramPolicyDisabled {
				latest.DMPolicy = telegramPolicyPairing
			}
			return nil
		}); err != nil {
			return err
		}
		slashCommandPrintln("Telegram enabled; restart bacli to start polling")
		return nil
	case "disable":
		if _, err := updateTelegramConfig(func(latest *telegramConfig) error {
			latest.Enabled = false
			return nil
		}); err != nil {
			return err
		}
		slashCommandPrintln("Telegram authorization disabled immediately; restart bacli to stop the running poller")
		return nil
	case "policy":
		if len(args) != 2 {
			return fmt.Errorf("usage: /telegram policy pairing|allowlist|disabled")
		}
		policy, err := normalizeTelegramPolicy(args[1])
		if err != nil {
			return err
		}
		if _, err := updateTelegramConfig(func(latest *telegramConfig) error {
			latest.DMPolicy = policy
			latest.Enabled = policy != telegramPolicyDisabled
			return nil
		}); err != nil {
			return err
		}
		slashCommandPrintf("Telegram DM policy set to %s; restart bacli to apply it\n", policy)
		return nil
	case "allow":
		return handleTelegramAllowCommand(cfg, args[1:])
	case "revoke":
		if len(args) != 2 || !validTelegramNumericID(args[1]) {
			return fmt.Errorf("usage: /telegram revoke <numeric-user-id>")
		}
		return revokeTelegramUser(args[1])
	default:
		return fmt.Errorf("unknown /telegram command %q; use /telegram help", args[0])
	}
}

func handleTelegramAllowCommand(cfg telegramConfig, args []string) error {
	if len(args) == 0 || strings.EqualFold(args[0], "list") {
		if len(cfg.AllowFrom) == 0 {
			slashCommandPrintln("Telegram configured allowlist is empty")
			return nil
		}
		slashCommandPrintln("Telegram configured allowlist:")
		for _, id := range cfg.AllowFrom {
			slashCommandPrintln("  " + id)
		}
		return nil
	}
	if len(args) != 2 || !validTelegramNumericID(args[1]) {
		return fmt.Errorf("usage: /telegram allow add|remove <numeric-user-id>")
	}
	id := args[1]
	action := strings.ToLower(args[0])
	if action != "add" && action != "remove" && action != "rm" {
		return fmt.Errorf("usage: /telegram allow add|remove <numeric-user-id>")
	}
	if _, err := updateTelegramConfig(func(latest *telegramConfig) error {
		switch action {
		case "add":
			for _, existing := range latest.AllowFrom {
				if existing == id {
					return nil
				}
			}
			latest.AllowFrom = append(latest.AllowFrom, id)
		case "remove", "rm":
			kept := latest.AllowFrom[:0]
			for _, existing := range latest.AllowFrom {
				if existing != id {
					kept = append(kept, existing)
				}
			}
			latest.AllowFrom = kept
		}
		return nil
	}); err != nil {
		return err
	}
	slashCommandPrintf("Telegram allowlist updated for user %s; running channels reload it before dispatch\n", id)
	return nil
}

func revokeTelegramUser(userID string) error {
	err := withTelegramLock(func() error {
		cfg, _, err := loadTelegramConfig()
		if err != nil {
			return err
		}
		state, err := readTelegramState()
		if err != nil {
			return err
		}
		approved := state.Approved[:0]
		for _, id := range state.Approved {
			if id != userID {
				approved = append(approved, id)
			}
		}
		state.Approved = approved
		allow := cfg.AllowFrom[:0]
		for _, id := range cfg.AllowFrom {
			if id != userID {
				allow = append(allow, id)
			}
		}
		cfg.AllowFrom = allow
		if err := validateTelegramConfig(&cfg); err != nil {
			return err
		}
		if err := validateTelegramState(&state); err != nil {
			return err
		}
		staging := cfg
		staging.Enabled = false
		staging.DMPolicy = telegramPolicyDisabled
		if err := privateAtomicWrite(telegramConfigFile(), staging); err != nil {
			return privateFileError(telegramConfigFile())
		}
		if err := privateAtomicWrite(telegramStateFile(), state); err != nil {
			return privateFileError(telegramStateFile())
		}
		if err := privateAtomicWrite(telegramConfigFile(), cfg); err != nil {
			return privateFileError(telegramConfigFile())
		}
		return nil
	})
	if err != nil {
		return err
	}
	slashCommandPrintf("Telegram user %s revoked; running channels reauthorize before dispatch\n", userID)
	return nil
}

func telegramOperatorStatus(now time.Time) (string, error) {
	cfg, exists, err := loadTelegramConfig()
	if err != nil {
		return "", err
	}
	_, tokenSource, tokenErr := resolveTelegramToken(cfg)
	if tokenErr != nil {
		return "", tokenErr
	}
	if tokenSource == "not configured" && telegramEnvironmentTokenCaptured {
		tokenSource = "environment (captured securely at startup)"
	}
	state, err := telegramPendingSummary(now)
	if err != nil {
		return "", err
	}
	approved := append([]string(nil), state.Approved...)
	sort.Strings(approved)
	var out strings.Builder
	fmt.Fprintln(&out, "Telegram channel: global")
	fmt.Fprintf(&out, "storage: %s\n", telegramDir())
	fmt.Fprintf(&out, "configuration: %s\n", map[bool]string{true: "saved", false: "defaults"}[exists])
	fmt.Fprintf(&out, "enabled: %t\n", cfg.Enabled && cfg.DMPolicy != telegramPolicyDisabled)
	fmt.Fprintf(&out, "DM policy: %s\n", cfg.DMPolicy)
	fmt.Fprintf(&out, "token: %s\n", tokenSource)
	fmt.Fprintf(&out, "configured allowlist users: %d\n", len(cfg.AllowFrom))
	fmt.Fprintf(&out, "paired users: %d\n", len(approved))
	for _, id := range approved {
		fmt.Fprintf(&out, "  approved %s\n", id)
	}
	fmt.Fprintf(&out, "pending pairings: %d\n", len(state.Pending))
	for _, request := range state.Pending {
		label := strings.TrimSpace(request.Label)
		if label != "" {
			label = " " + label
		}
		fmt.Fprintf(&out, "  %s user=%s%s expires-in=%s\n", request.Code, request.UserID, label, telegramFormatExpiry(request.CreatedAt, now))
	}
	if len(state.Pending) > 0 {
		fmt.Fprintln(&out, "approve offline: bacli --telegram-approve <code>")
	}
	if legacy := legacyTelegramProfiles(); len(legacy) > 0 {
		fmt.Fprintf(&out, "legacy profile-local Telegram files (not used): %s\n", strings.Join(legacy, ", "))
		if !exists {
			fmt.Fprintln(&out, "configure the global channel: bacli --telegram-setup")
		}
	}
	return out.String(), nil
}

func printTelegramOperatorHelp() {
	slashCommandPrintln("usage:")
	slashCommandPrintln("  /telegram status")
	slashCommandPrintln("  /telegram setup")
	slashCommandPrintln("  /telegram approve <8-character-code>")
	slashCommandPrintln("  /telegram enable | disable")
	slashCommandPrintln("  /telegram policy pairing|allowlist|disabled")
	slashCommandPrintln("  /telegram allow list")
	slashCommandPrintln("  /telegram allow add|remove <numeric-user-id>")
	slashCommandPrintln("  /telegram revoke <numeric-user-id>")
	slashCommandPrintln("offline setup: bacli --telegram-setup")
	slashCommandPrintln("offline approval: bacli --telegram-approve <code>")
}
