package main

import (
	"context"
	"os"
	"testing"
)

func TestDeriveWSURL(t *testing.T) {
	got := deriveWSURL("https://dev123.service-now.com/")
	want := "wss://dev123.service-now.com/sncapps/code/assist/ba/nirvana/web-socket"
	if got != want {
		t.Fatalf("deriveWSURL() = %q, want %q", got, want)
	}

	got = deriveWSURL("http://localhost:8080")
	want = "ws://localhost:8080/sncapps/code/assist/ba/nirvana/web-socket"
	if got != want {
		t.Fatalf("deriveWSURL(http) = %q, want %q", got, want)
	}

	got = deriveCodeAssistWSURL("https://dev123.service-now.com/")
	want = "wss://dev123.service-now.com/sncapps/code/assist/ba/web-socket"
	if got != want {
		t.Fatalf("deriveCodeAssistWSURL() = %q, want %q", got, want)
	}
}

func TestExtractAuthCode(t *testing.T) {
	if got := extractAuthCode("abc123"); got != "abc123" {
		t.Fatalf("plain code = %q", got)
	}
	if got := extractAuthCode("https://example.com/callback?state=s&code=abc123"); got != "abc123" {
		t.Fatalf("url code = %q", got)
	}
}

func TestNormalizeAuthModeDefaultsToForm(t *testing.T) {
	mode, err := normalizeAuthMode("")
	if err != nil {
		t.Fatal(err)
	}
	if mode != authModeForm {
		t.Fatalf("default auth mode = %q, want %q", mode, authModeForm)
	}
}

func TestNormalizeGatewayConversationID(t *testing.T) {
	withHyphens := "cabf08f3-50d6-4226-8fc8-09f8f6b1e54e"
	want := "cabf08f350d642268fc809f8f6b1e54e"
	if got := normalizeGatewayConversationID(withHyphens); got != want {
		t.Fatalf("normalizeGatewayConversationID() = %q, want %q", got, want)
	}
	if got := normalizeGatewayConversationID(want); got != want {
		t.Fatalf("compact id changed: %q", got)
	}
	if got := normalizeGatewayConversationID("not-a-valid-id"); got != "" {
		t.Fatalf("invalid id = %q, want empty", got)
	}
	if got := normalizeCodeAssistConversationID(want); got != withHyphens {
		t.Fatalf("normalizeCodeAssistConversationID() = %q, want %q", got, withHyphens)
	}
}

func TestSelectRuntimeModelInfersProvider(t *testing.T) {
	rt, err := selectRuntimeModel("", "gpt_large")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Provider != "openai" || rt.LargeModel != "gpt_large" {
		t.Fatalf("unexpected runtime config: %+v", rt)
	}
}

func TestInstanceProfileHelpers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := profileNameFromInstanceURL("https://demo.example.service-now.com/"); got != "demo-example" {
		t.Fatalf("profileNameFromInstanceURL = %q", got)
	}
	first := CLIConfig{InstanceURL: "https://dev1.service-now.com", WSURL: deriveWSURL("https://dev1.service-now.com")}
	second := CLIConfig{InstanceURL: "https://dev2.service-now.com", WSURL: deriveWSURL("https://dev2.service-now.com")}
	if err := saveProfileConfig("dev1", first); err != nil {
		t.Fatal(err)
	}
	if err := saveProfileConfig("dev2", second); err != nil {
		t.Fatal(err)
	}
	if err := saveCachedToken("dev1", TokenResponse{AccessToken: "tok", IssuedAt: 1, ExpiresIn: 3600, InstanceURL: first.InstanceURL}); err != nil {
		t.Fatal(err)
	}
	if err := saveWebSession("dev2", WebSession{InstanceURL: second.InstanceURL, AuthMode: authModeCookie, CookieHeader: "JSESSIONID=abc"}); err != nil {
		t.Fatal(err)
	}
	if err := saveActiveInstanceProfile("dev2"); err != nil {
		t.Fatal(err)
	}
	instances, err := listProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 || !instances[0].HasToken || !instances[1].HasSession || readActiveInstanceProfile() != "dev2" {
		t.Fatalf("unexpected instances: %+v active=%q", instances, readActiveInstanceProfile())
	}
	matched, err := resolveInstanceProfile("dev2.service-now.com")
	if err != nil || matched != "dev2" {
		t.Fatalf("resolve instance = %q %v", matched, err)
	}
	if err := deleteConfiguredInstance("dev2"); err != nil {
		t.Fatal(err)
	}
	if readActiveInstanceProfile() != "" {
		t.Fatalf("active instance should be cleared after delete, got %q", readActiveInstanceProfile())
	}
}

func TestSelectStartupInstanceUsesActiveForScriptedRuns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveProfileConfig("dev1", CLIConfig{InstanceURL: "https://dev1.service-now.com"}); err != nil {
		t.Fatal(err)
	}
	if err := saveProfileConfig("dev2", CLIConfig{InstanceURL: "https://dev2.service-now.com"}); err != nil {
		t.Fatal(err)
	}
	if err := saveActiveInstanceProfile("dev2"); err != nil {
		t.Fatal(err)
	}
	opts := Options{Profile: "default", Prompts: []string{"hi"}}
	if err := selectStartupInstance(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.Profile != "dev2" {
		t.Fatalf("selected profile = %q, want dev2", opts.Profile)
	}
}

func TestProfileAndWorkspaceState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := CLIConfig{InstanceURL: "https://dev123.service-now.com", WSURL: deriveWSURL("https://dev123.service-now.com")}
	if err := saveProfileConfig("default", cfg); err != nil {
		t.Fatal(err)
	}

	profiles, err := listProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Name != "default" || profiles[0].InstanceURL != cfg.InstanceURL {
		t.Fatalf("unexpected profiles: %+v", profiles)
	}

	client, err := NewClient(cfg, Options{Profile: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if client.workspaceName != defaultWorkspaceName {
		t.Fatalf("workspace = %q, want default", client.workspaceName)
	}
	if _, err := handleSlashCommand(context.Background(), client, "/app use x_demo Demo App"); err != nil {
		t.Fatal(err)
	}
	if client.CurrentApp() == nil || client.CurrentApp().ScopeID != "x_demo" {
		t.Fatalf("unexpected app: %+v", client.CurrentApp())
	}
	if _, err := handleSlashCommand(context.Background(), client, "/workspace new scratch"); err != nil {
		t.Fatal(err)
	}
	if client.workspaceName != "scratch" {
		t.Fatalf("workspace = %q, want scratch", client.workspaceName)
	}
	if _, err := os.Stat(workspaceFile("default", "scratch")); err != nil {
		t.Fatal(err)
	}
}

func TestPureGoWebSessionHelpers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	page := `<html><body><input type="hidden" name="sysparm_ck" value="abc&amp;123"><script>window.g_ck = 'token-123';</script></body></html>`
	values := parseHiddenInputs(page)
	if got := values.Get("sysparm_ck"); got != "abc&123" {
		t.Fatalf("hidden input = %q", got)
	}
	if got := extractGCK(page); got != "token-123" {
		t.Fatalf("g_ck = %q", got)
	}
	if got := normalizeCookieHeader("Cookie: JSESSIONID=abc; glide=def"); got != "JSESSIONID=abc; glide=def" {
		t.Fatalf("cookie header = %q", got)
	}

	session := WebSession{InstanceURL: "https://dev123.service-now.com", AuthMode: authModeCookie, CookieHeader: "JSESSIONID=abc", UserToken: "tok"}
	if err := saveWebSession("default", session); err != nil {
		t.Fatal(err)
	}
	loaded, ok := loadWebSession("default")
	if !ok {
		t.Fatal("expected saved session")
	}
	if loaded.CookieHeader != session.CookieHeader || loaded.UserToken != session.UserToken {
		t.Fatalf("unexpected session: %+v", loaded)
	}
	if err := deleteWebSession("default"); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadWebSession("default"); ok {
		t.Fatal("session should be deleted")
	}
}

func TestParseWebAgentConfig(t *testing.T) {
	fallback := WebAgentConfig{Model: "fallback-model", ProviderURL: "https://fallback", SkillID: "fallback-skill"}
	body := []byte(`{"result":{"model":"gpt_large","providerUrl":"https://provider","skillId":"skill-1"}}`)
	cfg := parseWebAgentConfig(body, fallback)
	if cfg.Model != "gpt_large" || cfg.ProviderURL != "https://provider" || cfg.SkillID != "skill-1" {
		t.Fatalf("unexpected code assist config: %+v", cfg)
	}

	body = []byte(`{"result":{"modelConfig":{"largeConfig":{"model":"claude-opus-4-6"}},"attributes":{"baseUrl":"https://provider2","capabilityId":"skill-2"},"glideAttributes":{"userId":"6816f79cc0a8016401c5a33be04be441","glideBuild":"06-19-2026_0938"}}}`)
	cfg = parseWebAgentConfig(body, fallback)
	if cfg.Model != "claude-opus-4-6" || cfg.ProviderURL != "https://provider2" || cfg.SkillID != "skill-2" {
		t.Fatalf("unexpected providerConfig config: %+v", cfg)
	}
	if cfg.GlideAttributes["userId"] != "6816f79cc0a8016401c5a33be04be441" || cfg.GlideAttributes["glideBuild"] != "06-19-2026_0938" {
		t.Fatalf("unexpected glideAttributes: %+v", cfg.GlideAttributes)
	}
}
