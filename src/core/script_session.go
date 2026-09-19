package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// Script processors require a browser session independently of OAuth. Probe the
// exact credentials used by the eventual POST: a jar or bearer must not make an
// expired saved cookie look valid. This probe never executes a script.
func (c *Client) validateInstanceScriptSession(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.New("Script session check canceled; no script was submitted")
	}
	base, err := url.Parse(c.cfg.InstanceURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return errors.New("Invalid instance URL for script session validation")
	}
	if !c.scriptExecutionCredentialsAvailable() {
		return fmt.Errorf("Script execution requires a current browser session and CSRF token; no script was submitted: %w", errInvalidWebSession)
	}
	instance, cookie, csrf := c.cfg.InstanceURL, c.sessionCookieHeader, c.userToken
	hc := http.Client{}
	if c.httpClient != nil {
		hc = *c.httpClient
	}
	hc.Jar = nil
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, path := range []string{"/api/sn_build_agent/build_agent_api/providerConfig", "/api/sn_ba_core/agent_config_api/config"} {
		endpoint := *base
		endpoint.Path, endpoint.RawPath = path, ""
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return errors.New("Could not prepare script session check; no script was submitted")
		}
		setBrowserishHeadersForBase(req.Header, instance)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Cookie", cookie)
		req.Header.Set("X-UserToken", csrf)
		res, err := hc.Do(req)
		if err != nil {
			return errors.New("Could not verify the script browser session; no script was submitted")
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, authResponseLimit+1))
		_ = res.Body.Close()
		if readErr != nil || len(body) > authResponseLimit {
			return errors.New("Invalid script session check response; no script was submitted")
		}
		if res.StatusCode == http.StatusForbidden {
			return fmt.Errorf("Script browser session or instance permissions were rejected (HTTP 403); reconnect and verify access; no script was submitted: %w", errInvalidWebSession)
		}
		if isUnauthenticatedSession(res.StatusCode, body) {
			return fmt.Errorf("Script browser session expired or was rejected; reauthenticate this profile; no script was submitted: %w", errInvalidWebSession)
		}
		if res.StatusCode == http.StatusNotFound {
			continue
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return fmt.Errorf("Script session check returned HTTP %d; no script was submitted", res.StatusCode)
		}
		var envelope map[string]interface{}
		if json.Unmarshal(body, &envelope) != nil {
			return errors.New("Malformed script session check response; no script was submitted")
		}
		result, ok := envelope["result"].(map[string]interface{})
		if !ok || result == nil || envelope["error"] != nil || result["error"] != nil || result["success"] == false || strings.EqualFold(stringify(envelope["status"]), "failure") || strings.EqualFold(stringify(result["status"]), "failure") {
			return errors.New("Unrecognized script session check response; no script was submitted")
		}
		if ctx.Err() != nil || instance != c.cfg.InstanceURL || cookie != c.sessionCookieHeader || csrf != c.userToken {
			return errors.New("Script session changed or was canceled during validation; no script was submitted")
		}
		return nil
	}
	return errors.New("Instance has no supported browser-session validation endpoint; no script was submitted")
}

// prepareInstanceScriptSession is best effort for OAuth chat, but not for script
// execution. A failed browser check disables script advertisement independently
// of the bearer token's health. Authentication prompts are never implicit in a
// noninteractive connection.
func (c *Client) prepareInstanceScriptSession(ctx context.Context, interactive bool) {
	if err := c.ensureInstanceScriptSession(ctx, interactive); err != nil {
		c.scriptSessionError = err.Error()
		c.reportAuthenticationNotice("Run Script unavailable: "+err.Error(), "warning: Run Script unavailable: "+err.Error(), true)
	}
}

func (c *Client) ensureInstanceScriptSession(ctx context.Context, interactive bool) error {
	if c.sessionCookieHeader == "" && c.gatewayAuth != authModeBasic {
		if session, ok := loadMatchingWebSession(c.opts.Profile, c.cfg.InstanceURL); ok {
			c.applyWebSession(session)
		}
	}
	err := c.validateInstanceScriptSession(ctx)
	if err == nil {
		c.scriptSessionError = ""
		return nil
	}
	if !errors.Is(err, errInvalidWebSession) || ctx.Err() != nil {
		return err
	}
	// Renew only a definitely missing/rejected session, never on an uncertain
	// network/processor failure. This is always before any script POST.
	if err := c.renewInstanceScriptSession(ctx, interactive); err != nil {
		return err
	}
	c.scriptSessionError = ""
	return nil
}

func (c *Client) renewInstanceScriptSession(ctx context.Context, interactive bool) error {
	instance := c.cfg.InstanceURL
	mode := c.gatewayAuth
	if mode == "" {
		mode, _ = normalizeAuthMode(c.opts.AuthMode)
	}
	if mode != authModeForm && mode != authModeCookie {
		return errors.New("Run Script requires form or cookie authentication; reconnect with a browser session; no script was submitted")
	}
	canPrompt := interactive && (c.hasTurnInteractionProvider() || credentialRecoveryInteractive())
	jar, err := cookiejar.New(nil)
	if err != nil {
		return errors.New("Could not prepare browser reauthentication; no script was submitted")
	}
	hc := http.Client{}
	if c.httpClient != nil {
		hc = *c.httpClient
	}
	hc.Jar = jar
	// Bound network operations independently of time spent in human prompts.
	hc.Timeout = 30 * time.Second
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !sameOriginInstance(instance, req.URL.String()) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	// The isolated login client must never inherit OAuth or old snapshot
	// credentials when fetching the newly authenticated page's CSRF token.
	login := &Client{cfg: c.cfg, opts: Options{AuthMode: mode}, gatewayAuth: mode, httpClient: &hc}
	var session WebSession
	entered, storedRejected := false, false
	var username, password string
	if mode == authModeForm {
		credentials, stored, loadErr := loadStoredInstanceCredentials(c.opts.Profile, instance)
		if loadErr != nil {
			return errors.New("Could not load saved browser credentials; reconnect this profile; no script was submitted")
		}
		if stored {
			username, password = credentials.Username, credentials.Password
			session, err = login.loginWithServiceNowForm(ctx, username, password)
			if err == nil {
				login.applyWebSession(session)
				err = login.validateInstanceScriptSession(ctx)
				if err != nil && !errors.Is(err, errInvalidWebSession) {
					return err
				}
			}
			storedRejected = err != nil
		}
		if !stored || storedRejected {
			if !canPrompt || ctx.Err() != nil {
				return errors.New("Script browser session expired or lacks CSRF; reauthenticate this profile in the TUI; no script was submitted")
			}
			username, password, err = c.promptServiceNowCredentials(ctx, c.basicUser, "Run Script requires a renewed browser session")
			if err != nil || ctx.Err() != nil {
				return errors.New("Browser reauthentication canceled or failed; no script was submitted")
			}
			entered = true
			login.clearWebSession()
			// clearWebSession installs a new jar; keep the same-origin policy.
			login.httpClient.Transport = hc.Transport
			login.httpClient.CheckRedirect = hc.CheckRedirect
			login.httpClient.Timeout = hc.Timeout
			session, err = login.loginWithServiceNowForm(ctx, username, password)
		}
	} else {
		if !canPrompt {
			return errors.New("Script browser session expired or lacks CSRF; reconnect with --auth cookie and provide fresh cookies and X-UserToken; no script was submitted")
		}
		cookie, promptErr := c.promptCredentialValue(ctx, "credential_cookie", "Run Script requires fresh authenticated browser cookies", true)
		if promptErr != nil || ctx.Err() != nil {
			return errors.New("Browser reauthentication canceled or failed; no script was submitted")
		}
		token, promptErr := c.promptCredentialValue(ctx, "credential_token", "Current X-UserToken / window.g_ck (required for Run Script)", true)
		if promptErr != nil || ctx.Err() != nil || strings.TrimSpace(token) == "-" {
			return errors.New("Browser CSRF token was not supplied; no script was submitted")
		}
		session = WebSession{InstanceURL: instance, AuthMode: mode, CookieHeader: normalizeCookieHeader(cookie), UserToken: strings.TrimSpace(token), CreatedAt: time.Now()}
	}
	if err != nil {
		return errors.New("Browser reauthentication failed; check credentials or use --auth cookie for SSO/MFA; no script was submitted")
	}
	login.applyWebSession(session)
	if err := login.validateInstanceScriptSession(ctx); err != nil {
		return err
	}
	if ctx.Err() != nil || c.cfg.InstanceURL != instance {
		return errors.New("Instance changed or reauthentication was canceled; no script was submitted")
	}
	if entered {
		if err := c.persistCredentialChoice(ctx, username, password, storedRejected); err != nil {
			return errors.New("Credential storage choice canceled or failed; no script was submitted")
		}
	}
	if ctx.Err() != nil || c.cfg.InstanceURL != instance {
		return errors.New("Instance changed or reauthentication was canceled; no script was submitted")
	}
	if err := saveWebSession(c.opts.Profile, session); err != nil {
		return errors.New("Could not save renewed browser session; no script was submitted")
	}
	c.applyWebSession(session)
	c.reportAuthenticationNotice("Renewed the browser session for Run Script.", "renewed browser session for Run Script", false)
	return nil
}
