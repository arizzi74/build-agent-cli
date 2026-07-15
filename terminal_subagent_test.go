package main

import (
	"strings"
	"testing"
)

func resetTerminalFooterSubagentsForTest(t *testing.T) {
	t.Helper()
	terminalFooterSubagents.Lock()
	previousEntries := terminalFooterSubagents.entries
	previousOrder := append([]string(nil), terminalFooterSubagents.order...)
	previousFrame := terminalFooterSubagents.frame
	terminalFooterSubagents.entries = make(map[string]string)
	terminalFooterSubagents.order = nil
	terminalFooterSubagents.frame = 0
	terminalFooterSubagents.Unlock()
	t.Cleanup(func() {
		terminalFooterSubagents.Lock()
		terminalFooterSubagents.entries = previousEntries
		terminalFooterSubagents.order = previousOrder
		terminalFooterSubagents.frame = previousFrame
		terminalFooterSubagents.Unlock()
	})
}

func TestTerminalFooterSubagentsRemoveOnlyMatchingAgent(t *testing.T) {
	resetTerminalFooterSubagentsForTest(t)
	setTerminalFooterSubagent("agent-a", "research")
	setTerminalFooterSubagent("agent-b", "implement")

	line := terminalFooterSubagentLine(100, false)
	if !strings.Contains(line, "research") || !strings.Contains(line, "implement") {
		t.Fatalf("running agents missing from footer line: %q", line)
	}

	clearTerminalFooterSubagent("agent-a", "research")
	line = terminalFooterSubagentLine(100, false)
	if strings.Contains(line, "research") || !strings.Contains(line, "implement") {
		t.Fatalf("clearing one agent changed the wrong footer entries: %q", line)
	}
}

func TestTerminalFooterSubagentsFallbackEndRemovesOneMatchingName(t *testing.T) {
	resetTerminalFooterSubagentsForTest(t)
	setTerminalFooterSubagent("first", "worker")
	setTerminalFooterSubagent("second", "worker")
	clearTerminalFooterSubagent("", "worker")

	terminalFooterSubagents.Lock()
	defer terminalFooterSubagents.Unlock()
	if len(terminalFooterSubagents.entries) != 1 {
		t.Fatalf("fallback end removed %d agents, want one remaining", len(terminalFooterSubagents.entries))
	}
	if _, ok := terminalFooterSubagents.entries["second"]; !ok {
		t.Fatalf("fallback end should preserve unrelated overlapping agent: %#v", terminalFooterSubagents.entries)
	}
}

func TestTerminalFooterSubagentLineUsesBrightGreen(t *testing.T) {
	resetTerminalFooterSubagentsForTest(t)
	setTerminalFooterSubagent("agent-a", "research")
	line := terminalFooterSubagentLine(100, true)
	if !strings.Contains(line, "\x1b[1;38;5;") || !strings.Contains(stripANSI(line), "research") {
		t.Fatalf("glowing footer line = %q", line)
	}
}

func TestTerminalFooterSubagentLineGlowAdvances(t *testing.T) {
	resetTerminalFooterSubagentsForTest(t)
	setTerminalFooterSubagent("agent-a", "research")
	first := terminalFooterSubagentLine(100, true)
	advanceTerminalFooterSubagentFrame()
	second := terminalFooterSubagentLine(100, true)
	if first == second {
		t.Fatalf("sub-agent glow did not advance: %q", first)
	}
}
