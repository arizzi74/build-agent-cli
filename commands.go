package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

type SlashCommandSuggestion struct {
	Text        string
	Description string
	Aliases     []string
	Behavior    SlashCommandBehavior
}

var slashCommandOutputWriter io.Writer = os.Stderr

func withSlashCommandOutput(w io.Writer, fn func() (bool, error)) (bool, error) {
	if w == nil {
		w = os.Stderr
	}
	previous := slashCommandOutputWriter
	slashCommandOutputWriter = w
	defer func() { slashCommandOutputWriter = previous }()
	return fn()
}

func slashCommandPrintln(args ...interface{}) {
	fmt.Fprintln(slashCommandOutputWriter, args...)
}

func slashCommandPrintf(format string, args ...interface{}) {
	fmt.Fprintf(slashCommandOutputWriter, format, args...)
}

func handleSlashCommandForTerminal(ctx context.Context, c *Client, line string, status statusBarState) (bool, error) {
	if !slashCommandOutputCanBeCaptured(line) {
		return handleSlashCommand(ctx, c, line)
	}
	var output strings.Builder
	handled, err := withSlashCommandOutput(&output, func() (bool, error) {
		return handleSlashCommand(ctx, c, line)
	})
	if text := strings.TrimSpace(output.String()); text != "" {
		if !terminalRecordSystemTextAndAppend("Command", text, status) {
			fmt.Fprintln(os.Stderr, text)
		}
	}
	return handled, err
}

func slashCommandOutputCanBeCaptured(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	command, ok := findSlashCommand(fields[0])
	if !ok {
		return true
	}
	return command.capturesOutput(fields[1:])
}

func slashCommandSuggestions() []SlashCommandSuggestion {
	return slashCommandSuggestionsForClient(nil)
}

func slashCommandSuggestionsForClient(c *Client) []SlashCommandSuggestion {
	suggestions := make([]SlashCommandSuggestion, 0, len(slashCommandRegistry))
	for _, command := range slashCommandRegistry {
		if command.Hidden || len(command.Suggestions) == 0 || !command.available(c) {
			continue
		}
		for _, suggestion := range command.Suggestions {
			suggestions = append(suggestions, SlashCommandSuggestion{
				Text: suggestion, Description: command.Description, Behavior: command.suggestionBehavior(suggestion),
				Aliases: command.Aliases,
			})
		}
	}
	return suggestions
}

func handleSlashCommand(ctx context.Context, c *Client, line string) (bool, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return false, nil
	}

	command, ok := findSlashCommand(fields[0])
	if !ok {
		slashCommandPrintf("unknown command %s; type /help\n", fields[0])
		return true, nil
	}
	if !command.available(c) {
		return true, command.unavailableReason(c)
	}
	return command.Handler(ctx, c, fields[1:])
}

func printSlashHelp(c *Client) {
	slashCommandPrintln("commands:")
	categories := make([]string, 0)
	seenCategories := make(map[string]bool)
	for _, command := range slashCommandRegistry {
		if command.Hidden || !command.available(c) || seenCategories[command.Category] {
			continue
		}
		seenCategories[command.Category] = true
		categories = append(categories, command.Category)
	}
	for _, category := range categories {
		slashCommandPrintln("  " + category + ":")
		for _, command := range slashCommandRegistry {
			if command.Hidden || !command.available(c) || command.Category != category {
				continue
			}
			slashCommandPrintln("    " + command.helpLine())
		}
	}
}

func handleMCPCommand(ctx context.Context, c *Client, args []string) error {
	if len(args) == 0 || strings.EqualFold(args[0], "help") {
		slashCommandPrintln("usage: /mcp list")
		return nil
	}
	if !c.opts.Nirvana {
		return errors.New("/mcp is only available in --nirvana mode; web gateway mode delegates tools to the ServiceNow backend")
	}

	switch strings.ToLower(args[0]) {
	case "list", "ls", "servers":
		servers := c.nirvanaMCPServerPayload(ctx)
		if len(servers) == 0 {
			slashCommandPrintln("mcp servers: none")
			return nil
		}
		slashCommandPrintln("mcp servers advertised to Nirvana/Forge:")
		for _, server := range servers {
			urlSuffix := ""
			if server.URL != "" {
				urlSuffix = " url=" + server.URL
			}
			slashCommandPrintf("* %s  id=%s transport=%s source=%s%s\n", server.Name, server.ServerID, server.Transport, server.Source, urlSuffix)
		}
		slashCommandPrintln("tools: WDF pass-through servers do not expose client-side tool schemas; Forge loads them after the Nirvana handshake, matching the Glider web client.")
		return nil
	default:
		return fmt.Errorf("unknown /mcp command %q", args[0])
	}
}

func handleConversationCommand(parent context.Context, c *Client, args []string) error {
	if c.opts.CodeAssistWS {
		return errors.New("/conversation is not available in experimental Code Assist websocket mode")
	}
	ctx, cancel := conversationCommandContext(parent)
	defer cancel()

	if len(args) == 0 || strings.EqualFold(args[0], "select") || strings.EqualFold(args[0], "choose") {
		return c.PromptWebConversation(ctx, "command")
	}

	switch strings.ToLower(args[0]) {
	case "help":
		slashCommandPrintln("usage: /conversation")
		slashCommandPrintln("opens the conversation picker; choose an existing conversation or New conversation")
		return nil
	case "current", "show":
		printCurrentConversation(c)
		return nil
	case "list", "ls":
		conversations, err := c.ListWebConversations(ctx)
		if err != nil {
			return err
		}
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(terminalStderrFD()) {
			return c.SelectWebConversation(ctx, conversations, true)
		}
		printConversationList(conversations, c.conversationID)
		return nil
	case "new", "create":
		return c.StartNewWebConversation(ctx)
	case "use", "open", "switch":
		if len(args) < 2 {
			return fmt.Errorf("usage: /conversation use <number|id|id-prefix|latest>")
		}
		return c.SelectConversationByArg(ctx, args[1], false)
	default:
		return fmt.Errorf("unknown /conversation command %q", args[0])
	}
}

func handleWorkspaceCommand(parent context.Context, c *Client, args []string) error {
	if len(args) == 0 {
		ctx, cancel := conversationCommandContext(parent)
		defer cancel()
		return c.PromptWorkspaceSelection(ctx)
	}
	if strings.EqualFold(args[0], "help") {
		slashCommandPrintln("usage: /workspace")
		slashCommandPrintln("opens the workspace picker; choose an existing Web UI/local workspace")
		return nil
	}
	ctx, cancel := conversationCommandContext(parent)
	defer cancel()

	switch strings.ToLower(args[0]) {
	case "select", "choose":
		return c.PromptWorkspaceSelection(ctx)
	case "current", "show":
		ws := c.WorkspaceState()
		slashCommandPrintf("workspace: %s\n", ws.Name)
		if ws.WebWorkspaceURI != "" {
			slashCommandPrintf("workspace uri: %s\n", ws.WebWorkspaceURI)
		}
		if ws.ConversationID != "" {
			slashCommandPrintf("conversation: %s\n", ws.ConversationID)
		} else {
			slashCommandPrintln("conversation: <new>")
		}
		if ws.App != nil {
			printApp(ws.App)
		} else {
			slashCommandPrintln("app: <none>")
		}
		return nil
	case "list", "ls":
		if c.canUseWebWorkspaceAPI() {
			webWorkspaces, err := c.ListWebWorkspaces(ctx)
			if err == nil {
				printWebWorkspaceList(webWorkspaces, c.workspaceName, c.workspaceURI)
				return nil
			}
			c.debugf("warning: could not list web workspaces: %v\n", err)
		}
		workspaces, err := listWorkspaces(c.opts.Profile)
		if err != nil {
			return err
		}
		if len(workspaces) == 0 {
			slashCommandPrintln("no workspaces yet")
			return nil
		}
		slashCommandPrintln("workspaces:")
		for _, ws := range workspaces {
			marker := " "
			if ws.Name == c.workspaceName {
				marker = "*"
			}
			conversation := "<new>"
			if ws.ConversationID != "" {
				conversation = ws.ConversationID
			}
			app := ""
			if ws.App != nil && ws.App.ScopeID != "" {
				app = " app=" + ws.App.ScopeID
			}
			slashCommandPrintf("%s %s  conversation=%s%s\n", marker, ws.Name, conversation, app)
		}
		return nil
	case "new":
		if len(args) < 2 {
			return fmt.Errorf("usage: /workspace new <name>")
		}
		name := strings.Join(args[1:], " ")
		if _, ok := loadWorkspace(c.opts.Profile, name); ok {
			return fmt.Errorf("workspace %q already exists", name)
		}
		return c.SwitchWorkspace(name, true)
	case "use", "switch":
		if len(args) < 2 {
			return fmt.Errorf("usage: /workspace use <name>")
		}
		selector := strings.Join(args[1:], " ")
		if c.canUseWebWorkspaceAPI() {
			webWorkspaces, err := c.ListWebWorkspaces(ctx)
			if err == nil {
				if ws, err := resolveWebWorkspaceSelector(webWorkspaces, selector); err == nil {
					return c.SwitchWebWorkspace(ctx, ws)
				} else if c.debug {
					c.debugf("warning: could not resolve web workspace: %v\n", err)
				}
			} else if c.debug {
				c.debugf("warning: could not list web workspaces: %v\n", err)
			}
		}
		return c.SwitchWorkspace(selector, false)
	case "reset":
		name := c.workspaceName
		if len(args) >= 2 {
			name = strings.Join(args[1:], " ")
		}
		if name != c.workspaceName {
			if err := c.SwitchWorkspace(name, false); err != nil {
				return err
			}
		}
		return c.ResetWorkspace()
	case "delete", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: /workspace delete <name>")
		}
		name := strings.Join(args[1:], " ")
		if err := deleteWorkspace(c.opts.Profile, c.workspaceName, name); err != nil {
			return err
		}
		slashCommandPrintf("deleted workspace %q\n", name)
		return nil
	default:
		return fmt.Errorf("unknown /workspace command %q", args[0])
	}
}

func handleAppCommand(parent context.Context, c *Client, args []string) error {
	if len(args) == 0 {
		ctx, cancel := conversationCommandContext(parent)
		defer cancel()
		return c.PromptAppSelection(ctx)
	}
	if strings.EqualFold(args[0], "help") {
		slashCommandPrintln("usage: /app")
		slashCommandPrintln("opens the app picker; choose an app from the active workspace")
		return nil
	}
	ctx, cancel := conversationCommandContext(parent)
	defer cancel()

	switch strings.ToLower(args[0]) {
	case "select", "choose", "list", "ls":
		return c.PromptAppSelection(ctx)
	case "current", "show":
		app := c.CurrentApp()
		if app == nil {
			slashCommandPrintln("app: <none>")
			return nil
		}
		printApp(app)
		return nil
	case "use", "set":
		if len(args) < 2 {
			return fmt.Errorf("usage: /app use <scopeId> [scopeName]")
		}
		app := AppScope{ScopeID: args[1], ScopeName: args[1]}
		if len(args) >= 3 {
			app.ScopeName = strings.Join(args[2:], " ")
		}
		if err := c.SetApp(app); err != nil {
			return err
		}
		_ = c.ensureActiveAppMetadata(ctx)
		slashCommandPrintf("app set: %s\n", app.ScopeID)
		return nil
	case "clear", "unset":
		if err := c.ClearApp(); err != nil {
			return err
		}
		slashCommandPrintln("app cleared")
		return nil
	default:
		return fmt.Errorf("unknown /app command %q", args[0])
	}
}

func printApp(app *AppScope) {
	slashCommandPrintf("app scope: %s\n", app.ScopeID)
	if app.ScopeName != "" {
		slashCommandPrintf("app name: %s\n", app.ScopeName)
	}
	if app.Scope != "" {
		slashCommandPrintf("service now scope: %s\n", app.Scope)
	}
	if app.AppSysID != "" {
		slashCommandPrintf("app sys_id: %s\n", app.AppSysID)
	}
}
