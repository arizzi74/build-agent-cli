package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPostBuildAgentToolTelemetryExactShape(t *testing.T) {
	var body []byte
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { body, _ = io.ReadAll(r.Body); w.WriteHeader(200) }))
	defer s.Close()
	c := &Client{cfg: CLIConfig{InstanceURL: s.URL}, httpClient: s.Client()}
	c.PostBuildAgentToolTelemetry(context.Background(), []BuildAgentToolTelemetry{{Name: "build", StartTime: "2026-07-09 09:19:33", EndTime: "2026-07-09 09:21:04", Status: "Success", SysID: "-1", EventID: "turn-id", Errors: ""}})
	want := `{"isEventTelemetry":true,"payload":[{"name":"build","start_time":"2026-07-09 09:19:33","end_time":"2026-07-09 09:21:04","status":"Success","sys_id":"-1","event_id":"turn-id","errors":""}]}`
	if string(body) != want {
		t.Fatalf("body=%s\nwant=%s", body, want)
	}
	_ = time.Second
}
