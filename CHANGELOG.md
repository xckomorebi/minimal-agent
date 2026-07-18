# Changelog

All notable changes to minimal-agent will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.2.0] - 2025-07-17

### Added

- **Flow control**: `max_tool_rounds` (default 50) caps LLM round-trips with tool calls per turn; `max_repeat_calls` (default 3) detects consecutive identical tool calls and stops runaway cycles. Both configurable via `~/.ma/settings.json` or `/config`.
- **Reasoning persistence**: `reasoning_content` from assistant messages is now persisted in session history and can be sent back to the API so the model references its prior chain-of-thought. Controlled by `send_reasoning` config (default `true`); toggle at runtime via `/config send-reasoning`.
- **Read tool offset/limit**: `read` tool now accepts `offset` and `limit` parameters for reading specific line ranges from large files. Parameter `path` renamed to `file_path`.

### Changed

- **Windows native support**: no longer requires WSL — runs natively on Windows with proper shell detection and path handling.
- ROADMAP.md: system reminder/injection (issue #3) moved from Planned to Not decided yet (parked until a dependency feature like plan mode or a todo tool lands).

### Fixed

- Context percentage inflated by accumulated streaming usage — now based on actual prompt token count.
- Denied tool call with a reason now continues the turn instead of stopping; denial without a reason stops the turn as before.
- Forward Ctrl-N/Ctrl-P to textarea for multi-line cursor navigation instead of being swallowed by the TUI.
- Resolved all gopls/staticcheck diagnostics.

---

## [v0.1.1] - 2025-07-17

### Changed

- **Transcript restyle**: replaced `you>`/`agent>` labels with a `❯` user glyph and a green gutter bar (`▎`) on agent output, so speaker is identified by color, not text.
- **GitHub-style bash approval**: commands requiring approval render in a dark code block with syntax highlighting (gruvbox theme via glamour). The block folds to a single outcome line after approve/deny.
- Inline code in markdown output no longer shows a background slab — foreground color only, for readability on light terminal themes.
- Tool result lines prefixed with an elbow connector (`└`) to visually attach to the tool-call line above.
- Streaming agent content now goes through the markdown renderer, eliminating the visible reflow/recolor when a turn ends.
- Collapsed thinking block shows word count (`N words · Ctrl-O to expand`).
- Hint bar now shows model name and context percentage when token tracking is available.

### Added

- **Input queuing during turns**: messages typed while the agent is running are queued and dispatched in order when the turn finishes. On error/cancel, queued input is restored to the text area (not auto-sent).
- **Double Ctrl-C to quit**: first Ctrl-C on an idle empty prompt shows "Ctrl-C again to quit"; only a second exits.
- **Escape** key now cancels a running turn (same as Ctrl-C).
- **Session picker enhancements**: vim-style `j`/`k` navigation and number keys `1-9` to directly pick a session.
- **Autocomplete padding**: items are width-padded so the row doesn't shift horizontally as the selection moves.
- Self-deprecating disclaimer in README about occasional human intervention.

### Fixed

- Cursor positions in inline inputs (approval reason, question "other" answer) now use rune indices instead of byte offsets, fixing movement and editing with multibyte text (CJK, emoji).
- Hint bar truncated to one row with `MaxWidth` to prevent layout breakage on narrow terminals.

### Housekeeping

- MCP SDK (`modelcontextprotocol/go-sdk`) promoted from indirect to direct dependency.

---

## [v0.1.0] - 2025-07-17

### Added

- **@file mention autocomplete** with fzf-style fuzzy matching — type `@` to search project files.
- **Profile support** for provider switching — define named profiles in `~/.ma/settings.json` and switch at runtime via `-profile` flag or `/profile` command.
- **Structured error logging** to a temp file for easier debugging.
- Global `~/.ma/AGENTS.md` injected into system prompt for user-level conventions.
- File modification times persisted in session JSON for cross-restart file tracking.
- Non-streaming API support, configurable via `stream` setting (defaults to streaming).
- `LICENSE` file (MIT).
- `CHANGELOG.md`.

### Changed

- Session storage renamed from `.ma-sessions` to `.ma/sessions`.
- Dropped redundant `session-` prefix from auto-generated session names.
- Improved session naming and picker UX.
- Cleaned up CLI flags and environment variable handling.
- @mention file references use content arrays instead of string concatenation.
- Faster braille spinner animation with separate dot cycling cadence.
- Blinking yellow dot indicator for running tool calls.

### Fixed

- Reworked autocomplete UX for `@` and `/` triggers to prevent overlap.
- Assistant text content now rendered before tool calls in `rebuildOutput`.

### Removed

- `.vscode` directory removed from git tracking.

---

## [v0.0.2] - 2025-07-16

### Added

- `extra_http_headers` config in `~/.ma/settings.json` for custom HTTP headers.

### Fixed

- Go module path corrected to match remote origin (`github.com/xckomorebi/minimal-agent`).

---

## [v0.0.1] - 2025-07-16

### Added

- Core agent loop with streaming SSE support.
- Tools: `bash`, `read`, `write`, `edit`, `web-search`, `web-fetch`, `skill`, `ask_user_question`.
- MCP (Model Context Protocol) support for external tools (stdio and streamable HTTP transports).
- Bubble Tea TUI with markdown rendering, viewport scrolling, and diff preview.
- Thinking/reasoning mode with inline dim rendering and expandable detail.
- Slash commands: `/save`, `/resume`, `/new-session`, `/list-session`, `/re-summarize`, `/config`, `/context`, `/compact`, `/model`, `/thinking`, `/effort`, `/help`, `/clear`.
- Slash-command autocomplete with session picker.
- Global config file (`~/.ma/settings.json`) with fsnotify hot-reload.
- Session persistence (JSON under `.ma/sessions/`) with auto-save and auto-resume.
- Multi-line input with `\` continuation.
- Token tracking, context window config, and `/compact` command for history compaction.
- Dynamic system prompt with cwd, git branch, and AGENTS.md injection.
- Approval system with multi-choice and custom denial reason.
- Cancelable agent (Ctrl-C) with dynamic hint bar.
- Version detection and User-Agent header.
- Startup banner.
- Colorized CLI output with reasoning/content separation.

### Fixed

- Tool call Type field preserved in history to prevent silent hangs.
- Empty messages filtered to prevent API 400 errors.
- Data race in session summary generation.
- Multi-line tool results padded correctly.
- Sticky-bottom auto-follow that respects manual scrolling.

[Unreleased]: https://github.com/xckomorebi/minimal-agent/compare/v0.2.0...HEAD
[v0.2.0]: https://github.com/xckomorebi/minimal-agent/compare/v0.1.1...v0.2.0
[v0.1.1]: https://github.com/xckomorebi/minimal-agent/compare/v0.1.0...v0.1.1
[v0.1.0]: https://github.com/xckomorebi/minimal-agent/compare/v0.0.2...v0.1.0
[v0.0.2]: https://github.com/xckomorebi/minimal-agent/compare/v0.0.1...v0.0.2
[v0.0.1]: https://github.com/xckomorebi/minimal-agent/releases/tag/v0.0.1
