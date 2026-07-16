package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	telegramConfigSchemaVersion = 1
	telegramPolicyPairing       = "pairing"
	telegramPolicyAllowlist     = "allowlist"
	telegramPolicyDisabled      = "disabled"
	telegramTokenEnv            = "BACLI_TELEGRAM_BOT_TOKEN"
)

var telegramEnvironmentTokenCaptured bool

type telegramConfig struct {
	SchemaVersion int      `json:"schemaVersion"`
	Enabled       bool     `json:"enabled"`
	DMPolicy      string   `json:"dmPolicy"`
	AllowFrom     []string `json:"allowFrom,omitempty"`
	TokenFile     string   `json:"tokenFile,omitempty"`
	PollTimeout   int      `json:"pollTimeoutSeconds,omitempty"`
}

func defaultTelegramConfig() telegramConfig {
	return telegramConfig{
		SchemaVersion: telegramConfigSchemaVersion,
		Enabled:       true,
		DMPolicy:      telegramPolicyPairing,
		PollTimeout:   30,
	}
}

func telegramConfigFile(profile string) string {
	return filepath.Join(profileDir(profile), "telegram.json")
}

func defaultTelegramTokenFile(profile string) string {
	return filepath.Join(profileDir(profile), "telegram.token")
}

func loadTelegramConfig(profile string) (telegramConfig, bool, error) {
	cfg := defaultTelegramConfig()
	path := telegramConfigFile(profile)
	if err := rejectSymlinkPath(path); err != nil {
		return telegramConfig{}, false, errors.New("unsafe Telegram configuration path")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return telegramConfig{}, false, fmt.Errorf("read Telegram configuration: %w", err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return telegramConfig{}, true, errors.New("Telegram configuration must be a regular file")
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return telegramConfig{}, true, errors.New("Telegram configuration permissions must be 0600")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return telegramConfig{}, true, errors.New("Telegram configuration is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return telegramConfig{}, true, errors.New("Telegram configuration has trailing data")
	}
	if err := validateTelegramConfig(&cfg); err != nil {
		return telegramConfig{}, true, err
	}
	return cfg, true, nil
}

func validateTelegramConfig(cfg *telegramConfig) error {
	if cfg.SchemaVersion != telegramConfigSchemaVersion {
		return fmt.Errorf("unsupported Telegram configuration schema %d", cfg.SchemaVersion)
	}
	policy, err := normalizeTelegramPolicy(cfg.DMPolicy)
	if err != nil {
		return err
	}
	cfg.DMPolicy = policy
	if cfg.PollTimeout == 0 {
		cfg.PollTimeout = 30
	}
	if cfg.PollTimeout < 5 || cfg.PollTimeout > 50 {
		return errors.New("Telegram pollTimeoutSeconds must be between 5 and 50")
	}
	seen := make(map[string]bool)
	allow := make([]string, 0, len(cfg.AllowFrom))
	for _, id := range cfg.AllowFrom {
		id = strings.TrimSpace(id)
		if !validTelegramNumericID(id) {
			return fmt.Errorf("invalid Telegram allowFrom user id %q; numeric IDs are required", id)
		}
		if !seen[id] {
			seen[id] = true
			allow = append(allow, id)
		}
	}
	sort.Strings(allow)
	cfg.AllowFrom = allow
	if strings.ContainsAny(cfg.TokenFile, "\r\n\x00") {
		return errors.New("Telegram tokenFile contains invalid characters")
	}
	return nil
}

func normalizeTelegramPolicy(policy string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", telegramPolicyPairing:
		return telegramPolicyPairing, nil
	case telegramPolicyAllowlist:
		return telegramPolicyAllowlist, nil
	case telegramPolicyDisabled:
		return telegramPolicyDisabled, nil
	default:
		return "", errors.New("Telegram dmPolicy must be pairing, allowlist, or disabled")
	}
}

func saveTelegramConfig(profile string, cfg telegramConfig) error {
	if err := validateTelegramConfig(&cfg); err != nil {
		return err
	}
	return withProfileLock(profile, "telegram.lock", func() error {
		return privateAtomicWrite(telegramConfigFile(profile), cfg)
	})
}

func configuredTelegramTokenFile(profile string, cfg telegramConfig) string {
	path := strings.TrimSpace(cfg.TokenFile)
	if path == "" {
		return defaultTelegramTokenFile(profile)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(profileDir(profile), path)
	}
	return filepath.Clean(path)
}

func resolveTelegramToken(profile string, cfg telegramConfig) (token, source string, err error) {
	return resolveTelegramTokenWithEnvironment(profile, cfg, os.Getenv(telegramTokenEnv))
}

func resolveTelegramTokenWithEnvironment(profile string, cfg telegramConfig, environmentToken string) (token, source string, err error) {
	path := configuredTelegramTokenFile(profile, cfg)
	if err := rejectSymlinkPath(path); err != nil {
		return "", "", errors.New("unsafe Telegram token path")
	}
	info, statErr := os.Lstat(path)
	if statErr == nil {
		if !info.Mode().IsRegular() {
			return "", "", errors.New("Telegram token file must be a regular file")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return "", "", fmt.Errorf("Telegram token file permissions are %04o; run chmod 600 on the configured token file", info.Mode().Perm())
		}
		if parent, parentErr := os.Stat(filepath.Dir(path)); parentErr != nil || !parent.IsDir() {
			return "", "", errors.New("could not inspect Telegram token directory")
		} else if runtime.GOOS != "windows" && parent.Mode().Perm()&0o022 != 0 {
			return "", "", errors.New("Telegram token directory must not be group- or world-writable")
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", "", errors.New("could not read Telegram token file")
		}
		token = strings.TrimSpace(string(raw))
		if err := validateTelegramToken(token); err != nil {
			return "", "", err
		}
		return token, "secure file", nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return "", "", errors.New("could not inspect Telegram token file")
	}
	token = strings.TrimSpace(environmentToken)
	if token == "" {
		return "", "not configured", nil
	}
	if err := validateTelegramToken(token); err != nil {
		return "", "", err
	}
	return token, "environment", nil
}

func takeTelegramEnvironmentToken() string {
	token := strings.TrimSpace(os.Getenv(telegramTokenEnv))
	_ = os.Unsetenv(telegramTokenEnv)
	telegramEnvironmentTokenCaptured = token != ""
	return token
}

func validateTelegramToken(token string) error {
	if token == "" {
		return errors.New("Telegram bot token is empty")
	}
	if len(token) > 512 || strings.Count(token, ":") != 1 {
		return errors.New("Telegram bot token has an invalid format")
	}
	parts := strings.SplitN(token, ":", 2)
	if !validTelegramTokenPart(parts[0], true) || !validTelegramTokenPart(parts[1], false) {
		return errors.New("Telegram bot token has an invalid format")
	}
	return nil
}

func validTelegramTokenPart(part string, numeric bool) bool {
	if part == "" {
		return false
	}
	for _, r := range part {
		if numeric {
			if r < '0' || r > '9' {
				return false
			}
			continue
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func telegramTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
