package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type OAuthConfig struct {
	ClientID              string
	AuthorizationEndpoint string
	TokenEndpoint         string
	RedirectURI           string
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	IssuedAt     int64  `json:"issued_at,omitempty"`
	InstanceURL  string `json:"instance_url,omitempty"`
}

func oauthConfig(cfg CLIConfig) OAuthConfig {
	return OAuthConfig{
		ClientID:              cfg.OAuthClientID,
		AuthorizationEndpoint: cfg.InstanceURL + "/oauth_auth.do",
		TokenEndpoint:         cfg.InstanceURL + "/oauth_token.do",
		RedirectURI:           cfg.OAuthRedirectURI,
	}
}

func getAccessToken(ctx context.Context, cfg OAuthConfig, profile, instanceURL string, noOpen bool, silent bool) (TokenResponse, error) {
	if tok, ok := loadCachedToken(profile); ok {
		if tok.InstanceURL != "" && tok.InstanceURL != instanceURL {
			deleteCachedToken(profile)
		} else if !tokenExpired(tok) {
			return tok, nil
		} else if tok.RefreshToken != "" {
			refreshed, err := refreshAccessToken(ctx, cfg, tok.RefreshToken)
			if err == nil {
				refreshed.IssuedAt = time.Now().UnixMilli()
				refreshed.InstanceURL = instanceURL
				if refreshed.RefreshToken == "" {
					refreshed.RefreshToken = tok.RefreshToken
				}
				_ = saveCachedToken(profile, refreshed)
				return refreshed, nil
			}
			if !silent {
				action, promptErr := promptCredentialRecovery(profile, instanceURL, "OAuth token", err)
				if promptErr != nil {
					return TokenResponse{}, promptErr
				}
				switch action {
				case credentialRecoveryRemove:
					if deleteErr := deleteConfiguredInstance(profile); deleteErr != nil {
						return TokenResponse{}, deleteErr
					}
					return TokenResponse{}, fmt.Errorf("removed instance %q", profile)
				case credentialRecoveryCancel:
					return TokenResponse{}, fmt.Errorf("authentication canceled for instance %q", profile)
				}
			}
			deleteCachedToken(profile)
		}
	}

	if silent {
		return TokenResponse{}, errors.New("no valid cached OAuth token and interactive auth is disabled")
	}

	tok, err := runOAuthPKCEFlow(ctx, cfg, noOpen)
	if err != nil {
		return TokenResponse{}, err
	}
	tok.IssuedAt = time.Now().UnixMilli()
	tok.InstanceURL = instanceURL
	if err := saveCachedToken(profile, tok); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not cache token: %v\n", err)
	}
	return tok, nil
}

func tokenExpired(tok TokenResponse) bool {
	if tok.AccessToken == "" || tok.IssuedAt == 0 || tok.ExpiresIn == 0 {
		return true
	}
	const expirationBuffer = int64(15 * 60 * 1000)
	expiresAt := tok.IssuedAt + tok.ExpiresIn*1000
	return time.Now().UnixMilli() > expiresAt-expirationBuffer
}

func noRedirectTokenClient(hc *http.Client) *http.Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	copy := *hc
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

func tokenEndpointAllowed(cfg OAuthConfig) bool {
	u, err := url.Parse(cfg.AuthorizationEndpoint)
	if err != nil {
		return false
	}
	return sameOriginInstance(u.Scheme+"://"+u.Host, cfg.TokenEndpoint)
}

func refreshAccessToken(ctx context.Context, cfg OAuthConfig, refreshToken string) (TokenResponse, error) {
	if !tokenEndpointAllowed(cfg) {
		return TokenResponse{}, errors.New("invalid OAuth token endpoint")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", cfg.ClientID)
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := noRedirectTokenClient(nil).Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer res.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(res.Body, authResponseLimit))
	if readErr != nil {
		return TokenResponse{}, errors.New("could not read token refresh response")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return TokenResponse{}, fmt.Errorf("token refresh failed (%d)", res.StatusCode)
	}
	return decodeTokenResponse(body)
}

var oauthOpenBrowser = openBrowser

func runOAuthPKCEFlow(ctx context.Context, cfg OAuthConfig, noOpen bool) (TokenResponse, error) {
	codeVerifier := randomBase64URL(32)
	sum := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomHex(16)
	authURL, err := url.Parse(cfg.AuthorizationEndpoint)
	if err != nil {
		return TokenResponse{}, err
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", cfg.RedirectURI)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	authURL.RawQuery = q.Encode()

	if noOpen || oauthOpenBrowser(authURL.String()) != nil {
		// State and PKCE challenge are transient public protocol parameters. This URL
		// never includes cookies, passwords, client secrets, authorization codes, or tokens.
		fmt.Fprintln(os.Stderr, "Open this authorization URL in a browser:")
		fmt.Fprintln(os.Stderr, authURL.String())
	} else {
		fmt.Fprintln(os.Stderr, "Complete authorization in the opened browser, then paste the redirected URL or code.")
	}
	fmt.Fprintln(os.Stderr, "Paste either the full redirected URL or just the code parameter.")
	input, err := promptLine("Auth code: ")
	if err != nil {
		return TokenResponse{}, err
	}
	code, inputState, fullRedirect := extractAuthCodeState(input)
	if code == "" {
		return TokenResponse{}, errors.New("no authorization code found")
	}
	if fullRedirect {
		if inputState != state {
			return TokenResponse{}, errors.New("OAuth state did not match")
		}
	} else {
		fmt.Fprintln(os.Stderr, "warning: bare authorization code accepted for legacy compatibility; OAuth state could not be independently verified")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", cfg.ClientID)
	form.Set("code", code)
	form.Set("redirect_uri", cfg.RedirectURI)
	form.Set("code_verifier", codeVerifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if !tokenEndpointAllowed(cfg) {
		return TokenResponse{}, errors.New("invalid OAuth token endpoint")
	}
	res, err := noRedirectTokenClient(nil).Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer res.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(res.Body, authResponseLimit))
	if readErr != nil {
		return TokenResponse{}, errors.New("could not read token response")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return TokenResponse{}, fmt.Errorf("token exchange failed (%d)", res.StatusCode)
	}
	tok, err := decodeTokenResponse(body)
	if err != nil {
		return TokenResponse{}, err
	}
	fmt.Fprintln(os.Stderr, "authorization complete")
	return tok, nil
}

func decodeTokenResponse(body []byte) (TokenResponse, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw map[string]interface{}
	if err := dec.Decode(&raw); err != nil {
		return TokenResponse{}, err
	}
	tok := TokenResponse{}
	if v, _ := raw["access_token"].(string); v != "" {
		tok.AccessToken = v
	}
	if v, _ := raw["token_type"].(string); v != "" {
		tok.TokenType = v
	}
	if v, _ := raw["refresh_token"].(string); v != "" {
		tok.RefreshToken = v
	}
	if v, _ := raw["scope"].(string); v != "" {
		tok.Scope = v
	}
	if n, ok := raw["expires_in"].(json.Number); ok {
		tok.ExpiresIn, _ = n.Int64()
	} else if f, ok := raw["expires_in"].(float64); ok {
		tok.ExpiresIn = int64(f)
	}
	if tok.AccessToken == "" {
		return TokenResponse{}, errors.New("token response did not include access_token")
	}
	return tok, nil
}

func extractAuthCodeState(input string) (code, state string, fullRedirect bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", false
	}
	if u, err := url.Parse(input); err == nil && u.IsAbs() {
		return u.Query().Get("code"), u.Query().Get("state"), true
	}
	return input, "", false
}

func extractAuthCode(input string) string { code, _, _ := extractAuthCodeState(input); return code }

func randomBase64URL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func openBrowser(rawurl string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawurl)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawurl)
	default:
		cmd = exec.Command("xdg-open", rawurl)
	}
	return cmd.Start()
}

func loadCachedToken(profile string) (TokenResponse, bool) {
	raw, err := os.ReadFile(fileTokenPath(profile))
	if err != nil {
		return TokenResponse{}, false
	}
	var tok TokenResponse
	if err := json.Unmarshal(raw, &tok); err != nil {
		return TokenResponse{}, false
	}
	return tok, true
}

func saveCachedToken(profile string, tok TokenResponse) error {
	raw, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return writePrivateFile(fileTokenPath(profile), append(raw, '\n'))
}

func deleteCachedToken(profile string) {
	_ = os.Remove(fileTokenPath(profile))
}

func fileTokenPath(profile string) string {
	return filepath.Join(profileDir(profile), "oauth-token.json")
}

func trimBody(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 1000 {
		return s[:1000] + "…"
	}
	return s
}
