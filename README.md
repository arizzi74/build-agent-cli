# Build Agent Go CLI

`bacli` is a cross-platform terminal client for ServiceNow Build Agent. It combines AI-assisted conversations and tool execution with a local application-development workflow, keeping source projects and builds on your machine while synchronizing the relevant ServiceNow state.

## Quick install

macOS or Linux:

```bash
curl -fsSL https://nowdemo.it/bacli/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://nowdemo.it/bacli/install.ps1 | iex
```

Configure your first ServiceNow instance, then start the interactive client:

```bash
bacli --setup
bacli
```

## Key features

- **Local project scaffolding and builds:** scaffold and maintain app projects under a configurable local root, prepare app-local Node/npm dependencies, run the bundled project build workflow, and package/install the result on the instance.
- **Glide VFS synchronization:** use conflict-aware `/sync status`, `/sync pull`, and `/sync push` workflows to reconcile local scaffolding with the application's Glide VFS files on the ServiceNow instance.
- **Workspaces and conversations:** discover and switch Web UI workspaces, refresh their Glider folders and working set, and create, resume, and synchronize server-backed Build Agent conversations and history.
- **Multiple ServiceNow instances:** keep credentials, OAuth sessions, projects, workspaces, apps, and conversations isolated in per-instance profiles, with safe interactive switching.
- **Telegram channel:** connect an authenticated private bot as a companion to the TUI or as a headless channel, with Build Agent turns, commands, progress, interactions, cancellation, and transcript parity.

## Installation and updates

The installer selects the correct published binary, verifies its SHA-256 checksum, creates `~/.local/bin` when needed, installs the executable as `bacli` (`bacli.exe` on Windows), and adds that directory to the user's PATH without duplicating existing entries. Open a new terminal if the installer reports that PATH was changed.

Every released `bacli` checks `https://nowdemo.it/bacli/version.json` at startup. If a newer version exists, it downloads and verifies the matching platform binary, installs/stages the update, exits, and prints a message asking you to relaunch `bacli`. Update-check failures are non-fatal; set `BACLI_NO_UPDATE=1` to disable the check temporarily.

Supported release platforms are Linux arm64, Linux amd64, macOS Intel, macOS Apple Silicon, Windows amd64, and Windows ARM64. See [BUILD.md](BUILD.md) for artifact names, publication ordering, release commands, and update-manifest details.

## Runtime and reliability

The primary/default mode is now the Glider Build Agent Nirvana websocket transport, matching the web UI path observed in Chrome HARs. It supports real streaming and advertises the same WDF/static MCP server configs as the Glider client. The older web Build Agent gateway/AMB transport remains available with `--web-gateway` for compatibility testing.

### Durable goals and local approvals

`/goal` keeps versioned, private, profile-local goal metadata only (safe labels/references, never prompt or assistant bodies). Use `/goal add <title>`, `/goal done <id>`, `/goal cancel <id>`, `/goal`, or `/goal --json`. An active goal is captured into the next immutable turn snapshot; changing it during a turn affects later turns.

`/approvals`, `/approve <id>`, and `/reject <id>` provide local, expiring, exactly-once approval requests. Workspace deletion is now explicitly gated: `/workspace delete <name>` creates an approval and does not delete until `/approve <id>`. No existing remote operation was retroactively gated. Both stores use private atomic profile-local files and recover corrupt local files by quarantine when a mutating goal/approval command runs; read-only diagnostics only report an in-memory degraded view. Approval execution is serialized across local processes and interrupted executions are never replayed.

### Centralized retry, fallback, and attempt telemetry

`getJSON` (metadata/app/conversation/MCP GET/list reads), `getRaw` (workspace discovery), and `sessionValidationGET` use a centralized bounded retry policy: at most three attempts and five seconds elapsed by default, retrying only transport/network errors, HTTP 408, 429, and 5xx. It honors `Retry-After` seconds or HTTP dates capped by `MaxDelay` and remaining elapsed budget, uses bounded exponential backoff, checks context cancellation/deadline before every attempt/sleep, and is clock/sleeper/jitter injectable for deterministic tests. POST/PUT/PATCH writes, message/conversation/app/workspace changes, tool execution, telemetry writes, and user-turn submission are deliberately never automatically retried. A bounded local attempt ring records only operation/attempt, path-only endpoint label, transport, timing, status/error category, retry delay/decision, and fallback reason—never URLs with queries, headers, bodies, prompts, tokens, cookies, or credentials. Central safe reads preserve only a sanitized response `Content-Type` for callers and cap metadata bodies at 1 MiB (session validation at 2 MiB); oversized bodies are discarded with a typed safe error. `/status` reports process-observed retry counts; active `/turn` reports only attempts recorded after its turn boundary (completed last-turn telemetry is explicitly process-observed). Typed `retry_scheduled`, `retry_attempted`, `retry_exhausted`, and `transport_fallback` semantic events are journaled/redacted; legacy core-gateway→legacy-send fallback is observable only before semantic assistant/tool/elicitation/terminal progress, preserving one logical user turn.

### Internal semantic lifecycle seam

The Nirvana websocket lifecycle now also emits a small, versioned internal semantic-event envelope into a deterministic turn reducer. This is an observational **Nirvana-only** compatibility seam: existing rendering, remote persistence, telemetry, and transport behavior remain unchanged, and web-gateway/Code Assist paths do not initialize it. The envelope contains only normalized/redaction-safe diagnostic metadata (never raw frames, OAuth tokens, cookies, passwords, authorization headers, or metadata values containing bearer/basic/cookie header material). Event IDs are client-lifecycle unique while `Sequence` remains per turn; the reducer binds the server turn ID at `turn_start`, deduplicates identical event-ID replays, rejects conflicting duplicates and invalid lifecycle/tool transitions, and covers connection, turn acceptance/start/terminal states, assistant output, tools, elicitation, usage, observed app/working-set updates, retry/fallback, and telemetry failures. Additional transports will migrate incrementally.

### Immutable per-turn runtime snapshot

At the Nirvana message submission boundary, the CLI captures a deep-copied, redaction-safe `TurnRuntimeSnapshot`. The active turn's outbound message context and `turn_accepted` semantic context use that snapshot rather than rereading mutable app, workspace, conversation, working-set, MCP, and effective model settings. It records normalized profile/instance/host, transport and auth capability labels (never credential values), conversation/workspace/app identity, deterministic working-set and MCP hashes, Web UI workspace metadata when available, effective model/skill settings, timeout configuration, and CLI build references. App/workspace/conversation/MCP/config changes received while a turn is active therefore affect a later turn, not the submitted request. OAuth tokens, cookies, passwords, `g_ck`, authorization headers, and raw credential material are excluded from snapshots, semantic events, journals, and tests.

### Deterministic local support bundle

`/support-bundle [path]` creates a local ZIP support artifact without network calls or remote state changes. With no path it writes a unique `support-<UTC timestamp>.zip` under the profile-local private `support/` directory; an explicit path must be a safe relative `.zip` destination and is never overwritten. The archive has stable member order, stored ZIP entries with normalized timestamps/modes, and a `manifest.json` (`schemaVersion: 1`) containing per-member SHA-256/size. Its fixed allowlist is `manifest.json`, `status.json`, `turn.json`, `events.json`, `journal.json`, `inventory.json`, `config.json`, and `environment.json`. It contains only redacted diagnostic summaries: no OAuth/session/cookie files, passwords, `g_ck`, headers, browser data, raw logs/frames, prompts/content, attachments, source/workspace files, HOME paths, or usernames. The artifact is mode `0600`; profile-local parent directories are mode `0700`.

### Local semantic recovery journal

For Nirvana turns, each already-redacted semantic event is also fsync-appended to a versioned local JSONL journal at `~/.ba-cli/profiles/<profile>/workspaces/<workspace>.semantic.jsonl` (file mode `0600`; state/journal directories mode `0700`). The journal has a workspace-monotonic sequence plus a workspace/conversation scope sequence, while the embedded semantic event retains existing per-turn sequence semantics. Workspace JSON remains the compact startup snapshot and stores only a local journal checkpoint. On startup, absent, corrupt, or behind snapshots are recovered by replaying journal records after that checkpoint; a corrupt trailing partial JSONL line is safely ignored, while earlier corruption fails recovery rather than being hidden. A crash during an active local turn is deterministically treated as an abandoned local turn: partial stream text is not converted into conversation history and no remote ServiceNow state is changed. Credentials and credential-bearing metadata/payload strings are rejected before append (OAuth tokens, passwords, cookies, `g_ck`, Authorization material, raw frames/headers). Retention archives the oldest records once a journal exceeds 1,000 entries, keeps a 500-entry live tail, and retains at most five private local archives; recovery validates a contiguous archive-plus-live sequence chain, safely deduplicates exact archive/live rotation overlap, and rejects sequence gaps or conflicting overlaps. If retention has pruned history older than the available archives, recovery requires a workspace snapshot checkpoint at or after the first retained record; replay then reduces only records after that checkpoint, whose first lifecycle record must be a safe boundary (`connection_state_changed` or `turn_accepted`). Otherwise startup fails explicitly rather than reconstructing unsafe/incomplete state. Before append, an interrupted malformed final JSONL record is safely truncated so the next record cannot be joined to corrupt bytes. If startup finds an unsafe post-checkpoint lifecycle boundary and every uncheckpointed record belongs to a different non-empty conversation than the valid workspace snapshot, the chain is provably stale: it is moved intact into a private `semantic-quarantine/` directory, the checkpoint is reset, and startup resumes from the snapshot with a warning. Same-conversation or otherwise ambiguous lifecycle damage still fails explicitly. If a turn's initial `turn_accepted` boundary cannot be journaled, the remainder of that turn is skipped so a future restart never sees a journal beginning mid-turn. This never mutates remote ServiceNow state.

## Auth

Default Nirvana mode is session-first: it validates and reuses the matching saved ServiceNow web session, or performs the existing form login and persists `session.json` before deriving OAuth PKCE authorization from that authenticated session. `--auth cookie` and `--auth basic` remain explicit recovery/compatibility choices. If session-derived OAuth cannot complete, the CLI warns and falls back to the manual PKCE code flow. It prints the browser URL only when it cannot open a browser (or `--no-open` is used); the URL contains only transient PKCE/state parameters, never cookies, passwords, client secrets, authorization codes, or tokens. The legacy `--web-gateway` mode remains pure Go.

```text
GET/POST <instance>/login.do
GET      <instance>/api/sn_build_agent/build_agent_api/providerConfig
GET      <instance>/api/sn_ba_core/agent_config_api/config
POST     <instance>/api/sn_ba_core/agent_gateway_api/conversation
POST     <instance>/amb
```

If the scoped `sn_ba_core` REST route is not installed on the instance, the CLI falls back to the older installed Build Agent API:

```text
POST <instance>/api/sn_build_agent/build_agent_api/send
```

The password is never stored. Use `--user <username>` to prefill only the username. If you specifically want Basic Authentication instead of a reusable web session, pass `--auth basic` explicitly.

Additional pure-Go web auth modes:

```bash
# ServiceNow form login for the legacy web gateway: GET/POST /login.do, validate, then persist cookies + g_ck under ~/.ba-cli
go run . --web-gateway --instance https://myinstance.service-now.com

# Explicit Basic Auth is still available for the legacy web gateway.
go run . --web-gateway --auth basic --instance https://myinstance.service-now.com

# Manual browser-cookie import for the legacy web gateway: paste Cookie: and optional X-UserToken/window.g_ck.
# The imported session is validated before saving and then reused for the same profile+instance.
go run . --web-gateway --auth cookie --instance https://myinstance.service-now.com

# Inspect saved web session or delete all saved authentication while preserving the instance profile.
go run . --session-status --profile default
go run . --logout --profile default
```

Saved web sessions live here and are written mode `0600`. Newly imported/form-login sessions are validated before the file is created; expired saved sessions are deleted automatically on reuse.

```text
~/.ba-cli/profiles/<profile>/session.json
```

Nirvana mode mirrors the Glider Build Agent web client / TypeScript CLI OAuth PKCE flow and is the default streaming/MCP parity path for web clients that use `/sncapps/code/assist/ba/nirvana/web-socket`:

```bash
go run . --instance https://myinstance.service-now.com
```

Nirvana OAuth details:

- auth endpoint: `<instance>/oauth_auth.do`
- token endpoint: `<instance>/oauth_token.do`
- client id default: `b77993a2359e472cad99679d1f124919`
- redirect URI default: `/api/sn_build_agent/build_agent_api/oauth_redirect`
- OAuth token is sent inside the Build Agent websocket handshake as `params.arguments.token`
- The websocket `connect` advertises web-client-compatible streaming, IDE, elicitation, search, fluent-docs, and safe tool-execution capabilities. Client elicitation parity includes remote active-app Glider-VFS `fs_copy`, `fs_move`, and `fs_find_and_replace`; all reject path escape, overlapping source/destination trees, and protected XML mutations and never operate on arbitrary local disk. Copy/move support recursive trees, collision/overwrite checks, bounded fetched content, cancellation, deterministic mutation ordering, and exact post-apply type/content/stale-entry verification. Find/replace exact mode is literal; regex mode uses Go's RE2-compatible syntax and replacement expansion, which differs from JavaScript regex behavior in some edge cases.
- `open_app` validates authoritative `sys_app` metadata before selecting the active bacli application; it does not launch VS Code, convert an app, or modify unrelated profile settings. `ui_diagnostics` returns `UI_DIAGNOSTICS_UNAVAILABLE` because browser-preview diagnostics are unavailable in the terminal client and exposes no credentials. MCP list actions report available backend-managed Nirvana/WDF configurations with client connection state explicitly unknown; connect/disconnect return `MCP_CLIENT_TRANSPORT_UNAVAILABLE` because bacli cannot create or tear down backend MCP transports. Dynamic backend MCP tool schemas cannot be enumerated by bacli.

Profile instance config is stored under this Go CLI's own state directory:

```text
~/.ba-cli/profiles/<profile>/config.json
```

Token cache is always a local file, on both macOS and Linux. The CLI never uses Keychain:

```text
~/.ba-cli/profiles/<profile>/oauth-token.json
```

The token file is written with `0600` permissions.

## Build

```bash
go mod tidy
go build ./...
```

## Run

Interactive default Nirvana mode:

```bash
go run . --instance https://myinstance.service-now.com
```

In interactive default/Nirvana mode the CLI restores the active profile's last workspace and last-opened Build Agent conversation before the `ba>` prompt starts, without showing a startup conversation picker. Pass `--conversation latest|<id>|<id-prefix>|new` when you explicitly want to override the restored conversation. Scripted `--prompt` runs also skip any picker unless you pass `--conversation latest|<id>|<id-prefix>|new`.

Authentication and any username/password/cookie prompts complete before the animated connecting screen, so animation cannot erase a hidden credential prompt. During the actual network handshake the screen shows `Esc cancel`; pressing Esc once cancels the connection, restores the original terminal screen, and exits cleanly. The interactive REPL enters the terminal alternate screen before connect/startup conversation restore so all CLI launch output and restored conversation scrollback appear inside the app screen, and the pre-launch shell screen is restored on exit; alternate-screen scrollback is preserved by default so users can scroll through the Build Agent transcript. Set `BA_CLI_CLEAR_ALT_SCROLLBACK=1` only if a terminal exposes stale redraw frames and you prefer clearing alternate-screen history. Assistant output is rendered with a small terminal Markdown formatter for completed/blocking responses and conversation-history scrollback, so headings, code blocks, tables, bold text, inline code, and bullets look closer to the Build Agent web UI instead of dumping raw Markdown. In an interactive terminal, saved user prompts use a subtle gray background, while the live `ba>` composer grows from one to five content rows, soft-wraps by terminal display cells, and scrolls to keep the editing cursor visible. Unicode grapheme editing keeps combining text and emoji sequences intact and places the cursor correctly for CJK and other wide glyphs. Assistant responses are bullet-prefixed with roomier vertical spacing, and active turns show `• Building (elapsed • esc to interrupt)`: the leading bullet pulses green, `Building` carries the wasabi-green bounce glow, and the elapsed time plus interrupt hint use the terminal's regular text color. The elapsed clock starts at the original send boundary and survives blocking approval prompts or footer redraws; pressing Esc once interrupts the active turn. The CLI reserves a terminal footer: a spacer/activity/spacer block sits above the dynamic composer band, a colorful background-free status bar sits below it, and a blank temporary-message row sits at the bottom. Idle prompt redraws clear the row immediately above the gray prompt band so startup/help text or the last transcript line never touches the prompt directly. The status footer stays persistent while typing, while the Building activity animates above the prompt band with one blank row above and below it, and while streamed assistant output scrolls above the whole footer; slash-command suggestions open above the prompt. Managed redraws are serialized as synchronized terminal updates and apply the cursor only after the frame is painted, avoiding intermediate cursor/background states in macOS terminals such as iTerm2. The animated activity line leaves the terminal's final column unused, avoiding delayed-autowrap footer corruption across macOS and Linux terminals. The terminal UI also keeps a small structured transcript model and, on menu open/close, composer-height change, or debounced terminal resize, rebuilds the managed viewport from that transcript plus fresh footer geometry by clearing rows in place instead of full-screen erasing. For live interactive turns, pressing Enter immediately resets the footer to an empty `ba>` prompt while the submitted prompt is recorded and the managed viewport is replayed before send setup/Building activity/assistant output as a gray prompt block matching loaded conversation history; after that replay, the cursor is explicitly returned to the input footer. While the animated Building phase is active, stdin uses the same Unicode-aware composer and terminal event decoder as the idle prompt, so typeahead arrows, paste, and editing cannot corrupt assistant output; pressing Enter during that phase queues the complete buffered draft as the next prompt. Assistant stream deltas update an active assistant entry in the same managed replay surface used by resize/final completion, so streaming remains visible without direct cursor printing. During footer typeahead capture, the footer prompt is redrawn after each replay; on completion the active entry is replaced by the final recorded answer and the viewport is replayed again, so generated output is aligned deterministically and does not steal the input cursor. The status bar shows the selected model, count of user/input messages in the loaded conversation, active workspace name, selected app name, cached active project path (with the home directory abbreviated as `~`), and the connected instance name. Nirvana mode streams real websocket `stream_delta` text through the terminal Markdown formatter by updating an active assistant entry in the managed replay surface, so partial output remains visible without stealing the footer cursor or erasing scrollback, and tables, bullets, headings, and code finish as formatted terminal output instead of raw Markdown chunks. Set `BA_CLI_LIVE_REPAINT=0` only if you explicitly want the older append-only raw streaming behavior for local debugging. Routine client elicitations/tool pass-through events stay hidden in normal UI. Legacy `--web-gateway` fallback `/send` responses are rendered when the single HTTP response arrives because that endpoint does not expose a stream to the CLI. When you select an existing conversation, the CLI reloads its saved messages, replaces the managed terminal transcript with that conversation history, and immediately replays the viewport/footer before returning to `ba>`; switching conversations no longer waits for a resize/redraw to show the newly selected transcript.

Legacy web gateway mode:

```bash
go run . --web-gateway --instance https://myinstance.service-now.com
```

Resume a previous Build Agent conversation before a scripted prompt:

```bash
go run . --instance https://myinstance.service-now.com \
  --conversation latest \
  --prompt "Continue from the previous context"
```

Scripted single prompt:

```bash
go run . --instance https://myinstance.service-now.com \
  --prompt "What Build Agent tools are available?"
```

Use saved/default profile:

```bash
go run . --profile default
```

Print the embedded release version without checking for updates, authenticating, or connecting:

```bash
go run . --version
```

Force setup, optionally choosing a persistent canonical project root:

```bash
go run . --setup --profile dev
go run . --setup --profile dev --project-root ~/Projects/ServiceNow
```

Change the persisted root of an existing profile without changing its instance settings:

```bash
go run . --profile dev --project-root ~/Projects/ServiceNow
```

List/delete profiles are command-line flags, matching the TypeScript CLI style:

```bash
go run . --profile-list
go run . --profile scratch --profile-delete dev
```

## Useful flags

- `--provider bedrock|openai|anthropic|vertex|nowllm`
- `--model claude-opus-4-6|gemini_large|gpt_large|llm_generic_large_v2|...`
- `--nirvana` explicitly uses the Glider Build Agent Nirvana websocket transport for web UI streaming/MCP parity; this is the default.
- `--web-gateway` uses the older web Build Agent gateway/AMB transport for compatibility testing.
- `--code-assist-ws` is an experimental diagnostic path for `/sncapps/code/assist/ba/web-socket`; keep it off for normal Build Agent/MCP use
- `--conversation latest|<id>|<id-prefix>|new` resumes/selects a Build Agent conversation before scripted prompts or the REPL; works in web gateway and Nirvana mode.
- `--auth form|cookie|basic` selects the ServiceNow web-session recovery/compatibility mode. In default Nirvana mode, `form` is session-first and saved form/cookie sessions are validated and reused for the same profile+instance; `cookie` is the explicit browser-session alternative. `basic` is supported only by `--web-gateway`, because it cannot establish a reusable browser session for session-derived Nirvana OAuth.
- `--user <username>` supplies the ServiceNow username for web gateway basic/form auth; password is still prompted
- `--session-status` shows saved web session metadata without printing cookies/tokens
- `-version` / `--version` prints the embedded bacli release version and exits offline
- `--logout` deletes both the saved web session and cached OAuth token for the active profile while preserving its instance/workspace configuration. Use `--instance-delete <profile|fqdn|url>` when the entire configured instance and its state should be removed.
- `--profile <name>` chooses the ServiceNow runtime profile under `~/.ba-cli/profiles/<name>/`; Telegram configuration and authorization are global and do not live in that profile.
- `--project-root <absolute-path|~/path>` sets that profile's canonical automatic-project root and persists it for an existing configured profile; use it with `--setup` when creating/updating a profile. Omit it to use the backward-compatible default `~/BA`. Relative paths, `~otheruser`, symlink roots/ancestors, and non-directory ancestors are rejected.
- `--profile-list` lists configured profiles and exits
- `--profile-delete <name>` deletes a profile directory and exits; it refuses to delete the active `--profile`
- `--auto-approve` for approval prompts
- `--telegram-setup` opens the local guided Telegram bot setup/reconfiguration wizard; run it in an interactive terminal so the pasted new or existing bot token stays hidden. Pairing policy can complete a fresh-machine user pairing inside the same wizard with a target-issued challenge.
- `--telegram-only` runs bacli headlessly as a private Telegram command channel; it uses the global bot configuration and the selected `--profile` only as its ServiceNow runtime context.
- `--telegram-status` shows the global Telegram configuration, paired numeric user IDs, and pending pairing codes without connecting to ServiceNow or Telegram.
- `--telegram-approve <code>` globally approves a pending one-hour Telegram pairing code locally and exits.
- `-debug <filename>`, `--debug <filename>`, or `--debug-file <filename>` to write a full log to a file: regular terminal output is still shown normally and is also copied to the file, while redacted debug trace output is written only to the file; bare `--debug` without a filename is intentionally invalid
- `--no-open` to avoid launching a browser for OAuth
- `--advertise-local-tools` exists only for experimentation; local tools are not implemented yet, so keep it off for normal backend/MCP use

## Interactive commands

Profiles stay command-line-only. The interactive slash-command picker exposes the regular CLI helpers, but only the bare conversation/workspace picker commands:

```text
/help
/exit | /quit
/conversation
/mcp list
/workspace
/app
/attach
/sync status
/project current
/telegram status
```

`/conversation` opens the conversation picker, where you can select an existing Build Agent conversation or choose `New conversation`. Conversation rows explicitly show their scope as `📦 <application name> · <title>` or `🌐 Global / no app · <title>`; an app-bound row with unavailable app metadata safely shows a shortened app ID instead. The saved workspace conversation is marked `🕘 Last used`, and when global rows are present the picker/list explains once that they are available across workspaces. `/workspace` opens the workspace picker, then proposes the conversations scoped to the selected workspace with that workspace's last-used conversation preselected. `/app` opens an app picker for apps in the active workspace. Parameterized conversation commands such as `/conversation current`, `/conversation list`, `/conversation new`, and `/conversation use ...`, workspace subcommands such as `/workspace list` and `/workspace use ...`, plus app subcommands such as `/app current`, `/app use ...`, and `/app clear`, remain backward-compatible for scripts/tests but are no longer advertised in the interactive picker.

The interactive prompt keeps in-memory command history for the current process. ↑/↓ moves through visual wrapped rows first and then command history, preserving the current draft when history navigation returns to it. ←/→ moves by full grapheme, Home/End uses the current logical line, Delete removes the next grapheme, Ctrl/Option+←/→ moves by word, and Ctrl-W deletes the previous word. Ctrl-J and supported Shift-Enter/CSI-u input insert a newline; ordinary Enter submits. Bracketed multiline paste is inserted as one draft instead of submitting partial lines. In the TTY app UI, startup connection/help guidance is not printed into transcript scrollback; instead a compact URL-free `Connected · type a message · /help /instance /conversation /mcp /workspace /app · /exit /quit` hint appears in the temporary message row below the status bar for 5 seconds. Press `/` on an empty prompt to open the slash-command picker; it includes the other helper commands but only one entry for each modal picker. Enter now runs the selected complete slash command immediately, so selected commands like `/instance`, `/workspace`, and `/conversation` execute instead of merely being inserted; Tab inserts the suggestion without running it. Output from non-modal slash commands such as `/help`, `/instance list`, and `/mcp list` is captured into the managed transcript so it stays visible above the fixed footer instead of being overwritten by the prompt. Slash suggestions that require an argument are inserted on Enter instead of executed; the currently advertised picker commands (`/instance`, `/conversation`, `/workspace`, `/app`) execute directly. Pressing Esc closes the picker immediately, clears the typed slash-filter text, and restores the original terminal contents. Pressing Ctrl-D once shows `Press Ctrl-D again to exit ....` in the temporary message row below the status bar; the message clears automatically after 2 seconds unless Ctrl-D is pressed again within that window to exit. The slash-command picker is rendered in the managed footer/viewport area; opening or closing it triggers a transcript replay so the rows behind the menu are restored. The `/conversation`, `/workspace`, and `/app` pickers remain true modals on the terminal alternate screen. The fixed footer/status bar watches terminal resizes and rebuilds the managed viewport from transcript state plus fresh footer dimensions, avoiding the previous black-screen/stale-pixel corruption. In a real terminal, `/conversation` opens a cursor-driven conversation picker: ↑/↓ choose, Enter opens, `n` starts a new local conversation, `q` cancels, and Esc cancels. `/workspace` opens the same style of picker for Web UI/local workspaces: ↑/↓ choose, Enter switches workspace, `q` cancels, and Esc cancels; after a workspace is selected, the CLI immediately opens the workspace-scoped conversation picker with the saved conversation for that workspace preselected. `/app` opens the same style of picker for apps in the active workspace, sourced from `now-file:/<app_sys_id>` workspace folders. A server conversation row is created on the first prompt, using that prompt as the title, so the record matches the web UI instead of leaving an empty `New conversation` row. Conversation listing/selection works in both web gateway and Nirvana mode; the picker is scoped to conversations for apps in the active workspace when workspace app ids are known, while still showing app-less/global conversations that the Web UI includes; Nirvana reuses the loaded server messages as `conversationHistory` and keeps the selected compact conversation id for subsequent websocket turns.

### Images and attachments

Default Nirvana mode supports the image flow observed in the Glider Web UI HAR: bacli uploads each staged file to the active Build Agent conversation, persists Web-compatible attachment metadata on the user message, and sends image bytes to Nirvana as bounded data URLs. Inline bytes are never retained in local workspace history, debug output, semantic journals, exports, or support bundles.

Use `/attach add <path>` on every platform. `/attach list`, `/attach remove <number|id|filename>`, and `/attach clear` manage the next turn's queue. On a bacli process running natively on macOS, Ctrl-V or `/attach paste` reads a PNG, JPEG, or TIFF image from the local clipboard; TIFF is converted with built-in macOS tools. A bacli process running on Linux over SSH cannot read the Mac host clipboard, so use a path visible to that process. The local safety limits are 10 files, 25 MiB per file, and 50 MiB total. Attachments require the default Nirvana transport.

The supplied HAR proves the PNG upload/persistence/WebSocket contract. Other file types use the same conservative metadata and upload path, but instance-side acceptance and ServiceNow limits can vary. Bacli does not call an undocumented remote-delete API: removing an item after it was already uploaded abandons it locally and warns that the remote orphan may remain.

### Local build prerequisites

The bacli executable itself does not require Node.js for chat, conversations, workspace/app selection, or synchronization. Local application builds require `node`; `npm` is additionally required whenever bacli must install missing project dependencies. Interactive startup checks both executables in `PATH` and, when either is unavailable, appends a non-blocking yellow warning to the managed main transcript with installation instructions for macOS, Linux, or Windows. The build path repeats the required checks so a checkout with existing `node_modules` still reports a clear `NODE_NOT_FOUND` error instead of a generic now-sdk failure. Bacli uses the app-local `node_modules/.bin/now-sdk`; no global now-sdk installation is required.

### Private Telegram command channel

Bacli can poll a Telegram bot alongside the terminal, or run headlessly with `--telegram-only`. Configure it with the offline wizard:

```bash
bacli --telegram-setup
```

Run the wizard in an interactive terminal. It can create a bot, reuse an existing bot on a new bacli machine, or rotate an existing bot token:

1. Stop any bacli poller that is still using this bot, including one on another machine.
2. For a new bot, open Telegram's verified `@BotFather`, send `/newbot`, and choose a username ending in `bot`. For an existing bot, obtain its current token from BotFather.
3. Paste the token at bacli's hidden prompt. The token is never accepted as a command-line argument or printed.
4. Bacli validates the token with Telegram's `getMe` method, confirms the bot identity, and saves the channel. Same-bot reconfiguration preserves the existing policy/allowlist by default; an exact-token refresh also preserves pending local requests, while token rotation invalidates them. A different bot never inherits the prior bot's authorization.
5. With the `pairing` policy, choose immediate pairing to have bacli display a fresh `BACLI-…` challenge. Send that exact one-line challenge to the bot in a private Telegram chat. The wizard temporarily polls only for that challenge, verifies `from.id` equals the private chat ID, persists every consumed update, and atomically approves that numeric user.

Telegram channel data is global for the local bacli installation, not duplicated per ServiceNow profile:

```text
~/.ba-cli/telegram/config.json  channel policy and allowlist
~/.ba-cli/telegram/token        bot token
~/.ba-cli/telegram/state.json   bot identity, pairing authority, and update offset
~/.ba-cli/telegram/lock         configuration/state lock
```

On Unix systems, the wizard protects the directory with mode `0700` and its files with mode `0600`. `BACLI_TELEGRAM_BOT_TOKEN` remains an in-memory token fallback for an already enabled global configuration and is not persisted by the wizard. Earlier per-profile Telegram files are not selected or merged automatically; `bacli --telegram-status` reports them and `bacli --telegram-setup` creates the global configuration. The legacy files are left untouched.

The channel accepts private messages, authenticates the numeric Telegram `from.id`, ignores groups, and never authorizes by username. An unknown private sender receives an eight-character code valid for one hour. That normal runtime code is bound to the pending sender record in this machine's private `state.json`; it is not a portable credential and cannot be approved on a different machine by itself. On the bacli host that generated it, inspect and approve it with:

```bash
bacli --telegram-status
bacli --telegram-approve ABCD2345
```

The bot configuration, paired users, and consumed update offset remain the same whichever ServiceNow profile is selected. `--profile` chooses only the ServiceNow instance/client that Telegram commands control at runtime. For predictable unattended operation, start one explicit headless poller and keep it running:

```bash
bacli --telegram-only --profile default
```

If immediate wizard pairing was skipped, open the bot in Telegram, send `/start`, and approve the newly returned code on that same host with `bacli --telegram-approve <code>`. Only one bacli process can poll a given bot token on a host at a time; Telegram also rejects competing pollers on different hosts, so stop the old machine before moving the bot.

Send ordinary text to the bot to start a Build Agent turn; `/ask` has been removed completely. For the full active turn bacli immediately sends Telegram's native `typing` chat action and refreshes it every four seconds, stopping the heartbeat before the final response is sent on success, cancellation, timeout, failure, or service shutdown. During a turn Telegram receives the live Building notice, tool start/completion and capped result summaries, build progress, warnings, summaries, retry/fallback notices, sub-agent lifecycle, usage, and the complete final assistant message. When the TUI is open, each authorized Telegram input—including interaction replies—and the corresponding assistant, command, interaction, cancellation, and presentation-only progress output are also recorded/rendered in the managed terminal transcript; Esc in the idle TUI can interrupt a Telegram-originated active turn. Responses that fit in at most ten Telegram messages remain readable text; larger responses are attached losslessly as bounded UTF-8 `.txt` parts instead of being silently truncated. When Build Agent requests an interview, approval, or application selection, bacli first delivers the complete request and then sends an `Input required [ID] is ready` acknowledgement. Reply directly in the same chat; if an answer begins with `/`, send `/answer <ID> <answer>`. Invalid approval replies are rejected without ending the request. These replies and `/cancel` bypass the serialized command queue so the websocket reader cannot deadlock waiting for terminal input. Only the user/chat that started a Telegram turn can answer or cancel it, command/interaction waits inherit the configured turn timeout, and cancellation acknowledgements are sequenced before the final cancellation result.

The Telegram command menu is generated from the same registry as bacli and publishes every registered top-level command. Telegram requires underscore command names, so `/support-bundle` is published as `/support_bundle` and maps back to the canonical command. Bare commands that normally open a terminal picker use a non-modal `current`, `list`, or `status` default. Explicit subcommands provide the full behavior, including context changes, sync pull/push, project operations, local support/export files, debug controls, approvals, and host attachment paths. `/help` always shows the same published catalog and labels commands unavailable in the current transport. A temporary Telegram menu-refresh failure is reported locally but does not disable the authenticated poller. These are authenticated remote-control operations with the same local and ServiceNow effects as invoking them in bacli; grant bot access only to trusted operators. `/telegram setup` still requires the host's hidden-input wizard, and remote `/exit` cannot terminate the host process. Telegram photo/document ingestion is not yet implemented; `/attach add` reads a path on the bacli host and `/attach paste` reads its clipboard. Pending attachments are tagged with their originating front end so a Telegram turn cannot accidentally consume files queued by the terminal or another user. If an owner abandons that host-wide queue, a trusted operator can explicitly purge it with `/attach clear-all`.

The local `/telegram` command manages global status, guided setup (`/telegram setup`), pairing approval, `pairing|allowlist|disabled` policy, explicit numeric allowlist entries, and revocation. Terminal and Telegram actions are serialized through one client controller except for owner-bound cancellation and active-interaction replies. A remotely requested `/instance use` uses only a usable cached OAuth credential or validated saved browser session—never host stdin, a password prompt, or a browser flow—and explicitly rebinds the poller to the replacement client after a successful switch. If reauthentication is needed, run the switch locally once. If the client is replaced locally instead, Telegram refuses privileged commands until bacli is restarted. Queued commands are reauthorized immediately before dispatch, global configuration/state files are private, changing to a different bot clears paired authority, and rotating a token for the same bot preserves the already-consumed update offset.

### Persistent local app source and `/sync`

Each active app used by automatic sync/build is materialized at exactly `<project-root>/<sanitized-app-name>`—by default `~/BA/<sanitized-app-name>`—for example, app `Bocota` uses `~/BA/Bocota` unless `--project-root` has configured another profile-local root. The root is stored as `projectRoot` in `~/.ba-cli/profiles/<profile>/config.json`; existing profiles without that field continue to use `~/BA`. `~`/`~/...` expands only to the current user's home, and the resulting absolute root is checked conservatively for symlinks/non-directory ancestors before any project directory is created. The directory name never contains an instance hostname or URL. Names preserve case and spaces when filesystem-safe; separators, controls, traversal-only names, and platform-reserved forms are deterministically sanitized into one safe directory component. If no human display name is known, the safe app sys-id is used instead. It contains app source and optional local `node_modules`, `dist`, and `target` build reuse. The CLI never uploads or removes those dependency/build directories.

A private `.ba-cli-sync/manifest.json` inside the app folder records its normalized instance, app ID, Glider root URI, stable checkout ID, scope, and source checksums for conflict-safe synchronization. A canonical folder is accepted only with a v2 manifest whose normalized instance, app ID, and root all match; a legacy v1/no-instance manifest at that shared name is ambiguous and rejected unless that exact path is already bound by the current profile registry, in which case the next mutating sync pins it to v2. A canonical folder bearing the same display name but owned by another instance or app is rejected rather than reused; a pre-existing non-empty canonical folder without a matching manifest is also never silently claimed. Checkout roots must be real directories rather than symlinks.

Each profile maintains a canonical checkout registry keyed by normalized instance URL plus app sys ID. On a mutating sync/build, a verified durable registered primary at any historical path is safely moved into `<project-root>/<sanitized-app-name>` (default `~/BA/...`) and made canonical primary without overwriting anything; an existing matching v2 canonical checkout wins and leaves the historical checkout registered as secondary. Explicit current-process overrides, cwd-discovered checkouts, and secondary clone-here checkouts stay where chosen. `/project current` and `/project list` inspect the active checkout, `/project use <path|checkout-id>` selects a checkout for only the current process, `/project clone-here [directory-name]` explicitly creates a secondary checkout directly below the launch directory, `/project primary <path|checkout-id>` changes the persistent default, and `/project forget <path|checkout-id>` unregisters without deleting files. The first valid registered checkout becomes primary; when a primary is forgotten or stale, a valid checkout is promoted deterministically. `/projects` is an alias for `/project`.

On first use only, if no canonical checkout is selected, bacli also finds the historical `./.build-agent/<instance>/<workspace-hash>/<app-id>/` folder and, when its v2 manifest proves it belongs to the same instance/app/root, renames that project into `<project-root>/<sanitized-app-name>`. It may remove an existing empty canonical directory before that safe rename, but never overwrites a non-empty conflicting folder or removes legacy parent directories.

Use `/sync` to merge local source and Glider files. `/sync status` is read-only and prints sorted `PULL:`, `PUSH:`, and `CONFLICT:` paths; when no checkout exists it compares the remote tree with an empty baseline and reports the expected local path without creating the app directory, registry, sync metadata, or project lock. A later pull can safely adopt the exact lock-only residue left by older status versions, while symlinks, foreign manifests, user files, and unexpected metadata remain collisions.

Remote-only pulls and local-only pushes keep their simple behavior. When local and Web changes coexist, or both sides changed the same path, `/sync pull`, `/sync push`, and build preparation construct one deterministic bidirectional plan. The plan lists every pull, push, overwrite, and deletion together with Local/Web timestamps, then requires an explicit confirmation that `--auto-approve` cannot bypass. Different-path changes merge. For a same-path divergence, timestamps are advisory and choose the proposed newer side only after confirmation; equal/missing timestamps and modification-versus-deletion remain unresolved instead of being guessed. Bacli re-reads both sides after approval, aborts a stale plan before mutation, verifies Local/Web convergence afterward, and advances the manifest only for verified matching paths. A declined or noninteractive build plan is cached for that turn and fingerprint so repeated backend build requests fail immediately instead of reopening the same prompt.

Sync deliberately excludes `node_modules`, `dist`, `target`, `.git`, `.jest_cache`, local caches, sync metadata, the root `package-lock.json`, and generated `.now/bom.json`. The SDK regenerates the BOM with volatile timestamps/identifiers, so it is neither uploaded nor allowed to create the recurring conflict loop. Build/dependency preparation holds the checkout lock through reconciliation, dependency installation, build, generated-file persistence, and packing; unsynchronized state enters the same one-shot reconciliation flow before npm/build. If a build operation encounters an ambiguous non-symlink canonical directory, it compares bounded local/Web state and asks for a separate explicit replacement confirmation, retains the old directory as a sibling backup, and rehydrates a fresh v2 checkout strictly from Web contents.

Workspace state is local to the active profile, but bare `/workspace` prefers the Web UI workspace list when an authenticated ServiceNow web session is available and opens a picker to choose the active workspace. After switching, it opens the conversation picker scoped to that workspace; the workspace's saved conversation id is used as the preselected row when present. The Web UI parity path discovers `window.sn_glider.user.userId` from `/sn_glider_app/ide.do`, calls `POST /api/sn_glider/v2/sync/state` for `settings:/users/<userId>`, filters `workspaces/*.code-workspace`, and switches by number, name, URI, or unique prefix. Backward-compatible `/workspace list` and `/workspace use <name>` subcommands remain available for scripts/tests but are no longer advertised in the interactive picker. Multi-word Web UI workspace names such as `Default - admin` and `Test Workspace` are valid.

Local cache files remain under:

```text
~/.ba-cli/profiles/<profile>/active-workspace
~/.ba-cli/profiles/<profile>/workspaces/<workspace>.json
```

Each workspace persists the Build Agent session context:

- Web UI workspace URI/checksum/description/folders when selected from `settings:/users/<userId>/workspaces/*.code-workspace`
- `conversationId`
- server conversation metadata/title/state when the conversation exists in ServiceNow
- `conversationHistory`
- `workingSet`
- selected app scope metadata

The active application is also cached at:

```text
~/.ba-cli/profiles/<profile>/active-app.json
```

When an app is selected with `/app` (or backward-compatible `/app use`), returned by the backend via `set_app_scope`, or implied by a selected conversation's application metadata, the CLI sends the app scope back on subsequent Build Agent payloads as `appScope`. Selecting a conversation updates the active app like the Web UI when the conversation includes an application id, and clears the active app/status field to `<none>` when the conversation has no app. Creating a new conversation also clears the selected app so the new conversation starts app-less and the status bar shows `app=<none>` until an app is selected or created.

## Transport

Legacy `--web-gateway` mode:

```text
GET  https://<instance>/api/sn_ba_core/conversations_api/conversations
POST https://<instance>/api/sn_ba_core/conversations_api/create
POST https://<instance>/api/sn_ba_core/conversations_api/conversation/<conversationId>/message
POST https://<instance>/api/sn_ba_core/agent_gateway_api/conversation
AMB channel /build_agent_core/stream/<conversationId> via POST https://<instance>/amb
legacy AMB channel fallback /build_agent/stream/<conversationId>
```

The send flow mirrors the web UI: ensure/create a server conversation record titled from the first user prompt, persist user/assistant rows with web-compatible Glider `content` JSON such as `{"id":"...","sender":"user","text":"...","hasCheckpoints":false}` and `{"id":"...","sender":"assistant","text":"...","complete":true,"duration":0}`, subscribe to the AMB stream channel before sending, then call the live Build Agent gateway. Legacy fallback responses are also persisted back as assistant text messages so the web UI conversation shows both sides of the turn.

Some instances expose the older/installed Build Agent API namespace instead of `sn_ba_core`. The CLI falls back automatically when `sn_ba_core` returns “Requested URI does not represent any resource”:

```text
GET/POST https://<instance>/api/sn_build_agent/conversations_api/...
GET/POST https://<instance>/api/sn_build_agent/build_agent_api/conversations...
POST     https://<instance>/api/sn_build_agent/build_agent_api/send
body: { "payload": { "requestId": "<conversationId>", "message": "<prompt>", "messages": [{ "role": "user", "content": "<prompt>" }] } }
```

For the installed `/api/sn_build_agent/build_agent_api/conversations` API, conversation listing now follows the browser HAR shape first: `GET /api/sn_build_agent/build_agent_api/conversations?application_id_list=<appSysIds>&client=ide`. Before the app or conversation picker derives that list, an authenticated Web workspace refreshes its `.code-workspace` file even when the local folder cache is already populated; a successful refresh replaces and persists the cached folders and checksum, so apps added from another machine become visible without reselecting the workspace. The existing cache remains available when the Web workspace API cannot be used. In Nirvana mode, Glider REST refreshes preserve a valid OAuth bearer instead of replacing it with an older saved browser cookie; explicit browser-session validation and package upload still use the cookie plus CSRF token. The app id list is collected first from active workspace folders/working set (`now-file:/<app_sys_id>`), then explicit `--application-id-list` / `BA_APPLICATION_ID_LIST` / `BA_APP_ID_LIST` / `BA_CONVERSATION_APP_IDS`, and only then the current selected app when there is no workspace scope. The CLI no longer falls back to discovering every IDE-created app from `/api/sn_glider/applications/all`, because that made `/conversation` show other workspaces. When an active workspace is known but no app ids are available, the picker reads the unscoped IDE conversation list but keeps only app-less/global rows, matching default Web UI workspaces without leaking app-bound rows from other workspaces. API and backing-table results are filtered to the same workspace applications for app-bound rows. Because ServiceNow releases may store `application_id` as either the app sys_id or its textual scope (for example `x_snc_example`), the CLI resolves both aliases for every active workspace folder before building the API query and applying the local safety filter; scope-valued rows from unrelated workspaces are therefore excluded. App-less/global rows are retained because the Web UI includes them alongside workspace app conversations. Conversation creation follows the browser HAR shape: `{"title":"<first prompt>","applicationId":null,"applicationName":"","client":"ide"}`. The CLI adopts the returned `sysId`, loads selected conversation history first through `GET /api/sn_build_agent/build_agent_api/conversations/<conversationId>/messages` (including the installed `{result:[{content,conversation,active,sequence,sys_created_on,sys_id}]}` shape), posts message rows as `{"content":"<Glider JSON>"}`, and still merges backing `sn_build_agent_conversation` table rows into the picker when they match the active app filter. Older `conversations_api` and direct-table message reads remain compatibility fallbacks for a missing or unsupported message route; authorization and server failures are surfaced rather than masked. If an instance has an empty `sn_ba_core_conversation` table and populated `sn_build_agent_conversation` table, the list fallback continues to the populated table instead of stopping on the empty one.

The instance request example only documents `message`, but live testing showed this backend ignores the prompt unless an OpenAI-style `messages` array is also present. For streaming parity, the CLI subscribes before sending: first to `/build_agent_core/stream/<conversationId>`, then to the older `/build_agent/stream/<conversationId>` if the core channel is not registered. If the legacy `/send` endpoint still does not emit AMB chunks, the CLI reports the legacy fallback once and renders the single blocking HTTP response when it arrives.

Important distinction from the tested demo instance: the streamed Chrome web client is the Glider Build Agent extension, not the blocking legacy `/send` REST fallback. A HAR captured while sending `This is a question you have to answer with 42` showed:

```text
GET  /sn_glider_app/ide.do?...workspace=...
GET  /scripts/raw/sn_glider_app/extensions/build-agent/dist/extension.js
GET  /scripts/raw/sn_glider_app/extensions/build-agent/dist/chatView/chatView.js
GET  /api/sn_build_agent/build_agent_api/providerConfig
GET  /api/sn_glider/applications/all
GET  /api/sn_build_agent/build_agent_api/conversations?application_id_list=...&client=ide
GET  /api/sn_build_agent/build_agent_api/conversations/<conversationId>/messages
POST /oauth_token.do                 # OAuth PKCE token exchange for websocket auth
GET  /sncapps/code/assist/ba/nirvana/web-socket
```

The streamed answer arrived as websocket events on `/sncapps/code/assist/ba/nirvana/web-socket`: `turn_start`, `client_elicitation`, `stream_start`, many `stream_delta` frames with `content_type` `pending`, `thinking`, then `text`, followed by `stream_end` and `turn_end`. The same HAR contained no `/api/sn_build_agent/build_agent_api/send` request and no `/build_agent/stream/**` or `/build_agent_core/stream/**` AMB stream subscription for the turn.

So, on this instance, web streaming parity means using the Nirvana websocket protocol used by the Glider Build Agent extension. The legacy `/send` path remains useful as an installed-API compatibility fallback, but it is a blocking REST transport on this instance and should not be expected to stream. If `--web-gateway` falls back to legacy `/send`, the CLI reports that the HTTP endpoint is non-streaming.

Some older instances have neither conversations REST API but do expose the backing records. In that case the CLI falls back to direct table APIs for conversation listing, conversation creation, message loading, and user/assistant message persistence:

```text
GET/POST/PATCH https://<instance>/api/now/table/sn_build_agent_conversation
GET/POST       https://<instance>/api/now/table/sn_build_agent_message
```

This is intentionally a compatibility fallback for instances where the web UI tables exist but `/api/sn_ba_core/conversations_api` and `/api/sn_build_agent/conversations_api` are not registered.

The experimental Code Assist websocket path mirrors the browser payload shape from `code-assist-client` by sending `content`, `conversationId`, `model`, `providerUrl`, `skillId`, `chatHistory`, `snapshot`, `instanceOrigin`, and `usertoken`/`authorization` to:

```text
wss://<instance>/sncapps/code/assist/ba/web-socket
```

On the current tested instance this websocket opens but closes after a sent payload, so the default remains REST+AMB.

Nirvana mode derived websocket URL:

```text
wss://<instance>/sncapps/code/assist/ba/nirvana/web-socket
```

Nirvana mode sends the same high-level protocol as the Glider Build Agent web client:

```text
1. `connect` with web-client-compatible capabilities, `invokeOptions`, OAuth token params, and Glider-style `mcpServers`
2. `message` with `conversation_id`, content, `conversationHistory`, empty `ideContext`, images/attachments, discovered/static `mcpServers`, `workingSet`, `invokeOptions`, and token params
3. discover connected WDF MCP servers from `/api/sn_wdf_mcp_client/mcp/servers?limit=50&offset=0&connected=true` using the saved web session and append the Glider static ATF Cloud runner when building the `mcpServers` array
4. handle `client_elicitation` for approval/app picker/scope setting, bounded read-only instance-skill list/body/resource reads through `/api/sn_build_agent/skills_api/`, safe stubs for unsupported local tools, and Web UI-style `create_new_servicenow_app`
5. for `create_new_servicenow_app`, run the same browser-side REST orchestration observed in the Web UI: check `sys_app`, call `/api/now/templates` with scope-collision retries, update Glider workspace files through `/api/sn_glider/v2/sync/*`, patch the Build Agent conversation application id/title/working set, and return the Web UI-compatible `content`/`ideContext` shape over the websocket
6. persist the user prompt before sending and the final assistant text on `turn_end` using the same Glider `sender` message shape that `/sn_glider_app/ide.do` reloads
7. stream/render `stream_delta` events: pending/thinking/client elicitation noise stays off the normal UI, text deltas stream through the interactive Markdown repaint renderer in real terminals, non-TTY output remains plain append-only, and `turn_end` saves the server snapshot/local history
```

## Current limitations

- No arbitrary local filesystem, SDK build, install, or memfs tool implementation.
- `create_new_servicenow_app` is implemented through ServiceNow/Glider REST APIs, but other unknown client-side elicitations are rejected unless handled generically.
- Web gateway mode uses a direct Bayeux/AMB long-poll implementation.
- On some instances, Basic Auth works for generic table REST but the Build Agent API still rejects it with “User is not authenticated”. Use `--auth cookie` with cookies copied from the authenticated web UI in that case. Cookie/form sessions are stored once, reused, and deleted with `--logout` when they go stale.
- `--auth form` only works with classic ServiceNow username/password login; if the instance enforces SSO/MFA/CAPTCHA, use `--auth cookie` to import a browser session manually.


Interactive Nirvana tool events: successful `tool_result` events render as a green `✓ <tool>` line in the managed transcript; failed results render as a red `✗ <tool>` line. Tool call IDs are tracked so result events can display the original tool name.


Committed transcript entries now use an opencode-inspired append model: submitted prompts, tool results, and final assistant rows are appended as new stable rows into the terminal scrollback instead of repainting the full transcript. Streaming assistant output commits newly stable rendered rows progressively and leaves the unstable tail until the next row/final completion. Resize remains the only path that rebuilds alternate-screen scrollback from the semantic transcript.


Markdown table blocks are held until stable while streaming: once a potential table is detected, the CLI commits only the blank-line-delimited stable prefix before the table; the completed table is appended after the block/final response is available so later rows cannot invalidate already-committed column widths.


The animated `• Building (elapsed • esc to interrupt)` indicator remains active while assistant output streams and is cleared only when the turn-end/finalization path runs.

### Multi-instance profiles

The CLI can manage multiple configured ServiceNow instances. Each configured instance is stored as its own profile under `~/.ba-cli/profiles/<profile>/`, so OAuth tokens, web sessions/cookies, workspace state, active app, and conversation state stay isolated per instance.

- `--instance-list` / `--instances` lists configured instances and saved credential types.
- `--instance-delete <profile|fqdn|url>` removes a configured instance and its saved credentials/state.
- `--setup` adds or updates an instance. Without an explicit `--profile`, the profile name is derived from the instance FQDN; with `--profile`, that profile name is used.
- On interactive startup, if more than one instance is configured and no explicit `--profile`/`--instance` is passed, the CLI opens an instance picker with the last-used instance preselected.
- In a connected REPL, `/instance` opens that picker; `/instance current`, `/instance list`, and `/instance use <profile|fqdn|url>` support scripts and direct switching. The target is constructed as a new client and authenticated/connected before the old connection is closed. Failed target authentication, connection, or active-profile persistence closes only the candidate and restores the original client/UI unchanged; profile-local conversations, apps, workspaces, runtime state, and lifecycle goroutines are never reset or shared across instances.
- If saved credentials are expired/rejected, the CLI asks whether to reauthenticate, remove the instance, or cancel. Non-interactive runs default to reauthentication behavior.

Instance and conversation picker menus use ANSI styling in interactive terminals: bold cyan headers, green selected arrows, yellow current markers, cyan instance URLs, green credential/state badges, and red cancel/error/no-credential badges. Plain/non-TTY output remains uncolored.

## Typed slash-command registry

Interactive slash commands are declared once in a typed registry. The registry drives aliases, presentation order, argument/menu behavior, modal output capture, runtime filtering, `/help`, dispatch, and slash-menu suggestions. `/status` and `/status --json` are available even while a turn is active. They are offline-safe/read-only: they do not make remote calls, authenticate, refresh tokens, create sessions, or save state. Human output is compact terminal health/context information; `--json` emits stable schema version `1`, deterministic server/timeout ordering, UTC RFC3339 timestamps, normalized instance host, safe auth capability labels, connection/turn/context/MCP/working-set/model/policy/journal/known-build fields, and explicit `healthy`, `degraded`, `unknown`, or `error` status rather than implying a remote check. Active turns use their immutable runtime snapshot—including authoritative empty/nil app, working-set, and MCP values—otherwise `/status` computes a safe in-memory preview. It never emits OAuth tokens, passwords, cookies, `g_ck`, auth headers, raw frames, prompts, or assistant content. Suggestions and `/help` reflect the active transport and whether a turn is processing: `/help` and `/exit`/`/quit` remain available during processing, while commands that mutate or switch conversation/workspace/app state are hidden and rejected with a clear processing error. The terminal prompt receives the active client so its menu uses the same availability rules as dispatch.

### Read-only turn and local diagnostics

`/turn` shows the active semantic turn from its immutable accepted snapshot, or the most recent reducer-owned terminal turn; `/turn --json` emits stable `schemaVersion: 1` JSON. Availability is explicitly `no_turn`, `active`, or `last`. It reports lifecycle/server-turn identity, accepted snapshot identity and generation, journal-derived start/terminal timestamps and frozen terminal duration where a complete retained journal proves them, tools, elicitation, observed usage, terminal/error labels, staged next-turn context, retry/fallback/telemetry indicators, and local journal health. Missing, corrupt, or pruned timing stays explicit/unknown rather than being fabricated or triggering network work.

`/debug` is local-only and off by default: `/debug status`, `/debug on`, `/debug off`, `/debug tail [N] [--json]`, and `/debug event <event-id> [--json]`. Inspection reads the retained local semantic journal without HTTP/WebSocket calls or writes; `tail` defaults to 20 records and caps at 100. Outputs expose only normalized event identities, timestamps, lifecycle/tool states, hashes, usage, and safe diagnostic labels. Assistant content is omitted and credentials, headers, raw frames, prompts, attachments, cookies, `g_ck`, and unsafe payloads are never displayed.

### Offline search and export

`/search <query> [--json] [--limit N]` is read-only and local-only. Its stable `schemaVersion: 1` output searches retained redacted semantic-event fields plus safe runtime/workspace/app/turn/tool and conversation-message metadata (IDs, roles, timestamps, byte counts, hashes). It does not index prompt or assistant bodies. Results have explicit source/type/id/time/matched-field/safe-preview/score fields, deterministic score/time/source/type/id ordering, default limit 20, hard limit 50, and separate overall/per-source availability so runtime/message metadata remains searchable if the journal is missing, corrupt, or pruned. Credential-like or overlong queries are rejected without echoing the value.

`/export [path] [--json]` creates a deterministic stored ZIP (`schemaVersion: 1`) with allowlisted `config.json`, `context.json`, `events.json`, `journal.json`, `messages.json`, `status.json`, `turn.json`, and `manifest.json`. Message records contain scope/role/ID/time/content-byte-count and `[omitted]` content only; content hashes are domain-separated and omitted for short or credential-like text. Events are bounded redacted summaries scoped to exactly the selected active/last conversation turn (or explicitly empty when identity is unknown); active exports use the immutable active snapshot and include its post-checkpoint journal suffix. Archives use sorted members, UTC fixed timestamps, 0600 member/artifact modes, SHA-256 manifest records, size/event/message caps, atomic no-clobber publication, and a default private profile-local `exports/` directory (0700). Custom destinations must be conservative relative `.zip` paths with no traversal or symlink ancestry. No tokens, sessions, browser data, raw journals/frames/logs, content bodies, attachments, source files, HOME paths, or arbitrary files are included.
