// A minimal, runnable agent: an OpenAI Chat Completions tool-calling loop with
// `bash`, `read`, `write`, and `edit` tools, built on the official openai-go SDK.
//
// Responses are streamed over SSE. Commands that change state require interactive
// approval: `write` and `edit` always prompt, and `bash` prompts when the model
// sets its `requires_approval` parameter. `read` never prompts.
//
// Configuration (flags override environment):
//
//	API key : MA_API_KEY  or  -ma-api-key
//	Base URL: MA_BASE_URL or  -url   (default https://api.openai.com/v1)
//	Model   : MA_MODEL or -model (default gpt-4o)
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
	"syscall"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

// ---- session management -------------------------------------------------

const sessionDir = ".ma-sessions"

// sessionPath returns the file path for a given session name.
func sessionPath(name string) string {
	return filepath.Join(sessionDir, name+".json")
}

// saveSession writes the current history to the session file.
func (a *agent) saveSession() error {
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a.history, "", "  ")
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

// loadSession loads history from a session file. Returns an error if the
// session does not exist.
func (a *agent) loadSession(name string) error {
	data, err := os.ReadFile(sessionPath(name))
	if err != nil {
		return err
	}
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
		fmt.Fprintf(os.Stderr, "  auto-save failed: %v\n", err)
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
			fmt.Println("  save error:", err)
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
			fmt.Println("  load error:", err)
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
		a.sessionName = name
		a.sessionDirty = true
		fmt.Printf("  new session %q\n", name)
	case "list-session":
		names, err := listSessions()
		if err != nil {
			fmt.Println("  list error:", err)
			return
		}
		if len(names) == 0 {
			fmt.Println("  (no saved sessions)")
			return
		}
		for _, n := range names {
			marker := " "
			if n == a.sessionName {
				marker = "*"
			}
			fmt.Printf("  %s %s\n", marker, n)
		}
	default:
		fmt.Printf("  unknown command: /%s\n", parts[0])
	}
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
	client         openai.Client
	model          string
	tools          []openai.ChatCompletionToolParam
	history        []openai.ChatCompletionMessageParamUnion
	in             *bufio.Scanner // shared stdin, also used for approval prompts
	thinkingEffort shared.ReasoningEffort
	sessionName    string // current session name
	sessionDirty   bool   // true if history has changed since last save
}

// runTurn streams the model's response, printing text as it arrives, and
// resolves any tool calls — looping until a message has no tool calls.
func (a *agent) runTurn(ctx context.Context) error {
	for {
		params := openai.ChatCompletionNewParams{
			Model:           openai.ChatModel(a.model),
			Messages:        cleanHistory(a.history),
			Tools:           a.tools,
			ReasoningEffort: a.thinkingEffort,
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
					fmt.Print("\n" + agentPrefix())
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
	if !a.approve() {
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
	if !a.approve() {
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

func main() {
	apiKey := flag.String("ma-api-key", os.Getenv("MA_API_KEY"), "MA API key (or MA_API_KEY)")
	baseURL := flag.String("url", envOr("MA_BASE_URL", "https://api.openai.com/v1"), "API base URL (or MA_BASE_URL)")
	model := flag.String("model", envOr("MA_MODEL", "gpt-4o"), "model id")
	session := flag.String("session", "", "session name (or MA_SESSION env); default: auto-resume")
	flag.Parse()

	if *apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: no API key; set MA_API_KEY or pass -ma-api-key")
		os.Exit(1)
	}

	// The SDK joins the request path onto the base URL, so keep a trailing slash
	// to preserve any path prefix (e.g. ".../maas/v1").
	url := strings.TrimRight(*baseURL, "/") + "/"

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	a := &agent{
		client: openai.NewClient(
			option.WithAPIKey(*apiKey),
			option.WithBaseURL(url),
		),
		model:          *model,
		in:             scanner,
		thinkingEffort: shared.ReasoningEffortMedium,
		sessionName:    resolveSession(*session),
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
	banner(a.model, a.sessionName)
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
			fmt.Fprintln(os.Stderr, "error: "+err.Error())
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "input error: "+err.Error())
	}
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
