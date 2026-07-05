# AGENTS.md

You are working on **minimal-agent**, a Go-based CLI coding agent. This file
describes the codebase conventions and architecture so you can contribute
effectively.

## Architecture

Single-file Go program (`main.go`) with no internal packages. Keep it this
way — no sub-packages unless there is a compelling reason.

- **LLM client**: openai-go SDK (`github.com/openai/openai-go`)
- **Agent loop**: read user input → append to history → stream response →
  execute tool calls → repeat
- **Tools**: `bash`, `read`, `write`, `edit` — all defined in `main()`
- **Session persistence**: history stored as JSON arrays under `.ma-sessions/`;
  auto-save on each turn and on exit; auto-resume on startup

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

Tests are not yet present. When adding tests, use the standard library `testing`
package and place them in `main_test.go`.

## Design principles

- **Minimal**: one binary, no frameworks, minimal dependencies (only openai-go SDK)
- **Dogfooding**: the agent writes its own code — all commits after the first
  were authored by the agent itself
- **Streaming-first**: responses stream over SSE; the accumulator pattern gathers
  chunks and converts to a final message for history
