package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/openai/openai-go"
)

// commandSpec describes a slash command.
type commandSpec struct {
	name        string
	description string                                       // short one-line for /help listing
	detail      string                                       // longer help for /help <cmd>
	handler     func(a *agent, parts []string) string        // parts[0] is the command name
}

// commands maps command name → spec. The map is populated in init().
var commands map[string]commandSpec

func init() {
	commands = map[string]commandSpec{
		"save": {
			name:        "save",
			description: "save the current session",
			detail:      "save [name] — persist the current conversation. If a name is given, rename the session first.",
			handler:     cmdSave,
		},
		"resume": {
			name:        "resume",
			description: "load a saved session",
			detail:      "resume <name> — load a previously saved session. Use /list-session to see available names.",
			handler:     cmdResume,
		},
		"new-session": {
			name:        "new-session",
			description: "start a new session",
			detail:      "new-session [name] — save the current session and start a fresh one. If no name is given, a timestamped name is generated.",
			handler:     cmdNewSession,
		},
		"list-session": {
			name:        "list-session",
			description: "list all saved sessions",
			detail:      "list-session — show all saved sessions with their summaries. The current session is marked with *.",
			handler:     cmdListSession,
		},
		"re-summarize": {
			name:        "re-summarize",
			description: "regenerate the session summary",
			detail:      "re-summarize — re-generate the short one-line summary used as the session title. Uses the first user message.",
			handler:     cmdReSummarize,
		},
		"config": {
			name:        "config",
			description: "show or change configuration",
			detail:      "config [key [value]] — view or change session-level configuration. Keys: model, auto-edit, thinking, thinking-effort (low|medium|high), thinking-detail.",
			handler:     cmdConfig,
		},
		"help": {
			name:        "help",
			description: "show slash command help",
			detail:      "help [command] — list all available slash commands with descriptions, or show detailed help for a specific command.",
			handler:     cmdHelp,
		},
	}
}

// handleCommandStr processes a slash command and returns a display string.
func (a *agent) handleCommandStr(cmd string) string {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return ""
	}
	spec, ok := commands[parts[0]]
	if !ok {
		return "unknown command: /" + parts[0] + "  (try /help to see available commands)"
	}
	return spec.handler(a, parts)
}

// --- command handlers ---

func cmdSave(a *agent, parts []string) string {
	if len(parts) > 1 {
		oldName := a.sessionName
		oldPath := sessionPath(oldName)
		a.sessionName = parts[1]
		a.sessionDirty = true
		os.Remove(oldPath)
	}
	if err := a.saveSession(); err != nil {
		return "save error: " + err.Error()
	}
	return fmt.Sprintf("saved %q (%d messages)", a.sessionName, len(a.history))
}

func cmdResume(a *agent, parts []string) string {
	if len(parts) < 2 {
		return "usage: /resume <name>  (use /list-session to see saved sessions)"
	}
	name := parts[1]
	if err := a.loadSession(name); err != nil {
		return "load error: " + err.Error()
	}
	a.reasonings = nil
	a.fileMtimes = nil
	return fmt.Sprintf("loaded %q (%d messages)", name, len(a.history))
}

func cmdNewSession(a *agent, parts []string) string {
	name := ""
	if len(parts) > 1 {
		name = parts[1]
	} else {
		name = fmt.Sprintf("session-%s", time.Now().Format("20060102-150405"))
	}
	a.autoSave()
	a.history = []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(buildSystemMessage()),
	}
	a.sessionName = name
	a.sessionDirty = true
	a.summary = ""
	a.summaryGenerated = false
	a.reasonings = nil
	a.fileMtimes = nil
	return fmt.Sprintf("new session %q", name)
}

func cmdListSession(a *agent, parts []string) string {
	names, err := listSessions()
	if err != nil {
		return "list error: " + err.Error()
	}
	if len(names) == 0 {
		return "(no saved sessions)"
	}
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteString("\n")
		}
		if n == a.sessionName {
			b.WriteString("*" + n)
		} else {
			b.WriteString(n)
		}
		if s := sessionSummary(n); s != "" {
			b.WriteString(" ‒ " + s)
		}
	}
	return b.String()
}

func cmdReSummarize(a *agent, parts []string) string {
	a.summaryGenerated = false
	// Find the first user message to pass as a prompt.
	var userText string
	for _, m := range a.history {
		if m.OfUser != nil {
			userText = m.OfUser.Content.OfString.Value
			break
		}
	}
	a.generateSessionSummary(userText)
	if a.summary == "" {
		return "could not generate summary (no user message yet?)"
	}
	return "summary: " + a.summary
}

func cmdConfig(a *agent, parts []string) string {
	return a.handleConfigStr(parts[1:])
}

// cmdHelp shows all commands (with descriptions) or detailed help for one.
func cmdHelp(a *agent, parts []string) string {
	if len(parts) > 1 {
		// /help <command>
		name := parts[1]
		spec, ok := commands[name]
		if !ok {
			return "unknown command: /" + name
		}
		var b strings.Builder
		b.WriteString("/")
		b.WriteString(spec.name)
		b.WriteString("\n\n")
		b.WriteString(spec.detail)
		return b.String()
	}

	// /help — list all commands.
	// Sort by name for a stable order.
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("slash commands:\n\n")
	maxLen := 0
	for _, n := range names {
		if len(n) > maxLen {
			maxLen = len(n)
		}
	}
	for _, n := range names {
		spec := commands[n]
		b.WriteString(fmt.Sprintf("  /%-*s  %s\n", maxLen, spec.name, spec.description))
	}
	b.WriteString("\ntype /help <command> for details")
	return b.String()
}

// --- config sub-handler ---

// handleConfigStr returns config info as a multi-line string.
func (a *agent) handleConfigStr(args []string) string {
	c := readGlobalCfg()
	if len(args) == 0 {
		model := a.effectiveModel()
		src := "(default)"
		if a.flagModel != "" {
			src = "(flag)"
		} else if a.config.Model != nil && *a.config.Model != "" {
			src = "(session)"
		} else if c != nil && c.Model != nil && *c.Model != "" {
			src = "(config file)"
		} else if os.Getenv("MA_MODEL") != "" {
			src = "(env)"
		}
		auto := onOff(a.autoEdit())
		think := onOff(a.thinking())
		effort := effortString(a.thinkingEffort())
		detail := onOff(a.thinkingDetail())
		return fmt.Sprintf("model     : %s %s\nauto-edit : %s\nthinking  : %s\neffort    : %s\ndetail    : %s",
			model, src, auto, think, effort, detail)
	}
	switch args[0] {
	case "model":
		if len(args) < 2 {
			return "usage: /config model <model-id>"
		}
		m := args[1]
		a.config.Model = &m
		a.sessionDirty = true
		return "model: " + m
	case "auto-edit":
		v := !a.autoEdit()
		a.config.AutoEdit = &v
		a.sessionDirty = true
		return "auto-edit: " + onOff(v)
	case "thinking":
		v := !a.thinking()
		a.config.Thinking = &v
		a.sessionDirty = true
		return "thinking: " + onOff(v)
	case "thinking-effort":
		if len(args) < 2 {
			return "usage: /config thinking-effort <low|medium|high>"
		}
		level := strings.ToLower(args[1])
		if level != "low" && level != "medium" && level != "high" {
			return "unknown effort level " + level + " (use low, medium, high)"
		}
		a.config.ThinkingEffort = &level
		a.sessionDirty = true
		return "thinking-effort: " + level
	case "thinking-detail":
		v := !a.thinkingDetail()
		a.config.ThinkingDetail = &v
		a.sessionDirty = true
		return "thinking-detail: " + onOff(v)
	default:
		return "unknown config key " + args[0] + "; try model, auto-edit, thinking, thinking-effort, thinking-detail"
	}
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// --- autocomplete ---

// autocompleteCommand returns possible completions for a slash command at the
// given cursor position in the input text. It returns nil if no completions
// are available.
func autocompleteCommand(input string, cursorPos int) []string {
	if !strings.HasPrefix(input, "/") {
		return nil
	}

	// Clamp cursor to end if beyond the string.
	if cursorPos > len(input) {
		cursorPos = len(input)
	}

	// Text up to the cursor.
	upToCursor := input[:cursorPos]
	rest := strings.TrimPrefix(upToCursor, "/")

	// Split into parts. The last part is the word being completed (may be
	// empty if cursor is after a trailing space).
	parts := strings.Fields(rest)

	// If the cursor is right after a space, the user hasn't started typing
	// the next word yet. Treat it as an empty word at the end.
	trailingSpace := len(rest) > 0 && rest[len(rest)-1] == ' '

	if trailingSpace {
		parts = append(parts, "")
	}

	if len(parts) == 0 {
		// Just "/" with nothing after.
		return filterPrefix("", commandNames())
	}

	cmdName := parts[0]

	// If the first word doesn't match any command exactly, suggest command names.
	matchedCmd := false
	for _, n := range commandNames() {
		if n == cmdName {
			matchedCmd = true
			break
		}
	}

	if !matchedCmd && len(parts) == 1 && !trailingSpace {
		// User is typing a command name — suggest matching commands.
		return filterPrefix(cmdName, commandNames())
	}

	if !matchedCmd {
		return nil
	}

	// We have a recognized command. Now handle subcommands/args per command.
	switch cmdName {
	case "help":
		if len(parts) >= 2 {
			helpArgs := parts[1:]
			if trailingSpace && len(helpArgs) > 0 && helpArgs[len(helpArgs)-1] == "" {
				helpArgs = helpArgs[:len(helpArgs)-1]
			}
			return autocompleteHelpArg(helpArgs, trailingSpace)
		}
		if trailingSpace {
			return commandNames()
		}
		return filterPrefix(cmdName, commandNames())
	case "config":
		configArgs := parts[1:]
		if trailingSpace && len(configArgs) > 0 && configArgs[len(configArgs)-1] == "" {
			configArgs = configArgs[:len(configArgs)-1]
		}
		return autocompleteConfigArg(configArgs, trailingSpace)
	case "save":
		if len(parts) == 2 {
			return filterPrefix(parts[1], allSessionNames())
		}
		return nil
	case "resume":
		if len(parts) == 2 {
			return filterPrefix(parts[1], allSessionNames())
		}
		return nil
	default:
		return nil
	}
}

// autocompleteHelpArg handles autocomplete for /help <command>.
func autocompleteHelpArg(args []string, trailingSpace bool) []string {
	if len(args) == 0 {
		if trailingSpace {
			return commandNames()
		}
		return nil
	}
	if len(args) == 1 && !trailingSpace {
		return filterPrefix(args[0], commandNames())
	}
	return nil
}

// commandNames returns all slash command names, sorted.
func commandNames() []string {
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// autocompleteConfigArg handles autocomplete for /config subcommands.
// trailingSpace indicates whether the user just pressed space after the last word,
// meaning they want completions for the next argument position.
func autocompleteConfigArg(args []string, trailingSpace bool) []string {
	if len(args) == 0 {
		if trailingSpace {
			return []string{"thinking", "auto-edit", "thinking-effort", "thinking-detail", "model"}
		}
		return nil // shouldn't happen: empty args without trailing space
	}

	subName := args[0]
	valueArgs := args[1:]

	// If there are no value args and trailing space, suggest values for the subcommand.
	if len(valueArgs) == 0 && trailingSpace {
		return autocompleteConfigValue(subName, "")
	}

	// If there are no value args and no trailing space, suggest subcommand names.
	if len(valueArgs) == 0 && !trailingSpace {
		return filterPrefix(subName, []string{"thinking", "auto-edit", "thinking-effort", "thinking-detail", "model"})
	}

	// If there's one value arg and no trailing space, filter existing value completions.
	if len(valueArgs) == 1 && !trailingSpace {
		return autocompleteConfigValue(subName, valueArgs[0])
	}

	// Otherwise, no completions.
	return nil
}

// autocompleteConfigValue returns completions for a config subcommand's value.
func autocompleteConfigValue(subName, prefix string) []string {
	switch subName {
	case "thinking", "auto-edit", "thinking-detail":
		if prefix == "" {
			return []string{"on", "off"}
		}
		return filterPrefix(prefix, []string{"on", "off"})
	case "thinking-effort":
		if prefix == "" {
			return []string{"low", "medium", "high"}
		}
		return filterPrefix(prefix, []string{"low", "medium", "high"})
	case "model":
		return nil
	default:
		return nil
	}
}

// filterPrefix returns items from candidates that start with prefix,
// case-insensitively. If prefix is empty, returns all candidates.
func filterPrefix(prefix string, candidates []string) []string {
	if prefix == "" {
		return candidates
	}
	lower := strings.ToLower(prefix)
	var result []string
	for _, c := range candidates {
		if strings.HasPrefix(strings.ToLower(c), lower) {
			result = append(result, c)
		}
	}
	return result
}

// allSessionNames returns the names of all saved sessions (no directory listing
// error handling — errors are swallowed and an empty slice returned).
func allSessionNames() []string {
	names, _ := listSessions()
	return names
}
