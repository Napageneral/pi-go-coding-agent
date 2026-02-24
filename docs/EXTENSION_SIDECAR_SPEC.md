# Extension Sidecar Spec (Node Runtime)

## Goal

Run extensions in a separate Node process while keeping the coding-agent runtime and session manager in Go.

This document locks the host/sidecar contract for the first extension phase.

## High-Level Design

- Go process is the host and source of truth for sessions, providers, and built-in tools.
- Node sidecar process hosts extension code and extension-registered tools.
- Communication is newline-delimited JSON request/response over stdio.
- Sidecar is optional. If not configured, runtime behavior remains unchanged.

## Transport

Each line on stdout/stdin is a full JSON object:

```json
{"id":"1","method":"initialize","params":{"protocolVersion":"2026-02-24"}}
{"id":"1","result":{"protocolVersion":"2026-02-24","tools":[]}}
```

Sidecar may also emit host notifications on stdout (not tied to `id`), e.g.:

```json
{"type":"extension_ui_request","id":"ui-1","method":"input","title":"Approve?"}
```

Response errors:

```json
{"id":"2","error":{"code":"tool_not_found","message":"unknown tool foo"}}
```

## Methods

### `initialize`

Request:
- `protocolVersion` (string)
- `cwd` (string)
- `sessionId` (string)
- `sessionFile` (string)
- `sessionDir` (string, optional)
- `sessionName` (string, optional)
- `leafId` (string, optional)
- `sessionHeader` (object, optional)
- `sessionEntries` (object[], optional)
- `currentModel` (`types.Model`, optional)
- `allModels` (`types.Model`[], optional)
- `availableModels` (`types.Model`[], optional)
- `providerApiKeys` (map<string,string>, optional)
- `providerAuthTypes` (map<string,string>, optional; values: `oauth` | `api_key`)
- `contextUsage` (object, optional)
- `systemPrompt` (string, optional)
- `thinkingLevel` (string, optional)
- `isIdle` (bool)
- `hasPendingMessages` (bool)
- `hostTools` (`types.Tool`[], optional)
- `activeTools` (string[], optional)
- `extensionPaths` (string[])
- `flagValues` (object, optional)
- `hasUI` (bool, optional; enables extension dialog methods over RPC)

Response:
- `protocolVersion` (string)
- `sidecarVersion` (string, optional)
- `capabilities` (string[], optional)
- `tools` (`types.Tool`[])
- `flags` (registered flag metadata)
- `commands` (registered command metadata; may include command source `path`)
- `providers` (registered provider definitions)

### `emit`

Request:
- `event.type` (string)
- `event.payload` (object)

Response (optional shape by event):
- `input`
- `beforeAgentStart`
- `context`
- `toolCall`
- `toolResult`
- `sessionBeforeSwitch`
- `sessionBeforeFork`
- `sessionBeforeCompact`
- `sessionBeforeTree`

### `tool.execute`

Request:
- `name` (string)
- `toolCallID` (string)
- `arguments` (object)

Response:
- `types.ToolResult`

### `command.execute`

Request:
- `name` (string)
- `args` (string)
- `currentModel` (`types.Model`, optional)
- `allModels` (`types.Model`[], optional)
- `availableModels` (`types.Model`[], optional)
- `providerApiKeys` (map<string,string>, optional)
- `providerAuthTypes` (map<string,string>, optional; values: `oauth` | `api_key`)
- `contextUsage` (object, optional)
- `systemPrompt` (string, optional)
- `thinkingLevel` (string, optional)
- `isIdle` (bool)
- `hasPendingMessages` (bool)

Response:
- `handled` (boolean)
- `output` (string, optional)
- `actions` (array, optional)

### `shutdown`

Graceful stop request from host to sidecar.

### `ui.respond`

Request:
- `id` (string; extension UI request id)
- `value` (string, optional)
- `confirmed` (bool, optional)
- `cancelled` (bool, optional)

Response:
- `{ "resolved": true|false }`

## Event Names (Phase 1)

- `session_start`
- `session_before_switch`
- `session_switch`
- `session_before_fork`
- `session_fork`
- `session_before_compact`
- `session_before_tree`
- `session_tree`
- `session_compact`
- `session_shutdown`
- `input`
- `before_agent_start`
- `context`
- `agent_start`
- `agent_end`
- `turn_start`
- `turn_end`
- `message_start`
- `message_end`
- `tool_execution_start`
- `tool_execution_end`
- `message_update` (synthetic/final update in current Go runtime)
- `tool_call`
- `tool_result`
- `model_select`

## Event Payload Compatibility

Goal:
- Match upstream extension event field names closely enough that common upstream-style handlers run without translation.

Conventions:
- `message_start` / `message_update` / `message_end` include `message`.
- Message events include both `toolCallId` and `toolCallID` (compat alias).
- `tool_execution_start` includes:
  - `toolCallId`
  - `toolName`
  - `args`
- `tool_execution_update` includes:
  - `toolCallId`
  - `toolName`
  - `args`
  - `partialResult`
- `tool_execution_end` includes:
  - `toolCallId`
  - `toolName`
  - `args`
  - `result`
  - `isError`
- `tool_call` includes:
  - `toolCallId`
  - `toolName`
  - `input`
- `tool_result` includes:
  - `toolCallId`
  - `toolName`
  - `input`
  - `content`
  - `details`
  - `isError`
- `input` event uses `source="interactive"` for CLI prompts.
- `model_select` payload includes `model`, `previousModel` (when available), and `source`.
- `session_start` payload includes `sessionId`, `sessionFile`, `sessionName`, `cwd`, `hostTools`, and `activeTools`.
- `session_start` payload also includes `leafId` for command-context tree hook preparation.
- `session_start` may include full session snapshot (`sessionDir`, `sessionHeader`, `sessionEntries`) used by sidecar `ctx.sessionManager` mirror.
- `session_start` may include context/model snapshots (`currentModel`, `allModels`, `availableModels`, `providerApiKeys`, `systemPrompt`, `thinkingLevel`, `isIdle`, `hasPendingMessages`) used by sidecar `ctx` and `ctx.modelRegistry` mirrors.
- Context/model snapshots may include `providerAuthTypes` used by `ctx.modelRegistry.isUsingOAuth(...)`.
- `session_start` may include `contextUsage` snapshot used by sidecar `ctx.getContextUsage()` mirror.
- `message_end` includes persisted `entry` payload for incremental sidecar session-mirror updates.
- `session_compact` includes `compactionEntry` payload for incremental sidecar session-mirror compaction updates.
- Host event payloads include `ctx*` snapshot aliases (`ctxModel`, `ctxSystemPrompt`, `ctxThinkingLevel`, `ctxIsIdle`, `ctxHasPendingMessages`, `ctxProviderAuthTypes`, `ctxContextUsage`) for ongoing context sync.

Tool override behavior:
- If sidecar registers a tool name matching a built-in tool, sidecar execution takes precedence for that name.

## Aggregation Semantics

- `input`: allow transform/handled outcomes.
- `before_agent_start`: chain `systemPrompt` updates, collect synthetic messages.
- `context`: allow per-turn context/system-prompt transforms before provider call.
- `tool_call`: any extension may block execution (`block=true`).
- `tool_result`: extensions may override `content`/`details`/`isError`.
- `session_before_switch`: first cancel wins (`cancel=true`) for `newSession`/`switchSession`.
- `session_before_fork`: first cancel wins (`cancel=true`) for `fork`.
- `session_before_compact`: first cancel wins (`cancel=true`) and latest valid `compaction` override wins for `compact`.
  - `compaction.details` is preserved in persisted `compaction` session entries and in `session_compact.compactionEntry`.
- `session_before_tree`: first cancel wins (`cancel=true`) for `navigateTree`.
- Other events are fire-and-forget notifications in phase 1.

Command-context cancellation semantics:
- Sidecar command-context methods (`newSession`, `switchSession`, `fork`, `navigateTree`) evaluate matching `session_before_*` handlers in-sidecar before dispatching host actions.
- If cancelled, command-context methods return `{ cancelled: true }` and no host action is emitted.
- For `navigateTree`, sidecar includes `session_before_tree` summary overrides in action payload so host can persist branch summary entries.
  - `summary.details` is preserved in persisted `branch_summary` entries.
- For `newSession({ setup })`, sidecar executes setup callback against a setup-session proxy and serializes supported append operations into `new_session.setupEntries` for host replay.

Host action bridge:
- `emit` and `command.execute` may return `actions`.
- Go host currently applies:
  - `send_user_message` (queued as steer/follow-up; command path can trigger an immediate turn)
    - supports `text` and/or structured `content` blocks (e.g. `text`, `image`)
  - `send_message` (assistant/user message append/queue semantics)
  - `set_model`
  - `set_thinking_level`
  - `append_entry`
  - `set_session_name`
  - `set_label`
  - `set_active_tools`
  - `new_session`
  - `switch_session`
  - `fork`
  - `navigate_tree`
  - `compact`
  - `reload` (no-op in current CLI mode)
  - `wait_for_idle` (no-op in current CLI mode)
- This enables extension commands to drive native Go turns via `pi.sendUserMessage(...)`.
- `navigate_tree` action supports option payload keys used by upstream command-context calls:
  - `targetId` (preferred) / `entryId` (compat)
  - `summarize`
  - `customInstructions`
  - `replaceInstructions`
  - `label`
- `new_session` action supports optional `setupEntries` serialized operations:
  - `parentSession` uses upstream path semantics when provided (session file path).
  - `append_message`
  - `append_thinking_level_change`
  - `append_model_change`
  - `append_custom_entry`
  - `append_custom_message`
  - `append_session_info`
  - `append_label` (with `targetId` or setup-entry `targetRef`)

Sidecar event bus:
- `pi.events.on(channel, handler)` and `pi.events.emit(channel, data)` are implemented in-process in the Node sidecar.
- Bus scope is the sidecar process (extension-to-extension signaling within one host runtime instance).

Sidecar host-state sync:
- Sidecar updates command-context session fields (`sessionId`, `sessionFile`, `sessionName`) from host `session_start` events.
- Sidecar updates command-context leaf tracking (`leafId`) from host `session_start` and `session_tree` events.
- Sidecar updates local read-only session mirror from host snapshots (`initialize`/`session_start`) plus incremental `message_end` and `session_compact` events.
- Sidecar updates local `ctx`/`ctx.modelRegistry` mirrors from host snapshots in `initialize`, `command.execute`, and `session_start`; per-event `ctx*` fields keep dynamic context fields synchronized.
- Sidecar updates local `ctx.getContextUsage()` mirror from host `contextUsage` snapshots and `ctxContextUsage` event updates.
- Host `session_start` refresh is emitted after extension-driven `new_session`/`switch_session`/`fork` actions.

## Failure Policy

- Sidecar startup/initialize failure: runtime construction fails fast.
- Event emission failures during a run: best-effort (host continues).
- Extension tool execution failures: surfaced as tool errors in-session.

## Non-Goals (Phase 1)

- Upstream package manager compatibility.
- Loading npm/git extension packages directly.
- Full upstream extension API parity.
