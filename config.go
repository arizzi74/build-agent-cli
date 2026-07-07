package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultOAuthClientID    = "b77993a2359e472cad99679d1f124919"
	defaultOAuthRedirectURI = "/api/sn_build_agent/build_agent_api/oauth_redirect"
	defaultLLMProxyURL      = "https://llmproxy-prod-gateway"
	defaultCapabilityID     = "64920c03ff6d6210509bffffffffff25"
	stateDirName            = ".ba-cli"
)

type CLIConfig struct {
	InstanceURL      string `json:"instanceUrl"`
	WSURL            string `json:"wsUrl"`
	OAuthClientID    string `json:"-"`
	OAuthRedirectURI string `json:"-"`
	LLMProxyURL      string `json:"-"`
	CapabilityID     string `json:"-"`
}

func loadDotEnvFiles() {
	candidates := []string{
		filepath.Join(mustGetwd(), ".env"),
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".env"))
	}
	for _, path := range candidates {
		loadDotEnv(path)
	}
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

func resolveConfig(opts *Options) (CLIConfig, error) {
	cfg := CLIConfig{
		OAuthClientID:    envOrDefault("OAUTH_CLIENT_ID", defaultOAuthClientID),
		OAuthRedirectURI: envOrDefault("OAUTH_REDIRECT_URI", defaultOAuthRedirectURI),
		LLMProxyURL:      envOrDefault("LLM_PROXY_URL", defaultLLMProxyURL),
		CapabilityID:     envOrDefault("CAPABILITY_ID", defaultCapabilityID),
	}

	if opts.Profile == "" {
		opts.Profile = "default"
	}
	if !isValidProfile(opts.Profile) {
		return cfg, fmt.Errorf("invalid profile %q: use letters, numbers, dash or underscore", opts.Profile)
	}

	if opts.Setup {
		instanceURL, err := promptInstanceURL()
		if err != nil {
			return cfg, err
		}
		cfg.InstanceURL = cleanInstanceURL(instanceURL)
		if !opts.ProfileExplicit {
			opts.Profile = profileNameFromInstanceURL(cfg.InstanceURL)
		}
		cfg.WSURL = deriveWSURL(cfg.InstanceURL)
		if opts.WSURL != "" {
			cfg.WSURL = opts.WSURL
		}
		if err := saveProfileConfig(opts.Profile, cfg); err != nil {
			return cfg, err
		}
		_ = saveActiveInstanceProfile(opts.Profile)
		fmt.Fprintf(os.Stderr, "saved profile %q to %s\n", opts.Profile, profileConfigFile(opts.Profile))
		return cfg, nil
	}

	if opts.InstanceURL != "" {
		cfg.InstanceURL = cleanInstanceURL(opts.InstanceURL)
		if !opts.ProfileExplicit {
			opts.Profile = profileNameFromInstanceURL(cfg.InstanceURL)
		}
		cfg.WSURL = deriveWSURL(cfg.InstanceURL)
		if opts.WSURL != "" {
			cfg.WSURL = opts.WSURL
		}
		return cfg, nil
	}

	if saved, ok := loadProfileConfig(opts.Profile); ok {
		cfg.InstanceURL = cleanInstanceURL(saved.InstanceURL)
		cfg.WSURL = saved.WSURL
		if cfg.WSURL == "" {
			cfg.WSURL = deriveWSURL(cfg.InstanceURL)
		}
		if opts.WSURL != "" {
			cfg.WSURL = opts.WSURL
		}
		return cfg, nil
	}

	if instanceURL := firstEnv("INSTANCE_URL", "BA_INSTANCE_URL"); instanceURL != "" {
		cfg.InstanceURL = cleanInstanceURL(instanceURL)
		cfg.WSURL = firstEnv("WS_URL", "BA_WS_URL")
		if cfg.WSURL == "" {
			cfg.WSURL = deriveWSURL(cfg.InstanceURL)
		}
		if opts.WSURL != "" {
			cfg.WSURL = opts.WSURL
		}
		return cfg, nil
	}

	instanceURL, err := promptInstanceURL()
	if err != nil {
		return cfg, err
	}
	cfg.InstanceURL = cleanInstanceURL(instanceURL)
	if !opts.ProfileExplicit {
		opts.Profile = profileNameFromInstanceURL(cfg.InstanceURL)
	}
	cfg.WSURL = deriveWSURL(cfg.InstanceURL)
	if opts.WSURL != "" {
		cfg.WSURL = opts.WSURL
	}
	if err := saveProfileConfig(opts.Profile, cfg); err != nil {
		return cfg, err
	}
	fmt.Fprintf(os.Stderr, "saved profile %q to %s\n", opts.Profile, profileConfigFile(opts.Profile))
	return cfg, nil
}

func profileNameFromInstanceURL(raw string) string {
	host := raw
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	if u, err := url.Parse(host); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".service-now.com")
	var out strings.Builder
	lastDash := false
	for _, r := range host {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && out.Len() > 0 {
			out.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(out.String(), "-")
	if name == "" {
		return "default"
	}
	return name
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return ""
}

func cleanInstanceURL(raw string) string {
	v := strings.TrimSpace(raw)
	v = strings.TrimRight(v, "/")
	if v == "" {
		return v
	}
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		v = "https://" + v
	}
	return v
}

func deriveWSURL(instanceURL string) string {
	base := strings.TrimRight(instanceURL, "/")
	proto := "wss://"
	if strings.HasPrefix(base, "http://") {
		proto = "ws://"
		base = strings.TrimPrefix(base, "http://")
	} else {
		base = strings.TrimPrefix(base, "https://")
	}
	return proto + base + "/sncapps/code/assist/ba/nirvana/web-socket"
}

func deriveCodeAssistWSURL(instanceURL string) string {
	base := strings.TrimRight(instanceURL, "/")
	proto := "wss://"
	if strings.HasPrefix(base, "http://") {
		proto = "ws://"
		base = strings.TrimPrefix(base, "http://")
	} else {
		base = strings.TrimPrefix(base, "https://")
	}
	return proto + base + "/sncapps/code/assist/ba/web-socket"
}

func promptInstanceURL() (string, error) {
	for {
		line, err := promptLine("Instance URL (e.g. https://dev12345.service-now.com): ")
		if err != nil {
			return "", err
		}
		line = cleanInstanceURL(line)
		if line != "" {
			return line, nil
		}
		fmt.Fprintln(os.Stderr, "instance URL is required")
	}
}

func stateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".", stateDirName)
	}
	return filepath.Join(home, stateDirName)
}

func profileDir(profile string) string {
	return filepath.Join(stateDir(), "profiles", profile)
}

func profileConfigFile(profile string) string {
	return filepath.Join(profileDir(profile), "config.json")
}

func legacyConfigFile() string {
	return filepath.Join(stateDir(), "config.json")
}

func loadProfileConfig(profile string) (CLIConfig, bool) {
	paths := []string{profileConfigFile(profile)}
	if profile == "default" {
		paths = append(paths, legacyConfigFile())
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg CLIConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			continue
		}
		if cfg.InstanceURL != "" {
			return cfg, true
		}
	}
	return CLIConfig{}, false
}

func saveProfileConfig(profile string, cfg CLIConfig) error {
	if cfg.InstanceURL == "" {
		return errors.New("cannot save empty instance URL")
	}
	if err := os.MkdirAll(profileDir(profile), 0o700); err != nil {
		return err
	}
	toSave := CLIConfig{InstanceURL: cfg.InstanceURL, WSURL: cfg.WSURL}
	raw, err := json.MarshalIndent(toSave, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(profileConfigFile(profile), append(raw, '\n'), 0o600)
}

func isValidProfile(profile string) bool {
	if profile == "" {
		return false
	}
	for _, r := range profile {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func tokenStorageDescription(profile string) string {
	return filepath.Join(profileDir(profile), "oauth-token.json")
}
