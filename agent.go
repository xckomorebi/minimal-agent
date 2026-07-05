package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/shared"
)

type agent struct {
	client       openai.Client
	flagModel    string
	tools        []openai.ChatCompletionToolParam
	history      []openai.ChatCompletionMessageParamUnion
	config       sessionConfig
	in           *bufio.Scanner
	sessionName  string
	sessionDirty bool
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
		param := msg.ToParam()
		a.history = append(a.history, param)
		a.sessionDirty = true

		if len(msg.ToolCalls) == 0 {
			return nil
		}
		denied := false
		for _, call := range msg.ToolCalls {
			result, toolDenied := a.runTool(call)
			a.history = append(a.history, result)
			if toolDenied {
				denied = true
			}
		}
		if denied {
			return nil
		}
	}
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


