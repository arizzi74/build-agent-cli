package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var errWebWorkspacesUnavailable = errors.New("web workspace API unavailable")

var gliderUserIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\buserId\s*:\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`"userId"\s*:\s*"([^"]+)"`),
}

type WebWorkspaceFolder struct {
	Name string `json:"name,omitempty"`
	URI  string `json:"uri,omitempty"`
}

type WebWorkspace struct {
	Name        string
	URI         string
	Path        string
	Checksum    string
	Size        string
	CreatedAt   string
	UpdatedAt   string
	Description string
	Folders     []WebWorkspaceFolder
}

type WorkspaceChoice struct {
	Name           string
	Value          string
	Label          string
	Current        bool
	Source         string
	WebWorkspace   WebWorkspace
	LocalWorkspace WorkspaceState
}

func (c *Client) PromptWorkspaceSelection(ctx context.Context) error {
	if c.processing {
		return errors.New("cannot switch workspace while a turn is processing")
	}
	choices, err := c.ListWorkspaceChoices(ctx)
	if err != nil {
		return err
	}
	return c.SelectWorkspace(ctx, choices)
}

func (c *Client) SelectWorkspace(ctx context.Context, choices []WorkspaceChoice) error {
	for {
		answer, err := promptWorkspaceSelection(choices)
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" || answer == "__cancel__" || strings.EqualFold(answer, "q") || strings.EqualFold(answer, "cancel") {
			fmt.Fprintln(os.Stderr, "workspace unchanged")
			return nil
		}
		choice, ok := workspaceChoiceBySelection(choices, answer)
		if !ok {
			fmt.Fprintln(os.Stderr, "unknown workspace; enter a list number, name/prefix, or q")
			continue
		}
		if err := c.UseWorkspaceChoice(ctx, choice); err != nil {
			return err
		}
		return c.PromptWebConversation(ctx, "workspace")
	}
}

func (c *Client) UseWorkspaceChoice(ctx context.Context, choice WorkspaceChoice) error {
	switch choice.Source {
	case "web":
		return c.SwitchWebWorkspace(ctx, choice.WebWorkspace)
	case "local":
		return c.SwitchWorkspace(choice.LocalWorkspace.Name, false)
	default:
		return fmt.Errorf("unknown workspace source %q", choice.Source)
	}
}

func (c *Client) ListWorkspaceChoices(ctx context.Context) ([]WorkspaceChoice, error) {
	if c.canUseWebWorkspaceAPI() {
		webWorkspaces, err := c.ListWebWorkspaces(ctx)
		if err == nil && len(webWorkspaces) > 0 {
			return c.webWorkspaceChoices(webWorkspaces), nil
		}
		if err != nil && c.debug {
			slashCommandPrintf("warning: could not list web workspaces: %v\n", err)
		}
	}
	localWorkspaces, err := listWorkspaces(c.opts.Profile)
	if err != nil {
		return nil, err
	}
	return c.localWorkspaceChoices(localWorkspaces), nil
}

func (c *Client) webWorkspaceChoices(workspaces []WebWorkspace) []WorkspaceChoice {
	choices := make([]WorkspaceChoice, 0, len(workspaces))
	for _, ws := range workspaces {
		current := (c.workspaceURI != "" && ws.URI == c.workspaceURI) || (c.workspaceURI == "" && ws.Name == c.workspaceName)
		choices = append(choices, WorkspaceChoice{
			Name:         ws.Name,
			Value:        ws.URI,
			Label:        webWorkspaceChoiceLabel(ws),
			Current:      current,
			Source:       "web",
			WebWorkspace: ws,
		})
	}
	return choices
}

func (c *Client) localWorkspaceChoices(workspaces []WorkspaceState) []WorkspaceChoice {
	choices := make([]WorkspaceChoice, 0, len(workspaces))
	for _, ws := range workspaces {
		choices = append(choices, WorkspaceChoice{
			Name:           ws.Name,
			Value:          ws.Name,
			Label:          localWorkspaceChoiceLabel(ws),
			Current:        ws.Name == c.workspaceName,
			Source:         "local",
			LocalWorkspace: ws,
		})
	}
	return choices
}

func (c *Client) canUseWebWorkspaceAPI() bool {
	if strings.TrimSpace(c.cfg.InstanceURL) == "" {
		return false
	}
	if c.gatewayAuth == authModeBasic && c.basicUser != "" {
		return true
	}
	if c.sessionCookieHeader != "" {
		return true
	}
	if _, ok := loadMatchingWebSession(c.opts.Profile, c.cfg.InstanceURL); ok {
		return true
	}
	if c.opts.Nirvana && c.nirvanaRESTAccessToken() != "" {
		return true
	}
	return false
}

func (c *Client) ListWebWorkspaces(ctx context.Context) ([]WebWorkspace, error) {
	if !c.canUseWebWorkspaceAPI() {
		return nil, errWebWorkspacesUnavailable
	}
	if err := c.ensureConversationHTTPClient(); err != nil {
		return nil, err
	}
	userID, err := c.discoverGliderUserID(ctx)
	if err != nil {
		return nil, err
	}
	body, _, err := c.postWebJSON(ctx, strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/sn_glider/v2/sync/state", map[string]interface{}{
		"uris": []string{"settings:/users/" + userID},
	})
	if err != nil {
		return nil, err
	}
	return parseWebWorkspaceState(body)
}

func (c *Client) SwitchWebWorkspace(ctx context.Context, ws WebWorkspace) error {
	if c.processing {
		return errors.New("cannot switch workspace while a turn is processing")
	}
	if !isValidWorkspaceName(ws.Name) {
		return fmt.Errorf("invalid workspace %q: use a non-empty name without path separators or control characters", ws.Name)
	}
	loaded := ws
	if err := c.loadWebWorkspaceContent(ctx, &loaded); err != nil && c.debug {
		fmt.Fprintf(slashCommandOutputWriter, "warning: could not read workspace file %s: %v\n", ws.URI, err)
	}
	if c.workspaceName != "" {
		if err := c.saveCurrentState(); err != nil {
			return err
		}
	}
	state, ok := loadWorkspace(c.opts.Profile, loaded.Name)
	if !ok {
		state = newWorkspaceState(loaded.Name)
	}
	state.WebWorkspaceURI = loaded.URI
	state.WebWorkspaceChecksum = loaded.Checksum
	state.WebWorkspaceDescription = loaded.Description
	state.WebWorkspaceFolders = append([]WebWorkspaceFolder(nil), loaded.Folders...)
	if len(loaded.Folders) > 0 {
		state.WorkingSet = webWorkspaceWorkingSet(loaded.Folders)
		if app := appFromWebWorkspaceFolders(loaded.Folders); app != nil {
			state.App = app
			state.AppScope = app.ScopeID
		}
	}
	c.applyWorkspace(state)
	if err := saveActiveWorkspaceName(c.opts.Profile, loaded.Name); err != nil {
		return err
	}
	if err := c.saveCurrentState(); err != nil {
		return err
	}
	c.drawPersistentStatus()
	slashCommandPrintf("workspace: %s\n", loaded.Name)
	if loaded.URI != "" {
		slashCommandPrintf("workspace uri: %s\n", loaded.URI)
	}
	if c.currentApp != nil {
		printApp(c.currentApp)
	}
	return nil
}

func (c *Client) discoverGliderUserID(ctx context.Context) (string, error) {
	if userID := userIDFromWorkspaceURI(c.workspaceURI); userID != "" {
		return userID, nil
	}
	body, _, status, err := c.getRaw(ctx, strings.TrimRight(c.cfg.InstanceURL, "/")+"/sn_glider_app/ide.do", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("glider IDE page returned status %d", status)
	}
	if userID := parseGliderUserID(body); userID != "" {
		return userID, nil
	}
	return "", errors.New("could not find window.sn_glider.user.userId in Glider IDE page")
}

func parseGliderUserID(body []byte) string {
	for _, pattern := range gliderUserIDPatterns {
		match := pattern.FindSubmatch(body)
		if len(match) >= 2 {
			return strings.TrimSpace(string(match[1]))
		}
	}
	return ""
}

func userIDFromWorkspaceURI(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	path := strings.TrimPrefix(raw, "settings:")
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path, _ = url.PathUnescape(path)
	const prefix = "/users/"
	idx := strings.Index(path, prefix)
	if idx < 0 {
		return ""
	}
	path = path[idx+len(prefix):]
	if slash := strings.Index(path, "/"); slash >= 0 {
		path = path[:slash]
	}
	return strings.TrimSpace(path)
}

func parseWebWorkspaceState(body []byte) ([]WebWorkspace, error) {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	files := syncStateFiles(root)
	workspaces := make([]WebWorkspace, 0, len(files))
	for _, item := range files {
		m := asMap(item)
		if m == nil {
			continue
		}
		ws, ok := webWorkspaceFromStateFile(m)
		if ok {
			workspaces = append(workspaces, ws)
		}
	}
	sort.Slice(workspaces, func(i, j int) bool { return strings.ToLower(workspaces[i].Name) < strings.ToLower(workspaces[j].Name) })
	return workspaces, nil
}

func syncStateFiles(v interface{}) []interface{} {
	switch typed := v.(type) {
	case map[string]interface{}:
		if files, ok := typed["files"].([]interface{}); ok {
			return files
		}
		if result, ok := typed["result"]; ok {
			return syncStateFiles(result)
		}
	case []interface{}:
		return typed
	}
	return nil
}

func webWorkspaceFromStateFile(m map[string]interface{}) (WebWorkspace, bool) {
	if !strings.EqualFold(strings.TrimSpace(stringify(m["type"])), "file") {
		return WebWorkspace{}, false
	}
	rawURI := strings.TrimSpace(firstString(m, "uri"))
	path := settingsURIPath(rawURI)
	if rawURI == "" || path == "" || !strings.Contains(path, "/workspaces/") || !strings.HasSuffix(path, ".code-workspace") {
		return WebWorkspace{}, false
	}
	name := path[strings.LastIndex(path, "/")+1:]
	name = strings.TrimSuffix(name, ".code-workspace")
	if !isValidWorkspaceName(name) {
		return WebWorkspace{}, false
	}
	return WebWorkspace{
		Name:      name,
		URI:       rawURI,
		Path:      path,
		Checksum:  strings.TrimSpace(firstString(m, "checksum")),
		Size:      strings.TrimSpace(firstString(m, "size")),
		CreatedAt: strings.TrimSpace(firstString(m, "ctime")),
		UpdatedAt: strings.TrimSpace(firstString(m, "mtime")),
	}, true
}

func settingsURIPath(rawURI string) string {
	rawURI = strings.TrimSpace(rawURI)
	if !strings.HasPrefix(rawURI, "settings:") {
		return ""
	}
	path := strings.TrimPrefix(rawURI, "settings:")
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path, _ = url.PathUnescape(path)
	return path
}

func resolveWebWorkspaceSelector(workspaces []WebWorkspace, selector string) (WebWorkspace, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return WebWorkspace{}, errors.New("workspace selector is required")
	}
	if idx, err := strconv.Atoi(selector); err == nil {
		if idx < 1 || idx > len(workspaces) {
			return WebWorkspace{}, fmt.Errorf("workspace number %d is out of range", idx)
		}
		return workspaces[idx-1], nil
	}
	var matches []WebWorkspace
	for _, ws := range workspaces {
		if strings.EqualFold(ws.Name, selector) || strings.EqualFold(ws.URI, selector) {
			return ws, nil
		}
	}
	for _, ws := range workspaces {
		if strings.HasPrefix(strings.ToLower(ws.Name), strings.ToLower(selector)) || strings.HasPrefix(strings.ToLower(ws.URI), strings.ToLower(selector)) {
			matches = append(matches, ws)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return WebWorkspace{}, fmt.Errorf("workspace selector %q is ambiguous", selector)
	}
	return WebWorkspace{}, fmt.Errorf("workspace %q not found", selector)
}

func workspaceChoiceBySelection(choices []WorkspaceChoice, answer string) (WorkspaceChoice, bool) {
	if idx, err := strconv.Atoi(strings.TrimSpace(answer)); err == nil {
		if idx >= 1 && idx <= len(choices) {
			return choices[idx-1], true
		}
		return WorkspaceChoice{}, false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "" {
		return WorkspaceChoice{}, false
	}
	for _, choice := range choices {
		if strings.EqualFold(choice.Name, answer) || strings.EqualFold(choice.Value, answer) || strings.EqualFold(choice.WebWorkspace.URI, answer) {
			return choice, true
		}
	}
	var matched *WorkspaceChoice
	for i := range choices {
		name := strings.ToLower(choices[i].Name)
		value := strings.ToLower(choices[i].Value)
		uri := strings.ToLower(choices[i].WebWorkspace.URI)
		if strings.HasPrefix(name, answer) || strings.HasPrefix(value, answer) || strings.HasPrefix(uri, answer) {
			if matched != nil {
				return WorkspaceChoice{}, false
			}
			matched = &choices[i]
		}
	}
	if matched == nil {
		return WorkspaceChoice{}, false
	}
	return *matched, true
}

func webWorkspaceChoiceLabel(ws WebWorkspace) string {
	label := singleLineLabel(ws.Name)
	if label == "" {
		label = "Untitled workspace"
	}
	parts := []string{label, "[web]"}
	if ws.Description != "" {
		parts = append(parts, ws.Description)
	}
	return strings.Join(parts, "  ")
}

func localWorkspaceChoiceLabel(ws WorkspaceState) string {
	label := singleLineLabel(ws.Name)
	if label == "" {
		label = defaultWorkspaceName
	}
	parts := []string{label, "[local]"}
	if ws.WebWorkspaceURI != "" {
		parts[1] = "[web-cache]"
	}
	if ws.ConversationID != "" {
		parts = append(parts, shortConversationID(ws.ConversationID))
	}
	if ws.App != nil && ws.App.ScopeName != "" {
		parts = append(parts, ws.App.ScopeName)
	}
	return strings.Join(parts, "  ")
}

func printWebWorkspaceList(workspaces []WebWorkspace, activeName, activeURI string) {
	if len(workspaces) == 0 {
		slashCommandPrintln("no web workspaces found")
		return
	}
	slashCommandPrintln("workspaces:")
	for i, ws := range workspaces {
		marker := " "
		if (activeURI != "" && ws.URI == activeURI) || (activeURI == "" && ws.Name == activeName) {
			marker = "*"
		}
		details := ""
		if ws.Description != "" {
			details = "  " + ws.Description
		} else if len(ws.Folders) > 0 {
			details = fmt.Sprintf("  folders=%d", len(ws.Folders))
		}
		slashCommandPrintf("%s %d. %s%s\n", marker, i+1, ws.Name, details)
	}
}

func (c *Client) loadWebWorkspaceContent(ctx context.Context, ws *WebWorkspace) error {
	if ws == nil || ws.URI == "" {
		return nil
	}
	contents, err := c.fetchV2SyncFiles(ctx, []string{ws.URI})
	if err != nil {
		return err
	}
	content := contents[ws.Checksum]
	if len(content) == 0 && len(contents) == 1 {
		for _, one := range contents {
			content = one
		}
	}
	if len(content) == 0 {
		return errors.New("workspace file content was not returned by sync/files")
	}
	description, folders, err := parseWebWorkspaceFileContent(content)
	if err != nil {
		return err
	}
	ws.Description = description
	ws.Folders = folders
	return nil
}

func parseWebWorkspaceFileContent(content []byte) (string, []WebWorkspaceFolder, error) {
	var root interface{}
	if err := json.Unmarshal(content, &root); err != nil {
		return "", nil, err
	}
	m := asMap(root)
	if m == nil {
		return "", nil, errors.New("workspace file is not a JSON object")
	}
	description := strings.TrimSpace(firstString(m, "description"))
	foldersRaw, _ := m["folders"].([]interface{})
	folders := make([]WebWorkspaceFolder, 0, len(foldersRaw))
	for _, item := range foldersRaw {
		fm := asMap(item)
		if fm == nil {
			continue
		}
		folder := WebWorkspaceFolder{
			Name: strings.TrimSpace(firstString(fm, "name")),
			URI:  strings.TrimSpace(firstString(fm, "uri", "path")),
		}
		if folder.Name == "" && folder.URI != "" {
			folder.Name = folder.URI
		}
		if folder.URI != "" {
			folders = append(folders, folder)
		}
	}
	return description, folders, nil
}

func (c *Client) fetchV2SyncFiles(ctx context.Context, uris []string) (map[string][]byte, error) {
	rawURIs, err := json.Marshal(uris)
	if err != nil {
		return nil, err
	}
	body, contentType, _, err := c.postMultipart(ctx, strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/sn_glider/v2/sync/files", func(writer *multipart.Writer) error {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="uris"; filename="blob"`)
		header.Set("Content-Type", "application/octet-stream")
		part, err := writer.CreatePart(header)
		if err != nil {
			return err
		}
		_, err = part.Write(rawURIs)
		return err
	})
	if err != nil {
		return nil, err
	}
	files, err := parseSyncFilesResponse(body, contentType)
	if err != nil {
		return nil, err
	}
	return files, nil
}

func parseSyncFilesResponse(body []byte, contentType string) (map[string][]byte, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err == nil && strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return nil, errors.New("multipart sync/files response missing boundary")
		}
		reader := multipart.NewReader(bytes.NewReader(body), boundary)
		files := map[string][]byte{}
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			name := part.FormName()
			if name == "" {
				name = part.FileName()
			}
			if name == "" || name == "uris" {
				continue
			}
			content, err := io.ReadAll(part)
			if err != nil {
				return nil, err
			}
			files[name] = content
		}
		return files, nil
	}
	return parseSyncFilesJSON(body)
}

func parseSyncFilesJSON(body []byte) (map[string][]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string][]byte{}, nil
	}
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	collectSyncFileJSONContents(root, files)
	return files, nil
}

func collectSyncFileJSONContents(v interface{}, out map[string][]byte) {
	switch typed := v.(type) {
	case map[string]interface{}:
		checksum := strings.TrimSpace(firstString(typed, "checksum", "name"))
		if checksum != "" {
			switch content := typed["content"].(type) {
			case string:
				out[checksum] = []byte(content)
			case []interface{}:
				buf := make([]byte, 0, len(content))
				for _, item := range content {
					if n, ok := item.(float64); ok {
						buf = append(buf, byte(n))
					}
				}
				out[checksum] = buf
			}
		}
		for _, item := range typed {
			collectSyncFileJSONContents(item, out)
		}
	case []interface{}:
		for _, item := range typed {
			collectSyncFileJSONContents(item, out)
		}
	}
}

func webWorkspaceWorkingSet(folders []WebWorkspaceFolder) []map[string]interface{} {
	workingSet := make([]map[string]interface{}, 0, len(folders))
	for _, folder := range folders {
		if strings.TrimSpace(folder.URI) == "" {
			continue
		}
		item := map[string]interface{}{"uri": folder.URI}
		if folder.Name != "" {
			item["name"] = folder.Name
		}
		workingSet = append(workingSet, item)
	}
	return workingSet
}

func appFromWebWorkspaceFolders(folders []WebWorkspaceFolder) *AppScope {
	for _, folder := range folders {
		app, ok := appScopeFromWorkspaceFolder(folder)
		if ok {
			return &app
		}
	}
	return nil
}

func (c *Client) getRaw(ctx context.Context, endpoint, accept string) ([]byte, string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", 0, err
	}
	c.setGliderWebHeaders(req)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return body, res.Header.Get("Content-Type"), res.StatusCode, fmt.Errorf("GET %s failed (%d): %s", endpoint, res.StatusCode, trimBody(body))
	}
	return body, res.Header.Get("Content-Type"), res.StatusCode, nil
}

func (c *Client) postMultipart(ctx context.Context, endpoint string, build func(*multipart.Writer) error) ([]byte, string, int, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := build(writer); err != nil {
		_ = writer.Close()
		return nil, "", 0, err
	}
	if err := writer.Close(); err != nil {
		return nil, "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return nil, "", 0, err
	}
	c.setGliderWebHeaders(req)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "*/*")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return body, res.Header.Get("Content-Type"), res.StatusCode, fmt.Errorf("POST %s failed (%d): %s", endpoint, res.StatusCode, trimBody(body))
	}
	return body, res.Header.Get("Content-Type"), res.StatusCode, nil
}

func (c *Client) postWebJSON(ctx context.Context, endpoint string, payload interface{}) ([]byte, int, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	c.setGliderWebHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return body, res.StatusCode, fmt.Errorf("POST %s failed (%d): %s", endpoint, res.StatusCode, trimBody(body))
	}
	return body, res.StatusCode, nil
}

func (c *Client) setGliderWebHeaders(req *http.Request) {
	c.setGatewayHeaders(req)
	if c.sessionCookieHeader != "" {
		req.Header.Set("Cookie", c.sessionCookieHeader)
		if c.gatewayAuth != authModeBasic {
			req.Header.Del("Authorization")
		}
	}
	if c.userToken != "" {
		req.Header.Set("X-UserToken", c.userToken)
	}
}
