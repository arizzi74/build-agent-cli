package main

import (
	"flag"
	"os"
	"strings"
	"testing"
)

func parseFlagsForTest(t *testing.T, args ...string) Options {
	t.Helper()
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine
	defer func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	}()
	os.Args = append([]string{"build-agent-go-cli"}, args...)
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	return parseFlags()
}

func TestParseFlagsDefaultsToNirvana(t *testing.T) {
	opts := parseFlagsForTest(t)
	if !opts.Nirvana {
		t.Fatalf("Nirvana default = false, want true")
	}
	if opts.WebGateway {
		t.Fatalf("WebGateway default = true, want false")
	}
}

func TestParseFlagsProjectRoot(t *testing.T) {
	opts := parseFlagsForTest(t, "--project-root", "~/Projects/ServiceNow")
	if opts.ProjectRoot != "~/Projects/ServiceNow" {
		t.Fatalf("ProjectRoot = %q", opts.ProjectRoot)
	}
}

func TestVersionFlagAliases(t *testing.T) {
	for _, arg := range []string{"-version", "--version"} {
		if !versionFlagRequested([]string{arg}) {
			t.Fatalf("versionFlagRequested(%q) = false", arg)
		}
		opts := parseFlagsForTest(t, arg)
		if !opts.Version {
			t.Fatalf("parseFlags(%q) did not set Version", arg)
		}
	}
	for _, args := range [][]string{{}, {"--profile", "version"}, {"--version=false"}} {
		if versionFlagRequested(args) {
			t.Fatalf("versionFlagRequested(%v) = true", args)
		}
	}
}

func TestConnectingCancelRequested(t *testing.T) {
	for _, input := range [][]byte{{27}, {3}, {'x', 27}, {27, '[', 'A'}} {
		if !connectingCancelRequested(input) {
			t.Fatalf("connectingCancelRequested(%v) = false", input)
		}
	}
	for _, input := range [][]byte{nil, {}, {'x'}, {'\r'}, {'[', 'A'}} {
		if connectingCancelRequested(input) {
			t.Fatalf("connectingCancelRequested(%v) = true", input)
		}
	}
}

func TestParseFlagsWebGatewayDisablesNirvana(t *testing.T) {
	opts := parseFlagsForTest(t, "--web-gateway")
	if opts.Nirvana {
		t.Fatalf("--web-gateway should disable Nirvana")
	}
	if !opts.WebGateway {
		t.Fatalf("--web-gateway flag not set")
	}
}

func TestParseFlagsCodeAssistDisablesNirvana(t *testing.T) {
	opts := parseFlagsForTest(t, "--code-assist-ws")
	if opts.Nirvana {
		t.Fatalf("--code-assist-ws should disable Nirvana")
	}
	if !opts.CodeAssistWS {
		t.Fatalf("--code-assist-ws flag not set")
	}
}

func TestParseFlagsDebugFileShortFormEnablesDebug(t *testing.T) {
	opts := parseFlagsForTest(t, "-debug", "trace.log")
	if !opts.Debug {
		t.Fatalf("-debug <file> should enable debug")
	}
	if opts.DebugFile != "trace.log" {
		t.Fatalf("DebugFile = %q, want trace.log", opts.DebugFile)
	}
}

func TestParseFlagsDebugFileEqualsFormEnablesDebug(t *testing.T) {
	opts := parseFlagsForTest(t, "--debug=/tmp/ba-trace.log")
	if !opts.Debug {
		t.Fatalf("--debug=<file> should enable debug")
	}
	if opts.DebugFile != "/tmp/ba-trace.log" {
		t.Fatalf("DebugFile = %q, want /tmp/ba-trace.log", opts.DebugFile)
	}
}

func TestParseFlagsDebugFileFlagEnablesDebug(t *testing.T) {
	opts := parseFlagsForTest(t, "--debug-file", "trace.log")
	if !opts.Debug {
		t.Fatalf("--debug-file should enable debug")
	}
	if opts.DebugFile != "trace.log" {
		t.Fatalf("DebugFile = %q, want trace.log", opts.DebugFile)
	}
}

func TestExtractDebugFileArgRejectsBareDebug(t *testing.T) {
	for _, args := range [][]string{{"--debug"}, {"--debug", "--profile-list"}, {"--debug=false"}} {
		if _, _, err := extractDebugFileArg(args); err == nil {
			t.Fatalf("extractDebugFileArg(%v) error = nil, want error", args)
		}
	}
}

func TestStartupConnectionDetailsPreservePreviousMessages(t *testing.T) {
	opts := Options{Profile: "zaiagents", Nirvana: true}
	client := &Client{runtime: RuntimeModelConfig{Provider: "openai", LargeModel: "large", SmallModel: "small"}}
	details := startupConnectionDetails(opts, client)
	for _, want := range []string{"profile: zaiagents", "oauth token cache:", "model: provider=openai large=large small=small", "transport: Nirvana websocket"} {
		if !strings.Contains(details, want) {
			t.Fatalf("startup details missing %q: %q", want, details)
		}
	}
}

func TestStartupReadyFooterMessageIsCompactAndInstanceURLFree(t *testing.T) {
	message := startupReadyFooterMessage()
	if strings.Contains(message, "service-now.com") || strings.Contains(message, "https://") {
		t.Fatalf("startup footer message should omit instance URL details: %q", message)
	}
	for _, want := range []string{"Connected", "/help", "/instance", "/conversation", "/mcp", "/workspace", "/app", "/exit", "/quit"} {
		if !strings.Contains(message, want) {
			t.Fatalf("startup footer message missing %q: %q", want, message)
		}
	}
}
