# Build Agent Go CLI

\`bacli\` is a cross-platform terminal client for ServiceNow Build Agent. It combines AI-assisted conversations and tool execution with a local application-development workflow, keeping source projects and builds on your machine while synchronizing the relevant ServiceNow state.

## Install and key features

macOS or Linux:

\`\`\`bash
curl -fsSL https://nowdemo.it/bacli/install.sh | sh
\`\`\`

Windows PowerShell:

\`\`\`powershell
irm https://nowdemo.it/bacli/install.ps1 | iex
\`\`\`

Configure an instance, then start the interactive client:

\`\`\`bash
bacli --setup
bacli
\`\`\`

- **Local project scaffolding and builds:** manage app projects locally, check Node/npm prerequisites, and build/install applications.
- **Glide VFS synchronization:** use conflict-aware \`/sync status\`, \`/sync pull\`, and \`/sync push\` to reconcile app files.
- **Workspaces and conversations:** switch Web UI workspaces, apps, and persisted Build Agent conversations.
- **Multiple instances:** isolate credentials, projects, workspaces, apps, and conversations by profile.
- **Telegram channel:** pair a private bot for headless or companion Build Agent interactions.

For architecture, behavior, protocols, safety guarantees, and operational details, see [SPECS.md](SPECS.md).

## Command reference

Use \`bacli --help\` for the generated flag help and \`/help\` in the interactive client for commands available in the selected transport.

### Command-line parameters

| Parameter | Brief description |
| --- | --- |
| \`--setup\` | Interactively configure and save the selected profile's ServiceNow instance. |
| \`--instance <url>\` | Use or save a ServiceNow instance URL. |
| \`--instance-list\`, \`--instances\` | List configured instances and exit. |
| \`--instance-delete <profile\\|fqdn\\|url>\` | Delete an instance profile and its saved state. |
| \`--profile <name>\` | Select the runtime profile (default: \`default\`). |
| \`--profile-list\` | List profiles and exit. |
| \`--profile-delete <name>\` | Delete a non-active profile and exit. |
| \`--project-root <path>\` | Set the profile's canonical local app-project root. |
| \`--conversation <latest\\|id\\|prefix\\|new>\` | Select or create a conversation before prompts or the REPL. |
| \`--prompt <text>\` | Send a prompt after connecting; repeat for scripted multi-turn use. |
| \`--provider <name>\`, \`--model <name>\` | Override the configured model provider or large model. |
| \`--turn-timeout <duration>\` | Set the per-turn timeout (default: 10 minutes). |
| \`--nirvana\` | Use the default Glider Build Agent streaming WebSocket transport. |
| \`--web-gateway\` | Use the legacy Build Agent gateway/AMB compatibility transport. |
| \`--code-assist-ws\` | Use the experimental Code Assist WebSocket diagnostic transport. |
| \`--ws-url <url>\` | Override the Build Agent WebSocket endpoint. |
| \`--auth <form\\|cookie\\|basic>\` | Choose web-session authentication; \`basic\` is for \`--web-gateway\`. |
| \`--user <username>\` | Prefill the web-gateway Basic/form username; the password is prompted. |
| \`--no-open\` | Print the OAuth URL without opening a browser. |
| \`--logout\` | Remove saved session and OAuth credentials for the selected profile. |
| \`--session-status\` | Show safe saved-session status and exit. |
| \`--auto-approve\` | Accept supported approval/client prompts automatically. |
| \`--application-id-list <ids>\` | Supply comma-separated app IDs for conversation listing. |
| \`--debug-file <file>\`, \`-debug <file>\`, \`--debug <file>\` | Write terminal output and a redacted debug trace to a file. |
| \`--advertise-local-tools\` | Experimental local-tool advertisement; not for normal operation. |
| \`--telegram-setup\` | Run the local hidden-input Telegram setup/reconfiguration wizard. |
| \`--telegram-only\` | Run headlessly through the paired private Telegram channel. |
| \`--telegram-status\` | Show Telegram pairing/configuration status without connecting. |
| \`--telegram-approve <code>\` | Approve a pending Telegram pairing code locally. |
| \`--version\`, \`-version\` | Print the embedded bacli version and exit. |
| \`--help\`, \`-h\` | Print generated flag help and exit. |

### Interactive \`/\` commands

| Command | Brief description |
| --- | --- |
| \`/help\`, \`/?\` | Show commands available in the current transport. |
| \`/exit\`, \`/quit\` | Leave the interactive client. |
| \`/instance\`, \`/instances\` | Open the instance picker; use \`current\`, \`list\`, or \`use <profile-or-instance>\` for direct control. |
| \`/conversation\`, \`/conv\` | Open the conversation picker; supports \`current\`, \`list\`, \`new\`, and \`use <selector>\`. |
| \`/workspace\`, \`/ws\` | Open the workspace picker; supports \`current\`, \`list\`, \`new <name>\`, \`use <name>\`, \`reset\`, and \`delete <name>\`. |
| \`/app\`, \`/application\` | Open the app picker; supports \`current\`, \`list\`, \`use <scope> [name]\`, and \`clear\`. |
| \`/sync [status\\|pull\\|push]\` | Compare or synchronize the active app's local source with Glide VFS. |
| \`/project <…>\`, \`/projects\` | Manage checkouts: \`current\`, \`list\`, \`use\`, \`clone-here\`, \`primary\`, or \`forget\`. |
| \`/attach\`, \`/attachments\` | Manage next-turn files: \`list\`, \`add <path>\`, \`paste\`, \`remove <item>\`, \`clear\`, or \`clear-all\` (Nirvana). |
| \`/mcp list\` | List MCP servers advertised to Nirvana (Nirvana only). |
| \`/status [--json]\` | Show offline-safe runtime health. |
| \`/turn [--json]\` | Show the active or most recent redacted turn. |
| \`/support-bundle [path] [--json]\` | Create a local redacted support archive. |
| \`/search <query> [--json] [--limit N]\` | Search local safe metadata and redacted events. |
| \`/export [path] [--json]\` | Create a local redacted conversation export. |
| \`/debug <status\\|on\\|off\\|tail\\|event>\` | Control or inspect local redacted diagnostics. |
| \`/goal [list\\|add\\|done\\|cancel] [--json]\` | Manage local durable goals. |
| \`/approvals [--json]\` | List pending or recent local approvals. |
| \`/approve <id>\`, \`/reject <id>\` | Resolve a pending approval exactly once. |
| \`/telegram <…>\` | Inspect or manage the private channel: \`status\`, \`setup\`, \`approve\`, \`enable\`, \`disable\`, \`policy\`, \`allow\`, or \`revoke\`. |
