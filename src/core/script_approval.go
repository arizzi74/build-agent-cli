package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Decisions are ephemeral, exact-payload authorizations. A successful HTTP
// request, a lost response, and an HTTP error all consume the authorization:
// none may cause an automatic replay of a potentially mutating script.
type scriptApprovalDecision struct {
	Digest     string
	Generation uint64
	Decided    bool
	Approved   bool
	Consumed   bool
}

const maxScriptDecisionsPerTurn = 1024

func isInstanceScriptAction(action string) bool {
	return action == "run_script" || action == "rollback_script"
}

func scriptActionError(code, message string) (map[string]interface{}, string) {
	return map[string]interface{}{"success": false, "error": map[string]interface{}{"code": code, "message": message}}, "error"
}

func (c *Client) scriptReview(action string, payload map[string]interface{}) (turnScriptReview, error) {
	review := turnScriptReview{Action: action, Instance: c.cfg.InstanceURL}
	switch action {
	case "run_script":
		request, err := parseInstanceScriptRequest(payload)
		if err != nil {
			return review, err
		}
		review.Script, review.Intent, review.Scope = request.Script, request.Intent, request.Scope
	case "rollback_script":
		id, err := parseInstanceRollbackContext(payload)
		if err != nil {
			return review, err
		}
		review.RollbackContext = id
		review.Intent = "Roll back the instance changes recorded in this rollback context."
	default:
		return review, errors.New("unsupported script action")
	}
	return review, nil
}

func scriptReviewDigest(review turnScriptReview) string {
	raw, _ := json.Marshal(review)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// An omitted tool name can refer to the newest pending tool in the Web UI
// protocol. Named requests must resolve unambiguously; never approve one
// sub-agent's script using another sub-agent's arguments.
func (c *Client) pendingScriptTool(action string) (nirvanaToolCall, bool, error) {
	var found nirvanaToolCall
	for i := len(c.nirvanaToolCallOrder) - 1; i >= 0; i-- {
		call, ok := c.nirvanaToolCalls[c.nirvanaToolCallOrder[i]]
		if !ok || (action != "" && call.ActualName != action) {
			continue
		}
		if action == "" {
			return call, isInstanceScriptAction(call.ActualName), nil
		}
		if found.ID != "" {
			return nirvanaToolCall{}, false, errors.New("multiple pending script tools; approval cannot be correlated safely")
		}
		found = call
	}
	return found, found.ID != "" && isInstanceScriptAction(found.ActualName), nil
}

// Script authorization is bound to a host-accepted turn, not a server's
// turn_start event. Lock in this order everywhere both states are inspected.
func (c *Client) scriptTurnState(ctx context.Context) (context.Context, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	c.activeTurnMu.Lock()
	defer c.activeTurnMu.Unlock()
	turnCtx := c.activeTurnCtx
	if turnCtx == nil || turnCtx.Err() != nil || c.activeTurnStopping {
		return nil, 0, errors.New("script execution requires an active, uncancelled turn")
	}
	c.scriptApprovalMu.Lock()
	generation := c.scriptApprovalGeneration
	c.scriptApprovalMu.Unlock()
	return turnCtx, generation, nil
}

// Usually ctx already is turnCtx. Linking explicitly also keeps a future
// caller's timeout and the active turn's cancellation authoritative together.
func scriptTurnRequestContext(ctx, turnCtx context.Context) (context.Context, context.CancelFunc) {
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(turnCtx, cancel)
	if turnCtx.Err() != nil {
		cancel()
	}
	return requestCtx, func() { stop(); cancel() }
}

func (c *Client) authorizeScriptReview(ctx context.Context, key string, review turnScriptReview) (bool, error) {
	turnCtx, generation, err := c.scriptTurnState(ctx)
	if err != nil {
		return false, err
	}
	ctx, cancel := scriptTurnRequestContext(ctx, turnCtx)
	defer cancel()
	if !c.opts.Nirvana {
		return false, errors.New("script execution requires an authenticated instance session with CSRF protection")
	}
	if review.Instance != c.cfg.InstanceURL {
		return false, errors.New("instance changed before script review")
	}
	digest := scriptReviewDigest(review)
	c.scriptApprovalMu.Lock()
	if c.scriptApprovalGeneration != generation {
		c.scriptApprovalMu.Unlock()
		return false, errors.New("script turn changed before review")
	}
	if existing, ok := c.scriptApprovals[key]; ok {
		c.scriptApprovalMu.Unlock()
		if existing.Digest != digest || existing.Generation != generation {
			return false, errors.New("script payload changed after review; a new tool call and approval are required")
		}
		if !existing.Decided || existing.Consumed {
			return false, errors.New("script approval is already pending or has been consumed")
		}
		return existing.Approved, nil
	}
	if len(c.scriptApprovals) >= maxScriptDecisionsPerTurn {
		c.scriptApprovalMu.Unlock()
		return false, errors.New("script approval limit reached for this turn")
	}
	if c.scriptApprovals == nil {
		c.scriptApprovals = make(map[string]scriptApprovalDecision)
	}
	c.scriptApprovals[key] = scriptApprovalDecision{Digest: digest, Generation: generation}
	c.scriptApprovalMu.Unlock()
	// Repair a definitely expired browser session before asking the user to
	// authorize the script. A later expiry is caught by the POST-boundary check.
	if err := c.ensureInstanceScriptSession(ctx, true); err != nil {
		return false, err
	}
	if ctx.Err() != nil || review.Instance != c.cfg.InstanceURL {
		return false, errors.New("script turn or instance changed before review")
	}

	// --auto-approve never bypasses script review. Script and rollback are
	// explicit instance mutations, unlike ordinary informational elicitations.
	answer, handled, err := c.requestTurnInteraction(ctx, turnInteractionRequest{
		Kind: "script_approval", Prompt: "Review the complete request before approving instance execution.",
		Review: &review, Options: []string{"Approve", "Reject"},
	})
	approved := false
	if handled {
		if err == nil {
			approved, err = parseTurnApprovalAnswer(answer)
		}
	} else {
		c.flushActiveStreamForTerminalInterruption()
		c.clearTurnStatus()
		approved, err = promptScriptApproval(ctx, review)
	}
	if ctx.Err() != nil {
		approved, err = false, ctx.Err()
	}
	if review.Instance != c.cfg.InstanceURL {
		approved, err = false, errors.New("instance changed during script review")
	}
	currentTurnCtx, currentGeneration, stateErr := c.scriptTurnState(ctx)
	if stateErr != nil || currentTurnCtx != turnCtx || currentGeneration != generation {
		approved, err = false, errors.New("script turn ended or changed during review")
	}
	c.scriptApprovalMu.Lock()
	current, present := c.scriptApprovals[key]
	if !present || c.scriptApprovalGeneration != generation || current.Generation != generation || current.Digest != digest || current.Decided {
		approved, err = false, errors.New("script review expired or changed")
	} else {
		c.scriptApprovals[key] = scriptApprovalDecision{Digest: digest, Generation: generation, Decided: true, Approved: approved && err == nil}
	}
	c.scriptApprovalMu.Unlock()
	return approved, err
}

func (c *Client) answerPendingScriptApproval(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string, bool, error) {
	action := strings.TrimSpace(firstString(payload, "tool", "toolName"))
	if action != "" && !isInstanceScriptAction(action) {
		return nil, "", false, nil
	}
	call, found, err := c.pendingScriptTool(action)
	if err != nil {
		result, status := scriptActionError("SCRIPT_APPROVAL_AMBIGUOUS", err.Error())
		return result, status, true, nil
	}
	if !found {
		if action == "" {
			return nil, "", false, nil
		}
		result, status := scriptActionError("NO_PENDING_TOOL", "No pending script tool to review")
		return result, status, true, nil
	}
	review, err := c.scriptReview(call.ActualName, asMap(call.Input))
	if err != nil {
		result, status := scriptActionError("INVALID_SCRIPT_REQUEST", err.Error())
		return result, status, true, nil
	}
	approved, err := c.authorizeScriptReview(ctx, "tool:"+call.ID, review)
	if err != nil {
		result, status := scriptActionError("SCRIPT_APPROVAL_REQUIRED", err.Error())
		return result, status, true, nil
	}
	return map[string]interface{}{"success": approved, "approved": approved}, "complete", true, nil
}

func (c *Client) answerInstanceScriptElicitation(ctx context.Context, elicitationID, action string, payload map[string]interface{}) (map[string]interface{}, string) {
	if strings.TrimSpace(elicitationID) == "" {
		return scriptActionError("MISSING_ELICITATION_ID", "Script execution requires an elicitation ID")
	}
	turnCtx, generation, err := c.scriptTurnState(ctx)
	if err != nil {
		return scriptActionError("SCRIPT_TURN_INACTIVE", "Script execution requires an active, uncancelled turn")
	}
	ctx, cancel := scriptTurnRequestContext(ctx, turnCtx)
	defer cancel()
	review, err := c.scriptReview(action, payload)
	if err != nil {
		return scriptActionError("INVALID_SCRIPT_REQUEST", err.Error())
	}
	call, found, err := c.pendingScriptTool(action)
	if err != nil {
		return scriptActionError("SCRIPT_APPROVAL_AMBIGUOUS", err.Error())
	}
	key := "elicitation:" + elicitationID
	if found {
		key = "tool:" + call.ID
	}
	approved, err := c.authorizeScriptReview(ctx, key, review)
	if err != nil {
		return scriptActionError("SCRIPT_APPROVAL_REQUIRED", err.Error())
	}
	if !approved {
		return scriptActionError("SCRIPT_REJECTED", "The user rejected instance script execution")
	}
	if ctx.Err() != nil {
		return scriptActionError("SCRIPT_CANCELLED", "Script execution was cancelled before submission")
	}
	c.activeTurnMu.Lock()
	if c.activeTurnCtx != turnCtx || turnCtx.Err() != nil || ctx.Err() != nil || c.activeTurnStopping || review.Instance != c.cfg.InstanceURL {
		c.activeTurnMu.Unlock()
		return scriptActionError("SCRIPT_TURN_CHANGED", "Script turn or instance changed before submission")
	}
	c.scriptApprovalMu.Lock()
	decision := c.scriptApprovals[key]
	if c.scriptApprovalGeneration != generation || decision.Generation != generation || decision.Digest != scriptReviewDigest(review) || !decision.Decided || !decision.Approved || decision.Consumed || c.scriptElicitations[elicitationID] {
		c.scriptApprovalMu.Unlock()
		c.activeTurnMu.Unlock()
		return scriptActionError("SCRIPT_ALREADY_SUBMITTED", "Script authorization is invalid or already consumed; execution was not repeated")
	}
	decision.Consumed = true
	c.scriptApprovals[key] = decision
	if c.scriptElicitations == nil {
		c.scriptElicitations = make(map[string]bool)
	}
	c.scriptElicitations[elicitationID] = true
	c.scriptApprovalMu.Unlock()
	c.activeTurnMu.Unlock()
	// Submit the reviewed immutable values, not a mutable caller-owned map.
	if action == "run_script" {
		return c.runInstanceScript(ctx, instanceScriptRequest{Script: review.Script, Intent: review.Intent, Scope: review.Scope})
	}
	if action == "rollback_script" {
		return c.rollbackInstanceScript(ctx, review.RollbackContext)
	}
	return scriptActionError("UNEXPECTED_CLIENT_ACTION", fmt.Sprintf("unsupported script action %q", action))
}
