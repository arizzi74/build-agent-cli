# Build Agent Web UI HAR Parity Audit

Last audited: 2026-07-13

## Reference captures

- `/tmp/New_conversation.har`
- `/tmp/lima_build.har`
- `/tmp/myapp_full.har`
- `/tmp/stop.har`
- `/tmp/demoalectriallwfze140800.service-now.com.har`
- `/tmp/workspace_demoalectriallwfze140800.service-now.com.har`
- `/tmp/zaiagents.service-now.com.har`

## Implemented protocol parity

- Current Nirvana websocket transport and connect/message/response/stop frame shapes.
- Exact current Web UI capability set, including `semantic_search`.
- Application-level JSON ping/pong keepalive.
- User/assistant conversation persistence and canonical Build Agent API fallback.
- App creation, conversation application binding/title update, and workspace Glider changes.
- Glider filesystem actions and generated Fluent file writeback.
- Build, dependency installation, packaging, app upload, progress polling, and installed-artifact links.
- Empty Web UI approval auto-response behavior.
- Double-Esc cancellation with the observed `stop` frame.
- Turn-id-based suppression of late events from cancelled turns.

## Remaining gaps supported by current HAR evidence

### Rich intermediate message persistence

The Web UI persists thinking/loading/tool/summary rows during a turn. The CLI currently persists final user and assistant rows and renders tool status locally, but does not persist the full intermediate timeline.

This does not block the agent protocol, builds, installs, or conversation continuation. It affects fidelity when opening a CLI-created turn later in the Web UI.

### Tool and turn telemetry

The Web UI posts turn-level and tool-level telemetry to:

```text
POST /api/sn_build_agent/build_agent_api/telemetry
```

The CLI does not currently emit this telemetry. It is observability parity rather than execution parity.

### App-created checkpoint mutation

After app creation, the Web UI patches the initiating user message with:

```json
{
  "hasCheckpoints": true,
  "checkpoints": [{"id":"APP_CREATED:<app-id>","appDir":"<app-name>"}]
}
```

The CLI already binds the application, updates the conversation title, clears the working set, and writes the workspace. It does not yet retain and patch the initiating server message row with this checkpoint.

### Full Glider package-manager parity

The Web UI installs dependencies into the Glider-backed virtual workspace. The CLI intentionally uses a local temporary npm/SDK project and writes relevant generated files back to Glider. Build/install behavior is equivalent for the captured flow, but virtual `node_modules` materialization is not.

### Upload authentication

The captured upload processor uses browser cookies and `sysparm_ck`. The CLI requires a validated browser-style session token for this step; OAuth-only upload remains unproven.

### Dynamic startup configuration

The Web UI reads health, variant, product availability, timeout/property bundles, and app update availability during startup. The CLI currently uses local/default timeout policy and only performs equivalent calls needed for active operations. Dynamic rollout/config parity remains optional and instance-dependent.

## Priorities

1. Preserve protocol correctness and cancellation isolation.
2. Maintain build/install/app/workspace parity.
3. Add rich intermediate persistence and telemetry only after their schemas and server row identifiers can be reproduced without risking duplicate or malformed history.
4. Avoid advertising capabilities that are not present in the current Web UI connect frame.
