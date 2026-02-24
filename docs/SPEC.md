# Go Coding Agent Spec (Frozen Target: pi-mono@380236a0)

## 1. Customer Experience

Primary goal:
- Deliver a native Go coding agent executable that can replace core coding-agent usage for project workflows without requiring RPC or TUI.

Must-have user experience:
- Start or resume persistent sessions from disk.
- Prompt the agent in text mode.
- Agent can call coding tools and iterate until completion.
- Session tree semantics preserved (branching by parentId and resumable leaves).
- Provider/model selection available via CLI/config.

Explicitly out of scope for v1:
- TUI/interactive terminal UI parity.
- Prompt-template/skill package discovery.

Extension compatibility scope (phase 1):
- Node sidecar extension runtime is in scope.
- Sidecar protocol + lifecycle events are in scope.
- Extension tool registration/execution via sidecar is in scope.
- Extension event payload shape compatibility with upstream hook types is in scope (`message`, `toolCallId`, `input`, `args`, `result`, `partialResult`).
- Extension flag/command/provider registration surfaces are in scope.
- Extension command/event action bridge is in scope for core runtime actions (`sendUserMessage`, `sendMessage`, model/thinking updates).
- Extension action bridge includes session metadata/state actions (`appendEntry`, `setSessionName`, `setLabel`) and active-tool control (`setActiveTools`).
- Extension command-context session actions are in scope (`newSession`, `switchSession`, `fork`, `navigateTree`, `reload`, `waitForIdle`) with CLI-mode no-op behavior for unsupported interactive flows.
- Session lifecycle hook parity is in scope for command/session control flows (`session_before_switch`, `session_before_fork`, `session_before_tree` cancellation and `session_switch`, `session_fork`, `session_tree` notifications).
- Sidecar command-context session field sync (`sessionId`, `sessionFile`, `sessionName`) from host `session_start` events is in scope.
- Sidecar-local extension event bus parity (`events.on/emit`) is in scope.
- Extension tools may override built-in tool names.
- `model_select` lifecycle emission on explicit model changes is in scope.
- Upstream JS package-manager/discovery/install behavior remains out of scope.

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
- internal/extensionsidecar: Node sidecar transport/client.
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
- Steering queue support (interrupt after current tool execution, skip remaining tools in turn).
- Follow-up queue support (continue with queued user message after agent would otherwise stop).

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
- --continue
- --system-prompt
- --extension-sidecar-command
- --extension-sidecar-arg (repeatable)
- --extension (repeatable)

Modes:
- Text output default.
- JSON output optional.

## 9. Validation

Required validation before calling complete:
- `go test ./...` passes.
- Create session, prompt once, confirm JSONL persisted.
- Resume same session and continue branch.
- Verify at least one tool call path end-to-end.
- Verify node-sidecar extension receives upstream-style event fields (`toolCallId`, `input`, `message`).
- Verify extension tool override path for built-in tool names.
- Verify extension command `pi.sendUserMessage(...)` triggers a native Go agent turn.
- Verify extension command metadata actions persist to session (`appendEntry`, `setSessionName`).
- Verify extension active-tool updates change provider tool context and enforce inactive-tool errors.
- Verify extension command session-control actions (`newsession`, `switchsession`, `forkat`, `navigate`, `reloadcmd`, `waitcmd`) execute end-to-end.
- Verify session lifecycle hook cancellation path for extension-driven session actions and confirm session state remains unchanged on cancel.
- Verify host `session_start` refresh updates sidecar command-context session fields after `newSession`/`switchSession`/`fork`.
- Verify `navigateTree` option payload (`summarize`, `customInstructions`, `replaceInstructions`, `label`) reaches `session_before_tree`.
- Verify sidecar event bus `events.on/emit` works across commands.
- Provider smoke checks for configured providers.

## 10. Non-Goals

- TUI rendering.
- Pi package manager parity.
- npm/git extension installation and package-source management.
