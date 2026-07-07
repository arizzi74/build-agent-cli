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
