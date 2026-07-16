package main

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

type telegramBotCommandDefinition struct {
	Name        string
	Canonical   string
	Description string
}

// telegramPublishedCommands is derived from the same registry as the terminal
// menu so the BotFather command menu cannot silently drift behind bacli. A
// transport alias is used for the one canonical name Telegram cannot accept.
func telegramPublishedCommands() []telegramBotCommandDefinition {
	commands := []telegramBotCommandDefinition{
		{Name: "start", Canonical: "/start", Description: "Start or show Telegram help"},
		{Name: "cancel", Canonical: "/cancel", Description: "Cancel your active Build Agent turn"},
	}
	for _, command := range slashCommandRegistry {
		if command.Hidden {
			continue
		}
		name := telegramBotCommandName(command.Canonical)
		if name == "" {
			continue
		}
		description := telegramPublishedCommandDescription(command)
		if runes := []rune(description); len(runes) > 256 {
			description = string(runes[:256])
		}
		commands = append(commands, telegramBotCommandDefinition{Name: name, Canonical: command.Canonical, Description: description})
	}
	commands = append(commands, telegramBotCommandDefinition{Name: "whoami", Canonical: "/whoami", Description: "Show your numeric Telegram user ID"})
	return commands
}

func telegramPublishedCommandDescription(command SlashCommandDefinition) string {
	description := singleLineLabel(command.Description)
	switch command.Canonical {
	case "/exit":
		description = "Host-local quit command; cannot stop bacli remotely"
	case "/telegram":
		description = "Manage Telegram; setup wizard is host-local"
	}
	return description
}

func telegramBotCommandName(canonical string) string {
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(canonical)), "/")
	name = strings.ReplaceAll(name, "-", "_")
	if name == "" || len(name) > 32 {
		return ""
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return ""
		}
	}
	return name
}

func validateTelegramPublishedCommands(commands []telegramBotCommandDefinition) error {
	if len(commands) == 0 || len(commands) > 100 {
		return fmt.Errorf("Telegram command menu must contain between 1 and 100 commands")
	}
	seen := make(map[string]bool, len(commands))
	for _, command := range commands {
		if command.Name == "" || len(command.Name) > 32 || telegramBotCommandName("/"+command.Name) != command.Name {
			return fmt.Errorf("invalid Telegram command name %q", command.Name)
		}
		if seen[command.Name] {
			return fmt.Errorf("duplicate Telegram command name %q", command.Name)
		}
		seen[command.Name] = true
		description := []rune(strings.TrimSpace(command.Description))
		if len(description) == 0 || len(description) > 256 {
			return fmt.Errorf("invalid Telegram command description for %q", command.Name)
		}
	}
	return nil
}

func telegramCanonicalCommand(command string) string {
	command = strings.ToLower(strings.TrimSpace(command))
	for _, published := range telegramPublishedCommands() {
		if command == "/"+published.Name {
			return published.Canonical
		}
	}
	if definition, ok := findSlashCommand(command); ok {
		return definition.Canonical
	}
	return command
}

type telegramCommandContextKey struct{}

type telegramCommandSource struct {
	ChatID string
	UserID string
}

func withTelegramCommandContext(ctx context.Context, chatID, userID string) context.Context {
	return context.WithValue(ctx, telegramCommandContextKey{}, telegramCommandSource{ChatID: chatID, UserID: userID})
}

func telegramCommandSourceFromContext(ctx context.Context) (telegramCommandSource, bool) {
	if ctx == nil {
		return telegramCommandSource{}, false
	}
	source, ok := ctx.Value(telegramCommandContextKey{}).(telegramCommandSource)
	return source, ok && source.ChatID != "" && source.UserID != ""
}

func telegramRemoteSlashLine(command, argument string) (string, bool) {
	command = telegramCanonicalCommand(command)
	definition, ok := findSlashCommand(command)
	if !ok {
		return "", false
	}
	argument = strings.TrimSpace(argument)
	if argument == "" {
		// Terminal defaults for these commands open a picker or perform an
		// implicit sync. A tapped Telegram menu entry must always have a useful,
		// deterministic non-modal meaning.
		switch definition.Canonical {
		case "/conversation", "/workspace", "/app", "/instance", "/project":
			argument = "current"
		case "/sync":
			argument = "status"
		case "/mcp":
			argument = "list"
		case "/attach":
			argument = "list"
		case "/telegram":
			argument = "status"
		}
	}
	line := definition.Canonical
	if argument != "" {
		line += " " + argument
	}
	return line, true
}

func telegramRemoteHelp(c *Client) string {
	var out strings.Builder
	out.WriteString("Send any private text message to run a Build Agent turn; /ask remains a compatibility alias.\n")
	out.WriteString("/cancel cancels your active turn. Replies to an interview or approval are accepted directly while it is waiting.\n\n")
	out.WriteString("BACLI commands:\n")
	for _, command := range slashCommandRegistry {
		if command.Hidden {
			continue
		}
		name := command.Canonical
		if menuName := telegramBotCommandName(name); menuName != strings.TrimPrefix(name, "/") {
			name = "/" + menuName + " (" + command.Canonical + ")"
		}
		description := telegramPublishedCommandDescription(command)
		if reason := telegramCommandRuntimeNote(command, c); reason != "" {
			description += " [" + reason + "]"
		}
		fmt.Fprintf(&out, "%s - %s\n", name, description)
	}
	out.WriteString("/whoami - show your numeric Telegram user ID\n")
	out.WriteString("\nCommands that normally open a terminal picker use a safe current/status default when tapped; supply an explicit subcommand such as list, use, add, pull, or push for the full command behavior. The bot is an authenticated control plane for this bacli process, so mutating commands have the same effects as running them locally.")
	return strings.TrimSpace(out.String())
}

func telegramCommandRuntimeNote(command SlashCommandDefinition, c *Client) string {
	if c == nil {
		return "runtime availability is unknown"
	}
	switch command.Runtime {
	case SlashCommandRuntimeNirvana:
		if !c.opts.Nirvana {
			return "unavailable in the current web-gateway runtime"
		}
	case SlashCommandRuntimeNoCodeAssistWS:
		if c.opts.CodeAssistWS {
			return "unavailable in the current Code Assist runtime"
		}
	}
	return ""
}

func telegramSafeRemoteText(text string, limit int) string {
	text = safeSnapshotString(text)
	if containsInlineAttachmentData(text) {
		text = redactedDebugAttachmentValue
	}
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, text)
	text = strings.TrimSpace(text)
	if limit > 0 {
		runes := []rune(text)
		if len(runes) > limit {
			text = string(runes[:limit]) + "…"
		}
	}
	return text
}
