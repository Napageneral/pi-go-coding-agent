# Go Rewrite Research (Frozen Target: pi-mono@380236a0)

## 1. Customer Experience First

Desired UX for your projects:
- Native Go binary.
- Optional RPC mode for headless clients.
- Persistent sessions with branchable history.
- Reliable tool-call loop for coding workflows.
- Provider/model selection at runtime.

What you explicitly do **not** need for this phase:
- TUI.
- Upstream package-manager/discovery/install behavior for extensions.

What is now in scope:
- Node sidecar runtime for extensions.
- RPC mode parity for coding-agent control workflows (`prompt`/queue/session/model/thinking/state/bash commands).
- Go host lifecycle/event and tool bridge to sidecar.
- Agent-loop parity behaviors from `packages/agent` that matter for coding runtime control:
  - steering queue interruption semantics.
  - follow-up queue continuation semantics.

## 2. Dependency Coupling in TS Monorepo

Core coupling in `pi-mono`:
- `@mariozechner/pi-coding-agent` depends on:
  - `@mariozechner/pi-agent-core` (agent loop/state/events/tool execution lifecycle)
  - `@mariozechner/pi-ai` (provider implementations, model registry, auth/OAuth integrations)
  - `@mariozechner/pi-tui` (interactive UI; out of scope for this rewrite)

Implication:
- Full coding-agent behavior is not just one package; it is a stack (`coding-agent` + `agent-core` + `ai`).

## 3. Main Rewrite Complications

1. Provider surface and compatibility matrix
- OpenAI-compatible providers have provider-specific quirks (tool-call IDs, token fields, strict-mode flags, tool-result naming).
- Anthropic/Google/Bedrock each have different message contracts for tool calls and tool results.
- OpenAI API family is split operationally:
  - `openai-completions`: chat-completions payload.
  - `openai-responses`: Responses API payload and stop-reason mapping.
  - `openai-codex-responses`: ChatGPT codex endpoint (`/codex/responses`) with JWT-derived account header.
- Google provider family is split operationally:
  - `google-generative-ai`: API-key request model.
  - `google-gemini-cli` / `google-antigravity`: OAuth Bearer + Cloud Code Assist request envelope.
  - `google-vertex`: project/location-scoped endpoint + cloud access token.

2. Credential model differences
- TS stack supports API keys + OAuth providers with refresh/headers logic.
- Go rewrite currently prioritizes API-key paths and auth-file/env resolution for the targeted coding-agent flow.

3. Session semantics
- Session files are tree-shaped append-only logs (`id`/`parentId`) with branching and context reconstruction.
- Cross-project usage requires session isolation by cwd (not a single flat sessions directory).

4. Tool behavior expectations
- Coding quality depends heavily on deterministic `read/write/edit/bash/ls/find/grep` semantics.
- TS implementation had runtime bootstrap for `fd`/`rg`; this is a portability and operational concern in Node-based flow.

## 4. “Tool Bootstrap Depends on Runtime Downloads” Clarified

In TS coding-agent:
- `find` and `grep` rely on `fd` and `rg`.
- When missing, TS path calls `ensureTool(...)` and can download managed binaries into `~/.pi/agent/bin`.

In this Go rewrite:
- No runtime binary downloads are required for these tools.
- `find`/`grep` are implemented natively in Go.

## 5. Go Module Dependency Surface (Current)

Direct dependencies:
- `github.com/aws/aws-sdk-go-v2/config`
- `github.com/aws/aws-sdk-go-v2/service/bedrockruntime`

Notes:
- OpenAI/Anthropic/Google providers use native Go `net/http` paths in this module (no provider SDK dependency there right now).

## 6. Extension Parity Findings (Current)

High-impact upstream compatibility points now treated as first-class:
- Event payload shape parity for common extension handlers:
  - `message` envelope on message events.
  - `toolCallId`/`input`/`args`/`result`/`partialResult` fields for tool lifecycle events.
- `model_select` lifecycle emission when model changes via runtime API.
- Extension tool override precedence for built-in tool names (e.g. extension `read` overrides built-in `read`).
- Action bridge for extension commands/events so `pi.sendUserMessage(...)` can queue/trigger native Go turns.
- `pi.sendUserMessage(...)` bridge now preserves structured content blocks (including `image` blocks) across sidecar -> host action dispatch.
- Session metadata/state action parity for extension commands (`appendEntry`, `setSessionName`, `setLabel`).
- Active-tool control parity path (`setActiveTools`) with host-side context filtering and inactive-tool enforcement.
- Session-control command-context parity path (`newSession`, `switchSession`, `fork`, `navigateTree`) in CLI mode.
- Session lifecycle hook parity path for extension-driven session control:
  - `session_before_switch`/`session_before_fork`/`session_before_tree` cancel handling in host runtime.
  - `session_switch`/`session_fork`/`session_tree` notifications emitted after state transitions.
- Session lineage parity: session header `parentSession` now follows upstream path semantics (session file path, not session ID).
- `newSession({ setup })` parity path now serializes setup-callback SessionManager mutations in sidecar and replays them in host after session creation.
- Command-context cancellation parity now uses sidecar-local `session_before_*` evaluation before dispatching host actions, so command handlers receive upstream-style `{ cancelled: true }` results.
- Sidecar command-context session field sync from host `session_start` events.
- `session_before_tree` summary overrides now bridge end-to-end from sidecar command hooks into host session persistence (`branch_summary` entries).
- Sidecar now maintains a synchronized local session mirror sufficient for upstream-style `ReadonlySessionManager` methods in extension contexts.
- Sidecar session mirror now applies compaction entries incrementally from `session_compact` events (not just message events), so `ctx.sessionManager` stays in sync after programmatic compaction.
- Sidecar now receives host context snapshots for extension `ctx` model surfaces (`ctx.model`, `ctx.getSystemPrompt`, `ctx.isIdle`, `ctx.hasPendingMessages`) across initialize/command/event paths.
- Sidecar `ctx.modelRegistry` now includes common upstream methods (`find`, `getAvailable`, `getApiKey`, `getApiKeyForProvider`) backed by host model snapshots and provider API-key sync.
- Sidecar `ctx.modelRegistry.isUsingOAuth(model)` now reflects host auth-mode snapshots (`oauth` vs `api_key`) instead of a hardcoded false fallback.
- Sidecar now receives host context-usage snapshots for `ctx.getContextUsage()` and keeps post-compaction token visibility aligned with upstream semantics (unknown until next assistant usage).
- Sidecar `ctx.compact({ customInstructions })` now bridges to a host `compact` action path that persists `compaction` entries and emits `session_compact`.
- Compaction hook parity now includes `session_before_compact` evaluation (cancel/custom compaction override) before host summary generation.
- Custom compaction/branch-summary `details` payloads from extension hooks are now preserved in persisted session entries (`compaction.details`, `branch_summary.details`).
- Runtime close now emits `session_shutdown` to sidecar for upstream lifecycle parity.
- Session context reconstruction now honors persisted `compaction.firstKeptEntryId` semantics so compaction entries actually reduce pre-compaction message replay.
- Compatibility shims for upstream extension APIs commonly used by extensions:
  - `pi.exec(...)` (spawn + stdout/stderr/code/killed result)
  - `pi.registerMessageRenderer(...)` (no-op in CLI mode)
- Sidecar-local extension event bus parity (`events.on/emit`) for extension-to-extension signaling.

Still intentionally deferred:
- Full interactive UI context parity and cross-process/runtime event bus parity.
- Upstream package-manager/discovery/install behavior for extensions.

## 7. RPC Parity Notes

Upstream reference:
- `packages/coding-agent/src/modes/rpc/rpc-mode.ts`
- `packages/coding-agent/src/modes/rpc/rpc-types.ts`
- `packages/coding-agent/docs/rpc.md`

Key protocol findings:
- Command transport is newline-delimited JSON on stdin.
- Responses use `{ type: "response", command, success, id?, data?/error? }`.
- Events are streamed continuously during agent activity.
- `prompt` is asynchronous; command loop must stay responsive while events stream.
- Queue behavior matters while streaming:
  - `streamingBehavior: "steer"` interrupts after current tool execution.
  - `streamingBehavior: "followUp"` appends a follow-up message.
- Queue delivery modes are explicit and runtime-configurable:
  - `all`
  - `one-at-a-time`
- RPC `export_html` returns a path and writes session HTML.
- Extension UI dialog flow is asynchronous over RPC:
  - runtime emits `extension_ui_request`
  - client replies with `extension_ui_response` (value/confirmed/cancelled)

Complications for Go implementation:
- Runtime needed a first-class event subscription surface independent of extension sidecar presence.
- Session control methods (`new/switch/fork/compact`) existed but were private; RPC needs exported API wrappers.
- Runtime queue dequeue logic was previously fixed to one-at-a-time, requiring mode-aware dequeue behavior.
- Bash command parity needed structured result fields (`output`, `exitCode`, `cancelled`, `truncated`) and abort hooks.
- `get_commands` parity required a native command catalog in Go:
  - extension commands from sidecar initialization metadata,
  - prompt templates from user/project default prompt directories,
  - skills from user/project default skills directories.

Closed parity gaps in current Go runtime:
- `export_html` is implemented and returns `{ path }`.
- `extension_ui_response` is now wired to sidecar UI request resolution (no longer ignored).
- `set_auto_retry` / `abort_retry` now control active retry behavior (bounded backoff + cancellation).
- `set_auto_compaction` now controls active auto-compaction behavior (threshold and overflow recovery paths).
