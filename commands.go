package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

type SlashCommandSuggestion struct {
	Text        string
	Description string
}

func slashCommandSuggestions() []SlashCommandSuggestion {
	return []SlashCommandSuggestion{
		{Text: "/help", Description: "show available commands"},
		{Text: "/conversation", Description: "choose an existing Build Agent conversation or create one"},
		{Text: "/mcp list", Description: "show MCP servers advertised to Nirvana/Forge"},
		{Text: "/workspace current", Description: "show active workspace"},
		{Text: "/workspace list", Description: "list local workspaces"},
		{Text: "/workspace new ", Description: "create and switch to a workspace"},
		{Text: "/workspace use ", Description: "switch to an existing workspace"},
		{Text: "/workspace reset", Description: "clear conversation/history for the active workspace"},
		{Text: "/workspace delete ", Description: "delete an inactive workspace"},
		{Text: "/app current", Description: "show selected app scope"},
		{Text: "/app use ", Description: "set selected app scope"},
		{Text: "/app clear", Description: "clear selected app scope"},
		{Text: "/exit", Description: "quit"},
		{Text: "/quit", Description: "quit"},
	}
}

func handleSlashCommand(ctx context.Context, c *Client, line string) (bool, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return false, nil
	}

	cmd := strings.ToLower(fields[0])
	switch cmd {
	case "/help", "/?":
		printSlashHelp()
		return true, nil
	case "/workspace", "/ws":
		return true, handleWorkspaceCommand(c, fields[1:])
	case "/conversation", "/conv":
		return true, handleConversationCommand(ctx, c, fields[1:])
	case "/mcp":
		return true, handleMCPCommand(ctx, c, fields[1:])
	case "/app", "/application":
		return true, handleAppCommand(c, fields[1:])
	case "/exit", "/quit":
		return false, nil
	default:
		fmt.Fprintf(os.Stderr, "unknown command %s; type /help\n", fields[0])
		return true, nil
	}
}

func printSlashHelp() {
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  /help")
	fmt.Fprintln(os.Stderr, "  /exit | /quit")
	fmt.Fprintln(os.Stderr, "  /conversation              (interactive selector; choose existing or New conversation)")
	fmt.Fprintln(os.Stderr, "  /mcp list                  (Nirvana: show advertised MCP servers)")
	fmt.Fprintln(os.Stderr, "  /workspace current")
	fmt.Fprintln(os.Stderr, "  /workspace list")
	fmt.Fprintln(os.Stderr, "  /workspace new <name>")
	fmt.Fprintln(os.Stderr, "  /workspace use <name>")
	fmt.Fprintln(os.Stderr, "  /workspace reset [name]")
	fmt.Fprintln(os.Stderr, "  /workspace delete <name>")
	fmt.Fprintln(os.Stderr, "  /app current")
	fmt.Fprintln(os.Stderr, "  /app use <scopeId> [scopeName]")
	fmt.Fprintln(os.Stderr, "  /app clear")
}

func handleMCPCommand(ctx context.Context, c *Client, args []string) error {
	if len(args) == 0 || strings.EqualFold(args[0], "help") {
		fmt.Fprintln(os.Stderr, "usage: /mcp list")
		return nil
	}
	if !c.opts.Nirvana {
		return errors.New("/mcp is only available in --nirvana mode; web gateway mode delegates tools to the ServiceNow backend")
	}

	switch strings.ToLower(args[0]) {
	case "list", "ls", "servers":
		servers := c.nirvanaMCPServerPayload(ctx)
		if len(servers) == 0 {
			fmt.Fprintln(os.Stderr, "mcp servers: none")
			return nil
		}
		fmt.Fprintln(os.Stderr, "mcp servers advertised to Nirvana/Forge:")
		for _, server := range servers {
			urlSuffix := ""
			if server.URL != "" {
				urlSuffix = " url=" + server.URL
			}
			fmt.Fprintf(os.Stderr, "* %s  id=%s transport=%s source=%s%s\n", server.Name, server.ServerID, server.Transport, server.Source, urlSuffix)
		}
		fmt.Fprintln(os.Stderr, "tools: WDF pass-through servers do not expose client-side tool schemas; Forge loads them after the Nirvana handshake, matching the Glider web client.")
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
		fmt.Fprintln(os.Stderr, "usage: /conversation")
		fmt.Fprintln(os.Stderr, "opens the conversation picker; choose an existing conversation or New conversation")
		return nil
	case "current", "show":
		printCurrentConversation(c)
		return nil
	case "list", "ls":
		conversations, err := c.ListWebConversations(ctx)
		if err != nil {
			return err
		}
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd())) {
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

func handleWorkspaceCommand(c *Client, args []string) error {
	if len(args) == 0 || strings.EqualFold(args[0], "help") {
		fmt.Fprintln(os.Stderr, "usage: /workspace current|list|new <name>|use <name>|reset [name]|delete <name>")
		return nil
	}

	switch strings.ToLower(args[0]) {
	case "current", "show":
		ws := c.WorkspaceState()
		fmt.Fprintf(os.Stderr, "workspace: %s\n", ws.Name)
		if ws.ConversationID != "" {
			fmt.Fprintf(os.Stderr, "conversation: %s\n", ws.ConversationID)
		} else {
			fmt.Fprintln(os.Stderr, "conversation: <new>")
		}
		if ws.App != nil {
			printApp(ws.App)
		} else {
			fmt.Fprintln(os.Stderr, "app: <none>")
		}
		return nil
	case "list":
		workspaces, err := listWorkspaces(c.opts.Profile)
		if err != nil {
			return err
		}
		if len(workspaces) == 0 {
			fmt.Fprintln(os.Stderr, "no workspaces yet")
			return nil
		}
		fmt.Fprintln(os.Stderr, "workspaces:")
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
			fmt.Fprintf(os.Stderr, "%s %s  conversation=%s%s\n", marker, ws.Name, conversation, app)
		}
		return nil
	case "new":
		if len(args) < 2 {
			return fmt.Errorf("usage: /workspace new <name>")
		}
		name := args[1]
		if _, ok := loadWorkspace(c.opts.Profile, name); ok {
			return fmt.Errorf("workspace %q already exists", name)
		}
		return c.SwitchWorkspace(name, true)
	case "use", "switch":
		if len(args) < 2 {
			return fmt.Errorf("usage: /workspace use <name>")
		}
		return c.SwitchWorkspace(args[1], false)
	case "reset":
		name := c.workspaceName
		if len(args) >= 2 {
			name = args[1]
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
		if err := deleteWorkspace(c.opts.Profile, c.workspaceName, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "deleted workspace %q\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown /workspace command %q", args[0])
	}
}

func handleAppCommand(c *Client, args []string) error {
	if len(args) == 0 || strings.EqualFold(args[0], "help") {
		fmt.Fprintln(os.Stderr, "usage: /app current|use <scopeId> [scopeName]|clear")
		return nil
	}

	switch strings.ToLower(args[0]) {
	case "current", "show":
		app := c.CurrentApp()
		if app == nil {
			fmt.Fprintln(os.Stderr, "app: <none>")
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
		fmt.Fprintf(os.Stderr, "app set: %s\n", app.ScopeID)
		return nil
	case "clear", "unset":
		if err := c.ClearApp(); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "app cleared")
		return nil
	default:
		return fmt.Errorf("unknown /app command %q", args[0])
	}
}

func printApp(app *AppScope) {
	fmt.Fprintf(os.Stderr, "app scope: %s\n", app.ScopeID)
	if app.ScopeName != "" {
		fmt.Fprintf(os.Stderr, "app name: %s\n", app.ScopeName)
	}
	if app.AppSysID != "" {
		fmt.Fprintf(os.Stderr, "app sys_id: %s\n", app.AppSysID)
	}
}
