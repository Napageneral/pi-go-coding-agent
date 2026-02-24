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
- JS extension/package ecosystem.
- Package-manager/runtime bootstrap for extension tools.

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
