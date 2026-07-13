package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time                                 { return f.now }
func (f *fakeClock) Sleep(_ context.Context, d time.Duration) error { f.now = f.now.Add(d); return nil }
func testPolicy(f *fakeClock) RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, MaxElapsed: 5 * time.Second, BaseDelay: time.Second, MaxDelay: 2 * time.Second, Now: f.Now, Sleep: f.Sleep, Jitter: func(d time.Duration, _ int) time.Duration { return d }}
}
func TestParseRetryAfterAndCap(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if parseRetryAfter("2", now) != 2*time.Second {
		t.Fatal("seconds")
	}
	if parseRetryAfter(now.Add(3*time.Second).Format(http.TimeFormat), now) != 3*time.Second {
		t.Fatal("date")
	}
	f := &fakeClock{now: now}
	d := testPolicy(f).Decide(now, 1, 429, nil, "9999")
	if d.Delay != 2*time.Second {
		t.Fatalf("cap %v", d.Delay)
	}
}
func TestRetryHTTPMatrix(t *testing.T) {
	for _, tc := range []struct{ status, want int }{{400, 1}, {401, 1}, {403, 1}, {404, 1}, {408, 3}, {429, 3}, {500, 3}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(tc.status) }))
			defer s.Close()
			f := &fakeClock{now: time.Unix(0, 0)}
			c := &Client{httpClient: s.Client(), remoteRetryPolicy: testPolicy(f)}
			_, _, _ = c.getJSON(context.Background(), s.URL+"/api?q=secret")
			if calls != tc.want {
				t.Fatalf("status=%d calls=%d", tc.status, calls)
			}
		})
	}
}
func TestRetryNetworkAndContextMatrix(t *testing.T) {
	f := &fakeClock{now: time.Unix(0, 0)}
	c := &Client{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("net") })}, remoteRetryPolicy: testPolicy(f)}
	_, _, _ = c.getJSON(context.Background(), "http://example.invalid/api")
	if len(c.retryTelemetry()) != 3 {
		t.Fatal("network not retried")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := c.getJSON(ctx, "http://example.invalid/api")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(c.retryTelemetry()) != 3 {
		t.Fatal("context attempted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDeterministicElapsedBudgetAndTelemetryRedaction(t *testing.T) {
	f := &fakeClock{now: time.Unix(0, 0)}
	p := testPolicy(f)
	p.MaxElapsed = 1500 * time.Millisecond
	c := &Client{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("Bearer secret") })}, remoteRetryPolicy: p}
	_, _, _ = c.retryGET(context.Background(), "http://user:pass@example.invalid/a?token=x", "op Bearer secret", "HTTP?bad", func(*http.Request) {})
	got := c.retryTelemetry()
	if len(got) != 2 || got[1].Decision != "elapsed_budget" || got[0].Endpoint != "/a" || got[0].Operation == "opBearersecret" || got[0].Operation != "redacted" {
		t.Fatalf("telemetry %#v", got)
	}
	if got[0].StartedAt.Location() != time.UTC || got[0].Duration < 0 {
		t.Fatal("time")
	}
}
func TestSemanticRetryLifecycleAndNoActiveJournalNoise(t *testing.T) {
	dir := t.TempDir()
	old := os.Getenv("HOME")
	_ = os.Setenv("HOME", dir)
	defer os.Setenv("HOME", old)
	f := &fakeClock{now: time.Unix(0, 0)}
	c := &Client{opts: Options{Nirvana: true, Profile: "p"}, workspaceName: "w", httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("net") })}, remoteRetryPolicy: testPolicy(f), semanticState: NewSemanticTurnState()}
	_, _, _ = c.getJSON(context.Background(), "http://x/api")
	if _, err := os.Stat(semanticJournalFile("p", "w")); !os.IsNotExist(err) {
		t.Fatal("startup retry journaled")
	}
	c.semanticState.Status = SemanticTurnAccepted
	_, _, _ = c.getJSON(context.Background(), "http://x/api")
	entries, err := readSemanticJournalChain("p", "w")
	if err != nil || len(entries) != 6 {
		t.Fatalf("entries=%d err=%v", len(entries), err)
	}
	if entries[0].Event.Type != EventRetryAttempted || entries[1].Event.Type != EventRetryScheduled || entries[4].Event.Type != EventRetryAttempted || entries[5].Event.Type != EventRetryExhausted {
		t.Fatalf("lifecycle %#v", entries)
	}
	if state, err := Reduce(func() []SemanticEvent {
		v := make([]SemanticEvent, 0, len(entries)+1)
		v = append(v, SemanticEvent{Version: SemanticEventVersion, ID: "accepted", Type: EventTurnAccepted, Payload: TurnAcceptedPayload{}})
		for _, e := range entries {
			decoded, _ := decodeSemanticJournalEvent(e.Event)
			v = append(v, decoded)
		}
		return v
	}()); err != nil || state.RetryCount != 2 {
		t.Fatalf("replay %#v %v", state, err)
	}
}
func TestPostBuildFallbackProgressGate(t *testing.T) {
	core, legacy := 0, 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sn_ba_core/agent_gateway_api/conversation" {
			core++
			w.WriteHeader(400)
			_, _ = w.Write([]byte("requested uri does not represent any resource"))
			return
		}
		legacy++
		w.WriteHeader(200)
	}))
	defer s.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: s.URL}, httpClient: s.Client(), opts: Options{Nirvana: true}, semanticState: NewSemanticTurnState()}
	c.semanticState.Status = SemanticTurnStarted
	c.semanticState.AssistantText = "progress"
	if _, _, err := c.postBuildAgentMessage(context.Background(), "one"); err == nil || legacy != 0 {
		t.Fatalf("core=%d legacy=%d", core, legacy)
	}
	c.semanticState = NewSemanticTurnState()
	c.semanticState.Status = SemanticTurnAccepted
	if _, _, err := c.postBuildAgentMessage(context.Background(), "one"); err != nil || legacy != 1 {
		t.Fatalf("pre progress fallback core=%d legacy=%d err=%v", core, legacy, err)
	}
}
