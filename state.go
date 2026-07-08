package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultWorkspaceName = "default"
)

type ProfileInfo struct {
	Name        string
	InstanceURL string
	WSURL       string
	HasToken    bool
	HasSession  bool
}

type AppScope struct {
	ScopeID   string `json:"scopeId"`
	ScopeName string `json:"scopeName,omitempty"`
	AppSysID  string `json:"appSysId,omitempty"`
}

type WorkspaceState struct {
	Name                    string               `json:"name"`
	WebWorkspaceURI         string               `json:"webWorkspaceUri,omitempty"`
	WebWorkspaceChecksum    string               `json:"webWorkspaceChecksum,omitempty"`
	WebWorkspaceDescription string               `json:"webWorkspaceDescription,omitempty"`
	WebWorkspaceFolders     []WebWorkspaceFolder `json:"webWorkspaceFolders,omitempty"`
	ConversationID          string               `json:"conversationId,omitempty"`
	ConversationTitle       string               `json:"conversationTitle,omitempty"`
	ConversationState       string               `json:"conversationState,omitempty"`
	ServerConversation      bool                 `json:"serverConversation,omitempty"`
	ConversationHistory     []interface{}        `json:"conversationHistory,omitempty"`
	UsageInputTokens        int64                `json:"usageInputTokens,omitempty"`
	UsageOutputTokens       int64                `json:"usageOutputTokens,omitempty"`
	UsageThinkingTokens     int64                `json:"usageThinkingTokens,omitempty"`
	WorkingSet              interface{}          `json:"workingSet,omitempty"`
	AppScope                interface{}          `json:"appScope,omitempty"`
	App                     *AppScope            `json:"app,omitempty"`
	CreatedAt               string               `json:"createdAt"`
	UpdatedAt               string               `json:"updatedAt"`
}

func profileManagementRequested(opts Options) bool {
	return opts.ProfileList || opts.ProfileDelete != ""
}

func handleInstanceFlags(opts Options) (bool, error) {
	if opts.InstanceList {
		instances, err := listProfiles()
		if err != nil {
			return true, err
		}
		if len(instances) == 0 {
			fmt.Fprintln(os.Stderr, "no instances configured")
			return true, nil
		}
		active := readActiveInstanceProfile()
		fmt.Fprintln(os.Stderr, "instances:")
		for _, instance := range instances {
			marker := " "
			if instance.Name == active {
				marker = "*"
			}
			creds := []string{}
			if instance.HasToken {
				creds = append(creds, "oauth")
			}
			if instance.HasSession {
				creds = append(creds, "web-session")
			}
			credText := "no-credentials"
			if len(creds) > 0 {
				credText = strings.Join(creds, ",")
			}
			fmt.Fprintf(os.Stderr, "%s %-24s %s  [%s]\n", marker, instance.Name, instance.InstanceURL, credText)
		}
		return true, nil
	}

	if opts.InstanceDelete != "" {
		profile, err := resolveInstanceProfile(opts.InstanceDelete)
		if err != nil {
			return true, err
		}
		if err := deleteConfiguredInstance(profile); err != nil {
			return true, err
		}
		fmt.Fprintf(os.Stderr, "deleted instance %q\n", profile)
		return true, nil
	}

	return false, nil
}

func selectStartupInstance(opts *Options) error {
	if opts.ProfileExplicit || opts.Setup || opts.InstanceURL != "" || opts.InstanceList || opts.InstanceDelete != "" {
		return nil
	}
	instances, err := listProfiles()
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		return nil
	}
	active := readActiveInstanceProfile()
	selected := instances[0].Name
	for _, instance := range instances {
		if instance.Name == active {
			selected = instance.Name
			break
		}
	}
	if len(instances) == 1 || len(opts.Prompts) > 0 {
		opts.Profile = selected
		return nil
	}
	choice, err := promptInstanceSelection(instances, selected)
	if err != nil {
		return err
	}
	choice = strings.TrimSpace(choice)
	if choice == "" || choice == "__cancel__" {
		return errors.New("instance selection canceled")
	}
	opts.Profile = choice
	return saveActiveInstanceProfile(choice)
}

func handleProfileFlags(opts Options) (bool, error) {
	if opts.ProfileList {
		profiles, err := listProfiles()
		if err != nil {
			return true, err
		}
		if len(profiles) == 0 {
			fmt.Fprintln(os.Stderr, "no profiles configured")
			return true, nil
		}
		fmt.Fprintln(os.Stderr, "profiles:")
		for _, p := range profiles {
			marker := " "
			if p.Name == opts.Profile {
				marker = "*"
			}
			token := ""
			if p.HasToken {
				token = " token"
			}
			fmt.Fprintf(os.Stderr, "%s %s  %s%s\n", marker, p.Name, p.InstanceURL, token)
		}
		return true, nil
	}

	if opts.ProfileDelete != "" {
		if err := deleteProfile(opts.ProfileDelete, opts.Profile); err != nil {
			return true, err
		}
		fmt.Fprintf(os.Stderr, "deleted profile %q\n", opts.ProfileDelete)
		return true, nil
	}

	return false, nil
}

func listProfiles() ([]ProfileInfo, error) {
	dir := filepath.Join(stateDir(), "profiles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	profiles := make([]ProfileInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		cfg, ok := loadProfileConfig(name)
		if !ok || cfg.InstanceURL == "" {
			continue
		}
		_, tokenErr := os.Stat(fileTokenPath(name))
		_, sessionErr := os.Stat(sessionFile(name))
		profiles = append(profiles, ProfileInfo{
			Name:        name,
			InstanceURL: cfg.InstanceURL,
			WSURL:       cfg.WSURL,
			HasToken:    tokenErr == nil,
			HasSession:  sessionErr == nil,
		})
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

func resolveInstanceProfile(selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", errors.New("instance selector is required")
	}
	instances, err := listProfiles()
	if err != nil {
		return "", err
	}
	cleaned := cleanInstanceURL(selector)
	selectorHost := instanceHost(selector)
	var matches []ProfileInfo
	for _, instance := range instances {
		if instance.Name == selector || sameInstance(instance.InstanceURL, cleaned) || instanceHost(instance.InstanceURL) == selectorHost {
			matches = append(matches, instance)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("configured instance %q not found", selector)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("configured instance %q is ambiguous; use profile name", selector)
	}
	return matches[0].Name, nil
}

func instanceHost(raw string) string {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return ""
	}
	if !strings.Contains(cleaned, "://") {
		cleaned = "https://" + cleaned
	}
	cleaned = cleanInstanceURL(cleaned)
	cleaned = strings.TrimPrefix(cleaned, "https://")
	cleaned = strings.TrimPrefix(cleaned, "http://")
	if slash := strings.Index(cleaned, "/"); slash >= 0 {
		cleaned = cleaned[:slash]
	}
	if host, _, ok := strings.Cut(cleaned, ":"); ok {
		cleaned = host
	}
	return strings.ToLower(cleaned)
}

func deleteConfiguredInstance(profile string) error {
	if !isValidProfile(profile) {
		return fmt.Errorf("invalid profile %q: use letters, numbers, dash or underscore", profile)
	}
	if _, err := os.Stat(profileDir(profile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("instance/profile %q does not exist", profile)
		}
		return err
	}
	if err := os.RemoveAll(profileDir(profile)); err != nil {
		return err
	}
	if readActiveInstanceProfile() == profile {
		_ = clearActiveInstanceProfile()
	}
	return nil
}

func activeInstanceProfileFile() string {
	return filepath.Join(stateDir(), "active-instance")
}

func readActiveInstanceProfile() string {
	raw, err := os.ReadFile(activeInstanceProfileFile())
	if err != nil {
		return ""
	}
	profile := strings.TrimSpace(string(raw))
	if !isValidProfile(profile) {
		return ""
	}
	return profile
}

func saveActiveInstanceProfile(profile string) error {
	if !isValidProfile(profile) {
		return fmt.Errorf("invalid profile %q: use letters, numbers, dash or underscore", profile)
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(activeInstanceProfileFile(), []byte(profile+"\n"), 0o600)
}

func clearActiveInstanceProfile() error {
	err := os.Remove(activeInstanceProfileFile())
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func deleteProfile(name, activeProfile string) error {
	if !isValidProfile(name) {
		return fmt.Errorf("invalid profile %q: use letters, numbers, dash or underscore", name)
	}
	if name == activeProfile {
		return fmt.Errorf("refusing to delete active profile %q; pass --profile <other> or delete a different profile", name)
	}
	dir := profileDir(name)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("profile %q does not exist", name)
		}
		return err
	}
	return os.RemoveAll(dir)
}

func activeWorkspaceFile(profile string) string {
	return filepath.Join(profileDir(profile), "active-workspace")
}

func workspacesDir(profile string) string {
	return filepath.Join(profileDir(profile), "workspaces")
}

func workspaceFile(profile, name string) string {
	return filepath.Join(workspacesDir(profile), name+".json")
}

func activeAppFile(profile string) string {
	return filepath.Join(profileDir(profile), "active-app.json")
}

func readActiveWorkspaceName(profile string) string {
	raw, err := os.ReadFile(activeWorkspaceFile(profile))
	if err != nil {
		return defaultWorkspaceName
	}
	name := strings.TrimSpace(string(raw))
	if !isValidWorkspaceName(name) {
		return defaultWorkspaceName
	}
	return name
}

func saveActiveWorkspaceName(profile, name string) error {
	if !isValidWorkspaceName(name) {
		return fmt.Errorf("invalid workspace %q: use a non-empty name without path separators or control characters", name)
	}
	if err := os.MkdirAll(profileDir(profile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(activeWorkspaceFile(profile), []byte(name+"\n"), 0o600)
}

func isValidWorkspaceName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 240 {
		return false
	}
	if strings.ContainsAny(name, `/\\`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func loadWorkspace(profile, name string) (WorkspaceState, bool) {
	if !isValidWorkspaceName(name) {
		return WorkspaceState{}, false
	}
	raw, err := os.ReadFile(workspaceFile(profile, name))
	if err != nil {
		return WorkspaceState{}, false
	}
	var ws WorkspaceState
	if err := json.Unmarshal(raw, &ws); err != nil {
		return WorkspaceState{}, false
	}
	if ws.Name == "" {
		ws.Name = name
	}
	return ws, true
}

func newWorkspaceState(name string) WorkspaceState {
	now := time.Now().UTC().Format(time.RFC3339)
	return WorkspaceState{Name: name, CreatedAt: now, UpdatedAt: now}
}

func saveWorkspace(profile string, ws WorkspaceState) error {
	if !isValidWorkspaceName(ws.Name) {
		return fmt.Errorf("invalid workspace %q: use a non-empty name without path separators or control characters", ws.Name)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if ws.CreatedAt == "" {
		ws.CreatedAt = now
	}
	ws.UpdatedAt = now
	if err := os.MkdirAll(workspacesDir(profile), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(ws, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(workspaceFile(profile, ws.Name), append(raw, '\n'), 0o600)
}

func listWorkspaces(profile string) ([]WorkspaceState, error) {
	entries, err := os.ReadDir(workspacesDir(profile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	workspaces := make([]WorkspaceState, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		if ws, ok := loadWorkspace(profile, name); ok {
			workspaces = append(workspaces, ws)
		}
	}
	sort.Slice(workspaces, func(i, j int) bool { return workspaces[i].Name < workspaces[j].Name })
	return workspaces, nil
}

func deleteWorkspace(profile, active, name string) error {
	if !isValidWorkspaceName(name) {
		return fmt.Errorf("invalid workspace %q: use a non-empty name without path separators or control characters", name)
	}
	if name == active {
		return fmt.Errorf("refusing to delete active workspace %q; switch or reset it instead", name)
	}
	path := workspaceFile(profile, name)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("workspace %q does not exist", name)
		}
		return err
	}
	return os.Remove(path)
}

func saveActiveApp(profile string, app AppScope) error {
	if strings.TrimSpace(app.ScopeID) == "" {
		return errors.New("scope id is required")
	}
	if err := os.MkdirAll(profileDir(profile), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(app, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(activeAppFile(profile), append(raw, '\n'), 0o600)
}

func loadActiveApp(profile string) (*AppScope, bool) {
	raw, err := os.ReadFile(activeAppFile(profile))
	if err != nil {
		return nil, false
	}
	var app AppScope
	if err := json.Unmarshal(raw, &app); err != nil || strings.TrimSpace(app.ScopeID) == "" {
		return nil, false
	}
	return &app, true
}

func deleteActiveApp(profile string) error {
	err := os.Remove(activeAppFile(profile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func appFromPayload(payload map[string]interface{}) *AppScope {
	scopeID := stringify(payload["scopeId"])
	if scopeID == "" {
		scopeID = stringify(payload["scope_id"])
	}
	if scopeID == "" {
		return nil
	}
	app := &AppScope{
		ScopeID:   scopeID,
		ScopeName: stringify(payload["scopeName"]),
		AppSysID:  stringify(payload["appSysId"]),
	}
	if app.ScopeName == "" {
		app.ScopeName = stringify(payload["scope_name"])
	}
	if app.AppSysID == "" {
		app.AppSysID = stringify(payload["app_sys_id"])
	}
	if app.ScopeName == "" {
		app.ScopeName = scopeID
	}
	return app
}
