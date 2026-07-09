package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/openai/openai-go"
)

// atMentionRe matches @filepath or @filepath:LN patterns in user input.
// The @ must be at start of input or preceded by whitespace to avoid
// matching email addresses or other uses of @.
// The path character class allows dots, slashes, dashes, and other filename
// characters; colons and whitespace delimit the path.
var atMentionRe = regexp.MustCompile(`(?:^|\s)@([^\s:]+)(?::L?(\d+))?`)

// maxAtMentionSize caps the file content included for a single @mention.
const maxAtMentionSize = 100 * 1024

// lineContextLines is the number of lines of context shown on each side of
// the requested line number when using @filepath:LN.
const lineContextLines = 5

// expandAtMentions scans user input for @filepath and @filepath:LN patterns,
// reads the referenced files, and returns a user message with content parts:
// the first part is the user's original text, followed by one text part per
// file attachment. This gives the LLM immediate context without requiring a
// tool call. Files are registered with rememberFile so the agent's freshness
// tracking works for subsequent edits.
func (a *agent) expandAtMentions(line string) openai.ChatCompletionMessageParamUnion {
	matches := atMentionRe.FindAllStringSubmatch(line, -1)
	if len(matches) == 0 {
		return openai.UserMessage(line)
	}

	parts := []openai.ChatCompletionContentPartUnionParam{
		openai.TextContentPart(line),
	}

	for _, m := range matches {
		path := expandTildePath(m[1])
		lineNum := 0
		if m[2] != "" {
			lineNum, _ = strconv.Atoi(m[2])
		}

		data, err := os.ReadFile(path)
		if err != nil {
			parts = append(parts, openai.TextContentPart(
				fmt.Sprintf("[file: %s]\nerror: %s", m[1], err)))
			continue
		}

		// Skip binary files.
		if bytes.ContainsRune(data, 0) {
			parts = append(parts, openai.TextContentPart(
				fmt.Sprintf("[file: %s]\n(binary file, %d bytes)", m[1], len(data))))
			continue
		}

		a.rememberFile(path)

		content := string(data)
		if len(content) > maxAtMentionSize {
			content = content[:maxAtMentionSize] +
				fmt.Sprintf("\n... (truncated, file is %d bytes)", len(data))
		}

		if lineNum > 0 {
			fileLines := strings.Split(content, "\n")
			if lineNum <= len(fileLines) {
				start := lineNum - lineContextLines
				if start < 0 {
					start = 0
				}
				end := lineNum + lineContextLines
				if end > len(fileLines) {
					end = len(fileLines)
				}
				var b strings.Builder
				for i := start; i < end; i++ {
					marker := "  "
					if i+1 == lineNum {
						marker = "> "
					}
					b.WriteString(fmt.Sprintf("%sL%d: %s\n", marker, i+1, fileLines[i]))
				}
				parts = append(parts, openai.TextContentPart(
					fmt.Sprintf("[file: %s  (lines %d-%d, line %d marked)]\n%s",
						m[1], start+1, end, lineNum, b.String())))
			} else {
				parts = append(parts, openai.TextContentPart(
					fmt.Sprintf("[file: %s]\n(line %d out of range; file has %d lines)",
						m[1], lineNum, len(fileLines))))
			}
		} else {
			parts = append(parts, openai.TextContentPart(
				fmt.Sprintf("[file: %s]\n%s", m[1], content)))
		}
	}

	return openai.UserMessage(parts)
}

// userMessageText extracts the user-typed text from a user message, whether
// it was sent as a plain string or as content parts (the first part is always
// the user's original input).
func userMessageText(msg openai.ChatCompletionMessageParamUnion) string {
	if msg.OfUser == nil {
		return ""
	}
	// Try string form first.
	if s := msg.OfUser.Content.OfString; s.Valid() {
		return s.Value
	}
	// Try content parts: first text part is the user's text.
	for _, part := range msg.OfUser.Content.OfArrayOfContentParts {
		if part.OfText != nil && part.OfText.Text != "" {
			return part.OfText.Text
		}
	}
	return ""
}

// userMessageHasContent returns true if a user message has any content
// (string or content parts).
func userMessageHasContent(msg openai.ChatCompletionMessageParamUnion) bool {
	if msg.OfUser == nil {
		return false
	}
	if s := msg.OfUser.Content.OfString; s.Valid() && s.Value != "" {
		return true
	}
	return len(msg.OfUser.Content.OfArrayOfContentParts) > 0
}

func expandTildePath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	if p == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			return home
		}
	}
	return p
}
