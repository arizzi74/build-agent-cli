package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

func handleTelegramOfflineFlags(opts Options) (bool, error) {
	if !opts.TelegramStatus && strings.TrimSpace(opts.TelegramApprove) == "" {
		return false, nil
	}
	if !isValidProfile(opts.Profile) {
		return true, fmt.Errorf("invalid profile %q", opts.Profile)
	}
	if code := strings.TrimSpace(opts.TelegramApprove); code != "" {
		userID, err := approveTelegramPairing(opts.Profile, code, time.Now().UTC())
		if err != nil {
			return true, err
		}
		fmt.Printf("Telegram user %s approved for profile %s.\n", userID, opts.Profile)
	}
	if opts.TelegramStatus {
		text, err := telegramOperatorStatus(opts.Profile, time.Now().UTC())
		if err != nil {
			return true, err
		}
		fmt.Print(text)
	}
	return true, nil
}

func handleTelegramCommand(_ context.Context, c *Client, args []string) error {
	if len(args) == 0 || strings.EqualFold(args[0], "status") {
		text, err := telegramOperatorStatus(c.opts.Profile, time.Now().UTC())
		if err != nil {
			return err
		}
		slashCommandPrintf("%s", text)
		return nil
	}
	cfg, _, err := loadTelegramConfig(c.opts.Profile)
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
		userID, err := approveTelegramPairing(c.opts.Profile, args[1], time.Now().UTC())
		if err != nil {
			return err
		}
		slashCommandPrintf("Telegram user %s approved\n", userID)
		return nil
	case "enable":
		cfg.Enabled = true
		if cfg.DMPolicy == telegramPolicyDisabled {
			cfg.DMPolicy = telegramPolicyPairing
		}
		if err := saveTelegramConfig(c.opts.Profile, cfg); err != nil {
			return err
		}
		slashCommandPrintln("Telegram enabled; restart bacli to start polling")
		return nil
	case "disable":
		cfg.Enabled = false
		if err := saveTelegramConfig(c.opts.Profile, cfg); err != nil {
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
		cfg.DMPolicy = policy
		cfg.Enabled = policy != telegramPolicyDisabled
		if err := saveTelegramConfig(c.opts.Profile, cfg); err != nil {
			return err
		}
		slashCommandPrintf("Telegram DM policy set to %s; restart bacli to apply it\n", policy)
		return nil
	case "allow":
		return handleTelegramAllowCommand(c.opts.Profile, cfg, args[1:])
	case "revoke":
		if len(args) != 2 || !validTelegramNumericID(args[1]) {
			return fmt.Errorf("usage: /telegram revoke <numeric-user-id>")
		}
		return revokeTelegramUser(c.opts.Profile, cfg, args[1])
	default:
		return fmt.Errorf("unknown /telegram command %q; use /telegram help", args[0])
	}
}

func handleTelegramAllowCommand(profile string, cfg telegramConfig, args []string) error {
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
	switch strings.ToLower(args[0]) {
	case "add":
		found := false
		for _, existing := range cfg.AllowFrom {
			found = found || existing == id
		}
		if !found {
			cfg.AllowFrom = append(cfg.AllowFrom, id)
		}
	case "remove", "rm":
		kept := cfg.AllowFrom[:0]
		for _, existing := range cfg.AllowFrom {
			if existing != id {
				kept = append(kept, existing)
			}
		}
		cfg.AllowFrom = kept
	default:
		return fmt.Errorf("usage: /telegram allow add|remove <numeric-user-id>")
	}
	if err := saveTelegramConfig(profile, cfg); err != nil {
		return err
	}
	slashCommandPrintf("Telegram allowlist updated for user %s; running channels reload it before dispatch\n", id)
	return nil
}

func revokeTelegramUser(profile string, cfg telegramConfig, userID string) error {
	_, err := updateTelegramState(profile, func(state *telegramState) error {
		approved := state.Approved[:0]
		for _, id := range state.Approved {
			if id != userID {
				approved = append(approved, id)
			}
		}
		state.Approved = approved
		return nil
	})
	if err != nil {
		return err
	}
	allow := cfg.AllowFrom[:0]
	for _, id := range cfg.AllowFrom {
		if id != userID {
			allow = append(allow, id)
		}
	}
	cfg.AllowFrom = allow
	if err := saveTelegramConfig(profile, cfg); err != nil {
		return err
	}
	slashCommandPrintf("Telegram user %s revoked; running channels reauthorize before dispatch\n", userID)
	return nil
}

func telegramOperatorStatus(profile string, now time.Time) (string, error) {
	cfg, exists, err := loadTelegramConfig(profile)
	if err != nil {
		return "", err
	}
	_, tokenSource, tokenErr := resolveTelegramToken(profile, cfg)
	if tokenErr != nil {
		return "", tokenErr
	}
	if tokenSource == "not configured" && telegramEnvironmentTokenCaptured {
		tokenSource = "environment (captured securely at startup)"
	}
	state, err := telegramPendingSummary(profile, now)
	if err != nil {
		return "", err
	}
	approved := append([]string(nil), state.Approved...)
	sort.Strings(approved)
	var out strings.Builder
	fmt.Fprintf(&out, "Telegram profile: %s\n", profile)
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
		fmt.Fprintf(&out, "approve offline: bacli --profile %s --telegram-approve <code>\n", profile)
	}
	return out.String(), nil
}

func printTelegramOperatorHelp() {
	slashCommandPrintln("usage:")
	slashCommandPrintln("  /telegram status")
	slashCommandPrintln("  /telegram approve <8-character-code>")
	slashCommandPrintln("  /telegram enable | disable")
	slashCommandPrintln("  /telegram policy pairing|allowlist|disabled")
	slashCommandPrintln("  /telegram allow list")
	slashCommandPrintln("  /telegram allow add|remove <numeric-user-id>")
	slashCommandPrintln("  /telegram revoke <numeric-user-id>")
	slashCommandPrintln("offline approval: bacli --profile <profile> --telegram-approve <code>")
}
