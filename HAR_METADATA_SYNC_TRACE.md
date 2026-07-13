# HAR trace: Web UI metadata sync before build/install

Analyzed: 2026-07-13 09:04 CEST

## Scope

Seven local captures were inspected without changing source:

- `/tmp/New_conversation.har` (1.6 MB)
- `/tmp/demoalectriallwfze140800.service-now.com.har` (106 MB)
- `/tmp/lima_build.har` (184 MB)
- `/tmp/myapp_full.har` (352 MB)
- `/tmp/stop.har` (560 KB)
- `/tmp/workspace_demoalectriallwfze140800.service-now.com.har` (140 MB)
- `/tmp/zaiagents.service-now.com.har` (184 MB)

The trace covered request URLs/bodies, response bodies, captured WebSocket frames, Glider sync payloads, downloaded Fluent/Build Agent extension bundles, and ordering around recorded Build/Install tool results.

## Conclusion

**Metadata sync is orchestrated client-side by the Web UI extensions. The actual Now SDK sync operation runs in the Fluent extension's `forge-web-worker.js` (SDK/Forge web worker). It is not a dedicated Build Agent REST tool invocation.**

REST is still involved in supporting roles:

1. The Fluent extension checks whether instance metadata is newer through Glider REST (`/api/sn_glider/sync/get_latest_updated_metadata` or `/api/sn_glider/sync/metadata/{scopeId}`).
2. The SDK worker necessarily makes the instance requests required by the SDK sync, although the captures do not contain a clean, isolated user-triggered `fluent.sync` transaction from start to finish.
3. Glider file-system transport persists local workspace changes, including `.now/.app-data.json`, through `/api/sn_glider/v2/sync/state`, `/sync/files`, and `/sync/changes/apply`.
4. Build Agent conversation REST only records/streams agent messages and Build/Install tool results. No `sync` Build Agent tool result or Build Agent sync endpoint appears in the seven captures.

Therefore the closest parity architecture is: **local pre-install orchestration -> execute Fluent/SDK sync semantics -> update `.app-data.json` -> build -> force deploy/install**. Merely clearing `syncNeeded` would skip the SDK metadata pull and is not parity.

## Recovered control flow

### 1. Detecting that sync is needed

The downloaded Fluent extension initializes app data as:

```text
{ lastSync: 0, syncBy: "", syncStatus: "never_ran", syncNeeded: false }
```

It marks `syncNeeded = true` from client-side metadata listeners and from REST timestamp comparison. The bundle contains both detection paths:

- AMB/BroadcastChannel listeners for scope metadata/customer update topics mark the local app data dirty.
- For configuration apps it fetches tracked record IDs in the SDK worker, then calls:

```text
GET /api/sn_glider/sync/get_latest_updated_metadata?sys_ids=...&scope_id=...
```

- A fallback/other path calls:

```text
GET /api/sn_glider/sync/metadata/{scopeId}
```

and compares the response `sys_updated_on` with `.app-data.json.lastSync`.

Concrete HAR evidence: `zaiagents.service-now.com.har` entries 374 and 376 are successful `GET /api/sn_glider/sync/metadata/{scopeId}` responses containing metadata identity plus numeric `sys_updated_on` values.

### 2. Build Agent install guard and orchestration

The downloaded Build Agent extension reads `.now/.app-data.json` through `workspace.fs` and evaluates:

```text
appData.syncNeeded === true && appData.syncStatus !== 'InProgress'
```

If true, its install tool performs this sequence:

1. Prompt **Sync & Build** / Cancel.
2. Execute VS Code command `fluent.sync(scopeId)`.
3. Re-read `.now/.app-data.json`; if it still needs sync, cancel install.
4. Execute `fluent.build(scopeId)`.
5. Execute `fluent.forceDeploy(scopeId)` to avoid Fluent deploy's duplicate sync check.

This code is present in Build Agent extension responses extracted from multiple captures (`demoalectriallwfze...`, `workspace_demoalectriallwfze...`, and `zaiagents...`), so it is not inferred only from UI labels.

### 3. What `fluent.sync` actually invokes

The Fluent extension registers `fluent.sync` to its sync controller. Before SDK sync it:

1. Reads `.now/.app-data.json`.
2. Writes `syncStatus = "in_progress"` and `syncBy = current user`.
3. Waits for the Synchrotron file-sync process to complete.
4. Calls `executeSyncUsingForgeWeb(...)`.
5. Calls `this.service.sync({ uri, userToken, incrementalSync, scopeId, lastPull, syncSpecificFiles })`.

`this.service` is a client for a browser module worker. The same bundle constructs:

```text
new Worker(<extension>/dist/forge-web-worker.js, { type: "module" })
```

and maps `service.sync(...)` to worker operation `forge/sync`. The worker is described in the bundle as the **Forge Web Worker**, while the UI labels the service **Now SDK Build Service**. This is direct evidence that the substantive SDK sync executes client-side in a worker, not inside the Build Agent REST API.

On success Fluent re-reads app data and writes:

```text
syncStatus = "completed"
lastSync = Date.now()
syncNeeded = false
```

On failure it writes:

```text
syncStatus = "failed"
syncNeeded = true
lastSync = 0
```

### 4. Local sync-state persistence is separate from metadata sync

The HARs repeatedly show Glider file transport:

```text
POST /api/sn_glider/v2/sync/state
POST /api/sn_glider/v2/sync/files
POST /api/sn_glider/v2/sync/changes/apply
```

These payloads carry workspace file state/deltas. They are how local files and `.now/.app-data.json` state cross the browser/server workspace boundary; they are **not** the metadata conversion/pull operation itself.

Examples:

- `myapp_full.har` entry 95 `/sync/changes/apply` includes `.now/.app-data.json` in the synchronized file tree. The app-data content visible in the capture is `lastSync: 0`, empty `syncBy`, `syncStatus: "never_ran"`, `syncNeeded: false`.
- `lima_build.har` entries 730-733 are `/sync/state` then `/sync/files`; entries 740-741 apply the resulting file changes.
- Raw literal searches found `.app-data.json` in four of seven captures and `syncNeeded` in the extension-bearing/full-workspace captures. No request body exposes a standalone REST operation equivalent to `fluent.sync`.

This separation matters: rewriting the state file can satisfy the guard but cannot reproduce the SDK worker's metadata download/transform behavior.

## Ordering around captured build/install

`lima_build.har` provides the clearest successful ordering:

1. Build Agent Build tool starts at `2026-07-09T09:19:33.749Z`.
2. Browser downloads/uses the virtual `@servicenow/sdk/dist/web/index.js` module during the build window.
3. Glider file sync occurs at `09:21:04.500Z` (`/sync/state`), `09:21:05.013Z` (`/sync/files`), and `09:21:05.174Z` (`/sync/changes/apply`).
4. Build tool result is posted to the Build Agent conversation at `09:21:05.146Z`, success, duration 90,850 ms.
5. Install tool starts at `09:21:09.729Z`.
6. Install tool result is posted at `09:21:27.730Z`, success, duration 17,445 ms.

No `fluent.sync` tool message appears between Build and Install in that capture. That is consistent with install finding no pending metadata sync (or sync having been resolved before the captured interval), not with Build Agent REST performing a hidden sync tool call.

The Build Agent REST traffic around these events is limited to conversation message persistence (`/api/sn_build_agent/build_agent_api/conversations/.../messages`) and telemetry. The posted tool records are explicitly `toolActualName: "build"` and `toolActualName: "install"`; there is no `toolActualName: "sync"` in the seven captures.

## WebSocket findings

WebSocket frames in the HARs are Build Agent/LLM streaming and UI control traffic (assistant deltas, stop-generation/control events, etc.). Searches of captured frames found no metadata file payload, `fluent.sync` command, `syncNeeded` transition, or SDK `forge/sync` operation.

The Fluent metadata-change listener uses an in-browser `BroadcastChannel` bridged to AMB topics. That internal extension-host traffic is not represented as a distinct metadata-sync WebSocket transaction in these HARs.

## REST vs worker vs Build Agent decision table

| Responsibility | Location demonstrated by evidence |
|---|---|
| Read/write `.now/.app-data.json` | Client extension via VS Code `workspace.fs`; persisted by Glider `/v2/sync/*` file transport |
| Decide `syncNeeded` | Fluent extension client logic using AMB events and Glider metadata timestamp REST |
| Prompt and order sync -> build -> install | Build Agent extension client-side install tool |
| Perform metadata SDK sync | Fluent `forge-web-worker.js`, operation `forge/sync` |
| Perform build | Same Fluent SDK worker/service (`fluent.build`) |
| Perform final install | Fluent command `fluent.forceDeploy` after the guard is satisfied |
| Record agent tool results | Build Agent conversation REST |

## Confidence and capture limitation

Confidence is **high** for component ownership and ordering because the downloaded production extension code explicitly names each command, state transition, worker operation, and install sequence. Confidence is **moderate** for the exact low-level HTTP calls emitted inside a live `forge/sync`, because none of the seven HARs contains an unambiguous complete user-triggered metadata-sync run; the strongest runtime REST evidence is sync-needed detection and Glider workspace-file persistence.
