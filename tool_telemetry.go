package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

type BuildAgentToolTelemetry struct {
	Name      string `json:"name"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Status    string `json:"status"`
	SysID     string `json:"sys_id"`
	EventID   string `json:"event_id"`
	Errors    string `json:"errors"`
}

func NewBuildAgentToolTelemetry(name string, start time.Time, success bool, result, eventID string) BuildAgentToolTelemetry {
	status := "Failure"
	errors := strings.TrimSpace(result)
	if success {
		status, errors = "Success", ""
	}
	return BuildAgentToolTelemetry{Name: name, StartTime: formatBuildAgentTelemetryTime(start), EndTime: formatBuildAgentTelemetryTime(time.Now()), Status: status, SysID: "-1", EventID: eventID, Errors: errors}
}

func (c *Client) PostBuildAgentToolTelemetry(ctx context.Context, rows []BuildAgentToolTelemetry) {
	if len(rows) == 0 || c == nil || c.httpClient == nil {
		return
	}
	raw, err := json.Marshal(map[string]interface{}{"payload": rows, "isEventTelemetry": true})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.buildAgentTelemetryEndpoint(), bytes.NewReader(raw))
	if err != nil {
		return
	}
	c.setGatewayHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
}
