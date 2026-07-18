# ROADMAP.md

What's next, what's off the table, and what's still up in the air for minimal-agent.

## Planned

* [ ] **Better tool definitions**

Glob, partial read, plan, and whatever else makes it a sharper agent. Easy to implement and likely a big improvement.

* [x] **Basic flow control**

Max tool-call retries and cycle detection implemented as simple counters in the agent loop. Configurable via `max_tool_rounds` (default 50) and `max_repeat_calls` (default 3) in settings.json or `/config`.

## Never gonna happen

* [ ] **Checkpoint / code reversion**

Too heavy and breaks "minimal." The whole point is building an agent without frameworks like CrewAI (they suck the fun out). History is just a JSON file — edit it to roll back. 😅

* [ ] **Customizable keybindings**

My project, my keybindings. No reason to make it configurable — the codebase is dead simple to hack on anyway.

* [ ] **LSP integration**

LSP is stateful, heavy, and fundamentally at odds with the agent's one-shot tool model. Per-language skills (`skill go`, `skill rust`, etc.) can cover code navigation and diagnostics without dragging in a full language server.

* [ ] **Auto memory / vector recall**

Implicit "learning" across sessions almost always degrades the experience — the agent remembers the wrong things or drifts over time. Explicit memory is better: write what you want it to know in `AGENTS.md` (or a `.local` variant you keep out of git).

* [ ] **RAG**

Vector DBs, embeddings, chunking — it's a whole infrastructure layer for answering questions over document corpora. This agent already has `read` and `rg`; it doesn't need a retrieval pipeline to work on a codebase it's standing in.

## Not decided yet

* [ ] **System reminder / injection** (parked)

Inject extra context into user messages — project-level reminders, hints, and nudges.

**Parked because:** every useful reminder type depends on a feature that doesn't exist yet.
The one genuinely useful case — proactive file-modification detection — is already handled
reactively (the edit tool refuses stale edits and forces a re-read). TodoWrite reminders need
a task tool; plan-mode reminders need a plan tool; IDE diagnostics need IDE integration
(explicitly "never gonna happen"). System reminders are glue *for other features* — they're
infrastructure without a payload until those features land. Revisit when plan mode or a
todo/task tool is on the table. Claude Code's own implementation (15+ injection types, infinite
loops, 15%+ context window consumed by hidden text, model confusing reminders with user input)
is also a cautionary tale on complexity.

* [ ] **Sub-agents**

Good for keeping context from blowing up, but they'd wreck the "minimal" premise.

* [ ] **WebSocket / Unix socket**

Would unlock IDE integration and headless mode, but it's a big lift and needs a stable protocol. This project moves too fast for that (it's a PoC playground at heart) — fork it and wire up what you need.
