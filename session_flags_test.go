package main

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestLogoutDeletesSessionAndOAuthButPreservesProfileState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile := "zaiagents"
	if err := saveProfileConfig(profile, CLIConfig{InstanceURL: "https://zaiagents.service-now.com"}); err != nil {
		t.Fatal(err)
	}
	if err := saveWebSession(profile, WebSession{
		InstanceURL:  "https://zaiagents.service-now.com",
		AuthMode:     authModeForm,
		CookieHeader: "JSESSIONID=secret",
		UserToken:    "secret-token",
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveCachedToken(profile, TokenResponse{
		AccessToken: "oauth-secret", InstanceURL: "https://zaiagents.service-now.com",
	}); err != nil {
		t.Fatal(err)
	}

	handled, err := handleSessionFlags(Options{Profile: profile, Logout: true})
	if err != nil || !handled {
		t.Fatalf("logout handled=%v err=%v", handled, err)
	}
	for _, path := range []string{sessionFile(profile), fileTokenPath(profile)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("authentication file still exists: %s err=%v", path, err)
		}
	}
	if cfg, ok := loadProfileConfig(profile); !ok || cfg.InstanceURL != "https://zaiagents.service-now.com" {
		t.Fatalf("logout removed profile config: ok=%v cfg=%#v", ok, cfg)
	}
}

func TestLogoutIsIdempotentWithoutSavedAuthentication(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	handled, err := handleSessionFlags(Options{Profile: "zaiagents", Logout: true})
	if err != nil || !handled {
		t.Fatalf("empty logout handled=%v err=%v", handled, err)
	}
}
