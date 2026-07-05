package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
	b.WriteString("You are a concise CLI coding agent. Use the bash, read, write, and edit tools to inspect and act on the system. Prefer edit over write when changing an existing file. Keep answers short.")
	b.WriteString("\n")
	b.WriteString("If an AGENTS.md file exists in the working directory, its contents tell you how to work on this specific project — follow its conventions and guidelines.\n")
	b.WriteString("Global user configuration is at ~/.ma/settings.json (JSON, watched via fsnotify).\n")
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
		fmt.Fprintln(os.Stderr, red("config watcher: "+err.Error()))
	}

	apiKey := firstNonEmpty(*apiKeyFlag,
		cfgStr(globalCfg, func(c *globalConfig) *string { return c.APIKey }),
		os.Getenv("MA_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, red("✗ no API key; set MA_API_KEY, add it to ~/.ma/settings.json, or pass -ma-api-key"))
		os.Exit(1)
	}

	baseURL := firstNonEmpty(*baseURLFlag,
		cfgStr(globalCfg, func(c *globalConfig) *string { return c.BaseURL }),
		os.Getenv("MA_BASE_URL"),
		"https://api.openai.com/v1")
	url := strings.TrimRight(baseURL, "/") + "/"

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	a := &agent{
		client: openai.NewClient(
			option.WithAPIKey(apiKey),
			option.WithBaseURL(url),
		),
		flagModel:   *modelFlag,
		in:          scanner,
		sessionName: resolveSession(*sessionFlag),
		tools: allTools(),
		history: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(buildSystemMessage()),
		},
	}

	loaded := false
	if *newFlag {
		a.sessionName = "" // force fresh timestamped session
	}
	if a.sessionName == "" {
		a.sessionName = fmt.Sprintf("session-%s", time.Now().Format("20060102-150405"))
	} else if err := a.loadSession(a.sessionName); err != nil {
		// Session file gone or corrupt — start fresh under the same name.
	} else {
		loaded = true
	}

	banner(a.effectiveModel(), a.sessionName)
	if loaded {
		a.printHistory()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println()
		a.autoSave()
		os.Exit(0)
	}()

	ctx := context.Background()
	for {
		fmt.Print("\n" + youPrefix())
		if !scanner.Scan() {
			a.autoSave()
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// If line ends with backslash, read continuation lines.
		for strings.HasSuffix(line, "\\") {
			line = strings.TrimSuffix(line, "\\")
			fmt.Print(dim("  ... "))
			if !scanner.Scan() {
				break
			}
			next := strings.TrimSpace(scanner.Text())
			line += "\n" + next
		}

		if strings.HasPrefix(line, "/") {
			a.handleCommand(strings.TrimPrefix(line, "/"))
			continue
		}

		a.history = append(a.history, openai.UserMessage(line))
		a.sessionDirty = true
		if err := a.runTurn(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "\n"+red("✗ API error: "+err.Error()))
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, red("✗ input error: "+err.Error()))
	}
}
