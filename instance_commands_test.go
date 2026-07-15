package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func setupInstanceSwitchTest(t *testing.T) (*Client, CLIConfig, CLIConfig) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	one := CLIConfig{InstanceURL: "https://one.service-now.com", WSURL: "wss://one.service-now.com/ws", ProjectRoot: filepath.Join(home, "projects-one")}
	two := CLIConfig{InstanceURL: "https://two.service-now.com", WSURL: "wss://two.service-now.com/ws", ProjectRoot: filepath.Join(home, "projects-two")}
	if err := saveProfileConfig("one", one); err != nil {
		t.Fatal(err)
	}
	if err := saveProfileConfig("two", two); err != nil {
		t.Fatal(err)
	}
	if err := saveWorkspace("one", WorkspaceState{Name: defaultWorkspaceName, ConversationID: "one-conversation", App: &AppScope{ScopeID: "one-app"}}); err != nil {
		t.Fatal(err)
	}
	if err := saveWorkspace("two", WorkspaceState{Name: defaultWorkspaceName, ConversationID: "two-conversation", App: &AppScope{ScopeID: "two-app"}}); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(one, Options{Profile: "one", ProfileExplicit: true, Nirvana: true})
	if err != nil {
		t.Fatal(err)
	}
	return c, one, two
}

func TestSwitchInstanceDispatchesInstalledCallback(t *testing.T) {
	c, _, _ := setupInstanceSwitchTest(t)
	var selector string
	c.instanceSwitch = func(ctx context.Context, got string) error {
		if ctx == nil {
			t.Fatal("callback context is nil")
		}
		selector = got
		return nil
	}
	if err := c.SwitchInstance(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if selector != "two" {
		t.Fatalf("callback selector=%q, want two", selector)
	}
}

func TestTransitionConfiguredInstanceSuccessUsesFreshClientAndIsolatesState(t *testing.T) {
	old, one, two := setupInstanceSwitchTest(t)
	old.localProjectOverride = "/tmp/one-project"
	old.statusProjectPath = "/tmp/one-project"
	old.workingSet = []interface{}{map[string]interface{}{"scopeId": "one-app"}}
	old.nirvanaMCPServersReady = true
	old.semanticSequence = 7
	old.opts.InstanceURL = one.InstanceURL
	old.opts.WSURL = "wss://override.one.example/ws"
	old.opts.Conversation = "startup-only-conversation"
	if err := saveActiveInstanceProfile("one"); err != nil {
		t.Fatal(err)
	}

	var prepared, connected *Client
	fresh, profile, switched, err := transitionConfiguredInstance(context.Background(), old, "two",
		func(_ context.Context, candidate *Client) error {
			prepared = candidate
			return nil
		},
		func(_ context.Context, candidate *Client) error {
			connected = candidate
			return nil
		},
		saveActiveInstanceProfile,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !switched || fresh == old || prepared != fresh || connected != fresh {
		t.Fatalf("transition = client=%p old=%p prepared=%p connected=%p switched=%v", fresh, old, prepared, connected, switched)
	}
	if profile != "two" || fresh.opts.Profile != "two" || fresh.cfg.InstanceURL != two.InstanceURL || fresh.cfg.ProjectRoot != two.ProjectRoot {
		t.Fatalf("target = profile=%q opts=%q url=%q projectRoot=%q", profile, fresh.opts.Profile, fresh.cfg.InstanceURL, fresh.cfg.ProjectRoot)
	}
	if fresh.opts.InstanceURL != "" || fresh.opts.WSURL != "" || fresh.opts.Conversation != "" {
		t.Fatalf("startup selectors leaked into target options: %#v", fresh.opts)
	}
	if fresh.conversationID != "two-conversation" || fresh.currentApp == nil || fresh.currentApp.ScopeID != "two-app" {
		t.Fatalf("target state was not loaded: conversation=%q app=%#v", fresh.conversationID, fresh.currentApp)
	}
	if fresh.localProjectOverride != "" || fresh.statusProjectPath != "" || fresh.workingSet != nil || fresh.nirvanaMCPServersReady || fresh.semanticSequence != 0 {
		t.Fatalf("old runtime state leaked into fresh client: project=%q status=%q working=%#v mcpReady=%v semantic=%d", fresh.localProjectOverride, fresh.statusProjectPath, fresh.workingSet, fresh.nirvanaMCPServersReady, fresh.semanticSequence)
	}
	if fresh.connected == old.connected || fresh.closed == old.closed || fresh.semanticLifecycleID == old.semanticLifecycleID {
		t.Fatal("fresh client reused old lifecycle state")
	}
	if old.opts.Profile != "one" || old.cfg.InstanceURL != one.InstanceURL || old.localProjectOverride != "/tmp/one-project" || old.semanticSequence != 7 {
		t.Fatalf("old client was mutated: %#v", old)
	}
	if got := readActiveInstanceProfile(); got != "two" {
		t.Fatalf("active instance profile=%q, want two", got)
	}
}

func TestTransitionConfiguredInstanceConnectionFailureKeepsOldClientAndProfile(t *testing.T) {
	old, one, _ := setupInstanceSwitchTest(t)
	old.localProjectOverride = "/tmp/one-project"
	if err := saveActiveInstanceProfile("one"); err != nil {
		t.Fatal(err)
	}
	var candidate *Client
	fresh, _, switched, err := transitionConfiguredInstance(context.Background(), old, "two", nil,
		func(_ context.Context, got *Client) error {
			candidate = got
			return errors.New("target unavailable")
		},
		saveActiveInstanceProfile,
	)
	if err == nil || !strings.Contains(err.Error(), "connect \"two\"") {
		t.Fatalf("transition error=%v", err)
	}
	if switched || fresh != old || candidate == nil || candidate == old {
		t.Fatalf("failure transition = fresh=%p old=%p candidate=%p switched=%v", fresh, old, candidate, switched)
	}
	if old.opts.Profile != "one" || old.cfg.InstanceURL != one.InstanceURL || old.localProjectOverride != "/tmp/one-project" {
		t.Fatalf("old client changed after target failure: %#v", old)
	}
	if got := readActiveInstanceProfile(); got != "one" {
		t.Fatalf("failed transition saved active profile %q", got)
	}
}

func TestTransitionConfiguredInstanceActiveProfileSaveFailureRollsBack(t *testing.T) {
	old, one, _ := setupInstanceSwitchTest(t)
	if err := saveActiveInstanceProfile("one"); err != nil {
		t.Fatal(err)
	}
	var candidate *Client
	fresh, _, switched, err := transitionConfiguredInstance(context.Background(), old, "two", nil,
		func(_ context.Context, got *Client) error {
			candidate = got
			return nil
		},
		func(string) error { return errors.New("disk full") },
	)
	if err == nil || !strings.Contains(err.Error(), "persist active instance profile") {
		t.Fatalf("transition error=%v", err)
	}
	if switched || fresh != old || candidate == nil || candidate == old {
		t.Fatalf("save failure transition = fresh=%p old=%p candidate=%p switched=%v", fresh, old, candidate, switched)
	}
	if old.opts.Profile != "one" || old.cfg.InstanceURL != one.InstanceURL {
		t.Fatalf("old client changed after active-profile save failure: %#v", old)
	}
	if got := readActiveInstanceProfile(); got != "one" {
		t.Fatalf("active instance profile=%q, want one", got)
	}
}

func TestTransitionConfiguredInstanceCurrentProfileIsNoOp(t *testing.T) {
	old, _, _ := setupInstanceSwitchTest(t)
	prepared := false
	fresh, profile, switched, err := transitionConfiguredInstance(context.Background(), old, "one",
		func(context.Context, *Client) error {
			prepared = true
			return nil
		}, nil, func(string) error {
			t.Fatal("active profile should not be saved for a no-op")
			return nil
		})
	if err != nil || switched || fresh != old || profile != "one" || prepared {
		t.Fatalf("no-op = fresh=%p old=%p profile=%q switched=%v prepared=%v err=%v", fresh, old, profile, switched, prepared, err)
	}
}

func TestReplaceTerminalConversationTranscriptClearsPriorInstanceWhenTargetIsEmpty(t *testing.T) {
	terminalSetConversationHistory("Old instance", []interface{}{
		map[string]interface{}{"role": "user", "content": "private old-instance text"},
	})
	t.Cleanup(func() { terminalSetConversationHistory("", nil) })

	target := &Client{}
	target.replaceTerminalConversationTranscript(statusBarState{})
	if entries := terminalTranscriptSnapshot(); len(entries) != 0 {
		t.Fatalf("old instance transcript leaked into empty target: %#v", entries)
	}
}

func TestInstanceSelectorResolvesConfiguredProfileAndInstanceURL(t *testing.T) {
	setupInstanceSwitchTest(t)
	for _, selector := range []string{"two", "two.service-now.com", "https://two.service-now.com"} {
		got, err := resolveInstanceProfile(selector)
		if err != nil || got != "two" {
			t.Fatalf("resolve %q = %q, %v", selector, got, err)
		}
	}
}

func TestInstanceSlashCommandParsingAndListing(t *testing.T) {
	c, _, _ := setupInstanceSwitchTest(t)
	var selected string
	c.instanceSwitch = func(_ context.Context, selector string) error {
		selected = selector
		return nil
	}
	if command, ok := findSlashCommand("/instance"); !ok || command.Canonical != "/instance" {
		t.Fatalf("/instance not registered: %#v %v", command, ok)
	}
	var output strings.Builder
	if handled, err := withSlashCommandOutput(&output, func() (bool, error) {
		return handleSlashCommand(context.Background(), c, "/instance current")
	}); err != nil || !handled {
		t.Fatalf("current handled=%v err=%v", handled, err)
	}
	if !strings.Contains(output.String(), "profile: one") {
		t.Fatalf("current output=%q", output.String())
	}
	output.Reset()
	if handled, err := withSlashCommandOutput(&output, func() (bool, error) {
		return handleSlashCommand(context.Background(), c, "/instance list")
	}); err != nil || !handled || !strings.Contains(output.String(), "one") || !strings.Contains(output.String(), "two") {
		t.Fatalf("list handled=%v err=%v output=%q", handled, err, output.String())
	}
	if handled, err := handleSlashCommand(context.Background(), c, "/instance use two"); err != nil || !handled {
		t.Fatalf("use handled=%v err=%v", handled, err)
	}
	if selected != "two" {
		t.Fatalf("use selected=%q", selected)
	}
	if _, err := handleSlashCommand(context.Background(), c, "/instance use"); err == nil {
		t.Fatal("missing use selector unexpectedly succeeded")
	}
}
