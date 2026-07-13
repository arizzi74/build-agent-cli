# Future Enhancements for Build Agent Go CLI

Date: 2026-07-07

Scope: analysis only. This file compares the current Build Agent Go CLI with OpenAI Codex and records design ideas for future work. No source-code changes are implied by this document.

Codex reference snapshot reviewed:

- Repository: https://github.com/openai/codex
- Commit: `9deb4f9c86426c40ba1e189831d7bc3634dd7b94`
- Commit title/date: `Handle mixed-case URLs in Windows command safety (#30879)`, 2026-07-07 17:50:39 -0400

Build Agent CLI reference snapshot:

- Repository path: `/home/ubuntu/.openclaw/workspace/build-agent-go-cli`
- Latest source commit at analysis time: `3df9e61 Confirm workspace reset slash command`

## What `/workspace reset` does today

`/workspace reset [name]` is a local workspace-state reset, not a remote ServiceNow conversation deletion.

Exact behavior from the current code:

1. If a workspace name is supplied and it is not the current workspace, the CLI first switches to that workspace.
2. If a Build Agent turn is currently processing, reset fails with `cannot reset workspace while a turn is processing`.
3. It clears the active workspace's local conversation/session state:
   - `conversationID`
   - `conversationTitle`
   - `conversationState`
   - `serverConversation`
   - local `history`
   - usage totals
   - `workingSet`
4. It saves the cleared workspace state back under `~/.ba-cli/profiles/<profile>/workspaces/<workspace>.json`.
5. It prints `workspace reset: <workspace>`.

It does **not** delete:

- the workspace file itself;
- the active profile;
- OAuth/session credentials;
- selected app scope/config;
- server-side ServiceNow conversation rows.

Practical meaning: after reset, that workspace behaves like a fresh local chat context. The next prompt creates/uses a new local conversation state. Old ServiceNow conversations can still be selected again through `/conversation` if they exist on the instance.

## Executive summary

Codex is useful less because of one specific feature and more because of its architecture:

- UI output is state-driven, not ad-hoc `fmt.Print`-driven.
- Slash commands are typed, centrally declared, filtered, and dispatched with explicit command metadata.
- Sessions are append-only journals that can be listed, resumed, reconstructed, compacted, forked, and inspected.
- Network streaming has explicit per-turn sessions, retry/fallback policy, transport telemetry, and typed lifecycle events.
- Config, permissions, status, and debug surfaces are first-class user-facing commands.

The Build Agent Go CLI already moved in the right direction with a managed terminal transcript, captured slash-command output, conversation picker parity, and source-controlled tests. The biggest next improvement is to make those ideas systematic: a typed command registry, an append-only local event journal, and a typed transport event pipeline.

## Recommended roadmap

### P0 — Highest impact, low-to-medium risk

1. **Replace scattered slash-command metadata with a typed command registry.**
   - Each command should declare: name, aliases, description, visibility, category, whether it is modal, whether it accepts inline args, whether selecting it should execute or insert a template, and whether output should be captured into the transcript.
   - This directly prevents regressions like hidden output, accidental removal of non-conversation commands, or selected-command crashes.

2. **Add an append-only local event journal per workspace/conversation.**
   - Keep the current compact workspace JSON as a fast snapshot, but also record turns/events to JSONL.
   - This enables crash recovery, local transcript search, export, better resume previews, and debugging of weird UI/network states.

3. **Introduce typed transport events.**
   - Normalize ServiceNow websocket/REST/AMB responses into internal events such as `TurnStarted`, `AssistantDelta`, `AssistantDone`, `ToolStart`, `ToolEnd`, `Usage`, `Error`, `StreamRetry`, `TurnComplete`.
   - UI, state persistence, logging, and tests should consume those events instead of raw backend frames.

4. **Improve network retry/fallback behavior.**
   - Codex has session-scoped websocket fallback to HTTP and explicit retry handling. Build Agent CLI should add a clear retry budget and surface `reconnecting...`, `retrying...`, or `fallback transport...` as visible system rows.

5. **Make `/conversation` and workspace resume screens richer.**
   - Show title, last update, app/scope, server/local marker, and a short preview.
   - Preserve current Web UI parity while improving local discoverability.

### P1 — UX polish and power-user value

1. **Command palette parity with Codex.**
   - Keep `/conversation` simple, but make all other commands consistently discoverable.
   - Add categories and maybe hidden advanced/debug commands.

2. **Raw/copy mode.**
   - Codex has a raw scrollback mode for copy-friendly terminal selection. Build Agent CLI would benefit from a toggle that temporarily disables styled prompt bands/markdown rendering for clean copying.

3. **`/status` and `/debug config`.**
   - Show active instance/profile/workspace/app, transport, auth source, model/provider, token counters, websocket state, loaded MCP/WDF server count, and config source precedence.

4. **Transcript search and export.**
   - Add `/search <text>` and `/export markdown|json` using the local event journal.

5. **User-configurable keymap/status line/theme.**
   - Codex treats keymap, status line, and theme as configurable TUI concerns. Build Agent CLI can start smaller: keybindings for picker navigation, status-line fields, and color disable/high-contrast modes.

### P2 — Larger features, only after P0/P1 foundations

1. **Fork/side conversations.**
   - Codex supports new/fork/side conversation concepts. Build Agent CLI could support local forks of a workspace conversation, even if server-side ServiceNow cannot represent all fork metadata.

2. **Local tool support with approvals.**
   - If `--advertise-local-tools` ever becomes real, copy Codex's approval-first mindset: explicit permission profiles, dry-run previews, and safe rejection for out-of-scope actions.

3. **Plugin-like extension points.**
   - Codex has plugin/app concepts. Build Agent CLI could eventually support local extensions for custom app pickers, transcript exporters, diagnostics, or team-specific commands.

## Detailed design blocks

## 1. UX and slash-command system

Codex references:

- `codex-rs/tui/src/slash_command.rs`
- `codex-rs/tui/src/bottom_pane/slash_commands.rs`
- `codex-rs/tui/src/bottom_pane/command_popup.rs`
- `codex-rs/tui/src/bottom_pane/chat_composer/slash_input.rs`

Codex uses a typed `SlashCommand` enum with command descriptions, presentation order, aliases, feature gates, and `supports_inline_args()` behavior. The command popup filters commands predictably, preserves presentation order, clamps selection, and returns a selected command as typed data rather than as loose text.

Relevant ideas for Build Agent CLI:

- Make slash commands data-driven rather than split across suggestions, help text, parser branches, tests, and docs.
- Add command kinds:
  - `Immediate`: executes on Enter, e.g. `/help`, `/workspace list`.
  - `Modal`: opens a picker, e.g. `/conversation`.
  - `Template`: inserts a text prefix requiring user args, e.g. `/workspace use `.
  - `InlineArgs`: runs with parsed args, e.g. `/app use <scope>`.
- Store visibility separately from support. Legacy commands can remain supported but hidden.
- Keep completion behavior explicit: Enter executes complete commands, Tab completes text, templates insert text.
- All non-modal output should render through the managed transcript, never directly to a hidden/overwritten stderr row.

Suggested Build Agent command metadata shape:

```go
type SlashCommandSpec struct {
    Text        string
    Aliases     []string
    Description string
    Category    string
    Kind        SlashCommandKind
    Hidden      bool
    RequiresArg bool
    CaptureOutput bool
    Handler     SlashCommandHandler
}
```

## 2. Terminal rendering and UI state

Codex references:

- `codex-rs/tui/src/app_event.rs`
- `codex-rs/tui/src/app/event_dispatch.rs`
- `codex-rs/tui/src/bottom_pane/chat_composer.rs`
- `codex-rs/tui/src/bottom_pane/command_popup.rs`

Codex routes UI work through app-level events and frame redraw requests. Pickers and commands do not own the whole terminal; they ask the app to change state, then the renderer redraws from state.

Build Agent CLI already moved toward this with the managed transcript and footer. The next step is to make all terminal changes event/state driven:

- One UI state object: transcript rows, active assistant stream, footer prompt, temp message, picker/menu, status bar, raw/copy mode.
- One render path: resize, command output, conversation switch, stream delta, and prompt typing all replay from state.
- No direct output writes for interactive command results.
- Explicit frame scheduling after modal picker close/open, conversation switch, and resize.
- Golden/snapshot tests for terminal rows after common sequences:
  - open slash menu;
  - execute `/workspace list`;
  - switch conversation;
  - stream assistant deltas;
  - resize while response is active.

## 3. Session and history management

Codex references:

- `codex-rs/rollout/src/recorder.rs`
- `codex-rs/rollout/src/list.rs`
- `codex-rs/core/src/session/rollout_reconstruction.rs`
- `codex-rs/protocol/src/protocol.rs`

Codex records sessions as inspectable JSONL rollouts. Session listing reads metadata such as first user message, preview, cwd, git branch, provider, version, created/updated timestamps, parent thread, and source. Resume reconstruction can rebuild history while handling compaction and rollback.

Build Agent CLI currently stores workspace snapshots. That is simple and fast, but fragile for debugging and resume UX.

Recommended design:

- Keep snapshot JSON for quick startup.
- Add `events/<workspace>/<conversation>.jsonl` or `workspaces/<workspace>.events.jsonl`.
- Record internal typed events, not raw secrets or raw websocket frames.
- Include metadata events:
  - instance URL host only;
  - profile name;
  - workspace;
  - app scope;
  - server conversation id/title/state;
  - model/provider;
  - CLI version/build SHA;
  - timestamps.
- Use event replay to rebuild transcript if the snapshot is missing/corrupt.
- Add local commands:
  - `/history` or `/conversation local` to browse local journals;
  - `/export markdown`;
  - `/archive` for local cleanup without deleting remote server conversations.

This would also clarify `/workspace reset`: reset can become a new journal event instead of only a destructive snapshot mutation.

## 4. Network communication, streaming, and retry policy

Codex references:

- `codex-rs/core/src/client.rs`
- `codex-rs/protocol/src/protocol.rs`

Codex separates session-scoped model client state from turn-scoped streaming sessions. It has:

- lazy websocket connection setup;
- websocket prewarm;
- a per-turn sticky routing token;
- explicit HTTP fallback if websocket transport becomes unavailable;
- transport telemetry;
- typed lifecycle events and stream error events.

Build Agent CLI has harder constraints because it is mirroring ServiceNow's Build Agent/Web UI/Nirvana behavior, not OpenAI Responses directly. Still, the same internal pattern applies:

- Define a `Transport` interface returning typed events, not direct UI text.
- Implement `NirvanaTransport` and legacy `WebGatewayTransport` behind that interface.
- Normalize backend-specific frames into common events.
- Track per-turn state explicitly:
  - turn id/request id;
  - conversation id;
  - app scope;
  - working set snapshot hash;
  - retry count;
  - stream start/end timestamps.
- Add heartbeat/ping handling with visible state only on problems.
- On transient websocket close, retry within a bounded budget and write a visible system row.
- If falling back to another transport, make it explicit in transcript/status, not silent.
- Keep debug redaction strict: tokens/cookies/user passwords must never enter event journals.

Suggested internal event names:

```text
TransportConnected
TransportFallback
TurnStarted
AssistantDelta
AssistantMessageDone
ToolCallStarted
ToolCallFinished
UsageUpdated
WorkingSetUpdated
AppScopeChanged
ConversationCreated
ConversationLoaded
StreamRetry
TurnFailed
TurnCompleted
```

## 5. Protocol and event model

Codex references:

- `codex-rs/protocol/src/protocol.rs`

Codex defines typed operations (`Op`) and typed events (`EventMsg`), including user input, approvals, compaction, rollback, token counts, assistant deltas, tool calls, stream errors, and completion.

Build Agent CLI should adopt a smaller internal protocol even if it never exposes it publicly:

- `CommandEvent`: emitted by slash commands.
- `TransportEvent`: emitted by ServiceNow backends.
- `StateEvent`: emitted by workspace/conversation/app changes.
- `UIEvent`: requests repaint, temp message, picker open/close.

Benefits:

- Easier tests: feed events, assert transcript/status/workspace state.
- Easier debug: event logs explain what happened.
- Easier future features: raw mode, export, side/fork, reconnect, local tools.

## 6. Configuration, profiles, and diagnostics

Codex references:

- `codex-rs/core/src/config/mod.rs`
- `codex-rs/tui/src/debug_config.rs`
- `docs/config.md`

Codex has layered config, requirements, keymaps, status-line settings, notification settings, permissions, and a debug command that explains config sources.

Build Agent CLI has profiles and workspace/app state. Recommended incremental improvements:

- Add a visible `/status` command:
  - profile;
  - instance host;
  - workspace;
  - app scope;
  - transport;
  - auth mode/token cache presence;
  - model/provider;
  - current conversation id/title;
  - WDF/MCP server count;
  - usage totals.
- Add `/debug config`:
  - show config values and their source: defaults, profile config, flags, env, workspace state.
- Add profile/global config files only when needed. Do not overcomplicate early.
- Keep CLI flags highest precedence and preserve current scriptability.

## 7. Permissions, approvals, and local tools

Codex references:

- `codex-rs/core/src/safety.rs`
- `codex-rs/protocol/src/protocol.rs`

Codex treats risky local actions as approval/permission problems with explicit events. Build Agent CLI currently does not implement local filesystem tools and should keep `--advertise-local-tools` experimental.

If local tools are added later:

- Require explicit opt-in.
- Add permission profiles, e.g. `read-only`, `workspace-write`, `disabled`.
- Show the exact command/patch/action before approval.
- Reject writes outside allowed roots by default.
- Journal approvals and denials without secrets.
- Never silently execute backend-requested local actions.

## 8. MCP/WDF and tool visibility

Codex references:

- `codex-rs/protocol/src/protocol.rs`
- Slash command `/mcp` behavior in `codex-rs/tui/src/slash_command.rs`

Build Agent CLI already has WDF/MCP parity work. Codex suggests improving tool visibility and startup diagnostics:

- `/mcp list`: compact list.
- `/mcp verbose`: transport, status, last error, discovered tool count.
- Show MCP startup progress only when slow or failed.
- Cache last known server/tool list per profile for startup troubleshooting.
- Add a diagnostic bundle command that redacts secrets.

## 9. Non-interactive/script mode

Codex keeps TUI behavior separate from non-interactive operation. Build Agent CLI should continue preserving clean scripted output.

Recommendations:

- Keep all new visual features behind TTY detection.
- Add `--json` for machine-readable turn events.
- Add `--print-conversation-id` and `--print-workspace` helpers for scripts.
- Ensure slash-command capture never pollutes non-TTY stdout/stderr unexpectedly.
- Add tests for every interactive feature's non-interactive no-op behavior.

## 10. Observability and supportability

Codex's rollouts and debug config make support easier because a session can be inspected after the fact.

Recommended Build Agent CLI support tooling:

- `--trace-file <path>`: write redacted internal events to JSONL.
- `/debug transport`: current websocket status, last close code, retry count, AMB/Nirvana path, provider config summary.
- `/debug last-turn`: last typed transport events and backend error details, redacted.
- `ba support-bundle`: creates a redacted archive with config summary, version, logs, and last N events.

## What not to copy from Codex yet

- Do not introduce a complex plugin marketplace before the command/event/session architecture is stable.
- Do not add local tools merely for parity; Build Agent's value is ServiceNow backend parity first.
- Do not expose every debug command in the default slash menu. Hide advanced commands behind `/debug` or feature flags.
- Do not let journal files capture raw websocket frames containing tokens, cookies, user data beyond transcript text, or secrets.

## Suggested first implementation sequence

1. Create `commands_registry.go` and migrate existing slash command metadata into it.
2. Add tests proving picker/help/parser/docs all derive from the same registry.
3. Add a small event journal writer for workspace state events and user/assistant transcript events.
4. Add `/status` and `/debug config` from existing runtime/profile/workspace state.
5. Wrap Nirvana and web-gateway stream handling behind a typed `TransportEvent` channel.
6. Add retry/fallback status rows using those transport events.
7. Improve `/conversation` rows with preview/app/updated metadata.
8. Add raw/copy mode and transcript export.

## Bottom line

The most Codex-like improvement is not a single visual feature. It is making Build Agent CLI state-driven:

- typed commands;
- typed events;
- append-only history;
- deterministic rendering;
- explicit transport lifecycle;
- user-visible diagnostics.

Those changes would make the CLI feel calmer and more trustworthy while preserving the current goal: ServiceNow Build Agent Web UI parity with a scriptable native terminal client.
