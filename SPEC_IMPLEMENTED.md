# Build Agent Go CLI — Implemented Specification

Last updated: 2026-07-13

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
- Normalize the initial Nirvana turn lifecycle through a versioned, redaction-safe semantic event/reducer seam without changing existing user-visible rendering or persistence.

## Durable goals and approval-gated local actions

- Goals use schema version 1 with stable IDs, timestamps, explicit lifecycle states, scoped references, bounded local retention, and manual source markers. Titles are safe single-line labels; raw prompt/assistant content and credentials are rejected. Persisted stores are treated as untrusted: every field is validated and deep-copied before any diagnostic, search, export, or support output.
- Goal and approval stores are profile-local (`goals.json`, `approvals.json`), atomically written with 0700 directories/0600 files and parent-directory fsync. Symlinked and non-regular storage paths are rejected. Invalid local JSON or unsafe schema-valid records are quarantined with a unique timestamp suffix by a mutating command rather than silently deleted; diagnostics remain read-only and report an in-memory degraded view. Cross-process locks protect all store mutations.
- `/goal`, `/goal --json`, `/goal add`, `/goal done`, and `/goal cancel` are offline/local-only. One active goal is maintained per workspace/conversation; terminal transitions are idempotent.
- An active goal reference is captured in the immutable accepted turn snapshot and appears in safe turn/status/search/export diagnostics. Mutating a goal during an active turn does not mutate that snapshot.
- Approval requests use schema version 1 with safe action summaries, scope references, risk, hash/preview, expiry, and exactly-once execution states. `/approvals`, `/approve`, and `/reject` are local-only. Cross-process locking serializes execution; a persisted interrupted `executing` state becomes failed-uncertain on a later mutating command and is never replayed.
- `/workspace delete <name>` is the integrated high-risk gated operation. It now creates a local request and leaves the workspace untouched until `/approve <id>`; `/reject` never executes it. This is the only deliberately changed destructive CLI behavior; remote operations retain prior behavior.

## 2. Binary and build

Implemented build output:

- Source directory: `build-agent-go-cli/`
- Release binary: `build-agent-go-cli`
- Target: Linux arm64/aarch64
- Linkage: static, stripped symbols/debug info
- Build instructions: `BUILD.md`

Required release build command:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w -buildid=' -o build-agent-go-cli .
```

Standard verification commands used after changes:

```bash
gofmt -w .
go test -count=1 ./...
go vet ./...
file build-agent-go-cli
ldd build-agent-go-cli
sha256sum build-agent-go-cli
```

`ldd` is expected to report `not a dynamic executable`. Per the active build policy, do not produce `build-agent-go-cli-linux-arm64` or dynamic/non-stripped release binaries unless a temporary debugging exception is explicitly requested.

Latest known rebuilt binary after `fluent_topics_list` local-doc fallback and `now-sdk pack` build artifact fix:

```text
build-agent-go-cli
sha256 431c00004d9f6b5d9a47a288998e8f08491d9dd991179a879f098f9d1c3c7a7d
```

## 2.1 Internal semantic event reducer

Implemented a deliberately small internal lifecycle envelope (`SemanticEventVersion = 1`) and pure `Apply`/`Reduce` state reducer. It covers connection state, accepted/started turns, assistant deltas/completion, tool start/completion, elicitation, usage, working-set/app/conversation updates, retry/fallback, cancellation/failure/completion, and telemetry failure. `Apply` clones reducer-owned maps/slices before mutation, deduplicates identical event-ID replays, rejects conflicting ID reuse, validates metadata keys case-insensitively and rejects bearer/basic/cookie header material in metadata values, binds a non-empty server turn ID at start, and rejects mismatched later IDs, invalid terminal ordering, negative/non-monotonic usage, post-completion assistant output, unknown tool completions, and conflicting tool duplicates. It preserves immutable accepted-turn context while staging later context updates for the next turn, and treats telemetry failure as non-terminal for the user operation. The initial integration is an observational **Nirvana-only** compatibility seam: per-turn `Sequence` resets, client-lifecycle event identity does not; normalized events leave current rendering, persistence, telemetry, and transport handlers intact. Web gateway and Code Assist paths do not initialize this seam.

## 2.2 Local semantic recovery journal

Nirvana semantic events are redaction-validated before a local versioned JSONL envelope is atomically append-opened and fsynced. The envelope adds workspace-monotonic `Sequence` and workspace/conversation `ScopeSequence` while preserving the embedded event's current per-turn semantics. Journal files are `0600`; journal/state/archive directories are `0700`. Workspace JSON remains the compatible compact snapshot, now with an optional local-only `semanticJournalSequence` checkpoint. Startup replays events beyond that checkpoint when the snapshot is missing, corrupt, or behind the journal. A malformed trailing JSONL record is ignored as an interrupted append; malformed non-trailing data is surfaced. Replayed identical event IDs remain reducer-idempotent and conflicting duplicates fail recovery. An accepted/started turn lacking a terminal event is deterministically abandoned locally: partial assistant text is not promoted to conversation history and the CLI never creates synthetic remote ServiceNow events. Before append, the journal rejects OAuth tokens, passwords, cookies, `g_ck`, Authorization/header/raw-frame material, unsafe metadata keys, and credential-bearing payload values. At more than 1,000 live records, the oldest records are privately archived, the latest 500 remain live, and only five archives are retained. Recovery reads the ordered retained archive chain plus the live file, accepts only exact duplicate sequence overlap during an interrupted rotation, and rejects conflicting overlap or gaps. If older archives have been pruned, a missing snapshot or a snapshot checkpoint older than the first retained sequence is rejected explicitly. Reducer replay applies only records after the snapshot checkpoint; its first record for each conversation must be a `connection_state_changed` or `turn_accepted` boundary, so recovery never guesses missing lifecycle state. Before O_APPEND, a malformed interrupted final JSONL record is truncated after verified complete records to prevent corruption being joined to the next envelope. No remote state is changed.

## 2.3 Immutable per-turn runtime snapshot

Nirvana captures a deep-copied `TurnRuntimeSnapshot` at accepted message submission, after conversation/app/MCP preparation and before constructing the outbound turn payload. The snapshot contains only safe effective values: normalized profile and instance/host, selected transport, auth mode/capability labels, server conversation ID, workspace and Web UI workspace fields, selected app, canonical working set plus SHA-256 hash, IDE/history context, effective provider/model/skill configuration, sorted MCP/WDF inventory plus deterministic generation/hash, effective startup timeouts, retry-policy label, and CLI version/build reference. No OAuth token, cookie, password, `g_ck`, authorization header, raw request/frame, or credential value is stored. The active Nirvana payload reads its conversation, app scope, working set, IDE context, history, and MCP inventory from this snapshot; semantic `turn_accepted` uses its snapshot identity/hash/generation. Later mutable updates are intentionally staged for the next turn. Connection setup and non-Nirvana transport payloads remain outside this incremental migration.

## 2.4 Runtime `/status`

The typed slash-command registry includes `/status` and `/status --json`, both available during active turns. The command is deliberately offline-safe and read-only: it performs no HTTP/WebSocket/auth/token-refresh work and does not write workspace, journal, profile, or remote state. Human output has compact sections for normalized profile/instance host, transport/auth capability labels, connection, current context, active snapshot identity/generation, MCP/WDF inventory, working set, effective model/skill, policy, journal, and last known build/metadata readiness. JSON emits stable `schemaVersion: 1` with UTC RFC3339 timestamps and deterministic MCP/timeout ordering. Health fields use `healthy`, `degraded`, `unknown`, or `error`; unavailable remote facts remain `unknown`. An active turn reports only its immutable snapshot (including authoritative absent/empty app, working-set, and MCP values); idle status builds a safe in-memory preview. All free-form snapshot and status fields, including MCP ID/name/transport/source/URL, are sanitized before deterministic ordering and generation/hash computation. Credentials, cookies, OAuth tokens, passwords, `g_ck`, authorization material, raw frames, prompts, and assistant content are excluded.

## 2.5 Deterministic local `/support-bundle`

The typed slash registry provides `/support-bundle [path] [--json]`, including help, completion, validation, and active-turn availability. It is local-only and creates no network traffic or remote state changes. The artifact format is ZIP with stable member ordering, normalized UTC entry timestamps/modes, atomic temp-and-rename creation, and a stable `SupportBundleSchemaVersion = 1` manifest containing SHA-256 and size for every non-manifest member. The exact allowlist is `manifest.json`, `status.json`, `turn.json`, `events.json`, `journal.json`, `inventory.json`, `config.json`, and `environment.json`; no directory recursion occurs. It captures schema-versioned `/status --json` and `/turn --json` equivalents, bounded allowlisted/redacted semantic event summaries, journal health/retention, sanitized MCP/WDF inventory and hashes, effective labels, last-known local remote summaries, selected safe config labels, and Go runtime environment labels. Archive content never includes secret/session/browser/raw-log/prompt/attachment/source/workspace/home/user material. The default destination is a unique timestamped ZIP inside private profile-local `support/`; explicit destinations are safe relative `.zip` paths only, reject traversal/symlinks/unsafe parents, and never overwrite. Archive file mode is `0600`; profile-local support parent is `0700`. Missing, corrupt, or pruned journals result in an explicit health document when possible rather than unsafe raw inclusion.

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
-debug <filename>                enable debug; tee regular output plus redacted debug trace to filename
--debug <filename>               same as -debug <filename>
--debug-file <filename>          explicit debug log file alias; also enables debug
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
workspaces/<workspace>.json         compact workspace startup snapshot
workspaces/<workspace>.semantic.jsonl redacted local semantic recovery journal, mode 0600
workspaces/semantic-archive/         bounded private local journal archives
active-app.json                     selected app scope cache
```

Workspace JSON persists:

- `conversationId`
- server conversation metadata/title/state
- local `conversationHistory`
- validated server-shaped `workingSet` entries only; folder-shaped workspace data is not persisted as working set
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
- Advertises the exact capability set observed in the current Web UI HAR captures: `client_ide`, `elicitation`, `fluent_docs`, `glob_and_grep`, `interview_choice_picker`, `keyword_search.preview_available`, `plan_approval`, `product_availability`, `semantic_search`, `server_tools`, `streaming.receive`, `sub_agents`, and `tools.execute`. Extension-only keys not observed in the current Web UI connect frame are not advertised in Nirvana mode.
- Sends application-level Nirvana `{ "type": "ping" }` keepalives every 25 seconds after the `connected` frame, matching the browser client's observed JSON ping/pong behavior.

Implemented message payload fields include:

- `type: "message"`
- `conversation_id`
- sanitized `conversationHistory`
- `ideContext`
- `images: []`
- `attachments: []`
- `workingSet` only when every item has `table`, `sysId`, and `scopeId`; folder-shaped workspace data is suppressed
- `mcpServers`
- `isGreeting: false`
- `isMCPRetry: false`
- `invokeOptions`
- `params`
- optional `appScope`

Outbound `workingSet` handling deliberately treats it as server record metadata only. Workspace folder records such as `{name, uri}` are not sent as `workingSet`, preventing Nirvana schema validation errors on `workingSet[*].table`, `workingSet[*].sysId`, and `workingSet[*].scopeId`.

Conversation history sent to Nirvana is sanitized:

- keeps only user/assistant-compatible history
- drops tool/thinking/loading rows
- converts unsupported system/stop style rows into user-visible notes where needed
- avoids backend errors caused by invalid `tool` roles

Implemented client-side app creation parity for the Web UI `create_new_servicenow_app` elicitation:

- Handles the prior `approval` elicitation using the existing yes/no or `--auto-approve` path.
- Accepts payload fields such as `appName` and `appDescription`.
- Checks `sys_app` through `/api/sn_build_agent/build_agent_api/runQuery/table/sys_app/query/scopeLIKE<scope>`.
- Creates the application with `POST /api/now/templates` using template sys_id `c305debeff236210c7c1ffffffffff03`.
- Polls `/api/now/templates/status?template_instance_id=<id>` until completion.
- Retries scope collisions using the Web UI-observed shape: base scope, numeric truncated suffixes, then random 4-character suffixes.
- Refreshes Glider state, writes the new app seed project and active `.code-workspace` update with multipart `POST /api/sn_glider/v2/sync/changes/apply`, patches the Build Agent conversation with the new `applicationId`, updates the conversation title, and clears the conversation working set like the Web UI flow.
- Returns a websocket response with `success`, JSON string `content.nowConfig`, and `ideContext` including `workspaceFolders`, `fluentVersion`, `scopeName`, and project structure, matching the Web UI response shape.

## 7. Streaming and turn handling

Implemented Nirvana stream handling:

- Processes real websocket stream events.
- Hides `pending` and routine thinking/tool plumbing in normal UI.
- Streams only real assistant text to stdout in normal UI.
- In interactive terminals, streams text through the Markdown formatter by repainting the current assistant block in place; this keeps tables, bullets, headings, inline code, and code fences formatted instead of leaving raw Markdown chunks in scrollback.
- Non-TTY output remains plain append-only.
- Supports forcing the older append-only terminal path with:

```bash
BA_CLI_LIVE_REPAINT=0
```

Debug mode behavior:

- Debug mode is only available with a filename: `-debug <filename>`, `--debug <filename>`, `--debug=<filename>`, or `--debug-file <filename>`.
- Bare `--debug` / `-debug` without a filename is invalid.
- Regular terminal output remains visible on the terminal and is also copied to the debug file.
- Redacted debug trace output is written only to the debug file, never to the terminal.
- Debug mode keeps the normal terminal UI behavior; the file log receives whatever regular output the terminal receives, plus redacted debug trace lines.

Turn lifecycle:

- `SendMessage` starts a turn and writes the message payload.
- `WaitTurn` waits for `turn_end`, AMB completion, error, timeout, cancellation, or connection close.
- During an active turn, the first Esc arms cancellation for two seconds and the second Esc sends the Web UI-compatible `{ "type": "stop", "conversation_id": "..." }` frame; Ctrl-C performs the same graceful cancellation immediately.
- Cancellation is correlated to the server `turn_id`, so late events from a cancelled turn stay suppressed even after a new turn starts, without suppressing the new turn's events.
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
- Interactive default/Nirvana startup restores the active profile's last workspace and last-opened conversation before the first `ba>` prompt, without showing a startup conversation picker. `--conversation latest|new|<id>|<id-prefix>|<number>` remains the explicit override path.
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
- app ids for conversation listing are scoped to the active workspace folders/working set first; explicit `--application-id-list` / environment overrides remain available, and current-app fallback is used only when there is no workspace scope
- the conversation picker no longer discovers every IDE-created app from `/api/sn_glider/applications/all`; if an active workspace is known but no app ids are available, the CLI reads the unscoped IDE list but keeps only app-less/global rows instead of showing app-bound rows from other workspaces
- API results and backing-table merge are filtered to the active application id list when present, excluding out-of-workspace app-bound rows while retaining app-less/global rows that the Web UI shows; missing in-scope `sn_build_agent_conversation` records can still appear; empty `sn_ba_core_conversation` responses do not stop fallback to `sn_build_agent_conversation`
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
[animated Building... row]
[blank spacer row]
[3-row full-width gray prompt band]
[colorful status bar]
[blank temporary-message row]
```

The footer reserves rows outside the terminal scroll region using terminal escape sequences. Output scrolls above the footer instead of overwriting it. After instance/transport selection, startup shows an animated wasabi-green `Connecting...` status below the transport screen while `client.Connect()` establishes the websocket/session.

Implemented prompt behavior:

- Input prompt is a full-width gray 3-line band.
- Idle prompt redraw clears the row immediately above the prompt band, giving a blank separator between the last output/startup text and the gray input area.
- `ba>` prompt text sits on the middle row.
- Typed text appears in the middle row.
- Slash-command suggestions open above the prompt band.
- Pressing Enter immediately resets the footer to an empty `ba>` prompt while the request is processing, and after the submitted gray prompt block is written to scrollback the cursor is returned to the footer input row. During `Building...`, stdin is kept in raw/no-echo capture mode and the buffered typeahead is redrawn through the footer prompt renderer, so typed characters preserve the gray prompt band and do not corrupt the transcript/output. Pressing Enter while the response is still running marks the buffered line as the next prompt. While this capture is active, assistant stream deltas are accumulated and the final answer is recorded and the whole managed viewport is replayed from transcript state, the same path used after resize, so output generation stays aligned and does not steal the input cursor.
- The submitted text is copied into scrollback as a gray prompt block before send setup and before `Building...`.

Implemented `Building...` behavior:

- Animated wasabi-green glow.
- Sits above the 3-line prompt band.
- Has one blank line above and below.
- Remains compatible with pinned footer/status layout.
- Remains visible while server/client elicitations and assistant output are in progress; blocking local prompts can temporarily own the terminal, after which the indicator is restored while the turn remains active. It clears on `turn_end`, `turn_error`, or finalization.

Implemented status bar:

- Pinned above the bottom temporary-message row.
- Background-free, colorful segmented text.
- Persists while typing, while `Building...` animates, and while assistant output streams.
- Updates in place as conversation/workspace state changes.

Status bar fields:

```text
model=<model> input_messages=<count> workspace=<workspace-name> app=<app-name> instance=<short-instance>
```

Color mapping:

- model: wasabi green
- input message count: yellow
- workspace: blue
- app: magenta
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
- The slash-command picker advertises the regular helper commands but only the bare picker commands for conversations/workspaces/apps: `/conversation`, `/workspace`, and `/app`.
- Parameterized conversation suggestions (`/conversation current`, `/conversation list`, `/conversation new`, `/conversation use ...`), workspace suggestions (`/workspace current`, `/workspace list`, `/workspace new`, `/workspace use ...`, `/workspace reset`, `/workspace delete`), and app suggestions (`/app current`, `/app use ...`, `/app clear`) are intentionally hidden.
- The slash-command picker is rendered in the managed footer/viewport area, and inserting/canceling it replays the visible transcript tail before redrawing the footer so rows behind the menu are restored.
- Pressing Esc closes the slash-command picker immediately, clears the typed slash-filter text, and restores the original terminal contents; arrow-key escape sequences still navigate the picker/history.
- Enter runs a selected complete slash command immediately, so `/workspace` or `/conversation` execute from the picker; Tab inserts without running.
- Non-modal slash command output (`/help`, `/mcp list`, etc.) is captured and appended as a managed transcript system block, keeping output visible above the fixed footer instead of printing into the prompt/footer row.
- Slash suggestions that require an argument and end with a trailing space are inserted on Enter instead of executed so the argument can be completed; the advertised picker commands execute directly.
- Ctrl-C aborts current prompt input.
- Ctrl-D uses a two-step exit guard: the first press shows `Press Ctrl-D again to exit ....` in the temporary message row below the status bar; the message clears automatically after 2 seconds, and a second Ctrl-D within that window exits.
- Esc cancels popup menus such as slash-command and conversation pickers.
- Falls back to simple line input when stdin/stderr are not TTYs.

Implemented interactive slash picker commands:

```text
/help
/exit
/quit
/workspace
/conversation
/mcp list
/app
```

`/conversation` opens the conversation picker and supports selecting an existing conversation or creating a new one. `/workspace` opens the workspace picker and, after a workspace is selected, opens the workspace-scoped conversation picker with that workspace's last-used conversation preselected. `/app` opens an app picker for apps in the active workspace. Older direct `/conversation current|list|new|use`, `/workspace current|list|new|use|reset|delete`, and `/app current|use|clear` handlers remain backward-compatible but are no longer advertised in the interactive picker/help.

## 16. Workspace and app scope commands

Implemented primary workspace command:

```text
/workspace
```

Backward-compatible workspace subcommands remain available for scripts/tests but are no longer advertised in the interactive slash picker:

```text
/workspace current
/workspace list
/workspace new <name>
/workspace use <name>
/workspace reset [name]
/workspace delete <name>
```

Workspace behavior:

- When an authenticated ServiceNow web session is available, bare `/workspace` follows the Glider Web UI path: discover `window.sn_glider.user.userId` from `/sn_glider_app/ide.do`, call `POST /api/sn_glider/v2/sync/state` for `settings:/users/<userId>`, and show `workspaces/*.code-workspace` entries in a picker.
- `/workspace` can switch to a Web UI workspace by picker selection; after picker-based switching, the CLI proposes the conversations in the newly active workspace and preselects that workspace's saved conversation when present. The backward-compatible `/workspace use <selector>` can still switch by number, exact name, URI, or unique prefix, and multi-word names are joined, so Web UI names like `Default - admin` work.
- When `sync/files` returns the selected `.code-workspace` file, the CLI persists the Web UI workspace URI/checksum/description/folders and derives a current app scope from `now-file:/<app_sys_id>` folders.
- Without web workspace auth or when the remote list is unavailable, the command falls back to local profile workspaces.

Implemented primary app scope command:

```text
/app
```

Backward-compatible app subcommands remain available for scripts/tests but are no longer advertised in the interactive slash picker:

```text
/app current
/app use <scopeId> [scopeName]
/app clear
```

App scope behavior:

- Bare `/app` opens a picker listing apps available in the active workspace. The list is sourced from active workspace `.code-workspace` folders whose URI is `now-file:/<app_sys_id>`; selecting an entry persists the app through the same `SetApp`/workspace state path as `/app use`.
- Selected app metadata is cached locally.
- Backend `set_app_scope` elicitations can update/persist app metadata.
- Selecting a conversation updates/persists the active app too, matching the Web UI behavior: when conversation application metadata exists, the CLI prefers matching active-workspace app names and falls back to the conversation application name/id; when the conversation has no app, the CLI clears the active app so the status line shows `app=<none>`. Creating a new conversation also clears the selected app and persisted active-app state, so the new conversation starts app-less and the footer shows `app=<none>`.
- Subsequent Build Agent payloads include `appScope` when selected.

## 17. Client-side Build Agent elicitations and Glider VFS tools

Implemented client-side elicitation handling includes:

- `approval` and `plan_approval` through yes/no prompts or `--auto-approve`.
- `interview` and `interview_choice_picker` local prompts.
- `app_picker`, `set_app_scope`, and `create_new_servicenow_app` Web UI-style app creation/update flows.
- `instance_skills_list`, currently returning an empty JSON list when no instance skill catalog is available.
- Glider workspace filesystem actions against the active app's `now-file:/<app_sys_id>` workspace:
  - `fs_read_directory`
  - `fs_read_file`
  - `fs_write_file`
  - `fs_create_directory`
  - `fs_tree`
  - `fs_stat`
  - `fs_glob`
  - `local_search`
- `build`, `install`, `install_dependencies`, and `build_install` Web UI-parity client actions for the active Glider app.

Implemented local Fluent build/install parity:

- Resolves app context from the active app/app id, reads `now.config.json` and `package.json` through Glider sync, and uses the active app sys_id as the `now-file:/<app_sys_id>` project root.
- Syncs the Glider app workspace to a temporary local project while skipping heavyweight/generated directories such as `node_modules`, `dist`, `target`, and `.git`.
- Validates dependencies from `dependencies`, `devDependencies`, and `optionalDependencies`, ignoring `eslint` like the Web extension.
- Installs missing dependencies locally with `npm install --no-audit --no-fund`; missing `node` or `npm` is reported visibly on the terminal and through structured error codes.
- Runs the app-local `node_modules/.bin/now-sdk build` command; missing `now-sdk` or build failures return terminal-visible structured errors.
- Persists generated Fluent source changes back to Glider, especially `src/fluent/generated/**`, using `/api/sn_glider/v2/sync/changes/apply` with non-null array parts.
- Runs app-local `node_modules/.bin/now-sdk pack` after build so the installable ZIP is actually emitted.
- Locates the package ZIP under configured `packOutputDir`, `target`, or `dist` and keeps the successful build temp project for a same-session `install` call.
- Enforces an install precondition: `install` requires a successful build for the same active app in the current CLI session.
- Checks `.now/.app-data.json` before build and, when metadata sync is needed, silently refreshes the selected profile OAuth token and runs the project-local ServiceNow SDK incremental transform with the exact `lastSync` timestamp.
- Atomically marks metadata sync complete only after transform success, persists transform-created/updated/removed source files plus `.app-data.json` to Glider, verifies optimistic checksums and post-write state, then rebuilds and packs from the synchronized project.
- Invalidates an older same-session package when a new build begins, so a failed sync/build cannot leave stale output installable.
- Performs install prechecks against `sys_upgrade_history` and `sys_scope`.
- Uploads the package ZIP to `sn_appclient_upload_processor.do` with `sysparm_track_fluent_install=true`, `sysparm_async_fluent_install=false`, scope id/name/version query params, multipart `upload_type=file`, `load_demo=true`, `sysparm_ck`, and `attachFile=blob`.
- Requires a browser-session CSRF token (`sysparm_ck` / `X-UserToken`) for upload; if unavailable, returns `CSRF_TOKEN_NOT_FOUND` and tells the user to authenticate with cookie/form web-session auth.
- Polls `/api/sn_cicd/progress/<executionTracker>` until success/failure/timeout, with a `sys_upgrade_history` fallback when the upload response does not include an execution tracker.
- Queries Build Agent `runQuery` endpoints for `sys_ui_page` and `sys_db_object` artifacts and formats installed table links as `<instance>/<table>_list.do?sysparm_clear_stack=true`.
- Prints concise terminal progress lines for project sync, dependency install, SDK build, generated-file sync, upload, progress polling, and artifact discovery.

Glider VFS behavior:

- Uses `/api/sn_glider/v2/sync/state`, `/api/sn_glider/v2/sync/files`, and multipart `/api/sn_glider/v2/sync/changes/apply` for remote app workspace state/content.
- Resolves relative paths inside the active app root and rejects paths escaping that root.
- Blocks XML writes.
- Normalizes multipart `create`, `update`, and `remove` change lists to JSON arrays (`[]`) instead of `null`, matching the backend's expected shape and avoiding the observed `sync/changes/apply` HTTP 500/null-list failure mode.
- If `fs_write_file` or `fs_create_directory` receives a Glider apply error after the backend persisted the object, the CLI verifies remote content/state and returns success with a warning instead of reporting a false tool failure.

Implemented `run_diagnostics` behavior:

- Syncs the active Glider app/project into a temporary local directory.
- Validates up to 10 requested `.ts`, `.tsx`, `.js`, or `.jsx` files.
- Locates `node_modules/.bin/tsc` in the synced project or `tsc` on `PATH`.
- Runs local TypeScript diagnostics with `tsc --noEmit --pretty false` and returns captured diagnostics.
- If `tsc` is unavailable, prints `run_diagnostics: TypeScript compiler 'tsc' was not found in PATH or project node_modules/.bin; local diagnostics could not run.` on the terminal and returns code `TSC_NOT_FOUND`.

Implemented `fluent_topics_list` parity path:

- Resolves `payload.appId` or the active app id to `now-file:/<app_sys_id>`.
- Checks for a Fluent project through `now.config.json`; when exact-file Glider state misses the file, falls back to root app state and then direct `/sync/files` content lookup.
- Scans Glider state under `node_modules/@servicenow/sdk/docs`.
- If SDK docs are not present in Glider state, falls back to the last same-app local build/dependency temp project or syncs the Glider project to temp, installs `@servicenow/sdk` from `package.json`, and scans local SDK docs.
- Reads Markdown docs through Glider sync files or the local SDK package.
- Extracts a topic name from each Markdown basename and a summary from the first paragraph after the first heading.
- Returns JSON `[{"name":"...","summary":"..."}]` in `content` for `fluent-overview` and `*-guide` topics.
- Returns Web-extension-style errors such as `NO_FLUENT_PROJECT` and `SDK_VERSION_TOO_OLD` for missing project/old SDK docs cases.

## 18. Debugging and redaction

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

## 19. Script and pipe behavior

Implemented non-interactive guarantees:

- No alternate-screen REPL and no fixed footer/prompt band when stdin/stderr are not real TTYs.
- Non-interactive legacy `/conversation list` prints plain list output rather than opening picker.
- Scripted `--prompt` skips startup conversation selection unless `--conversation` is explicitly provided.
- Status bar is not injected into pipe/CI output.
- Usage lines remain available in non-interactive output and are copied into debug logs when `-debug <filename>` is active.
- Terminal Markdown renderer avoids TTY-only ANSI decoration when not interactive.

## 20. Known intentionally limited areas

Implemented but intentionally limited:

- Glider filesystem actions operate on the remote ServiceNow app workspace (`now-file:/<app_sys_id>`), not arbitrary local host paths.
- `--advertise-local-tools` remains experimental and should stay off for normal backend/MCP use.
- Local `build`/`install` parity shells out to local Node/npm and the app-local Fluent SDK. It does not emulate the Web IDE package-manager internals or sync full `node_modules` into Glider.
- Upload/install uses the browser-style upload processor and therefore needs a valid web-session CSRF token (`sysparm_ck` / `X-UserToken`); OAuth-only upload support is not assumed.
- `install` is same-session after a successful `build`; the package ZIP path is not persisted across CLI restarts.
- Metadata sync requires a refreshable OAuth profile because the Fluent incremental download API is OAuth-backed. If no valid/refreshable OAuth credential is available, build returns `METADATA_SYNC_AUTH_REQUIRED` without clearing the marker or installing stale metadata.
- `run_diagnostics` depends on a locally available TypeScript compiler in the synced project or on `PATH`; missing `tsc` is reported visibly as `TSC_NOT_FOUND`.
- `fluent_topics_list` first prefers SDK docs in the Glider project tree, then falls back to local same-session build/dependency temp docs or a temporary project dependency install. Missing docs in an installed SDK still reports `SDK_VERSION_TOO_OLD`, but missing Glider `node_modules` no longer falsely reports no Fluent project.
- Legacy `/send` streaming is not faked when the backend returns blocking JSON.
- WDF tool schemas are not exposed client-side by `/mcp list`; Forge loads pass-through tools after the Nirvana handshake, matching the web client behavior.
- Code Assist websocket mode is diagnostic/experimental, not the normal Build Agent path.

## 21. Current validation coverage

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
- Web UI-style app creation REST/Glider orchestration
- Glider `sync/changes/apply` nil-list normalization
- Glider FS post-error persistence verification
- local `run_diagnostics` missing-`tsc` and compiler-error behavior
- two-line tool-result terminal rendering
- `Building...` / `Connecting...` animations
- `fluent_topics_list` catalog scanning/error cases
- local build/install helper behavior: dependency merging with `eslint` ignored, scoped node dependency detection, generated-dir resolution, package ZIP discovery, app-local `now-sdk pack`, upload/progress/upgrade-history parsing, artifact-link formatting, and path safety
- metadata-sync behavior: absent/completed markers, exact millisecond-to-UTC `lastPull`, silent OAuth refresh, secure child input without argv leakage, transform failure preserving app-data, unknown-field-preserving atomic marker updates, create/update/remove Glider diffs, deletion timestamps, optimistic conflict rejection, post-write verification, and no-op sync

Recent manual/PTYS smokes confirmed:

- footer remains pinned while typing and streaming
- `Building...` appears above the 3-row prompt band
- status row stays at bottom and updates usage
- assistant output stays in scrollback, not row 1
- submitted prompt appears immediately as a gray prompt block
- footer prompt resets to empty while waiting


Interactive Nirvana tool events: successful `tool_result` events render as a green `✓ <tool>` line in the managed transcript; failed results render as a red `✗ <tool>` line. Tool call IDs are tracked so result events can display the original tool name. When a short result summary is available, it is rendered on the row below the green/red tool name in neutral table-text color, not appended to the tool-name line.


Committed transcript entries now use an opencode-inspired append model: submitted prompts, tool results, and final assistant rows are appended as new stable rows into the terminal scrollback instead of repainting the full transcript. Streaming assistant output commits newly stable rendered rows progressively and leaves the unstable tail until the next row/final completion. Resize remains the only path that rebuilds alternate-screen scrollback from the semantic transcript.


Markdown table blocks are held until stable while streaming: once a potential table is detected, the CLI commits only the blank-line-delimited stable prefix before the table; the completed table is appended after the block/final response is available so later rows cannot invalidate already-committed column widths.


The animated `Building...` indicator remains active while assistant output streams and is cleared only when the turn-end/finalization path runs.

## Multi-instance profiles

- Configured instances are represented by profile directories under `~/.ba-cli/profiles/<profile>/`; credentials and local workspace/conversation state remain per-instance/profile.
- Added `--instance-list` / `--instances` and `--instance-delete <profile|fqdn|url>` management commands.
- `--setup` can create/update an instance profile; when `--profile` is omitted the profile is derived from the instance FQDN.
- Interactive startup prompts with an instance picker when multiple instances are configured and no explicit profile/instance was passed; the last-used instance is preselected via `~/.ba-cli/active-instance`.
- Saved web-session and OAuth refresh failures prompt for reauthentication, instance removal, or cancel in interactive terminals.

## Picker menu styling

- Instance and conversation selection menus render with ANSI styling in interactive terminals: bold cyan headers, green selected arrows, yellow current markers, cyan URLs, green positive badges (`[oauth]`, `[web-session]`, `[open]`), and red cancel/error/no-credential badges.
- The picker line helpers remain plain-text-compatible for tests/non-TTY output.

## Typed slash-command registry

Slash commands are implemented through a single typed registry with canonical names, aliases, descriptions/categories, strict presentation order, argument policy, suggestion behavior, runtime availability, active-turn availability, modal/capture policy, and handlers. `/help`, dispatch, completion, capture decisions, and interactive-menu behavior derive from that registry. The interactive prompt receives the active `Client`, so menu suggestions correctly exclude unavailable transport commands and commands blocked while a turn is processing. `/help` and `/exit`/`/quit` remain available while processing; context-changing commands return `cannot run /<command> while a turn is processing`.

## 2.6 Offline `/search` and `/export`

`/search <query> [--json] [--limit N]` and `/export [path] [--json]` are typed, processing-available commands with no remote calls. Search emits stable `SearchSchemaVersion: 1` JSON and only inspects retained redacted semantic-event summaries, safe runtime/context/app/turn/tool labels, and local history metadata (role/ID/time/hash/size). Full user/assistant bodies are neither indexed nor displayed. Query length and result limits are bounded; credential-like queries are rejected without raw echo; result scoring/order is deterministic and overall versus journal/runtime/message source health remains explicit. Export selects exactly one active/last conversation-turn scope (or an explicitly empty unknown scope), never mixes another conversation, retains a pruned safe suffix when scope evidence remains, and fails closed only event inclusion on corruption. It emits a distinct `ExportSchemaVersion: 1` deterministic ZIP with allowlisted redacted JSON members and a sorted manifest containing SHA-256/size entries. It snapshots active state through existing immutable turn snapshots, bounds event/message/payload volume, omits message bodies in favor of scope-tagged metadata; hashes are domain-separated and omitted for short or credential-like content, rejects canaries/private material, and uses private default `exports/`, conservative custom relative `.zip` paths, symlink-ancestry rejection, 0600/0700 permissions, temp+hard-link atomic no-overwrite publish, and directory sync. It never includes credentials/tokens/session/browser data, raw journals/frames/logs, prompt/assistant content, attachments, arbitrary files, source, HOME paths, or usernames.

## 2.9 Centralized retry/fallback/telemetry policy

Safe GET/discovery/status reads use typed bounded `RetryPolicy`/`RetryDecision` and redacted local `AttemptTelemetry`; retries are limited to network, 408, 429, and 5xx responses, honor `Retry-After`, backoff/jitter, elapsed/attempt budgets, and context cancellation. Writes, tools, and logical user-turn submission are not auto-retried. Typed retry/fallback semantic events are reducer/journal compatible, debug-redacted, and legacy transport fallback is admitted only before turn semantic progress. `/status` and `/turn` report actual observed retry telemetry.

## 2.5 Runtime `/turn` and local `/debug`

The typed slash-command registry includes `/turn`, `/turn --json`, and `/debug status|on|off|tail|event`; all are available while a turn is processing. `/turn` is offline-safe and read-only. During a turn it reads the immutable accepted `TurnRuntimeSnapshot` plus cloned reducer state; after a turn it reports the last reducer-owned semantic state. Stable JSON is `schemaVersion: 1`, uses UTC RFC3339 timestamps, deterministic tool/component ordering, and explicitly reports availability as `no_turn`, `active`, or `last`. Start/terminal timing is derived read-only from a complete retained archive-plus-live semantic lifecycle chain; active duration advances from the local active start time, while terminal duration is frozen at terminal-minus-start. Missing, corrupt, or pruned journal timing remains unknown rather than inferred. It also explicitly distinguishes staged next-turn context, observed versus unknown usage/retry/fallback/telemetry facts, and local journal missing/empty/corrupt/pruned health. `/debug` is local process state, defaults off, and does not contact a remote service. `tail` has a default of 20 and hard cap of 100; `event` looks up a retained journal event by ID. Both only read the archive-plus-live journal chain and report missing/corrupt/pruned conditions explicitly. Debug event details are normalized allowlisted lifecycle/tool/usage/context/retry/fallback/telemetry fields; assistant deltas are represented as `[omitted]`, and human detail maps are compact deterministic JSON. Command validation/not-found errors do not echo raw command-controlled values. Credentials, cookies, OAuth tokens, passwords, `g_ck`, auth headers, raw transport frames, full user prompts/assistant content, attachment bodies, and arbitrary unsafe metadata/payloads are excluded.
