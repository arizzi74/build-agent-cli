package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenPostsDoNotFollowExternalRedirects(t *testing.T) {
	externalHits := 0
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { externalHits++ }))
	defer external.Close()
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, external.URL, http.StatusTemporaryRedirect)
	}))
	defer instance.Close()
	cfg := OAuthConfig{ClientID: "client", AuthorizationEndpoint: instance.URL + "/oauth_auth.do", TokenEndpoint: instance.URL + "/oauth_token.do", RedirectURI: instance.URL + "/callback"}
	if _, err := exchangeAuthorizationCode(context.Background(), cfg, "code-secret", "verifier-secret", instance.Client()); err == nil || externalHits != 0 {
		t.Fatalf("session exchange err=%v external=%d", err, externalHits)
	}
	if _, err := refreshAccessToken(context.Background(), cfg, "refresh-secret"); err == nil || externalHits != 0 {
		t.Fatalf("refresh err=%v external=%d", err, externalHits)
	}
}

func TestManualPKCERejectsRedirectAndStateMismatch(t *testing.T) {
	if code, state, full := extractAuthCodeState("https://example/callback?code=x&state=s"); code != "x" || state != "s" || !full {
		t.Fatalf("parser = %q %q %v", code, state, full)
	}
	if code, state, full := extractAuthCodeState("bare"); code != "bare" || state != "" || full {
		t.Fatalf("bare parser = %q %q %v", code, state, full)
	}
}

func TestPrivatePersistenceRepairsModesAndRejectsSymlinks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name, path string
		save       func() error
	}{
		{"session", sessionFile("p"), func() error {
			return saveWebSession("p", WebSession{InstanceURL: "https://example", CookieHeader: "J=x"})
		}},
		{"token", fileTokenPath("p"), func() error { return saveCachedToken("p", TokenResponse{AccessToken: "x"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(tc.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tc.path, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := tc.save(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(tc.path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("mode=%v err=%v", info.Mode(), err)
			}
			if err := os.Remove(tc.path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/tmp/not-a-secret", tc.path); err != nil {
				t.Fatal(err)
			}
			if err := tc.save(); err == nil || !strings.Contains(err.Error(), "unsafe") {
				t.Fatalf("symlink save err=%v", err)
			}
		})
	}

	t.Run("symlink ancestor", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		realProfiles := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".ba-cli"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realProfiles, filepath.Join(home, ".ba-cli", "profiles")); err != nil {
			t.Fatal(err)
		}
		if err := saveCachedToken("p", TokenResponse{AccessToken: "x"}); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("symlink ancestor save err=%v", err)
		}
		if _, err := os.Stat(filepath.Join(realProfiles, "p", "oauth-token.json")); !os.IsNotExist(err) {
			t.Fatalf("credential escaped through symlink ancestor: %v", err)
		}
	})
}

func TestNirvanaTokenCacheIsReusedWithoutSecondAuthRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatalf("unexpected auth request: %s", r.URL.Path) }))
	defer server.Close()
	if err := saveCachedToken("p", TokenResponse{AccessToken: "cached", ExpiresIn: 3600, IssuedAt: time.Now().UnixMilli(), InstanceURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "p", Nirvana: true}}
	first, err := c.getNirvanaAccessToken(context.Background(), false)
	if err != nil || first.AccessToken != "cached" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := c.getNirvanaAccessToken(context.Background(), false)
	if err != nil || second.AccessToken != "cached" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestTokenEndpointMustMatchInstance(t *testing.T) {
	cfg := OAuthConfig{AuthorizationEndpoint: "https://instance.example/oauth_auth.do", TokenEndpoint: "https://evil.example/oauth_token.do"}
	if tokenEndpointAllowed(cfg) {
		t.Fatal("cross-origin token endpoint allowed")
	}
	if _, err := refreshAccessToken(context.Background(), cfg, "r"); err == nil || !strings.Contains(err.Error(), "invalid OAuth token endpoint") {
		t.Fatalf("err=%v", err)
	}
}
