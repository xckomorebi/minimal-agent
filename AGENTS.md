# AGENTS.md

You are working on **minimal-agent**, a Go-based CLI coding agent. This file
describes the codebase conventions and architecture so you can contribute
effectively.

## Architecture

Multi-file Go program, all in package `main`. No internal packages — keep it
this way unless there is a compelling reason.

| File | Responsibility |
|---|---|
| `main.go` | Entry point, CLI flags, tool definitions, system prompt, helper utils |
| `agent.go` | Agent struct, `runTurn` streaming loop, reasoning extraction |
| `commands.go` | `/save`, `/resume`, `/new-session`, `/list-session`, `/config` |
| `config.go` | Global config file, fsnotify watcher, priority-chain resolution |
| `messages.go` | `cleanHistory`, `isEmptyMessage` |
| `session.go` | Session load/save/list, auto-resume, `printHistory` |
| `tools.go` | Tool def helpers, `builtinTools`, `allTools`, `runTool` dispatch, `externalTools` placeholder, implementations |
| `ui.go` | ANSI helpers, banner, diff printing |

- **LLM client**: openai-go SDK (`github.com/openai/openai-go`)
- **Agent loop**: read user input → append to history → stream response →
  execute tool calls → repeat
- **Tools**: `bash`, `read`, `write`, `edit` — defined in `main()`, implemented in `tools.go`
- **Session persistence**: history stored as JSON under `.ma-sessions/`;
  auto-save on each turn and on exit; auto-resume on startup
- **Global config**: `~/.ma/settings.json` (JSON, watched via fsnotify) —
  API key, base URL, model, thinking, effort level, auto-edit

## Coding conventions

- Use `edit` over `write` when modifying existing files; `write` only for new files
- Tool call results always go through `openai.ToolMessage(result, call.ID)`
- State-changing operations (`write`, `edit`, destructive `bash`) require approval
- Print user-facing output with ANSI-styled prefixes (`you>`/`agent>`/tool dots)
- Reasoning blocks are printed in dim italic — extract them from raw SSE JSON
- System prompt is built dynamically in `buildSystemMessage()` — inject cwd,
  git branch, and this file's contents

## Build & run

```
go build -o minimal-agent .
MA_API_KEY=sk-... ./minimal-agent
```

## Configuration priority (highest to lowest)

1. CLI flags (`-ma-api-key`, `-url`, `-model`, `-session`, `-new`)
2. Session config (`/config` commands, stored in `.ma-sessions/<name>.json`)
3. Global config file (`~/.ma/settings.json`, JSON, watched via fsnotify)
4. Environment variables (`MA_API_KEY`, `MA_BASE_URL`, `MA_MODEL`)

Settings configurable via `~/.ma/settings.json`:

```json
{
  "api_key": "sk-...",
  "base_url": "https://api.openai.com/v1",
  "model": "gpt-4o",
  "thinking": true,
  "thinking_effort": "medium",
  "auto_edit": false
}
```

All keys are optional — unset keys fall through to the next priority level.

Tests are not yet present. When adding tests, use the standard library `testing`
package and place them in `main_test.go`.

## Design principles

- **Minimal**: one binary, no frameworks, minimal dependencies (openai-go SDK + fsnotify)
- **Dogfooding**: the agent writes its own code — all commits after the first
  were authored by the agent itself
- **Streaming-first**: responses stream over SSE; the accumulator pattern gathers
  chunks and converts to a final message for history
