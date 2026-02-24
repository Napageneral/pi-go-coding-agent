# Go Rewrite Research (Frozen Target: pi-mono@380236a0)

## 1. Customer Experience First

Desired UX for your projects:
- Native Go binary.
- No RPC dependency.
- Persistent sessions with branchable history.
- Reliable tool-call loop for coding workflows.
- Provider/model selection at runtime.

What you explicitly do **not** need for this phase:
- TUI.
- Upstream package-manager/discovery/install behavior for extensions.

What is now in scope:
- Node sidecar runtime for extensions.
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
- Session metadata/state action parity for extension commands (`appendEntry`, `setSessionName`, `setLabel`).
- Active-tool control parity path (`setActiveTools`) with host-side context filtering and inactive-tool enforcement.
- Session-control command-context parity path (`newSession`, `switchSession`, `fork`, `navigateTree`) in CLI mode.
- Session lifecycle hook parity path for extension-driven session control:
  - `session_before_switch`/`session_before_fork`/`session_before_tree` cancel handling in host runtime.
  - `session_switch`/`session_fork`/`session_tree` notifications emitted after state transitions.
- Sidecar command-context session field sync from host `session_start` events.
- Compatibility shims for upstream extension APIs commonly used by extensions:
  - `pi.exec(...)` (spawn + stdout/stderr/code/killed result)
  - `pi.registerMessageRenderer(...)` (no-op in CLI mode)
- Sidecar-local extension event bus parity (`events.on/emit`) for extension-to-extension signaling.

Still intentionally deferred:
- Full interactive UI context parity and cross-process/runtime event bus parity.
- Upstream package-manager/discovery/install behavior for extensions.
