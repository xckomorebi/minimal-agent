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
| `agent.go` | Agent struct, streaming/non-streaming loop, reasoning extraction, token tracking, summary gen, compaction |
| `commands.go` | Slash commands: `/save`, `/resume`, `/new-session`, `/list-session`, `/re-summarize`, `/config` (including `stream`), `/context`, `/compact`, `/model`, `/thinking`, `/effort`, `/profile`, `/help`, plus autocomplete |
| `config.go` | Global config file, fsnotify watcher, priority-chain resolution |
| `messages.go` | `cleanHistory`, `isEmptyMessage` |
| `session.go` | Session load/save/list, auto-resume, summary/token-usage persistence |
| `tools.go` | Tool def helpers, `builtinTools`, `allTools`, `runTool` dispatch, implementations (bash, read, write, edit, web-search, web-fetch, skill), skill index |
| `tui.go` | Bubble Tea TUI model, viewport, streaming display, approval, picker, autocomplete |
| `styles.go` | Lipgloss styles, markdown rendering, unified diff rendering, banner |
| `mcp.go` | MCP client: connect to servers (stdio or streamable HTTP), discover tools, convert to OpenAI format, proxy tool calls |

- **LLM client**: openai-go SDK (`github.com/openai/openai-go`)
- **MCP client**: official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`)
- **Agent loop**: read user input → append to history → send request (streaming or non‑streaming) →
  execute tool calls → repeat
- **Tools**: `bash`, `read`, `write`, `edit`, `web-search`, `web-fetch`, `skill` — defined in `main()`, implemented in `tools.go`
- **Session persistence**: history stored as JSON under `.ma/sessions/`;
  auto-save on each turn and on exit; auto-resume on startup
- **Global config**: `~/.ma/settings.json` (JSON, watched via fsnotify) —
  API key, base URL, model, thinking, effort level, thinking detail, auto-edit, context window, stream, custom HTTP headers

## Coding conventions

- Use `edit` over `write` when modifying existing files; `write` only for new files
- Tool call results always go through `openai.ToolMessage(result, call.ID)`
- State-changing operations (`write`, `edit`, destructive `bash`) require approval
- All rendering goes through the Bubble Tea TUI (`tui.go` + `styles.go`); no direct stdout printing
- Reasoning blocks stream in dim italic (rolling 10-line window by default); expand with Ctrl-O or `/config thinking-detail`
- System prompt is built dynamically in `buildSystemMessage()` — inject cwd,
  git branch, and this file's contents

## Build & run

```
go build -o minimal-agent .
MA_API_KEY=sk-... ./minimal-agent
```

## Configuration priority (highest to lowest)

1. CLI flags (`-ma-api-key`, `-url`, `-model`, `-profile`, `-session`, `-new`, `-context-window`)
2. Session config (`/config` commands, stored in `.ma/sessions/<name>.json`)
3. Global config file (`~/.ma/settings.json`, JSON, watched via fsnotify)
4. Environment variables (`MA_API_KEY`, `MA_BASE_URL`, `MA_MODEL`, `MA_CONTEXT_WINDOW`)

Settings configurable via `~/.ma/settings.json`:

```json
{
  "api_key": "sk-...",
  "base_url": "https://api.openai.com/v1",
  "model": "gpt-4o",
  "thinking": true,
  "thinking_effort": "medium",
  "thinking_detail": false,
  "send_reasoning": true,
  "auto_edit": false,
  "context_window": 200000,
  "stream": true,
  "extra_http_headers": {
    "X-Custom-Header": "value"
  },
  "profile": "deepseek",
  "profiles": {
    "deepseek": {
      "api_key": "sk-...",
      "base_url": "https://api.deepseek.com/v1",
      "model": "deepseek-chat"
    },
    "glm": {
      "api_key": "...",
      "base_url": "https://open.bigmodel.cn/api/paas/v4",
      "model": "glm-4-plus"
    }
  }
}
```

All keys are optional — unset keys fall through to the next priority level.

- `profile`: name of the profile to activate. When set, the corresponding
  entry in `profiles` is used as the first source for `api_key`, `base_url`,
  `model`, `thinking`, `thinking_effort`, `thinking_detail`, `send_reasoning`,
  `auto_edit`,
  `context_window`, `stream`, and `extra_http_headers`. Any field not set in
  the profile falls through to the top-level setting, then to lower layers.
  Can also be set per-invocation via `-profile` flag or switched at runtime
  via `/profile <name>`.
- `profiles`: map of named profile configs. Each profile can define any subset
  of the same fields as the top-level config (except `profiles` itself).

- `thinking_detail`: when `false` (default), thinking streams in a rolling 10-line
  window and collapses to "thought about it" when done. When `true`, the full
  thinking text is expanded in the output.
- `send_reasoning`: when `true` (default), `reasoning_content` from previous
  assistant messages is included in API requests so the model can reference its
  prior chain-of-thought. When `false`, reasoning is still persisted in session
  files and displayed in the TUI, but stripped from outgoing requests. Useful
  for providers that reject `reasoning_content` in input. Can also be toggled
  at runtime via `/config send-reasoning`.
- `stream`: when `true` (default), responses stream token-by-token over SSE.
  When `false`, responses arrive in a single non-streaming request.

## MCP (Model Context Protocol) Support

minimal-agent can connect to MCP servers to access external tools. Configure
servers in `~/.ma/settings.json` under the `mcp_servers` key:

```json
{
  "mcp_servers": [
    {
      "name": "filesystem",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    },
    {
      "name": "remote-api",
      "url": "http://localhost:3000/mcp"
    }
  ]
}
```

Two transport modes:

- **stdio** (set `command` + `args`): minimal-agent spawns the server as a
  subprocess and communicates over stdin/stdout
- **streamable HTTP** (set `url`): connects to an already-running MCP server
  over HTTP (supports the MCP 2025-03-26 streamable transport)

MCP tools are named `mcp.<server>.<tool>` (e.g., `mcp.filesystem.read_file`)
and require user approval by default (they come from external sources).

### Key implementation details

| File | What it does |
|---|---|
| `mcp.go` | `connectMCPServers()`, tool discovery, schema conversion, `runMCPTool()` proxy |
| `config.go` | `mcpServerConfig` type and `MCPServers` field in `globalConfig` |
| `agent.go` | `runToolCall()` routes `mcp.`-prefixed calls; `toolApprovalInfo()` requires approval for MCP tools |
| `main.go` | Calls `connectMCPServers()` at startup, `closeMCPServers()` on shutdown, adds MCP summary to system prompt |

Tests are not yet present. When adding tests, use the standard library `testing`
package and place them in `main_test.go`.

## Design principles

- **Minimal**: one binary, no frameworks, minimal dependencies (openai-go SDK + Bubble Tea + glamour + fsnotify)
- **Dogfooding**: the agent writes its own code — all commits after the first
  were authored by the agent itself
- **Streaming-first**: responses stream over SSE by default (configurable via `stream` setting); the accumulator pattern gathers chunks and converts to a final message for history
- **TUI**: full-screen terminal UI with viewport scrolling, markdown rendering, approval prompts, interactive session picker, and slash-command autocomplete
- **Frozen system prompt**: the system prompt (history[0]) is built once at
  session start and never modified mid-session. On resume, the saved system
  message is restored verbatim — it is not rebuilt from the current
  environment. This preserves prefix-cache validity across turns (same as
  Claude Code and Codex). Environment changes (shell, cwd, git branch) take
  effect on the next fresh session, not on resume.
