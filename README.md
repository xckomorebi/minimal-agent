# minimal-agent

A **hobby project**, purely vibe-coded.

The first commit was written by **Claude Code**. Every commit since has been
written by the agent itself — dogfooding all the way down.

## What it is

A Go-based CLI coding agent with a full-screen TUI, built on the
[openai-go](https://github.com/openai/openai-go) SDK and
[Bubble Tea](https://github.com/charmbracelet/bubbletea). It has an OpenAI
Chat Completions tool-calling loop — the agent calls the API (streaming by default,
configurable to non-streaming), calls tools, and can inspect, build, and rewrite itself.

**Tools:** `bash`, `read`, `write`, `edit`, `web-search`, `web-fetch`, `skill`, `ask_user_question`, plus any tools exposed by [MCP servers](#mcp-support)

**Features:**
- Full-screen TUI with markdown rendering, viewport scrolling, and colored diff previews
- Streaming responses with reasoning/thinking support (collapsible, expand with `Ctrl-O`)
- Session persistence (auto-save on each turn, auto-resume on startup)
- Approval flow: state-changing tools prompt `[y/N]` before executing
- Skill system: reusable instruction sets in `~/.agents/skills/<name>/SKILL.md`, indexed at startup, loaded on demand via the `skill` tool
- Context window management: token tracking, `/context` display, `/compact` summarization
- Global config file with hot-reload (`~/.ma/settings.json`)
- Slash commands with tab-autocomplete and an interactive session picker

## Run

**Install:**

```sh
go install github.com/xckomorebi/minimal-agent@latest
```

**Or build from source:**

```sh
git clone https://github.com/xckomorebi/minimal-agent.git
cd minimal-agent
go build -o minimal-agent .
MA_API_KEY=sk-... ./minimal-agent
```

Requires **Go 1.25+**.

Set your API key in `~/.ma/settings.json` (see [Configuration](#configuration)
below), or via the `MA_API_KEY` environment variable.

## Slash commands

| Command | Description |
|---|---|
| `/save [name]` | Save the current session; optionally rename |
| `/resume <name>` | Load a saved session (or just `/resume` for picker) |
| `/new-session [name]` | Start a fresh session |
| `/list-session` | List all saved sessions with summaries |
| `/re-summarize` | Regenerate the session title |
| `/config` | Show current configuration and sources |
| `/config <key> [value]` | Get/set: `model`, `auto-edit`, `thinking`, `thinking-effort`, `thinking-detail`, `context-window`, `stream` |
| `/context` | Show token usage vs. context window |
| `/compact` | Summarize conversation to free context space |
| `/model <id>` | Shorthand for `/config model` |
| `/thinking` | Toggle thinking on/off |
| `/effort <low\|medium\|high>` | Set reasoning effort |
| `/help [command]` | List commands or get detailed help |

## Configuration

Priority (highest to lowest): **CLI flags > session config > `~/.ma/settings.json` > environment variables**.

### CLI flags

| Flag | Env | Default |
|---|---|---|
| `-ma-api-key` | `MA_API_KEY` | (required) |
| `-url` | `MA_BASE_URL` | `https://api.openai.com/v1` |
| `-model` | `MA_MODEL` | `gpt-4o` |
| `-session` | `MA_SESSION` | auto-resume latest |
| `-new` | — | `false` |
| `-context-window` | `MA_CONTEXT_WINDOW` | `200000` |

`-session` selects a named session to load or create. `-new` forces a fresh
timestamped session, ignoring any existing sessions or `-session` flag.

### Global config file (`~/.ma/settings.json`)

A JSON file that is **watched via fsnotify** — edit it and the agent picks up
new settings immediately. All keys are optional:

```json
{
  "api_key": "sk-...",
  "base_url": "https://api.openai.com/v1",
  "model": "gpt-4o",
  "thinking": true,
  "thinking_effort": "medium",
  "thinking_detail": false,
  "auto_edit": false,
  "context_window": 200000,
  "stream": true
}
```

- `thinking_detail`: when `false` (default), thinking streams in a rolling 10-line
  window and collapses to "thought about it" when done. When `true`, the full
  thinking text is expanded. Toggle at runtime with `Ctrl-O` or `/config thinking-detail`.
- `context_window`: the token budget for the conversation. When usage approaches
  this limit, use `/compact` to summarize and free space.
- `stream`: when `true` (default), responses stream token-by-token via SSE.
  Set to `false` for a single non‑streaming request.

### Session config

Use `/config` inside the agent to view or toggle per-session settings:

- `/config` — show current settings and their sources
- `/config thinking` — toggle thinking on/off for this session
- `/config thinking-effort low|medium|high` — set reasoning effort
- `/config thinking-detail` — toggle collapsed/expanded thinking display
- `/config auto-edit` — toggle auto-approve for write/edit
- `/config model <id>` — switch models mid-session
- `/config context-window <tokens>` — change the context window
- `/config stream` — toggle between streaming and non‑streaming API calls

The `-url` value is the API base (must include `/v1`); the client appends
`/chat/completions`. Point it at any OpenAI-compatible gateway:

```sh
go run . -url https://my-gateway.example.com/v1 -model gpt-4o
```

## How it works

1. Read input from the TUI textarea. If it starts with `/`, handle as a slash
   command; otherwise treat as a user message appended to the conversation.
2. POST the history + tool schemas to `/chat/completions`. By default this uses
   streaming (SSE); set `"stream": false` in the config to switch to a single
   non-streaming request.
3. Stream the assistant text as it arrives, rendering markdown in the viewport.
   Reasoning/thinking content streams in dim italic alongside the response.
4. For each `tool_calls` entry, display the tool name and details. State-changing
   tools (`bash` with destructive commands, `write`, `edit`) prompt for approval
   (showing a unified diff for write/edit). Read-only tools run immediately.
5. Feed each tool result back as a `role: "tool"` message.
6. Repeat until the model returns a message with no tool calls.
7. The session auto-saves to `.ma/sessions/<name>.json` after each turn.

The system prompt is built dynamically at startup, injecting the current working
directory, git branch, the contents of `AGENTS.md` (if present), and an index of
available skills — so the agent always knows what project it's working in.

## Skills

Skills are reusable, on-demand instruction sets stored in `~/.agents/skills/`.
Each skill is a subdirectory with a `SKILL.md` file containing YAML frontmatter
(`name`, `description`) and markdown instructions.

```
~/.agents/skills/
  git-commit/
    SKILL.md    ← YAML frontmatter + instructions
  expr-config/
    SKILL.md
```

At startup, the agent scans the skills directory and builds an index from the
frontmatter `description` fields. The index is included in the system prompt so
the model knows what's available. Use the `skill("name")` tool to load a skill's
full instructions on demand — they're injected as a tool result and the agent
follows them from that point on. `skill("list")` enumerates all available skills.

## MCP (Model Context Protocol)

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

- **stdio** (`command` + `args`): spawns the server as a subprocess and communicates over stdin/stdout.
- **streamable HTTP** (`url`): connects to an already-running MCP server over HTTP (MCP 2025-03-26 streamable transport).

MCP tools are namespaced as `mcp.<server>.<tool>` (e.g. `mcp.filesystem.read_file`)
and require user approval by default since they come from external sources.

## Keyboard shortcuts

| Key | Action |
|---|---|
| `Enter` | Submit message |
| `Tab` | Trigger autocomplete (slash commands only) |
| `Ctrl-C` | Cancel current agent turn (or quit when idle) |
| `Ctrl-O` | Toggle expanded/collapsed thinking display |
| `↑↓` / `PgUp` / `PgDn` | Scroll viewport |

## Directory layout

```
.ma/sessions/          — session JSON files (auto-created)
~/.ma/settings.json    — global config (optional, hot-reloaded)
~/.agents/skills/      — skill files (optional, indexed at startup)
go.mod / go.sum        — Go module dependencies
*.go                   — single package main (no sub-packages)
```
