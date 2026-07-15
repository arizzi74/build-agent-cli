package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	serviceNowAppTemplateSysID = "c305debeff236210c7c1ffffffffff03"
	fluentSDKVersion           = "4.8.1"
	glideDevDependencyVersion  = "27.0.5"
	serviceNowScopeMaxLength   = 18
)

type appCreationRequest struct {
	AppName        string
	AppDescription string
}

type createdServiceNowApp struct {
	Name           string
	Description    string
	Scope          string
	ScopeID        string
	PackageName    string
	Workspace      WebWorkspace
	WorkspaceJSON  []byte
	WorkspaceFiles []WebWorkspaceFolder
}

type gliderChangeEntry struct {
	Checksum string      `json:"checksum,omitempty"`
	CTime    interface{} `json:"ctime,omitempty"`
	DTime    interface{} `json:"dtime,omitempty"`
	MTime    int64       `json:"mtime"`
	Size     int         `json:"size"`
	Type     string      `json:"type"`
	URI      string      `json:"uri"`
}

type gliderFileBlob struct {
	Path     string
	Checksum string
	Content  []byte
}

func (c *Client) answerCreateNewServiceNowApp(payload map[string]interface{}) (map[string]interface{}, string, error) {
	c.clearTurnStatus()
	req := appCreationRequestFromPayload(payload)
	if req.AppName == "" {
		return map[string]interface{}{"error": "create_new_servicenow_app requires appName", "code": "NO_APP_NAME"}, "error", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	app, err := c.createServiceNowAppLikeWebUI(ctx, req)
	if err != nil {
		return nil, "error", err
	}
	result, err := serviceNowAppCreationResult(app)
	if err != nil {
		return nil, "error", err
	}
	fmt.Fprintf(slashCommandOutputWriter, "created app: %s (%s)\n", app.Name, app.ScopeID)
	return result, "complete", nil
}

func appCreationRequestFromPayload(payload map[string]interface{}) appCreationRequest {
	return appCreationRequest{
		AppName:        singleLineLabel(firstString(payload, "appName", "app_name", "name", "applicationName", "application_name")),
		AppDescription: singleLineLabel(firstString(payload, "appDescription", "app_description", "description", "shortDescription", "short_description")),
	}
}

func (c *Client) createServiceNowAppLikeWebUI(ctx context.Context, req appCreationRequest) (createdServiceNowApp, error) {
	if err := c.ensureConversationHTTPClient(); err != nil {
		return createdServiceNowApp{}, err
	}
	ws, workspaceJSON, err := c.activeWebWorkspaceForAppCreation(ctx)
	if err != nil {
		return createdServiceNowApp{}, err
	}
	scope, scopeID, err := c.createServiceNowApplicationRecord(ctx, req)
	if err != nil {
		return createdServiceNowApp{}, err
	}
	if err := c.refreshGliderStateForCreatedApp(ctx, ws, scopeID); err != nil && c.debug {
		c.debugf("warning: could not refresh Glider state for new app: %v\n", err)
	}
	folders, updatedWorkspaceJSON, err := appendAppFolderToWorkspaceJSON(workspaceJSON, req.AppName, scopeID)
	if err != nil {
		return createdServiceNowApp{}, err
	}
	app := createdServiceNowApp{
		Name:           req.AppName,
		Description:    req.AppDescription,
		Scope:          scope,
		ScopeID:        scopeID,
		PackageName:    packageNameFromAppName(req.AppName),
		Workspace:      ws,
		WorkspaceJSON:  updatedWorkspaceJSON,
		WorkspaceFiles: folders,
	}
	if err := c.applyCreatedAppFiles(ctx, app); err != nil {
		return createdServiceNowApp{}, err
	}
	if err := c.patchConversationApplication(ctx, app); err != nil {
		return createdServiceNowApp{}, err
	}
	if err := c.PatchAppCreatedCheckpoint(ctx, c.lastUserMessageSysID, c.lastUserMessageContent, app.ScopeID, app.Name); err != nil && c.debug {
		c.debugf("warning: could not annotate APP_CREATED checkpoint: %v\n", err)
	}
	c.absorbCreatedApp(app)
	c.postAppSelectionStatus(ctx)
	return app, nil
}

func (c *Client) activeWebWorkspaceForAppCreation(ctx context.Context) (WebWorkspace, []byte, error) {
	if strings.TrimSpace(c.workspaceURI) != "" {
		ws := WebWorkspace{
			Name:        c.workspaceName,
			URI:         c.workspaceURI,
			Checksum:    c.workspaceChecksum,
			Description: c.workspaceDescription,
			Folders:     append([]WebWorkspaceFolder(nil), c.workspaceFolders...),
		}
		content, err := c.fetchWorkspaceFileContent(ctx, ws)
		if err != nil {
			return WebWorkspace{}, nil, err
		}
		if len(ws.Folders) == 0 {
			description, folders, parseErr := parseWebWorkspaceFileContent(content)
			if parseErr == nil {
				ws.Description = description
				ws.Folders = folders
			}
		}
		return ws, content, nil
	}
	workspaces, err := c.ListWebWorkspaces(ctx)
	if err != nil {
		return WebWorkspace{}, nil, fmt.Errorf("could not discover web workspaces for app creation: %w", err)
	}
	if len(workspaces) == 0 {
		return WebWorkspace{}, nil, errors.New("no web workspace found; select/create a Glider workspace before creating an app")
	}
	ws := chooseWorkspaceForAppCreation(workspaces, c.workspaceName)
	content, err := c.fetchWorkspaceFileContent(ctx, ws)
	if err != nil {
		return WebWorkspace{}, nil, err
	}
	description, folders, parseErr := parseWebWorkspaceFileContent(content)
	if parseErr == nil {
		ws.Description = description
		ws.Folders = folders
	}
	c.workspaceName = ws.Name
	c.workspaceURI = ws.URI
	c.workspaceChecksum = ws.Checksum
	c.workspaceDescription = ws.Description
	c.workspaceFolders = append([]WebWorkspaceFolder(nil), ws.Folders...)
	c.workingSet = nil
	_ = saveActiveWorkspaceName(c.opts.Profile, ws.Name)
	return ws, content, nil
}

func chooseWorkspaceForAppCreation(workspaces []WebWorkspace, activeName string) WebWorkspace {
	for _, ws := range workspaces {
		if activeName != "" && strings.EqualFold(ws.Name, activeName) {
			return ws
		}
	}
	for _, ws := range workspaces {
		if strings.EqualFold(ws.Name, "Default - admin") {
			return ws
		}
	}
	return workspaces[0]
}

func (c *Client) fetchWorkspaceFileContent(ctx context.Context, ws WebWorkspace) ([]byte, error) {
	if strings.TrimSpace(ws.URI) == "" {
		return nil, errors.New("active workspace has no Glider URI")
	}
	files, err := c.fetchV2SyncFiles(ctx, []string{ws.URI})
	if err != nil {
		return nil, err
	}
	if ws.Checksum != "" {
		if content := files[ws.Checksum]; len(content) > 0 {
			return content, nil
		}
	}
	if len(files) == 1 {
		for _, content := range files {
			return content, nil
		}
	}
	return nil, fmt.Errorf("workspace file content was not returned for %s", ws.URI)
}

func (c *Client) createServiceNowApplicationRecord(ctx context.Context, req appCreationRequest) (string, string, error) {
	var lastErr error
	for i, scope := range scopeCandidatesForAppName(req.AppName) {
		if i == 0 {
			if exists, err := c.serviceNowScopeExists(ctx, scope); err == nil && exists {
				lastErr = fmt.Errorf("scope %s already exists", scope)
				continue
			}
		}
		instanceID, err := c.startServiceNowAppTemplate(ctx, req, scope)
		if err != nil {
			lastErr = err
			continue
		}
		status, err := c.pollServiceNowAppTemplate(ctx, instanceID)
		if err != nil {
			lastErr = err
			if status.ScopeCollision {
				continue
			}
			return "", "", err
		}
		if status.AppSysID != "" {
			return scope, status.AppSysID, nil
		}
		lastErr = errors.New("template completed without app_sys_id")
	}
	if lastErr == nil {
		lastErr = errors.New("could not create a unique ServiceNow app scope")
	}
	return "", "", lastErr
}

func (c *Client) serviceNowScopeExists(ctx context.Context, scope string) (bool, error) {
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/sn_build_agent/build_agent_api/runQuery/table/sys_app/query/" + url.PathEscape("scopeLIKE"+scope)
	body, _, err := c.getJSON(ctx, endpoint)
	if err != nil {
		return false, err
	}
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return false, err
	}
	for _, m := range flattenMaps(root) {
		if n := numericValue(m["num_results"]); n > 0 {
			return true, nil
		}
		if n := numericValue(m["numResults"]); n > 0 {
			return true, nil
		}
		if text := strings.ToLower(stringify(m["query_results"])); text != "" {
			if strings.Contains(text, "no matching records") {
				return false, nil
			}
			if strings.Contains(text, scope) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (c *Client) startServiceNowAppTemplate(ctx context.Context, req appCreationRequest, scope string) (string, error) {
	payload := map[string]interface{}{
		"template_sys_id": serviceNowAppTemplateSysID,
		"variables": map[string]interface{}{
			"application_name":  req.AppName,
			"short_description": buildAgentGeneratedDescription(req.AppDescription),
			"scope_id":          scope,
		},
	}
	body, _, err := c.postWebJSON(ctx, strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/now/templates", payload)
	if err != nil {
		return "", err
	}
	instanceID := parseTemplateInstanceID(body)
	if instanceID == "" {
		return "", fmt.Errorf("template API did not return a template instance id: %s", trimBody(body))
	}
	return instanceID, nil
}

type serviceNowTemplateStatus struct {
	Status         string
	AppSysID       string
	ErrorMessage   string
	HasError       bool
	ScopeCollision bool
}

func (c *Client) pollServiceNowAppTemplate(ctx context.Context, instanceID string) (serviceNowTemplateStatus, error) {
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/now/templates/status?template_instance_id=" + url.QueryEscape(instanceID)
	var last serviceNowTemplateStatus
	deadline := time.Now().Add(2 * time.Minute)
	for {
		body, _, err := c.getJSON(ctx, endpoint)
		if err != nil {
			return last, err
		}
		last = parseServiceNowTemplateStatus(body)
		if strings.EqualFold(last.Status, "COMPLETE") || strings.EqualFold(last.Status, "COMPLETED") || strings.EqualFold(last.Status, "SUCCESS") {
			if last.HasError || last.ErrorMessage != "" {
				if last.ScopeCollision || strings.Contains(strings.ToLower(last.ErrorMessage), "scope") && strings.Contains(strings.ToLower(last.ErrorMessage), "unique") {
					last.ScopeCollision = true
				}
				return last, errors.New(last.ErrorMessage)
			}
			return last, nil
		}
		if strings.EqualFold(last.Status, "ERROR") || strings.EqualFold(last.Status, "FAILED") || strings.EqualFold(last.Status, "FAILURE") {
			if last.ErrorMessage == "" {
				last.ErrorMessage = "ServiceNow app template failed"
			}
			return last, errors.New(last.ErrorMessage)
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("timed out waiting for app template %s", instanceID)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(750 * time.Millisecond):
		}
	}
}

func parseTemplateInstanceID(body []byte) string {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	for _, m := range flattenMaps(root) {
		for _, key := range []string{"template_instance_id", "templateInstanceId", "sys_id", "id"} {
			if v := strings.TrimSpace(stringify(m[key])); v != "" {
				return v
			}
		}
	}
	if m := asMap(root); m != nil {
		return strings.TrimSpace(stringify(m["result"]))
	}
	return ""
}

func parseServiceNowTemplateStatus(body []byte) serviceNowTemplateStatus {
	var root interface{}
	_ = json.Unmarshal(body, &root)
	status := serviceNowTemplateStatus{}
	for _, m := range flattenMaps(root) {
		if status.Status == "" {
			status.Status = firstString(m, "status", "state")
		}
		if outputs, ok := m["outputs"].([]interface{}); ok {
			for _, item := range outputs {
				om := asMap(item)
				if om == nil {
					continue
				}
				name := strings.TrimSpace(firstString(om, "name"))
				value := strings.TrimSpace(stringify(om["value"]))
				switch name {
				case "app_sys_id", "scope_id":
					status.AppSysID = value
				case "error_message":
					status.ErrorMessage = value
				case "has_error":
					status.HasError = parseBoolish(value)
				}
			}
		}
	}
	if status.ErrorMessage != "" {
		lower := strings.ToLower(status.ErrorMessage)
		status.ScopeCollision = strings.Contains(lower, "scope") && strings.Contains(lower, "unique")
	}
	return status
}

func (c *Client) refreshGliderStateForCreatedApp(ctx context.Context, ws WebWorkspace, appSysID string) error {
	uris := []string{}
	seen := map[string]struct{}{}
	add := func(uri string) {
		uri = strings.TrimSpace(uri)
		if uri == "" {
			return
		}
		key := strings.ToLower(uri)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		uris = append(uris, uri)
	}
	for _, folder := range ws.Folders {
		if strings.HasPrefix(strings.TrimSpace(folder.URI), "now-file:") {
			add(folder.URI)
		}
	}
	if strings.TrimSpace(appSysID) != "" {
		add("now-file:/" + strings.TrimSpace(appSysID))
	}
	if userID := userIDFromWorkspaceURI(ws.URI); userID != "" {
		add("settings:/users/" + userID)
	}
	if len(uris) == 0 {
		return nil
	}
	_, _, err := c.postWebJSON(ctx, strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/sn_glider/v2/sync/state", map[string]interface{}{"uris": uris})
	return err
}

func (c *Client) applyCreatedAppFiles(ctx context.Context, app createdServiceNowApp) error {
	nowMS := time.Now().UnixMilli()
	baseURI := "now-file:/" + app.ScopeID
	files := appFileBlobs(app, nowMS)
	create := []gliderChangeEntry{}
	for _, dir := range []string{".vscode", "src", "src/server", "src/fluent", "src/fluent/generated", ".now"} {
		create = append(create, gliderChangeEntry{CTime: nowMS, MTime: nowMS, Size: 0, Type: "dir", URI: baseURI + "/" + dir})
	}
	for _, file := range files {
		create = append(create, gliderChangeEntry{Checksum: file.Checksum, CTime: nowMS, MTime: nowMS, Size: len(file.Content), Type: "file", URI: baseURI + "/" + file.Path})
	}
	workspaceChecksum := sha1Hex(app.WorkspaceJSON)
	update := []gliderChangeEntry{{Checksum: workspaceChecksum, CTime: workspaceCTime(app.Workspace), MTime: nowMS, Size: len(app.WorkspaceJSON), Type: "file", URI: app.Workspace.URI}}
	files = append(files, gliderFileBlob{Path: settingsURIPath(app.Workspace.URI), Checksum: workspaceChecksum, Content: app.WorkspaceJSON})
	return c.applyGliderChanges(ctx, create, update, nil, files)
}

func appFileBlobs(app createdServiceNowApp, nowMS int64) []gliderFileBlob {
	contents := map[string][]byte{
		".gitignore":                   []byte(".DS_Store\n.now/\ndist/\nnode_modules/\ntarget/\n*.tsbuildinfo\n.jest_cache"),
		".vscode/extensions.json":      []byte("{\n    \"recommendations\": [\n        \"servicenow.fluent-language-extension\"\n    ]\n}"),
		"now.config.json":              []byte(nowConfigJSON(app, true)),
		"package.json":                 []byte(packageJSON(app)),
		"src/fluent/generated/keys.ts": []byte("import '@servicenow/sdk/global'\n\ndeclare global {\n    namespace Now {\n        namespace Internal {\n            interface Keys extends KeysRegistry {}\n        }\n    }\n}"),
		".now/.app-data.json":          []byte(appDataJSON(nowMS)),
		"src/server/tsconfig.json":     []byte(serverTSConfigJSON()),
	}
	paths := make([]string, 0, len(contents))
	for path := range contents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]gliderFileBlob, 0, len(paths))
	for _, path := range paths {
		content := contents[path]
		out = append(out, gliderFileBlob{Path: path, Checksum: sha1Hex(content), Content: content})
	}
	return out
}

func (c *Client) applyGliderChanges(ctx context.Context, create, update, remove []gliderChangeEntry, files []gliderFileBlob) error {
	_, _, _, err := c.postMultipart(ctx, strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/sn_glider/v2/sync/changes/apply", func(writer *multipart.Writer) error {
		if create == nil {
			create = []gliderChangeEntry{}
		}
		if err := writeMultipartJSONPart(writer, "create", create); err != nil {
			return err
		}
		if update == nil {
			update = []gliderChangeEntry{}
		}
		if err := writeMultipartJSONPart(writer, "update", update); err != nil {
			return err
		}
		if remove == nil {
			remove = []gliderChangeEntry{}
		}
		if err := writeMultipartJSONPart(writer, "remove", remove); err != nil {
			return err
		}
		for _, file := range files {
			if err := writeMultipartFilePart(writer, file.Checksum, file.Content); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func writeMultipartJSONPart(writer *multipart.Writer, name string, value interface{}) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="blob"`, name))
	header.Set("Content-Type", "application/json")
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(raw)
	return err
}

func writeMultipartFilePart(writer *multipart.Writer, checksum string, content []byte) error {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, checksum, checksum))
	header.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(content)
	return err
}

func (c *Client) patchConversationApplication(ctx context.Context, app createdServiceNowApp) error {
	if strings.TrimSpace(c.conversationID) == "" {
		return nil
	}
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + "/api/sn_build_agent/build_agent_api/conversations/" + url.PathEscape(c.conversationID)
	if _, _, err := c.patchJSON(ctx, endpoint, map[string]interface{}{"applicationId": app.ScopeID, "applicationName": ""}); err != nil {
		return err
	}
	if title := createdAppConversationTitle(app.Name, c.conversationTitle); title != "" {
		if _, _, err := c.putJSON(ctx, endpoint+"/title", map[string]interface{}{"title": title}); err != nil && c.debug {
			c.debugf("warning: could not update conversation title: %v\n", err)
		} else if err == nil {
			c.conversationTitle = title
		}
	}
	if _, _, err := c.patchJSON(ctx, endpoint, map[string]interface{}{"workingSet": []interface{}{}}); err != nil && c.debug {
		c.debugf("warning: could not update conversation working set: %v\n", err)
	}
	return nil
}

func createdAppConversationTitle(appName, currentTitle string) string {
	appName = singleLineLabel(appName)
	currentTitle = singleLineLabel(currentTitle)
	if appName == "" || currentTitle == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(currentTitle), strings.ToLower(appName)+":") {
		return currentTitle
	}
	return appName + ": " + currentTitle
}

func (c *Client) absorbCreatedApp(app createdServiceNowApp) {
	folder := WebWorkspaceFolder{Name: app.Name, URI: "now-file:/" + app.ScopeID}
	c.workspaceName = app.Workspace.Name
	c.workspaceURI = app.Workspace.URI
	c.workspaceChecksum = sha1Hex(app.WorkspaceJSON)
	c.workspaceDescription = app.Workspace.Description
	c.workspaceFolders = upsertWorkspaceFolder(c.workspaceFolders, folder)
	c.workingSet = nil
	_ = c.SetApp(AppScope{ScopeID: app.ScopeID, ScopeName: app.Name, Scope: app.Scope, AppSysID: app.ScopeID})
}

func appendAppFolderToWorkspaceJSON(content []byte, appName, appSysID string) ([]WebWorkspaceFolder, []byte, error) {
	var root map[string]interface{}
	if len(bytes.TrimSpace(content)) > 0 {
		if err := json.Unmarshal(content, &root); err != nil {
			return nil, nil, err
		}
	}
	if root == nil {
		root = map[string]interface{}{}
	}
	if _, ok := root["settings"]; !ok {
		root["settings"] = map[string]interface{}{"workbench.editor.untitled.hint": "hidden"}
	}
	foldersRaw, _ := root["folders"].([]interface{})
	newFolder := map[string]interface{}{"name": appName, "uri": "now-file:/" + appSysID}
	found := false
	for i, raw := range foldersRaw {
		m := asMap(raw)
		if m == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(firstString(m, "uri", "path")), "now-file:/"+appSysID) {
			m["name"] = appName
			m["uri"] = "now-file:/" + appSysID
			foldersRaw[i] = m
			found = true
		}
	}
	if !found {
		foldersRaw = append(foldersRaw, newFolder)
	}
	root["folders"] = foldersRaw
	raw, err := json.MarshalIndent(root, "", "    ")
	if err != nil {
		return nil, nil, err
	}
	description, folders, err := parseWebWorkspaceFileContent(raw)
	_ = description
	return folders, raw, err
}

func upsertWorkspaceFolder(folders []WebWorkspaceFolder, folder WebWorkspaceFolder) []WebWorkspaceFolder {
	out := append([]WebWorkspaceFolder(nil), folders...)
	for i := range out {
		if strings.EqualFold(out[i].URI, folder.URI) {
			out[i] = folder
			return out
		}
	}
	return append(out, folder)
}

func workspaceCTime(ws WebWorkspace) interface{} {
	if strings.TrimSpace(ws.CreatedAt) != "" {
		return ws.CreatedAt
	}
	return time.Now().UnixMilli()
}

func serviceNowAppCreationResult(app createdServiceNowApp) (map[string]interface{}, error) {
	content := map[string]interface{}{
		"text": fmt.Sprintf("### ServiceNow application created successfully with the following details:\n- **Scope**: %s\n- **Scope ID**: %s\n- **Config Name**: %s\n\nFluent SDK guide catalog refreshed (%d topics): %s", app.Scope, app.ScopeID, app.Name, len(fluentTopicCatalog), strings.Join(fluentTopicCatalog, ", ")),
		"nowConfig": map[string]interface{}{
			"scope":        app.Scope,
			"scopeId":      app.ScopeID,
			"name":         app.Name,
			"tsconfigPath": "./src/server/tsconfig.json",
		},
	}
	rawContent, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	ideContext := emptyIDEContext()
	ideContext["workspaceFolders"] = []string{"/" + app.ScopeID}
	ideContext["fluentVersion"] = fluentSDKVersion
	ideContext["scopeName"] = app.Scope
	ideContext["projectStructure"] = []map[string]interface{}{{
		"appName":        app.Name,
		"textTree":       fmt.Sprintf("/%s/\n├── now.config.json\n├── package.json\n└── src/\n    └── server/\n        └── tsconfig.json", app.ScopeID),
		"fileCount":      3,
		"directoryCount": 3,
	}}
	return map[string]interface{}{"content": string(rawContent), "ideContext": ideContext}, nil
}

var fluentTopicCatalog = []string{
	"alias-guide", "alias-template-guide", "application-menu-guide", "assignment-rule-guide", "atf-guide", "building-ai-agents-guide", "business-rule-guide", "client-script-guide", "creating-workspaces-guide", "cross-scope-privilege-guide", "data-lookup-guide", "data-policy-guide", "developing-apps-guide", "email-notification-guide", "encoded-query-guide", "external-services-guide", "fluent-overview", "importing-data-guide", "instance-scan-guide", "module-guide", "nowassist-skills-guide", "platform-view-guide", "platform-view-lists-guide", "playbook-activities-guide", "playbook-anti-patterns-guide", "playbook-datapills-guide", "playbook-guide", "playbook-lanes-guide", "playbook-patterns-guide", "playbook-triggers-guide", "playbook-unsupported-features-guide", "property-guide", "query-guide", "registering-events-guide", "rest-message-guide", "retry-policy-guide", "scheduled-script-guide", "script-include-guide", "scripted-rest-api-guide", "security-guide", "service-catalog-guide", "service-catalog-variables-guide", "service-portal-guide", "service-portal-reference-guide", "table-augments-guide", "table-guide", "ui-page-guide", "ui-page-patterns-guide", "ui-page-theming-guide", "user-criteria-examples-guide", "user-criteria-guide", "wfa-custom-action-guide", "wfa-flow-actions-guide", "wfa-flow-guide", "wfa-flow-logic-guide", "wfa-flow-stages-guide", "wfa-subflow-guide", "wfa-trigger-guide", "data-helpers-guide", "now-attach-guide", "now-del-guide", "now-id-guide", "now-include-guide", "now-ref-guide", "override-guide",
}

func buildAgentGeneratedDescription(description string) string {
	description = singleLineLabel(description)
	if strings.HasPrefix(description, "[BUILD AGENT GENERATED]:") {
		return description
	}
	if description == "" {
		return "[BUILD AGENT GENERATED]"
	}
	return "[BUILD AGENT GENERATED]:  " + description
}

func scopeCandidatesForAppName(appName string) []string {
	base := serviceNowScopeBase(appName)
	seen := map[string]struct{}{}
	out := []string{}
	add := func(scope string) {
		scope = truncateScope(scope, "")
		if scope == "" {
			return
		}
		key := strings.ToLower(scope)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, scope)
	}
	add(base)
	for i := 1; i <= 2; i++ {
		add(truncateScope(base, "_"+strconv.Itoa(i)))
	}
	for len(out) < 8 {
		suffix := randomScopeSuffix()
		add(truncateScope(base, "_"+suffix))
	}
	return out
}

func serviceNowScopeBase(appName string) string {
	slug := strings.Trim(serviceNowScopeSlug(appName), "_")
	if slug == "" {
		slug = "app"
	}
	return truncateScope("x_snc_"+slug, "")
}

var nonScopeCharPattern = regexp.MustCompile(`[^a-z0-9]+`)

func serviceNowScopeSlug(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	input = nonScopeCharPattern.ReplaceAllString(input, "_")
	input = strings.Trim(input, "_")
	for strings.Contains(input, "__") {
		input = strings.ReplaceAll(input, "__", "_")
	}
	return input
}

func truncateScope(base, suffix string) string {
	base = strings.Trim(base, "_")
	suffix = strings.TrimSpace(suffix)
	maxBase := serviceNowScopeMaxLength - len(suffix)
	if maxBase < len("x_snc_a") {
		maxBase = serviceNowScopeMaxLength
		suffix = ""
	}
	if len(base) > maxBase {
		base = strings.TrimRight(base[:maxBase], "_")
	}
	return base + suffix
}

func randomScopeSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < 4; i++ {
		b.WriteByte(alphabet[rand.Intn(len(alphabet))])
	}
	return b.String()
}

func packageNameFromAppName(appName string) string {
	slug := strings.ReplaceAll(serviceNowScopeSlug(appName), "_", "-")
	if slug == "" {
		slug = "app"
	}
	return "x-snc-" + slug
}

func nowConfigJSON(app createdServiceNowApp, includeTSConfig bool) string {
	m := map[string]interface{}{"scope": app.Scope, "scopeId": app.ScopeID, "name": app.Name}
	if includeTSConfig {
		m["tsconfigPath"] = "./src/server/tsconfig.json"
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	return string(raw)
}

func packageJSON(app createdServiceNowApp) string {
	m := map[string]interface{}{
		"name":        app.PackageName,
		"version":     "1.0.0",
		"description": buildAgentGeneratedDescription(app.Description),
		"license":     "UNLICENSED",
		"imports": map[string]interface{}{
			"#now:*": "./@types/servicenow/fluent/*/index.js",
		},
		"scripts": map[string]interface{}{
			"build":     "now-sdk build",
			"deploy":    "now-sdk install",
			"transform": "now-sdk transform",
			"types":     "now-sdk dependencies",
		},
		"devDependencies": map[string]interface{}{
			"@servicenow/sdk":   fluentSDKVersion,
			"@servicenow/glide": glideDevDependencyVersion,
		},
	}
	raw, _ := json.MarshalIndent(m, "", "    ")
	return string(raw)
}

func appDataJSON(nowMS int64) string {
	m := map[string]interface{}{"lastSync": nowMS, "syncBy": "", "syncStatus": "never_ran", "syncNeeded": false}
	raw, _ := json.MarshalIndent(m, "", "  ")
	return string(raw)
}

func serverTSConfigJSON() string {
	return `{
  "compilerOptions": {
    "rootDir": "./",
    "outDir": "../../dist/server",
    "module": "es2022",
    "target": "es2022",
    "moduleResolution": "bundler",
    "allowJs": true,
    "declaration": false,
    "sourceMap": false,
    "skipLibCheck": true,
    "noUnusedLocals": false,
    "checkJs": true
  },
  "include": [
    "./**/*.ts",
    "./**/*.js"
  ],
  "exclude": [
    "**/*.now.ts"
  ]
}`
}

func sha1Hex(content []byte) string {
	sum := sha1.Sum(content)
	return hex.EncodeToString(sum[:])
}

func flattenMaps(v interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	var walk func(interface{})
	walk = func(item interface{}) {
		switch typed := item.(type) {
		case map[string]interface{}:
			out = append(out, typed)
			for _, child := range typed {
				walk(child)
			}
		case []interface{}:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(v)
	return out
}

func parseBoolish(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "y":
		return true
	default:
		return false
	}
}

func numericValue(v interface{}) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i
	default:
		return 0
	}
}
