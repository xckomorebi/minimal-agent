# minimal runnable agent

A ~180-line Go agent: an OpenAI Chat Completions tool-calling loop with a single
`bash` tool, built on the official [openai-go](https://github.com/openai/openai-go) SDK.

## Run

```sh
export MA_API_KEY=sk-...
go run .
```

Type a request at the `you>` prompt. The model can run shell commands via the
`bash` tool (each command is echoed as `$ ...` before it runs). `Ctrl-D`, `exit`,
or `quit` to leave.

## Configuration

Flags override environment variables.

| What     | Env               | Flag       | Default                      |
| -------- | ----------------- | ---------- | ---------------------------- |
| API key  | `MA_API_KEY`  | `-ma-api-key` | (required)                   |
| Base URL | `MA_BASE_URL` | `-url`     | `https://api.openai.com/v1`  |
| Model    | `MA_MODEL` | `-model` | `gpt-4o` |

The `-url` value is the API base (must include `/v1`); the client appends
`/chat/completions`. Point it at any OpenAI-compatible gateway:

```sh
go run . -url https://my-gateway.example.com/v1 -model gpt-4o
```

## How it works

1. Read a line from stdin, append it to the conversation as a user message.
2. POST the history + tool schema to `/chat/completions`.
3. Print the assistant text; for each `tool_calls` entry, run the command and feed
   a `role: "tool"` message back.
4. Repeat until the model returns a message with no tool calls.

The whole conversation is kept in memory (`agent.history`), so context carries
across turns for the life of the process.
