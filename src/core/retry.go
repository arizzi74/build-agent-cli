package core

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// RetryPolicy is a bounded, injectable policy for idempotent remote reads only.
type RetryPolicy struct {
	MaxAttempts int
	MaxElapsed  time.Duration
	BaseDelay   time.Duration
	MaxDelay    time.Duration // Also caps Retry-After.
	Now         func() time.Time
	Sleep       func(context.Context, time.Duration) error
	Jitter      func(time.Duration, int) time.Duration
}
type RetryDecision struct {
	Retry  bool
	Delay  time.Duration
	Reason string
}
type retryGETResult struct {
	Body        []byte
	Status      int
	ContentType string
}
type errRetryBodyTooLarge struct{ Limit int64 }

func (e errRetryBodyTooLarge) Error() string { return "safe read response body too large" }

type AttemptTelemetry struct {
	Operation      string        `json:"operation"`
	Attempt        int           `json:"attempt"`
	Endpoint       string        `json:"endpoint"`
	Transport      string        `json:"transport"`
	StartedAt      time.Time     `json:"startedAt"`
	EndedAt        time.Time     `json:"endedAt"`
	Duration       time.Duration `json:"duration"`
	StatusCategory string        `json:"statusCategory"`
	ErrorCategory  string        `json:"errorCategory,omitempty"`
	RetryDelay     time.Duration `json:"retryDelay,omitempty"`
	Decision       string        `json:"decision"`
	FallbackReason string        `json:"fallbackReason,omitempty"`
}

const attemptTelemetryLimit = 100

func defaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, MaxElapsed: 5 * time.Second, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, Now: time.Now, Sleep: sleepWithContext, Jitter: func(d time.Duration, _ int) time.Duration { return d }}
}
func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (p RetryPolicy) normalized() RetryPolicy {
	d := defaultRetryPolicy()
	if p.MaxAttempts > 0 {
		d.MaxAttempts = p.MaxAttempts
	}
	if p.MaxElapsed > 0 {
		d.MaxElapsed = p.MaxElapsed
	}
	if p.BaseDelay > 0 {
		d.BaseDelay = p.BaseDelay
	}
	if p.MaxDelay > 0 {
		d.MaxDelay = p.MaxDelay
	}
	if p.Now != nil {
		d.Now = p.Now
	}
	if p.Sleep != nil {
		d.Sleep = p.Sleep
	}
	if p.Jitter != nil {
		d.Jitter = p.Jitter
	}
	return d
}
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, e := strconv.Atoi(v); e == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if when, e := http.ParseTime(v); e == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}
func retryStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}
func retryError(err error) bool {
	var tooLarge errRetryBodyTooLarge
	return err != nil && !errors.As(err, &tooLarge) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
func retryCategory(status int, transportErr error) string {
	if transportErr != nil {
		if errors.Is(transportErr, context.DeadlineExceeded) {
			return "deadline"
		}
		if errors.Is(transportErr, context.Canceled) {
			return "cancelled"
		}
		return "network"
	}
	if status == 429 {
		return "rate_limited"
	}
	if status == 408 {
		return "timeout"
	}
	if status >= 500 {
		return "server"
	}
	if status >= 400 {
		return "client"
	}
	return "success"
}
func (p RetryPolicy) Decide(start time.Time, attempt, status int, transportErr error, retryAfter string) RetryDecision {
	p = p.normalized()
	retryable := retryStatus(status) || retryError(transportErr)
	if !retryable {
		return RetryDecision{Reason: "not_retryable"}
	}
	if attempt >= p.MaxAttempts {
		return RetryDecision{Reason: "max_attempts"}
	}
	now := p.Now()
	elapsed := now.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed >= p.MaxElapsed {
		return RetryDecision{Reason: "elapsed_budget"}
	}
	delay := p.BaseDelay << (attempt - 1)
	if delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	if ra := parseRetryAfter(retryAfter, now); ra > delay {
		delay = ra
	}
	if delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	delay = p.Jitter(delay, attempt)
	if delay < 0 {
		delay = 0
	}
	if delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	if delay > p.MaxElapsed-elapsed {
		return RetryDecision{Reason: "elapsed_budget"}
	}
	return RetryDecision{Retry: true, Delay: delay, Reason: "retryable"}
}
func (c *Client) retryPolicy() RetryPolicy {
	if c.remoteRetryPolicy.MaxAttempts != 0 {
		return c.remoteRetryPolicy.normalized()
	}
	return defaultRetryPolicy()
}
func (c *Client) recordAttempt(t AttemptTelemetry) {
	c.attemptTelemetryMu.Lock()
	defer c.attemptTelemetryMu.Unlock()
	t.Operation = safeTelemetryLabel(t.Operation)
	t.Endpoint = safeEndpointLabel(t.Endpoint)
	t.Transport = safeTelemetryLabel(t.Transport)
	t.StatusCategory = safeTelemetryLabel(t.StatusCategory)
	t.ErrorCategory = safeTelemetryLabel(t.ErrorCategory)
	t.Decision = safeTelemetryLabel(t.Decision)
	t.FallbackReason = safeTelemetryLabel(t.FallbackReason)
	t.StartedAt = t.StartedAt.UTC()
	t.EndedAt = t.EndedAt.UTC()
	if t.EndedAt.Before(t.StartedAt) {
		t.Duration = 0
	} else {
		t.Duration = t.EndedAt.Sub(t.StartedAt)
	}
	if len(c.attemptTelemetry) >= attemptTelemetryLimit {
		copy(c.attemptTelemetry, c.attemptTelemetry[len(c.attemptTelemetry)-attemptTelemetryLimit+1:])
		c.attemptTelemetry = c.attemptTelemetry[:attemptTelemetryLimit-1]
	}
	c.attemptTelemetry = append(c.attemptTelemetry, t)
}
func safeTelemetryLabel(v string) string {
	v = safeSnapshotString(strings.TrimSpace(v))
	lower := strings.ToLower(v)
	if strings.Contains(lower, "[redacted]") || strings.Contains(lower, "bearer") || strings.Contains(lower, "basic") || strings.Contains(lower, "cookie") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") {
		return "redacted"
	}
	var b strings.Builder
	for _, r := range v {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-/", r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
func safeEndpointLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unknown"
	}
	if u, e := url.Parse(raw); e == nil && u.IsAbs() {
		raw = u.EscapedPath()
		if raw == "" {
			return "unknown"
		}
	}
	if !strings.HasPrefix(raw, "/") {
		return "unknown"
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	return safeTelemetryLabel(raw)
}
func (c *Client) retryTelemetry() []AttemptTelemetry {
	c.attemptTelemetryMu.Lock()
	defer c.attemptTelemetryMu.Unlock()
	return append([]AttemptTelemetry(nil), c.attemptTelemetry...)
}
func (c *Client) turnRetryTelemetry(active bool) []AttemptTelemetry {
	c.attemptTelemetryMu.Lock()
	defer c.attemptTelemetryMu.Unlock()
	start := 0
	if active {
		start = c.turnAttemptOffset
		if start < 0 || start > len(c.attemptTelemetry) {
			start = len(c.attemptTelemetry)
		}
	}
	return append([]AttemptTelemetry(nil), c.attemptTelemetry[start:]...)
}
func deterministicJitter(seed int64) func(time.Duration, int) time.Duration {
	r := rand.New(rand.NewSource(seed))
	var mu sync.Mutex
	return func(d time.Duration, _ int) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return d + time.Duration(r.Int63n(int64(d/5+1)))
	}
}

type FallbackDecision struct {
	Allowed bool
	Reason  string
}

func (c *Client) canFallback(from, to, reason string) FallbackDecision {
	if !c.opts.Nirvana {
		return FallbackDecision{Allowed: true, Reason: "no_semantic_turn"}
	}
	c.semanticMu.Lock()
	s := cloneSemanticTurnState(c.semanticState)
	c.semanticMu.Unlock()
	if s.Status == SemanticTurnIdle || s.Terminal() {
		return FallbackDecision{Allowed: true, Reason: "no_active_progress"}
	}
	if s.AssistantText != "" || len(s.Tools) > 0 || s.ElicitationPending || s.AssistantCompleted {
		return FallbackDecision{Reason: "semantic_progress"}
	}
	return FallbackDecision{Allowed: true, Reason: safeTelemetryLabel(reason)}
}
