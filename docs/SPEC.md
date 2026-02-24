# Go Coding Agent Spec (Frozen Target: pi-mono@380236a0)

## 1. Customer Experience

Primary goal:
- Deliver a native Go coding agent executable that can replace core coding-agent usage for project workflows without requiring RPC, TUI, or JS extensions.

Must-have user experience:
- Start or resume persistent sessions from disk.
- Prompt the agent in text mode.
- Agent can call coding tools and iterate until completion.
- Session tree semantics preserved (branching by parentId and resumable leaves).
- Provider/model selection available via CLI/config.

Explicitly out of scope for v1:
- TUI/interactive terminal UI parity.
- JS extension runtime and package ecosystem.
- Prompt-template/skill/extension package discovery.

## 2. Frozen Scope & Compatibility

Target baseline:
- Repository: badlogic/pi-mono
- Frozen commit: 380236a003ec7f0e69f54463b0f00b3118d78f3c (2026-02-23)

Port targets:
- coding-agent core runtime responsibilities.
- session-manager JSONL persistence semantics.
- model resolution/auth loading needed for runtime.
- provider execution layer sufficient for coding-agent operation.
- built-in coding tools (read/write/edit/bash/ls/find/grep).

## 3. Architecture

Module:
- go-coding-agent (Go module)

Packages:
- internal/types: shared message/tool/provider/session types.
- internal/session: JSONL session file format, migration/versioning, tree operations, context reconstruction.
- internal/tools: built-in tool implementations.
- internal/providers: provider adapters + model/provider resolution.
- internal/agent: runtime loop (LLM call -> tool calls -> tool results -> continue).
- internal/config: settings/auth/model config loading.
- cmd/pi-go: CLI entrypoint (text mode).

## 4. Session Manager Requirements

Storage:
- Session file JSONL, header + entries.
- Entry graph via id/parentId.
- Default session storage isolated per cwd (`~/.pi/agent/sessions/--<cwd-encoded>--`).

Operations:
- Create new session.
- Resume existing session.
- Append message entries.
- Append model/thinking-level changes.
- Append custom summary entries for compaction/branch summary (data-preserving no-op if not used).
- Branch navigation by selecting a leaf and reconstructing lineage to root.
- Read metadata: session id, file path, cwd, modified timestamp.

Compatibility decisions:
- Keep CURRENT_SESSION_VERSION = 3 semantics in Go for new files.
- Accept older files when possible; best-effort migration for missing id/parentId.

## 5. Agent Runtime Requirements

Prompt flow:
1. Add user message to active branch.
2. Build context from branch lineage.
3. Call selected provider/model.
4. If assistant returns tool calls, execute tools, append tool result messages, continue.
5. Stop when assistant returns no tool call or error.
6. Persist every message event to session.

Control:
- Abort current run via signal.
- Resume previous session.
- Continue mode only when current leaf ends in `user`/`toolResult` (rejects dangling assistant leaf).

## 6. Tools Requirements

Required tools:
- read, write, edit, bash, ls, find, grep.

Behavior goals:
- Deterministic structured results.
- Reasonable truncation for very large outputs.
- Safe path normalization with cwd scoping.

## 7. Providers & Models (v1 parity focus)

Implement provider adapters for:
- OpenAI-compatible chat-completions class.
- OpenAI responses family:
  - `openai-responses`
  - `openai-codex-responses`
  - `azure-openai-responses`
- Anthropic messages.
- Google provider family:
  - Generative AI API.
  - Cloud Code Assist (`google-gemini-cli` / `google-antigravity`) envelope.
  - Vertex endpoint path.
- Amazon Bedrock converse.

Model/provider resolution:
- Provider + model via CLI flags.
- Config-driven base URL/API key override.
- Default model table equivalent to coding-agent defaults where available.
- Cross-provider replay normalization for tool-call IDs and orphan tool results.

Known limitation policy:
- If a provider’s advanced edge-case feature from TS is not yet ported, return explicit actionable error rather than silent fallback.

## 8. CLI Requirements

Command:
- pi-go [message]

Flags:
- --provider
- --model
- --api-key
- --session
- --session-dir
- --no-session
- --cwd
- --json

Modes:
- Text output default.
- JSON output optional.

## 9. Validation

Required validation before calling complete:
- `go test ./...` passes.
- Create session, prompt once, confirm JSONL persisted.
- Resume same session and continue branch.
- Verify at least one tool call path end-to-end.
- Provider smoke checks for configured providers.

## 10. Non-Goals

- TUI rendering.
- Extension event system.
- Pi package manager.
- npm/git extension installation.
