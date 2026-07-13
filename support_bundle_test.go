package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSupportBundleRegistryHelpAndJSON(t *testing.T) {
	inSupportBundleTestDir(t)
	c := newStatusClient(t)
	command, ok := findSlashCommand("/support-bundle")
	if !ok || !command.AvailableWhileProcessing {
		t.Fatalf("registry=%+v %v", command, ok)
	}
	var help strings.Builder
	if _, err := withSlashCommandOutput(&help, func() (bool, error) { return handleSlashCommand(context.Background(), c, "/help") }); err != nil || !strings.Contains(help.String(), "/support-bundle") {
		t.Fatalf("help=%q err=%v", help.String(), err)
	}
	if _, err := handleSlashCommand(context.Background(), c, "/support-bundle a.zip b.zip"); err == nil {
		t.Fatal("multiple paths accepted")
	}
	for _, unsafe := range []string{"token=secret.zip", "Bearer-secret.zip", "cookies.zip", "password.zip", "nested/.hidden.zip"} {
		if _, err := handleSlashCommand(context.Background(), c, "/support-bundle "+unsafe); err == nil || strings.Contains(strings.ToLower(err.Error()), "secret") || strings.Contains(strings.ToLower(err.Error()), "bearer") {
			t.Fatalf("unsafe path error leaked or accepted: %v", err)
		}
	}
	var out strings.Builder
	if _, err := withSlashCommandOutput(&out, func() (bool, error) {
		return handleSlashCommand(context.Background(), c, "/support-bundle bundle.zip --json")
	}); err != nil {
		t.Fatal(err)
	}
	var result supportBundleResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &result); err != nil || result.Format != "zip" || result.Path != "bundle.zip" {
		t.Fatalf("result=%q err=%v", out.String(), err)
	}
}

func TestSupportBundleDeterministicAllowlistedAndManifest(t *testing.T) {
	c := newStatusClient(t)
	fixed := time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC)
	statusNow = func() time.Time { return fixed }
	defer func() { statusNow = func() time.Time { return time.Now().UTC() } }()
	first, manifest, err := c.supportBundlePayloads(fixed)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := c.supportBundlePayloads(fixed)
	if err != nil {
		t.Fatal(err)
	}
	var a, b bytes.Buffer
	if err := writeDeterministicSupportZIP(&a, withManifest(first, manifest), fixed); err != nil {
		t.Fatal(err)
	}
	if err := writeDeterministicSupportZIP(&b, withManifest(second, manifest), fixed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("archive bytes are not deterministic")
	}
	zr, err := zip.NewReader(bytes.NewReader(a.Bytes()), int64(a.Len()))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"config.json", "environment.json", "events.json", "inventory.json", "journal.json", "manifest.json", "status.json", "turn.json"}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Mode().Perm() != 0o600 {
			t.Fatalf("mode %s = %o", f.Name, f.Mode().Perm())
		}
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("members=%v", names)
	}
	var got supportBundleManifest
	for _, f := range zr.File {
		if f.Name == "manifest.json" {
			r, _ := f.Open()
			data := new(bytes.Buffer)
			_, _ = data.ReadFrom(r)
			_ = r.Close()
			if err := json.Unmarshal(data.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got.SchemaVersion != SupportBundleSchemaVersion || got.Format != "zip" || len(got.Members) != len(want)-1 {
		t.Fatalf("manifest=%+v", got)
	}
	for _, member := range got.Members {
		raw := first[member.Name]
		sum := sha256.Sum256(raw)
		if member.Size != int64(len(raw)) || member.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("member=%+v", member)
		}
	}
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data := new(bytes.Buffer)
		_, _ = data.ReadFrom(r)
		_ = r.Close()
		for _, canary := range []string{"token-secret", "Bearer secret", "password=", "cookie=", "Authorization:", "g_ck", "session.json", "full prompt"} {
			if strings.Contains(strings.ToLower(f.Name+data.String()), strings.ToLower(canary)) {
				t.Fatalf("archive canary %q leaked in %s", canary, f.Name)
			}
		}
	}
}

func inSupportBundleTestDir(t *testing.T) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func withManifest(payloads map[string][]byte, manifest supportBundleManifest) map[string][]byte {
	out := map[string][]byte{}
	for k, v := range payloads {
		out[k] = v
	}
	raw, _ := json.Marshal(manifest)
	out["manifest.json"] = raw
	return out
}

func TestSupportBundlePathPolicyPermissionsAndNoOverwrite(t *testing.T) {
	inSupportBundleTestDir(t)
	c := newStatusClient(t)
	fixed := time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC)
	statusNow = func() time.Time { return fixed }
	defer func() { statusNow = func() time.Time { return time.Now().UTC() } }()
	result, err := c.createSupportBundle("")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(profileDir(c.opts.Profile), result.Path)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file=%v mode=%v", err, info.Mode())
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || parent.Mode().Perm() != 0o700 {
		t.Fatalf("parent=%v mode=%v", err, parent.Mode())
	}
	if _, err := c.createSupportBundle("custom.zip"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.createSupportBundle("custom.zip"); err == nil {
		t.Fatal("overwrite accepted")
	}
	for _, unsafe := range []string{"../x.zip", "/tmp/x.zip", "x", "x.tar.gz"} {
		if _, _, err := supportBundleDestination(c.opts.Profile, unsafe, fixed); err == nil {
			t.Fatalf("unsafe %q accepted", unsafe)
		}
	}
	if err := os.Symlink(t.TempDir(), "symlink-parent"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove("symlink-parent") })
	if _, err := c.createSupportBundle("symlink-parent/x.zip"); err == nil {
		t.Fatal("symlink parent accepted")
	}
	if err := os.Mkdir("nested", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join("nested", "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.createSupportBundle("nested/escape/x.zip"); err == nil {
		t.Fatal("nested symlink ancestor accepted")
	}
	supportDir := filepath.Join(profileDir(c.opts.Profile), "support")
	if err := os.Chmod(supportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.createSupportBundle(""); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(supportDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("existing support dir was not hardened: %v %v", err, info.Mode())
	}
}

func TestSupportBundleConcurrentPublishDoesNotOverwrite(t *testing.T) {
	inSupportBundleTestDir(t)
	c := newStatusClient(t)
	fixed := time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC)
	statusNow = func() time.Time { return fixed }
	defer func() { statusNow = func() time.Time { return time.Now().UTC() } }()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := c.createSupportBundle("race.zip"); errs <- err }()
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successes=%d", success)
	}
	info, err := os.Stat("race.zip")
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() == 0 {
		t.Fatalf("winner=%v %v", err, info)
	}
}

func TestSupportBundleActiveSnapshotJournalFailureCapsAndRedaction(t *testing.T) {
	inSupportBundleTestDir(t)
	c := newStatusClient(t)
	c.processing = true
	c.turnRuntimeSnapshot = &TurnRuntimeSnapshot{CapturedAt: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC), Profile: "frozen", InstanceHost: "frozen.example", Transport: "nirvana_websocket", Workspace: TurnWorkspaceSnapshot{Name: "frozen"}}
	c.opts.Profile, c.workspaceName = "changed", "changed"
	if err := os.MkdirAll(filepath.Dir(semanticJournalFile(c.opts.Profile, c.workspaceName)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(semanticJournalFile(c.opts.Profile, c.workspaceName), []byte("{bad}\nnext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payloads, _, err := c.supportBundlePayloads(time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payloads["status.json"]), "frozen") || !strings.Contains(string(payloads["journal.json"]), "corrupt") {
		t.Fatalf("snapshot/journal=%s %s", payloads["status.json"], payloads["journal.json"])
	}
	entries := make([]SemanticJournalEnvelope, 0, supportBundleEventLimit+5)
	for i := 1; i <= supportBundleEventLimit+5; i++ {
		entries = append(entries, SemanticJournalEnvelope{Version: SemanticJournalVersion, Workspace: c.workspaceName, Sequence: uint64(i), ScopeSequence: uint64(i), Event: SemanticEvent{Version: SemanticEventVersion, ID: "id", Sequence: uint64(i), Type: EventAssistantDelta, OccurredAt: time.Unix(int64(i), 0), Payload: AssistantDeltaPayload{Delta: "secret prompt"}}})
	}
	if err := writeSemanticJournal(semanticJournalFile(c.opts.Profile, c.workspaceName), entries); err != nil {
		t.Fatal(err)
	}
	c.processing = false
	payloads, _, err = c.supportBundlePayloads(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var events []debugEvent
	if err := json.Unmarshal(payloads["events.json"], &events); err != nil || len(events) != supportBundleEventLimit {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	for name, raw := range payloads {
		if strings.Contains(strings.ToLower(name+string(raw)), "secret prompt") || strings.Contains(strings.ToLower(name+string(raw)), "authorization") {
			t.Fatalf("canary leaked in %s", name)
		}
	}
}

func TestSupportBundleRecursiveAuditRejectsCredentialJSON(t *testing.T) {
	for _, raw := range []string{
		`{"token":"secret"}`,
		`{"cookie":"abc"}`,
		`{"nested":{"password":"abc"}}`,
		`{"message":"Authorization: Bearer abc"}`,
		`{"message":"oauth_token=abc"}`,
	} {
		if err := supportBundleSafeBytes("status.json", []byte(raw)); err == nil {
			t.Fatalf("credential JSON accepted: %s", raw)
		}
	}
	if err := supportBundleSafeBytes("config.json", []byte(`{"authMode":"oauth","authCapabilities":["oauth_bearer"]}`)); err != nil {
		t.Fatalf("safe labels rejected: %v", err)
	}
}
