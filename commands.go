package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
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
			detail:      "re-summarize — re-generate the short one-line summary used as the session title. Uses all user messages to capture the full conversation context.",
			handler:     cmdReSummarize,
		},
		"config": {
			name:        "config",
			description: "show or change configuration",
			detail:      "config [key [value]] — view or change session-level configuration. Keys: model, auto-edit, thinking, thinking-effort (low|medium|high), thinking-detail, context-window, stream.",
			handler:     cmdConfig,
		},
		"context": {
			name:        "context",
			description: "show current token usage",
			detail:      "context — show current prompt, completion, and total tokens for the conversation, plus the configured context window size and percentage used.",
			handler:     cmdContext,
		},
		"compact": {
			name:        "compact",
			description: "summarize conversation to free context",
			detail:      "compact — ask the LLM to summarize the conversation so far and replace the history with the summary, freeing up context window space. The system prompt and tool definitions are preserved.",
			handler:     cmdCompact,
		},
		"model": {
			name:        "model",
			description: "shorthand for /config model",
			detail:      "model <model-id> — shorthand for /config model <model-id>.",
			handler:     cmdModel,
		},
		"thinking": {
			name:        "thinking",
			description: "shorthand for /config thinking",
			detail:      "thinking [on|off] — shorthand for /config thinking. Toggles if no value given.",
			handler:     cmdThinking,
		},
		"effort": {
			name:        "effort",
			description: "shorthand for /config thinking-effort",
			detail:      "effort <low|medium|high> — shorthand for /config thinking-effort.",
			handler:     cmdEffort,
		},
		"clear": {
			name:        "clear",
			description: "clear history and delete saved session",
			detail:      "clear — remove all messages from history, delete the saved session file from disk, regenerate the system prompt, and clear the summary. Keeps the session name and configuration settings.",
			handler:     cmdClear,
		},
		"help": {
			name:        "help",
			description: "show slash command help",
			detail:      "help [command] — list all available slash commands with descriptions, or show detailed help for a specific command.",
			handler:     cmdHelp,
		},
		"mcp": {
			name:        "mcp",
			description: "manage connected MCP servers",
			detail:      "mcp list — show all connected MCP servers with their tools.\nmcp reconnect — disconnect and reconnect all configured MCP servers.",
			handler:     cmdMcp,
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
		name = time.Now().Format("20060102-150405")
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
	a.tokenUsage = tokenUsage{}
	return fmt.Sprintf("new session %q", name)
}

func cmdClear(a *agent, parts []string) string {
	os.Remove(sessionPath(a.sessionName))
	a.history = []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(buildSystemMessage()),
	}
	a.summary = ""
	a.summaryGenerated = false
	a.reasonings = nil
	a.reasoningAcc = ""
	a.fileMtimes = nil
	a.tokenUsage = tokenUsage{}
	a.sessionDirty = false
	return "cleared history (session: " + a.sessionName + ")"
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
	// Collect all user messages to use as context for the summary.
	var userMessages []string
	for _, m := range a.history {
		if m.OfUser != nil {
			userMessages = append(userMessages, m.OfUser.Content.OfString.Value)
		}
	}
	userText := strings.Join(userMessages, "\n")
	if userText == "" {
		return "no user messages yet"
	}
	a.generateSessionSummary(userText)
	if a.summary == "" {
		return "could not generate summary"
	}
	return "summary: " + a.summary
}

func cmdConfig(a *agent, parts []string) string {
	return a.handleConfigStr(parts[1:])
}

// cmdModel is /model — shorthand for /config model.
func cmdModel(a *agent, parts []string) string {
	// parts[0] = "model"
	args := append([]string{"model"}, parts[1:]...)
	return a.handleConfigStr(args)
}

// cmdThinking is /thinking — shorthand for /config thinking.
func cmdThinking(a *agent, parts []string) string {
	args := append([]string{"thinking"}, parts[1:]...)
	return a.handleConfigStr(args)
}

// cmdEffort is /effort — shorthand for /config thinking-effort.
func cmdEffort(a *agent, parts []string) string {
	args := append([]string{"thinking-effort"}, parts[1:]...)
	return a.handleConfigStr(args)
}

// cmdContext shows current token usage for the session.
func cmdContext(a *agent, parts []string) string {
	cw := a.contextWindow()
	tu := a.tokenUsage
	if tu.Total == 0 {
		return fmt.Sprintf("no token usage data yet (context window: %s)", formatTokens(cw))
	}
	pct := float64(tu.Total) / float64(cw) * 100
	return fmt.Sprintf("token usage (current):\n  prompt       %s\n  completion   %s\n  total        %s\n  context win  %s\n  used         %.1f%%",
		formatTokens(tu.Prompt),
		formatTokens(tu.Completion),
		formatTokens(tu.Total),
		formatTokens(cw),
		pct)
}

// cmdCompact kicks off an asynchronous conversation compaction. It returns a
// status string immediately and runs the summarization in a goroutine.
func cmdCompact(a *agent, parts []string) string {
	// Must have at least a system message and one user message to compact.
	if len(a.history) < 3 {
		return "nothing to compact (need at least one user message)"
	}
	go a.compactHistory()
	return "compacting..."
}
func formatTokens(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	rem := len(s) % 3
	if rem == 0 {
		rem = 3
	}
	b.WriteString(s[:rem])
	for i := rem; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// cmdMcp handles /mcp — manage MCP servers.
func cmdMcp(a *agent, parts []string) string {
	if len(parts) < 2 {
		// Default to list.
		return mcpList()
	}
	switch parts[1] {
	case "list":
		return mcpList()
	case "reconnect":
		return mcpReconnect(a)
	default:
		return "usage: /mcp [list|reconnect]"
	}
}

func mcpList() string {
	if len(activeMCPServers) == 0 {
		return "(no connected MCP servers)"
	}
	var b strings.Builder
	for _, cs := range activeMCPServers {
		fmt.Fprintf(&b, "%s", cs.config.Name)
		if cs.config.URL != "" {
			fmt.Fprintf(&b, "  [%s]", cs.config.URL)
		}
		if cs.config.Command != "" {
			fmt.Fprintf(&b, "  [%s %s]", cs.config.Command, strings.Join(cs.config.Args, " "))
		}
		fmt.Fprintf(&b, "  (%d tools)\n", len(cs.tools))
		for _, t := range cs.tools {
			fmt.Fprintf(&b, "    - %s\n", t.Function.Name)
		}
	}
	return b.String()
}

func mcpReconnect(a *agent) string {
	closeMCPServers()
	activeMCPServers = nil

	// Reset external tools: keep only MCP tools rebuilt from reconnect.
	var newExternal []openai.ChatCompletionToolParam
	for _, t := range externalTools {
		if strings.HasPrefix(t.Function.Name, "mcp__") {
			continue // drop old MCP tools
		}
		newExternal = append(newExternal, t)
	}
	externalTools = newExternal

	var configs []mcpServerConfig
	if c := readGlobalCfg(); c != nil {
		configs = c.MCPServers
	}
	if len(configs) == 0 {
		return "no MCP servers configured in settings.json"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connectMCPServers(ctx, configs)

	// Update the agent's tool snapshot.
	a.tools = allTools()

	return mcpList()
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
		stream := onOff(a.stream())
		cw := a.contextWindow()
		return fmt.Sprintf("model         : %s %s\nauto-edit     : %s\nthinking      : %s\neffort        : %s\ndetail        : %s\nstream        : %s\ncontext-window: %s",
			model, src, auto, think, effort, detail, stream, formatTokens(cw))
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
	case "context-window":
		if len(args) < 2 {
			return "usage: /config context-window <tokens>"
		}
		n, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || n <= 0 {
			return "invalid token count: " + args[1]
		}
		a.config.ContextWindow = &n
		a.sessionDirty = true
		return "context-window: " + formatTokens(n)
	case "stream":
		v := !a.stream()
		a.config.Stream = &v
		a.sessionDirty = true
		return "stream: " + onOff(v)
	default:
		return "unknown config key " + args[0] + "; try model, auto-edit, thinking, thinking-effort, thinking-detail, context-window, stream"
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
	case "model":
		modelArgs := parts[1:]
		if trailingSpace && len(modelArgs) > 0 && modelArgs[len(modelArgs)-1] == "" {
			modelArgs = modelArgs[:len(modelArgs)-1]
		}
		return autocompleteConfigArg(append([]string{"model"}, modelArgs...), trailingSpace)
	case "thinking":
		thinkArgs := parts[1:]
		if trailingSpace && len(thinkArgs) > 0 && thinkArgs[len(thinkArgs)-1] == "" {
			thinkArgs = thinkArgs[:len(thinkArgs)-1]
		}
		return autocompleteConfigArg(append([]string{"thinking"}, thinkArgs...), trailingSpace)
	case "effort":
		effortArgs := parts[1:]
		if trailingSpace && len(effortArgs) > 0 && effortArgs[len(effortArgs)-1] == "" {
			effortArgs = effortArgs[:len(effortArgs)-1]
		}
		return autocompleteConfigArg(append([]string{"thinking-effort"}, effortArgs...), trailingSpace)
	case "save", "resume":
		return nil
	case "mcp":
		mcpArgs := parts[1:]
		if trailingSpace && len(mcpArgs) > 0 && mcpArgs[len(mcpArgs)-1] == "" {
			mcpArgs = mcpArgs[:len(mcpArgs)-1]
		}
		return autocompleteMcpArg(mcpArgs, trailingSpace)
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

// autocompleteMcpArg handles autocomplete for /mcp subcommands.
func autocompleteMcpArg(args []string, trailingSpace bool) []string {
	subs := []string{"list", "reconnect"}
	if len(args) == 0 {
		if trailingSpace {
			return subs
		}
		return nil
	}
	if len(args) == 1 && !trailingSpace {
		return filterPrefix(args[0], subs)
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
			return []string{"thinking", "auto-edit", "thinking-effort", "thinking-detail", "model", "context-window", "stream"}
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
		return filterPrefix(subName, []string{"thinking", "auto-edit", "thinking-effort", "thinking-detail", "model", "context-window", "stream"})
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
	case "thinking", "auto-edit", "thinking-detail", "stream":
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
	case "context-window":
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
