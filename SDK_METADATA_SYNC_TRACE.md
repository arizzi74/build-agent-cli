# ServiceNow SDK metadata synchronization trace

Investigated locally on 2026-07-13. Installed SDK family: `@servicenow/sdk`, `@servicenow/sdk-cli`, `@servicenow/sdk-api`, and `@servicenow/sdk-build-core` version **4.6.1**. No network requests or source modifications were made during the trace.

## Executive finding

The operation that legitimately clears `.now/.app-data.json.syncNeeded` is the Fluent IDE command **`fluent.sync`** (or the destructive full variant **`fluent.syncAllMetadata`**), whose worker operation is **`forge/sync`**.

Its native SDK equivalent is not a command named `sync`. The installed CLI exposes it as:

```text
now-sdk transform --source <project> --auth <alias> --incremental
```

and the installed programmatic API is:

```ts
await orchestrator.transform({ method: 'incremental', lastPull })
```

For a full download/transform, use `orchestrator.transform({ method: 'complete' })`; that corresponds to the IDE's forced-all-metadata intent and must be treated as potentially overwriting local Fluent changes.

**Critical distinction:** `now-sdk download` only downloads and unpacks instance XML into a requested directory. It does not transform/save the project and therefore is not the operation to use for clearing the guard.

The SDK transform itself does **not** know about `.now/.app-data.json`; the Fluent extension wraps a successful `forge/sync` and then writes the bookkeeping fields. A headless caller must perform that final state update itself, but only after a successful SDK transform.

## Exact IDE operation

Local extracted Fluent extension code identifies these command IDs:

- `fluent.sync`
- `fluent.syncAllMetadata`
- `fluent.build`
- `fluent.deploy`
- `fluent.buildAndDeploy`
- `fluent.forceDeploy`

`fluent.sync` is registered to the sync controller. The controller:

1. rejects dirty editors;
2. reads or creates `<project>/.now/.app-data.json`;
3. refuses a duplicate run when `syncStatus === "in_progress"`;
4. writes:
   - `syncStatus: "in_progress"`
   - `syncBy: <current user>`;
5. waits for the Synchrotron extension's active process to complete;
6. posts worker operation `forge/sync` with:

```js
{
  uri: projectUri,
  userToken: g_ck,
  incrementalSync: <IDE setting>,
  scopeId,
  lastPull: <lastSync formatted UTC as YYYY-MM-DD HH:mm:ss>,
  syncSpecificFiles: []
}
```

For `fluent.syncAllMetadata`, `lastPull` is omitted and incremental mode is disabled after a destructive-overwrite warning.

On success, and only on success, the controller rereads `.app-data.json` and writes:

```json
{
  "syncStatus": "completed",
  "lastSync": 1783926386000,
  "syncNeeded": false
}
```

(the number above is illustrative; the implementation uses `Date.now()`). It also removes the in-memory pending-sync marker for the scope. In `finally`, the JSON is persisted.

On failure it writes:

```json
{
  "syncStatus": "failed",
  "syncNeeded": true,
  "lastSync": 0
}
```

The default schema used when the file is missing or invalid is:

```json
{
  "lastSync": 0,
  "syncBy": "",
  "syncStatus": "never_ran",
  "syncNeeded": false
}
```

The IDE's normal deploy path checks this state before build/install. If sync is needed, it runs **sync, then build**, before deploy. Its build command writes `.now/.build-stats.json` with a timestamp; deploy considers the build stale when build time is older than `lastSync`.

## Native SDK/CLI implementation behind sync

The installed `@servicenow/sdk-api` implementation supplies the headless equivalent through `Orchestrator.transform`.

For an application project:

```ts
await orchestrator.transform({ method: 'incremental', lastPull })
```

performs this sequence:

1. `Connector.download(scopeId, options)` sends:

```text
GET /api/fluent/download/<scopeId>
    ?sysparam_mode=incremental
    &sysparam_timestamp=<lastPull>   # when supplied
```

For complete mode it sends `sysparam_mode=complete`.

2. The response ZIP is written under a temporary ServiceNow download directory.
3. `Orchestrator.unpack` expands the ZIP into a temporary transform directory.
4. `Project.transform(files, options)` converts incoming instance XML into Fluent/project sources and resolves records against the existing project.
5. Temporary ZIP and extracted metadata are removed.
6. `Project.save()` saves every changed project file and removes files queued for deletion.
7. `TransformResult` returns:
   - `changedFiles`
   - `handledPaths`.

The CLI command that drives this is `now-sdk transform`, not `now-sdk download`. The transform handler constructs `NowConfig`, `Project`, and `Orchestrator(project, credential)`, then calls `orchestrator.transform(...)`.

The SDK package itself contains no `.app-data.json` or `syncNeeded` reference. Those fields belong to the IDE extension's safety/bookkeeping layer. Consequently, after a successful CLI/API transform, a headless integration must atomically update the app-data file to mirror the extension's success transition.

## Authentication and configuration requirements

### Project configuration

The project directory must pass `validateWorkingDir` and contain parseable:

- `package.json`
- `now.config.json`
- installed/pinned project SDK dependencies expected by the project.

`now.config.json.scopeId` is used as the `/api/fluent/download/<scopeId>` identifier. Normal application projects may use complete or incremental transform. Configuration projects require explicit record IDs/table or an update set; the application-wide download branch rejects them.

### CLI authentication

`credentialProvider` supports either:

1. a stored keychain credential, selected by `--auth <alias>` or the configured default (`now-sdk auth --add ...`, then optionally `now-sdk auth --use ...`); or
2. CI basic credentials when:

```text
SN_SDK_NODE_ENV=SN_SDK_CI_INSTALL
SN_SDK_INSTANCE_URL=https://<instance>
SN_SDK_USER=<user>
SN_SDK_USER_PWD=<password>
```

Stored credentials may be basic or OAuth. Basic auth is resolved to a UI session containing both a cookie and user token; OAuth resolves to a bearer token. `Connector` obtains request headers from the resulting SDK credential.

### IDE authentication

The IDE worker is given the browser/UI anti-CSRF token `g_ck` as `userToken`, and the IDE request client also carries the current session/transaction context. Build Agent Go CLI should not attempt to synthesize `g_ck`; it should use `credentialProvider` via `now-sdk transform`, or construct the same SDK `LazyCredential`/`Orchestrator` stack.

The instance user must be permitted to call the Fluent download API and read the app metadata. Exact role names are not encoded in the installed client source, so they cannot be stated from local evidence alone.

## Files and state mutated

A successful metadata transform can mutate materially more than the marker file:

- Fluent generated/source files selected by SDK transform plugins, commonly under the configured Fluent source/generated directories;
- metadata XML files under the configured metadata directory when records cannot/should not be represented as Fluent;
- the Fluent keys/sys-id mapping when transform introduces or resolves records;
- hosted-plugin source areas when configured;
- deletions of local metadata paths marked handled/replaced by transform;
- temporary download ZIP/extraction directories (created and then removed);
- finally, by the wrapper, `.now/.app-data.json`.

The IDE wrapper may also initialize Git or stage local changes before sync, and it coordinates with Synchrotron. Those are IDE safeguards, not requirements of `Orchestrator.transform`.

Do **not** represent the operation as merely editing `.app-data.json`. Clearing `syncNeeded` without transforming the downloaded metadata bypasses the safety check and can deploy stale metadata.

## Required ordering relative to build, pack, and install

Safe order:

```text
read app-data / decide whether sync is required
  -> metadata transform/sync
  -> mark completed + syncNeeded=false + lastSync=now
  -> build
  -> pack
  -> install
```

Why:

- `now-sdk build` compiles current local source into the app output. It does not pull instance metadata.
- `now-sdk pack` only zips the already-built app output. It does not build or sync.
- `now-sdk install` constructs `Project`/`Orchestrator` and installs; it does not invoke transform or build first. If no package path is supplied internally, SDK install packs current build output, so stale build output remains stale.
- The IDE explicitly tells the user to build after sync, and its timestamp check requires build time to be newer than `lastSync`.

If Build Agent invokes separate CLI processes, use:

```sh
now-sdk transform --source "$PROJECT" --auth "$ALIAS" --incremental
now-sdk build "$PROJECT"
now-sdk pack "$PROJECT"
now-sdk install --source "$PROJECT" --auth "$ALIAS"
```

Pass the transform's last-pull timestamp programmatically if exact IDE incremental semantics are required; the installed CLI's `--incremental` switch does not expose `lastPull`, so it asks the instance for incremental mode without the stored timestamp.

## Recommended Build Agent Go CLI integration

### Recommended: small Node helper using installed SDK API

Invoke a checked-in or generated Node helper from Go. The helper should:

1. validate/lock one project root;
2. read `.now/.app-data.json` without changing it;
3. load `NowConfig`, `Project`, SDK logger, and `credentialProvider` from the installed 4.6.1 packages;
4. format `lastSync` as UTC `YYYY-MM-DD HH:mm:ss`;
5. call:

```ts
const result = await orchestrator.transform({
  method: incremental ? 'incremental' : 'complete',
  ...(incremental && lastPull ? { lastPull } : {}),
})
```

6. only after the Promise resolves, atomically replace `.now/.app-data.json` with preserved unknown fields plus:

```json
{
  "syncStatus": "completed",
  "syncNeeded": false,
  "lastSync": 1783926386000
}
```

7. run SDK build; only proceed to pack/install if build succeeds.

This is preferable to reverse-engineering the web worker. It uses the same installed SDK `Connector.download -> unpack -> Project.transform -> Project.save` path and supports the exact `lastPull` value that the CLI flag omits.

### Acceptable simpler integration: spawn installed CLI

Build Agent can spawn the project-local binary (not an arbitrary PATH binary):

```text
<workspace>/node_modules/.bin/now-sdk transform \
  --source <project> --auth <alias> --incremental
```

Then it must update app-data atomically, build, pack, and install. Pin/verify the SDK version first. This is simpler but does not pass `lastPull` through the current CLI surface.

### Direct HTTP reproduction: not recommended

A Go-native implementation could call `/api/fluent/download/<scopeId>` with SDK-equivalent auth, unzip the archive, and then still invoke the SDK transform engine over those XML files. Reimplementing the transform engine in Go is unsafe: plugin-specific record conversion, key handling, conflict/deletion semantics, hosted plugins, and formatting all live in the SDK.

Never call the endpoint and clear the bit without `Project.transform` + `Project.save` succeeding.

## Transaction and failure rules for Build Agent

- Serialize sync/build/install per project root.
- Snapshot/hash `.now/.app-data.json` before work and preserve unknown keys.
- Do not clear the marker before transform success.
- On transform failure, preserve `syncNeeded: true`; set `syncStatus: "failed"` and `lastSync: 0` only if intentionally mirroring IDE behavior.
- Write app-data through temp-file + fsync + rename; avoid a torn JSON file.
- Treat a subsequent build failure as **not installable**, even though synchronization succeeded and the marker is now legitimately clear.
- Do not silently fall back from incremental to complete; complete sync can overwrite local changes.
- Before complete/full sync, require explicit confirmation or a clean/staged worktree snapshot.
- Capture changed/deleted paths from `TransformResult` or Git diff for audit.
- Verify after build that output is newer than the successful sync timestamp before pack/install.
- Avoid `fluent.forceDeploy`: it intentionally permits bypassing the sync/build warnings and is not a synchronization operation.

## Local evidence map

Primary installed-source evidence:

- `node_modules/@servicenow/sdk-cli/src/command/transform/index.ts`
- `node_modules/@servicenow/sdk-cli/src/command/download/index.ts`
- `node_modules/@servicenow/sdk-cli/src/command/build/index.ts`
- `node_modules/@servicenow/sdk-cli/src/command/pack/index.ts`
- `node_modules/@servicenow/sdk-cli/src/command/install/index.ts`
- `node_modules/@servicenow/sdk-cli/src/auth/index.ts`
- `node_modules/@servicenow/sdk-api/dist/orchestrator.js` and `.d.ts`
- `node_modules/@servicenow/sdk-api/dist/connector.js`
- `node_modules/@servicenow/sdk-api/dist/project.js`
- `node_modules/@servicenow/sdk/dist/web/index.js`

Locally preserved/extracted IDE bundle evidence:

- `/tmp/fluent-extension.js`

That bundle provides the command IDs, `forge/sync` message schema, `.app-data.json` transitions, and sync-before-build/deploy ordering. The installed SDK source provides the exact native transform/download/save implementation.

## Bottom line

The guard-clearing operation is **not** “set `syncNeeded` to false.” It is:

```text
fluent.sync
  -> forge/sync
  -> SDK incremental download + transform + project.save
  -> successful app-data transition
  -> build -> pack -> install
```

For Build Agent Go CLI, invoke `Orchestrator.transform({method:'incremental', lastPull})` through a small Node bridge (best), or spawn project-local `now-sdk transform --incremental` (simpler), and clear the marker only after that operation succeeds.
