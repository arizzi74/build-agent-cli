package main

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func semanticTestEvent(id string, typ SemanticEventType, payload any) SemanticEvent {
	return SemanticEvent{Version: SemanticEventVersion, ID: id, Type: typ, OccurredAt: time.Unix(0, 0), Payload: payload}
}

func activeSemanticEvents() []SemanticEvent {
	return []SemanticEvent{
		semanticTestEvent("1", EventTurnAccepted, TurnAcceptedPayload{Context: TurnContext{Profile: "default", Workspace: "main", AppScopeID: "scope-old", WorkingSetHash: "ws-old"}}),
		semanticTestEvent("2", EventTurnStarted, nil),
	}
}

func TestSemanticReducerDeterministicReplay(t *testing.T) {
	events := append(activeSemanticEvents(),
		semanticTestEvent("3", EventAssistantDelta, AssistantDeltaPayload{Delta: "Hello "}),
		semanticTestEvent("4", EventToolStarted, ToolStartedPayload{ToolID: "tool-1", Name: "read"}),
		semanticTestEvent("5", EventToolCompleted, ToolCompletedPayload{ToolID: "tool-1", Success: true}),
		semanticTestEvent("6", EventUsageUpdated, UsageUpdatedPayload{InputTokens: 12, OutputTokens: 5, ThinkingTokens: 1}),
		semanticTestEvent("7", EventWorkingSetUpdated, WorkingSetUpdatedPayload{Hash: "ws-next"}),
		semanticTestEvent("8", EventAppScopeChanged, AppScopeChangedPayload{ScopeID: "scope-next"}),
		semanticTestEvent("9", EventAssistantCompleted, AssistantCompletedPayload{}),
		semanticTestEvent("10", EventTurnCompleted, TurnCompletedPayload{Reason: "done"}),
	)
	one, err := Reduce(events)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Reduce(events)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(one, two) {
		t.Fatalf("replay differs:\n%#v\n%#v", one, two)
	}
	if one.Status != SemanticTurnCompleted || one.AssistantText != "Hello " || !one.Tools["tool-1"].Completed {
		t.Fatalf("unexpected final state: %#v", one)
	}
	if one.Context.AppScopeID != "scope-old" || one.Context.WorkingSetHash != "ws-old" {
		t.Fatalf("turn-bound context mutated: %#v", one.Context)
	}
	if one.NextAppScopeID != "scope-next" || one.NextWorkingSetHash != "ws-next" {
		t.Fatalf("next-turn staging missing: %#v", one)
	}
}

func TestSemanticApplyClonesReferenceStateOnSuccessAndFailure(t *testing.T) {
	prior, err := Reduce(append(activeSemanticEvents(), semanticTestEvent("3", EventToolStarted, ToolStartedPayload{ToolID: "tool", Name: "read"})))
	if err != nil {
		t.Fatal(err)
	}
	prior, err = Apply(prior, semanticTestEvent("4", EventTelemetryFailed, TelemetryFailedPayload{Component: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	original := cloneSemanticTurnState(prior)
	success, err := Apply(prior, semanticTestEvent("5", EventToolCompleted, ToolCompletedPayload{ToolID: "tool", Success: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prior, original) {
		t.Fatalf("successful Apply mutated prior state: %#v", prior)
	}
	if !success.Tools["tool"].Completed || len(prior.TelemetryFailures) != 1 {
		t.Fatalf("unexpected clone result: %#v", success)
	}
	_, err = Apply(prior, semanticTestEvent("6", EventToolCompleted, ToolCompletedPayload{ToolID: "missing", Success: true}))
	if err == nil {
		t.Fatal("unknown tool completion accepted")
	}
	if !reflect.DeepEqual(prior, original) {
		t.Fatalf("failed Apply mutated prior state: %#v", prior)
	}
}

func TestSemanticReducerTransitionAndReplayInvariants(t *testing.T) {
	if _, err := Apply(NewSemanticTurnState(), semanticTestEvent("x", EventTurnCompleted, TurnCompletedPayload{})); err == nil {
		t.Fatal("terminal before acceptance accepted")
	}
	accepted, err := Apply(NewSemanticTurnState(), semanticTestEvent("accepted", EventTurnAccepted, TurnAcceptedPayload{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(accepted, semanticTestEvent("early-fail", EventTurnFailed, TurnFailedPayload{})); err == nil {
		t.Fatal("failure before start accepted")
	}
	state, err := Reduce(activeSemanticEvents())
	if err != nil {
		t.Fatal(err)
	}
	state, err = Apply(state, semanticTestEvent("3", EventAssistantCompleted, AssistantCompletedPayload{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(state, semanticTestEvent("4", EventAssistantCompleted, AssistantCompletedPayload{})); err == nil {
		t.Fatal("duplicate assistant completion accepted")
	}
	if _, err := Apply(state, semanticTestEvent("5", EventAssistantDelta, AssistantDeltaPayload{Delta: "late"})); err == nil {
		t.Fatal("delta after assistant completion accepted")
	}
	if _, err := Apply(state, semanticTestEvent("6", EventUsageUpdated, UsageUpdatedPayload{InputTokens: -1})); err == nil {
		t.Fatal("negative usage accepted")
	}

	state, err = Reduce(activeSemanticEvents())
	if err != nil {
		t.Fatal(err)
	}
	delta := semanticTestEvent("replay", EventAssistantDelta, AssistantDeltaPayload{Delta: "once"})
	state, err = Apply(state, delta)
	if err != nil {
		t.Fatal(err)
	}
	state, err = Apply(state, delta)
	if err != nil || state.AssistantText != "once" {
		t.Fatalf("identical replay not idempotent: %#v %v", state, err)
	}
	if _, err := Apply(state, semanticTestEvent("replay", EventAssistantDelta, AssistantDeltaPayload{Delta: "twice"})); err == nil {
		t.Fatal("event id conflict accepted")
	}
}

func TestSemanticToolInvariants(t *testing.T) {
	state, err := Reduce(activeSemanticEvents())
	if err != nil {
		t.Fatal(err)
	}
	start := semanticTestEvent("3", EventToolStarted, ToolStartedPayload{ToolID: "tool", Name: "read"})
	state, err = Apply(state, start)
	if err != nil {
		t.Fatal(err)
	}
	state, err = Apply(state, semanticTestEvent("4", EventToolStarted, ToolStartedPayload{ToolID: "tool", Name: "read"}))
	if err != nil {
		t.Fatalf("identical tool start should be idempotent: %v", err)
	}
	if _, err := Apply(state, semanticTestEvent("5", EventToolStarted, ToolStartedPayload{ToolID: "tool", Name: "write"})); err == nil {
		t.Fatal("conflicting tool start accepted")
	}
	state, err = Apply(state, semanticTestEvent("6", EventToolCompleted, ToolCompletedPayload{ToolID: "tool", Success: true}))
	if err != nil {
		t.Fatal(err)
	}
	state, err = Apply(state, semanticTestEvent("7", EventToolCompleted, ToolCompletedPayload{ToolID: "tool", Success: true}))
	if err != nil {
		t.Fatalf("identical tool completion should be idempotent: %v", err)
	}
	if _, err := Apply(state, semanticTestEvent("8", EventToolCompleted, ToolCompletedPayload{ToolID: "tool", Success: false})); err == nil {
		t.Fatal("conflicting tool completion accepted")
	}
}

func TestSemanticTurnIDBinding(t *testing.T) {
	accepted := semanticTestEvent("1", EventTurnAccepted, TurnAcceptedPayload{})
	state, err := Apply(NewSemanticTurnState(), accepted)
	if err != nil {
		t.Fatal(err)
	}
	started := semanticTestEvent("2", EventTurnStarted, nil)
	started.TurnID = "server-turn"
	state, err = Apply(state, started)
	if err != nil || state.TurnID != "server-turn" {
		t.Fatalf("server turn id not adopted: %#v %v", state, err)
	}
	delta := semanticTestEvent("3", EventAssistantDelta, AssistantDeltaPayload{Delta: "ok"})
	delta.TurnID = "server-turn"
	if _, err := Apply(state, delta); err != nil {
		t.Fatal(err)
	}
	mismatch := semanticTestEvent("4", EventAssistantDelta, AssistantDeltaPayload{Delta: "no"})
	mismatch.TurnID = "other"
	if _, err := Apply(state, mismatch); err == nil {
		t.Fatal("mismatched server turn id accepted")
	}
}

func TestSemanticEventRejectsSecretMetadata(t *testing.T) {
	for _, metadata := range []map[string]string{{"AUTHORIZATION": "safe"}, {"label": "Bearer secret"}, {"trace": "Cookie: session=secret"}} {
		event := semanticTestEvent("1", EventConnectionStateChanged, ConnectionStateChangedPayload{State: "connected"})
		event.Metadata = metadata
		if _, err := Apply(NewSemanticTurnState(), event); err == nil {
			t.Fatalf("unsafe metadata accepted: %#v", metadata)
		}
	}
	good := semanticTestEvent("safe", EventConnectionStateChanged, ConnectionStateChangedPayload{State: "connected"})
	good.Metadata = map[string]string{"label": "normal diagnostic"}
	if _, err := Apply(NewSemanticTurnState(), good); err != nil {
		t.Fatal(err)
	}
}

func TestSemanticReducerTerminalInvariants(t *testing.T) {
	state, err := Reduce(append(activeSemanticEvents(), semanticTestEvent("3", EventTurnCancelled, TurnCancelledPayload{Reason: "user"})))
	if err != nil {
		t.Fatal(err)
	}
	identical := semanticTestEvent("4", EventTurnCancelled, TurnCancelledPayload{Reason: "user"})
	if got, err := Apply(state, identical); err != nil || !reflect.DeepEqual(got, state) {
		t.Fatalf("identical terminal event should be idempotent: state=%#v err=%v", got, err)
	}
	if _, err := Apply(state, semanticTestEvent("5", EventTurnCompleted, TurnCompletedPayload{})); err == nil {
		t.Fatal("conflicting terminal outcome accepted")
	}
	if _, err := Apply(state, semanticTestEvent("6", EventAssistantDelta, AssistantDeltaPayload{Delta: "late"})); err == nil {
		t.Fatal("late assistant delta accepted")
	}
}

func TestSemanticSeamNirvanaLifecycleAndNonNirvanaGate(t *testing.T) {
	nirvana := &Client{
		opts:                Options{Nirvana: true, Profile: "test"},
		semanticState:       NewSemanticTurnState(),
		semanticLifecycleID: "client",
		streamTypes:         map[string]string{},
		toolCallNames:       map[string]string{},
		toolCallInputs:      map[string]interface{}{},
		toolCallStarted:     map[string]time.Time{},
	}
	nirvana.beginSemanticTurnSnapshot(nirvana.captureTurnRuntimeSnapshot(nil))
	nirvana.handleEvent([]byte(`{"type":"turn_start","turn_id":"server-turn"}`))
	nirvana.handleEvent([]byte(`{"type":"stream_start","stream_id":"text","content_type":"text"}`))
	nirvana.handleEvent([]byte(`{"type":"stream_delta","stream_id":"text","delta":"hello"}`))
	nirvana.handleEvent([]byte(`{"type":"turn_end"}`))
	if nirvana.semanticState.Status != SemanticTurnCompleted || nirvana.semanticState.TurnID != "server-turn" || nirvana.semanticState.AssistantText != "hello" || !nirvana.semanticState.AssistantCompleted {
		t.Fatalf("bad Nirvana semantic lifecycle: %#v", nirvana.semanticState)
	}
	if nirvana.semanticSequence != 5 {
		t.Fatalf("semantic sequence = %d, want 5", nirvana.semanticSequence)
	}
	firstTurnEventIDSequence := nirvana.semanticEventIDSequence
	nirvana.beginSemanticTurnSnapshot(nirvana.captureTurnRuntimeSnapshot(nil))
	if nirvana.semanticSequence != 1 || nirvana.semanticEventIDSequence != firstTurnEventIDSequence+1 {
		t.Fatalf("turn sequence/id sequence not scoped correctly: turn=%d id=%d", nirvana.semanticSequence, nirvana.semanticEventIDSequence)
	}

	web := &Client{opts: Options{Nirvana: false}, semanticState: NewSemanticTurnState()}
	web.beginActiveTurn(context.Background())
	if web.semanticSequence != 0 || web.semanticState.Status != SemanticTurnIdle {
		t.Fatalf("non-Nirvana initialized semantic seam: %#v", web.semanticState)
	}
}
