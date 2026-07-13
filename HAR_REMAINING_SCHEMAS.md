# Build Agent Web UI HAR Remaining Schema Report

Generated: 2026-07-13 (CEST)

Scope: all seven captures named in `HAR_PARITY_AUDIT.md`:

- `/tmp/New_conversation.har`
- `/tmp/lima_build.har`
- `/tmp/myapp_full.har`
- `/tmp/stop.har`
- `/tmp/demoalectriallwfze140800.service-now.com.har`
- `/tmp/workspace_demoalectriallwfze140800.service-now.com.har`
- `/tmp/zaiagents.service-now.com.har`

Conventions below: identifiers, tokens, hostnames, and user text are sanitized; field names, methods, endpoint shapes, content types, and ordering are kept as observed. Many HAR entries appear twice with identical request bodies because the browser worker/main-thread capture records duplicate views of one logical request; implement idempotently rather than deliberately double-posting.

## Shared HTTP/auth requirements

For Build Agent and Glider same-origin REST calls, the Web UI uses an authenticated browser session plus ServiceNow CSRF token header:

```http
X-UserToken: <session csrf token>
Content-Type: application/json | multipart/form-data; boundary=...
Accept: application/json or */* depending on caller
Referer: https://<instance>/scripts/raw/sn_glider_app/.../workerMain.js or forge-web-worker.js
```

No `Authorization: Bearer ...` header was observed on the message, telemetry, Glider sync, dynamic config, or app upload endpoints. OAuth/PKCE is captured, but the install upload processor still uses `X-UserToken` and a `sysparm_ck` form field.

## 1. Rich intermediate message persistence

### Endpoints

| Purpose | Method | Endpoint | Body | Success response |
|---|---:|---|---|---|
| Load stored messages | GET | `/api/sn_build_agent/build_agent_api/conversations/{conversation_sys_id}/messages` | none | `200 {"result":[messageRow...]}` |
| Persist a message row | POST | `/api/sn_build_agent/build_agent_api/conversations/{conversation_sys_id}/messages` | JSON outer object with `content` string | `201 {"result":{"content":"...","conversation":"{conversation_sys_id}","sysId":"{message_sys_id}"}}` |
| Mutate existing message row | PATCH | `/api/sn_build_agent/build_agent_api/conversations/{conversation_sys_id}/messages/{message_sys_id}` | JSON outer object with `content` string | `200 {"result":{"content":"...","conversation":"{conversation_sys_id}","sysId":"{message_sys_id}"}}` |

`POST`/`PATCH` bodies do **not** send the message content as a nested JSON object. They send a JSON string in the outer `content` field:

```json
{
  "content": "{\"id\":\"<client-uuid>\",\"sender\":\"assistant-thinking\",\"text\":\"...\",\"complete\":true}"
}
```

Stored `GET` rows use ServiceNow snake-case row metadata:

```json
{
  "content": "{...message JSON string...}",
  "conversation": "<conversation_sys_id>",
  "active": "true",
  "sequence": "157",
  "sys_created_on": "2026-03-23 20:16:37",
  "sys_id": "<message_sys_id>"
}
```

### Inner `content` schemas by `sender`

#### User

Observed on every new turn and as the row patched for checkpoints.

```json
{
  "id": "<client-uuid>",
  "sender": "user",
  "text": "<prompt text>",
  "hasCheckpoints": false
}
```

Optional fields from older/stored rows:

```json
{
  "author_type": "user",
  "type": "text",
  "images": [
    {
      "url": "",
      "name": "screenshot.png",
      "size": 406999,
      "type": "image/png",
      "sys_attachment_id": "<attachment_sys_id>"
    }
  ],
  "checkpoints": [
    {"id": "APP_CREATED:<app_sys_id>", "appDir": "<app display name>"}
  ]
}
```

#### Assistant thinking

Persisted for model reasoning/thinking rows during a turn.

```json
{
  "id": "<client-uuid>",
  "text": "<thinking text>",
  "complete": true,
  "duration": 1598,
  "sender": "assistant-thinking",
  "startTime": "2026-07-09T09:19:28.770Z"
}
```

Stored historical rows may include:

```json
{"signature": "<opaque signed thinking signature>"}
```

#### Assistant normal text

```json
{
  "id": "<client-uuid>",
  "text": "<assistant markdown>",
  "complete": true,
  "duration": 1704,
  "sender": "assistant",
  "startTime": "2026-07-07T15:11:48.198Z",
  "isLastMessageInTurn": true
}
```

`isLastMessageInTurn` appears on some stored final rows; captured live posts often omit it.

#### Assistant tool

Persisted after each tool completes. `toolInput` is the raw tool argument object; `result` is usually a string, often multiline.

```json
{
  "id": "<client-uuid>",
  "sender": "assistant-tool",
  "toolName": "Read File",
  "toolActualName": "fs_read_file",
  "complete": true,
  "duration": 82,
  "toolUseId": "c-<tool-call-uuid>",
  "startTime": "2026-07-09T09:19:30.371Z",
  "requiresApproval": false,
  "requiresSelection": false,
  "toolInput": {"path": "src/fluent/example.now.ts"},
  "success": true,
  "result": "<tool output>"
}
```

Optional fields observed in historical rows:

```json
{
  "checkpoint": "<checkpoint id or object>",
  "isLastDiff": true,
  "metadata": {"<tool-specific>": "..."}
}
```

#### Stop/system

Stop row after cancellation:

```json
{
  "id": "<client-uuid>",
  "sender": "stop",
  "text": "Processing was stopped. You can send a new message to continue."
}
```

Historical system row example:

```json
{"id":"<client-uuid>","sender":"system","text":"fetch failed"}
```

### Ordering for a full turn

Observed ordering for successful and cancelled turns:

1. Start telemetry with `status:"in-progress"` and `sys_id:"-1"`.
2. Persist user row with `POST /messages`; save returned `result.sysId` if later mutation/checkpoints are possible.
3. Run Nirvana websocket turn.
4. As model events complete, persist intermediate rows in chronological order: `assistant-thinking`, `assistant-tool`, normal `assistant`, etc. Rows include `startTime` and `duration` so replay can sort or display elapsed time even if post timing differs.
5. On cancellation, persist a `sender:"stop"` row after the websocket `stop` frame/turn abort.
6. Persist final assistant row; final row may include `isLastMessageInTurn:true`.
7. Patch conversation working set/application state as needed.
8. Complete/abort telemetry and then post tool telemetry batch.

## 2. Tool and turn telemetry

### Endpoint

```http
POST /api/sn_build_agent/build_agent_api/telemetry
Content-Type: application/json
X-UserToken: <session csrf token>
```

### Turn-level telemetry (`isEventTelemetry:false`)

Request body at turn start:

```json
{
  "payload": {
    "user": "<user_sys_id>",
    "request": "<original prompt>",
    "task_type": "create",
    "agent_status": "success",
    "start_time": "2026-07-09 09:19:14",
    "end_time": "",
    "status": "in-progress",
    "rollbacks": 0,
    "metadata_types": 0,
    "sys_id": "-1",
    "lines_added": 0,
    "lines_edited": 0,
    "lines_deleted": 0,
    "build_fix_cycles": 0,
    "build_fix_errors": ""
  },
  "isEventTelemetry": false
}
```

Request body at successful completion:

```json
{
  "payload": {
    "user": "<user_sys_id>",
    "request": "<original prompt>",
    "task_type": "create",
    "agent_status": "success",
    "start_time": "2026-07-09 09:19:14",
    "end_time": "2026-07-09 09:21:33",
    "status": "complete",
    "rollbacks": 0,
    "metadata_types": 0,
    "sys_id": "<telemetry_sys_id_from_start_response>",
    "lines_added": 0,
    "lines_edited": 0,
    "lines_deleted": 0,
    "build_fix_cycles": 0,
    "build_fix_errors": ""
  },
  "isEventTelemetry": false
}
```

Request body at cancellation/abort:

```json
{
  "payload": {
    "agent_status": "error_internal",
    "status": "aborted",
    "sys_id": "<telemetry_sys_id_from_start_response>",
    "end_time": "2026-07-12 21:59:07"
  },
  "isEventTelemetry": false
}
```

Keep all other turn fields from the start payload when sending the abort/completion update.

Success response:

```json
{
  "result": {
    "error": false,
    "msg": "Telemetry record updated successfully",
    "sys_id": "<telemetry_sys_id>"
  }
}
```

Implementation note: when the start call uses `sys_id:"-1"`, capture `result.sys_id`; later turn update and all tool telemetry rows reference that ID.

### Tool/event telemetry (`isEventTelemetry:true`)

Sent after the turn-level completion/abort update as an array, one object per tool attempt.

```json
{
  "payload": [
    {
      "name": "fs_read_directory",
      "start_time": "2026-07-09 09:19:26",
      "end_time": "2026-07-09 09:19:26",
      "status": "Failure",
      "sys_id": "-1",
      "event_id": "<turn_telemetry_sys_id>",
      "errors": "Failed to read directory .build-agent/skills: ENOENT: <path>"
    },
    {
      "name": "build",
      "start_time": "2026-07-09 09:19:33",
      "end_time": "2026-07-09 09:21:04",
      "status": "Success",
      "sys_id": "-1",
      "event_id": "<turn_telemetry_sys_id>",
      "errors": ""
    }
  ],
  "isEventTelemetry": true
}
```

Success response:

```json
{
  "result": [
    {"error": false, "msg": "Telemetry record updated successfully", "sys_id": "<tool_telemetry_sys_id>"}
  ]
}
```

Observed `status` values are title-cased `Success` and `Failure`; turn-level status values are lower-case (`in-progress`, `complete`, `aborted`).

## 3. `APP_CREATED` checkpoint mutation

### Endpoints and ordering

Captured successful app creation mutates the initiating user message after the app exists and after conversation binding/title update, but before the create-app tool row/final assistant row are persisted.

Observed order:

1. `POST /api/sn_build_agent/build_agent_api/conversations/{conversation_id}/messages` for the initiating user row.
2. Save `result.sysId` from that POST as `{initiating_message_sys_id}`.
3. Create app through `/api/now/templates` + `/api/now/templates/status` and write workspace through Glider sync.
4. `PATCH /api/sn_build_agent/build_agent_api/conversations/{conversation_id}` with app binding.
5. `PUT /api/sn_build_agent/build_agent_api/conversations/{conversation_id}/title`.
6. `PATCH /api/sn_build_agent/build_agent_api/conversations/{conversation_id}/messages/{initiating_message_sys_id}` with updated user content below.
7. Persist `assistant-tool` row for `create_new_servicenow_app`.
8. Persist final assistant row.
9. `PATCH /api/sn_build_agent/build_agent_api/conversations/{conversation_id}` with `{"workingSet":[]}`.

### Conversation binding/title requests

```http
PATCH /api/sn_build_agent/build_agent_api/conversations/{conversation_id}
Content-Type: application/json
```

```json
{
  "applicationId": "<app_sys_id>",
  "applicationName": ""
}
```

Response key fields:

```json
{
  "result": {
    "sys_id": "<conversation_id>",
    "application_id": "<app_sys_id>",
    "application_name": null,
    "working_set": null,
    "context_summary": null,
    "last_summarized_msg_index": "0"
  }
}
```

```http
PUT /api/sn_build_agent/build_agent_api/conversations/{conversation_id}/title
Content-Type: application/json
```

```json
{"title":"<App Name>: <first user prompt>"}
```

Response:

```json
{"result":{"title":"<App Name>: <first user prompt>"}}
```

### Checkpoint patch request

```http
PATCH /api/sn_build_agent/build_agent_api/conversations/{conversation_id}/messages/{initiating_message_sys_id}
Content-Type: application/json
```

```json
{
  "content": "{\"id\":\"<same user client uuid>\",\"sender\":\"user\",\"text\":\"create application <App Name>\",\"hasCheckpoints\":true,\"checkpoints\":[{\"id\":\"APP_CREATED:<app_sys_id>\",\"appDir\":\"<App Name>\"}]}"
}
```

Response:

```json
{
  "result": {
    "content": "{...same patched content string...}",
    "conversation": "<conversation_id>",
    "sysId": "<initiating_message_sys_id>"
  }
}
```

Implementation detail: keep the original user row `id` and `text`; only flip `hasCheckpoints` to `true` and add/merge `checkpoints`. The observed app-created checkpoint ID is exactly `APP_CREATED:<app_sys_id>` and `appDir` is the display app name, not the scope string.

## 4. Glider virtual package-manager materialization

The Web UI does not sync a physical `node_modules` tree through `/api/sn_glider/v2/sync/changes/apply`. Captured filesystem sync writes normal project files, `.gitignore` excludes `node_modules/`, and dependency code is later served by Glider virtual-module and package-manager paths.

### Glider sync state

```http
POST /api/sn_glider/v2/sync/state
Content-Type: application/json
X-UserToken: <session csrf token>
```

```json
{
  "uris": [
    "now-file:/<template_or_app_sys_id>",
    "now-file:/<active_app_sys_id>",
    "settings:/users/<user_sys_id>"
  ]
}
```

Response key fields:

```json
{
  "timeStamp": 1783588865578,
  "files": [
    {
      "uri": "now-file:/<app_sys_id>/package.json",
      "type": "file",
      "checksum": "<sha1>",
      "size": "546",
      "ctime": "1783514924281",
      "dtime": null,
      "mtime": "1783514924281",
      "mode": null
    },
    {
      "uri": "now-file:/<app_sys_id>/src",
      "type": "dir",
      "checksum": null,
      "size": "0",
      "ctime": "1783514924276",
      "dtime": null,
      "mtime": "1783514924276",
      "mode": null
    }
  ]
}
```

### Glider file fetch

```http
POST /api/sn_glider/v2/sync/files
Content-Type: multipart/form-data
```

Multipart fields:

- `uris` — `filename="blob"`, `Content-Type: application/octet-stream`, payload is a JSON array of URI strings. Empty fetches were captured as `[]`.

### Glider change apply

```http
POST /api/sn_glider/v2/sync/changes/apply
Content-Type: multipart/form-data
```

Multipart fields:

- `create` — `filename="blob"`, `Content-Type: application/json`, JSON array of file/dir descriptors.
- `update` — same shape.
- `remove` — same shape.
- One binary part per file content, named by the file descriptor `checksum`; `filename` is the same checksum; `Content-Type: application/octet-stream`.

Descriptor schema:

```json
{
  "checksum": "<sha1>",
  "ctime": 1783516383828,
  "dtime": null,
  "mtime": 1783516383831,
  "size": 406,
  "type": "file",
  "uri": "now-file:/<app_sys_id>/src/server/tsconfig.json"
}
```

Directory descriptor omits `checksum` and uses `size:0`, `type:"dir"`.

Representative app bootstrap `create` includes dirs/files such as:

- `.vscode/`
- `src/`
- `src/fluent/`
- `src/fluent/generated/`
- `.now/`
- `.gitignore` (`node_modules/` is ignored)
- `.vscode/extensions.json`
- `now.config.json`
- `package.json`
- `src/fluent/generated/keys.ts`

### Package-manager/virtual-module fetches

NPM package metadata/tarball fetches are plain GETs from same origin/package paths. Observed examples:

```http
GET /@servicenow/isomorphic-rollup
Accept: application/vnd.npm.install-v1+json

GET /oidc-token-hash
GET /packageurl-js
GET /prebuild-install
GET /github-from-package
GET /socks-proxy-agent
GET /socks
```

Tarballs:

```http
GET /@servicenow/isomorphic-rollup/-/isomorphic-rollup-1.3.6.tgz
GET /oidc-token-hash/-/oidc-token-hash-5.2.0.tgz
GET /packageurl-js/-/packageurl-js-2.0.1.tgz
GET /prebuild-install/-/prebuild-install-7.1.3.tgz
```

Response MIME types:

- Metadata: `application/vnd.npm.install-v1+json`
- Tarball: `application/octet-stream`

The Web UI then loads SDK code from the Glider virtual package tree:

```http
GET /sn_glider_app/virtual-module:/{app_sys_id}/node_modules/@servicenow/sdk/dist/web/index.js?c=<cache_token_or_undefined>
```

Response MIME: `application/javascript`; captured response size is about 19 MB.

Implementation implication: for full parity, either reproduce the Web UI's virtual package-manager resolution enough for tools that expect `now-file:/<app>/node_modules/...`, or explicitly keep the current local temp npm install path and only sync generated project files. Do **not** push a physical `node_modules` tree through Glider sync; that is not what the HAR shows.

## 5. Upload authentication options

### OAuth/PKCE sequence captured before turns

The Web UI attempts to reuse a stored refresh token, then falls back to PKCE authorization-code flow when refresh fails.

1. Fetch stored refresh token:

```http
GET /api/sn_build_agent/build_agent_api/oauth_refresh_token
Accept: application/json
X-UserToken: <session csrf token>
```

Response:

```json
{"result":{"token":"<refresh_token_or_empty>"}}
```

2. Try refresh grant:

```http
POST /oauth_token.do
Content-Type: application/x-www-form-urlencoded
```

Form fields:

```text
grant_type=refresh_token
refresh_token=<stored refresh token>
client_id=b77993a2359e472cad99679d1f124919
client_secret=<client secret>
```

Captured failure response:

```json
{"error_description":"access_denied","error":"server_error"}
```

3. Start PKCE authorization:

```http
GET /oauth_auth.do?response_type=code&client_id=b77993a2359e472cad99679d1f124919&state=<state>&redirect_uri=/api/sn_build_agent/build_agent_api/oauth_redirect&code_challenge=<S256 challenge>&code_challenge_method=S256
```

Captured statuses: `200` with `{"result":{"code":"<code>"}}` and browser redirect `302`.

4. Redirect endpoint:

```http
GET /api/sn_build_agent/build_agent_api/oauth_redirect?code=<code>&state=<state>
```

Response:

```json
{"result":{"code":"<code>"}}
```

5. Exchange authorization code:

```http
POST /oauth_token.do
Content-Type: application/x-www-form-urlencoded
```

Form fields:

```text
grant_type=authorization_code
code=<authorization code>
redirect_uri=%2Fapi%2Fsn_build_agent%2Fbuild_agent_api%2Foauth_redirect
client_id=b77993a2359e472cad99679d1f124919
code_verifier=<PKCE verifier>
```

Response:

```json
{
  "access_token": "<access token>",
  "refresh_token": "<refresh token>",
  "scope": "useraccount",
  "token_type": "Bearer",
  "expires_in": 1799
}
```

6. Store refresh token:

```http
POST /api/sn_build_agent/build_agent_api/oauth_store_refresh_token
Content-Type: application/json
```

```json
{"refresh_token":"<refresh token>"}
```

Response:

```json
{"result":{"success":true,"message":"Refresh token stored securely"}}
```

### App upload/install processor

Captured successful Fluent app upload does **not** use OAuth bearer auth. It uses the same-origin session token and an explicit `sysparm_ck` field.

```http
POST /sn_appclient_upload_processor.do?sysparm_track_fluent_install=true&sysparm_async_fluent_install=false&sysparm_fluent_scope_id=<app_sys_id>&sysparm_fluent_scope_name=<scope_name>&sysparm_fluent_app_version=0.0.1&sysparm_request_type=custom_app
X-UserToken: <session csrf token>
Content-Type: multipart/form-data; boundary=...
Referer: https://<instance>/scripts/raw/sn_glider_app/extensions/fluent/dist/forge-web-worker.js?...
```

Multipart fields:

```text
upload_type=file
load_demo=true
sysparm_ck=<session csrf token>
attachFile=<binary zip/app package>; filename="blob"; Content-Type=application/octet-stream
```

Response:

```json
{
  "rollbackContext": "<rollback_context_sys_id>",
  "executionTracker": "<execution_tracker_sys_id>"
}
```

Implementation recommendation: keep requiring a browser-style session token for upload unless a separate capture proves `/sn_appclient_upload_processor.do` accepts `Authorization: Bearer <oauth_access_token>` without `sysparm_ck`. The current evidence only proves the browser/session path.

## 6. Dynamic startup configuration

The Web UI performs a broader startup/config discovery pass than the CLI needs for active operations. These calls are same-origin, authenticated with `X-UserToken`, and generally have no body unless noted.

### Observed startup/config endpoints

| Order bucket | Method | Endpoint | Request body/query | Key response fields/use |
|---:|---:|---|---|---|
| IDE shell | GET | `/sn_glider_app/ide.do` | normal page query | HTML bootstraps Glider/VS Code web app. |
| Apps | GET | `/api/sn_glider/applications/all` | none | `result` array of `sys_app` records; fields include `sys_id`, `name`, `scope`, `sys_scope`, `sys_updated_on`, `private`, `source`, `sys_class_name`, etc. |
| App updates | GET | `/api/sn_glider/applications/ide/{ide_app_sys_id}/version/update-available` | none | `result` update-availability object/boolean for IDE app version. |
| Sync | POST | `/api/sn_glider/v2/sync/state` | `{"uris":["settings:/users/<user>","now-file:/<app>",...]}` | `timeStamp`, `files[]` descriptors. |
| Properties | GET | `/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/nameINsn_build_agent.use_mock_llm` | encoded query in path | `result` property rows for mock-LLM flag. |
| Variant | GET | `/api/sn_build_agent/build_agent_api/getVariant` | none | `result` variant/config value used to select Build Agent UI/runtime variant. |
| Health | GET | `/api/sn_build_agent/build_agent_health/check` | none | `result` health/check bundle; `200` in captures before enabling chat. |
| Nirvana URL | GET | `/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/name%3Dsn_build_agent.nirvana_websocket_url` | encoded query in path | `result` property row(s); value supplies websocket URL override. |
| Properties TTL | GET | `/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/name%3Dsn_build_agent.properties_cache_ttl_minutes` | encoded query in path | `result` property row(s); value controls cache TTL. |
| Provider config | GET | `/api/sn_build_agent/build_agent_api/providerConfig` | none | `result` provider config bundle. |
| Timeout/property bundle | GET | `/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/nameINsn_build_agent.tool.execution.timeouts%2Csn_build_agent.tier_override%2Csn_build_agent.user_prompt_limit%2Csn_build_agent.user_prompt_limit_pdi%2Csn_build_agent.enable_wdf_server_discovery%2Csn_build_agent.use_mock_wdf_endpoint%2Cglide.regulated_instance` | encoded `nameIN...` query | `result` property rows for timeouts, tier/user limits, WDF discovery/mock endpoint, regulated instance. |
| Timeout single | GET | `/api/sn_build_agent/build_agent_api/runQuery/table/sys_properties/query/name%3Dsn_build_agent.tool.execution.timeouts` | encoded query | `result` property row(s), used as direct timeout fallback/refresh. |
| Conversations | GET | `/api/sn_build_agent/build_agent_api/conversations` | none | `result` conversation list for sidebar/history. |
| Messages | GET | `/api/sn_build_agent/build_agent_api/conversations/{conversation_id}/messages` | none | `result` message rows; see message schema above. |
| Product availability | GET | `/api/sn_build_agent/build_agent_api/isProductAvailable` | none | `result` product availability value/object gating Build Agent. |
| Skills | GET | `/api/sn_build_agent/skills_api/summary` | none | `result` skills summary used for UI display/capability context. |
| Semantic search | GET | `/api/sn_ba_glide_tools/build_agent_glide_tools_search/getSemanticSearchStatus` | none | `result` semantic-search availability/status. Observed in `zaiagents` startup. |
| UX batch | POST | `/api/now/v1/batch` | JSON batch request | `result` batch responses for UI framework data. Not Build Agent protocol-critical. |

### Startup ordering guidance

A parity startup sequence should be safe if ordered as:

1. Load IDE shell/static assets.
2. Fetch Glider app list and IDE update availability.
3. Initialize Glider settings/workspace sync with `/api/sn_glider/v2/sync/state` and `/files`.
4. Fetch Build Agent property/config bundle: mock LLM flag, variant, health, Nirvana URL, properties TTL, provider config, timeout/tier/prompt/WDF bundle, timeout direct fallback.
5. Fetch conversations and selected conversation messages.
6. Fetch product availability, skills summary, semantic-search status if available.
7. Open Nirvana websocket using the discovered/default URL and send the observed connect capabilities.

Implementation recommendation: treat these as optional/dynamic overrides. Do not fail the CLI if one instance lacks an optional endpoint; preserve current local defaults unless the endpoint returns a valid override. Cache property bundle according to `sn_build_agent.properties_cache_ttl_minutes` when present.

## Implementation checklist

- Capture and retain `result.sysId` from every persisted user row; it is needed for checkpoint mutation.
- Persist intermediate rows as complete rows with stable client UUIDs, `startTime`, and `duration`; post each row once per logical event.
- Telemetry start must happen before turn execution; completion/abort must reuse the start telemetry `sys_id`; tool telemetry rows reference that ID in `event_id`.
- Checkpoint patch must send the full stringified user content, not a partial patch of only checkpoint fields.
- Upload parity currently requires browser/session auth: `X-UserToken` header plus multipart `sysparm_ck` field.
- Dynamic config should be additive and tolerant: read overrides when present; fall back to existing CLI defaults when absent or malformed.
