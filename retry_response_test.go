package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestRetryGETRawPreservesSafeContentType(t *testing.T) {
	for _, ct := range []string{"application/json", "text/html"} {
		calls := 0
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write([]byte("ok"))
		}))
		f := &fakeClock{now: time.Unix(0, 0)}
		c := &Client{httpClient: s.Client(), remoteRetryPolicy: testPolicy(f)}
		body, got, _, err := c.getRaw(context.Background(), s.URL+"/raw?token=secret", "*")
		s.Close()
		if err != nil || string(body) != "ok" || got != ct {
			t.Fatalf("ct=%q got=%q body=%q err=%v", ct, got, body, err)
		}
	}
}
func TestRetryGETBodyLimitsAndReadFailure(t *testing.T) {
	for _, status := range []int{200, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			served := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				served++
				w.WriteHeader(status)
				_, _ = w.Write(append(bytes.Repeat([]byte("x"), 1024), []byte("SECRETTAIL")...))
			}))
			defer s.Close()
			f := &fakeClock{now: time.Unix(0, 0)}
			p := testPolicy(f)
			p.MaxAttempts = 2
			c := &Client{httpClient: s.Client(), remoteRetryPolicy: p}
			result, err := c.retryGETResult(context.Background(), s.URL, "safe", "http", 32, func(*http.Request) {})
			if result.Body != nil {
				t.Fatal("oversized body retained")
			}
			var tooLarge errRetryBodyTooLarge
			wantServed := 1
			if status == http.StatusInternalServerError {
				wantServed = 2
			} // status remains independently retryable; each body is bounded.
			if !errors.As(err, &tooLarge) || served != wantServed {
				t.Fatalf("err=%v served=%d", err, served)
			}
			for _, a := range c.retryTelemetry() {
				if a.ErrorCategory == "SECRETTAIL" || a.Endpoint == "SECRETTAIL" {
					t.Fatal("secret telemetry")
				}
			}
		})
	}
	f := &fakeClock{now: time.Unix(0, 0)}
	c := &Client{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(failingReadReader{})}, nil
	})}, remoteRetryPolicy: testPolicy(f)}
	_, err := c.retryGETResult(context.Background(), "http://x/read", "safe", "http", 32, func(*http.Request) {})
	if err == nil || len(c.retryTelemetry()) != 3 {
		t.Fatalf("read failure retry err=%v attempts=%d", err, len(c.retryTelemetry()))
	}
}

type failingReadReader struct{}

func (failingReadReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func TestTurnRetryTelemetryOffset(t *testing.T) {
	f := &fakeClock{now: time.Unix(0, 0)}
	c := &Client{remoteRetryPolicy: testPolicy(f)}
	c.recordAttempt(AttemptTelemetry{Operation: "prior", StartedAt: f.Now(), EndedAt: f.Now()})
	c.beginActiveTurn(context.Background())
	c.recordAttempt(AttemptTelemetry{Operation: "active", StartedAt: f.Now(), EndedAt: f.Now()})
	if got := c.turnRetryTelemetry(true); len(got) != 1 || got[0].Operation != "active" {
		t.Fatalf("active=%#v", got)
	}
	if got := c.turnRetryTelemetry(false); len(got) != 2 {
		t.Fatalf("idle=%#v", got)
	}
}
func TestSemanticRetryCanarySanitized(t *testing.T) {
	dir := t.TempDir()
	oldHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", dir)
	defer os.Setenv("HOME", oldHome)
	f := &fakeClock{now: time.Unix(0, 0)}
	c := &Client{opts: Options{Nirvana: true, Profile: "p"}, workspaceName: "w", semanticState: NewSemanticTurnState(), semanticLifecycleID: "id", remoteRetryPolicy: testPolicy(f)}
	c.semanticState.Status = SemanticTurnAccepted
	c.emitRetryAttempted("Bearer secret", 1, "token=secret")
	entries, err := readSemanticJournalChain("p", "w")
	if err != nil || len(entries) != 1 {
		t.Fatalf("journal %v %#v", err, entries)
	}
	raw := stringify(entries[0].Event.Payload)
	if bytes.Contains([]byte(raw), []byte("secret")) {
		t.Fatal("secret journaled")
	}
}
