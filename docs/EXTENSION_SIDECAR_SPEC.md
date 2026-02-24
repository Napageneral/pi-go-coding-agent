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
- `sessionName` (string, optional)
- `hostTools` (`types.Tool`[], optional)
- `activeTools` (string[], optional)
- `extensionPaths` (string[])
- `flagValues` (object, optional)

Response:
- `protocolVersion` (string)
- `sidecarVersion` (string, optional)
- `capabilities` (string[], optional)
- `tools` (`types.Tool`[])
- `flags` (registered flag metadata)
- `commands` (registered command metadata)
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

Response:
- `handled` (boolean)
- `output` (string, optional)
- `actions` (array, optional)

### `shutdown`

Graceful stop request from host to sidecar.

## Event Names (Phase 1)

- `session_start`
- `session_before_switch`
- `session_switch`
- `session_before_fork`
- `session_fork`
- `session_before_tree`
- `session_tree`
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
- `session_before_tree`: first cancel wins (`cancel=true`) for `navigateTree`.
- Other events are fire-and-forget notifications in phase 1.

Host action bridge:
- `emit` and `command.execute` may return `actions`.
- Go host currently applies:
  - `send_user_message` (queued as steer/follow-up; command path can trigger an immediate turn)
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
  - `reload` (no-op in current CLI mode)
  - `wait_for_idle` (no-op in current CLI mode)
- This enables extension commands to drive native Go turns via `pi.sendUserMessage(...)`.
- `navigate_tree` action supports option payload keys used by upstream command-context calls:
  - `targetId` (preferred) / `entryId` (compat)
  - `summarize`
  - `customInstructions`
  - `replaceInstructions`
  - `label`

Sidecar event bus:
- `pi.events.on(channel, handler)` and `pi.events.emit(channel, data)` are implemented in-process in the Node sidecar.
- Bus scope is the sidecar process (extension-to-extension signaling within one host runtime instance).

Sidecar host-state sync:
- Sidecar updates command-context session fields (`sessionId`, `sessionFile`, `sessionName`) from host `session_start` events.
- Host `session_start` refresh is emitted after extension-driven `new_session`/`switch_session`/`fork` actions.

## Failure Policy

- Sidecar startup/initialize failure: runtime construction fails fast.
- Event emission failures during a run: best-effort (host continues).
- Extension tool execution failures: surfaced as tool errors in-session.

## Non-Goals (Phase 1)

- Upstream package manager compatibility.
- Loading npm/git extension packages directly.
- Full upstream extension API parity.
