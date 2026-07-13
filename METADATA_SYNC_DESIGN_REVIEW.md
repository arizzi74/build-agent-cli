# Safe Incremental Metadata Sync Before Install — Implementation Review

Date: 2026-07-13 CEST

## Executive assessment

The correct architectural direction is to use the project-local ServiceNow SDK API to perform an **incremental transform with the exact `.now/.app-data.json:lastSync` value**, persist the resulting project delta to Glider, and only then build and pack.

The current in-progress implementation has the right broad insertion point (`answerBuildParity`, after dependencies are available and before `runNowSDKBuild`), but it is not yet safe to ship. The main blockers are:

1. no per-app serialization or optimistic remote conflict check;
2. the filesystem diff is based on the original Glider checkout even though `npm install` has already run, so dependency-install side effects can be written to Glider as metadata changes;
3. Glider writeback is not verified or reconciled after ambiguous/partial failure;
4. removed-file semantics are underspecified and currently omit a deletion timestamp;
5. app-data preservation uses `map[string]interface{}`, which can alter unknown numeric values, and the atomic replacement does not fsync the parent directory;
6. an old successful `lastGliderBuild` can remain installable after a later sync/build attempt fails;
7. install validates only the retained temp directory, not the current Glider metadata revision.

These issues should be resolved before enabling the sync path by default.

## 1. Exact insertion point and operation boundary

### Recommended insertion

Keep metadata synchronization in `answerBuildParity`, not in `answerInstallParity` and not as a marker-clearing step inside `checkMetadataSyncState`.

The required order is:

```text
resolve app + acquire per-app operation lock
  -> checkout Glider project to temp
  -> make project-local SDK dependencies available
  -> snapshot a pre-transform baseline
  -> read app-data and decide whether sync is required
  -> run exact incremental SDK transform
  -> atomically update local app-data
  -> compute transform-only create/update/remove diff
  -> conflict-check and commit the complete diff to Glider
  -> verify Glider commit
  -> build
  -> persist generated build outputs
  -> pack
  -> publish a new installable build state
  -> release lock
```

In current code, the call at `build_install.go` immediately before `runNowSDKBuild` is the right **logical** location. However, the baseline used by `diffProjectForGlider` must be taken immediately before the transform, after dependency setup. The existing `project.Original` represents the checkout before `npm install`; therefore an npm-modified `package-lock.json` or `package.json` can be misclassified as an SDK transform result and pushed to Glider.

Use one of these approaches:

- **Recommended:** add a `snapshotProjectTree(project.Dir)` baseline immediately before `runMetadataIncrementalTransform`; diff the post-transform tree against that baseline. Classify creates versus updates using the original Glider entry map, but include only paths changed relative to the pre-transform baseline.
- Also make dependency installation non-mutating where possible (`--no-save` and no lockfile update), but do not rely on npm behavior as the correctness boundary.

The lock should cover checkout through successful pack/build-state publication. A narrower lock permits another build or Glider edit to invalidate the source/package relationship.

### Build/install state invalidation

At the start of a new build attempt for an app, invalidate the prior installable build state for that app. Currently, if synchronization succeeds but build or pack later fails, `lastGliderBuild` can still point to an older successful package and a subsequent install can deploy it.

Prefer a build generation object containing:

- app ID/root URI;
- source snapshot fingerprint;
- committed app-data checksum and `lastSync`;
- package path and package checksum;
- build start/completion timestamps;
- `BuildSucceeded` only after pack completes.

`answerInstallParity` should require this generation to match the current Glider app-data revision, not merely inspect the retained temp copy.

## 2. Node bridge and credential transport

### Credential source

Construct a resolved SDK credential from the already-authenticated Client state. Do not make the Node helper read profile files or SDK keychains.

Support both credential shapes accepted by `LazyCredential`:

- OAuth: `{type: "oauth", token: accessToken}`
- authenticated web session: `{type: "basic", token: userToken, cookie: cookieHeader}`

For OAuth, obtain a valid token through the existing profile OAuth lifecycle, including silent refresh when possible. `nirvanaRESTAccessToken()` only returns the in-memory/cached token and does not itself refresh it; treating an expired token as a transform failure produces avoidable auth errors. Never pass a refresh token to Node.

### Transport without leakage

Do not put credentials in:

- command-line arguments (`ps` exposure);
- environment variables (`/proc/<pid>/environ`, crash dumps, inherited children);
- generated helper source;
- temp files or project files;
- normal JSON output or progress messages.

Use inherited anonymous pipes:

- stdin or FD 3: non-secret control request (`instanceURL`, `projectDir`, `lastPull`);
- a separate inherited FD: one-shot credential JSON;
- a separate result FD: strict machine-readable result JSON;
- stderr: sanitized diagnostics only.

Passing the token in stdin, as the current implementation does, is substantially safer than argv/env/files, but separating control data, credentials, and result data reduces accidental token echoing and prevents SDK/module stdout noise from corrupting the result protocol. Close the credential FD immediately after parsing. In Go, overwrite credential byte buffers after the child exits as a best-effort hygiene measure.

Before including child output in an error, redact exact known secrets (access token, cookie header, user token, username/password if basic auth is ever supported), authorization headers, and common token JSON fields. The helper must never stringify the request or resolved credential in an exception.

### Helper behavior

The helper should:

1. resolve modules only from `<project>/node_modules`;
2. verify required exports (`NowConfig`, `Project`, `Orchestrator`, `LazyCredential`) and report the installed SDK package versions;
3. parse `NowConfig` from the selected project root;
4. call only `orchestrator.transform({method: "incremental", lastPull, format: true})`;
5. never fall back to complete sync;
6. emit normalized project-relative changed and removed paths plus transform diagnostics through the result FD;
7. dispose the Project/compiler in `finally` if supported.

The Go side remains responsible for app-data transition and Glider persistence. This keeps the commit policy in one place and prevents the helper from declaring sync complete before remote persistence succeeds.

## 3. Preserve unknown app-data fields exactly

`updateMetadataSyncCompleted` correctly attempts a temp-file + sync + rename update, but unmarshalling into `map[string]interface{}` can rewrite unknown numbers through `float64`, alter representation, and normalize all JSON formatting.

Use `map[string]json.RawMessage` (with duplicate-key rejection if practical) and replace only:

- `syncNeeded` with `false`;
- `syncStatus` with `"completed"`;
- `lastSync` with the chosen completion timestamp.

This preserves unknown field values byte-for-byte at the JSON value level, including large integers. Preserve the original file mode where available. For durable atomic replacement:

1. create a temp file in the same `.now` directory;
2. write and `fsync` it;
3. close it;
4. rename over `.app-data.json`;
5. open and `fsync` the parent directory on platforms that support it.

Do not set completed status before `Orchestrator.transform` and `Project.save` return successfully. Include the updated app-data in the same Glider change set as all transformed files so the remote marker cannot intentionally clear independently of the transformed source delta.

On transform failure, leave the original local and remote app-data unchanged by default. Recording `syncStatus:"failed"` is optional UI state, but if implemented it must preserve `syncNeeded:true`, must not change `lastSync`, and must not obscure the original transform error.

## 4. Glider create/update/delete diff semantics

### Build a transform-only diff

Create a baseline manifest immediately before transform containing every included project file's bytes or checksum, mode if relevant, and corresponding Glider entry/checksum. Compare the post-transform tree to that baseline:

- baseline absent, post present: **create**;
- baseline present, post present, content changed: **update**;
- baseline present, post absent: **remove**;
- unchanged: omit.

Normalize and validate every relative path with `safeBuildLocalPath` rules. Reject absolute paths, `..`, paths outside the app root, duplicate paths after normalization/case folding, and changes under excluded roots (`node_modules`, `target`, `dist`, `.git`, caches). Explicitly include `.now/.app-data.json`.

The SDK's `Project.save()` processes `filesForDeletion`, so removed local files are legitimate transform output and must be represented in Glider. `TransformResult.changedFiles` alone is not sufficient; filesystem comparison is the authoritative deletion detector.

### Multipart entries

For create/update file entries:

- URI: app root plus normalized relative path;
- type: `file`;
- checksum: SHA-1 expected by existing Glider helpers;
- size: exact byte length;
- mtime: commit time;
- ctime: commit time for creates; preserve original ctime for updates if the endpoint honors it;
- include one binary part keyed by checksum.

For removes:

- send no file blob;
- preserve the original URI/type/checksum/size metadata when available;
- set `dtime` to the commit timestamp;
- use deterministic child-before-parent ordering if directory removal is later added.

Do not infer and remove empty directories in the first implementation. Persist removed files only. Directory cleanup can be added after the endpoint's directory-delete contract is proven. Missing parent directory creates should be deterministic, parent-first, deduplicated, and based on one fetched remote state rather than repeated best-effort calls.

### Update the in-memory snapshot

After a verified commit, update both `project.Original` **and** `project.Entries` for creates, updates, and removes. The current code updates only `Original`. Without updating `Entries`, a file created by transform and subsequently changed by SDK build/generated-file persistence can be sent as a second create instead of an update.

## 5. Concurrency, commit verification, and rollback

### Serialization

Add a per-instance/per-app lock keyed by normalized instance URL plus app sys_id.

- In-process mutexes are necessary but insufficient if two CLI processes can operate on the same app.
- Use a profile-local advisory file lock for cross-process serialization where supported, with context-aware timeout/cancellation.
- Keep the optimistic remote checks below even with the lock, because Web IDE or other clients do not share the CLI lock.

### Optimistic conflict detection

Immediately before Glider apply, refetch state for every affected URI (or the app root if that is the only reliable API) and compare it to the checkout snapshot:

- update/remove requires the remote checksum/existence to match the original entry;
- create requires the path still not to exist;
- app-data requires its remote checksum/content to match the value used to decide `lastSync`.

Abort with `METADATA_SYNC_CONFLICT` before applying if any path changed concurrently. Never overwrite another editor's changes silently.

### Ambiguous apply failure

A network error after posting does not prove the Glider operation failed. On any apply error:

1. refetch all affected paths;
2. compare remote state/content with the complete desired post-sync state;
3. if every path matches, treat the commit as successful with a warning;
4. if none match, report a clean writeback failure and retain `syncNeeded:true` remotely;
5. if only some match, report `METADATA_SYNC_PARTIAL_WRITEBACK` and block build/install.

Do not perform a blind rollback: it can overwrite concurrent work. A guarded compensating rollback is acceptable only for paths whose remote value still exactly equals the value written by this attempt. Restore updated/removed files from the checkout snapshot and remove newly-created files; verify the compensation. If safe compensation is impossible, leave the app blocked and return the exact affected path list for manual recovery.

The desired invariant is that the updated app-data and transformed files are one logical commit. If the Glider endpoint is proven atomic, document and test that contract; verification should still handle lost responses.

### Build and install after commit

Only build after the Glider commit is verified. A build failure does **not** require rolling back a successful metadata synchronization: the synchronized source is legitimate, but no installable build may be published.

Before install, refetch `.now/.app-data.json` from Glider and require:

- `syncNeeded == false`;
- `syncStatus == "completed"`;
- checksum/`lastSync` equals the build generation;
- package exists and its checksum matches the recorded package checksum.

If current Glider state diverges, require another build.

## 6. Required result/reporting shape

Return a bounded metadata-sync summary with no secrets:

- mode: `incremental`;
- lastPull UTC string;
- completedAt milliseconds;
- created/updated/removed relative paths (bounded, with counts);
- SDK versions;
- whether an ambiguous Glider response was verified;
- warnings.

Normalize SDK absolute `changedFiles` paths to project-relative paths before exposing them. Do not return temp-directory paths or credentials.

## 7. Test strategy

### Unit tests

1. **App-data parsing/update**
   - absent marker and `syncNeeded:false` are no-ops;
   - invalid/missing `lastSync` blocks incremental sync;
   - exact millisecond-to-UTC conversion;
   - unknown strings, objects, arrays, booleans, large integers, and raw numeric forms survive;
   - original mode preserved;
   - temp file and parent-directory durability path exercised where supported;
   - transform failure leaves original bytes unchanged.

2. **Diff calculation**
   - create/update/remove/no-op in one tree;
   - removed SDK files are included;
   - excluded dependency/build/cache trees never appear;
   - npm-mutated files before baseline are not included;
   - path traversal, absolute paths, normalization collisions, symlinks escaping root, and case collisions are rejected;
   - app-data is included as an update.

3. **Glider request encoding**
   - deterministic create/update/remove ordering;
   - parent directories before files;
   - remove entries contain `dtime` and no blob;
   - each create/update checksum has exactly one matching blob;
   - no credential text appears in body, URL, headers, or diagnostics.

4. **Credential bridge**
   - OAuth and cookie/user-token credential shapes;
   - silent OAuth refresh path;
   - no token in argv/env/helper file/stdout/stderr/error;
   - result protocol survives unrelated stdout noise or avoids stdout entirely;
   - cancellation kills the child and closes credential pipes.

### Component tests with fake Node/helper

Inject the helper runner rather than requiring real ServiceNow access. Cover:

- successful transform with create/update/remove;
- helper non-zero exit, malformed result, timeout, and cancellation;
- app-data update failure after transform;
- Glider clean failure, lost-response-but-committed, and partial commit;
- build failure after successful sync leaves no installable build generation;
- package failure likewise invalidates install.

### Concurrency tests

- two goroutines building the same app serialize;
- different apps can proceed independently;
- two processes contend on the advisory lock;
- remote checksum changes between checkout and commit produce `METADATA_SYNC_CONFLICT`;
- install is rejected if app-data changes after build.

Run these with `go test -race`.

### SDK compatibility integration test

Create a disposable local fixture project using the installed project-local SDK packages. Stub the Connector/download boundary or serve a controlled metadata archive, then verify that `Orchestrator.transform`:

- receives `method:"incremental"` and exact `lastPull`;
- updates a TypeScript metadata file;
- creates generated keys;
- deletes an elective/deleted record file;
- returns a parseable result;
- can be followed by `now-sdk build` and `now-sdk pack`.

Keep the existing read-only ServiceNow probe as an optional manual acceptance test. Never run install in automated tests.

## 8. Ship criteria

Do not enable automatic metadata sync until all of the following are true:

- transform-only baseline diff is implemented;
- deleted files have proven Glider semantics and tests;
- credentials cannot appear in argv/env/files/logs;
- app-data unknown values are preserved without float conversion;
- per-app lock and optimistic conflict checks exist;
- ambiguous/partial Glider writes are verified and block install when unresolved;
- old build state is invalidated on every new build attempt;
- install verifies current remote app-data against the package generation;
- race, unit, component, and SDK fixture tests pass.
