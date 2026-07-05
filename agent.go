package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/shared"
)

type agent struct {
	client       openai.Client
	flagModel    string
	tools        []openai.ChatCompletionToolParam
	history      []openai.ChatCompletionMessageParamUnion
	config       sessionConfig
	sessionName  string
	sessionDirty bool
	msgCh        chan tea.Msg // channel to send events to the TUI

	// reasonings stores the full reasoning text for each assistant message in
	// history, keyed by the message's index in the history slice. Reasoning is
	// never persisted (API rejects it as input), but kept in memory so features
	// like "show thinking detail" can expand collapsed blocks.
	reasonings   map[int]string
	reasoningAcc string // accumulator during streaming

	// fileMtimes tracks the on-disk modification time of each file at the moment
	// this agent last read or wrote it (keyed by absolute path). The edit tool
	// uses it to refuse edits to files it hasn't seen, or that changed on disk
	// since it last saw them, so it never clobbers an unseen external change.
	fileMtimes map[string]time.Time

	// summary is a brief one-line description of the session, generated
	// asynchronously after the first user message.
	summary string
	// summaryGenerated prevents duplicate async summary generation.
	summaryGenerated bool
}

// rememberFile records the file's current on-disk mtime as the version this
// agent has seen. Call after a successful read or write.
func (a *agent) rememberFile(path string) {
	key, err := filepath.Abs(path)
	if err != nil {
		key = path
	}
	info, err := os.Stat(key)
	if err != nil {
		return
	}
	if a.fileMtimes == nil {
		a.fileMtimes = map[string]time.Time{}
	}
	a.fileMtimes[key] = info.ModTime()
}

// checkFileFresh verifies the agent has seen the file's current on-disk state,
// so an edit won't silently overwrite changes it never read. It returns a
// non-empty error message (for the model) when the file is unseen or stale.
func (a *agent) checkFileFresh(path string) string {
	key, err := filepath.Abs(path)
	if err != nil {
		key = path
	}
	info, err := os.Stat(key)
	if err != nil {
		return "error: " + err.Error()
	}
	seen, ok := a.fileMtimes[key]
	if !ok {
		return "error: you have not read " + path + " yet; read it first so you modify its current contents"
	}
	if info.ModTime().After(seen) {
		return "error: " + path + " has changed on disk since you last read it; read it again before modifying it to avoid overwriting external changes"
	}
	return ""
}

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

func (a *agent) thinkingDetail() bool {
	if a.config.ThinkingDetail != nil {
		return *a.config.ThinkingDetail
	}
	if c := readGlobalCfg(); c != nil && c.ThinkingDetail != nil {
		return *c.ThinkingDetail
	}
	return false
}

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

// sendDisplay sends a display event to the TUI. May drop if channel is full.
func (a *agent) sendDisplay(msg tea.Msg) {
	if a.msgCh != nil {
		select {
		case a.msgCh <- msg:
		default:
		}
	}
}

// sendCritical sends a critical message (turn done/error) to the TUI, blocking briefly.
func (a *agent) sendCritical(msg tea.Msg) {
	if a.msgCh != nil {
		a.msgCh <- msg
	}
}

// doTurn runs a full agent turn in a goroutine, sending display events to the TUI
// via the msgCh channel. It handles the full loop: stream → tool calls → stream → ...
func (a *agent) doTurn(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			a.sendCritical(turnErrMsg{fmt.Errorf("panic: %v", r)})
		}
	}()

	for {
		select {
		case <-ctx.Done():
			a.sendCritical(turnErrMsg{ctx.Err()})
			return
		default:
		}

		params := openai.ChatCompletionNewParams{
			Model:    openai.ChatModel(a.effectiveModel()),
			Messages: cleanHistory(a.history),
			Tools:    a.tools,
		}
		if a.thinking() {
			params.ReasoningEffort = a.thinkingEffort()
		} else {
			// Explicitly disable thinking on backends that accept the
			// Anthropic-style param (no standard field for this in chat/completions).
			params.SetExtraFields(map[string]any{
				"thinking": map[string]any{"type": "disabled"},
			})
		}

		stream := a.client.Chat.Completions.NewStreaming(ctx, params)
		acc := openai.ChatCompletionAccumulator{}
		hasReasoning := false

		for stream.Next() {
			select {
			case <-ctx.Done():
				a.sendCritical(turnErrMsg{ctx.Err()})
				return
			default:
			}

			chunk := stream.Current()
			acc.AddChunk(chunk)

			if reasoning := extractReasoning(chunk.RawJSON()); reasoning != "" {
				if !hasReasoning {
					hasReasoning = true
				}
				a.reasoningAcc += reasoning
				a.sendDisplay(reasoningMsg(reasoning))
				// Small delay so the TUI can keep up with rendering.
				time.Sleep(5 * time.Millisecond)
			}

			if len(chunk.Choices) > 0 {
				if delta := chunk.Choices[0].Delta.Content; delta != "" {
					a.sendDisplay(contentMsg(delta))
				}
			}
		}

		if err := stream.Err(); err != nil {
			a.sendCritical(turnErrMsg{err})
			return
		}
		if len(acc.Choices) == 0 {
			a.sendCritical(turnErrMsg{fmt.Errorf("empty response (no choices)")})
			return
		}

		msg := acc.Choices[0].Message
		param := msg.ToParam()
		idx := len(a.history)
		a.history = append(a.history, param)
		if a.reasoningAcc != "" {
			if a.reasonings == nil {
				a.reasonings = make(map[int]string)
			}
			a.reasonings[idx] = a.reasoningAcc
			a.reasoningAcc = ""
		}
		a.sessionDirty = true

		if len(msg.ToolCalls) == 0 {
			a.sendCritical(turnDoneMsg{})
			return
		}

		calls := msg.ToolCalls
		denied := false
		for i := range calls {
			call := calls[i]
			select {
			case <-ctx.Done():
				a.appendCancelledResults(calls[i:])
				a.sendCritical(turnErrMsg{ctx.Err()})
				return
			default:
			}

			// Determine if approval is needed.
			needsApproval, toolName, toolDetail := a.toolApprovalInfo(call)

			// Show tool call display.
			a.sendDisplay(toolCallDisplayMsg{name: toolName, detail: toolDetail})

			// For write/edit, show a unified diff of the pending change so the
			// user sees what they're approving.
			if lines := a.toolDiffLines(call); len(lines) > 0 {
				a.sendDisplay(diffDisplayMsg{lines: lines})
			}

			if needsApproval {
				respondCh := make(chan bool, 1)
				a.sendCritical(approvalReqMsg{name: toolName, detail: toolDetail, respond: respondCh})
				var approved bool
				select {
				case approved = <-respondCh:
				case <-ctx.Done():
					a.appendCancelledResults(calls[i:])
					a.sendCritical(turnErrMsg{ctx.Err()})
					return
				}
				if !approved {
					a.sendDisplay(toolResultDisplayMsg{result: "(denied)"})
					a.history = append(a.history, toolDeniedMessage(call))
					denied = true
					continue
				}
			}

			result, toolDenied := a.runToolCall(ctx, call)
			a.history = append(a.history, result)

			// Extract result text for display (skip "read" — too verbose).
			if call.Function.Name != "read" {
				resultText := a.toolResultText(result)
				a.sendDisplay(toolResultDisplayMsg{result: resultText})
			}

			if toolDenied {
				denied = true
			}
		}
		if denied {
			a.sendCritical(turnDoneMsg{})
			return
		}
	}
}

// toolApprovalInfo returns whether a tool requires approval, its display name, and detail.
func (a *agent) toolApprovalInfo(call openai.ChatCompletionMessageToolCall) (needsApproval bool, name, detail string) {
	var args struct {
		Command          string `json:"command"`
		RequiresApproval bool   `json:"requires_approval"`
		Path             string `json:"path"`
		Content          string `json:"content"`
		OldString        string `json:"old_string"`
		NewString        string `json:"new_string"`
		Query            string `json:"query"`
		URL              string `json:"url"`
	}
	json.Unmarshal([]byte(call.Function.Arguments), &args)

	switch call.Function.Name {
	case "bash":
		name = "bash"
		detail = "$ " + args.Command
		return args.RequiresApproval, name, detail
	case "write":
		name = "write"
		detail = fmt.Sprintf("%s (%d bytes)", relPath(args.Path), len(args.Content))
		return !a.autoEdit(), name, detail
	case "edit":
		name = "edit"
		detail = relPath(args.Path)
		return !a.autoEdit(), name, detail
	case "read":
		return false, "read", relPath(args.Path)
	case "web-search":
		return false, "web-search", args.Query
	case "web-fetch":
		return false, "web-fetch", args.URL
	default:
		return false, call.Function.Name, ""
	}
}

// toolDiffLines computes a unified diff for a pending write/edit tool call, or
// nil for other tools (or when the change can't be previewed).
func (a *agent) toolDiffLines(call openai.ChatCompletionMessageToolCall) []string {
	var args struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" {
		return nil
	}
	switch call.Function.Name {
	case "write":
		existing, _ := os.ReadFile(args.Path)
		return diffLines(string(existing), args.Content)
	case "edit":
		data, err := os.ReadFile(args.Path)
		if err != nil {
			return nil
		}
		content := string(data)
		if args.OldString == "" || strings.Count(content, args.OldString) != 1 {
			return nil
		}
		updated := strings.Replace(content, args.OldString, args.NewString, 1)
		return diffLines(content, updated)
	}
	return nil
}

// runToolCall executes a tool call and returns the tool-result message and denied flag.
func (a *agent) runToolCall(ctx context.Context, call openai.ChatCompletionMessageToolCall) (openai.ChatCompletionMessageParamUnion, bool) {
	switch call.Function.Name {
	case "bash":
		return a.runBash(ctx, call)
	case "read":
		return a.readFile(call), false
	case "write":
		return a.writeFile(call)
	case "edit":
		return a.editFile(call)
	case "web-search":
		return a.webSearch(ctx, call), false
	case "web-fetch":
		return a.webFetch(ctx, call), false
	default:
		return openai.ToolMessage("error: unknown tool: "+call.Function.Name, call.ID), false
	}
}

// toolResultText extracts a concise display string from a tool result message.
func (a *agent) toolResultText(msg openai.ChatCompletionMessageParamUnion) string {
	if msg.OfTool != nil {
		content := msg.OfTool.Content.OfString.Value
		// Truncate long results.
		if len(content) > 200 {
			return content[:200] + "..."
		}
		return content
	}
	return ""
}

// toolDeniedMessage creates a tool result for a denied tool call.
func toolDeniedMessage(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	return openai.ToolMessage("error: the user denied permission to run this command", call.ID)
}

// toolCancelledMessage creates a tool result for a cancelled tool call.
func toolCancelledMessage(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	return openai.ToolMessage("error: tool call was cancelled by user (Ctrl-C)", call.ID)
}

// appendCancelledResults appends cancelled tool results for each call in the
// slice and sends a display event for each.
func (a *agent) appendCancelledResults(calls []openai.ChatCompletionMessageToolCall) {
	for _, call := range calls {
		a.history = append(a.history, toolCancelledMessage(call))
		a.sendDisplay(toolResultDisplayMsg{result: "(canceled)"})
	}
}

// userMessage creates a user message for the given line.
func userMessage(line string) openai.ChatCompletionMessageParamUnion {
	return openai.UserMessage(line)
}

// relPath converts an absolute path to a cwd-relative path if it's under cwd.
func relPath(p string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(cwd, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return rel
}

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

// generateSessionSummary makes a non-streaming LLM call to produce a brief
// one-line summary from the given user text. It runs asynchronously and
// persists the result to the session file. userText is passed in explicitly to
// avoid reading a.history from a concurrent goroutine.
func (a *agent) generateSessionSummary(userText string) {
	if userText == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(a.effectiveModel()),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("You are minimal-agent, a coding assistant. Your task here is to generate a short summary that will be used as the title for a coding session. Given the user's first message, produce a brief one-line summary (max 80 characters) of what the session is about. Be specific: mention the language, framework, or task. Return only the summary text, no quotes or prefix."),
			openai.UserMessage("Summarize the following user message into a session title:\n\n" + userText),
		},
		MaxTokens: openai.Int(60),
	}
	// The summary is a trivial one-liner; explicitly disable thinking so no
	// reasoning budget is spent on it.
	params.SetExtraFields(map[string]any{
		"thinking": map[string]any{"type": "disabled"},
	})

	completion, err := a.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return // silently fail; summary is a best-effort feature
	}
	if len(completion.Choices) == 0 {
		return
	}
	summary := strings.TrimSpace(completion.Choices[0].Message.Content)
	if summary == "" {
		return
	}
	// Truncate if the model ignored the limit.
	if len(summary) > 120 {
		summary = summary[:120]
	}
	a.summary = summary
	a.sessionDirty = true
	a.autoSave()
}
