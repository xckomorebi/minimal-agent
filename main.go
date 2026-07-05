// A minimal, runnable agent: an OpenAI Chat Completions tool-calling loop with
// `bash`, `read`, `write`, and `edit` tools, built on the official openai-go SDK.
//
// Responses are streamed over SSE. Commands that change state require interactive
// approval: `write` and `edit` always prompt, and `bash` prompts when the model
// sets its `requires_approval` parameter. `read` never prompts.
//
// Configuration — priority (highest to lowest):
//
//	CLI flags > session config > ~/.ma/settings.json > environment
//
//	API key : -ma-api-key  >  ~/.ma/settings.json  >  MA_API_KEY
//	Base URL: -url         >  ~/.ma/settings.json  >  MA_BASE_URL  >  https://api.openai.com/v1
//	Model   : -model       >  ~/.ma/settings.json  >  MA_MODEL     >  gpt-4o
//	Model, thinking, effort, auto-edit: /config set  >  ~/.ma/settings.json  >  built-in defaults
//
// The global config file (~/.ma/settings.json) is watched via fsnotify and
// reloaded automatically — no restart required.
//
// Run:
//
//	export MA_API_KEY=sk-...
//	go run .            # then type a request, Ctrl-C to quit
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

// ---- session management -------------------------------------------------

const sessionDir = ".ma-sessions"

// sessionConfig holds per-session overrides. Fields use pointers so that
// unset keys (nil) are omitted from JSON and fall through to global defaults.
type sessionConfig struct {
	Model          *string `json:"model,omitempty"`
	AutoEdit       *bool   `json:"auto_edit,omitempty"`
	Thinking       *bool   `json:"thinking,omitempty"`
	ThinkingEffort *string `json:"thinking_effort,omitempty"`
}

// sessionFile is the top-level JSON structure stored in a session file.
type sessionFile struct {
	Config  sessionConfig                                `json:"config"`
	History []openai.ChatCompletionMessageParamUnion     `json:"history"`
}

// ---- global config file ----------------------------------------------------

const globalConfigDir = ".ma"
const globalConfigFile = "settings.json"

// globalConfig holds settings from ~/.ma/settings.json. Fields use pointers
// so that unset keys (nil) are omitted from JSON and fall through to lower layers.
type globalConfig struct {
	APIKey         *string `json:"api_key,omitempty"`
	BaseURL        *string `json:"base_url,omitempty"`
	Model          *string `json:"model,omitempty"`
	Thinking       *bool   `json:"thinking,omitempty"`
	ThinkingEffort *string `json:"thinking_effort,omitempty"`
	AutoEdit       *bool   `json:"auto_edit,omitempty"`
}

// globalCfg is the currently loaded global configuration, protected by mu.
// Updated automatically via fsnotify when ~/.ma/settings.json changes.
var (
	globalCfg *globalConfig
	globalMu  sync.RWMutex
)

// readGlobalCfg returns a snapshot of the current global config (nil if none).
func readGlobalCfg() *globalConfig {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalCfg
}

// globalConfigPath returns the full path to ~/.ma/settings.json.
func globalConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, globalConfigDir, globalConfigFile), nil
}

// loadGlobalConfig reads and parses ~/.ma/settings.json. Returns nil if the
// file does not exist or cannot be parsed.
func loadGlobalConfig() *globalConfig {
	path, err := globalConfigPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg globalConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	return &cfg
}

// startConfigWatcher begins watching the config file via fsnotify. It runs in
// a goroutine and reloads globalCfg whenever the file changes. Returns an error
// if the watcher cannot be set up.
func startConfigWatcher() error {
	path, err := globalConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	// Always watch the directory — editors often save via rename (temp file →
	// real name), which we catch via directory-level Create/Write events.
	if err := w.Add(dir); err != nil {
		w.Close()
		return err
	}

	go func() {
		defer w.Close()
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				// Only reload when settings.json itself changes (not other
				// files in ~/.ma).
				if filepath.Clean(ev.Name) != filepath.Clean(path) {
					continue
				}
				if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
					// File was deleted — clear config.
					globalMu.Lock()
					globalCfg = nil
					globalMu.Unlock()
					continue
				}
				if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) {
					globalMu.Lock()
					globalCfg = loadGlobalConfig()
					globalMu.Unlock()
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				fmt.Fprintf(os.Stderr, "  %s\n", red("config watcher error: "+err.Error()))
			}
		}
	}()
	return nil
}

// ---- effective config ------------------------------------------------------
// Priority chain: session (/config) > global (~/.ma/settings.json) > built-in default.

func (a *agent) autoEdit() bool {
	if a.config.AutoEdit != nil {
		return *a.config.AutoEdit
	}
	if c := readGlobalCfg(); c != nil && c.AutoEdit != nil {
		return *c.AutoEdit
	}
	return false
}

func (a *agent) thinking() bool {
	if a.config.Thinking != nil {
		return *a.config.Thinking
	}
	if c := readGlobalCfg(); c != nil && c.Thinking != nil {
		return *c.Thinking
	}
	return true
}

func (a *agent) thinkingEffort() shared.ReasoningEffort {
	resolve := func(s *string) (shared.ReasoningEffort, bool) {
		if s == nil {
			return "", false
		}
		switch *s {
		case "low":
			return shared.ReasoningEffortLow, true
		case "high":
			return shared.ReasoningEffortHigh, true
		case "medium":
			return shared.ReasoningEffortMedium, true
		}
		return "", false
	}
	if v, ok := resolve(a.config.ThinkingEffort); ok {
		return v
	}
	if c := readGlobalCfg(); c != nil {
		if v, ok := resolve(c.ThinkingEffort); ok {
			return v
		}
	}
	return shared.ReasoningEffortMedium
}

// sessionPath returns the file path for a given session name.
func sessionPath(name string) string {
	return filepath.Join(sessionDir, name+".json")
}

// saveSession writes the current history to the session file.
func (a *agent) saveSession() error {
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return err
	}
	sf := sessionFile{
		Config:  a.config,
		History: a.history,
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(sessionPath(a.sessionName), data, 0644); err != nil {
		return err
	}
	a.sessionDirty = false
	return nil
}

// printHistory prints all conversational messages (user + assistant text),
// skipping system messages, tool calls, tool results, and reasoning blocks.
func (a *agent) printHistory() {
	for _, msg := range a.history {
		if msg.OfUser != nil {
			fmt.Println("\n" + youPrefix() + msg.OfUser.Content.OfString.Value)
		}
		if msg.OfAssistant != nil {
			// Skip assistant messages that are just tool calls (no text content).
			if len(msg.OfAssistant.ToolCalls) > 0 {
				continue
			}
			if text := msg.OfAssistant.Content.OfString.Value; text != "" {
				fmt.Println("\n" + agentPrefix() + text)
			}
		}
		// Skip system, tool, function, and developer messages.
	}
}

// loadSession loads history and config from a session file. Returns an error
// if the session does not exist. Handles both the legacy format (bare JSON
// array) and the current format ({"config":..., "history":...}).
func (a *agent) loadSession(name string) error {
	data, err := os.ReadFile(sessionPath(name))
	if err != nil {
		return err
	}

	// Try new format first: {"config":..., "history":...}
	var sf sessionFile
	if err := json.Unmarshal(data, &sf); err == nil && sf.History != nil {
		a.config = sf.Config
		a.history = cleanHistory(sf.History)
		if len(a.history) == 0 {
			return fmt.Errorf("empty session %q", name)
		}
		a.sessionName = name
		a.sessionDirty = false
		return nil
	}

	// Fall back to legacy format: bare JSON array
	var hist []openai.ChatCompletionMessageParamUnion
	if err := json.Unmarshal(data, &hist); err != nil {
		return fmt.Errorf("corrupt session %q: %w", name, err)
	}
	if len(hist) == 0 {
		return fmt.Errorf("empty session %q", name)
	}
	a.history = cleanHistory(hist)
	a.sessionName = name
	a.sessionDirty = false
	// Keep default config for legacy sessions.
	return nil
}

// listSessions returns the names of all available sessions, sorted by
// modification time (newest first).
func listSessions() ([]string, error) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type se struct {
		name string
		mod  time.Time
	}
	var sessions []se
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		sessions = append(sessions, se{name, info.ModTime()})
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].mod.After(sessions[j].mod)
	})
	names := make([]string, len(sessions))
	for i, s := range sessions {
		names[i] = s.name
	}
	return names, nil
}

// resolveSession figures out which session to start with: the one given on the
// command line, the one from MA_SESSION env, or the most recent.
// Returns "" if no session exists and nothing was explicitly requested.
func resolveSession(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("MA_SESSION"); env != "" {
		return env
	}
	names, _ := listSessions()
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// autoSave saves the session if it has changed since the last save.
func (a *agent) autoSave() {
	if !a.sessionDirty {
		return
	}
	if err := a.saveSession(); err != nil {
		fmt.Fprintln(os.Stderr, "  "+red("auto-save failed: "+err.Error()))
	} else {
		fmt.Printf("  (saved %q)\n", a.sessionName)
	}
}

// handleCommand processes a session-management command entered as "/cmd [arg]".
func (a *agent) handleCommand(cmd string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return
	}
	switch parts[0] {
	case "save":
		// Optional rename: /save <new-name>
		if len(parts) > 1 {
			oldName := a.sessionName
			oldPath := sessionPath(oldName)
			a.sessionName = parts[1]
			a.sessionDirty = true
			// Remove old session file if it exists.
			os.Remove(oldPath)
			fmt.Printf("  renamed %q -> %q\n", oldName, a.sessionName)
		}
		if err := a.saveSession(); err != nil {
			fmt.Println("  " + red("save error: "+err.Error()))
		} else {
			fmt.Printf("  saved %q (%d messages)\n", a.sessionName, len(a.history))
		}
	case "resume":
		if len(parts) < 2 {
			fmt.Println("  usage: /resume <name>  (use /list-session to see saved sessions)")
			return
		}
		name := parts[1]
		if err := a.loadSession(name); err != nil {
			fmt.Println("  " + red("load error: "+err.Error()))
		} else {
			fmt.Printf("  loaded %q (%d messages)\n", name, len(a.history))
			a.printHistory()
		}
	case "new-session":
		name := ""
		if len(parts) > 1 {
			name = parts[1]
		} else {
			name = fmt.Sprintf("session-%s", time.Now().Format("20060102-150405"))
		}
		a.autoSave() // save current session first
		a.history = []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(buildSystemMessage()),
		}
		// Copy current session config to the new session.
		a.sessionName = name
		a.sessionDirty = true
		fmt.Printf("  new session %q\n", name)
	case "list-session":
		names, err := listSessions()
		if err != nil {
			fmt.Println("  " + red("list error: "+err.Error()))
			return
		}
		if len(names) == 0 {
			fmt.Println("  (no saved sessions)")
			return
		}
		for _, n := range names {
			if n == a.sessionName {
				fmt.Printf("  %s %s\n", green("*"), n)
			} else {
				fmt.Printf("    %s\n", n)
			}
		}
	case "config":
		a.handleConfig(parts[1:])
	default:
		fmt.Printf("  unknown command: /%s\n", parts[0])
	}
}

// handleConfig processes /config commands to view or change session settings.
func (a *agent) handleConfig(args []string) {
	c := readGlobalCfg()
	if len(args) == 0 {
		// Show effective config with source.
		model := a.effectiveModel()
		modelSrc := dim("(default)")
		if a.flagModel != "" {
			modelSrc = dim("(flag)")
		} else if a.config.Model != nil && *a.config.Model != "" {
			modelSrc = dim("(session)")
		} else if c != nil && c.Model != nil && *c.Model != "" {
			modelSrc = dim("(config file)")
		} else if os.Getenv("MA_MODEL") != "" {
			modelSrc = dim("(env)")
		}
		fmt.Printf("  model          : %s %s\n", model, modelSrc)
		fmt.Printf("  auto-edit      : %s %s\n", onOff(a.autoEdit()), sourceLabel(a.config.AutoEdit != nil, c != nil && c.AutoEdit != nil))
		fmt.Printf("  thinking       : %s %s\n", onOff(a.thinking()), sourceLabel(a.config.Thinking != nil, c != nil && c.Thinking != nil))
		fmt.Printf("  thinking-effort: %s %s\n", effortString(a.thinkingEffort()), sourceLabel(a.config.ThinkingEffort != nil, c != nil && c.ThinkingEffort != nil))
		return
	}
	switch args[0] {
	case "model":
		if len(args) < 2 {
			fmt.Println("  usage: /config model <model-id>")
			return
		}
		m := args[1]
		a.config.Model = &m
		a.sessionDirty = true
		fmt.Printf("  model: %s\n", m)
	case "auto-edit":
		v := !a.autoEdit()
		a.config.AutoEdit = &v
		a.sessionDirty = true
		fmt.Printf("  auto-edit: %s\n", onOff(v))
	case "thinking":
		v := !a.thinking()
		a.config.Thinking = &v
		a.sessionDirty = true
		fmt.Printf("  thinking: %s\n", onOff(v))
	case "thinking-effort":
		if len(args) < 2 {
			fmt.Println("  usage: /config thinking-effort <low|medium|high>")
			return
		}
		level := strings.ToLower(args[1])
		if level != "low" && level != "medium" && level != "high" {
			fmt.Printf("  unknown effort level %q (use low, medium, or high)\n", args[1])
			return
		}
		a.config.ThinkingEffort = &level
		a.sessionDirty = true
		fmt.Printf("  thinking-effort: %s\n", level)
	default:
		fmt.Printf("  unknown config key %q; try model, auto-edit, thinking, or thinking-effort\n", args[0])
	}
}

// effortString returns the string form of a ReasoningEffort.
func effortString(e shared.ReasoningEffort) string {
	switch e {
	case shared.ReasoningEffortLow:
		return "low"
	case shared.ReasoningEffortHigh:
		return "high"
	default:
		return "medium"
	}
}

// onOff returns "on" or "off" for a boolean.
func onOff(v bool) string {
	if v {
		return green("on")
	}
	return red("off")
}

// sourceLabel returns a dim parenthetical showing where a setting comes from.
func sourceLabel(fromSession, fromGlobal bool) string {
	if fromSession {
		return dim("(session)")
	}
	if fromGlobal {
		return dim("(config file)")
	}
	return dim("(default)")
}

// ---- message validation ---------------------------------------------------

// cleanHistory removes messages that have neither content nor tool_calls,
// which would cause a 400 error from the API.
func cleanHistory(history []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(history))
	for _, msg := range history {
		if !isEmptyMessage(msg) {
			out = append(out, msg)
		}
	}
	return out
}

// isEmptyMessage returns true if a message has no content AND no tool_calls.
// Such messages cause 400 errors from the OpenAI API. Tool messages always
// carry a tool_call_id and are always valid.
func isEmptyMessage(msg openai.ChatCompletionMessageParamUnion) bool {
	// System messages: must have non-empty content.
	if msg.OfSystem != nil {
		return msg.OfSystem.Content.OfString.Value == ""
	}
	// User messages: must have non-empty content.
	if msg.OfUser != nil {
		return msg.OfUser.Content.OfString.Value == ""
	}
	// Assistant messages: must have non-empty content or non-empty tool_calls.
	if msg.OfAssistant != nil {
		hasContent := msg.OfAssistant.Content.OfString.Value != ""
		hasToolCalls := len(msg.OfAssistant.ToolCalls) > 0
		return !hasContent && !hasToolCalls
	}
	// Tool messages always carry tool_call_id — always valid.
	// Developer messages always have content — always valid.
	// Unknown variants: be conservative and keep them.
	return false
}

// ---- agent ---------------------------------------------------------------

type agent struct {
	client       openai.Client
	flagModel    string // from -model flag; empty if not set (fall through to config/env)
	tools        []openai.ChatCompletionToolParam
	history      []openai.ChatCompletionMessageParamUnion
	config       sessionConfig
	in           *bufio.Scanner // shared stdin, also used for approval prompts
	sessionName  string         // current session name
	sessionDirty bool           // true if history has changed since last save
}

// effectiveModel resolves the model name through the priority chain:
// CLI flag > session config > config file > env var > default.
func (a *agent) effectiveModel() string {
	if a.flagModel != "" {
		return a.flagModel
	}
	if a.config.Model != nil && *a.config.Model != "" {
		return *a.config.Model
	}
	if c := readGlobalCfg(); c != nil && c.Model != nil && *c.Model != "" {
		return *c.Model
	}
	if m := os.Getenv("MA_MODEL"); m != "" {
		return m
	}
	return "gpt-4o"
}

// runTurn streams the model's response, printing text as it arrives, and
// resolves any tool calls — looping until a message has no tool calls.
func (a *agent) runTurn(ctx context.Context) error {
	for {
		params := openai.ChatCompletionNewParams{
			Model:    openai.ChatModel(a.effectiveModel()),
			Messages: cleanHistory(a.history),
			Tools:    a.tools,
		}
		if a.thinking() {
			params.ReasoningEffort = a.thinkingEffort()
		}
		stream := a.client.Chat.Completions.NewStreaming(ctx, params)

		acc := openai.ChatCompletionAccumulator{}
		printed := false
		reasoned := false
		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)

			// Try to extract reasoning_content from the raw JSON — the SDK
			// doesn't expose it as a named field on the delta struct yet.
			if reasoning := extractReasoning(chunk.RawJSON()); reasoning != "" {
				if !printed {
					fmt.Print("\n" + thinkingPrefix())
					printed = true
				}
				fmt.Print(dim(reasoning))
				reasoned = true
			}

			if len(chunk.Choices) == 0 {
				continue
			}
			if delta := chunk.Choices[0].Delta.Content; delta != "" {
				if !printed {
					fmt.Print("\n" + agentPrefix())
					printed = true
				} else if reasoned {
					fmt.Print("\n" + agentPrefix())
					reasoned = false
				}
				fmt.Print(delta)
			}
		}
		if printed {
			fmt.Println()
		}
		if err := stream.Err(); err != nil {
			return err
		}
		if len(acc.Choices) == 0 {
			return fmt.Errorf("empty response (no choices)")
		}

		msg := acc.Choices[0].Message
		a.history = append(a.history, msg.ToParam())
		a.sessionDirty = true

		if len(msg.ToolCalls) == 0 {
			return nil // turn complete
		}
		for _, call := range msg.ToolCalls {
			a.history = append(a.history, a.runTool(call))
		}
	}
}

// extractReasoning pulls reasoning_content from a raw SSE chunk JSON. Returns ""
// on failure (malformed JSON, missing field, etc.).
func extractReasoning(raw string) string {
	var chunk struct {
		Choices []struct {
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		return ""
	}
	if len(chunk.Choices) == 0 {
		return ""
	}
	return chunk.Choices[0].Delta.ReasoningContent
}

// dim wraps text in ANSI escape codes for dim/italic rendering.
func dim(s string) string {
	return "\033[2m\033[3m" + s + "\033[0m"
}

// youPrefix returns a styled "you>" prompt.
func youPrefix() string {
	return "\033[1m\033[36myou>\033[0m "
}

// agentPrefix returns a styled "agent>" prompt.
func agentPrefix() string {
	return "\033[1m\033[32magent>\033[0m "
}

// thinkingPrefix returns a styled "thinking>" prompt for reasoning blocks.
func thinkingPrefix() string {
	return "\033[1m\033[35mthinking>\033[0m "
}

// toolDot returns a yellow bullet for tool-call output lines.
func toolDot() string {
	return "\033[1m\033[33m●\033[0m "
}

// toolLabel returns a bold yellow tool-name label.
func toolLabel(name string) string {
	return "\033[1m\033[33m" + name + "\033[0m"
}

// red wraps text in red for warnings / denials.
func red(s string) string {
	return "\033[31m" + s + "\033[0m"
}

// green wraps text in green for positive / active indicators.
func green(s string) string {
	return "\033[1m\033[32m" + s + "\033[0m"
}

// banner prints a startup banner with model, session name, and quit hint.
func banner(model, session string) {
	lines := []string{
		"minimal-agent",
		"model   : " + model,
		"session : " + session,
		"Ctrl-C to quit",
	}

	width := 0
	for _, l := range lines {
		if len(l) > width {
			width = len(l)
		}
	}
	width += 4 // padding: "  " on each side

	pad := func(s string) string {
		return "  " + s + strings.Repeat(" ", width-2-len(s))
	}

	top := "╭" + strings.Repeat("─", width) + "╮"
	btm := "╰" + strings.Repeat("─", width) + "╯"

	fmt.Println("\n" + top)
	for _, l := range lines {
		fmt.Println("│" + pad(l) + "│")
	}
	fmt.Println(btm)
}

// runTool dispatches a single tool call to its handler and returns a tool result.
func (a *agent) runTool(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	switch call.Function.Name {
	case "bash":
		return a.runBash(call)
	case "read":
		return a.readFile(call)
	case "write":
		return a.writeFile(call)
	case "edit":
		return a.editFile(call)
	default:
		return openai.ToolMessage("error: unknown tool: "+call.Function.Name, call.ID)
	}
}

// runBash runs a shell command, prompting for approval when the model flags it.
func (a *agent) runBash(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Command          string `json:"command"`
		RequiresApproval bool   `json:"requires_approval"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Command == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"command": "..."}`, call.ID)
	}

	fmt.Println("\n  " + toolDot() + toolLabel("bash") + " $ " + args.Command)
	if args.RequiresApproval && !a.approve() {
		fmt.Println("  " + red("(denied)"))
		return openai.ToolMessage("error: the user denied permission to run this command", call.ID)
	}

	out, err := exec.Command("bash", "-c", args.Command).CombinedOutput()
	result := string(out)
	if err != nil {
		result += "\n[exit: " + err.Error() + "]"
	}
	if result == "" {
		result = "(no output)"
	}
	return openai.ToolMessage(result, call.ID)
}

// readFile returns the contents of a file. Reading never requires approval.
func (a *agent) readFile(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "..."}`, call.ID)
	}

	fmt.Println("\n  " + toolDot() + toolLabel("read") + " " + args.Path)
	data, err := os.ReadFile(args.Path)
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	if len(data) == 0 {
		return openai.ToolMessage("(empty file)", call.ID)
	}
	return openai.ToolMessage(string(data), call.ID)
}

// writeFile writes (or overwrites) a file. Writing always requires approval.
func (a *agent) writeFile(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "...", "content": "..."}`, call.ID)
	}

	fmt.Printf("\n  %s%s %s (%d bytes)\n", toolDot(), toolLabel("write"), args.Path, len(args.Content))
	if !a.autoEdit() && !a.approve() {
		fmt.Println("  " + red("(denied)"))
		return openai.ToolMessage("error: the user denied permission to write this file", call.ID)
	}

	if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	return openai.ToolMessage(fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), call.ID)
}

// editFile replaces an exact, unique occurrence of old_string with new_string in
// an existing file. If old_string is missing or matches more than once, it returns
// an error so the model can re-read and retry with more context. Always prompts.
func (a *agent) editFile(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" || args.OldString == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "...", "old_string": "...", "new_string": "..."}`, call.ID)
	}

	data, err := os.ReadFile(args.Path)
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	content := string(data)

	switch strings.Count(content, args.OldString) {
	case 1: // the unique match we require
	case 0:
		return openai.ToolMessage("error: old_string not found in "+args.Path+"; read the file and retry with text that matches exactly", call.ID)
	default:
		return openai.ToolMessage("error: old_string matches multiple times in "+args.Path+"; add surrounding context to make it unique", call.ID)
	}

	fmt.Println("\n  " + toolDot() + toolLabel("edit") + " " + args.Path)
	printDiff(content, args.OldString, args.NewString)
	if !a.autoEdit() && !a.approve() {
		fmt.Println("  " + red("(denied)"))
		return openai.ToolMessage("error: the user denied permission to edit this file", call.ID)
	}

	updated := strings.Replace(content, args.OldString, args.NewString, 1)
	if err := os.WriteFile(args.Path, []byte(updated), 0644); err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	return openai.ToolMessage("edited "+args.Path, call.ID)
}

// printDiff shows the removed and added lines of an edit for the approval prompt.
// Output is git-like: a hunk header followed by context lines (prefixed " "),
// removed lines (prefixed "-") and added lines (prefixed "+").
func printDiff(content, oldString, newString string) {
	idx := strings.Index(content, oldString)
	if idx < 0 {
		return // shouldn't happen — caller already validated
	}
	before := content[:idx]
	after := content[idx+len(oldString):]

	// Split into lines, treating "" as empty slice so we count lines correctly.
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	beforeLines := split(before)
	afterLines := split(after)
	oldLines := split(oldString)
	newLines := split(newString)

	// Up to 3 lines of surrounding context.
	ctxBefore := min(3, len(beforeLines))
	ctxAfter := min(3, len(afterLines))

	// Hunk header: line numbers are 1‑based.
	oldStart := len(beforeLines) - ctxBefore + 1
	oldCount := ctxBefore + len(oldLines) + ctxAfter
	newStart := oldStart
	newCount := ctxBefore + len(newLines) + ctxAfter

	fmt.Printf("@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)

	// Context before
	for _, line := range beforeLines[len(beforeLines)-ctxBefore:] {
		fmt.Println("  " + line)
	}
	// Removed lines
	for _, line := range oldLines {
		fmt.Println("  \033[31m-" + line + "\033[0m")
	}
	// Added lines
	for _, line := range newLines {
		fmt.Println("  \033[32m+" + line + "\033[0m")
	}
	// Context after
	for _, line := range afterLines[:ctxAfter] {
		fmt.Println("  " + line)
	}
}

// approve asks the user to confirm the pending command. Anything other than an
// explicit yes (including EOF) is treated as a denial.
func (a *agent) approve() bool {
	fmt.Print("  run this command? [y/N] ")
	if !a.in.Scan() {
		fmt.Println()
		return false
	}
	switch strings.ToLower(strings.TrimSpace(a.in.Text())) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// ---- main ----------------------------------------------------------------

// firstNonEmpty returns the first non-empty string from the given candidates.
func firstNonEmpty(candidates ...string) string {
	for _, s := range candidates {
		if s != "" {
			return s
		}
	}
	return ""
}

func main() {
	// CLI flags default to empty so we can detect whether they were set.
	apiKeyFlag := flag.String("ma-api-key", "", "MA API key")
	baseURLFlag := flag.String("url", "", "API base URL")
	modelFlag := flag.String("model", "", "model id")
	sessionFlag := flag.String("session", "", "session name (or MA_SESSION env); default: auto-resume")
	flag.Parse()

	// Load global config file (~/.ma/settings.json).
	globalMu.Lock()
	globalCfg = loadGlobalConfig()
	globalMu.Unlock()

	// Start the file watcher so config changes are picked up live.
	if err := startConfigWatcher(); err != nil {
		fmt.Fprintln(os.Stderr, red("config watcher: "+err.Error()))
	}

	// Resolve API key: flag > config file > env var.
	apiKey := firstNonEmpty(*apiKeyFlag,
		cfgStr(globalCfg, func(c *globalConfig) *string { return c.APIKey }),
		os.Getenv("MA_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, red("✗ no API key; set MA_API_KEY, add it to ~/.ma/settings.json, or pass -ma-api-key"))
		os.Exit(1)
	}

	// Resolve base URL: flag > config file > env var > default.
	baseURL := firstNonEmpty(*baseURLFlag,
		cfgStr(globalCfg, func(c *globalConfig) *string { return c.BaseURL }),
		os.Getenv("MA_BASE_URL"),
		"https://api.openai.com/v1")

	// The SDK joins the request path onto the base URL, so keep a trailing slash
	// to preserve any path prefix (e.g. ".../maas/v1").
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
		tools: []openai.ChatCompletionToolParam{
			toolDef("bash", "Run a shell command with bash -c and return its combined stdout/stderr.",
				prop("command", "string", "the shell command to run"),
				prop("requires_approval", "boolean", "whether this command needs explicit user approval before running. Set true for anything destructive, irreversible, or state-changing (writes, deletes, moves, installs, network calls, git push, etc.); set false for read-only inspection (ls, cat, grep, git status, etc.)."),
			),
			toolDef("read", "Read and return the full contents of a file at the given path.",
				prop("path", "string", "path to the file to read"),
			),
			toolDef("write", "Write (creating or overwriting) a file with the given content. Use this for new files; use edit to modify an existing file. Always prompts the user for approval.",
				prop("path", "string", "path to the file to write"),
				prop("content", "string", "the full content to write to the file"),
			),
			toolDef("edit", "Modify an existing file by replacing an exact, unique occurrence of old_string with new_string. old_string must match the file byte-for-byte (including whitespace) and appear exactly once; include enough surrounding context to make it unique. Always prompts the user for approval.",
				prop("path", "string", "path to the file to edit"),
				prop("old_string", "string", "the exact existing text to replace; must be unique within the file"),
				prop("new_string", "string", "the replacement text"),
			),
		},
		history: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(buildSystemMessage()),
		},
	}

	// Try to load the requested session; if it fails, start fresh.
	loaded := false
	if a.sessionName == "" {
		// No session to load — start a fresh one.
		a.sessionName = fmt.Sprintf("session-%s", time.Now().Format("20060102-150405"))
	} else if err := a.loadSession(a.sessionName); err != nil {
		// Session file gone or corrupt — start fresh under the same name.
	} else {
		loaded = true
	}

	// Banner — printed before history so it acts as a header.
	banner(a.effectiveModel(), a.sessionName)
	if loaded {
		a.printHistory()
	}

	// Catch SIGINT (Ctrl-C) and save before exiting.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println() // newline after "^C"
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

		// Session commands (prefixed with "/")
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

// cfgStr safely dereferences a string pointer from a *globalConfig, returning ""
// if the config or field is nil.
func cfgStr(cfg *globalConfig, fn func(*globalConfig) *string) string {
	if cfg == nil {
		return ""
	}
	if p := fn(cfg); p != nil {
		return *p
	}
	return ""
}

// property describes one tool input field: its JSON Schema plus whether it is required.
type property struct {
	name   string
	schema map[string]string
}

// prop builds a required tool-input property with the given type and description.
func prop(name, typ, description string) property {
	return property{name: name, schema: map[string]string{"type": typ, "description": description}}
}

// toolDef assembles a function tool from a name, description, and its properties
// (all of which are marked required).
func toolDef(name, description string, props ...property) openai.ChatCompletionToolParam {
	properties := map[string]any{}
	required := make([]string, 0, len(props))
	for _, p := range props {
		properties[p.name] = p.schema
		required = append(required, p.name)
	}
	return openai.ChatCompletionToolParam{
		Function: openai.FunctionDefinitionParam{
			Name:        name,
			Description: openai.String(description),
			Parameters: openai.FunctionParameters{
				"type":       "object",
				"properties": properties,
				"required":   required,
			},
		},
	}
}

// buildSystemMessage constructs the system prompt, injecting dynamic context
// like the current working directory, git branch, and the project's AGENTS.md
// (development guidelines for working on this codebase).
func buildSystemMessage() string {
	var b strings.Builder
	b.WriteString("You are a concise CLI coding agent. Use the bash, read, write, and edit tools to inspect and act on the system. Prefer edit over write when changing an existing file. Keep answers short.")
	b.WriteString("\n")
	b.WriteString("If an AGENTS.md file exists in the working directory, its contents tell you how to work on this specific project — follow its conventions and guidelines.\n")
	b.WriteString("Global user configuration is at ~/.ma/settings.json (JSON, watched via fsnotify).\n")
	b.WriteString("\n")

	// Current working directory
	if cwd, err := os.Getwd(); err == nil {
		b.WriteString("Current working directory: ")
		b.WriteString(cwd)
		b.WriteString("\n")
	}

	// Git branch (if in a git repo)
	if branch := gitBranch(); branch != "" {
		b.WriteString("Current git branch: ")
		b.WriteString(branch)
		b.WriteString("\n")
	}

	// AGENTS.md (or similar files) — check common names in precedence order
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", ".agents.md", "CONTEXT.md"} {
		if data, err := os.ReadFile(name); err == nil {
			b.WriteString("\n--- ")
			b.WriteString(name)
			b.WriteString(" ---\n")
			b.WriteString(string(data))
			break // only include one
		}
	}

	return b.String()
}

// gitBranch returns the current git branch, or empty string if not in a repo.
func gitBranch() string {
	// Try reading .git/HEAD directly — avoids spawning a process.
	head, err := os.ReadFile(filepath.Join(".git", "HEAD"))
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(head))
	const prefix = "ref: refs/heads/"
	if strings.HasPrefix(ref, prefix) {
		return ref[len(prefix):]
	}
	return "" // detached HEAD or unexpected format
}
