# TODO.md

1. **Editing experience** — undo after applying edits.
2. **UI improvement** — syntax highlighting in code blocks.
3. **External tools** — let the agent run user-defined scripts or call web APIs.
4. **Skills** — reusable prompt templates for common tasks like code review.
5. **Auto-approve mode** — a `-y` flag to skip confirmation prompts.
6. **Context awareness** — auto-detect project type and respect `.gitignore`.
7. ~~**Session history** — save and reload past conversations.~~ ✓ done: `.ma-sessions/` with auto-save, auto-resume, picker, and summarization
8. ~~**Configuration file** — a simple config file for API keys, model, and preferences.~~ ✓ done: `~/.ma/settings.json` with hot-reload via fsnotify
9. ~~**Diff before edit** — show a unified diff before applying write/edit tool calls.~~ ✓ done: colored unified diff in approval preview
10. ~~**Context window management** — token tracking, compaction, `/context` display.~~ ✓ done
