# Changelog

All notable changes to minimal-agent will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[Unreleased]: https://github.com/xckomorebi/minimal-agent/compare/v0.1.0...HEAD
[v0.1.0]: https://github.com/xckomorebi/minimal-agent/compare/v0.0.2...v0.1.0
[v0.0.2]: https://github.com/xckomorebi/minimal-agent/compare/v0.0.1...v0.0.2
[v0.0.1]: https://github.com/xckomorebi/minimal-agent/releases/tag/v0.0.1
