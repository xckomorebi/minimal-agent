package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

func firstNonEmpty(candidates ...string) string {
	for _, s := range candidates {
		if s != "" {
			return s
		}
	}
	return ""
}

func cfgStr(cfg *globalConfig, fn func(*globalConfig) *string) string {
	if cfg == nil {
		return ""
	}
	if p := fn(cfg); p != nil {
		return *p
	}
	return ""
}

func buildSystemMessage() string {
	var b strings.Builder
	b.WriteString("You are a concise CLI coding agent. Use the bash, read, write, edit, web-search, web-fetch, skill, and ask_user_question tools to inspect and act on the system. Prefer edit over write when changing an existing file. Keep answers short.\n")
	b.WriteString("Editing a file, or overwriting an existing one with write, requires that you already know its current contents. Any earlier read, write, or edit of the file in this session is enough — you need not re-read right before changing it. These tools refuse only when you have never seen the file, or when it changed on disk since you last saw it; in that case read it again to pick up the current contents before retrying. Creating a brand-new file with write is unrestricted.\n")
	b.WriteString("If an AGENTS.md file exists in the working directory, its contents tell you how to work on this specific project — follow its conventions and guidelines. A global ~/.ma/AGENTS.md may also exist with user-level conventions that apply across all projects.\n")
	b.WriteString("\n")
	b.WriteString("State-changing operations (write, edit, destructive bash) require user approval before execution. Read-only operations (read, ls, cat, grep, git status) run immediately.\n")
	b.WriteString("Responses are sent via the chat completions API. By default responses stream token-by-token over SSE; when streaming is disabled, the full response arrives at once. When you need to think through a problem, use reasoning blocks (shown in dim italic to the user) before your final response or tool calls.\n")
	b.WriteString("Tool call results must always go through openai.ToolMessage(result, call.ID). Sessions auto-save on every turn and on exit.\n")
	b.WriteString("\n")

	if cwd, err := os.Getwd(); err == nil {
		b.WriteString("Current working directory: ")
		b.WriteString(cwd)
		b.WriteString("\n")
	}

	if branch := gitBranch(); branch != "" {
		b.WriteString("Current git branch: ")
		b.WriteString(branch)
		b.WriteString("\n")
	}

	// Load global user memory file first (more general), then project memory (more specific).
	if home, err := os.UserHomeDir(); err == nil {
		globalPath := filepath.Join(home, ".ma", "AGENTS.md")
		if data, err := os.ReadFile(globalPath); err == nil {
			b.WriteString("\n--- ")
			b.WriteString(globalPath)
			b.WriteString(" ---\n")
			b.WriteString(string(data))
		}
	}

	for _, name := range []string{"AGENTS.md", "CLAUDE.md", ".agents.md", "CONTEXT.md"} {
		if data, err := os.ReadFile(name); err == nil {
			b.WriteString("\n--- ")
			b.WriteString(name)
			b.WriteString(" ---\n")
			b.WriteString(string(data))
			break
		}
	}

	// Include available skills with their descriptions.
	if len(skillIndex) > 0 {
		b.WriteString("\n## Available Skills\n\n")
		b.WriteString("You have a `skill` tool that loads skill instructions from ~/.agents/skills/<name>.\n")
		b.WriteString("Call skill(name) to load a skill's full instructions before using it. Use name='list' to enumerate.\n")
		b.WriteString("Available skills (use the skill tool to load one before applying it):\n\n")
		for _, se := range skillIndex {
			b.WriteString(fmt.Sprintf("- **%s**: %s\n", se.Name, se.Description))
		}
	}

	// Include MCP tools summary.
	if summary := mcpServerSummary(); summary != "" {
		b.WriteString("\n")
		b.WriteString(summary)
		b.WriteString("\n")
	}

	return b.String()
}

func gitBranch() string {
	head, err := os.ReadFile(filepath.Join(".git", "HEAD"))
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(head))
	const prefix = "ref: refs/heads/"
	if strings.HasPrefix(ref, prefix) {
		return ref[len(prefix):]
	}
	return ""
}

func main() {
	apiKeyFlag := flag.String("ma-api-key", "", "MA API key")
	baseURLFlag := flag.String("url", "", "API base URL")
	modelFlag := flag.String("model", "", "model id")
	sessionFlag := flag.String("session", "", "session name (or MA_SESSION env); default: auto-resume")
	newFlag := flag.Bool("new", false, "start a new session instead of auto-resuming")
	contextWindowFlag := flag.Int64("context-window", 0, "context window size in tokens (default 200000)")
	flag.Parse()

	globalMu.Lock()
	globalCfg = loadGlobalConfig()
	globalMu.Unlock()

	if err := startConfigWatcher(); err != nil {
		fmt.Fprintln(os.Stderr, "config watcher:", err)
	}

	// Build the skill index at startup so it's available in the system prompt.
	buildSkillIndex()

	apiKey := firstNonEmpty(*apiKeyFlag,
		cfgStr(globalCfg, func(c *globalConfig) *string { return c.APIKey }),
		os.Getenv("MA_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "no API key; set MA_API_KEY, add it to ~/.ma/settings.json, or pass -ma-api-key")
		os.Exit(1)
	}

	baseURL := firstNonEmpty(*baseURLFlag,
		cfgStr(globalCfg, func(c *globalConfig) *string { return c.BaseURL }),
		os.Getenv("MA_BASE_URL"),
		"https://api.openai.com/v1")
	url := strings.TrimRight(baseURL, "/") + "/"

	// Build client options, allowing custom headers from the global config.
	clientOpts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithBaseURL(url),
		option.WithHeader("User-Agent", "minimal-agent/"+Version),
	}
	if cfg := readGlobalCfg(); cfg != nil {
		for k, v := range cfg.HTTPHeaders {
			clientOpts = append(clientOpts, option.WithHeader(k, v))
		}
	}

	a := &agent{
		client: openai.NewClient(clientOpts...),
		flagModel:         *modelFlag,
		flagContextWindow: *contextWindowFlag,
		sessionName:       resolveSession(*sessionFlag),
		tools:       allTools(),
		history: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(buildSystemMessage()),
		},
	}

	if *newFlag {
		a.sessionName = "" // force fresh timestamped session
	}
	if a.sessionName == "" {
		a.sessionName = fmt.Sprintf("session-%s", time.Now().Format("20060102-150405"))
	} else if err := a.loadSession(a.sessionName); err != nil {
		// Session file gone or corrupt — start fresh under the same name.
	}

	// Connect to MCP servers asynchronously so startup is never blocked.
	if globalCfg != nil && len(globalCfg.MCPServers) > 0 {
		configs := globalCfg.MCPServers // copy slice
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			connectMCPServers(ctx, configs)
			a.tools = allTools()
		}()
	}

	// Build the TUI model (history is loaded on first WindowSizeMsg).
	m := newTUIModel(a)

	// Handle SIGTERM for graceful shutdown. SIGINT is left to Bubble Tea
	// which delivers it as a tea.KeyCtrlC so the TUI can decide between
	// canceling the current agent turn (when running) or quitting (when idle).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
		a.autoSave()
		closeMCPServers()
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		closeMCPServers()
		os.Exit(1)
	}

	// Save on clean exit.
	a.autoSave()
	closeMCPServers()
}
