package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBuildAgentTelemetryStartPostsExactEndpointAndPayload(t *testing.T) {
	previousNow := buildAgentTelemetryNow
	buildAgentTelemetryNow = func() time.Time { return time.Date(2026, 7, 9, 9, 21, 20, 0, time.UTC) }
	defer func() { buildAgentTelemetryNow = previousNow }()

	var gotMethod, gotPath, gotContentType, gotAccept string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"sys_id":"telemetry-start-sys-id"}}`))
	}))
	defer server.Close()

	client := &Client{
		cfg:              CLIConfig{InstanceURL: server.URL + "/"},
		httpClient:       server.Client(),
		oauthAccessToken: telemetryJWTWithSub(t, "user-123"),
	}
	state := client.StartBuildAgentTelemetry(context.Background(), "Please build and install the app")

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/sn_build_agent/build_agent_api/telemetry" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotContentType != "application/json" || gotAccept != "application/json" {
		t.Fatalf("headers Content-Type=%q Accept=%q, want application/json", gotContentType, gotAccept)
	}
	wantBody := `{"payload":{"user":"user-123","request":"Please build and install the app","task_type":"create","agent_status":"success","start_time":"2026-07-09 09:21:20","end_time":"","status":"in-progress","rollbacks":0,"metadata_types":0,"sys_id":"-1","lines_added":0,"lines_edited":0,"lines_deleted":0,"build_fix_cycles":0,"build_fix_errors":""},"isEventTelemetry":false}`
	if string(gotBody) != wantBody {
		t.Fatalf("body = %s\nwant = %s", gotBody, wantBody)
	}
	if state.SysID != "telemetry-start-sys-id" {
		t.Fatalf("state.SysID = %q", state.SysID)
	}
}

func TestBuildAgentTelemetryFinishPostsExactPayloadWithSysID(t *testing.T) {
	previousNow := buildAgentTelemetryNow
	buildAgentTelemetryNow = func() time.Time { return time.Date(2026, 7, 9, 9, 21, 33, 0, time.UTC) }
	defer func() { buildAgentTelemetryNow = previousNow }()

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sn_build_agent/build_agent_api/telemetry" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"sys_id":"telemetry-finish-sys-id"}}`))
	}))
	defer server.Close()

	client := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	state := &BuildAgentTelemetryState{
		User:           "user-123",
		Request:        "Please build and install the app",
		TaskType:       BuildAgentTelemetryTaskTypeCreate,
		AgentStatus:    BuildAgentTelemetryAgentStatusSuccess,
		StartTime:      "2026-07-09 09:21:20",
		Status:         BuildAgentTelemetryStatusInProgress,
		Rollbacks:      1,
		MetadataTypes:  1,
		SysID:          "telemetry-start-sys-id",
		LinesAdded:     7,
		LinesEdited:    3,
		LinesDeleted:   1,
		BuildFixCycles: 2,
		BuildFixErrors: "fix failed once",
	}

	client.FinishBuildAgentTelemetry(context.Background(), state, BuildAgentTelemetryStatusComplete)

	wantBody := `{"payload":{"user":"user-123","request":"Please build and install the app","task_type":"create","agent_status":"success","start_time":"2026-07-09 09:21:20","end_time":"2026-07-09 09:21:33","status":"complete","rollbacks":1,"metadata_types":1,"sys_id":"telemetry-start-sys-id","lines_added":7,"lines_edited":3,"lines_deleted":1,"build_fix_cycles":2,"build_fix_errors":"fix failed once"},"isEventTelemetry":false}`
	if string(gotBody) != wantBody {
		t.Fatalf("body = %s\nwant = %s", gotBody, wantBody)
	}
	if state.SysID != "telemetry-finish-sys-id" {
		t.Fatalf("state.SysID = %q", state.SysID)
	}
}

func TestBuildAgentTelemetryFinishSupportsErrorAndCancelStatuses(t *testing.T) {
	cases := map[string]string{
		"failed":    BuildAgentTelemetryStatusError,
		"failure":   BuildAgentTelemetryStatusError,
		"error":     BuildAgentTelemetryStatusError,
		"canceled":  BuildAgentTelemetryStatusCancel,
		"cancelled": BuildAgentTelemetryStatusCancel,
		"cancel":    BuildAgentTelemetryStatusCancel,
		"complete":  BuildAgentTelemetryStatusComplete,
		"unknown":   BuildAgentTelemetryStatusError,
	}
	for input, want := range cases {
		if got := normalizeBuildAgentTelemetryFinishStatus(input); got != want {
			t.Fatalf("normalizeBuildAgentTelemetryFinishStatus(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBuildAgentTelemetryNetworkFailureIsNonFatal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &Client{cfg: CLIConfig{InstanceURL: server.URL}, httpClient: server.Client()}
	state := client.StartBuildAgentTelemetry(context.Background(), "request survives telemetry failure")
	if state == nil {
		t.Fatal("expected state even when telemetry POST fails")
	}
	if state.Status != BuildAgentTelemetryStatusInProgress {
		t.Fatalf("state.Status = %q", state.Status)
	}

	client.FinishBuildAgentTelemetry(context.Background(), state, BuildAgentTelemetryStatusCancel)
	if state.Status != BuildAgentTelemetryStatusCancel {
		t.Fatalf("state.Status = %q", state.Status)
	}
}

func telemetryJWTWithSub(t *testing.T, sub string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"sub": sub})
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
