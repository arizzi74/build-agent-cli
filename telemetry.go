package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	BuildAgentTelemetryTaskTypeCreate     = "create"
	BuildAgentTelemetryAgentStatusSuccess = "success"
	BuildAgentTelemetryStatusInProgress   = "in-progress"
	BuildAgentTelemetryStatusComplete     = "complete"
	BuildAgentTelemetryStatusError        = "error"
	BuildAgentTelemetryStatusCancel       = "aborted"
)

const buildAgentTelemetryTimeFormat = "2006-01-02 15:04:05"

var buildAgentTelemetryNow = time.Now

// BuildAgentTelemetryState carries the per-turn row mirrored to
// /api/sn_build_agent/build_agent_api/telemetry. The same state is posted when
// the turn starts and again when it finishes so the returned sys_id can be sent
// back on completion.
type BuildAgentTelemetryState struct {
	User           string
	Request        string
	TaskType       string
	AgentStatus    string
	StartTime      string
	EndTime        string
	Status         string
	Rollbacks      int
	MetadataTypes  int
	SysID          string
	LinesAdded     int
	LinesEdited    int
	LinesDeleted   int
	BuildFixCycles int
	BuildFixErrors string
}

type buildAgentTelemetryEnvelope struct {
	Payload          buildAgentTelemetryPayload `json:"payload"`
	IsEventTelemetry bool                       `json:"isEventTelemetry"`
}

type buildAgentTelemetryPayload struct {
	User           string `json:"user"`
	Request        string `json:"request"`
	TaskType       string `json:"task_type"`
	AgentStatus    string `json:"agent_status"`
	StartTime      string `json:"start_time"`
	EndTime        string `json:"end_time"`
	Status         string `json:"status"`
	Rollbacks      int    `json:"rollbacks"`
	MetadataTypes  int    `json:"metadata_types"`
	SysID          string `json:"sys_id"`
	LinesAdded     int    `json:"lines_added"`
	LinesEdited    int    `json:"lines_edited"`
	LinesDeleted   int    `json:"lines_deleted"`
	BuildFixCycles int    `json:"build_fix_cycles"`
	BuildFixErrors string `json:"build_fix_errors"`
}

// buildAgentTelemetryUserFromOAuthJWT derives the HAR-observed telemetry user
// from the OAuth JWT subject when it is available.
func buildAgentTelemetryUserFromOAuthJWT(oauthJWT string) string {
	claims := decodeJWTPayload(strings.TrimSpace(oauthJWT))
	if sub, ok := claims["sub"].(string); ok {
		return strings.TrimSpace(sub)
	}
	return ""
}

// NewBuildAgentTelemetryState returns an in-progress per-turn telemetry state.
// oauthJWT is a parameter on purpose so tests and future call sites can provide
// the exact access token used for the turn.
func NewBuildAgentTelemetryState(request, oauthJWT string) *BuildAgentTelemetryState {
	return &BuildAgentTelemetryState{
		User:        buildAgentTelemetryUserFromOAuthJWT(oauthJWT),
		Request:     request,
		TaskType:    BuildAgentTelemetryTaskTypeCreate,
		AgentStatus: BuildAgentTelemetryAgentStatusSuccess,
		StartTime:   formatBuildAgentTelemetryTime(buildAgentTelemetryNow()),
		Status:      BuildAgentTelemetryStatusInProgress,
		SysID:       "-1",
	}
}

// StartBuildAgentTelemetry posts the in-progress telemetry row. Telemetry is
// best-effort: network, encoding, and server failures are swallowed after debug
// logging so they cannot break a turn.
func (c *Client) StartBuildAgentTelemetry(ctx context.Context, request string) *BuildAgentTelemetryState {
	state := NewBuildAgentTelemetryState(request, c.buildAgentTelemetryOAuthJWT())
	_ = c.postBuildAgentTelemetry(ctx, state)
	return state
}

// FinishBuildAgentTelemetry posts the terminal telemetry row with status set to
// complete, error, or cancel. Unknown statuses are treated as error.
func (c *Client) FinishBuildAgentTelemetry(ctx context.Context, state *BuildAgentTelemetryState, status string) {
	if state == nil {
		return
	}
	state.Status = normalizeBuildAgentTelemetryFinishStatus(status)
	state.EndTime = formatBuildAgentTelemetryTime(buildAgentTelemetryNow())
	_ = c.postBuildAgentTelemetry(ctx, state)
}

func (c *Client) CompleteBuildAgentTelemetry(ctx context.Context, state *BuildAgentTelemetryState) {
	c.FinishBuildAgentTelemetry(ctx, state, BuildAgentTelemetryStatusComplete)
}

func (c *Client) ErrorBuildAgentTelemetry(ctx context.Context, state *BuildAgentTelemetryState) {
	c.FinishBuildAgentTelemetry(ctx, state, BuildAgentTelemetryStatusError)
}

func (c *Client) CancelBuildAgentTelemetry(ctx context.Context, state *BuildAgentTelemetryState) {
	if state != nil {
		state.AgentStatus = "error_internal"
	}
	c.FinishBuildAgentTelemetry(ctx, state, BuildAgentTelemetryStatusCancel)
}

func (c *Client) postBuildAgentTelemetry(ctx context.Context, state *BuildAgentTelemetryState) error {
	if state == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if c.httpClient == nil {
		if err := c.initGatewayHTTPClient(); err != nil {
			c.debugBuildAgentTelemetryFailure(err)
			return nil
		}
	}
	raw, err := json.Marshal(buildAgentTelemetryEnvelope{
		Payload:          state.buildAgentTelemetryPayload(),
		IsEventTelemetry: false,
	})
	if err != nil {
		c.debugBuildAgentTelemetryFailure(err)
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.buildAgentTelemetryEndpoint(), bytes.NewReader(raw))
	if err != nil {
		c.debugBuildAgentTelemetryFailure(err)
		return nil
	}
	c.setGatewayHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		c.debugBuildAgentTelemetryFailure(err)
		return nil
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		c.debugBuildAgentTelemetryFailure(fmt.Errorf("telemetry POST failed (%d): %s", res.StatusCode, trimBody(body)))
		return nil
	}
	if sysID := parseBuildAgentTelemetrySysID(body); sysID != "" {
		state.SysID = sysID
	}
	return nil
}

func (c *Client) buildAgentTelemetryEndpoint() string {
	return strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/sn_build_agent/build_agent_api/telemetry"
}

func (c *Client) buildAgentTelemetryOAuthJWT() string {
	if token := strings.TrimSpace(c.oauthAccessToken); token != "" {
		return token
	}
	return c.nirvanaRESTAccessToken()
}

func (c *Client) debugBuildAgentTelemetryFailure(err error) {
	if c != nil && c.debug && err != nil {
		c.debugf("warning: build agent telemetry failed: %v\n", err)
	}
}

func (state *BuildAgentTelemetryState) buildAgentTelemetryPayload() buildAgentTelemetryPayload {
	return buildAgentTelemetryPayload{
		User:           state.User,
		Request:        state.Request,
		TaskType:       defaultString(state.TaskType, BuildAgentTelemetryTaskTypeCreate),
		AgentStatus:    defaultString(state.AgentStatus, BuildAgentTelemetryAgentStatusSuccess),
		StartTime:      state.StartTime,
		EndTime:        state.EndTime,
		Status:         state.Status,
		Rollbacks:      state.Rollbacks,
		MetadataTypes:  state.MetadataTypes,
		SysID:          state.SysID,
		LinesAdded:     state.LinesAdded,
		LinesEdited:    state.LinesEdited,
		LinesDeleted:   state.LinesDeleted,
		BuildFixCycles: state.BuildFixCycles,
		BuildFixErrors: state.BuildFixErrors,
	}
}

func parseBuildAgentTelemetrySysID(body []byte) string {
	var response struct {
		Result map[string]interface{} `json:"result"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return ""
	}
	return strings.TrimSpace(firstString(response.Result, "sys_id", "sysId"))
}

func normalizeBuildAgentTelemetryFinishStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case BuildAgentTelemetryStatusComplete:
		return BuildAgentTelemetryStatusComplete
	case BuildAgentTelemetryStatusCancel, "cancel", "cancelled", "canceled":
		return BuildAgentTelemetryStatusCancel
	case BuildAgentTelemetryStatusError, "failed", "failure":
		return BuildAgentTelemetryStatusError
	default:
		return BuildAgentTelemetryStatusError
	}
}

func formatBuildAgentTelemetryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(buildAgentTelemetryTimeFormat)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
