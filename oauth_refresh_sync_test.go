package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMetadataSyncRefreshesExpiredSelectedProfileBeforeNode(t *testing.T) {
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	defer os.Setenv("HOME", oldHome)

	const profile = "sync-refresh"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth_token.do" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-old" {
			t.Fatalf("form=%v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "access-refreshed", "refresh_token": "refresh-new", "expires_in": 1800, "token_type": "Bearer"})
	}))
	defer server.Close()
	cfg := CLIConfig{InstanceURL: server.URL, OAuthClientID: "client"}
	if err := saveCachedToken(profile, TokenResponse{AccessToken: "expired", RefreshToken: "refresh-old", ExpiresIn: 1, IssuedAt: time.Now().Add(-time.Hour).UnixMilli(), InstanceURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	oldRunner := metadataSyncRunCommand
	defer func() { metadataSyncRunCommand = oldRunner }()
	metadataSyncRunCommand = func(ctx context.Context, projectDir, nodePath, helperPath string, stdin []byte) ([]byte, error) {
		var request map[string]string
		if err := json.Unmarshal(stdin, &request); err != nil {
			t.Fatal(err)
		}
		if request["token"] != "access-refreshed" {
			t.Fatalf("token=%q", request["token"])
		}
		return []byte(`{"changedFiles":[],"handledPaths":[],"completedAtMs":1783926386123}`), nil
	}
	c := &Client{cfg: cfg, opts: Options{Profile: profile}, oauthAccessToken: "expired"}
	if _, err := c.runMetadataIncrementalTransform(context.Background(), dir, "2026-07-13 07:46:26"); err != nil {
		t.Fatal(err)
	}
	if c.oauthAccessToken != "access-refreshed" {
		t.Fatalf("client token not refreshed")
	}
	if _, err := os.Stat(filepath.Join(profileDir(profile), "oauth-token.json")); err != nil {
		t.Fatal(err)
	}
}
