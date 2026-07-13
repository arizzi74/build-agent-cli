package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	authModeBasic  = "basic"
	authModeForm   = "form"
	authModeCookie = "cookie"
)

var errInvalidWebSession = errors.New("saved web session is not authenticated")

type WebSession struct {
	InstanceURL  string    `json:"instanceUrl"`
	AuthMode     string    `json:"authMode"`
	Username     string    `json:"username,omitempty"`
	CookieHeader string    `json:"cookieHeader"`
	UserToken    string    `json:"userToken,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

func normalizeAuthMode(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = authModeForm
	}
	switch mode {
	case authModeBasic, authModeForm, authModeCookie:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid --auth %q: use basic, form or cookie", mode)
	}
}

func sessionFile(profile string) string {
	return filepath.Join(profileDir(profile), "session.json")
}

func loadWebSession(profile string) (WebSession, bool) {
	raw, err := os.ReadFile(sessionFile(profile))
	if err != nil {
		return WebSession{}, false
	}
	var session WebSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return WebSession{}, false
	}
	if strings.TrimSpace(session.CookieHeader) == "" {
		return WebSession{}, false
	}
	return session, true
}

func loadMatchingWebSession(profile, instanceURL string) (WebSession, bool) {
	session, ok := loadWebSession(profile)
	if !ok || !sameInstance(session.InstanceURL, instanceURL) {
		return WebSession{}, false
	}
	return session, true
}

func saveWebSession(profile string, session WebSession) error {
	if strings.TrimSpace(session.CookieHeader) == "" {
		return errors.New("cannot save web session without cookies")
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now()
	}
	raw, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(sessionFile(profile), append(raw, '\n'))
}

func deleteWebSession(profile string) error {
	err := os.Remove(sessionFile(profile))
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

func (c *Client) initGatewayHTTPClient() error {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	c.httpClient = &http.Client{Jar: jar}
	return nil
}

func (c *Client) configureGatewayAuth(ctx context.Context) error {
	mode, err := normalizeAuthMode(c.opts.AuthMode)
	if err != nil {
		return err
	}
	c.gatewayAuth = mode
	if err := c.initGatewayHTTPClient(); err != nil {
		return err
	}
	switch mode {
	case authModeBasic:
		return c.configureBasicAuth()
	case authModeForm:
		return c.configureFormAuth(ctx)
	case authModeCookie:
		return c.configureCookieAuth(ctx)
	default:
		return fmt.Errorf("unsupported auth mode %q", mode)
	}
}

func (c *Client) configureBasicAuth() error {
	if c.basicUser == "" {
		if c.opts.BasicUser != "" {
			c.basicUser = c.opts.BasicUser
		} else {
			user, err := promptLine("ServiceNow username: ")
			if err != nil {
				return err
			}
			c.basicUser = strings.TrimSpace(user)
		}
	}
	if c.basicUser == "" {
		return errors.New("ServiceNow username is required for basic auth")
	}
	if c.basicPass == "" {
		pass, err := promptPassword("ServiceNow password: ")
		if err != nil {
			return err
		}
		c.basicPass = pass
	}
	return nil
}

func (c *Client) configureFormAuth(ctx context.Context) error {
	if ok, err := c.trySavedWebSession(ctx); ok || err != nil {
		return err
	}
	if err := c.initGatewayHTTPClient(); err != nil {
		return err
	}
	if c.opts.BasicUser != "" {
		c.basicUser = c.opts.BasicUser
	} else {
		user, err := promptLine("ServiceNow username: ")
		if err != nil {
			return err
		}
		c.basicUser = strings.TrimSpace(user)
	}
	if c.basicUser == "" {
		return errors.New("ServiceNow username is required for form auth")
	}
	pass, err := promptPassword("ServiceNow password: ")
	if err != nil {
		return err
	}
	session, err := c.loginWithServiceNowForm(ctx, c.basicUser, pass)
	if err != nil {
		return err
	}
	if err := c.applyAndValidateWebSession(ctx, session); err != nil {
		return fmt.Errorf("form login did not produce a reusable Build Agent web session: %w", err)
	}
	if err := saveWebSession(c.opts.Profile, session); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "saved web session: %s\n", sessionFile(c.opts.Profile))
	return nil
}

func (c *Client) configureCookieAuth(ctx context.Context) error {
	if ok, err := c.trySavedWebSession(ctx); ok || err != nil {
		return err
	}
	if err := c.initGatewayHTTPClient(); err != nil {
		return err
	}
	cookieHeader, err := promptPassword("Cookie header from authenticated browser request: ")
	if err != nil {
		return err
	}
	cookieHeader = normalizeCookieHeader(cookieHeader)
	if cookieHeader == "" {
		return errors.New("cookie header is required")
	}
	userToken, err := promptPassword("X-UserToken / window.g_ck (optional, press Enter to skip): ")
	if err != nil {
		return err
	}
	session := WebSession{
		InstanceURL:  c.cfg.InstanceURL,
		AuthMode:     authModeCookie,
		CookieHeader: cookieHeader,
		UserToken:    strings.TrimSpace(userToken),
		CreatedAt:    time.Now(),
	}
	if err := c.applyAndValidateWebSession(ctx, session); err != nil {
		return fmt.Errorf("provided cookie session is not authenticated for Build Agent: %w", err)
	}
	if err := saveWebSession(c.opts.Profile, session); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "saved web session: %s\n", sessionFile(c.opts.Profile))
	return nil
}

func (c *Client) trySavedWebSession(ctx context.Context) (bool, error) {
	session, ok := loadMatchingWebSession(c.opts.Profile, c.cfg.InstanceURL)
	if !ok {
		return false, nil
	}
	c.applyWebSession(session)
	if err := c.validateWebSession(ctx); err != nil {
		if errors.Is(err, errInvalidWebSession) {
			action, promptErr := promptCredentialRecovery(c.opts.Profile, c.cfg.InstanceURL, "saved web session", err)
			if promptErr != nil {
				return false, promptErr
			}
			switch action {
			case credentialRecoveryRemove:
				if deleteErr := deleteConfiguredInstance(c.opts.Profile); deleteErr != nil {
					return false, deleteErr
				}
				return false, fmt.Errorf("removed instance %q", c.opts.Profile)
			case credentialRecoveryCancel:
				return false, fmt.Errorf("authentication canceled for instance %q", c.opts.Profile)
			default:
				fmt.Fprintf(os.Stderr, "saved web session is expired or rejected; deleting %s\n", sessionFile(c.opts.Profile))
				if deleteErr := deleteWebSession(c.opts.Profile); deleteErr != nil {
					return false, deleteErr
				}
				c.clearWebSession()
				return false, nil
			}
		}
		fmt.Fprintf(os.Stderr, "warning: could not validate saved web session (%v); trying it anyway\n", err)
	}
	fmt.Fprintf(os.Stderr, "loaded web session: %s\n", sessionFile(c.opts.Profile))
	return true, nil
}

func (c *Client) applyAndValidateWebSession(ctx context.Context, session WebSession) error {
	c.applyWebSession(session)
	if err := c.validateWebSession(ctx); err != nil {
		c.clearWebSession()
		return err
	}
	return nil
}

func (c *Client) clearWebSession() {
	c.sessionCookieHeader = ""
	c.userToken = ""
	_ = c.initGatewayHTTPClient()
}

func (c *Client) applyWebSession(session WebSession) {
	c.sessionCookieHeader = normalizeCookieHeader(session.CookieHeader)
	c.userToken = strings.TrimSpace(session.UserToken)
	if c.basicUser == "" {
		c.basicUser = session.Username
	}
	if c.httpClient != nil && c.httpClient.Jar != nil {
		setJarCookiesFromHeader(c.httpClient.Jar, c.cfg.InstanceURL, c.sessionCookieHeader)
	}
}

func (c *Client) validateWebSession(ctx context.Context) error {
	if strings.TrimSpace(c.sessionCookieHeader) == "" {
		return errors.New("saved web session has no cookies")
	}
	for _, path := range []string{
		"/api/sn_build_agent/build_agent_api/providerConfig",
		"/api/sn_ba_core/agent_config_api/config",
	} {
		body, status, err := c.sessionValidationGET(ctx, path)
		if isUnauthenticatedSession(status, body) {
			return errInvalidWebSession
		}
		if err == nil {
			return nil
		}
		if status == http.StatusNotFound || isMissingRESTResource(status, body) {
			continue
		}
	}

	body, status, err := c.sessionValidationGET(ctx, "/navpage.do")
	if isUnauthenticatedSession(status, body) {
		return errInvalidWebSession
	}
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) sessionValidationGET(ctx context.Context, path string) ([]byte, int, error) {
	result, err := c.retryGETResult(ctx, strings.TrimRight(c.cfg.InstanceURL, "/")+path, "session_validation", "http", 2*1024*1024, func(req *http.Request) {
		c.setGatewayHeaders(req)
		req.Header.Set("Accept", "application/json,text/html;q=0.8,*/*;q=0.5")
	})
	body, status := result.Body, result.Status
	if err != nil {
		return body, status, err
	}
	if looksLikeLoginPage(body) || isUserNotAuthenticated(body) {
		return body, status, errInvalidWebSession
	}
	return body, status, nil
}

func isUnauthenticatedSession(status int, body []byte) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || looksLikeLoginPage(body) || isUserNotAuthenticated(body)
}

func (c *Client) loginWithServiceNowForm(ctx context.Context, username, password string) (WebSession, error) {
	loginURL := strings.TrimRight(c.cfg.InstanceURL, "/") + "/login.do"
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, loginURL, nil)
	if err != nil {
		return WebSession{}, err
	}
	setBrowserishHeaders(getReq, c.cfg.InstanceURL)
	getRes, err := c.httpClient.Do(getReq)
	if err != nil {
		return WebSession{}, err
	}
	getBody, _ := io.ReadAll(getRes.Body)
	_ = getRes.Body.Close()
	if getRes.StatusCode < 200 || getRes.StatusCode >= 400 {
		return WebSession{}, fmt.Errorf("GET login.do failed (%d): %s", getRes.StatusCode, trimBody(getBody))
	}

	formValues := parseHiddenInputs(string(getBody))
	formValues.Set("user_name", username)
	formValues.Set("user_password", password)
	formValues.Set("not_important", "")
	formValues.Set("ni.nolog.user_password", "true")
	formValues.Set("ni.noecho.user_name", "true")
	formValues.Set("ni.noecho.user_password", "true")
	formValues.Set("screensize", "1280x720")
	formValues.Set("sys_action", "sysverb_login")
	// These are present as hidden inputs on some login pages but are not sent by the
	// browser's actual ServiceNow login form submit. Keeping the POST shape close to
	// the browser matters on instances that reject hand-built login submissions.
	formValues.Del("sysparm_referring_url")
	formValues.Del("sysparm_view")
	postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(formValues.Encode()))
	if err != nil {
		return WebSession{}, err
	}
	setBrowserishHeaders(postReq, c.cfg.InstanceURL)
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRes, err := c.httpClient.Do(postReq)
	if err != nil {
		return WebSession{}, err
	}
	postBody, _ := io.ReadAll(postRes.Body)
	_ = postRes.Body.Close()
	if postRes.StatusCode < 200 || postRes.StatusCode >= 400 {
		return WebSession{}, fmt.Errorf("POST login.do failed (%d): %s", postRes.StatusCode, trimBody(postBody))
	}
	if looksLikeLoginPage(postBody) {
		return WebSession{}, errors.New("form login did not appear to authenticate; if this instance uses SSO/MFA, use --auth cookie")
	}

	userToken := extractGCK(string(postBody))
	if userToken == "" {
		userToken = c.fetchUserToken(ctx)
	}
	cookieHeader := cookieHeaderFromJar(c.httpClient.Jar, c.cfg.InstanceURL)
	if cookieHeader == "" {
		return WebSession{}, errors.New("form login returned no cookies")
	}
	return WebSession{
		InstanceURL:  c.cfg.InstanceURL,
		AuthMode:     authModeForm,
		Username:     username,
		CookieHeader: cookieHeader,
		UserToken:    userToken,
		CreatedAt:    time.Now(),
	}, nil
}

func (c *Client) fetchUserToken(ctx context.Context) string {
	for _, path := range []string{"/now/build-agent", "/navpage.do", "/"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.cfg.InstanceURL, "/")+path, nil)
		if err != nil {
			continue
		}
		c.setGatewayHeaders(req)
		res, err := c.httpClient.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024))
		_ = res.Body.Close()
		if res.StatusCode >= 200 && res.StatusCode < 400 {
			if token := extractGCK(string(body)); token != "" {
				return token
			}
		}
	}
	return ""
}

func setBrowserishHeaders(req *http.Request, instanceURL string) {
	base := strings.TrimRight(instanceURL, "/")
	req.Header.Set("User-Agent", "Mozilla/5.0 build-agent-go-cli")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,application/json;q=0.8,*/*;q=0.7")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/")
}

func parseHiddenInputs(page string) url.Values {
	values := url.Values{}
	inputRe := regexp.MustCompile(`(?is)<input\b[^>]*>`)
	for _, input := range inputRe.FindAllString(page, -1) {
		name := attrValue(input, "name")
		if name == "" {
			continue
		}
		values.Set(name, attrValue(input, "value"))
	}
	return values
}

func attrValue(tag, attr string) string {
	quoted := regexp.MustCompile(`(?is)\b` + regexp.QuoteMeta(attr) + `\s*=\s*["']([^"']*)["']`)
	if m := quoted.FindStringSubmatch(tag); len(m) >= 2 {
		return html.UnescapeString(m[1])
	}
	unquoted := regexp.MustCompile(`(?is)\b` + regexp.QuoteMeta(attr) + `\s*=\s*([^\s>]+)`)
	if m := unquoted.FindStringSubmatch(tag); len(m) >= 2 {
		return html.UnescapeString(m[1])
	}
	return ""
}

func extractGCK(page string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?is)\b(?:window\.)?g_ck\s*=\s*['"]([^'"]+)['"]`),
		regexp.MustCompile(`(?is)['"]g_ck['"]\s*:\s*['"]([^'"]+)['"]`),
		regexp.MustCompile(`(?is)<meta\b[^>]*(?:name|id)\s*=\s*['"]g_ck['"][^>]*content\s*=\s*['"]([^'"]+)['"]`),
	}
	for _, re := range patterns {
		if m := re.FindStringSubmatch(page); len(m) >= 2 {
			return html.UnescapeString(m[1])
		}
	}
	return ""
}

func looksLikeLoginPage(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("user_password")) && bytes.Contains(lower, []byte("user_name")) && bytes.Contains(lower, []byte("login"))
}

func cookieHeaderFromJar(jar http.CookieJar, instanceURL string) string {
	if jar == nil {
		return ""
	}
	u, err := url.Parse(strings.TrimRight(instanceURL, "/") + "/")
	if err != nil {
		return ""
	}
	cookies := jar.Cookies(u)
	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie.Name == "" {
			continue
		}
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(parts, "; ")
}

func setJarCookiesFromHeader(jar http.CookieJar, instanceURL, header string) {
	if jar == nil || strings.TrimSpace(header) == "" {
		return
	}
	u, err := url.Parse(strings.TrimRight(instanceURL, "/") + "/")
	if err != nil {
		return
	}
	var cookies []*http.Cookie
	for _, part := range strings.Split(normalizeCookieHeader(header), ";") {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "=") {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		cookies = append(cookies, &http.Cookie{Name: name, Value: strings.TrimSpace(value), Path: "/"})
	}
	jar.SetCookies(u, cookies)
}

func normalizeCookieHeader(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(strings.ToLower(v), "cookie:") {
		v = strings.TrimSpace(v[len("cookie:"):])
	}
	return strings.TrimSpace(v)
}

func sameInstance(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(cleanInstanceURL(a), "/"), strings.TrimRight(cleanInstanceURL(b), "/"))
}
