# go-coding-agent

Native Go port of the `pi` coding-agent core (frozen target: `pi-mono@380236a0`) focused on non-TUI workflows.

Upstream reference: [`badlogic/pi-mono`](https://github.com/badlogic/pi-mono)

Important: this repository is a partial Go port for coding-agent workflows. It does not attempt to encompass the entire original `pi-mono` feature surface.

## Scope

Included:
- Session manager with JSONL persistence and tree lineage (`id`/`parentId`)
- Core agent loop (LLM call -> tool calls -> tool results -> continue)
- Built-in tools: `read`, `write`, `edit`, `bash`, `ls`, `find`, `grep`
- Provider adapters: OpenAI-compatible, Anthropic, Google, Bedrock (tool-calling supported)
- CLI text/json mode
- RPC mode (`--mode rpc`) with newline-delimited JSON command/response/event protocol
  - includes `export_html` path export
  - includes extension UI request/response bridge (`extension_ui_request` / `extension_ui_response`)
  - includes runtime-backed auto-retry and auto-compaction controls (`set_auto_retry`, `abort_retry`, `set_auto_compaction`)

Excluded from this module:
- TUI
- Upstream extension package manager/discovery/install behavior

Partially included:
- Node extension sidecar runtime (protocol + event/tool bridge, not full upstream extension/package parity)
- Upstream-style extension event payload fields for core lifecycle/tool hooks
- Extension tool override path for built-in tool names
- Extension action bridge for command/event driven messaging (`pi.sendUserMessage(...)`), including structured content blocks (`text`, `image`)
- Extension action bridge for session metadata/state (`appendEntry`, `setSessionName`, `setLabel`) and active-tool updates
- Command-context session control bridge (`newSession`, `switchSession`, `fork`, `navigateTree`) and sidecar-local `events.on/emit`
- Session lineage semantics aligned with upstream (`parentSession` stored as session file path when provided)
- `newSession({ setup })` setup-callback bridge (serialized setup operations replayed by host in the new session)
- Session lifecycle hook parity path for `session_before_*` cancellation (`switch`, `fork`, `tree`) plus `session_switch`/`session_fork`/`session_tree` emissions and `session_shutdown` on runtime close
- Sidecar command-context session field sync (`sessionId`/`sessionFile`/`sessionName`) via host `session_start` updates
- Extension API compatibility shims for `registerMessageRenderer` (CLI no-op) and `exec(...)`
- Command-context session methods now return upstream-style cancellation semantics when `session_before_*` handlers cancel (`{ cancelled: true }`)
- Sidecar-local `ReadonlySessionManager` compatibility surface (`getCwd/getSessionDir/getSessionId/getSessionFile/getLeafId/getLeafEntry/getEntry/getLabel/getBranch/getHeader/getEntries/getTree/getSessionName`)
- Sidecar `ctx` model/context compatibility surface (`ctx.model`, `ctx.modelRegistry`, `ctx.getSystemPrompt`, `ctx.isIdle`, `ctx.hasPendingMessages`)
- Sidecar `ctx.modelRegistry` compatibility methods for common extensions (`find`, `getAvailable`, `getApiKey`, `getApiKeyForProvider`, `isUsingOAuth`)
- Sidecar `ctx.getContextUsage()` compatibility snapshot (tokens/contextWindow/percent with post-compaction unknown-token behavior)
- Sidecar `ctx.compact({ customInstructions })` bridge to host compaction action (`compact`), persisting `compaction` entries and emitting `session_compact`
- Compaction lifecycle hook parity path for `session_before_compact` cancel/custom override handling

## Build

```bash
cd go-coding-agent
go build ./cmd/pi-go
```

## Run

```bash
cd go-coding-agent
go run ./cmd/pi-go --provider anthropic --model claude-opus-4-6 "Summarize this repo"
```

Flags:
- `--mode` (`text` default, `rpc` for headless JSON protocol)
- `--provider`
- `--model`
- `--api-key`
- `--session`
- `--session-dir`
- `--no-session`
- `--cwd`
- `--json`
- `--continue`
- `--system-prompt`
- `--extension-sidecar-command`
- `--extension-sidecar-arg` (repeatable)
- `--extension` (repeatable extension module path)
- Unknown `--<name>` flags are forwarded as extension flag values to the sidecar.

Behavior:
- Running with no prompt now requires `--continue` (or piped stdin), so resume is explicit.
- `Ctrl+C` aborts the active run (provider call/tool execution) instead of waiting for timeout.
- `--continue` only runs when the current leaf ends in `user` or `toolResult`.
- CWD is normalized to an absolute path for session-directory isolation consistency.
- Runtime API supports queued `Steer(...)` and `FollowUp(...)` messages (pi-agent-like turn control semantics).
- Prompt text beginning with `/` is treated as an extension command when registered by sidecar.
- Runtime emits `model_select` when model is changed through runtime/CLI pathways.
- Auto-retry and auto-compaction toggles in RPC mode now control active runtime behavior (not just stored flags).

RPC mode:

```bash
go run ./cmd/pi-go --mode rpc --provider openai --model gpt-5.1-codex
```

- Send one JSON command per line on stdin.
- Responses are JSON objects with `type: "response"`, `command`, `success`, optional `id`, and optional `data`/`error`.
- Runtime lifecycle/tool/session events are streamed as JSON lines on stdout.
- `get_commands` returns extension commands plus local prompt/skill commands discovered from:
  - `~/.pi/agent/prompts`, `<cwd>/.pi/prompts`
  - `~/.pi/agent/skills`, `<cwd>/.pi/skills`

Node sidecar extension runtime:

```bash
go run ./cmd/pi-go \
  --extension-sidecar-command node \
  --extension-sidecar-arg /Users/tyler/nexus/home/projects/pi-go-coding-agent/sidecar/node-extension-runtime/main.mjs \
  --extension /absolute/path/to/my-extension.mjs \
  "Run extension-enabled prompt"
```

## Config Paths

Default config paths mirror `~/.pi/agent`:
- `auth.json`
- `models.json`
- `sessions/--<cwd-encoded>--/`

`PI_CODING_AGENT_DIR` can override the agent directory.

## Validation

```bash
go test ./...
```

Current test coverage includes:
- Session create/append/reopen/context reconstruction
- End-to-end behavior of core file/search tools
- OpenAI-family provider normalization and response parsing (chat + responses/codex paths)

See [`docs/RESEARCH.md`](docs/RESEARCH.md) for migration coupling/complication notes and dependency rationale.
See [`docs/EXTENSION_SIDECAR_SPEC.md`](docs/EXTENSION_SIDECAR_SPEC.md) for the Go host <-> Node sidecar protocol contract.

## Notes

- OpenAI-compatible path is used for multiple providers that expose OpenAI-style APIs.
- Cross-provider history replay includes tool-call ID normalization and synthetic tool-result fill-ins for orphaned tool calls.
- OpenAI API-family dispatch is API-specific:
  - `openai-completions` -> `/chat/completions`
  - `openai-responses` -> `/responses`
  - `openai-codex-responses` -> `/codex/responses` (ChatGPT backend + account header extraction)
  - `azure-openai-responses` -> Azure Responses endpoint (deployment mapping via `AZURE_OPENAI_DEPLOYMENT_NAME_MAP`)
- `google-gemini-cli` expects OAuth credentials shaped as JSON `{ \"token\": \"...\", \"projectId\": \"...\" }` (mirrors auth storage behavior).
- `google-vertex` uses `GOOGLE_CLOUD_PROJECT` + `GOOGLE_CLOUD_LOCATION` and either `GOOGLE_VERTEX_ACCESS_TOKEN` or `gcloud auth ... print-access-token`.
- Provider-level `models.json` overrides (`baseUrl`, `api`) are applied to built-in models.
