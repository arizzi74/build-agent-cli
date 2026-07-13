# Build Agent CLI Install Failure Investigation

Started: 2026-07-13 08:53 CEST
Model policy: openai/gpt-5.6-sol only; high reasoning.

## Step 1 — Inspect saved screen dump

Source: `/tmp/install_fail.txt`

Observed failure repeated twice:

> Metadata sync is required before install; run sync/build in the Web UI or clear the sync-needed state before installing from the CLI.

The build itself succeeds. The failure is a CLI pre-install guard, not an SDK compiler or upload processor failure. The transcript also shows server-side probing of update sets, properties, Glider tables, and `sn_glider_ide_file_index`, but no successful clearing of the state.

## Step 2 — Trace the pre-install guard

`Client.ensureMetadataReadyForInstall` reads `.now/.app-data.json` through the Glider sync API and rejects install only when:

```go
syncNeeded == true && syncStatus != "InProgress"
```

The code comment and `SPEC_IMPLEMENTED.md` explicitly state that detection exists but local metadata synchronization (`fluent.sync`) is not implemented. Therefore the guard is behaving as designed, but the CLI lacks the operation needed to satisfy it.

Important distinction: Glider file synchronization (`/sync/state`, `/sync/files`, `/sync/changes/apply`) is not the same operation as Now SDK metadata synchronization. Rewriting `.app-data.json` or clearing its flag would only bypass the safety check and may install stale/incomplete metadata.

## Step 3 — Locate the real ServiceNow SDK synchronization implementation

The CLI's parity design explicitly deferred metadata synchronization. A local installed copy of `@servicenow/sdk` is available under `/home/ubuntu/.openclaw/workspace/node_modules/@servicenow/sdk`, so the next step is to inspect its command surface and implementation rather than bypass `.app-data.json`.

## Step 4 — Inspect the local SDK CLI command surface

`@servicenow/sdk` version 4.6.1 exposes `auth`, `init`, `download`, `build`, `install`, `dependencies`, `transform`, `clean`, `pack`, and `explain`. It has no metadata `sync` command. The build command delegates directly to `@servicenow/sdk-api` `Orchestrator.build()`.

No `syncNeeded`, `syncStatus`, or `.app-data.json` references exist in the installed Node SDK package family. This state is therefore likely owned by the browser Glider/Fluent extension rather than standard `now-sdk build/install`.

## Step 5 — HAR literal search attempt

The first bulk Python/HAR extraction attempt failed because the output redirection was accidentally placed after the heredoc terminator. It made no source changes and produced no evidence; rerunning with corrected shell syntax.

## Step 6 — Browser extension extraction retry

The first extension extraction selected an empty duplicate HAR response, and the context script repeated the earlier heredoc-redirection syntax error. No application/source state changed. Corrective action: write a standalone Python extractor and select the largest non-empty matching response across all HARs.

## Step 7 — Recover the Web UI install control flow

Extracted the non-empty browser extension bundles from the HAR. The Build Agent extension implements this exact sequence:

1. `checkMetadataSyncNeeded()` reads `.now/.app-data.json` and tests `syncNeeded === true && syncStatus !== 'InProgress'`.
2. Prompt `Sync & Build`.
3. Execute VS Code command `fluent.sync(scopeId)`.
4. Re-read `.now/.app-data.json`; if still needed, treat sync as cancelled/failed.
5. Execute `fluent.build(scopeId)`.
6. Execute `fluent.forceDeploy(scopeId)` because sync was already handled.

The Go CLI implements step 1 and then stops. This establishes the immediate failure cause as missing Web UI orchestration parity, not a failed upload or broken package.

## Step 8 — Determine how sync-needed is set and where sync executes

The Fluent extension detects server-side metadata newer than `.app-data.json.lastSync` through:

- `GET /api/sn_glider/sync/get_latest_updated_metadata?sys_ids=...&scope_id=...` for configuration apps.
- `GET /api/sn_glider/sync/metadata/<scopeId>` for other apps.

When newer metadata is observed, it writes `syncNeeded=true`. The `fluent.sync` command delegates the record transfer to the Synchrotron extension and waits for `onSyncComplete`; it is not merely a local flag reset.

A broad minified-context extraction was terminated after returning the relevant section. Continuing with targeted endpoint/service extraction.

## Step 9 — HAR sync endpoint query retry

A read-only inline Python query repeated the heredoc redirection error. No source/instance state changed. To prevent recurrence, all further nontrivial Python probes will be saved as standalone scripts before execution.

## Step 10 — HAR coverage of metadata sync

The captures include read-only sync detection calls (`GET /api/sn_glider/sync/metadata/<scopeId>`) and responses identifying the latest changed metadata record. They do not contain a completed `fluent.sync` transfer, so the full Synchrotron download protocol cannot be reconstructed from those requests alone. Next: inspect the current Lima Glider files and latest metadata read-only using the saved authenticated profile.

## Step 11 — Read live Lima sync state

Used the saved `groqz` OAuth profile without printing credentials to read only:

- `now-file:/1baf0f902b4a07506969f1cc6e91bf82/.now/.app-data.json`
- `GET /api/sn_glider/sync/metadata/1baf0f902b4a07506969f1cc6e91bf82`

Raw sanitized output is stored at `/tmp/lima_sync_state.txt`.

## Step 12 — Identify the conflicting instance metadata

Live state proves the guard is legitimate:

- Workspace `lastSync`: `1783587823210`.
- `syncNeeded=true`, `syncStatus=never_ran`.
- Latest scoped metadata: Script Include `TempPowerBankInsert`, updated by `admin` at `1783893210000`.

The instance metadata is newer than the workspace snapshot, so clearing the flag would risk installing a package that does not include that change.

## Step 13 — 2026-07-13 09:04 CEST — Trace metadata-sync ownership across seven HARs

Completed a request/response/WebSocket/bundle trace of all seven `/tmp/*.har` captures; full evidence is in `HAR_METADATA_SYNC_TRACE.md`. The Web UI's Build Agent install tool only orchestrates `fluent.sync` -> re-check `.now/.app-data.json` -> `fluent.build` -> `fluent.forceDeploy`. The Fluent extension implements `fluent.sync` through the client-side Now SDK `forge-web-worker.js` (`forge/sync`), while Glider REST supplies metadata timestamp checks and `/v2/sync/*` workspace-file persistence. Build Agent REST only records conversation/tool results; no Build Agent `sync` tool invocation or sync endpoint was observed. Therefore CLI parity requires implementing the SDK metadata-sync operation and state transitions, not clearing `syncNeeded` or treating ordinary Glider file sync as metadata sync.

## Step 13 — Measure divergence after last sync

A read-only `sys_metadata` query for Lima records newer than the workspace `lastSync` reached the 200-row limit. Within those first 200 rows: 84 `sys_documentation`, 83 `sys_dictionary`, 13 `sys_choice_set`, 6 `sys_db_object`, 6 `sys_number`, 6 licensing records, plus form/section metadata. This is substantial divergence, consistent with tables/fields created directly on the instance. Bypassing the guard is unsafe.

## Step 14 — Check whether the newest instance record is in the built package

Inspected the preserved failed-build temp directory `/tmp/ba-glider-build-572276101`, its source tree, generated `dist`, and package ZIP for `TempPowerBankInsert` and its instance identifiers. This tests the concrete overwrite/omission risk behind the sync guard.

## Step 15 — Verify concrete package omission

`TempPowerBankInsert` and its relevant instance identifiers are absent from the preserved source tree, generated `dist`, and actual app package `/tmp/ba-glider-build-572276101/target/lima_0_0_1.zip`. Therefore the attempted install artifact does not contain the newest instance-side Script Include; the sync guard prevented deployment of a materially stale package.

A harmless shell-word-splitting error occurred while checking unrelated nested dependency ZIP filenames containing spaces; the actual app ZIP was checked correctly.

## Step 16 — Locate the client-side SDK sync worker

Extracting the largest captured `forge-web-worker.js` response and its endpoint/operation strings to determine whether the CLI can invoke the worker API directly or needs a native equivalent.

## Step 17 — Worker availability

The HARs reference `forge-web-worker.js` but contain no non-empty worker response body. They do contain the full Synchrotron extension bundle. Therefore the worker cannot be copied/reused from the captures; tracing Synchrotron protocol and local installed SDK internals instead.

## Step 17 — 2026-07-13T09:06:26+02:00 — Trace exact SDK metadata synchronization

Completed the local source trace in `SDK_METADATA_SYNC_TRACE.md`. The legitimate guard-clearing path is IDE command `fluent.sync` / worker operation `forge/sync`, backed headlessly by SDK `Orchestrator.transform({ method: 'incremental', lastPull })` (`now-sdk transform --incremental`). It downloads `/api/fluent/download/<scopeId>`, unpacks and transforms metadata, saves changed/deleted project files, then the IDE wrapper sets `.now/.app-data.json` to `syncStatus=completed`, `lastSync=Date.now()`, `syncNeeded=false`. Safe ordering is sync/transform → state transition → build → pack → install; directly clearing the JSON flag is an unsafe bypass.

## Step 14 — Validate existing OAuth credential against Fluent download

Performed a read-only incremental download probe against `/api/fluent/download/<Lima scope>` using the Build Agent CLI's saved OAuth bearer token and the exact `lastSync` timestamp. Only response headers, byte count, and ZIP magic were recorded; metadata was not extracted or applied.

## Step 15 — Retry Fluent download through the installed SDK Connector

The direct HTTP probe's `Accept` header caused HTTP 406. Retried through the exact installed SDK `LazyCredential` + `Connector.download` implementation, using the saved OAuth bearer and timestamp, while reading only response status/headers/byte count/magic and not applying data.

## Step 16 — Inspect incremental archive manifest without applying it

Downloaded the same read-only incremental archive to `/tmp/lima-incremental-metadata.zip` and listed entries with `unzip -l`. It was not extracted into the project and no transform/save operation was run.

## Step 18 — Execute incremental transform on an isolated project copy

Copied `/tmp/ba-glider-build-572276101` to `/tmp/lima-transform-probe` and invoked the installed SDK API with the saved OAuth credential and exact `lastPull`. This may mutate only the disposable copy. Captured transform result, stderr, before/after manifests, and app-data state.

## Step 19 — Diff and rebuild the transformed disposable project

The transform changed one logical source file and the Fluent key registry. Captured the exact source diff at `/tmp/lima-power-banks-transform.diff`, then ran `now-sdk build` against only `/tmp/lima-transform-probe` to validate the required post-sync rebuild. No Glider or instance writes were made.

## Step 20 — Validate handling of elective update records

Verified the transformed rebuild emits the newly downloaded Script Include and fix script under `dist/app/author_elective_update/`. Their `deleted: true` key entries are SDK bookkeeping for records represented as elective-update XML, not evidence that the instance records would be deleted. The transformed build therefore preserves the downloaded delta in deployable output.

## Step 20 correction — Elective records are deletion instructions

Correction to the initial interpretation: the rebuilt `author_elective_update` XML explicitly has `action="DELETE"` for both `TempPowerBankInsert` and its fix script. The SDK is importing instance-side deletion semantics, not preserving active script bodies. Investigating whether these are genuine deletions or part of a replacement flow before concluding safety.

## Step 21 — Final disposable pack validation

Ran `now-sdk pack` only on `/tmp/lima-transform-probe` after the successful incremental transform and rebuild. Verified the resulting package includes the synchronized table delta and the two legitimate instance deletion records. No install/upload was performed.

## Conclusion — Root cause and repair

### Root cause

The CLI install does not fail in upload or ServiceNow deployment. It is intentionally stopped before upload by `checkMetadataSyncState(build.TempDir)` because the Glider workspace's `.now/.app-data.json` says:

```json
{"lastSync":1783587823210,"syncNeeded":true,"syncStatus":"never_ran"}
```

The workspace was last synchronized on 2026-07-09 11:03:43 CEST. Newer Lima metadata exists through 2026-07-12 23:53:30 CEST. The Web UI handles this by `fluent.sync -> fluent.build -> fluent.forceDeploy`; the Go CLI implemented only detection and rejection.

### Verified safe implementation

Using the existing `groqz` OAuth token, installed SDK 4.6.1, and an isolated copy, this sequence succeeded:

1. `Orchestrator.transform({method:"incremental", lastPull:"2026-07-09 09:03:43"})`.
2. Transform changed `src/fluent/x_snc_lima_power_banks.now.ts` and `src/fluent/generated/keys.ts`.
3. It imported legitimate deletion records for two temporary scripts removed on the instance.
4. `now-sdk build` succeeded.
5. `now-sdk pack` emitted `/tmp/lima-transform-probe/target/lima_0_0_1.zip`, containing the synchronized delta.

### Required CLI fix

When `syncNeeded` is true, the Build Agent Go CLI should:

1. Sync the current Glider project to its temp directory.
2. Invoke a Node helper using installed `@servicenow/sdk-api` with the saved OAuth token and exact app-data `lastSync`:
   `Orchestrator.transform({method:"incremental", lastPull:<UTC timestamp>})`.
3. On success only, atomically set local temp `.now/.app-data.json` to `syncStatus:"completed"`, `syncNeeded:false`, `lastSync:now` while preserving unknown fields.
4. Persist all transformed changed/deleted project files and the updated app-data back to Glider.
5. Rebuild and repack from the synchronized sources.
6. Proceed to upload/install.
7. On transform failure, leave `syncNeeded:true`, record failed status, and do not install.

Do not merely clear the marker. Do not silently fall back to complete sync. Serialize this sequence per app and surface the transformed file list to the user.

Investigation was read-only against ServiceNow except for authenticated GET/download requests. All mutating transform/build/pack tests were confined to `/tmp/lima-transform-probe`; no Glider source or instance metadata was changed.

## Cleanup — orphaned investigation greps

At 2026-07-13 09:23 CEST Antonio noticed a grep consuming 100% CPU. Two orphaned read-only searches launched during the investigation were still attached to the long-lived shell parent:

- PID 3162691: regex scan across seven large HAR files; ~100% CPU for ~20 minutes.
- PID 3161604: recursive workspace sync-term scan; mostly sleeping.

Both were terminated and process state was verified. Future large HAR searches should use bounded standalone scripts and explicit command timeouts rather than unbounded binary grep.
