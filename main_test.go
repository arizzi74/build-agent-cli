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

func TestStartupReadyFooterMessageIsCompactAndInstanceFree(t *testing.T) {
	message := startupReadyFooterMessage()
	if strings.Contains(message, "service-now.com") || strings.Contains(message, "https://") || strings.Contains(message, "instance") {
		t.Fatalf("startup footer message should omit instance details: %q", message)
	}
	for _, want := range []string{"Connected", "/help", "/conversation", "/mcp", "/workspace", "/app", "/exit", "/quit"} {
		if !strings.Contains(message, want) {
			t.Fatalf("startup footer message missing %q: %q", want, message)
		}
	}
}
