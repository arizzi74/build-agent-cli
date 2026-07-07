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

func refreshAccessToken(ctx context.Context, cfg OAuthConfig, refreshToken string) (TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", cfg.ClientID)
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return TokenResponse{}, fmt.Errorf("token refresh failed (%d): %s", res.StatusCode, trimBody(body))
	}
	return decodeTokenResponse(body)
}

func runOAuthPKCEFlow(ctx context.Context, cfg OAuthConfig, noOpen bool) (TokenResponse, error) {
	codeVerifier := randomBase64URL(32)
	sum := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomHex(16)
	clientSecret := randomHex(32)

	authURL, err := url.Parse(cfg.AuthorizationEndpoint)
	if err != nil {
		return TokenResponse{}, err
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("client_secret", clientSecret)
	q.Set("redirect_uri", cfg.RedirectURI)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	authURL.RawQuery = q.Encode()

	fmt.Fprintln(os.Stderr, "\nOpen this URL to authorize the CLI:")
	fmt.Fprintf(os.Stderr, "%s\n\n", authURL.String())
	if !noOpen {
		_ = openBrowser(authURL.String())
	}
	fmt.Fprintln(os.Stderr, "After approving, paste either the full redirected URL or just the code parameter.")
	input, err := promptLine("Auth code: ")
	if err != nil {
		return TokenResponse{}, err
	}
	code := extractAuthCode(input)
	if code == "" {
		return TokenResponse{}, errors.New("no authorization code found")
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

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return TokenResponse{}, fmt.Errorf("token exchange failed (%d): %s", res.StatusCode, trimBody(body))
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

func extractAuthCode(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	if u, err := url.Parse(input); err == nil && u.RawQuery != "" {
		if code := u.Query().Get("code"); code != "" {
			return code
		}
	}
	return input
}

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
	if err := os.MkdirAll(profileDir(profile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(fileTokenPath(profile), append(raw, '\n'), 0o600)
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
