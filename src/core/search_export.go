package core

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SearchSchemaVersion and ExportSchemaVersion are stable, offline-only documents.
const (
	SearchSchemaVersion = 1
	ExportSchemaVersion = 1
	searchQueryMaxBytes = 256
	searchDefaultLimit  = 20
	searchMaximumLimit  = 50
	exportEventLimit    = 100
	exportMessageLimit  = 100
	exportMaxBytes      = 1 << 20
)

type offlineSearchDocument struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Query         string                `json:"query"`
	Limit         int                   `json:"limit"`
	Availability  string                `json:"availability"`
	Sources       searchSourceHealth    `json:"sources"`
	Results       []offlineSearchResult `json:"results"`
}
type searchSourceHealth struct {
	Journal  string `json:"journal"`
	Runtime  string `json:"runtime"`
	Messages string `json:"messages"`
}
type offlineSearchResult struct {
	Source       string `json:"source"`
	Type         string `json:"type"`
	ID           string `json:"id"`
	OccurredAt   string `json:"occurredAt,omitempty"`
	MatchedField string `json:"matchedField"`
	Preview      string `json:"preview"`
	Score        int    `json:"score"`
}
type offlineSearchRecord struct {
	source, typ, id, occurredAt string
	fields                      map[string]string
}

type conversationExportResult struct {
	SchemaVersion int    `json:"schemaVersion"`
	Format        string `json:"format"`
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	MemberCount   int    `json:"memberCount"`
}
type conversationExportManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Format        string                `json:"format"`
	GeneratedAt   string                `json:"generatedAt"`
	Profile       string                `json:"profile"`
	Workspace     string                `json:"workspace"`
	Conversation  string                `json:"conversationId,omitempty"`
	Snapshot      string                `json:"snapshot"`
	Scope         exportScope           `json:"scope"`
	Members       []supportBundleMember `json:"members"`
	Safety        string                `json:"safety"`
}
type exportScope struct {
	Kind           string `json:"kind"`
	Workspace      string `json:"workspace"`
	ConversationID string `json:"conversationId,omitempty"`
	TurnID         string `json:"turnId,omitempty"`
	MessagePolicy  string `json:"messagePolicy"`
}
type exportMessageMetadata struct {
	ID           string `json:"id"`
	Role         string `json:"role"`
	OccurredAt   string `json:"occurredAt,omitempty"`
	ContentHash  string `json:"contentHash,omitempty"`
	ContentBytes int    `json:"contentBytes"`
	Content      string `json:"content"`
	Scope        string `json:"scope"`
}

func handleSearchCommand(_ context.Context, c *Client, args []string) (bool, error) {
	query, limit, jsonOutput, err := parseSearchArgs(args)
	if err != nil {
		return true, err
	}
	doc := c.offlineSearch(query, limit)
	if jsonOutput {
		raw, err := json.Marshal(doc)
		if err != nil {
			return true, err
		}
		slashCommandPrintln(string(raw))
		return true, nil
	}
	slashCommandPrintf("search: %d result(s), sources=%s\n", len(doc.Results), doc.Availability)
	for _, result := range doc.Results {
		slashCommandPrintf("%s %s %s %s: %s\n", result.Source, result.Type, result.ID, result.MatchedField, result.Preview)
	}
	return true, nil
}

func parseSearchArgs(args []string) (string, int, bool, error) {
	limit, jsonOutput := searchDefaultLimit, false
	queryParts := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			if jsonOutput {
				return "", 0, false, fmt.Errorf("usage: /search <query> [--json] [--limit N]")
			}
			jsonOutput = true
		case "--limit":
			if i+1 >= len(args) {
				return "", 0, false, fmt.Errorf("usage: /search <query> [--json] [--limit N]")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 || n > searchMaximumLimit {
				return "", 0, false, fmt.Errorf("search limit must be between 1 and %d", searchMaximumLimit)
			}
			limit = n
		default:
			queryParts = append(queryParts, args[i])
		}
	}
	query := strings.TrimSpace(strings.Join(queryParts, " "))
	if query == "" {
		return "", 0, false, fmt.Errorf("usage: /search <query> [--json] [--limit N]")
	}
	if len(query) > searchQueryMaxBytes {
		return "", 0, false, fmt.Errorf("search query is too long")
	}
	if credentialValuePattern.MatchString(query) || strings.Contains(strings.ToLower(query), "token=") {
		return "", 0, false, fmt.Errorf("search query is unsafe")
	}
	return query, limit, jsonOutput, nil
}

func (c *Client) offlineSearch(query string, limit int) offlineSearchDocument {
	doc := offlineSearchDocument{SchemaVersion: SearchSchemaVersion, Query: safeSearchQuery(query), Limit: limit, Availability: "healthy", Results: []offlineSearchResult{}}
	records, sources := c.offlineSearchRecords()
	doc.Sources = sources
	if sources.Journal == "corrupt" || sources.Journal == "pruned" || sources.Journal == "missing" || sources.Runtime != "healthy" || sources.Messages != "healthy" {
		doc.Availability = "degraded"
	}
	terms := strings.Fields(strings.ToLower(query))
	for _, record := range records {
		for field, value := range record.fields {
			score := searchScore(terms, strings.ToLower(value), field)
			if score == 0 {
				continue
			}
			doc.Results = append(doc.Results, offlineSearchResult{Source: record.source, Type: record.typ, ID: record.id, OccurredAt: record.occurredAt, MatchedField: field, Preview: safeSearchPreview(value), Score: score})
		}
	}
	sort.Slice(doc.Results, func(i, j int) bool {
		a, b := doc.Results[i], doc.Results[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.OccurredAt != b.OccurredAt {
			return a.OccurredAt > b.OccurredAt
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.MatchedField != b.MatchedField {
			return a.MatchedField < b.MatchedField
		}
		if a.Preview != b.Preview {
			return a.Preview < b.Preview
		}
		return a.Score < b.Score
	})
	if len(doc.Results) > limit {
		doc.Results = doc.Results[:limit]
	}
	return doc
}
func safeSearchQuery(query string) string {
	if credentialValuePattern.MatchString(query) {
		return "[redacted]"
	}
	return safeSearchPreview(query)
}
func safeSearchPreview(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 160 {
		value = value[:160] + "…"
	}
	return safeSnapshotString(value)
}
func searchScore(terms []string, value, field string) int {
	if value == "" {
		return 0
	}
	score := 0
	for _, term := range terms {
		at := strings.Index(value, term)
		if at < 0 {
			return 0
		}
		score += 10
		if at == 0 || strings.ContainsAny(field, "id") {
			score += 5
		}
		if value == term {
			score += 20
		}
	}
	if field == "type" || field == "name" {
		score += 3
	}
	return score
}

func (c *Client) offlineSearchRecords() ([]offlineSearchRecord, searchSourceHealth) {
	status, turn := c.statusDocument(), c.turnDocument()
	records := []offlineSearchRecord{{"runtime", "workspace", safeSnapshotString(status.Context.Workspace), "", map[string]string{"workspace": safeSnapshotString(status.Context.Workspace), "conversationId": safeSnapshotString(status.Context.ConversationID)}}, {"runtime", "app", safeSnapshotString(status.Context.AppScopeID()), "", map[string]string{"scopeId": safeStatusAppScope(status.Context.App), "name": safeStatusAppName(status.Context.App)}}, {"runtime", "turn", safeSnapshotString(turn.Turn.ServerTurnID), turn.Turn.StartedAt, map[string]string{"state": turn.Turn.State, "terminalReason": turn.Turn.TerminalReason, "errorCategory": turn.Turn.ErrorCategory}}}
	for _, tool := range turn.Turn.Tools {
		records = append(records, offlineSearchRecord{"runtime", "tool", tool.ID, "", map[string]string{"name": tool.Name, "state": tool.State}})
	}
	for _, goal := range c.listGoals() {
		records = append(records, offlineSearchRecord{"local", "goal", goal.ID, goal.UpdatedAt, map[string]string{"title": goal.Title, "status": string(goal.Status), "workspace": goal.Workspace}})
	}
	for _, approval := range c.listApprovals() {
		records = append(records, offlineSearchRecord{"local", "approval", approval.ID, approval.CreatedAt, map[string]string{"actionClass": approval.ActionClass, "status": string(approval.Status), "summary": approval.Summary}})
	}
	entries, journal := readTurnJournal(c.opts.Profile, c.workspaceName, c.semanticJournalSequence)
	sources := searchSourceHealth{Journal: journal.Health, Runtime: "healthy", Messages: "healthy"}
	if journal.Health == "healthy" || journal.Health == "pruned" {
		for _, entry := range entries {
			event := redactedDebugEvent(entry)
			fields := map[string]string{"type": event.Type, "id": event.ID, "turnId": event.TurnID}
			keys := make([]string, 0, len(event.Details))
			for k := range event.Details {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v := event.Details[k]
				if s, ok := v.(string); ok && s != "[omitted]" {
					fields[k] = s
				}
				if n, ok := v.(float64); ok {
					fields[k] = fmt.Sprint(n)
				}
			}
			records = append(records, offlineSearchRecord{"journal", "semantic_event", event.ID, event.OccurredAt, fields})
		}
	}
	messageScope := c.exportScope(status, turn)
	for _, msg := range c.exportMessageMetadata(messageScope) {
		records = append(records, offlineSearchRecord{"conversation", "message", msg.ID, msg.OccurredAt, map[string]string{"role": msg.Role, "contentHash": msg.ContentHash, "contentBytes": strconv.Itoa(msg.ContentBytes)}})
	}
	return records, sources
}
func (s statusContext) AppScopeID() string {
	if s.App == nil {
		return ""
	}
	return s.App.ScopeID
}
func safeStatusAppScope(app *statusApp) string {
	if app == nil {
		return ""
	}
	return app.ScopeID
}
func safeStatusAppName(app *statusApp) string {
	if app == nil {
		return ""
	}
	return app.Name
}

func handleExportCommand(_ context.Context, c *Client, args []string) (bool, error) {
	jsonOutput, pathArg := false, ""
	for _, arg := range args {
		if arg == "--json" {
			if jsonOutput {
				return true, fmt.Errorf("usage: /export [path] [--json]")
			}
			jsonOutput = true
		} else if pathArg == "" {
			pathArg = arg
		} else {
			return true, fmt.Errorf("usage: /export [path] [--json]")
		}
	}
	result, err := c.createConversationExport(pathArg)
	if err != nil {
		return true, err
	}
	if jsonOutput {
		raw, _ := json.Marshal(result)
		slashCommandPrintln(string(raw))
		return true, nil
	}
	slashCommandPrintf("export: %s (%d bytes, %d members)\n", result.Path, result.Bytes, result.MemberCount)
	return true, nil
}

func (c *Client) createConversationExport(pathArg string) (conversationExportResult, error) {
	now := statusNow().UTC()
	payloads, manifest, err := c.conversationExportPayloads(now)
	if err != nil {
		return conversationExportResult{}, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return conversationExportResult{}, err
	}
	payloads["manifest.json"] = raw
	path, local, err := conversationExportDestination(c.opts.Profile, pathArg, now)
	if err != nil {
		return conversationExportResult{}, err
	}
	if local {
		if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
			return conversationExportResult{}, err
		}
	}
	if err := rejectUnsafeBundleDestination(path); err != nil {
		return conversationExportResult{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".export-*")
	if err != nil {
		return conversationExportResult{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return conversationExportResult{}, err
	}
	if err := writeDeterministicConversationExportZIP(tmp, payloads, now); err != nil {
		tmp.Close()
		return conversationExportResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return conversationExportResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return conversationExportResult{}, err
	}
	if err := os.Link(tmpName, path); err != nil {
		if os.IsExist(err) {
			return conversationExportResult{}, fmt.Errorf("export destination already exists")
		}
		return conversationExportResult{}, err
	}
	if err := os.Remove(tmpName); err != nil {
		return conversationExportResult{}, err
	}
	if err := syncSupportBundleDir(filepath.Dir(path)); err != nil {
		return conversationExportResult{}, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return conversationExportResult{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return conversationExportResult{}, err
	}
	return conversationExportResult{ExportSchemaVersion, "zip", conversationExportDisplayPath(path, local), info.Size(), len(payloads)}, nil
}

func (c *Client) conversationExportPayloads(now time.Time) (map[string][]byte, conversationExportManifest, error) {
	status, turn := c.statusDocument(), c.turnDocument()
	entries, journal := readTurnJournal(c.opts.Profile, c.workspaceName, turn.Journal.Checkpoint)
	scope := c.exportScope(status, turn)
	events := selectExportEvents(entries, journal, scope)
	contextDoc := struct {
		SchemaVersion  int             `json:"schemaVersion"`
		Workspace      string          `json:"workspace"`
		ConversationID string          `json:"conversationId,omitempty"`
		App            *statusApp      `json:"app,omitempty"`
		Snapshot       string          `json:"snapshot"`
		Goals          statusGoals     `json:"goals"`
		Approvals      statusApprovals `json:"approvals"`
	}{ExportSchemaVersion, scope.Workspace, scope.ConversationID, status.Context.App, exportSnapshotLabel(status, turn), status.Goals, status.Approvals}
	journalDoc := struct {
		SchemaVersion int             `json:"schemaVersion"`
		Health        turnJournalInfo `json:"health"`
		Included      int             `json:"includedEvents"`
		Limit         int             `json:"eventLimit"`
	}{ExportSchemaVersion, journal, len(events), exportEventLimit}
	configDoc := struct {
		SchemaVersion int    `json:"schemaVersion"`
		Transport     string `json:"transport"`
		Runtime       string `json:"runtime"`
		Model         string `json:"model"`
	}{ExportSchemaVersion, safeSnapshotString(status.Transport.Mode), safeSnapshotString(status.Runtime.Provider), safeSnapshotString(status.Runtime.Model)}
	values := map[string]interface{}{"config.json": configDoc, "context.json": contextDoc, "events.json": events, "journal.json": journalDoc, "messages.json": c.exportMessageMetadata(scope), "status.json": status, "turn.json": turn}
	payloads := map[string][]byte{}
	members := make([]supportBundleMember, 0, len(values))
	for name, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, conversationExportManifest{}, err
		}
		if err := conversationExportSafeBytes(name, raw); err != nil {
			return nil, conversationExportManifest{}, err
		}
		payloads[name] = raw
		sum := sha256.Sum256(raw)
		members = append(members, supportBundleMember{name, hex.EncodeToString(sum[:]), int64(len(raw))})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	if totalPayloadBytes(payloads) > exportMaxBytes {
		return nil, conversationExportManifest{}, fmt.Errorf("export exceeds local size limit")
	}
	manifest := conversationExportManifest{ExportSchemaVersion, "zip", now.Format(time.RFC3339Nano), safeSnapshotString(status.Profile.Name), scope.Workspace, scope.ConversationID, exportSnapshotLabel(status, turn), scope, members, "allowlisted_redacted_metadata_only_local"}
	return payloads, manifest, nil
}

func (c *Client) exportScope(status runtimeStatusDocument, turn turnDocument) exportScope {
	state, snapshot, active, _, _ := c.turnReadState()
	scope := exportScope{Kind: "no_turn", Workspace: safeSnapshotString(status.Context.Workspace), ConversationID: safeSnapshotString(status.Context.ConversationID), MessagePolicy: "current_history_only_when_conversation_identity_is_not_provable"}
	if active && snapshot != nil {
		scope.Kind, scope.Workspace, scope.ConversationID, scope.TurnID = "active_turn", safeSnapshotString(snapshot.Workspace.Name), safeSnapshotString(snapshot.ConversationID), safeSnapshotString(state.TurnID)
		return scope
	}
	if state.Status != SemanticTurnIdle && state.TurnID != "" {
		scope.Kind, scope.TurnID = "last_turn", safeSnapshotString(state.TurnID)
		if state.Context.Workspace != "" {
			scope.Workspace = safeSnapshotString(state.Context.Workspace)
		}
		if state.Context.ConversationID != "" {
			scope.ConversationID = safeSnapshotString(state.Context.ConversationID)
		}
	}
	return scope
}

func selectExportEvents(entries []SemanticJournalEnvelope, journal turnJournalInfo, scope exportScope) []debugEvent {
	if journal.Health == "corrupt" || scope.Workspace == "" || (scope.Kind != "no_turn" && scope.TurnID == "") {
		return []debugEvent{}
	}
	out := []debugEvent{}
	for _, entry := range entries {
		if entry.Workspace != scope.Workspace {
			continue
		}
		event := redactedDebugEvent(entry)
		if scope.TurnID != "" {
			if event.TurnID != scope.TurnID {
				continue
			}
		} else if scope.ConversationID == "" || !eventMatchesConversation(event, scope.ConversationID) {
			continue
		}
		out = append(out, event)
	}
	if len(out) > exportEventLimit {
		out = out[len(out)-exportEventLimit:]
	}
	return out
}
func eventMatchesConversation(event debugEvent, conversationID string) bool {
	if event.Details == nil {
		return false
	}
	value, _ := event.Details["conversationId"].(string)
	return safeSnapshotString(value) == conversationID
}
func exportSnapshotLabel(status runtimeStatusDocument, turn turnDocument) string {
	if status.Turn.Active || turn.Availability == "active" {
		return "immutable_active_snapshot"
	}
	return "current_local_snapshot"
}
func (c *Client) exportMessageMetadata(scope exportScope) []exportMessageMetadata {
	// An active turn exports the accepted immutable snapshot rather than mutable
	// client history, keeping all conversation metadata aligned with that turn.
	c.activeTurnMu.Lock()
	history := append([]interface{}(nil), c.history...)
	if c.processing && c.turnRuntimeSnapshot != nil {
		history = append([]interface{}(nil), c.turnRuntimeSnapshot.ConversationHistory...)
	}
	c.activeTurnMu.Unlock()
	out := make([]exportMessageMetadata, 0, len(history))
	for i, raw := range history {
		if len(out) >= exportMessageLimit {
			break
		}
		msg := asMap(raw)
		if msg == nil {
			continue
		}
		messageConversation := safeSnapshotString(firstString(msg, "conversationId", "conversation_id", "conversationID"))
		if messageConversation != "" && messageConversation != scope.ConversationID {
			continue
		}
		role := safeSnapshotString(strings.ToLower(strings.TrimSpace(firstString(msg, "role", "author", "sender", "type"))))
		text := historyMessageText(msg)
		id := safeSnapshotString(firstString(msg, "id", "sys_id", "messageId"))
		if id == "" {
			id = fmt.Sprintf("message-%04d", i+1)
		}
		occurred := safeSnapshotString(firstString(msg, "createdAt", "created_at", "timestamp", "sys_created_on"))
		contentHash := safeMessageContentHash(text)
		messageScope := "current_history_unproven"
		if messageConversation != "" {
			messageScope = "conversation_tagged"
		}
		out = append(out, exportMessageMetadata{ID: id, Role: role, OccurredAt: occurred, ContentHash: contentHash, ContentBytes: len(text), Content: "[omitted]", Scope: messageScope})
	}
	return out
}
func safeMessageContentHash(text string) string {
	text = strings.TrimSpace(text)
	if len(text) < 64 || credentialValuePattern.MatchString(text) || snapshotCredentialPattern.MatchString(text) {
		return ""
	}
	sum := sha256.Sum256([]byte("ba-cli-export-content-v1:\x00" + text))
	return "sha256:" + hex.EncodeToString(sum[:])
}
func conversationExportDestination(profile, arg string, now time.Time) (string, bool, error) {
	if arg == "" {
		base := filepath.Join(profileDir(profile), "exports")
		name := "export-" + now.Format("20060102T150405.000000000Z") + ".zip"
		for i := 0; ; i++ {
			candidate := filepath.Join(base, name)
			if i > 0 {
				candidate = filepath.Join(base, strings.TrimSuffix(name, ".zip")+fmt.Sprintf("-%d.zip", i))
			}
			if _, err := os.Lstat(candidate); os.IsNotExist(err) {
				return candidate, true, nil
			} else if err != nil {
				return "", false, err
			}
		}
	}
	if filepath.IsAbs(arg) || strings.Contains(arg, "\x00") {
		return "", false, fmt.Errorf("export path must be a safe relative .zip path")
	}
	clean := filepath.Clean(arg)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || filepath.Ext(clean) != ".zip" {
		return "", false, fmt.Errorf("export path must be a safe relative .zip path")
	}
	for _, component := range strings.Split(filepath.ToSlash(clean), "/") {
		if component == "." || !supportBundlePathComponentRE.MatchString(component) || !supportBundlePathComponentSafe(component) {
			return "", false, fmt.Errorf("export path must be a safe relative .zip path")
		}
	}
	return filepath.Join(".", clean), false, nil
}
func conversationExportDisplayPath(path string, local bool) string {
	if local {
		return filepath.ToSlash(filepath.Join("exports", filepath.Base(path)))
	}
	return filepath.ToSlash(path)
}
func writeDeterministicConversationExportZIP(w io.Writer, payloads map[string][]byte, when time.Time) error {
	names := make([]string, 0, len(payloads))
	for name := range payloads {
		if name != "manifest.json" && !strings.HasSuffix(name, ".json") {
			return fmt.Errorf("unsupported export member")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if totalPayloadBytes(payloads) > exportMaxBytes {
		return fmt.Errorf("export exceeds local size limit")
	}
	zw := zip.NewWriter(w)
	for _, name := range names {
		raw := payloads[name]
		if err := conversationExportSafeBytes(name, raw); err != nil {
			zw.Close()
			return err
		}
		h := &zip.FileHeader{Name: name, Method: zip.Store, Modified: when.UTC()}
		h.SetMode(0o600)
		entry, err := zw.CreateHeader(h)
		if err != nil {
			zw.Close()
			return err
		}
		if _, err = entry.Write(raw); err != nil {
			zw.Close()
			return err
		}
	}
	return zw.Close()
}
func conversationExportSafeBytes(name string, raw []byte) error {
	if err := supportBundleSafeBytes(name, raw); err != nil {
		return err
	}
	lower := strings.ToLower(string(raw))
	for _, canary := range []string{"/home/", "prompt", "assistant content", "attachment", "raw journal", "raw frame", "session.json"} {
		if strings.Contains(lower, canary) {
			return fmt.Errorf("unsafe export material omitted")
		}
	}
	return nil
}
