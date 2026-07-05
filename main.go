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
	b.WriteString("You are a concise CLI coding agent. Use the bash, read, write, edit, web-search, and web-fetch tools to inspect and act on the system. Prefer edit over write when changing an existing file. Keep answers short.\n")
	b.WriteString("Editing a file, or overwriting an existing one with write, requires that you already know its current contents. Any earlier read, write, or edit of the file in this session is enough — you need not re-read right before changing it. These tools refuse only when you have never seen the file, or when it changed on disk since you last saw it; in that case read it again to pick up the current contents before retrying. Creating a brand-new file with write is unrestricted.\n")
	b.WriteString("If an AGENTS.md file exists in the working directory, its contents tell you how to work on this specific project — follow its conventions and guidelines.\n")
	b.WriteString("\n")
	b.WriteString("State-changing operations (write, edit, destructive bash) require user approval before execution. Read-only operations (read, ls, cat, grep, git status) run immediately.\n")
	b.WriteString("Responses stream token-by-token over SSE. When you need to think through a problem, use reasoning blocks (shown in dim italic to the user) before your final response or tool calls.\n")
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

	for _, name := range []string{"AGENTS.md", "CLAUDE.md", ".agents.md", "CONTEXT.md"} {
		if data, err := os.ReadFile(name); err == nil {
			b.WriteString("\n--- ")
			b.WriteString(name)
			b.WriteString(" ---\n")
			b.WriteString(string(data))
			break
		}
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
	flag.Parse()

	globalMu.Lock()
	globalCfg = loadGlobalConfig()
	globalMu.Unlock()

	if err := startConfigWatcher(); err != nil {
		fmt.Fprintln(os.Stderr, "config watcher:", err)
	}

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

	a := &agent{
		client: openai.NewClient(
			option.WithAPIKey(apiKey),
			option.WithBaseURL(url),
		),
		flagModel:   *modelFlag,
		sessionName: resolveSession(*sessionFlag),
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

	// Build the TUI model (history is loaded on first WindowSizeMsg).
	m := newTUIModel(a)

	// Handle graceful shutdown signals.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
			a.autoSave()
			os.Exit(0)
		case <-ctx.Done():
		}
	}()

	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Save on clean exit.
	a.autoSave()
}
