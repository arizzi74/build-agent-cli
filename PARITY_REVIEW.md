# HAR Remaining Schemas — Implementation Review

Reviewed 2026-07-13 CEST against `HAR_REMAINING_SCHEMAS.md`.

Scope reviewed: `rich_persistence.go`, `telemetry.go`, `app_checkpoint.go`, `startup_config.go`, and their active integrations in `client.go` / `app_creation.go`. No source files were edited.

## Verification

- `go build ./...` — **PASS**
- `go vet ./...` — **PASS**
- `go test -count=1 ./...` — **FAILS TO COMPILE** before tests run:
  - `app_checkpoint_test.go:28`: obsolete three-argument call to `PatchAppCreatedCheckpoint`; production signature now takes `RichUserContent` and app name.
  - `telemetry_test.go:81,82,88`: obsolete `[]string` fixture values for fields now implemented as `int`, `int`, and `string`.

This leaves the implementation without an executable regression suite in the current tree.

## Concrete parity gaps / regressions

### P0 — repository test suite is broken by stale tests

**Observed:** Full `go test ./...` cannot compile because `app_checkpoint_test.go` and `telemetry_test.go` no longer match the production APIs/schema types.

**Why it matters:** The newly added parity behavior cannot be validated in CI, and stale tests still assert the old incorrect checkpoint body and telemetry array types.

**Exact fix:**
1. Replace the checkpoint test call with:
   `PatchAppCreatedCheckpoint(ctx, "msg", NewRichUserContent("user-id", "create application My App"), "app123", "My App")`.
2. Assert the full required outer payload and stringified content:
   `{"content":"{\"id\":\"user-id\",\"sender\":\"user\",\"text\":\"create application My App\",\"hasCheckpoints\":true,\"checkpoints\":[{\"id\":\"APP_CREATED:app123\",\"appDir\":\"My App\"}]}"}`.
3. Update telemetry fixtures/assertions to use `rollbacks: 0`, `metadata_types: 0`, `build_fix_errors: ""`, and start `sys_id: "-1"`.
4. Run `go test -count=1 ./...` in CI.

### P0 — tool/event telemetry is completely missing

**Observed:** `telemetry.go` only posts `isEventTelemetry:false`. There is no event payload type, no tool-attempt collection, no `event_id`, and no terminal batch post. `client.go` persists UI tool rows but never posts the required telemetry batch.

**HAR requirement:** After complete/abort telemetry, POST one `isEventTelemetry:true` array entry per tool attempt. Each row requires `name`, `start_time`, `end_time`, title-cased `Success` / `Failure`, `sys_id:"-1"`, `event_id:<turn telemetry id>`, and `errors`.

**Exact fix:**
1. Add `BuildAgentToolTelemetry` state to `Client` (or the turn telemetry state), populated at `tool_call` and completed at `tool_result`.
2. Preserve the untruncated raw failure text for `errors`; use `""` on success.
3. Add `postBuildAgentToolTelemetry(ctx, eventID, rows)` that sends `{"payload":[...],"isEventTelemetry":true}`.
4. Call it **after** `CompleteBuildAgentTelemetry` or `CancelBuildAgentTelemetry`/abort terminal update, only if the start response produced a real telemetry sys_id.
5. Add order-sensitive `httptest` coverage for start → terminal → event batch, plus success/failure serialization.

### P0 — start telemetry is ordered after the persisted user row

**Observed:** In Nirvana `SendMessage`, the client persists the user message first and only then calls `StartBuildAgentTelemetry`.

**HAR requirement:** The first operation of a turn is start telemetry (`status:"in-progress"`, `sys_id:"-1"`), followed by the user message POST.

**Exact fix:** Move `c.turnStartedAt = time.Now()` and `c.turnTelemetry = c.StartBuildAgentTelemetry(ctx, content)` to immediately after conversation creation/selection and before `PersistRichWebMessageContent`. Keep cleanup if the later user persistence or websocket write fails. Add a request-sequence test asserting telemetry start precedes `/messages` POST.

### P0 — cancellation never persists the required `sender:"stop"` message

**Observed:** `cancelActiveTurn` sends the websocket stop frame and posts abort telemetry, but does not persist a rich stop row.

**HAR requirement:** After the websocket stop/turn abort, persist:
`{"id":"...","sender":"stop","text":"Processing was stopped. You can send a new message to continue."}`.

**Exact fix:** In `cancelActiveTurn`, after writing the websocket stop frame and before resolving `turnDone`, best-effort POST one rich stop row for the active conversation. Guard it with a per-turn `stopPersisted` latch so repeated cancel/late events cannot double-post. Add a test for websocket-stop → message POST order and exact nested JSON.

### P1 — APP_CREATED ordering clears `workingSet` too early

**Observed:** `createServiceNowAppLikeWebUI` calls `patchConversationApplication`, which performs:
1. conversation application binding,
2. title PUT,
3. `PATCH {"workingSet":[]}`,
then returns to call `PatchAppCreatedCheckpoint`.

**HAR requirement:** binding → title → initiating-user-message checkpoint PATCH → create-app `assistant-tool` row → final assistant row → only then clear `workingSet`.

**Exact fix:** Make `patchConversationApplication` only bind the application and set the title. Move the working-set-clear PATCH to the Nirvana terminal path, after the final assistant rich row is persisted (and after the tool row naturally produced by the tool result event). For app creation failures, do not clear it merely because the tool was attempted. Add an ordered integration test covering binding, title, checkpoint, tool, final, working-set clear.

### P1 — rich tool rows do not preserve the HAR’s raw result or a dependable actual tool name

**Observed:** At `tool_result`, `Result` is `eventToolSummary(event)`, which reduces output to a single-line, 160-rune display summary. `ToolActualName` is set equal to `name`, which may be a display name or be prefixed as `server/name` by `eventToolName`.

**HAR requirement:** `toolInput` is the raw argument object; `result` is normally the raw (potentially multiline) tool output; `toolName` is display text and `toolActualName` is the raw executable tool identifier.

**Exact fix:** Extend tracked tool-call metadata with both display and raw tool names. Store a raw result/error value from the event separately from terminal display summarization. Marshal the raw result to the rich row; only use `eventToolSummary` for terminal UI. Add a test with multiline output and distinct display/actual names.

### P1 — rich message/telemetry REST requests use OAuth Bearer in Nirvana mode instead of the captured browser-session scheme

**Observed:** `PersistRichWebMessageContent`, checkpoint PATCH, and telemetry use `postJSON`/`patchJSON`/`setGatewayHeaders`. When `opts.Nirvana` and an OAuth token are present, `setGatewayHeaders` sets `Authorization: Bearer ...` and deliberately omits both session cookies and `X-UserToken`.

**HAR requirement:** Message, telemetry, Glider sync, and dynamic-config endpoints are same-origin browser-session calls using `X-UserToken` plus cookie session. No Bearer header was observed.

**Exact fix:** Add a Build-Agent-web request header helper (analogous to `setGliderWebHeaders`) that always retains available session cookie + `X-UserToken`, removes OAuth Bearer for these browser REST endpoints, and uses the appropriate worker/IDE referer. Use it for rich message POST/PATCH, telemetry, startup config, and conversation binding/title calls. Keep Bearer only for the Nirvana websocket/OAuth flow unless a capture proves REST Bearer support. Add header tests in Nirvana mode.

### P1 — dynamic startup discovery is materially incomplete

**Observed:** `loadWebStartupConfig` only requests variant, health, mock-LLM property, Nirvana URL property, one property bundle, and IDE update availability.

**Missing HAR endpoints/behavior:**
- Glider applications list and startup workspace `sync/state` + `/files`.
- `sn_build_agent.properties_cache_ttl_minutes` lookup and caching.
- `providerConfig`.
- direct `sn_build_agent.tool.execution.timeouts` fallback lookup.
- conversation list and selected conversation messages.
- product availability, skills summary, and semantic-search status (optional/tolerant).

**Exact fix:** Expand the startup plan in HAR order: Glider app list/update → workspace sync/files → Build Agent property/config bundle (including TTL and direct-timeout fallback) → conversations/messages → availability/skills/semantic status. Make optional endpoint failures non-fatal. Cache property-derived overrides until the returned TTL expires. Add endpoint-order and optional-404 tests.

### P1 — parsed startup timeouts are not applied anywhere

**Observed:** `WebStartupConfig.ToolTimeouts` and `DefaultTimeout` are parsed and returned, but no client field is retained and no tool execution path consumes them.

**HAR requirement:** Dynamic timeout property is used as an override/fallback.

**Exact fix:** Store the loaded config on `Client`; route tool execution timeout selection through `cfg.ToolTimeout(toolName)` before static defaults. Preserve explicit CLI overrides. Add a test showing the parsed `create_new_servicenow_app` timeout is selected for that handler and default applies to an unknown tool.

### P2 — no properties TTL caching; configuration is fetched on every `Connect`

**Observed:** `Connect` always calls `loadWebStartupConfig`; `sn_build_agent.properties_cache_ttl_minutes` is not fetched or honored.

**Exact fix:** Add a timestamped startup-config cache keyed by instance/profile. Fetch/parse the TTL property; reuse valid cached overrides, and retain local defaults for malformed/missing values. Test no refetch inside TTL and refetch after expiry.

### P2 — turn terminal semantics are not fully HAR-aligned for errors

**Observed:** cancellation correctly uses `agent_status:"error_internal"` and `status:"aborted"`, but generic `turn_error` uses `agent_status:"success"` inherited from start and a non-observed `status:"error"`.

**HAR evidence:** Captured terminal paths are success (`complete`) and cancellation/abort (`agent_status:"error_internal"`, `status:"aborted"`), retaining all start fields.

**Exact fix:** Define an explicit server-error terminal mapping based on an additional capture, or conservatively map unrecoverable `turn_error` to the same documented abort shape with an appropriate non-success agent status. Add a test asserting the terminal update preserves all start fields and reuses the start sys_id.

### P2 — APP_CREATED checkpoint is append-only and can duplicate the same checkpoint

**Observed:** `PatchAppCreatedCheckpoint` always appends `APP_CREATED:<appSysID>` without de-duplicating an existing checkpoint.

**Exact fix:** Merge by checkpoint ID before serializing the full original user row; preserve unrelated existing checkpoints. Add a test with an existing APP_CREATED checkpoint and a second unrelated checkpoint.

### P2 — only the Nirvana send/receive path is integrated with rich persistence and telemetry

**Observed:** The non-Nirvana `sendGatewayMessage` path still persists legacy rows/endpoints and never starts/finalizes telemetry or posts rich final/tool/thinking rows. If users can select that transport, behavior diverges from the captured Web UI protocol.

**Exact fix:** Either integrate the same persistence/telemetry lifecycle into the supported non-Nirvana transport, or explicitly mark it legacy/non-parity and prevent it from being advertised as Web UI parity. Add a transport-specific regression test.

## Items that match the HAR schema (once test fixtures are repaired)

- Rich user content now includes stable UUID `id`, `sender:"user"`, original `text`, and `hasCheckpoints:false`.
- Rich assistant thinking/final/tool content field order and stringified outer `content` envelope are substantially aligned.
- The checkpoint production code now keeps the original user `id`/`text`, sends `APP_CREATED:<appSysID>`, and uses display app name as `appDir`.
- App bootstrap sync excludes physical `node_modules` and `.gitignore` excludes it, matching the virtual-package-manager guidance.
- App upload/Glider paths use the session-oriented helper, unlike the rich/telemetry paths noted above.
