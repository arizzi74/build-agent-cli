# Build Agent Go CLI — Implemented Specification

Last updated: 2026-07-07

This document describes what has been implemented so far in `/home/ubuntu/.openclaw/workspace/build-agent-go-cli`. It is a functional spec of the current Go CLI behavior, not a future roadmap.

## 1. Purpose

The Build Agent Go CLI is a native Go terminal client for ServiceNow Build Agent. Its current default transport mirrors the Glider Build Agent web client streaming path so the CLI can behave close to the web UI while staying scriptable from pipes/CI.

Primary goals implemented:

- Use the Glider Build Agent Nirvana websocket path as the default.
- Support real streaming responses and WDF/MCP tool availability.
- Persist and resume Build Agent conversations compatible with the ServiceNow web UI.
- Provide a polished interactive terminal UI with a persistent footer/status bar.
- Preserve clean non-interactive output for automation.
- Keep secrets local and redact debug output.

## 2. Binary and build

Implemented build outputs:

- Source directory: `build-agent-go-cli/`
- Static Linux arm64 binary: `build-agent-go-cli-linux-arm64`
- Checksum file: `build-agent-go-cli-linux-arm64.sha256`

Standard verification commands used after changes:

```bash
gofmt -w *.go
go test -count=1 ./...
go build -o /tmp/build-agent-go-cli-check .
```

Static binary verification has also been used:

```bash
file build-agent-go-cli-linux-arm64
ldd build-agent-go-cli-linux-arm64
sha256sum build-agent-go-cli-linux-arm64
```

Latest known rebuilt binary after fixing slash picker Enter execution:

```text
build-agent-go-cli-linux-arm64
sha256 533e8ce4ed7ada4bdf32b016cf88a33dbe7eea94ed42dacb77fe10c29f4bfdb2
```

## 3. Command-line flags

Implemented top-level flags include:

```text
--instance <url>                 ServiceNow instance URL
--ws-url <url>                   override websocket URL
--profile <name>                 profile under ~/.ba-cli/profiles/<name>/
--profile-list                   list profiles and exit
--profile-delete <name>          delete a profile; refuses active profile
--setup                          force profile setup
--prompt <text>                  scripted prompt; repeatable for multi-turn scripts
--conversation <selector>        latest | new | id | id-prefix | number
--provider <name>                model provider override
--model <name>                   large model override
--no-open                        do not open browser during OAuth
--auto-approve                   safe-default approval mode
--debug                          raw redacted websocket/AMB JSON frames
--nirvana                        use Glider/Nirvana websocket transport; default
--web-gateway                    use legacy web gateway/AMB fallback transport
--code-assist-ws                 experimental diagnostic websocket path
--auth form|cookie|basic         legacy web-gateway auth mode
--user <username>                username for web-gateway auth
--logout                         delete saved web session
--session-status                 show saved web session metadata
--advertise-local-tools          experimental only
--turn-timeout <duration>        per-turn timeout; default 10m
```

Transport selection rules:

- `--nirvana` is the default.
- `--web-gateway` disables Nirvana and uses the older REST/AMB path.
- `--code-assist-ws` disables Nirvana and uses the experimental Code Assist websocket diagnostic path.

## 4. Local profile and state layout

Implemented state root:

```text
~/.ba-cli/profiles/<profile>/
```

Implemented files:

```text
config.json                         saved instance/profile config
oauth-token.json                    Nirvana OAuth token cache, mode 0600
session.json                        validated web session for legacy gateway, mode 0600
active-workspace                    active local workspace name
workspaces/<workspace>.json         workspace state
active-app.json                     selected app scope cache
```

Workspace JSON persists:

- `conversationId`
- server conversation metadata/title/state
- local `conversationHistory`
- `workingSet`
- selected app scope metadata
- cumulative usage counters used by the terminal status bar

Usage counters reset when switching, creating, or resetting conversations.

## 5. Authentication

### 5.1 Default Nirvana/OAuth auth

Default mode implements the Glider/TypeScript CLI-style OAuth PKCE flow.

Implemented OAuth defaults:

```text
auth endpoint:  <instance>/oauth_auth.do
token endpoint: <instance>/oauth_token.do
client id:      b77993a2359e472cad99679d1f124919
redirect URI:   /api/sn_build_agent/build_agent_api/oauth_redirect
```

The OAuth access token is sent in the Nirvana websocket handshake payload under the Build Agent parameters. Tokens are cached locally in `oauth-token.json`; the CLI does not use macOS Keychain or browser automation for the token cache.

### 5.2 Legacy web gateway auth

Implemented auth modes for `--web-gateway`:

- `form` — default for legacy gateway; performs ServiceNow `GET/POST /login.do`, captures cookies and `g_ck`, validates session, persists only valid sessions.
- `cookie` — imports a manually pasted browser `Cookie:` header and optional user token/`g_ck`, validates before saving.
- `basic` — explicit Basic Auth mode.

Implemented session behavior:

- Saved `session.json` is reused only if it matches the active profile/instance and validates.
- Stale sessions are deleted on rejection.
- Invalid newly imported cookies are rejected and not persisted.
- Passwords are never stored.

## 6. Default Nirvana websocket transport

Default transport connects to the Glider Build Agent Nirvana websocket path:

```text
/sncapps/code/assist/ba/nirvana/web-socket
```

Implemented connect/message parity:

- Uses saved OAuth bearer token.
- Uses saved web-session cookies/user token where useful for REST sidecar calls.
- Builds a Glider-like `connect` payload with:
  - `protocol_version: 1`
  - web-client-compatible capabilities
  - `invokeOptions`
  - `params`
  - `mcpServers`
- Preserves provider config fields including `glideAttributes`.
- Uses ServiceNow user sys_id from provider config when available.
- Keeps `instanceUrl` in HAR-compatible trailing-slash form.
- Uses compact 32-hex Glider conversation ids, not hyphenated UUIDs, in Nirvana mode.

Implemented message payload fields include:

- `type: "message"`
- `conversation_id`
- sanitized `conversationHistory`
- `ideContext`
- `images: []`
- `attachments: []`
- `workingSet`
- `mcpServers`
- `isGreeting: false`
- `isMCPRetry: false`
- `invokeOptions`
- `params`
- optional `appScope`

Conversation history sent to Nirvana is sanitized:

- keeps only user/assistant-compatible history
- drops tool/thinking/loading rows
- converts unsupported system/stop style rows into user-visible notes where needed
- avoids backend errors caused by invalid `tool` roles

## 7. Streaming and turn handling

Implemented Nirvana stream handling:

- Processes real websocket stream events.
- Hides `pending` and routine thinking/tool plumbing in normal UI.
- Streams only real assistant text to stdout in normal UI.
- In interactive terminals, streams text through the Markdown formatter by repainting the current assistant block in place; this keeps tables, bullets, headings, inline code, and code fences formatted instead of leaving raw Markdown chunks in scrollback.
- Non-TTY/debug output remains plain append-only.
- Supports forcing the older append-only terminal path with:

```bash
BA_CLI_LIVE_REPAINT=0
```

Debug mode behavior:

- `--debug` prints redacted raw websocket frames to stderr.
- In debug mode, the CLI avoids in-place repaint and streams plain chunks so stderr JSON and stdout text do not garble each other.

Turn lifecycle:

- `SendMessage` starts a turn and writes the message payload.
- `WaitTurn` waits for `turn_end`, AMB completion, error, timeout, or connection close.
- `--turn-timeout` controls max wait per turn.

Normal-mode noise removed:

- no routine `--- turn started ---`
- no routine `--- turn ended ---`
- no routine `[client request]`
- no routine `[tool call]` / `[tool result]`
- debug mode still exposes diagnostic detail.

## 8. MCP/WDF parity

Implemented Nirvana MCP parity layer:

- Discovers WDF MCP servers from:

```text
/api/sn_wdf_mcp_client/mcp/servers?limit=50&offset=0&connected=true
```

- Implements pagination using `meta.total` semantics.
- Filters unsupported/missing transports.
- Filters Git/GitHub WDF servers like the decoded Glider client.
- Normalizes WDF servers such as `MCP Script Runner`.
- Adds the static Glider ATF Cloud runner:

```text
serverId:  atf-cloud-runner
transport: streamable-http
url:       https://atf-rel-boq/mcp
```

Implemented `/mcp list` command:

- Shows read-only MCP servers advertised to Forge.
- Explains that WDF tool schemas are not client-side because Forge loads pass-through tools after the Nirvana handshake.

Live validation previously confirmed:

- MCP Script Runner is available through the WDF/Forge/Nirvana path.
- A read-only syslog prompt executed MCP Script Runner successfully.
- Streaming and MCP execution can happen in the same turn.

## 9. Conversation support

Implemented primary interactive conversation selector:

```text
/conversation
```

`/conversation` opens the conversation picker and includes both existing conversations and `New conversation`. Legacy/direct selectors (`/conversation current|list|new|use`) and the `--conversation latest|new|<id>|<id-prefix>|<number>` flag remain accepted for scripts/tests but are no longer advertised in the interactive slash picker or help.

Conversation behavior:

- Works in both default Nirvana and legacy web-gateway mode.
- Interactive default/Nirvana startup can prompt to continue an existing conversation or start a new local conversation before the first `ba>` prompt.
- New conversations are deferred until the first prompt is sent; the server row is titled from that first prompt, matching the web UI and avoiding empty `New conversation` rows.
- Scripted `--prompt` skips picker unless `--conversation` is supplied.
- Existing conversations load server messages into local history before the next send.
- Selecting a different conversation replaces the managed terminal transcript with the loaded conversation history and immediately replays the viewport/footer, so the visible screen updates without waiting for a resize/redraw.
- `/conversation` opens a cursor-driven picker in a real TTY.
- The real-TTY conversation picker uses the terminal alternate screen, so closing/canceling it restores the previous terminal contents instead of erasing the text that was behind the menu.
- TTY startup connection/help guidance is rendered as a compact instance-free temporary footer hint for 5 seconds instead of being appended to transcript history.
- Non-interactive legacy `/conversation list` remains one-line-per-conversation and script-safe.

Implemented server API fallback chain includes:

```text
/api/sn_ba_core/conversations_api/...
/api/sn_build_agent/conversations_api/...
/api/sn_build_agent/build_agent_api/conversations...
Direct table APIs for sn_build_agent_conversation / sn_build_agent_message
Compatible sn_ba_core table attempts where applicable
```

Fallback handling includes:

- missing API routes
- empty intermediate APIs
- `/api/sn_build_agent/build_agent_api/conversations` browser-HAR parity: list first through `?application_id_list=<appSysIds>&client=ide`, create payload `title/applicationId/applicationName/client`, adopt returned `sysId`, and message payload `content` only
- when no local app/workspace ids are known, discover Glider IDE-created app sys_ids from `/api/sn_glider/applications/all` before listing conversations, matching the zaiagents HAR flow where the web UI sends those app ids as `application_id_list`
- backing-table merge for any non-empty conversation API result, filtered to the active application id list when present, so out-of-workspace rows do not leak while missing in-scope `sn_build_agent_conversation` records can still appear; empty `sn_ba_core_conversation` responses do not stop fallback to `sn_build_agent_conversation`
- some known old API 500s such as `fCSRFEvaluator/applyRotatedTokens`
- selected 401/403 fallback cases where another table/API path may still work

## 10. Web UI-compatible message persistence

Implemented Glider-compatible message persistence for CLI-created turns.

For user rows, content JSON is written with top-level fields like:

```json
{"id":"...","sender":"user","text":"...","hasCheckpoints":false}
```

For assistant rows, content JSON is written with top-level fields like:

```json
{"id":"...","sender":"assistant","text":"...","complete":true,"duration":0}
```

Implemented persistence behavior:

- Conversation creation uses the first user prompt as the title when the row does not already exist.
- Nirvana mode persists the user message before websocket send.
- Nirvana mode persists assistant text on `turn_end`.
- Legacy fallback persists assistant response text too.
- Local history is appended when no server snapshot is returned.
- Reload parser handles modern Glider-shaped rows.
- Reload parser recovers old bad bare-text rows by inferring roles where possible.
- Thinking/tool/loading rows remain hidden from normal transcript rendering.

This fixed the prior web UI issue where CLI turns appeared as `Unknown sender` or disappeared after CLI restart.

## 11. Legacy web gateway / AMB transport

Implemented legacy mode via:

```bash
./build-agent-go-cli-linux-arm64 --web-gateway --instance https://<instance>
```

Implemented gateway flow:

1. Ensure or create server conversation, titled from the first prompt when newly created.
2. Persist user message in compatible conversation/message store.
3. Subscribe to AMB before sending so fast chunks are not missed.
4. Try AMB channel:

```text
/build_agent_core/stream/<conversationId>
```

5. Fall back to:

```text
/build_agent/stream/<conversationId>
```

6. Call live gateway where available:

```text
POST /api/sn_ba_core/agent_gateway_api/conversation
```

7. Fall back to older API:

```text
POST /api/sn_build_agent/build_agent_api/send
```

Legacy `/send` payload includes both documented and live-required forms:

```json
{
  "payload": {
    "requestId": "<conversationId>",
    "message": "<prompt>",
    "messages": [{"role":"user","content":"<prompt>"}]
  }
}
```

Live testing showed some instances ignore `payload.message` unless OpenAI-style `payload.messages` is also sent.

Known implemented distinction:

- Legacy `/send` may return blocking HTTP JSON and emit no AMB frames.
- The CLI renders that response when received instead of faking stream output.
- True web streaming parity on the tested web UI is the Nirvana websocket path, not legacy `/send`.

## 12. Terminal Markdown rendering

Implemented dependency-light terminal renderer for assistant text:

- strips raw Markdown headings/bold/fences where appropriate
- renders headings and bold text with ANSI when interactive
- renders inline code
- boxes fenced code blocks
- aligns Markdown tables with Unicode borders and wraps long cell text within the table and width-aware wrapped cells
- normalizes bullet lists
- wraps/stabilizes terminal width
- adds a wasabi-green bullet prefix in interactive output
- keeps pipe/CI output plain and script-friendly

Assistant responses have roomier vertical spacing in TTY mode.

## 13. Interactive terminal UI/footer

Implemented fixed footer layout for real TTYs:

```text
[normal scrollback/output region]

[blank spacer row]
[animated Working ... row]
[blank spacer row]
[3-row full-width gray prompt band]
[colorful status bar]
[blank temporary-message row]
```

The footer reserves rows outside the terminal scroll region using terminal escape sequences. Output scrolls above the footer instead of overwriting it.

Implemented prompt behavior:

- Input prompt is a full-width gray 3-line band.
- Idle prompt redraw clears the row immediately above the prompt band, giving a blank separator between the last output/startup text and the gray input area.
- `ba>` prompt text sits on the middle row.
- Typed text appears in the middle row.
- Slash-command suggestions open above the prompt band.
- Pressing Enter immediately resets the footer to an empty `ba>` prompt while the request is processing, and after the submitted gray prompt block is written to scrollback the cursor is returned to the footer input row. During `Working ...`, stdin is kept in raw/no-echo capture mode and the buffered typeahead is redrawn through the footer prompt renderer, so typed characters preserve the gray prompt band and do not corrupt the transcript/output. Pressing Enter while the response is still running marks the buffered line as the next prompt. While this capture is active, assistant stream deltas are accumulated and the final answer is recorded and the whole managed viewport is replayed from transcript state, the same path used after resize, so output generation stays aligned and does not steal the input cursor.
- The submitted text is copied into scrollback as a gray prompt block before send setup and before `Working ...`.

Implemented `Working ...` behavior:

- Animated wasabi-green glow.
- Sits above the 3-line prompt band.
- Has one blank line above and below.
- Remains compatible with pinned footer/status layout.
- Clears before assistant text, errors, warnings, or real prompt redraws.

Implemented status bar:

- Pinned above the bottom temporary-message row.
- Background-free, colorful segmented text.
- Persists while typing, while `Working ...` animates, and while assistant output streams.
- Updates in place as usage information arrives.

Status bar fields:

```text
model=<model> input_messages=<count> input_tokens=<n> output_tokens=<n> instance=<short-instance>
```

Color mapping:

- model: wasabi green
- input message count: yellow
- input tokens: blue
- output tokens: magenta
- instance: green
- labels: dim

Terminal stability fixes implemented:

- Scroll-region setup saves/restores cursor because DECSTBM resets cursor to home in many terminals.
- Assistant answers no longer jump to row 1/top of terminal after prompt submit.
- `prepareTerminalScrollbackOutput()` positions transcript/assistant output at the bottom of the active scroll region above the footer.
- Overlay-style menus no longer erase underlying scrollback: the slash-command picker is drawn inside the managed footer/viewport and closing it replays the structured transcript behind it; the conversation picker remains a true alternate-screen modal and restores the original screen on exit.
- Fixed footer/status rendering handles terminal resize with an opencode-inspired replay model: the CLI keeps recent structured transcript entries, clears the managed viewport after resize, re-renders and wraps the visible transcript tail at the new width, then redraws the footer/prompt/status rows. This avoids both stale resized pixels and the previous black-screen-without-replay regression.

## 14. Submitted prompt transcript blocks

Implemented live submitted prompt blocks:

- Every submitted interactive prompt is recorded and replayed into the managed viewport before the assistant answer.
- Format is:

```text
[gray full-width row]
[gray full-width row: › prompt text]
[gray full-width row]
```

- There is 3-line gray styling for every user prompt.
- Loaded conversation history uses the same visual grammar.
- Rendering is TTY-only/script-safe:
  - skipped for non-TTY
  - skipped for scripted `--prompt`
  - kept clean for debug/log automation paths

Recent timing fix implemented:

- The gray prompt block is replayed immediately in the REPL after Enter and before `runPrompt`/`SendMessage`, avoiding a blank-gap where the footer cleared but the submitted prompt was not visible yet.

Recent footer reset fix implemented:

- After Enter, the bottom input/footer prompt redraws as an empty 3-row gray `ba>` placeholder while waiting for the response.
- The submitted text remains visible in the scrollback gray prompt block.

## 15. Interactive input features

Implemented REPL input features:

- In-memory command history for current process.
- Up/down arrows browse prompt and slash-command history.
- Press `/` on an empty prompt to open the slash-command picker.
- The slash-command picker advertises the regular helper commands but only one conversation command: bare `/conversation`.
- Parameterized conversation suggestions (`/conversation current`, `/conversation list`, `/conversation new`, `/conversation use ...`) are intentionally hidden.
- The slash-command picker is rendered in the managed footer/viewport area, and inserting/canceling it replays the visible transcript tail before redrawing the footer so rows behind the menu are restored.
- Pressing Esc closes the slash-command picker immediately, clears the typed slash-filter text, and restores the original terminal contents; arrow-key escape sequences still navigate the picker/history.
- Enter runs a selected complete slash command immediately, so `/workspace list` or `/conversation` execute from the picker; Tab inserts without running.
- Slash suggestions that require an argument and end with a trailing space, such as `/workspace use ` or `/app use `, are inserted on Enter instead of executed so the argument can be completed.
- Ctrl-C aborts current prompt input.
- Ctrl-D uses a two-step exit guard: the first press shows `Press Ctrl-D again to exit ....` in the temporary message row below the status bar; the message clears automatically after 2 seconds, and a second Ctrl-D within that window exits.
- Esc cancels popup menus such as slash-command and conversation pickers.
- Falls back to simple line input when stdin/stderr are not TTYs.

Implemented interactive slash picker commands:

```text
/help
/exit
/quit
/workspace current|list|new|use|reset|delete
/conversation
/mcp list
/app current|use|clear
```

`/conversation` opens the conversation picker and supports selecting an existing conversation or creating a new one. Older direct `/conversation current|list|new|use` handlers remain backward-compatible but are no longer advertised in the interactive picker/help.

## 16. Workspace and app scope commands

Implemented workspace commands:

```text
/workspace current
/workspace list
/workspace new <name>
/workspace use <name>
/workspace reset [name]
/workspace delete <name>
```

Implemented app scope commands:

```text
/app current
/app use <scopeId> [scopeName]
/app clear
```

App scope behavior:

- Selected app metadata is cached locally.
- Backend `set_app_scope` elicitations can update/persist app metadata.
- Subsequent Build Agent payloads include `appScope` when selected.

## 17. Debugging and redaction

Implemented debug redaction:

- authorization headers
- cookies
- user tokens / `X-UserToken`
- passwords
- secrets
- `g_ck` / `glide_ck`
- OAuth access/refresh tokens
- token-like auth fields

Implemented non-redaction for safe metrics:

- numeric `maxOutputTokens`
- numeric `thinkingTokens`
- numeric `request_tokens`
- normal content/author fields

`--session-status` reports saved session metadata without printing secret values.

## 18. Script and pipe behavior

Implemented non-interactive guarantees:

- No alternate-screen REPL and no fixed footer/prompt band when stdin/stderr are not real TTYs.
- Non-interactive legacy `/conversation list` prints plain list output rather than opening picker.
- Scripted `--prompt` skips startup conversation picker unless `--conversation` is explicitly provided.
- Status bar is not injected into pipe/CI output.
- Usage lines remain available in non-interactive/debug output.
- Terminal Markdown renderer avoids TTY-only ANSI decoration when not interactive.

## 19. Known intentionally limited areas

Implemented but intentionally limited:

- Local filesystem tools are guarded/safe stubs; the CLI does not expose broad local file access as Build Agent tools.
- `--advertise-local-tools` remains experimental and should stay off for normal backend/MCP use.
- Legacy `/send` streaming is not faked when the backend returns blocking JSON.
- WDF tool schemas are not exposed client-side by `/mcp list`; Forge loads pass-through tools after the Nirvana handshake, matching the web client behavior.
- Code Assist websocket mode is diagnostic/experimental, not the normal Build Agent path.

## 20. Current validation coverage

Regression and smoke coverage implemented across the project includes:

- flag defaults and transport overrides
- saved session validation/rejection
- OAuth/bearer REST sidecar headers
- provider config/glide attributes parsing
- Nirvana compact conversation ids
- Nirvana history sanitization
- stream event filtering/rendering
- MCP/WDF server parsing, filtering, and payload assembly
- conversation list/use/new flows
- immediate managed-viewport replay after conversation switch
- automatic IDE app-id discovery for Web UI conversation-list parity
- fallback API/table conversation loading
- Glider-compatible user/assistant persistence JSON
- old bad-row reload recovery
- terminal Markdown rendering
- status bar formatting and truncation
- fixed footer prompt/status placement
- submitted prompt transcript formatting
- prompt submit/reset timing
- non-interactive script-safe behavior

Recent manual/PTYS smokes confirmed:

- footer remains pinned while typing and streaming
- `Working ...` appears above the 3-row prompt band
- status row stays at bottom and updates usage
- assistant output stays in scrollback, not row 1
- submitted prompt appears immediately as a gray prompt block
- footer prompt resets to empty while waiting


Interactive Nirvana tool events: successful `tool_result` events render as a green `✓ <tool>` line in the managed transcript; failed results render as a red `✗ <tool>` line. Tool call IDs are tracked so result events can display the original tool name.


Committed transcript entries now use an opencode-inspired append model: submitted prompts, tool results, and final assistant rows are appended as new stable rows into the terminal scrollback instead of repainting the full transcript. Streaming assistant output commits newly stable rendered rows progressively and leaves the unstable tail until the next row/final completion. Resize remains the only path that rebuilds alternate-screen scrollback from the semantic transcript.


Markdown table blocks are held until stable while streaming: once a potential table is detected, the CLI commits only the blank-line-delimited stable prefix before the table; the completed table is appended after the block/final response is available so later rows cannot invalidate already-committed column widths.


The animated `Working ...` indicator remains active while assistant output streams and is cleared only when the turn-end/finalization path runs.

## Multi-instance profiles

- Configured instances are represented by profile directories under `~/.ba-cli/profiles/<profile>/`; credentials and local workspace/conversation state remain per-instance/profile.
- Added `--instance-list` / `--instances` and `--instance-delete <profile|fqdn|url>` management commands.
- `--setup` can create/update an instance profile; when `--profile` is omitted the profile is derived from the instance FQDN.
- Interactive startup prompts with an instance picker when multiple instances are configured and no explicit profile/instance was passed; the last-used instance is preselected via `~/.ba-cli/active-instance`.
- Saved web-session and OAuth refresh failures prompt for reauthentication, instance removal, or cancel in interactive terminals.

## Picker menu styling

- Instance and conversation selection menus render with ANSI styling in interactive terminals: bold cyan headers, green selected arrows, yellow current markers, cyan URLs, green positive badges (`[oauth]`, `[web-session]`, `[open]`), and red cancel/error/no-credential badges.
- The picker line helpers remain plain-text-compatible for tests/non-TTY output.
