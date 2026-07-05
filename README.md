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

Flags override environment variables.

| What     | Env           | Flag           | Default                     |
| -------- | ------------- | -------------- | --------------------------- |
| API key  | `MA_API_KEY`  | `-ma-api-key`  | (required)                  |
| Base URL | `MA_BASE_URL` | `-url`         | `https://api.openai.com/v1` |
| Model    | `MA_MODEL`    | `-model`       | `gpt-4o`                    |

Thinking mode is always on with effort `medium`. Reasoning content is rendered
in dim italic inline with the response.

The `-url` value is the API base (must include `/v1`); the client appends
`/chat/completions`. Point it at any OpenAI-compatible gateway:

```sh
go run . -url https://my-gateway.example.com/v1 -model gpt-4o
```

## How it works

1. Read a line from stdin, append it to the conversation as a user message.
2. POST the history + tool schemas to `/chat/completions` with streaming.
3. Print the assistant text as it arrives; for each `tool_calls` entry, run the
   tool and feed a `role: "tool"` message back.
4. State-changing tools (`bash` with destructive commands, `write`, `edit`)
   prompt for user approval before executing.
5. Repeat until the model returns a message with no tool calls.

The system prompt is built dynamically at startup, injecting the current working
directory, git branch, and the contents of `AGENTS.md` (if present) — so the
agent always knows what project it's working in.

The conversation is kept in memory (`agent.history`), so context carries across
turns for the life of the process.

## Roadmap

See [TODO.md](TODO.md) for planned features.
