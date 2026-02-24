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

Excluded from this module:
- TUI
- Extension runtime and package ecosystem
- RPC mode dependency

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

Behavior:
- Running with no prompt now requires `--continue` (or piped stdin), so resume is explicit.
- `Ctrl+C` aborts the active run (provider call/tool execution) instead of waiting for timeout.
- `--continue` only runs when the current leaf ends in `user` or `toolResult`.
- CWD is normalized to an absolute path for session-directory isolation consistency.

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
