# Web UI Build / Install Parity Notes

Last analyzed: 2026-07-09
Source capture: `/tmp/lima_build.har`
Purpose: document the ServiceNow Web UI build/install/dependency flow so the Go CLI can later implement parity.

> This is analysis only. It intentionally does not change CLI behavior.

## Captured scenario

User request in the Web UI: `Please build and install the app`.

Observed app/context:

- Instance: `https://groqz.service-now.com`
- Conversation: `c4af8b902b4a07506969f1cc6e91bfc7`
- App sys_id / scope id: `1baf0f902b4a07506969f1cc6e91bf82`
- Scope name: `x_snc_lima`
- App name: `Lima`
- SDK version in IDE context: `4.8.1`
- Project root used by Web UI: `now-file:/1baf0f902b4a07506969f1cc6e91bf82`
- Key source file read by the agent: `src/fluent/x_snc_lima_cars.now.ts`

The HAR contains duplicated network entries for many requests. Treat the full-body request as canonical when a duplicate has an empty body.

## High-level flow

1. The chat message is persisted and sent over the Nirvana websocket.
2. The model/tool loop asks the client to execute Web-extension tools.
3. The `build` tool validates/install dependencies if needed, then runs `fluent.build`.
4. Dependency installation is done by the Web IDE package manager against `registry.npmjs.org` and writes `node_modules` into the Glider `now-file:` workspace.
5. The Fluent SDK web bundle is loaded from the Glider virtual module path.
6. Build generates metadata output and updates `src/fluent/generated/keys.ts` through Glider sync.
7. The model/tool loop then calls `install` with a change summary.
8. Install checks prior upgrade history and app scope, packages the built output into an app ZIP, uploads it to `sn_appclient_upload_processor.do`, then polls `api/sn_cicd/progress`.
9. Post-install hooks query installed artifacts and return user-facing links.
10. Build Agent telemetry and conversation app association are saved.

## Nirvana tool sequence

Websocket entry: `#26 GET /sncapps/code/assist/ba/nirvana/web-socket`.

Important websocket messages:

| WS msg | Direction | Time (UTC) | Meaning |
| --- | --- | --- | --- |
| `0` | send | 09:19:14.993 | `connect`, advertises client-side tools/elicitation and `tools.execute=true` |
| `2` | send | 09:19:15.130 | user message: `Please build and install the app`, includes IDE context and app scope |
| `3` | receive | 09:19:15.204 | `turn_start` |
| `7` | receive | 09:19:26.761 | client elicitation `fs_read_directory` for `.build-agent/skills` |
| `8` | receive | 09:19:26.761 | client elicitation `instance_skills_list` |
| `10` | receive | 09:19:26.761 | client elicitation `fluent_topics_list` |
| `30/31` | receive | 09:19:31.052 | tool/client call `fs_read_file src/fluent/x_snc_lima_cars.now.ts` |
| `32/33` | send/receive | 09:19:31.138 | file content returned successfully |
| `53/54` | receive | 09:19:34.429 | tool/client call `build` with `{ "path": "." }` |
| `64/65` | send/receive | 09:21:05.285 | build response: `ServiceNow application Lima built successfully!` |
| `73/76` | receive | 09:21:10.406 | tool/client call `install` with `{ "path": ".", "changeSummary": "..." }` |
| `80/81` | send/receive | 09:21:27.862 | install response: `Lima installed successfully!` plus artifact link |
| `121` | receive | 09:21:34.285 | `turn_end` |

Tool result rows persisted to the conversation:

- Build result at HAR `#738`: `toolActualName=build`, duration `90850ms`, success `true`.
- Install result at HAR `#770`: `toolActualName=install`, duration `17445ms`, success `true`.

## Build Agent extension behavior

Decoded extension source: `/tmp/ba-har-assets/333_extension_decoded.js`.

### Workspace/app resolution

`findWorkspaceFolderByAppId(appId)` constructs:

```text
now-file:/<appId>/now.config.json
```

It reads `now.config.json` through `vscode.workspace.fs`, parses it, and returns a mock workspace folder with URI `now-file:/<appId>`.

CLI implication: resolve build/install app context strictly from the active app sys_id/scope metadata, then use Glider VFS APIs to read `now.config.json`. Do not infer scope from conversation history.

### Dependency validation during build

Before `fluent.build`, the Web extension runs `validateInstalledDependencies(workspaceFolder.uri)`:

- Reads `package.json` from the app root.
- Combines `dependencies`, `devDependencies`, and `optionalDependencies`.
- For each dependency, checks whether `node_modules/<dep>` exists via `vscode.workspace.fs.stat`.
- Ignores `eslint`.
- If any dependency is missing, executes:

```text
servicenow.@servicenow/glider-extension-package-manager.installDependencies(workspaceFolder.uri, true)
```

This dependency install is automatic inside the `build` tool. It is not a separate model-requested `install_dependencies` tool in this HAR.

### Build tool

The Web extension `build` tool:

1. Requires app context.
2. Resolves app root and `now.config.json` by app id.
3. Installs missing dependencies as described above.
4. Executes:

```text
fluent.build(nowConfig.scopeId)
```

5. Returns:

```text
ServiceNow application <name-or-appId> built successfully!
```

On SDK diagnostics/errors, it formats diagnostic file/position/message details for the tool result.

### Install tool

The Web extension `install` tool:

- Requires app context.
- Source declares `requiresApproval: true`, but this HAR's Nirvana `tool_call` had `requires_approval: false`.
- Has a precondition: the last `build` tool in history must have succeeded.
- Resolves app root and `now.config.json` by app id.
- Reads `.now/.app-data.json`; if `syncNeeded === true` and `syncStatus !== 'InProgress'`, it prompts to sync/build before install.
- Executes:

```text
fluent.forceDeploy(nowConfig.scopeId)
```

- Returns JSON containing `message` and `nowConfigJson`.
- A post-execution hook then queries installed artifacts and rewrites the result into user-friendly links.

## Dependency installation observed in HAR

Registry activity starts immediately after the `build` tool begins:

- First registry request: HAR `#55`, `09:19:34.029Z`, `GET https://registry.npmjs.org/@servicenow/sdk`
- Last registry request: HAR `#717`, `09:20:51.768Z`, tarball `define-data-property-1.1.4.tgz`
- Total registry requests: `657`
- Unique metadata package paths: `310`
- Tarball requests: `51`

Important package metadata paths observed:

- `/@servicenow/sdk`
- `/@servicenow/sdk-api`
- `/@servicenow/sdk-cli`
- `/@servicenow/sdk-core`
- `/@servicenow/sdk-build-core`
- `/@servicenow/sdk-build-plugins`
- `/@servicenow/isomorphic-rollup`
- `/@servicenow/sdk-repack`
- `/@servicenow/glide`

After registry fetches complete, the Web IDE loads the SDK web build from the app workspace:

```text
#724 09:20:59.385Z GET /sn_glider_app/virtual-module:/1baf0f902b4a07506969f1cc6e91bf82/node_modules/@servicenow/sdk/dist/web/index.js
```

This strongly indicates dependency installation materializes `node_modules` in the Glider-backed `now-file:` workspace, and subsequent Web Fluent commands import SDK code from that virtual module path.

## Low-level SDK build behavior

Decoded SDK web source: `/tmp/sdk-web-index.js`.

The SDK `Orchestrator.build()` flow observed in source:

1. Start telemetry timer `build`.
2. Unless `skipClean`, delete build output directories:
   - repack output dir
   - app output dir
   - package output dir
   - static content dir
3. Run `uiPrebuild`.
4. Save output from Fluent project (`project.saveOutput(...)`).
5. Reload plugins.
6. If `frozenKeys`, verify `keys.ts` did not change.
7. Save the generated keys source file.
8. Run `uiPostbuild`.
9. Return `{ success: true, files, packOutput, diagnostics: [] }`.
10. Send build telemetry.

Build UX metric at HAR `#742`:

```text
Fluent build attempted:  scopeId=1baf0f902b4a07506969f1cc6e91bf82, source=forge-web
Fluent build successful: scopeId=1baf0f902b4a07506969f1cc6e91bf82, source=forge-web, duration=12083ms
```

### Generated keys write

Near the end of build, the Web UI used Glider sync to inspect workspaces and update the generated keys file:

```text
#730 POST /api/sn_glider/v2/sync/state
     body uris includes now-file:/1baf0f902b4a07506969f1cc6e91bf82

#732 POST /api/sn_glider/v2/sync/files
     body: []

#740 POST /api/sn_glider/v2/sync/changes/apply
     create: []
     update: [{ uri: now-file:/1baf.../src/fluent/generated/keys.ts, checksum: 9559df..., size: 8141, ... }]
     remove: []
     file part: generated keys.ts content
```

`keys.ts` after build contained generated registry entries for the table, dictionary, documentation, choices, number record, and modules for `x_snc_lima_cars`.

CLI implication: after a local/SDK build, persist changed generated files back into Glider VFS. At minimum this includes `src/fluent/generated/keys.ts`; do not leave generated build state only in a temp local directory.

## Install / deploy observed in HAR

The model called `install` only after build succeeded.

### Pre-upload checks

Before upload, SDK/install code checked previous upgrade history and app scope:

```text
#750 GET /api/now/table/sys_upgrade_history
     sysparm_query=to_version=x_snc_lima^ORDERBYDESCupgrade_started
     sysparm_fields=upgrade_finished,sys_id
     sysparm_limit=1
     response: {"result":[]}

#752 GET /api/now/table/sys_scope
     sysparm_query=scope=x_snc_lima^sys_id=1baf0f902b4a07506969f1cc6e91bf82
     sysparm_fields=sys_id,sys_class_name,active,scope,name,short_description
     sysparm_limit=2
     response: sys_app Lima, active=true
```

### Package contents

The upload package is a ZIP generated from SDK build output. The HAR body was binary, but filenames were visible in the multipart text/central directory:

```text
/package_inventory.csv
/update/sys_module_d5df0f655f224c64a65136836c857f1c.xml
/update/sys_choice_x_snc_lima_cars_status.xml
/update/sys_number_7194234f479c480586fa5563682eb59b.xml
/update/sys_dictionary_x_snc_lima_cars_make.xml
/update/sys_dictionary_x_snc_lima_cars_model.xml
/update/sys_dictionary_x_snc_lima_cars_year.xml
/update/sys_dictionary_x_snc_lima_cars_color.xml
/update/sys_dictionary_x_snc_lima_cars_vin.xml
/update/sys_dictionary_x_snc_lima_cars_mileage.xml
/update/sys_dictionary_x_snc_lima_cars_status.xml
/update/sys_dictionary_x_snc_lima_cars_null.xml
/update/sys_db_object_14976bec26b143409e90d913804acb77.xml
/update/sys_module_1e7bd7ac03304e76881459caa696bc70.xml
/dictionary/x_snc_lima_cars.xml
/scope/sys_app_1baf0f902b4a07506969f1cc6e91bf82.xml
```

The SDK pack flow writes `package_inventory.csv` unless `skipPackageInventory` is set, using:

- build identifier: scope name for scoped apps, scope id for global
- type: `scoped` unless global
- app version from `package.json`
- package inventory version: `1.0.0`

### Upload endpoint

Canonical upload request:

```text
#754 POST /sn_appclient_upload_processor.do
```

Query string:

```text
sysparm_track_fluent_install=true
sysparm_async_fluent_install=false
sysparm_fluent_scope_id=1baf0f902b4a07506969f1cc6e91bf82
sysparm_fluent_scope_name=x_snc_lima
sysparm_fluent_app_version=0.0.1
sysparm_request_type=custom_app
```

Multipart form fields:

```text
upload_type = file
load_demo = true
sysparm_ck = <session csrf token>
attachFile = <zip blob>
```

Response:

```json
{
  "rollbackContext": "5cb35b1c2b4a07506969f1cc6e91bff6",
  "executionTracker": "58b35b1c2b4a07506969f1cc6e91bff4"
}
```

### Progress polling

The SDK polls the tracker returned by upload:

```text
#756 GET /api/sn_cicd/progress/58b35b1c2b4a07506969f1cc6e91bff4
```

Observed response:

```json
{
  "result": {
    "status": "2",
    "status_label": "Successful",
    "status_message": "Application installed successfully for Lima",
    "status_detail": "RollBack Context Id: 5cb35b1c2b4a07506969f1cc6e91bff6",
    "error": "",
    "percent_complete": 100
  }
}
```

SDK source behavior:

- If an `executionTracker` is returned, poll `/api/sn_cicd/progress/<trackerId>` until status is `ERROR` or `SUCCESSFUL`.
- Poll interval in source: `1s`.
- Every 30 loops, log `App install pending...`.
- On success, log rollback URL.
- On failure, throw status message.
- If no tracker is returned, fall back to polling `sys_upgrade_history` for up to 30 seconds and compare against the pre-install history id.

### Deploy/install telemetry

Observed UX metrics:

```text
#758 Fluent deploy attempted
     scopeId=1baf0f902b4a07506969f1cc6e91bf82
     source=forge-web
     forceDeploy=true
     reinstall=false

#763 install
     version=4.8.1
     clientName=IDE
     ideVersion=4.3.2
     installed=true
     isStoreAppInstall=false
     durationMs=10189

#782 Fluent deploy successful
     duration=10456
     forceDeploy=true
     reinstall=false
```

### Post-install artifact discovery

After successful install, the Build Agent post-execution hook queried artifacts:

```text
#766 GET /api/sn_build_agent/build_agent_api/runQuery/table/sys_ui_page/query/sys_scope=1baf0f902b4a07506969f1cc6e91bf82
     response: no matching records

#767 GET /api/sn_build_agent/build_agent_api/runQuery/table/sys_db_object/query/sys_scope=1baf0f902b4a07506969f1cc6e91bf82
     response: table x_snc_lima_cars, label Cars
```

It converted the table artifact into this user link:

```text
https://groqz.service-now.com/x_snc_lima_cars_list.do?sysparm_clear_stack=true
```

Final assistant message at HAR `#774` reported the app built and installed successfully and included the Cars list link.

## Conversation/telemetry persistence

At request start:

```text
#009/#010 POST /api/sn_build_agent/build_agent_api/telemetry
     request="Please build and install the app"
     status=in-progress

#020/#021 POST /api/sn_build_agent/build_agent_api/conversations/<conversation>/messages
     user message persisted
```

At completion:

```text
#776/#777 POST /api/sn_build_agent/build_agent_api/telemetry
     status=complete
     end_time=2026-07-09 09:21:33

#778/#779 PATCH /api/sn_build_agent/build_agent_api/conversations/<conversation>
     { "applicationId": "1baf0f902b4a07506969f1cc6e91bf82", "applicationName": "" }

#780/#781 POST /api/sn_build_agent/build_agent_api/telemetry
     event telemetry for fs_read_directory, fs_read_file, build, install
```

CLI implication: the Go CLI already sends websocket tool responses, but parity should preserve tool status, durations, success/failure, and active application association when the backend expects it.

## CLI parity implementation plan

### 1. Add client-side tool handlers

Implement handlers for at least:

- `build`
- `install`
- optionally explicit `install_dependencies`

These are client-side elicitations from Nirvana, not normal backend tools. They should return Web-extension-compatible response shapes:

```json
{
  "success": true,
  "content": "ServiceNow application Lima built successfully!",
  "ideContext": { "...": "updated active app context" }
}
```

Failures should be concise but specific and should print a terminal summary because Antonio wants all tool executions visible in terminal.

### 2. Resolve app context from active app

For every build/install handler:

1. Resolve `appId` from payload, current app state, or `appScope.scopeId`.
2. Resolve actual scope/name from active app metadata (`sys_scope` / current `AppScope.Scope`), not conversation text.
3. Read `now-file:/<appId>/now.config.json` through Glider VFS.
4. Treat missing `now.config.json` as a project resolution error.

### 3. Dependency strategy

Web UI installs dependencies into Glider VFS using the Glider package-manager extension. Go CLI has no browser package manager, so choose one of these implementation approaches:

#### Recommended v1: local temp project + Node/npm + SDK

1. Sync the Glider app root to a temp directory.
2. Read `package.json`.
3. Check local `node_modules` in the temp project.
4. If missing dependencies, run package install locally (`npm install`, `npm ci`, or package-manager-compatible install).
5. Use the installed app-local `@servicenow/sdk` to build/package.
6. Persist generated source changes that matter to Glider (`src/fluent/generated/keys.ts`, possibly package lock if intentionally changed).
7. Use the local package artifact for upload.

Pros: implementable in Go by shelling out to Node/npm; no need to emulate browser package manager internals.
Cons: requires local `node`/`npm`; may differ from Web package-manager lockfile behavior; does not put full `node_modules` into Glider unless explicitly synced back.

If `node`, `npm`, or the SDK build command is unavailable, print a terminal-visible error (same style as `run_diagnostics` missing `tsc`).

#### Strict Web parity: install dependencies into Glider VFS

Emulate the package-manager extension closely enough to materialize `node_modules` under `now-file:/<appId>/node_modules`.

Pros: matches Web UI and makes `fluent_topics_list` docs available from Glider.
Cons: large implementation; requires npm resolution/tarball extraction/writes via Glider; can be slow and heavy.

A hybrid is acceptable: local npm cache for build, plus selective SDK docs sync/cache for `fluent_topics_list`.

### 4. Build implementation

Minimum viable build parity:

1. Validate dependencies from `package.json` like Web UI:
   - dependencies + devDependencies + optionalDependencies
   - ignore `eslint`
2. Install missing dependencies using chosen strategy.
3. Run SDK build equivalent. Candidate commands/APIs to verify during implementation:
   - app-local `node_modules/.bin/now-sdk build`
   - app-local `@servicenow/sdk` programmatic API / CLI entrypoint
4. Capture diagnostics and format them like Web extension:
   - header: `<error.name>: <error.message>`
   - for each diagnostic: file, position, message
5. Persist generated `keys.ts` back to Glider with `/api/sn_glider/v2/sync/changes/apply`.
   - Send `create`, `update`, and `remove` as JSON arrays, never `null`.
6. Record enough in memory/state to let `install` know there was a successful build and where the package output/artifact is.
7. Return `ServiceNow application <name> built successfully!` on success.

### 5. Install implementation

Minimum viable install parity:

1. Enforce successful build precondition when install is called by the model.
2. Read `.now/.app-data.json` from Glider/temp project.
   - If metadata sync is required, initially fail with a clear message or ask for confirmation.
   - Later parity can implement `fluent.sync` + rebuild.
3. Ensure a package ZIP exists.
   - If build produced one, use it.
   - Otherwise run SDK pack/build as needed.
4. Query prior install status:

```text
GET /api/now/table/sys_upgrade_history?sysparm_query=to_version=<scope>^ORDERBYDESCupgrade_started&sysparm_fields=upgrade_finished,sys_id&sysparm_limit=1
```

5. Query scope info:

```text
GET /api/now/table/sys_scope?sysparm_query=scope=<scope>^sys_id=<appId>&sysparm_fields=sys_id,sys_class_name,active,scope,name,short_description&sysparm_limit=2
```

6. Upload package:

```text
POST /sn_appclient_upload_processor.do?sysparm_track_fluent_install=true&sysparm_async_fluent_install=false&sysparm_fluent_scope_id=<appId>&sysparm_fluent_scope_name=<scope>&sysparm_fluent_app_version=<version>&sysparm_request_type=custom_app
```

Multipart fields:

```text
upload_type=file
load_demo=true
sysparm_ck=<csrf/session token>
attachFile=<zip>
```

7. Parse `executionTracker` and `rollbackContext`.
8. Poll `/api/sn_cicd/progress/<tracker>` until success/error.
9. Query artifacts with Build Agent `runQuery` endpoint:
   - `sys_ui_page` by `sys_scope=<appId>`
   - `sys_db_object` by `sys_scope=<appId>`
10. Format result links exactly like Web extension:
   - UI page: `<base>/<endpoint>`
   - table: `<base>/<table_name>_list.do?sysparm_clear_stack=true`
11. Return `Lima installed successfully!` plus links.

### 6. Auth and CSRF caution

The upload endpoint is not a normal `/api/...` endpoint. Web UI includes `sysparm_ck` and runs under browser session credentials.

Implementation must verify whether `sn_appclient_upload_processor.do` accepts the CLI's existing OAuth bearer token. If not, the CLI needs a way to acquire/session a valid `sysparm_ck` and cookies, or a supported REST equivalent for scoped app upload.

Do not assume `/api/now/table` OAuth behavior means the upload processor will work with OAuth only.

### 7. Terminal/UI behavior

When these handlers run in the CLI:

- Show `Building...` during long build/install activity.
- Print minimal terminal descriptions for dependency install, build, package upload, progress polling, and artifact discovery.
- On missing local tools (`node`, `npm`, `now-sdk`, etc.), print a clear terminal line before returning failure.
- Keep descriptions in the neutral row under the green/red tool name, matching current terminal result conventions.

## Known gaps / decisions for implementation time

- Whether to sync full `node_modules` into Glider VFS or keep dependencies local and cache SDK docs separately.
- Exact local SDK invocation (`now-sdk build`, package script, or programmatic API) needs verification against an app-local install.
- Upload processor authentication/CSRF must be proven from the Go CLI, not assumed.
- Metadata sync (`fluent.sync`) is only documented here; initial CLI parity can detect and fail/ask rather than implement full sync.
- HAR shows duplicate request records; implementation should follow SDK/source behavior, not duplicate requests.

## Quick endpoint checklist

Build/dependencies:

- `GET https://registry.npmjs.org/<package>`
- `GET https://registry.npmjs.org/<package>/-/<tarball>.tgz`
- `GET /sn_glider_app/virtual-module:/<appId>/node_modules/@servicenow/sdk/dist/web/index.js`
- `POST /api/sn_glider/v2/sync/state`
- `POST /api/sn_glider/v2/sync/files`
- `POST /api/sn_glider/v2/sync/changes/apply`

Install:

- `GET /api/now/table/sys_upgrade_history?...`
- `GET /api/now/table/sys_scope?...`
- `POST /sn_appclient_upload_processor.do?...`
- `GET /api/sn_cicd/progress/<executionTracker>`
- `GET /api/sn_build_agent/build_agent_api/runQuery/table/sys_ui_page/query/sys_scope=<appId>`
- `GET /api/sn_build_agent/build_agent_api/runQuery/table/sys_db_object/query/sys_scope=<appId>`

Conversation/telemetry:

- `POST /api/sn_build_agent/build_agent_api/conversations/<id>/messages`
- `PATCH /api/sn_build_agent/build_agent_api/conversations/<id>`
- `POST /api/sn_build_agent/build_agent_api/telemetry`
