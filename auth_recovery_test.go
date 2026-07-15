package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTokenExpiredUsesExactFiveMinuteSafetyBuffer(t *testing.T) {
	oldNow := tokenNow
	defer func() { tokenNow = oldNow }()
	fixed := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	tokenNow = func() time.Time { return fixed }

	// expiresAt - five minutes == now is expired: callers must not begin a
	// request with a token that has only the reserved safety window remaining.
	atBoundary := TokenResponse{AccessToken: "access", IssuedAt: fixed.Add(-55 * time.Minute).UnixMilli(), ExpiresIn: 3600}
	if !tokenExpired(atBoundary) {
		t.Fatal("token at the five-minute safety boundary must be expired")
	}
	justOutside := TokenResponse{AccessToken: "access", IssuedAt: fixed.Add(-55*time.Minute + time.Millisecond).UnixMilli(), ExpiresIn: 3600}
	if tokenExpired(justOutside) {
		t.Fatal("token with one millisecond beyond the safety buffer must remain usable")
	}
}

func saveExpiredRecoveryCredentials(t *testing.T, profile, instance string) {
	t.Helper()
	if err := saveCachedToken(profile, TokenResponse{
		AccessToken:  "expired-access",
		RefreshToken: "refresh-secret",
		ExpiresIn:    1,
		IssuedAt:     time.Now().Add(-time.Hour).UnixMilli(),
		InstanceURL:  instance,
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveWebSession(profile, WebSession{InstanceURL: instance, AuthMode: authModeCookie, CookieHeader: "JSESSIONID=session-secret"}); err != nil {
		t.Fatal(err)
	}
}

func newSavedSessionValidationServer(t *testing.T, events *[]string, valid bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sn_build_agent/build_agent_api/providerConfig" {
			*events = append(*events, "validate-session")
			if !valid {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(`<form><input name="user_name"><input name="user_password"><button>login</button></form>`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"provider":"test"}}`))
			return
		}
		// Session timeout inspection is best effort and not part of this test.
		http.NotFound(w, r)
	}))
}

func TestNirvanaExpiredTokenRefreshesBeforeSavedSessionOAuth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var events []string
	server := newSavedSessionValidationServer(t, &events, true)
	defer server.Close()
	saveExpiredRecoveryCredentials(t, "p", server.URL)

	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "p", Nirvana: true}, httpClient: server.Client()}
	c.oauthRefresh = func(context.Context, OAuthConfig, string) (TokenResponse, error) {
		events = append(events, "refresh")
		return TokenResponse{}, errors.New("refresh rejected")
	}
	c.oauthSessionPKCE = func(_ context.Context, interactive bool) (TokenResponse, error) {
		if !interactive {
			t.Fatal("interactive recovery should permit session OAuth consent")
		}
		events = append(events, "session-oauth")
		return TokenResponse{AccessToken: "fresh-access", ExpiresIn: 3600}, nil
	}
	c.oauthRecoveryChoice = func(string, string, string, error) (string, error) {
		t.Fatal("credential recovery must not prompt after saved-session OAuth succeeds")
		return "", nil
	}

	tok, err := c.getNirvanaAccessToken(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "fresh-access" || strings.Join(events, ",") != "refresh,validate-session,session-oauth" {
		t.Fatalf("recovery ordering = %v, token=%q", events, tok.AccessToken)
	}
	cached, ok := loadCachedToken("p")
	if !ok || cached.AccessToken != "fresh-access" {
		t.Fatalf("fresh session-derived OAuth token was not saved: %#v", cached)
	}
}

func TestPrepareConnectionAuthenticationUsesSavedSessionBeforeRecoveryPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var events []string
	server := newSavedSessionValidationServer(t, &events, true)
	defer server.Close()
	saveExpiredRecoveryCredentials(t, "p", server.URL)

	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "p", Nirvana: true}, httpClient: server.Client()}
	c.oauthRefresh = func(context.Context, OAuthConfig, string) (TokenResponse, error) {
		events = append(events, "refresh")
		return TokenResponse{}, errors.New("refresh rejected")
	}
	c.oauthSessionPKCE = func(_ context.Context, interactive bool) (TokenResponse, error) {
		if !interactive {
			t.Fatal("interactive startup should permit saved-session OAuth consent")
		}
		events = append(events, "session-oauth")
		return TokenResponse{AccessToken: "session-access", ExpiresIn: 3600}, nil
	}
	c.oauthConfigure = func(context.Context) error {
		t.Fatal("expired-token startup prompted for credentials before saved-session recovery")
		return nil
	}
	c.oauthRecoveryChoice = func(string, string, string, error) (string, error) {
		t.Fatal("successful saved-session recovery must not show the recovery chooser")
		return "", nil
	}

	if err := c.PrepareConnectionAuthentication(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "refresh,validate-session,session-oauth" {
		t.Fatalf("startup recovery ordering = %v", events)
	}
	if !c.connectionAuthPrepared || c.oauthAccessToken != "session-access" {
		t.Fatalf("prepared=%v access=%q", c.connectionAuthPrepared, c.oauthAccessToken)
	}
}

func TestNirvanaExpiredTokenCancelPreservesSavedAuthAndRedactsFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var events []string
	server := newSavedSessionValidationServer(t, &events, false)
	defer server.Close()
	saveExpiredRecoveryCredentials(t, "p", server.URL)

	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "p", Nirvana: true}, httpClient: server.Client()}
	c.oauthRefresh = func(context.Context, OAuthConfig, string) (TokenResponse, error) {
		events = append(events, "refresh")
		return TokenResponse{}, errors.New("refresh token refresh-secret rejected")
	}
	c.oauthRecoveryChoice = func(_, _, _ string, cause error) (string, error) {
		events = append(events, "choice")
		if !strings.Contains(cause.Error(), "OAuth refresh") || !strings.Contains(cause.Error(), "saved-session recovery") {
			t.Fatalf("combined failure context missing: %v", cause)
		}
		if strings.Contains(cause.Error(), "refresh-secret") || strings.Contains(cause.Error(), "session-secret") {
			t.Fatalf("credential leaked in recovery error: %v", cause)
		}
		return credentialRecoveryCancel, nil
	}

	_, err := c.getNirvanaAccessToken(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if strings.Join(events, ",") != "refresh,validate-session,choice" {
		t.Fatalf("recovery ordering = %v", events)
	}
	if _, err := os.Stat(fileTokenPath("p")); err != nil {
		t.Fatalf("cancel deleted OAuth token: %v", err)
	}
	if _, err := os.Stat(sessionFile("p")); err != nil {
		t.Fatalf("cancel deleted saved session: %v", err)
	}
}

func TestNirvanaExpiredTokenRecoveryChoicesRemoveAndReauthenticateOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice string
	}{
		{name: "remove", choice: credentialRecoveryRemove},
		{name: "reauthenticate", choice: credentialRecoveryReauth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var events []string
			server := newSavedSessionValidationServer(t, &events, false)
			defer server.Close()
			saveExpiredRecoveryCredentials(t, "p", server.URL)

			c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "p", Nirvana: true}, httpClient: server.Client()}
			c.oauthRefresh = func(context.Context, OAuthConfig, string) (TokenResponse, error) {
				events = append(events, "refresh")
				return TokenResponse{}, errors.New("refresh failed")
			}
			c.oauthRecoveryChoice = func(string, string, string, error) (string, error) {
				events = append(events, "choice")
				return tc.choice, nil
			}
			if tc.choice == credentialRecoveryReauth {
				configureCalls := 0
				c.oauthConfigure = func(context.Context) error {
					configureCalls++
					events = append(events, "configure")
					return nil
				}
				c.oauthSessionPKCE = func(context.Context, bool) (TokenResponse, error) {
					events = append(events, "session-oauth")
					return TokenResponse{AccessToken: "reauth-access", ExpiresIn: 3600}, nil
				}
				tok, err := c.getNirvanaAccessToken(context.Background(), false)
				if err != nil {
					t.Fatal(err)
				}
				if tok.AccessToken != "reauth-access" || configureCalls != 1 {
					t.Fatalf("reauth token=%q configure calls=%d", tok.AccessToken, configureCalls)
				}
				if _, err := os.Stat(sessionFile("p")); !os.IsNotExist(err) {
					t.Fatalf("reauth should clear rejected saved session, stat err=%v", err)
				}
				return
			}

			_, err := c.getNirvanaAccessToken(context.Background(), false)
			if err == nil || !strings.Contains(err.Error(), "removed instance") {
				t.Fatalf("remove error = %v", err)
			}
			if _, err := os.Stat(profileDir("p")); !os.IsNotExist(err) {
				t.Fatalf("remove kept configured instance, stat err=%v", err)
			}
		})
	}
}

func TestNirvanaSilentRecoveryNeverPromptsOrStartsManualOAuth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var events []string
	server := newSavedSessionValidationServer(t, &events, true)
	defer server.Close()
	saveExpiredRecoveryCredentials(t, "p", server.URL)

	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "p", Nirvana: true}, httpClient: server.Client()}
	c.oauthRefresh = func(context.Context, OAuthConfig, string) (TokenResponse, error) {
		events = append(events, "refresh")
		return TokenResponse{}, errors.New("refresh failed")
	}
	c.oauthSessionPKCE = func(_ context.Context, interactive bool) (TokenResponse, error) {
		if interactive {
			t.Fatal("silent mode must not allow an interactive session OAuth flow")
		}
		events = append(events, "session-oauth")
		return TokenResponse{}, errors.New("consent required")
	}
	c.oauthRecoveryChoice = func(string, string, string, error) (string, error) {
		t.Fatal("silent mode must not prompt for credential recovery")
		return "", nil
	}
	c.oauthConfigure = func(context.Context) error {
		t.Fatal("silent mode must not start interactive auth")
		return nil
	}

	_, err := c.getNirvanaAccessToken(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "noninteractive") {
		t.Fatalf("silent recovery error = %v", err)
	}
	if strings.Join(events, ",") != "refresh,validate-session,session-oauth" {
		t.Fatalf("silent recovery ordering = %v", events)
	}
}
