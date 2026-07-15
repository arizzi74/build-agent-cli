package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func sessionOAuthClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL, OAuthClientID: "client", OAuthRedirectURI: server.URL + "/callback"}, opts: Options{Profile: "p", Nirvana: true}, httpClient: server.Client(), sessionCookieHeader: "JSESSIONID=ok"}
	return c
}

func TestPrepareConnectionAuthenticationValidatesWebSessionBeforeCachedOAuth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var validations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sn_build_agent/build_agent_api/providerConfig" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		validations++
		if !strings.Contains(r.Header.Get("Cookie"), "JSESSIONID=valid") || r.Header.Get("X-UserToken") != "gck" {
			t.Fatalf("saved web credentials missing: cookie=%q token=%q", r.Header.Get("Cookie"), r.Header.Get("X-UserToken"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"model":"gpt_large"}}`))
	}))
	defer server.Close()
	if err := saveWebSession("zaiagents", WebSession{
		InstanceURL: server.URL, AuthMode: authModeForm, Username: "admin",
		CookieHeader: "JSESSIONID=valid", UserToken: "gck", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveCachedToken("zaiagents", TokenResponse{
		AccessToken: "cached-oauth", InstanceURL: server.URL, IssuedAt: time.Now().UnixMilli(), ExpiresIn: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	c := &Client{cfg: CLIConfig{InstanceURL: server.URL}, opts: Options{Profile: "zaiagents", Nirvana: true, AuthMode: authModeForm}}
	if err := c.PrepareConnectionAuthentication(context.Background()); err != nil {
		t.Fatal(err)
	}
	if validations != 1 || !c.connectionAuthPrepared || c.oauthAccessToken != "cached-oauth" || c.sessionCookieHeader != "JSESSIONID=valid" {
		t.Fatalf("prepared auth validations=%d prepared=%v oauth=%q cookie=%q", validations, c.connectionAuthPrepared, c.oauthAccessToken, c.sessionCookieHeader)
	}
	if err := c.PrepareConnectionAuthentication(context.Background()); err != nil || validations != 1 {
		t.Fatalf("second prepare should be a no-op: validations=%d err=%v", validations, err)
	}
}

func TestSessionOAuthDirectJSONExchangesAndCaches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var tokenForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth_auth.do":
			if !strings.Contains(r.Header.Get("Cookie"), "JSESSIONID=ok") {
				t.Fatal("web session cookie missing")
			}
			_, _ = w.Write([]byte(`{"result":{"code":"direct-code","state":"` + r.URL.Query().Get("state") + `"}}`))
		case "/oauth_token.do":
			_ = r.ParseForm()
			tokenForm = r.Form
			_, _ = w.Write([]byte(`{"access_token":"access-secret","refresh_token":"refresh-secret","expires_in":3600}`))
		}
	}))
	defer server.Close()
	c := sessionOAuthClient(t, server)
	tok, err := c.runSessionOAuthPKCE(context.Background())
	if err != nil || tok.AccessToken != "access-secret" {
		t.Fatalf("token=%+v err=%v", tok, err)
	}
	if tokenForm.Get("code") != "direct-code" || tokenForm.Get("code_verifier") == "" {
		t.Fatalf("bad exchange form: %v", tokenForm)
	}
}

func TestSessionOAuthXMLCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth_auth.do":
			_, _ = w.Write([]byte(`<result><code>xml-code</code><state>` + r.URL.Query().Get("state") + `</state></result>`))
		case "/oauth_token.do":
			_, _ = w.Write([]byte(`{"access_token":"xml-access","expires_in":3600}`))
		}
	}))
	defer server.Close()
	tok, err := sessionOAuthClient(t, server).runSessionOAuthPKCE(context.Background())
	if err != nil || tok.AccessToken != "xml-access" {
		t.Fatalf("token=%+v err=%v", tok, err)
	}
}

func TestSessionOAuthRedirectAndStateMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_auth.do" {
			http.Redirect(w, r, "/callback?code=redirect-code&state=wrong", http.StatusFound)
		}
	}))
	defer server.Close()
	c := sessionOAuthClient(t, server)
	c.httpClient = &http.Client{Transport: server.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	_, err := c.runSessionOAuthPKCE(context.Background())
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("err=%v, want state mismatch", err)
	}
}

func TestSessionOAuthConsentApprovalPostsForm(t *testing.T) {
	old := authConfirm
	defer func() { authConfirm = old }()
	authConfirm = func(string) (bool, error) { return true, nil }
	var contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`<form action="/oauth_auth.do"><input name="token" value="x"><input name="state" value="` + r.URL.Query().Get("state") + `">Authorize consent allow</form>`))
			return
		}
		contentType = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		if r.Form.Get("code_verifier") != "" {
			_, _ = w.Write([]byte(`{"access_token":"access","expires_in":3600}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":{"code":"code","state":"` + r.Form.Get("state") + `"}}`))
	}))
	defer server.Close()
	c := sessionOAuthClient(t, server)
	if _, err := c.runSessionOAuthPKCE(context.Background()); err != nil {
		t.Fatal(err)
	}
	if contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type=%q", contentType)
	}
}

func TestSessionOAuthConsentRequiresApproval(t *testing.T) {
	old := authConfirm
	defer func() { authConfirm = old }()
	asked, posted := false, false
	authConfirm = func(string) (bool, error) { asked = true; return false, nil }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		_, _ = w.Write([]byte(`<form action="/oauth_auth.do"><input name="token" value="x">Authorize consent allow</form>`))
	}))
	defer server.Close()
	_, err := sessionOAuthClient(t, server).runSessionOAuthPKCE(context.Background())
	if err == nil || !asked || posted {
		t.Fatalf("err=%v asked=%v posted=%v", err, asked, posted)
	}
}

func TestSessionTimeoutBehavior(t *testing.T) {
	old := authConfirm
	defer func() { authConfirm = old }()
	var value = "60"
	writes := 0
	approve := false
	authConfirm = func(string) (bool, error) { return approve, nil }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/runQuery/"):
			_, _ = w.Write([]byte(`{"result":{"query_results":[{"sys_id":"prop1","name":"glide.ui.session_timeout","value":"` + value + `"}]}}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/now/table/sys_properties/prop1":
			writes++
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"1440"`) {
				t.Fatalf("unexpected update %s", body)
			}
			value = "1440"
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	c := sessionOAuthClient(t, server)
	if err := c.checkSessionTimeout(context.Background()); err != nil || writes != 0 {
		t.Fatalf("decline err=%v writes=%d", err, writes)
	}
	approve = true
	if err := c.checkSessionTimeout(context.Background()); err != nil || writes != 1 {
		t.Fatalf("approve err=%v writes=%d", err, writes)
	}
	if err := c.checkSessionTimeout(context.Background()); err != nil || writes != 1 {
		t.Fatalf("1440 err=%v writes=%d", err, writes)
	}
}

func TestSessionOAuthRejectsExternalActionAndSessionRequest(t *testing.T) {
	old := authConfirm
	defer func() { authConfirm = old }()
	authConfirm = func(string) (bool, error) { return true, nil }
	externalHits := 0
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { externalHits++ }))
	defer external.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<form action="` + external.URL + `"><input name="x" value="y">Authorize consent allow</form>`))
	}))
	defer server.Close()
	c := sessionOAuthClient(t, server)
	if _, err := c.runSessionOAuthPKCE(context.Background()); err == nil || externalHits != 0 {
		t.Fatalf("err=%v externalHits=%d", err, externalHits)
	}
	if _, err := c.sessionRequest(context.Background(), http.MethodGet, external.URL, nil); err == nil || externalHits != 0 {
		t.Fatalf("cross-origin request was attempted")
	}
}

func TestSessionTimeoutMissingPropertyCreateOnlyAfterApproval(t *testing.T) {
	old := authConfirm
	defer func() { authConfirm = old }()
	approved, creates, value := false, 0, ""
	authConfirm = func(string) (bool, error) { return approved, nil }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/runQuery/"):
			_, _ = w.Write([]byte(`{"result":{"query_results":[]}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"result":[]}`))
		case r.Method == http.MethodPost:
			creates++
			b, _ := io.ReadAll(r.Body)
			value = string(b)
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer server.Close()
	c := sessionOAuthClient(t, server)
	if err := c.checkSessionTimeout(context.Background()); err != nil || creates != 0 {
		t.Fatalf("decline err=%v creates=%d", err, creates)
	}
	approved = true
	// Subsequent reread deliberately remains absent, so create happens once but verify reports failure.
	if err := c.checkSessionTimeout(context.Background()); err == nil || creates != 1 || !strings.Contains(value, `"name":"glide.ui.session_timeout"`) {
		t.Fatalf("err=%v creates=%d value=%s", err, creates, value)
	}
}

func TestSessionTimeoutAutoApproveNeverWrites(t *testing.T) {
	var writes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			writes++
		}
		_, _ = w.Write([]byte(`{"result":{"query_results":[{"sys_id":"p","value":"20"}]}}`))
	}))
	defer server.Close()
	c := sessionOAuthClient(t, server)
	c.opts.AutoApprove = true
	if err := c.checkSessionTimeout(context.Background()); err != nil || writes != 0 {
		t.Fatalf("err=%v writes=%d", err, writes)
	}
}

func TestSessionAndTokenFilePermissions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveWebSession("p", WebSession{InstanceURL: "https://example", CookieHeader: "J=x"}); err != nil {
		t.Fatal(err)
	}
	if err := saveCachedToken("p", TokenResponse{AccessToken: "x", ExpiresIn: 1, IssuedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sessionFile("p"), fileTokenPath("p")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%v err=%v", path, info.Mode(), err)
		}
	}
}
