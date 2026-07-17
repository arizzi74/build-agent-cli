package core

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
	telegramMaxConfigBytes      = 64 << 10
	telegramMaxTokenBytes       = 1024
	telegramMaxTokenLength      = 512
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
		Enabled:       false,
		DMPolicy:      telegramPolicyPairing,
		PollTimeout:   30,
	}
}

func telegramDir() string {
	return filepath.Join(stateDir(), "telegram")
}

func telegramConfigFile() string {
	return filepath.Join(telegramDir(), "config.json")
}

func defaultTelegramTokenFile() string {
	return filepath.Join(telegramDir(), "token")
}

func withTelegramLock(fn func() error) error {
	path := filepath.Join(telegramDir(), "lock")
	if err := rejectSymlinkPath(path); err != nil {
		return errors.New("unsafe Telegram lock path")
	}
	if err := os.MkdirAll(telegramDir(), 0o700); err != nil {
		return errors.New("could not create Telegram storage directory")
	}
	if err := os.Chmod(telegramDir(), 0o700); err != nil {
		return errors.New("could not secure Telegram storage directory")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return errors.New("could not open Telegram storage lock")
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return errors.New("could not secure Telegram storage lock")
	}
	if err := lockProfileFile(file); err != nil {
		return errors.New("could not lock Telegram storage")
	}
	defer unlockProfileFile(file)
	return fn()
}

func validateTelegramStorageDirectory() error {
	info, err := os.Lstat(telegramDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Telegram storage path must be a private directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return errors.New("Telegram storage directory permissions must be 0700")
	}
	return nil
}

func loadTelegramConfig() (telegramConfig, bool, error) {
	cfg := defaultTelegramConfig()
	path := telegramConfigFile()
	if err := validateTelegramStorageDirectory(); err != nil {
		return telegramConfig{}, false, err
	}
	if err := rejectSymlinkPath(path); err != nil {
		return telegramConfig{}, false, errors.New("unsafe Telegram configuration path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return telegramConfig{}, false, fmt.Errorf("inspect Telegram configuration: %w", err)
	}
	if !info.Mode().IsRegular() {
		return telegramConfig{}, true, errors.New("Telegram configuration must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return telegramConfig{}, true, errors.New("Telegram configuration permissions must be 0600")
	}
	if info.Size() > telegramMaxConfigBytes {
		return telegramConfig{}, true, errors.New("Telegram configuration exceeds the size limit")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return telegramConfig{}, true, fmt.Errorf("read Telegram configuration: %w", err)
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
	if cfg.TokenFile != "" && !filepath.IsAbs(cfg.TokenFile) {
		clean := filepath.Clean(cfg.TokenFile)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return errors.New("Telegram tokenFile must stay within the global Telegram directory")
		}
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

func saveTelegramConfig(cfg telegramConfig) error {
	if err := validateTelegramConfig(&cfg); err != nil {
		return err
	}
	return withTelegramLock(func() error {
		return privateAtomicWrite(telegramConfigFile(), cfg)
	})
}

func updateTelegramConfig(fn func(*telegramConfig) error) (telegramConfig, error) {
	var cfg telegramConfig
	err := withTelegramLock(func() error {
		var err error
		cfg, _, err = loadTelegramConfig()
		if err != nil {
			return err
		}
		if err := fn(&cfg); err != nil {
			return err
		}
		if err := validateTelegramConfig(&cfg); err != nil {
			return err
		}
		return privateAtomicWrite(telegramConfigFile(), cfg)
	})
	return cfg, err
}

func configuredTelegramTokenFile(cfg telegramConfig) string {
	path := strings.TrimSpace(cfg.TokenFile)
	if path == "" {
		return defaultTelegramTokenFile()
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(telegramDir(), path)
	}
	return filepath.Clean(path)
}

func resolveTelegramToken(cfg telegramConfig) (token, source string, err error) {
	return resolveTelegramTokenWithEnvironment(cfg, os.Getenv(telegramTokenEnv))
}

func resolveTelegramTokenWithEnvironment(cfg telegramConfig, environmentToken string) (token, source string, err error) {
	path := configuredTelegramTokenFile(cfg)
	if err := validateTelegramStorageDirectory(); err != nil {
		return "", "", err
	}
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
		if info.Size() > telegramMaxTokenBytes {
			return "", "", errors.New("Telegram token file exceeds the size limit")
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
	if len(token) > telegramMaxTokenLength || strings.Count(token, ":") != 1 {
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
