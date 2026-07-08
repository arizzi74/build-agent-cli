package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

type AppChoice struct {
	App     AppScope
	Value   string
	Label   string
	Current bool
	Source  string
}

func (c *Client) PromptAppSelection(ctx context.Context) error {
	if c.processing {
		return errors.New("cannot switch app while a turn is processing")
	}
	choices, err := c.ListWorkspaceAppChoices(ctx)
	if err != nil {
		return err
	}
	return c.SelectApp(ctx, choices)
}

func (c *Client) SelectApp(ctx context.Context, choices []AppChoice) error {
	for {
		answer, err := promptAppSelection(choices)
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" || answer == "__cancel__" || strings.EqualFold(answer, "q") || strings.EqualFold(answer, "cancel") {
			fmt.Fprintln(os.Stderr, "app unchanged")
			return nil
		}
		choice, ok := appChoiceBySelection(choices, answer)
		if !ok {
			fmt.Fprintln(os.Stderr, "unknown app; enter a list number, app id/name/prefix, or q")
			continue
		}
		return c.UseAppChoice(ctx, choice)
	}
}

func (c *Client) UseAppChoice(ctx context.Context, choice AppChoice) error {
	_ = ctx
	if strings.TrimSpace(choice.App.ScopeID) == "" {
		return errors.New("selected app has no scope id")
	}
	if err := c.SetApp(choice.App); err != nil {
		return err
	}
	slashCommandPrintf("app set: %s\n", choice.App.ScopeID)
	printApp(&choice.App)
	return nil
}

func (c *Client) ListWorkspaceAppChoices(ctx context.Context) ([]AppChoice, error) {
	if err := c.ensureWorkspaceFoldersForAppPicker(ctx); err != nil && c.debug {
		slashCommandPrintf("warning: could not refresh workspace apps: %v\n", err)
	}
	folders := c.workspaceFolders
	if len(folders) == 0 {
		folders = workspaceFoldersFromWorkingSet(c.workingSet)
	}
	choices := c.appChoicesFromWorkspaceFolders(folders)
	if len(choices) == 0 && c.currentApp != nil && strings.TrimSpace(c.currentApp.ScopeID) != "" {
		choices = append(choices, AppChoice{
			App:     *c.currentApp,
			Value:   c.currentApp.ScopeID,
			Label:   appChoiceLabel(*c.currentApp, "current"),
			Current: true,
			Source:  "current",
		})
	}
	return choices, nil
}

func (c *Client) ensureWorkspaceFoldersForAppPicker(ctx context.Context) error {
	if len(c.workspaceFolders) > 0 || strings.TrimSpace(c.workspaceURI) == "" || !c.canUseWebWorkspaceAPI() {
		return nil
	}
	ws := WebWorkspace{
		Name:        c.workspaceName,
		URI:         c.workspaceURI,
		Checksum:    c.workspaceChecksum,
		Description: c.workspaceDescription,
		Folders:     append([]WebWorkspaceFolder(nil), c.workspaceFolders...),
	}
	if err := c.loadWebWorkspaceContent(ctx, &ws); err != nil {
		return err
	}
	c.workspaceDescription = ws.Description
	c.workspaceFolders = append([]WebWorkspaceFolder(nil), ws.Folders...)
	if len(ws.Folders) > 0 {
		c.workingSet = webWorkspaceWorkingSet(ws.Folders)
	}
	return c.saveCurrentState()
}

func (c *Client) appChoicesFromWorkspaceFolders(folders []WebWorkspaceFolder) []AppChoice {
	choices := make([]AppChoice, 0, len(folders))
	seen := map[string]struct{}{}
	for _, folder := range folders {
		app, ok := appScopeFromWorkspaceFolder(folder)
		if !ok {
			continue
		}
		key := strings.ToLower(app.AppSysID)
		if key == "" {
			key = strings.ToLower(app.ScopeID)
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		current := c.currentApp != nil && sameAppScope(*c.currentApp, app)
		choices = append(choices, AppChoice{
			App:     app,
			Value:   app.ScopeID,
			Label:   appChoiceLabel(app, "workspace"),
			Current: current,
			Source:  "workspace",
		})
	}
	sort.SliceStable(choices, func(i, j int) bool {
		return strings.ToLower(choices[i].App.ScopeName) < strings.ToLower(choices[j].App.ScopeName)
	})
	return choices
}

func appScopeFromWorkspaceFolder(folder WebWorkspaceFolder) (AppScope, bool) {
	uri := strings.TrimSpace(folder.URI)
	if !strings.HasPrefix(uri, "now-file:") {
		return AppScope{}, false
	}
	id := strings.TrimLeft(strings.TrimPrefix(uri, "now-file:"), "/")
	if i := strings.IndexAny(id, "?#"); i >= 0 {
		id = id[:i]
	}
	if decoded, err := url.PathUnescape(id); err == nil {
		id = decoded
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return AppScope{}, false
	}
	name := singleLineLabel(folder.Name)
	if name == "" {
		name = id
	}
	return AppScope{ScopeID: id, ScopeName: name, AppSysID: id}, true
}

func workspaceFoldersFromWorkingSet(v interface{}) []WebWorkspaceFolder {
	var folders []WebWorkspaceFolder
	var walk func(interface{})
	walk = func(item interface{}) {
		switch typed := item.(type) {
		case []interface{}:
			for _, child := range typed {
				walk(child)
			}
		case []map[string]interface{}:
			for _, child := range typed {
				walk(child)
			}
		case map[string]interface{}:
			uri := strings.TrimSpace(firstString(typed, "uri", "path"))
			name := strings.TrimSpace(firstString(typed, "name", "label", "displayName", "display_name"))
			if uri != "" {
				folders = append(folders, WebWorkspaceFolder{Name: name, URI: uri})
			}
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(v)
	return folders
}

func sameAppScope(a, b AppScope) bool {
	for _, left := range []string{a.AppSysID, a.ScopeID} {
		left = strings.TrimSpace(left)
		if left == "" {
			continue
		}
		for _, right := range []string{b.AppSysID, b.ScopeID} {
			right = strings.TrimSpace(right)
			if right != "" && strings.EqualFold(left, right) {
				return true
			}
		}
	}
	return false
}

func appChoiceBySelection(choices []AppChoice, answer string) (AppChoice, bool) {
	if idx, err := strconv.Atoi(strings.TrimSpace(answer)); err == nil {
		if idx >= 1 && idx <= len(choices) {
			return choices[idx-1], true
		}
		return AppChoice{}, false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "" {
		return AppChoice{}, false
	}
	for _, choice := range choices {
		if strings.EqualFold(choice.App.ScopeID, answer) || strings.EqualFold(choice.App.AppSysID, answer) || strings.EqualFold(choice.App.ScopeName, answer) {
			return choice, true
		}
	}
	var matched *AppChoice
	for i := range choices {
		if strings.HasPrefix(strings.ToLower(choices[i].App.ScopeID), answer) ||
			strings.HasPrefix(strings.ToLower(choices[i].App.AppSysID), answer) ||
			strings.HasPrefix(strings.ToLower(choices[i].App.ScopeName), answer) {
			if matched != nil {
				return AppChoice{}, false
			}
			matched = &choices[i]
		}
	}
	if matched == nil {
		return AppChoice{}, false
	}
	return *matched, true
}

func appChoiceLabel(app AppScope, source string) string {
	name := singleLineLabel(app.ScopeName)
	if name == "" {
		name = app.ScopeID
	}
	parts := []string{name}
	if source != "" {
		parts = append(parts, "["+source+"]")
	}
	if app.AppSysID != "" {
		parts = append(parts, app.AppSysID)
	} else if app.ScopeID != "" {
		parts = append(parts, app.ScopeID)
	}
	return strings.Join(parts, "  ")
}
