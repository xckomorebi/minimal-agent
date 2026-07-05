package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
		a.history = append(a.history, param)
		a.sessionDirty = true

		if len(msg.ToolCalls) == 0 {
			a.sendCritical(turnDoneMsg{})
			return
		}

		denied := false
		for _, call := range msg.ToolCalls {
			select {
			case <-ctx.Done():
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

			result, toolDenied := a.runToolCall(call)
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
		detail = fmt.Sprintf("%s (%d bytes)", args.Path, len(args.Content))
		return !a.autoEdit(), name, detail
	case "edit":
		name = "edit"
		detail = args.Path
		return !a.autoEdit(), name, detail
	case "read":
		return false, "read", args.Path
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
func (a *agent) runToolCall(call openai.ChatCompletionMessageToolCall) (openai.ChatCompletionMessageParamUnion, bool) {
	switch call.Function.Name {
	case "bash":
		return a.runBash(call)
	case "read":
		return a.readFile(call), false
	case "write":
		return a.writeFile(call)
	case "edit":
		return a.editFile(call)
	case "web-search":
		return a.webSearch(call), false
	case "web-fetch":
		return a.webFetch(call), false
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

// userMessage creates a user message for the given line.
func userMessage(line string) openai.ChatCompletionMessageParamUnion {
	return openai.UserMessage(line)
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
