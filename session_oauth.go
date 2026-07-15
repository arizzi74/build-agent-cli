package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const authResponseLimit = 2 << 20

// authConfirm is injectable so security-sensitive prompts are deterministic in tests.
var authConfirm = func(prompt string) (bool, error) {
	answer, err := promptLine(prompt + " [y/N]: ")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes"), nil
}

func (c *Client) getNirvanaAccessToken(ctx context.Context, silent bool) (TokenResponse, error) {
	var refreshErr error
	tok, hadCachedToken := loadCachedToken(c.opts.Profile)
	if hadCachedToken {
		if tok.InstanceURL != "" && !sameInstance(tok.InstanceURL, c.cfg.InstanceURL) {
			// This profile's token belongs to another instance, so it cannot be
			// used for this connection. Leave it on disk until recovery is chosen.
			refreshErr = errors.New("cached OAuth token belongs to a different instance")
		} else if !tokenExpired(tok) {
			c.oauthAccessToken = tok.AccessToken
			return tok, nil
		} else if tok.RefreshToken != "" {
			refreshed, err := c.refreshNirvanaAccessToken(ctx, tok.RefreshToken)
			if err == nil {
				refreshed.IssuedAt, refreshed.InstanceURL = time.Now().UnixMilli(), c.cfg.InstanceURL
				if refreshed.RefreshToken == "" {
					refreshed.RefreshToken = tok.RefreshToken
				}
				_ = saveCachedToken(c.opts.Profile, refreshed)
				c.oauthAccessToken = refreshed.AccessToken
				return refreshed, nil
			}
			refreshErr = err
		} else {
			refreshErr = errors.New("cached OAuth token is expired and has no refresh token")
		}
	}
	if !hadCachedToken {
		if silent {
			tok, err := c.trySavedSessionOAuth(ctx, true)
			if err != nil {
				return TokenResponse{}, fmt.Errorf("noninteractive Nirvana authentication failed: saved-session recovery: %s", safeAuthRecoveryDetail(err))
			}
			return c.saveNirvanaAccessToken(tok)
		}
		return c.getNirvanaInteractiveAccessToken(ctx, silent)
	}

	// An expired token must first try refresh, then the matching saved browser
	// session. Neither credential is deleted merely because an earlier fallback
	// failed; users choose what to discard only after both recovery paths fail.
	tok, sessionErr := c.trySavedSessionOAuth(ctx, silent)
	if sessionErr == nil {
		return c.saveNirvanaAccessToken(tok)
	}
	recoveryErr := combinedNirvanaRecoveryError(refreshErr, sessionErr)
	if silent {
		return TokenResponse{}, fmt.Errorf("noninteractive Nirvana authentication failed: %w", recoveryErr)
	}

	mode, err := normalizeAuthMode(c.opts.AuthMode)
	if err != nil {
		return TokenResponse{}, err
	}
	if mode == authModeBasic {
		return TokenResponse{}, errors.New("--auth basic is only supported with --web-gateway; Nirvana requires a reusable ServiceNow web session (--auth form or --auth cookie)")
	}
	action, err := c.chooseNirvanaCredentialRecovery(recoveryErr)
	if err != nil {
		return TokenResponse{}, err
	}
	switch action {
	case credentialRecoveryRemove:
		if err := deleteConfiguredInstance(c.opts.Profile); err != nil {
			return TokenResponse{}, err
		}
		return TokenResponse{}, fmt.Errorf("removed instance %q", c.opts.Profile)
	case credentialRecoveryCancel:
		return TokenResponse{}, fmt.Errorf("authentication canceled for instance %q", c.opts.Profile)
	case credentialRecoveryReauth:
		// The cached token is known unusable. A saved session is only removed
		// after the user explicitly chooses reauthentication; configureGatewayAuth
		// then performs one normal interactive authentication attempt.
		deleteCachedToken(c.opts.Profile)
		if _, ok := loadMatchingWebSession(c.opts.Profile, c.cfg.InstanceURL); ok {
			if err := deleteWebSession(c.opts.Profile); err != nil {
				return TokenResponse{}, err
			}
		}
		c.clearWebSession()
		c.connectionAuthPrepared = false
		tok, err := c.getNirvanaInteractiveAccessToken(ctx, false)
		if err != nil {
			return TokenResponse{}, fmt.Errorf("reauthentication failed: %w", err)
		}
		return tok, nil
	default:
		return TokenResponse{}, errors.New("unknown credential recovery choice")
	}
}

// getNirvanaInteractiveAccessToken is deliberately a one-shot normal auth
// path. It never presents the credential-recovery chooser, so selecting
// reauthenticate cannot loop back into that chooser.
func (c *Client) getNirvanaInteractiveAccessToken(ctx context.Context, silent bool) (TokenResponse, error) {
	if silent {
		return TokenResponse{}, errors.New("no valid cached OAuth token or saved web session and interactive auth is disabled")
	}
	mode, err := normalizeAuthMode(c.opts.AuthMode)
	if err != nil {
		return TokenResponse{}, err
	}
	if mode == authModeBasic {
		return TokenResponse{}, errors.New("--auth basic is only supported with --web-gateway; Nirvana requires a reusable ServiceNow web session (--auth form or --auth cookie)")
	}
	if !c.connectionAuthPrepared {
		if err := c.configureNirvanaInteractiveAuth(ctx); err != nil {
			return TokenResponse{}, err
		}
		c.connectionAuthPrepared = true
	}
	if err := c.checkSessionTimeout(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not inspect web session timeout; continuing OAuth")
	}
	tok, err := c.runNirvanaSessionOAuth(ctx, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: web-session OAuth could not complete; using manual authorization flow")
		tok, err = runOAuthPKCEFlow(ctx, oauthConfig(c.cfg), c.opts.NoOpen)
	}
	if err != nil {
		return TokenResponse{}, err
	}
	return c.saveNirvanaAccessToken(tok)
}

func (c *Client) refreshNirvanaAccessToken(ctx context.Context, refreshToken string) (TokenResponse, error) {
	if c.oauthRefresh != nil {
		return c.oauthRefresh(ctx, oauthConfig(c.cfg), refreshToken)
	}
	return refreshAccessToken(ctx, oauthConfig(c.cfg), refreshToken)
}

func (c *Client) runNirvanaSessionOAuth(ctx context.Context, interactive bool) (TokenResponse, error) {
	if c.oauthSessionPKCE != nil {
		return c.oauthSessionPKCE(ctx, interactive)
	}
	return c.runSessionOAuthPKCEWithInteraction(ctx, interactive)
}

func (c *Client) configureNirvanaInteractiveAuth(ctx context.Context) error {
	if c.oauthConfigure != nil {
		return c.oauthConfigure(ctx)
	}
	return c.configureGatewayAuth(ctx)
}

func (c *Client) chooseNirvanaCredentialRecovery(cause error) (string, error) {
	if c.oauthRecoveryChoice != nil {
		return c.oauthRecoveryChoice(c.opts.Profile, c.cfg.InstanceURL, "OAuth and saved web session", cause)
	}
	if !credentialRecoveryInteractive() {
		return "", errors.New("Nirvana credential recovery requires an interactive terminal; no saved credentials were changed")
	}
	return promptCredentialRecovery(c.opts.Profile, c.cfg.InstanceURL, "OAuth and saved web session", cause)
}

func (c *Client) trySavedSessionOAuth(ctx context.Context, silent bool) (TokenResponse, error) {
	session, ok := loadMatchingWebSession(c.opts.Profile, c.cfg.InstanceURL)
	if !ok {
		return TokenResponse{}, errors.New("no matching saved web session")
	}
	if c.httpClient == nil {
		if err := c.initGatewayHTTPClient(); err != nil {
			return TokenResponse{}, err
		}
	}
	c.gatewayAuth = session.AuthMode
	if err := c.applyAndValidateWebSession(ctx, session); err != nil {
		return TokenResponse{}, fmt.Errorf("saved web session validation failed: %w", err)
	}
	c.connectionAuthPrepared = true
	tok, err := c.runNirvanaSessionOAuth(ctx, !silent)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("saved web-session OAuth failed: %w", err)
	}
	return tok, nil
}

func (c *Client) saveNirvanaAccessToken(tok TokenResponse) (TokenResponse, error) {
	tok.IssuedAt, tok.InstanceURL = time.Now().UnixMilli(), c.cfg.InstanceURL
	if err := saveCachedToken(c.opts.Profile, tok); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not cache OAuth token")
	}
	c.oauthAccessToken = tok.AccessToken
	return tok, nil
}

func combinedNirvanaRecoveryError(refreshErr, sessionErr error) error {
	refreshDetail := "no usable cached OAuth token"
	if refreshErr != nil {
		refreshDetail = safeAuthRecoveryDetail(refreshErr)
	}
	sessionDetail := "no matching saved web session"
	if sessionErr != nil {
		sessionDetail = safeAuthRecoveryDetail(sessionErr)
	}
	return fmt.Errorf("OAuth refresh: %s; saved-session recovery: %s", refreshDetail, sessionDetail)
}

func safeAuthRecoveryDetail(err error) string {
	if err == nil {
		return "not attempted"
	}
	if safeTelemetryLabel(err.Error()) == "redacted" {
		return "failed (sensitive details redacted)"
	}
	return err.Error()
}

func sameOriginInstance(instance, raw string) bool {
	a, errA := url.Parse(instance)
	b, errB := url.Parse(raw)
	return errA == nil && errB == nil && a.Scheme != "" && b.Scheme == a.Scheme && strings.EqualFold(a.Host, b.Host)
}

func (c *Client) sessionRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Response, error) {
	return c.sessionRequestContentType(ctx, method, rawURL, body, "")
}
func (c *Client) sessionRequestContentType(ctx context.Context, method, rawURL string, body io.Reader, contentType string) (*http.Response, error) {
	if !sameOriginInstance(c.cfg.InstanceURL, rawURL) {
		return nil, errors.New("refusing cross-origin session request")
	}
	if c.httpClient == nil {
		if err := c.initGatewayHTTPClient(); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	setBrowserishHeaders(req, c.cfg.InstanceURL)
	if c.sessionCookieHeader != "" {
		req.Header.Set("Cookie", c.sessionCookieHeader)
	}
	if c.userToken != "" {
		req.Header.Set("X-UserToken", c.userToken)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else if method == http.MethodPatch || method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	// Do not follow a redirect with credentials; inspect same-origin Location explicitly.
	hc := *c.httpClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return hc.Do(req)
}

func readLimited(res *http.Response) ([]byte, error) {
	if res == nil || res.Body == nil {
		return nil, errors.New("empty HTTP response")
	}
	return io.ReadAll(io.LimitReader(res.Body, authResponseLimit))
}

func (c *Client) runSessionOAuthPKCE(ctx context.Context) (TokenResponse, error) {
	return c.runSessionOAuthPKCEWithInteraction(ctx, true)
}

func (c *Client) runSessionOAuthPKCEWithInteraction(ctx context.Context, interactive bool) (TokenResponse, error) {
	cfg := oauthConfig(c.cfg)
	verifier, state := randomBase64URL(32), randomHex(16)
	u, err := url.Parse(cfg.AuthorizationEndpoint)
	if err != nil || !sameOriginInstance(c.cfg.InstanceURL, cfg.AuthorizationEndpoint) {
		return TokenResponse{}, errors.New("invalid OAuth authorization endpoint")
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", cfg.RedirectURI)
	q.Set("state", state)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	res, err := c.sessionRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return TokenResponse{}, err
	}
	body, readErr := readLimited(res)
	_ = res.Body.Close()
	if readErr != nil {
		return TokenResponse{}, errors.New("could not read OAuth authorization response")
	}
	if isUnauthenticatedSession(res.StatusCode, body) {
		return TokenResponse{}, errInvalidWebSession
	}
	code, returnedState := authorizationCode(res, body)
	if code != "" && returnedState != state {
		return TokenResponse{}, errors.New("OAuth state did not match")
	}
	if code == "" && explicitConsentPage(string(body)) {
		if !interactive || c.opts.AutoApprove {
			return TokenResponse{}, errors.New("OAuth consent requires interactive approval")
		}
		approved, err := authConfirm("Allow this ServiceNow session to authorize Build Agent CLI?")
		if err != nil {
			return TokenResponse{}, err
		}
		if !approved {
			return TokenResponse{}, errors.New("OAuth consent declined")
		}
		action := consentAction(string(body))
		postURL, err := sameOriginActionURL(c.cfg.InstanceURL, action)
		if err != nil {
			return TokenResponse{}, err
		}
		form := parseHiddenInputs(string(body))
		form.Set("sysverb_allow", "Allow")
		res, err = c.sessionRequestContentType(ctx, http.MethodPost, postURL, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
		if err != nil {
			return TokenResponse{}, err
		}
		body, readErr = readLimited(res)
		_ = res.Body.Close()
		if readErr != nil {
			return TokenResponse{}, errors.New("could not read OAuth consent response")
		}
		code, returnedState = authorizationCode(res, body)
		if code != "" && returnedState != state {
			return TokenResponse{}, errors.New("OAuth state did not match")
		}
	}
	if code == "" {
		return TokenResponse{}, errors.New("web session OAuth did not return an authorization code")
	}
	return exchangeAuthorizationCode(ctx, cfg, code, verifier, c.httpClient)
}

func pkceChallenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func sameOriginActionURL(base, action string) (string, error) {
	if strings.TrimSpace(action) == "" {
		return "", errors.New("OAuth consent action was not found")
	}
	u, err := url.Parse(action)
	if err != nil || strings.HasPrefix(action, "//") {
		return "", errors.New("refusing unsafe OAuth consent action")
	}
	b, _ := url.Parse(base)
	resolved := b.ResolveReference(u)
	if !sameOriginInstance(base, resolved.String()) {
		return "", errors.New("refusing cross-origin OAuth consent action")
	}
	return resolved.String(), nil
}
func consentAction(page string) string {
	if !explicitConsentPage(page) {
		return ""
	}
	if m := regexp.MustCompile(`(?is)<form[^>]*action=["']([^"']+)`).FindStringSubmatch(page); len(m) > 1 {
		return html.UnescapeString(m[1])
	}
	return "/oauth_auth.do"
}
func explicitConsentPage(page string) bool {
	p := strings.ToLower(page)
	return strings.Contains(p, "consent") && (strings.Contains(p, "authorize") || strings.Contains(p, "allow"))
}

func authorizationCode(res *http.Response, body []byte) (string, string) {
	if res != nil {
		if loc := res.Header.Get("Location"); loc != "" {
			if u, err := url.Parse(loc); err == nil {
				return u.Query().Get("code"), u.Query().Get("state")
			}
		}
		if res.Request != nil {
			if code := res.Request.URL.Query().Get("code"); code != "" {
				return code, res.Request.URL.Query().Get("state")
			}
		}
	}
	var j struct {
		Result struct {
			Code  string `json:"code"`
			State string `json:"state"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &j) == nil && j.Result.Code != "" {
		return j.Result.Code, j.Result.State
	}
	var x struct {
		Code  string `xml:"code"`
		State string `xml:"state"`
	}
	if xml.Unmarshal(body, &x) == nil && x.Code != "" {
		return x.Code, x.State
	}
	return "", ""
}

func exchangeAuthorizationCode(ctx context.Context, cfg OAuthConfig, code, verifier string, hc *http.Client) (TokenResponse, error) {
	if !tokenEndpointAllowed(cfg) {
		return TokenResponse{}, errors.New("invalid OAuth token endpoint")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {cfg.ClientID}, "code": {code}, "redirect_uri": {cfg.RedirectURI}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := noRedirectTokenClient(hc).Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer res.Body.Close()
	body, err := readLimited(res)
	if err != nil {
		return TokenResponse{}, errors.New("could not read token response")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return TokenResponse{}, fmt.Errorf("token exchange failed (%d)", res.StatusCode)
	}
	return decodeTokenResponse(body)
}

func (c *Client) checkSessionTimeout(ctx context.Context) error {
	rows, err := c.readSessionTimeoutProperty(ctx)
	if err != nil {
		return err
	}
	minutes, found := 30, len(rows) != 0
	if found {
		minutes, err = strconv.Atoi(strings.TrimSpace(stringify(rows[0]["value"])))
		if err != nil {
			return errors.New("session timeout property value is not minutes")
		}
	}
	if found && minutes == 1440 {
		return nil
	}
	if c.opts.AutoApprove {
		fmt.Fprintln(os.Stderr, "warning: glide.ui.session_timeout is not 1440; leaving it unchanged in noninteractive mode")
		return nil
	}
	fmt.Fprintf(os.Stderr, "glide.ui.session_timeout is %d minutes%s. ServiceNow guidance favors <=60 minutes (fallback 30); Antonio's requested 1440-minute/24-hour convenience setting is materially weaker.\n", minutes, map[bool]string{true: "", false: " (property absent; fallback)"}[found])
	approved, err := authConfirm("Explicitly set glide.ui.session_timeout to Antonio's 1440-minute convenience setting?")
	if err != nil {
		return err
	}
	if !approved {
		return nil
	}
	if found {
		id := stringify(rows[0]["sys_id"])
		if id == "" {
			return errors.New("session timeout property has no sys_id")
		}
		res, err := c.sessionRequest(ctx, http.MethodPatch, strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/now/table/sys_properties/"+url.PathEscape(id), bytes.NewReader([]byte(`{"value":"1440"}`)))
		if err != nil {
			return err
		}
		_, readErr := readLimited(res)
		_ = res.Body.Close()
		if readErr != nil || res.StatusCode < 200 || res.StatusCode >= 300 {
			return fmt.Errorf("session timeout update failed (%d)", res.StatusCode)
		}
	} else {
		payload := []byte(`{"name":"glide.ui.session_timeout","type":"integer","value":"1440","description":"Antonio-requested 24-hour convenience setting; materially weaker than ServiceNow's recommended short session timeout."}`)
		res, err := c.sessionRequest(ctx, http.MethodPost, strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/now/table/sys_properties", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		_, readErr := readLimited(res)
		_ = res.Body.Close()
		if readErr != nil || res.StatusCode < 200 || res.StatusCode >= 300 {
			return fmt.Errorf("session timeout create failed (%d)", res.StatusCode)
		}
	}
	return c.verifySessionTimeout(ctx)
}

func (c *Client) readSessionTimeoutProperty(ctx context.Context) ([]map[string]interface{}, error) {
	path := webStartupPropertyEndpoint("name=", []string{"glide.ui.session_timeout"})
	res, err := c.sessionRequest(ctx, http.MethodGet, strings.TrimRight(c.cfg.InstanceURL, "/")+path, nil)
	if err == nil {
		body, readErr := readLimited(res)
		_ = res.Body.Close()
		if readErr == nil && res.StatusCode >= 200 && res.StatusCode < 300 {
			if result, parseErr := startupResult(body); parseErr == nil {
				return startupPropertyRecords(result)
			}
		}
	}
	tableURL := strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/now/table/sys_properties?sysparm_query=name%3Dglide.ui.session_timeout&sysparm_fields=sys_id,name,value"
	res, tableErr := c.sessionRequest(ctx, http.MethodGet, tableURL, nil)
	if tableErr != nil {
		return nil, tableErr
	}
	body, readErr := readLimited(res)
	_ = res.Body.Close()
	if readErr != nil {
		return nil, errors.New("could not read session timeout property")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("property read failed (%d)", res.StatusCode)
	}
	var env struct {
		Result []map[string]interface{} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	return env.Result, nil
}
func (c *Client) verifySessionTimeout(ctx context.Context) error {
	rows, err := c.readSessionTimeoutProperty(ctx)
	if err != nil || len(rows) == 0 || strings.TrimSpace(stringify(rows[0]["value"])) != "1440" {
		return errors.New("session timeout update could not be verified")
	}
	return nil
}
