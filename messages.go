package main

import "github.com/openai/openai-go"

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
func isEmptyMessage(msg openai.ChatCompletionMessageParamUnion) bool {
	if msg.OfSystem != nil {
		return msg.OfSystem.Content.OfString.Value == ""
	}
	if msg.OfUser != nil {
		return msg.OfUser.Content.OfString.Value == ""
	}
	if msg.OfAssistant != nil {
		hasContent := msg.OfAssistant.Content.OfString.Value != ""
		hasToolCalls := len(msg.OfAssistant.ToolCalls) > 0
		return !hasContent && !hasToolCalls
	}
	return false
}
