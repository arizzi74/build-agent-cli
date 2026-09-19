package core

import (
	"context"
	"fmt"
	"strings"
)

// turnPresentationEvent is the ephemeral, front-end-neutral view of a live
// turn. Unlike SemanticEvent it is never persisted: it may contain the same
// concise text the local terminal shows (for example a capped tool summary),
// and exists only long enough for the front end that owns the turn to render
// it.
type turnPresentationEvent struct {
	Kind    string
	Name    string
	Text    string
	Success bool
}

const (
	turnPresentationToolStarted   = "tool_started"
	turnPresentationToolCompleted = "tool_completed"
	turnPresentationToolWarning   = "tool_warning"
	turnPresentationBuildProgress = "build_progress"
	turnPresentationSummary       = "summary"
	turnPresentationSubAgentStart = "sub_agent_started"
	turnPresentationSubAgentEnd   = "sub_agent_ended"
	turnPresentationRuntimeError  = "runtime_error"
	turnPresentationRetry         = "retry"
	turnPresentationFallback      = "fallback"
	turnPresentationUsage         = "usage"
)

type turnInteractionRequest struct {
	Kind    string
	Prompt  string
	Options []string
	Rows    [][2]string
	Secret  bool
	Review  *turnScriptReview
}

// turnScriptReview is ephemeral approval content, not a capped tool summary.
// Front ends must deliver every line before accepting an approving answer.
type turnScriptReview struct {
	Script          string
	Intent          string
	Scope           string
	RollbackContext string
	Instance        string
	Action          string
}

type turnPresentationSink func(turnPresentationEvent)
type turnInteractionProvider func(context.Context, turnInteractionRequest) (string, error)

// snapshotTurnFrontend lets a short-lived replacement candidate inherit the
// front end that initiated an instance switch. The caller installs the values
// on the candidate and must restore them when the transition finishes.
func (c *Client) snapshotTurnFrontend() (turnPresentationSink, turnInteractionProvider) {
	if c == nil {
		return nil, nil
	}
	c.turnFrontendMu.RLock()
	defer c.turnFrontendMu.RUnlock()
	return c.turnPresentationSink, c.turnInteractionProvider
}

// installTurnFrontend binds one transient owner to the Client. Stateful client
// actions are serialized by bacliActionMu, but transport callbacks are on other
// goroutines, so registration and delivery still need their own lock. The
// generation check prevents a late defer from removing a newer front end.
func (c *Client) installTurnFrontend(sink turnPresentationSink, provider turnInteractionProvider) func() {
	if c == nil {
		return func() {}
	}
	c.turnFrontendMu.Lock()
	c.turnFrontendGeneration++
	generation := c.turnFrontendGeneration
	c.turnPresentationSink = sink
	c.turnInteractionProvider = provider
	c.turnFrontendMu.Unlock()
	return func() {
		c.turnFrontendMu.Lock()
		if c.turnFrontendGeneration == generation {
			c.turnPresentationSink = nil
			c.turnInteractionProvider = nil
		}
		c.turnFrontendMu.Unlock()
	}
}

func (c *Client) publishTurnPresentation(event turnPresentationEvent) {
	if c == nil {
		return
	}
	event.Kind = strings.TrimSpace(event.Kind)
	event.Name = singleLineLabel(event.Name)
	event.Text = strings.TrimSpace(event.Text)
	if event.Kind == "" || (event.Name == "" && event.Text == "") {
		return
	}
	c.turnFrontendMu.RLock()
	sink := c.turnPresentationSink
	c.turnFrontendMu.RUnlock()
	if sink != nil {
		sink(event)
	}
}

func (c *Client) requestTurnInteraction(ctx context.Context, request turnInteractionRequest) (string, bool, error) {
	if c == nil {
		return "", false, nil
	}
	c.turnFrontendMu.RLock()
	provider := c.turnInteractionProvider
	c.turnFrontendMu.RUnlock()
	if provider == nil {
		return "", false, nil
	}
	answer, err := provider(ctx, request)
	return answer, true, err
}

func parseTurnApprovalAnswer(answer string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(answer, "/"))) {
	case "approve", "approved", "yes", "y", "1", "true":
		return true, nil
	case "reject", "rejected", "no", "n", "2", "false":
		return false, nil
	default:
		return false, fmt.Errorf("reply Approve or Reject")
	}
}
