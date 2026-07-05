# minimal runnable agent

A **hobby project**, purely vibe-coded.

The first commit was written by **Claude Code**. Every commit since has been
written by the agent itself — dogfooding all the way down.

## What it is

A Go-based CLI coding agent with an OpenAI Chat Completions tool-calling loop,
built on the official [openai-go](https://github.com/openai/openai-go) SDK.

**Tools:** `bash`, `read`, `write`, `edit` — enough to inspect, build, and
rewrite itself.

## Run

```sh
export MA_API_KEY=sk-...
go run .
```

Type a request at the `you>` prompt. `Ctrl-D`, `exit`, or `quit` to leave.

## Configuration

Priority (highest to lowest): **CLI flags > session config > `~/.ma/settings.json` > environment variables**.

### CLI flags

| Flag          | Env           | Default                     |
| ------------- | ------------- | --------------------------- |
| `-ma-api-key` | `MA_API_KEY`  | (required)                  |
| `-url`        | `MA_BASE_URL` | `https://api.openai.com/v1` |
| `-model`      | `MA_MODEL`    | `gpt-4o`                    |
| `-session`    | `MA_SESSION`  | auto-resume latest          |
| `-new`        | —             | `false`                     |

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
  "auto_edit": false
}
```

### Session config

Use `/config` inside the agent to view or toggle per-session settings:

- `/config` — show current settings and their sources
- `/config thinking` — toggle thinking on/off for this session
- `/config thinking-effort low|medium|high` — set reasoning effort
- `/config auto-edit` — toggle auto-approve for write/edit

The `-url` value is the API base (must include `/v1`); the client appends
`/chat/completions`. Point it at any OpenAI-compatible gateway:

```sh
go run . -url https://my-gateway.example.com/v1 -model gpt-4o
```

## How it works

1. Read input from stdin (supporting `\` line continuation for multi-line
   input), append it to the conversation as a user message.
2. POST the history + tool schemas to `/chat/completions` with streaming.
3. Print the assistant text as it arrives; for each `tool_calls` entry, run the
   tool and feed a `role: "tool"` message back.
4. State-changing tools (`bash` with destructive commands, `write`, `edit`)
   prompt for user approval before executing.
5. Repeat until the model returns a message with no tool calls.

The system prompt is built dynamically at startup, injecting the current working
directory, git branch, and the contents of `AGENTS.md` (if present) — so the
agent always knows what project it's working in.

Global settings live in `~/.ma/settings.json`. The file is watched via fsnotify:
edit it and changes take effect immediately without restarting the agent.

The conversation is kept in memory (`agent.history`), so context carries across
turns for the life of the process.

## Roadmap

See [TODO.md](TODO.md) for planned features.
