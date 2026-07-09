package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// maxFileSuggestions limits how many file paths the @mention autocomplete shows.
const maxFileSuggestions = 10

// ignoreDirs are directory names skipped during file search for @mentions.
var ignoreDirs = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"vendor":        true,
	".ma":           true,
	"__pycache__":   true,
	".next":         true,
	".nuxt":         true,
	"dist":          true,
	"build":         true,
	"target":        true,
	".cache":        true,
	".gradle":       true,
	".idea":         true,
	".vscode":       true,
}

// autocompleteFileMention detects an @mention query at the end of the input
// and returns matching file paths (relative to cwd). Returns nil if there is
// no active @mention query.
//
// An @mention is active when the input ends with @query where query is a
// non-empty string of path characters (no whitespace). The @ must be at the
// start of input or preceded by whitespace, matching atMentionRe.
func autocompleteFileMention(input string) []string {
	// Find the last @ that starts a mention: preceded by start-of-string
	// or whitespace.
	atIdx := -1
	for i := len(input) - 1; i >= 0; i-- {
		if input[i] == '@' {
			if i == 0 || input[i-1] == ' ' || input[i-1] == '\t' || input[i-1] == '\n' {
				atIdx = i
				break
			}
		}
		// Stop at whitespace — the @ must be in the current word.
		if input[i] == ' ' || input[i] == '\t' || input[i] == '\n' {
			break
		}
	}
	if atIdx < 0 {
		return nil
	}

	query := input[atIdx+1:]
	// Allow empty query — return all files (up to limit) when just @ is typed.
	return searchFiles(query)
}

// searchFiles recursively walks cwd and returns up to maxFileSuggestions file
// paths that fuzzy-match the query (case-insensitive), sorted by match quality.
// An empty query matches all files.
func searchFiles(query string) []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}

	type match struct {
		path  string
		score int // lower = better match; negative means no match
	}

	var matches []match
	filepath.WalkDir(cwd, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if ignoreDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		score, ok := fuzzyScore(query, rel)
		if !ok {
			return nil
		}
		matches = append(matches, match{path: rel, score: score})
		return nil
	})

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score < matches[j].score
		}
		return matches[i].path < matches[j].path
	})

	limit := len(matches)
	if limit > maxFileSuggestions {
		limit = maxFileSuggestions
	}
	result := make([]string, limit)
	for i := 0; i < limit; i++ {
		result[i] = matches[i].path
	}
	return result
}

// fuzzyScore returns a match quality score for query against target, and
// whether query matches at all. Lower scores are better matches.
//
// The scoring is fzf-style: characters in query must appear in order in
// target (case-insensitive). Consecutive matches, matches at the start of
// path segments, and earlier/tighter matches all reduce the score. Good
// matches routinely score negative, so "no match" is signaled via the bool
// return rather than a sentinel score value.
// An empty query matches anything with score 0.
func fuzzyScore(query, target string) (int, bool) {
	if query == "" {
		return 0, true
	}
	q := strings.ToLower(query)
	t := strings.ToLower(target)

	qi := 0    // position in query
	prev := -2 // previous matched index (-2 = before start, for gap penalty)
	score := 0
	consecutive := 0 // number of consecutive matches so far

	for ti := 0; ti < len(t) && qi < len(q); ti++ {
		if t[ti] != q[qi] {
			continue
		}
		// Matched character qi at target position ti.
		qi++

		// Consecutive bonus: adjacent matches are very good.
		if prev == ti-1 {
			consecutive++
			score -= 5 + consecutive // -5, -6, -7, ... per consecutive char
		} else {
			consecutive = 0
			// Gap penalty: bigger gaps are worse.
			gap := ti - prev - 1
			score += gap * 2

			// Bonus for matching at a path segment boundary (after '/' or at start).
			if ti == 0 || t[ti-1] == '/' {
				score -= 4
			}
		}
		prev = ti
	}

	if qi < len(q) {
		return 0, false // not all query characters found
	}

	// Penalty for unmatched characters after the last match.
	score += (len(t) - prev - 1)

	return score, true
}
