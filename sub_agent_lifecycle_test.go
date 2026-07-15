package main

import "testing"

func TestSemanticSubAgentLifecyclePreservesOverlappingAgents(t *testing.T) {
	state, err := Reduce(append(activeSemanticEvents(),
		semanticTestEvent("3", EventSubAgentStarted, SubAgentLifecyclePayload{AgentID: "a", Name: "research"}),
		semanticTestEvent("4", EventSubAgentStarted, SubAgentLifecyclePayload{AgentID: "b", Name: "implement"}),
		semanticTestEvent("5", EventSubAgentEnded, SubAgentLifecyclePayload{AgentID: "a", Name: "research"}),
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.SubAgents["a"]; ok {
		t.Fatalf("ended sub-agent remained in state: %#v", state.SubAgents)
	}
	if state.SubAgents["b"] != "implement" {
		t.Fatalf("unrelated sub-agent was not preserved: %#v", state.SubAgents)
	}
}

func TestSemanticTerminalEventClearsRunningSubAgents(t *testing.T) {
	state, err := Reduce(append(activeSemanticEvents(),
		semanticTestEvent("3", EventSubAgentStarted, SubAgentLifecyclePayload{AgentID: "a", Name: "research"}),
		semanticTestEvent("4", EventAssistantCompleted, AssistantCompletedPayload{}),
		semanticTestEvent("5", EventTurnCompleted, TurnCompletedPayload{}),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.SubAgents) != 0 {
		t.Fatalf("terminal turn retained sub-agents: %#v", state.SubAgents)
	}
}

func TestSubAgentIdentityPrefersStableIDAndFallsBackToName(t *testing.T) {
	id, name := eventSubAgentIdentity(map[string]interface{}{"agentId": "agent-1", "name": "research"})
	if id != "agent-1" || name != "research" {
		t.Fatalf("stable identity = %q, %q", id, name)
	}
	id, name = eventSubAgentIdentity(map[string]interface{}{"name": "research"})
	if id != "research" || name != "research" {
		t.Fatalf("name fallback identity = %q, %q", id, name)
	}
}
