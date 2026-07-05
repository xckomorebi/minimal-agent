# AGENTS.md

This is a minimal Go-based CLI coding agent. It uses:
- OpenAI-compatible Chat Completions API with streaming
- Tool calling: bash, read, write, edit
- Interactive approval prompts for state-changing operations

## Build
```
go build -o minimal-agent .
```

## Run
```
export MA_API_KEY=sk-...
./minimal-agent
```
