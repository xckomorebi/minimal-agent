package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/shared"
)

type agent struct {
	client            openai.Client
	flagModel         string
	flagContextWindow int64 // 0 means unset
	tools             []openai.ChatCompletionToolParam
	history           []openai.ChatCompletionMessageParamUnion
	config            sessionConfig
	sessionName       string
	sessionDirty      bool
	msgCh             chan tea.Msg // channel to send events to the TUI

	// tokenUsage tracks the token count for the current conversation state.
	// It is set (not accumulated) after each turn so it reflects the last
	// request's usage. Persisted in the session JSON.
	tokenUsage tokenUsage

	// reasonings stores the full reasoning text for each assistant message in
	// history, keyed by the message's index in the history slice. Reasoning is
	// persisted inline in the session JSON (as reasoning_content on assistant
	// messages) and sent back to the API on subsequent turns. This in-memory
	// map is used for TUI rendering (e.g. "show thinking detail").
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

// tokenUsage tracks token counts for the current conversation state.
type tokenUsage struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Total      int64 `json:"total"`
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
	if c := readGlobalCfg(); c != nil {
		if pm := c.profileModel(); pm != "" {
			return pm
		}
		if c.Model != nil && *c.Model != "" {
			return *c.Model
		}
	}
	if m := os.Getenv("MA_MODEL"); m != "" {
		return m
	}
	return "gpt-4o"
}

func (a *agent) contextWindow() int64 {
	if a.flagContextWindow > 0 {
		return a.flagContextWindow
	}
	if c := readGlobalCfg(); c != nil {
		if p := c.resolvedProfile(); p != nil && p.ContextWindow != nil && *p.ContextWindow > 0 {
			return *p.ContextWindow
		}
		if c.ContextWindow != nil && *c.ContextWindow > 0 {
			return *c.ContextWindow
		}
	}
	return 200000
}

func (a *agent) autoEdit() bool {
	if a.config.AutoEdit != nil {
		return *a.config.AutoEdit
	}
	if c := readGlobalCfg(); c != nil {
		if p := c.resolvedProfile(); p != nil && p.AutoEdit != nil {
			return *p.AutoEdit
		}
		if c.AutoEdit != nil {
			return *c.AutoEdit
		}
	}
	return false
}

func (a *agent) stream() bool {
	if a.config.Stream != nil {
		return *a.config.Stream
	}
	if c := readGlobalCfg(); c != nil {
		if p := c.resolvedProfile(); p != nil && p.Stream != nil {
			return *p.Stream
		}
		if c.Stream != nil {
			return *c.Stream
		}
	}
	return true
}

func (a *agent) thinking() bool {
	if a.config.Thinking != nil {
		return *a.config.Thinking
	}
	if c := readGlobalCfg(); c != nil {
		if p := c.resolvedProfile(); p != nil && p.Thinking != nil {
			return *p.Thinking
		}
		if c.Thinking != nil {
			return *c.Thinking
		}
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
		if p := c.resolvedProfile(); p != nil {
			if v, ok := resolve(p.ThinkingEffort); ok {
				return v
			}
		}
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
	if c := readGlobalCfg(); c != nil {
		if p := c.resolvedProfile(); p != nil && p.ThinkingDetail != nil {
			return *p.ThinkingDetail
		}
		if c.ThinkingDetail != nil {
			return *c.ThinkingDetail
		}
	}
	return false
}

// sendReasoning controls whether reasoning_content is included in assistant
// messages sent to the API. When false, reasoning is still persisted in
// session files and displayed in the TUI, but stripped from outgoing requests.
func (a *agent) sendReasoning() bool {
	if a.config.SendReasoning != nil {
		return *a.config.SendReasoning
	}
	if c := readGlobalCfg(); c != nil {
		if p := c.resolvedProfile(); p != nil && p.SendReasoning != nil {
			return *p.SendReasoning
		}
		if c.SendReasoning != nil {
			return *c.SendReasoning
		}
	}
	return true
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
	if a.stream() {
		a.doTurnStreaming(ctx)
	} else {
		a.doTurnNonStreaming(ctx)
	}
}

// doTurnStreaming runs the agent turn using the streaming API.
func (a *agent) doTurnStreaming(ctx context.Context) {
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

		params := a.buildCompletionParams()
		stream := a.client.Chat.Completions.NewStreaming(ctx, params)
		acc := openai.ChatCompletionAccumulator{}

		// lastUsage tracks the most recent non-zero usage chunk. Some API
		// providers (e.g. DeepSeek) include usage in every streaming chunk;
		// the SDK accumulator sums them with +=, producing inflated counts.
		// We capture the last non-zero usage instead.
		var lastUsage openai.CompletionUsage

		for stream.Next() {
			select {
			case <-ctx.Done():
				a.sendCritical(turnErrMsg{ctx.Err()})
				return
			default:
			}

			chunk := stream.Current()
			acc.AddChunk(chunk)

			// Capture usage from chunks that carry it. With the standard
			// OpenAI API only the final chunk has usage; with other providers
			// every chunk may carry it. Either way, the last non-zero one is
			// the correct total for the entire request.
			if cu := chunk.Usage; cu.PromptTokens > 0 || cu.CompletionTokens > 0 {
				lastUsage = cu
			}

			if reasoning := extractReasoning(chunk.RawJSON()); reasoning != "" {
				a.reasoningAcc += reasoning
				a.sendDisplay(reasoningMsg(reasoning))
				time.Sleep(5 * time.Millisecond)
			}

			if len(chunk.Choices) > 0 {
				if delta := chunk.Choices[0].Delta.Content; delta != "" {
					a.sendDisplay(contentMsg(delta))
				}
			}
		}

		if err := stream.Err(); err != nil {
			slog.Error("LLM stream failed", "model", a.effectiveModel(), errAttrs(err))
			a.sendCritical(turnErrMsg{err})
			return
		}

		u := lastUsage
		a.tokenUsage.Prompt = u.PromptTokens
		a.tokenUsage.Completion = u.CompletionTokens
		a.tokenUsage.Total = u.TotalTokens
		slog.Debug("turn complete", "prompt", u.PromptTokens, "completion", u.CompletionTokens, "total", u.TotalTokens)
		if len(acc.Choices) == 0 {
			a.sendCritical(turnErrMsg{fmt.Errorf("empty response (no choices)")})
			return
		}

		msg := acc.Choices[0].Message
		if !a.processAssistantMessage(ctx, msg) {
			return
		}
	}
}

// doTurnNonStreaming runs the agent turn using the non-streaming API.
func (a *agent) doTurnNonStreaming(ctx context.Context) {
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

		params := a.buildCompletionParams()
		completion, err := a.client.Chat.Completions.New(ctx, params)
		if err != nil {
			slog.Error("LLM completion failed", "model", a.effectiveModel(), errAttrs(err))
			a.sendCritical(turnErrMsg{err})
			return
		}

		u := completion.Usage
		a.tokenUsage.Prompt = u.PromptTokens
		a.tokenUsage.Completion = u.CompletionTokens
		a.tokenUsage.Total = u.TotalTokens
		slog.Debug("turn complete", "prompt", u.PromptTokens, "completion", u.CompletionTokens, "total", u.TotalTokens)

		if len(completion.Choices) == 0 {
			a.sendCritical(turnErrMsg{fmt.Errorf("empty response (no choices)")})
			return
		}

		msg := completion.Choices[0].Message

		// Extract reasoning from the non-streaming response if available.
		if reasoning := extractReasoningNonStreaming(completion.RawJSON()); reasoning != "" {
			a.reasoningAcc = reasoning
			a.sendDisplay(reasoningMsg(reasoning))
		}

		// Send the full content as one display event.
		if content := msg.Content; content != "" {
			a.sendDisplay(contentMsg(content))
		}

		if !a.processAssistantMessage(ctx, msg) {
			return
		}
	}
}

// buildCompletionParams constructs the params shared by both streaming and
// non-streaming paths.
func (a *agent) buildCompletionParams() openai.ChatCompletionNewParams {
	msgs := cleanHistory(a.history)
	if !a.sendReasoning() {
		stripReasoningContent(msgs)
	}
	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(a.effectiveModel()),
		Messages: msgs,
		Tools:    a.tools,
	}
	if a.thinking() {
		params.ReasoningEffort = a.thinkingEffort()
	} else {
		params.SetExtraFields(map[string]any{
			"thinking": map[string]any{"type": "disabled"},
		})
	}
	return params
}

// processAssistantMessage appends the assistant message to history, stores
// reasoning, and processes tool calls. Returns false if the turn should end
// (denial without reason, error, or no more tool calls to process).
func (a *agent) processAssistantMessage(ctx context.Context, msg openai.ChatCompletionMessage) bool {
	param := msg.ToParam()
	idx := len(a.history)
	if a.reasoningAcc != "" {
		// Attach reasoning_content to the assistant message param so it is
		// included in subsequent API requests and persisted in session files.
		// SetExtraFields must be called on the inner assistant param, not the
		// union — the union's MarshalJSON delegates to the inner variant.
		if param.OfAssistant != nil {
			param.OfAssistant.SetExtraFields(map[string]any{"reasoning_content": a.reasoningAcc})
		}
		if a.reasonings == nil {
			a.reasonings = make(map[int]string)
		}
		a.reasonings[idx] = a.reasoningAcc
		a.reasoningAcc = ""
	}
	a.history = append(a.history, param)
	a.sessionDirty = true

	if len(msg.ToolCalls) == 0 {
		a.sendCritical(turnDoneMsg{})
		return false
	}

	calls := msg.ToolCalls
	deniedNoReason := false
	for i := range calls {
		call := calls[i]
		select {
		case <-ctx.Done():
			a.appendCancelledResults(calls[i:])
			a.sendCritical(turnErrMsg{ctx.Err()})
			return false
		default:
		}

		needsApproval, toolName, toolDetail := a.toolApprovalInfo(call)
		a.sendDisplay(toolCallDisplayMsg{name: toolName, detail: toolDetail})

		if lines := a.toolDiffLines(call); len(lines) > 0 {
			a.sendDisplay(diffDisplayMsg{lines: lines})
		}

		if needsApproval {
			respondCh := make(chan approvalAnswer, 1)
			a.sendCritical(approvalReqMsg{name: toolName, detail: toolDetail, respond: respondCh})
			var answer approvalAnswer
			select {
			case answer = <-respondCh:
			case <-ctx.Done():
				a.appendCancelledResults(calls[i:])
				a.sendCritical(turnErrMsg{ctx.Err()})
				return false
			}
			if !answer.approved {
				a.sendDisplay(toolResultDisplayMsg{result: "(denied)"})
				a.history = append(a.history, toolDeniedMessage(call, answer.reason))
				if answer.reason == "" {
					deniedNoReason = true
				}
				continue
			}
			// Re-send tool call display so the TUI re-enters the pending-tool
			// state and shows a blinking dot during the actual execution.
			a.sendDisplay(toolCallDisplayMsg{name: toolName, detail: toolDetail})
		}

		result := a.runToolCall(ctx, call)
		a.history = append(a.history, result)

		if call.Function.Name != "read" && call.Function.Name != "skill" {
			resultText := a.toolResultText(result)
			a.sendDisplay(toolResultDisplayMsg{result: resultText})
		}
	}
	if deniedNoReason {
		a.sendCritical(turnDoneMsg{})
		return false
	}
	return true
}

// toolApprovalInfo returns whether a tool requires approval, its display name, and detail.
func (a *agent) toolApprovalInfo(call openai.ChatCompletionMessageToolCall) (needsApproval bool, name, detail string) {
	var args struct {
		Command          string `json:"command"`
		RequiresApproval bool   `json:"requires_approval"`
		FilePath         string `json:"file_path"`
		Offset           *int   `json:"offset,omitempty"`
		Limit            *int   `json:"limit,omitempty"`
		Content          string `json:"content"`
		OldString        string `json:"old_string"`
		NewString        string `json:"new_string"`
		Query            string `json:"query"`
		URL              string `json:"url"`
		Name             string `json:"name"`
		Question         string `json:"question"`
	}
	json.Unmarshal([]byte(call.Function.Arguments), &args)

	switch call.Function.Name {
	case "bash":
		name = "bash"
		detail = "$ " + args.Command
		return args.RequiresApproval, name, detail
	case "write":
		name = "write"
		detail = fmt.Sprintf("%s (%d bytes)", relPath(args.FilePath), len(args.Content))
		return !a.autoEdit(), name, detail
	case "edit":
		name = "edit"
		detail = relPath(args.FilePath)
		return !a.autoEdit(), name, detail
	case "read":
		detail := relPath(args.FilePath)
		if args.Offset != nil {
			start := *args.Offset
			if args.Limit != nil {
				detail = fmt.Sprintf("%s:%d-%d", detail, start, start+*args.Limit-1)
			} else {
				detail = fmt.Sprintf("%s:%d+", detail, start)
			}
		}
		return false, "read", detail
	case "web-search":
		return false, "web-search", args.Query
	case "web-fetch":
		return false, "web-fetch", args.URL
	case "skill":
		return false, "skill", args.Name
	case "ask_user_question":
		return false, "ask", args.Question
	default:
		// External tools (MCP, etc.) require approval by default.
		if strings.HasPrefix(call.Function.Name, "mcp__") {
			server, tool := parseMCPToolName(call.Function.Name)
			return true, "mcp:" + server, tool
		}
		return false, call.Function.Name, ""
	}
}

// toolDiffLines computes a unified diff for a pending write/edit tool call, or
// nil for other tools (or when the change can't be previewed).
func (a *agent) toolDiffLines(call openai.ChatCompletionMessageToolCall) []string {
	var args struct {
		FilePath  string `json:"file_path"`
		Content   string `json:"content"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.FilePath == "" {
		return nil
	}
	switch call.Function.Name {
	case "write":
		existing, _ := os.ReadFile(args.FilePath)
		return diffLines(string(existing), args.Content)
	case "edit":
		data, err := os.ReadFile(args.FilePath)
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

// runToolCall executes a tool call and returns the tool-result message.
func (a *agent) runToolCall(ctx context.Context, call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	// Route MCP-prefixed tool calls to the MCP layer.
	if strings.HasPrefix(call.Function.Name, "mcp__") {
		return a.runMCPTool(ctx, call)
	}
	switch call.Function.Name {
	case "bash":
		return a.runBash(ctx, call)
	case "read":
		return a.readFile(call)
	case "write":
		return a.writeFile(call)
	case "edit":
		return a.editFile(call)
	case "web-search":
		return a.webSearch(ctx, call)
	case "web-fetch":
		return a.webFetch(ctx, call)
	case "skill":
		return a.runSkill(call)
	case "ask_user_question":
		return a.askUserQuestion(ctx, call)
	default:
		return openai.ToolMessage("error: unknown tool: "+call.Function.Name, call.ID)
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
// If reason is non-empty, it's included in the error message so the LLM
// can adapt to the user's feedback.
func toolDeniedMessage(call openai.ChatCompletionMessageToolCall, reason string) openai.ChatCompletionMessageParamUnion {
	if reason != "" {
		return openai.ToolMessage("error: the user denied the tool call with reason: "+reason, call.ID)
	}
	return openai.ToolMessage("error: the user denied the tool call", call.ID)
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

// extractReasoningNonStreaming extracts reasoning content from a non-streaming
// completion response's raw JSON. In non-streaming mode, reasoning is under
// choices[0].message.reasoning_content rather than choices[0].delta.reasoning_content.
func extractReasoningNonStreaming(raw string) string {
	var resp struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return ""
	}
	if len(resp.Choices) == 0 {
		return ""
	}
	return resp.Choices[0].Message.ReasoningContent
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
		slog.Error("session summary failed", "model", a.effectiveModel(), errAttrs(err))
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

// compactHistory sends the full conversation history to the LLM and asks it to
// produce a detailed summary. It then replaces the history with: system
// message + summary-as-user + assistant acknowledgment. Runs in a goroutine
// and communicates results back to the TUI via the msgCh.
func (a *agent) compactHistory() {
	slog.Debug("compaction starting", "msg_count", len(a.history))
	// Save the system message (always the first message).
	sysMsg := a.history[0]

	// Build a summarization request from the current history.
	var b strings.Builder
	for _, msg := range a.history[1:] {
		if msg.OfUser != nil {
			b.WriteString("User: ")
			b.WriteString(userMessageText(msg))
			b.WriteString("\n")
		}
		if msg.OfAssistant != nil {
			if text := msg.OfAssistant.Content.OfString.Value; text != "" {
				b.WriteString("Assistant: ")
				b.WriteString(text)
				b.WriteString("\n")
			}
			for _, tc := range msg.OfAssistant.ToolCalls {
				fmt.Fprintf(&b, "Assistant: tool call %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			}
		}
		if msg.OfTool != nil {
			result := msg.OfTool.Content.OfString.Value
			if len(result) > 500 {
				result = result[:500] + "..."
			}
			fmt.Fprintf(&b, "Tool result: %s\n", result)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(a.effectiveModel()),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("You are a conversation summarizer. Your task is to produce a detailed but concise summary of the conversation below. Include: the user's requests, the agent's approach, key decisions made, files examined or modified, and any important context that would help a coding agent pick up where it left off. Write in prose, as a narrative summary. Be thorough about technical details: include file names, function names, code snippets where relevant, and the reasoning behind changes. Do not use bullet points — write continuous paragraphs."),
			openai.UserMessage("Summarize this conversation:\n\n" + b.String()),
		},
		MaxTokens: openai.Int(4096),
	}
	params.SetExtraFields(map[string]any{
		"thinking": map[string]any{"type": "disabled"},
	})

	completion, err := a.client.Chat.Completions.New(ctx, params)
	if err != nil {
		slog.Error("history compaction failed", "model", a.effectiveModel(), errAttrs(err))
		a.sendCritical(turnErrMsg{fmt.Errorf("compact failed: %w", err)})
		return
	}
	if len(completion.Choices) == 0 {
		a.sendCritical(turnErrMsg{fmt.Errorf("compact: empty response")})
		return
	}

	summary := strings.TrimSpace(completion.Choices[0].Message.Content)
	if summary == "" {
		a.sendCritical(turnErrMsg{fmt.Errorf("compact: empty summary")})
		return
	}

	// After compaction, the new conversation consists of the system prompt
	// plus the summary (which was the compact call's completion). So the
	// prompt token count is: system prompt (estimated as len/4) + compact completion tokens.
	// Completion resets to 0.
	u := completion.Usage
	sysTokens := int64(len(sysMsg.OfSystem.Content.OfString.Value) / 4)
	a.tokenUsage.Prompt = sysTokens + u.CompletionTokens
	a.tokenUsage.Completion = 0
	a.tokenUsage.Total = sysTokens + u.CompletionTokens

	// Replace history.
	oldCount := len(a.history)
	a.history = []openai.ChatCompletionMessageParamUnion{
		sysMsg,
		openai.UserMessage("Here is a summary of the conversation so far:\n\n" + summary + "\n\nContinue helping the user based on this summary. Remember the context, decisions, and any files discussed."),
	}
	a.reasonings = nil // reasoning is tied to old message indices
	a.sessionDirty = true

	newCount := len(a.history)
	result := fmt.Sprintf("compacted %d messages → %d messages", oldCount, newCount)

	slog.Debug("compaction complete", "old_count", oldCount, "new_count", newCount, "summary_tokens", u.CompletionTokens)
	a.sendCritical(compactDoneMsg{result: result})
}
