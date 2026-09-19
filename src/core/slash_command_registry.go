package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SlashCommandArgumentPolicy describes the top-level command's argument contract.
type SlashCommandArgumentPolicy int

const (
	SlashCommandNoArguments SlashCommandArgumentPolicy = iota
	SlashCommandOptionalSubcommand
	SlashCommandRequiredSubcommand
)

// SlashCommandBehavior controls how a bare command suggestion is consumed by the
// terminal menu. Commands requiring a subcommand become immediately runnable once
// their suggestion includes that subcommand.
type SlashCommandBehavior int

const (
	SlashCommandImmediate SlashCommandBehavior = iota
	SlashCommandModal
	SlashCommandTemplate
)

// SlashCommandRuntime declares the transport/runtime in which a command is meaningful.
type SlashCommandRuntime int

const (
	SlashCommandRuntimeAny SlashCommandRuntime = iota
	SlashCommandRuntimeNirvana
	SlashCommandRuntimeNoCodeAssistWS
)

// SlashCommandCapturePolicy declares whether terminal command output belongs in the
// managed transcript. Modal commands retain direct terminal ownership.
type SlashCommandCapturePolicy int

const (
	SlashCommandCaptureOutput SlashCommandCapturePolicy = iota
	SlashCommandDoNotCaptureModal
)

type SlashCommandDefinition struct {
	Canonical                string
	Aliases                  []string
	Description              string
	Category                 string
	Order                    int
	Arguments                SlashCommandArgumentPolicy
	Behavior                 SlashCommandBehavior
	Runtime                  SlashCommandRuntime
	CapturePolicy            SlashCommandCapturePolicy
	AvailableWhileProcessing bool
	// TelegramDisabled keeps host-lifecycle commands local even though the TUI
	// and Telegram otherwise share this authoritative registry.
	TelegramDisabled bool
	Hidden           bool
	Suggestions      []string
	IsModal          func(args []string) bool
	Handler          func(context.Context, *Client, []string) (bool, error)
}

func (d SlashCommandDefinition) names() []string {
	return append([]string{d.Canonical}, d.Aliases...)
}

func (d SlashCommandDefinition) available(c *Client) bool {
	if c == nil {
		return true
	}
	if c.processing && !d.AvailableWhileProcessing {
		return false
	}
	switch d.Runtime {
	case SlashCommandRuntimeNirvana:
		return c.opts.Nirvana
	case SlashCommandRuntimeNoCodeAssistWS:
		return !c.opts.CodeAssistWS
	default:
		return true
	}
}

func (d SlashCommandDefinition) unavailableReason(c *Client) error {
	if c != nil && c.processing && !d.AvailableWhileProcessing {
		return fmt.Errorf("cannot run %s while a turn is processing", d.Canonical)
	}
	if d.Runtime == SlashCommandRuntimeNirvana {
		return fmt.Errorf("%s is only available in --nirvana mode; web gateway mode delegates tools to the ServiceNow backend", d.Canonical)
	}
	return fmt.Errorf("%s is not available in this runtime", d.Canonical)
}

func (d SlashCommandDefinition) modal(args []string) bool {
	return d.IsModal != nil && d.IsModal(args)
}

func (d SlashCommandDefinition) capturesOutput(args []string) bool {
	return d.CapturePolicy != SlashCommandDoNotCaptureModal || !d.modal(args)
}

func (d SlashCommandDefinition) suggestionBehavior(suggestion string) SlashCommandBehavior {
	if d.Arguments == SlashCommandRequiredSubcommand && len(strings.Fields(suggestion)) > 1 {
		return SlashCommandImmediate
	}
	return d.Behavior
}

func (d SlashCommandDefinition) helpLine() string {
	names := d.Canonical
	if len(d.Aliases) > 0 {
		names += " | " + strings.Join(d.Aliases, " | ")
	}
	return fmt.Sprintf("%-28s %s", names, d.Description)
}

func commandArgsStartModal(args []string, modalSubcommands ...string) bool {
	if len(args) == 0 {
		return true
	}
	for _, subcommand := range modalSubcommands {
		if strings.EqualFold(args[0], subcommand) {
			return true
		}
	}
	return false
}

var slashCommandRegistry = []SlashCommandDefinition{
	{
		Canonical: "/help", Aliases: []string{"/?"}, Description: "show available commands", Category: "General", Order: 10,
		Arguments: SlashCommandNoArguments, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/help"},
	},
	{
		Canonical: "/conversation", Aliases: []string{"/conv"}, Description: "choose an existing Build Agent conversation or create one", Category: "Context", Order: 15,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandModal, Runtime: SlashCommandRuntimeNoCodeAssistWS,
		CapturePolicy: SlashCommandDoNotCaptureModal, Suggestions: []string{"/conversation"},
		IsModal: func(args []string) bool { return commandArgsStartModal(args, "select", "choose", "list", "ls") },
		Handler: func(ctx context.Context, c *Client, args []string) (bool, error) {
			return true, handleConversationCommand(ctx, c, args)
		},
	},
	{
		Canonical: "/status", Description: "show offline-safe runtime health (/status --json for schema output)", Category: "General", Order: 20,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/status", "/status --json"},
		Handler: handleStatusCommand,
	},
	{
		Canonical: "/turn", Description: "show active or last redacted semantic turn (/turn --json for schema output)", Category: "General", Order: 25,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/turn", "/turn --json"},
		Handler: handleTurnCommand,
	},
	{
		Canonical: "/support-bundle", Description: "create a local redacted deterministic support archive", Category: "General", Order: 26,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/support-bundle", "/support-bundle --json"},
		Handler: handleSupportBundleCommand,
	},
	{
		Canonical: "/search", Description: "search offline-safe local metadata and redacted events", Category: "General", Order: 27,
		Arguments: SlashCommandRequiredSubcommand, Behavior: SlashCommandTemplate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/search tool", "/search --json tool"},
		Handler: handleSearchCommand,
	},
	{
		Canonical: "/export", Description: "create a local redacted conversation export", Category: "General", Order: 28,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/export", "/export --json"},
		Handler: handleExportCommand,
	},
	{
		Canonical: "/debug", Description: "control local redacted diagnostics; inspect semantic journal", Category: "General", Order: 29,
		Arguments: SlashCommandRequiredSubcommand, Behavior: SlashCommandTemplate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/debug status", "/debug tail", "/debug event "},
		Handler: handleDebugCommand,
	},
	{
		Canonical: "/goal", Description: "manage local durable goals (/goal --json for schema output)", Category: "General", Order: 30,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/goal", "/goal add ", "/goal --json"},
		Handler: handleGoalCommand,
	},
	{
		Canonical: "/approvals", Description: "list local pending/recent approval requests", Category: "General", Order: 31,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/approvals", "/approvals --json"},
		Handler: handleApprovalsCommand,
	},
	{
		Canonical: "/approve", Description: "execute one approved local action exactly once", Category: "General", Order: 32,
		Arguments: SlashCommandRequiredSubcommand, Behavior: SlashCommandTemplate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: false, Suggestions: []string{"/approve "},
		Handler: handleApproveCommand,
	},
	{
		Canonical: "/reject", Description: "reject one pending local action", Category: "General", Order: 33,
		Arguments: SlashCommandRequiredSubcommand, Behavior: SlashCommandTemplate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/reject "},
		Handler: handleRejectCommand,
	},
	{
		Canonical: "/mcp", Description: "show configured MCP servers and available WDF tool schemas", Category: "Tools", Order: 40,
		Arguments: SlashCommandRequiredSubcommand, Behavior: SlashCommandTemplate, Runtime: SlashCommandRuntimeNirvana,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, Suggestions: []string{"/mcp list", "/mcp tools"},
		Handler: func(ctx context.Context, c *Client, args []string) (bool, error) {
			return true, handleMCPCommand(ctx, c, args)
		},
	},
	{
		Canonical: "/instance", Aliases: []string{"/instances"}, Description: "switch to a configured ServiceNow instance", Category: "Context", Order: 45,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandModal, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandDoNotCaptureModal, Suggestions: []string{"/instance"},
		IsModal: func(args []string) bool { return commandArgsStartModal(args, "select", "choose") },
		Handler: func(ctx context.Context, c *Client, args []string) (bool, error) {
			return true, handleInstanceCommand(ctx, c, args)
		},
	},
	{
		Canonical: "/setup", Description: "configure, authenticate, and switch to a new ServiceNow instance", Category: "Context", Order: 46,
		Arguments: SlashCommandNoArguments, Behavior: SlashCommandModal, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandDoNotCaptureModal, Suggestions: []string{"/setup"},
		IsModal: func(args []string) bool { return len(args) == 0 },
		Handler: func(ctx context.Context, c *Client, args []string) (bool, error) {
			if len(args) != 0 {
				return true, errors.New("usage: /setup")
			}
			return true, c.SetupInstance(ctx)
		},
	},
	{
		Canonical: "/workspace", Aliases: []string{"/ws"}, Description: "choose an existing Web UI/local workspace", Category: "Context", Order: 50,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandModal, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandDoNotCaptureModal, Suggestions: []string{"/workspace"},
		IsModal: func(args []string) bool { return commandArgsStartModal(args, "select", "choose") },
		Handler: func(ctx context.Context, c *Client, args []string) (bool, error) {
			return true, handleWorkspaceCommand(ctx, c, args)
		},
	},
	{
		Canonical: "/app", Aliases: []string{"/application"}, Description: "choose an app from the active workspace", Category: "Context", Order: 60,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandModal, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandDoNotCaptureModal, Suggestions: []string{"/app"},
		IsModal: func(args []string) bool { return commandArgsStartModal(args, "select", "choose", "list", "ls") },
		Handler: func(ctx context.Context, c *Client, args []string) (bool, error) {
			return true, handleAppCommand(ctx, c, args)
		},
	},
	{
		Canonical: "/sync", Description: "safely synchronize persistent local app source with ServiceNow", Category: "Context", Order: 65,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, Suggestions: []string{"/sync", "/sync status", "/sync pull", "/sync push"},
		Handler: func(ctx context.Context, c *Client, args []string) (bool, error) {
			return true, handleSyncCommand(ctx, c, args)
		},
	},
	{
		Canonical: "/attach", Aliases: []string{"/attachments"}, Description: "manage files and clipboard images for the next prompt", Category: "Context", Order: 66,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeNirvana,
		CapturePolicy: SlashCommandCaptureOutput, Suggestions: []string{"/attach list", "/attach add ", "/attach paste", "/attach remove ", "/attach clear"},
		Handler: func(ctx context.Context, c *Client, args []string) (bool, error) {
			return true, handleAttachmentCommand(ctx, c, args)
		},
	},
	{
		Canonical: "/project", Aliases: []string{"/projects"}, Description: "manage local app project checkouts", Category: "Context", Order: 67,
		Arguments: SlashCommandRequiredSubcommand, Behavior: SlashCommandTemplate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, Suggestions: []string{"/project help", "/project current", "/project list", "/project use ", "/project clone-here", "/project primary ", "/project forget "},
		Handler: func(ctx context.Context, c *Client, args []string) (bool, error) {
			return true, handleProjectCommand(ctx, c, args)
		},
	},
	{
		Canonical: "/telegram", Description: "inspect and manage the private Telegram channel", Category: "General", Order: 68,
		Arguments: SlashCommandOptionalSubcommand, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandDoNotCaptureModal, AvailableWhileProcessing: false, Suggestions: []string{"/telegram status", "/telegram setup", "/telegram help"},
		IsModal: func(args []string) bool { return len(args) > 0 && strings.EqualFold(args[0], "setup") },
		Handler: func(ctx context.Context, c *Client, args []string) (bool, error) {
			return true, handleTelegramCommand(ctx, c, args)
		},
	},
	{
		Canonical: "/exit", Aliases: []string{"/quit"}, Description: "quit", Category: "General", Order: 70,
		Arguments: SlashCommandNoArguments, Behavior: SlashCommandImmediate, Runtime: SlashCommandRuntimeAny,
		CapturePolicy: SlashCommandCaptureOutput, AvailableWhileProcessing: true, TelegramDisabled: true, Suggestions: []string{"/exit", "/quit"},
		Handler: func(_ context.Context, _ *Client, _ []string) (bool, error) { return false, nil },
	},
}

func init() {
	for i := range slashCommandRegistry {
		if slashCommandRegistry[i].Canonical == "/help" {
			slashCommandRegistry[i].Handler = handleHelpSlashCommand
		}
	}
}

func handleHelpSlashCommand(_ context.Context, c *Client, _ []string) (bool, error) {
	printSlashHelp(c)
	return true, nil
}

func findSlashCommand(name string) (SlashCommandDefinition, bool) {
	name = strings.ToLower(name)
	for _, command := range slashCommandRegistry {
		for _, candidate := range command.names() {
			if strings.EqualFold(candidate, name) {
				return command, true
			}
		}
	}
	return SlashCommandDefinition{}, false
}

func validateSlashCommandRegistry(commands []SlashCommandDefinition) error {
	seenNames := make(map[string]string)
	seenSuggestions := make(map[string]string)
	previousOrder := -1
	for _, command := range commands {
		if command.Canonical == "" || command.Handler == nil {
			return fmt.Errorf("slash command requires canonical name and handler")
		}
		if command.Order <= previousOrder {
			return fmt.Errorf("slash command %s order must be strictly increasing", command.Canonical)
		}
		previousOrder = command.Order
		if command.Canonical != strings.ToLower(command.Canonical) {
			return fmt.Errorf("slash command canonical name %q must be lowercase", command.Canonical)
		}
		for _, name := range command.names() {
			name = strings.ToLower(name)
			if !strings.HasPrefix(name, "/") || len(name) == 1 {
				return fmt.Errorf("slash command %q is missing a valid / name", name)
			}
			if prior, ok := seenNames[name]; ok {
				return fmt.Errorf("slash command %q is registered by both %s and %s", name, prior, command.Canonical)
			}
			seenNames[name] = command.Canonical
		}
		for _, suggestion := range command.Suggestions {
			fields := strings.Fields(suggestion)
			if len(fields) == 0 {
				return fmt.Errorf("slash command %s has an empty suggestion", command.Canonical)
			}
			resolved, ok := findSlashCommand(fields[0])
			if !ok || resolved.Canonical != command.Canonical {
				return fmt.Errorf("slash command %s suggestion %q must use its canonical name or alias", command.Canonical, suggestion)
			}
			if prior, ok := seenSuggestions[suggestion]; ok {
				return fmt.Errorf("slash suggestion %q is registered by both %s and %s", suggestion, prior, command.Canonical)
			}
			seenSuggestions[suggestion] = command.Canonical
			if command.suggestionBehavior(suggestion) == SlashCommandTemplate && command.Arguments != SlashCommandRequiredSubcommand {
				return fmt.Errorf("slash command %s template suggestion requires arguments", command.Canonical)
			}
		}
	}
	return nil
}
