package main

import (
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
)

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

// stripReasoningContent removes the reasoning_content extra field from all
// assistant messages in-place. This is used when send_reasoning is disabled:
// reasoning is still persisted in session files and displayed in the TUI, but
// not sent to the API.
func stripReasoningContent(msgs []openai.ChatCompletionMessageParamUnion) {
	for i := range msgs {
		if msgs[i].OfAssistant == nil {
			continue
		}
		extras := msgs[i].OfAssistant.ExtraFields()
		if extras == nil {
			continue
		}
		if _, ok := extras["reasoning_content"]; !ok {
			continue
		}
		// Rebuild extra fields, replacing reasoning_content with Omit so the
		// SDK drops it from the serialized JSON.
		filtered := make(map[string]any, len(extras))
		for k, v := range extras {
			if k == "reasoning_content" {
				filtered[k] = param.Omit
			} else {
				filtered[k] = v
			}
		}
		msgs[i].OfAssistant.SetExtraFields(filtered)
	}
}

// isEmptyMessage returns true if a message has no content AND no tool_calls.
func isEmptyMessage(msg openai.ChatCompletionMessageParamUnion) bool {
	if msg.OfSystem != nil {
		return msg.OfSystem.Content.OfString.Value == ""
	}
	if msg.OfUser != nil {
		return !userMessageHasContent(msg)
	}
	if msg.OfAssistant != nil {
		hasContent := msg.OfAssistant.Content.OfString.Value != ""
		hasToolCalls := len(msg.OfAssistant.ToolCalls) > 0
		return !hasContent && !hasToolCalls
	}
	return false
}
