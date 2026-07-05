package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/openai/openai-go"
)

// --- tool definition helpers ---

type property struct {
	name   string
	schema map[string]string
}

func prop(name, typ, description string) property {
	return property{name: name, schema: map[string]string{"type": typ, "description": description}}
}

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

// --- built-in tools ---

func builtinTools() []openai.ChatCompletionToolParam {
	return []openai.ChatCompletionToolParam{
		toolDef("bash", "Run a shell command with bash -c and return its combined stdout/stderr.",
			prop("command", "string", "the shell command to run"),
			prop("requires_approval", "boolean", "set true for destructive/irreversible operations (writes, deletes, installs, network calls, git push); false for read-only inspection (ls, cat, grep, git status, git diff, go build, etc.)"),
		),
		toolDef("read", "Read and return the full contents of a file at the given path.",
			prop("path", "string", "path to the file to read"),
		),
		toolDef("write", "Create or overwrite a file with the given content. Use this for new files; prefer edit for modifying existing files. Creating a new file is unrestricted, but overwriting an existing one follows the same rule as edit: you must already know its current contents (from an earlier read/write/edit this session), and it must not have changed on disk since — otherwise read it again first.",
			prop("path", "string", "path to the file to write"),
			prop("content", "string", "the full content to write to the file"),
		),
		toolDef("edit", "Modify an existing file by replacing a single, unique occurrence of old_string with new_string. old_string must match the file byte-for-byte (including whitespace) and appear exactly once; include enough surrounding context to make it unambiguous. Editing only requires that you already know the file's current contents — from reading it earlier this session or from your own prior write/edit; you need not re-read right before each edit. Edit fails only if you have never seen the file, or if it changed on disk since you last saw it (read it again to pick up the new contents).",
			prop("path", "string", "path to the file to edit"),
			prop("old_string", "string", "the exact existing text to replace; must be unique within the file"),
			prop("new_string", "string", "the replacement text"),
		),
		toolDef("web-search", "Search the web using DuckDuckGo and return the top results as formatted text (title, URL, snippet). Use this to look up current information, documentation, or answers to questions.",
			prop("query", "string", "the search query"),
			prop("num", "integer", "number of results to return (default 5, max 10)"),
		),
		toolDef("web-fetch", "Fetch the content of a URL and return it as readable text. Strips HTML down to plain text. Use this to read documentation, blog posts, or any page found via web-search. Returns up to 50KB of text.",
			prop("url", "string", "the URL to fetch"),
		),
	}
}

// --- external tools (placeholder) ---

// Register external tools by appending to this slice at init time.
var externalTools []openai.ChatCompletionToolParam

// --- combined ---

func allTools() []openai.ChatCompletionToolParam {
	return append(builtinTools(), externalTools...)
}

// --- tool dispatch ---

// --- tool implementations ---

func (a *agent) runBash(ctx context.Context, call openai.ChatCompletionMessageToolCall) (openai.ChatCompletionMessageParamUnion, bool) {
	var args struct {
		Command          string `json:"command"`
		RequiresApproval bool   `json:"requires_approval"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Command == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"command": "..."}`, call.ID), false
	}

	// Approval is handled by the TUI before this is called.
	// Use a process group so that cancel kills bash and all its children.
	cmd := exec.Command("bash", "-c", args.Command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID), false
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		result := out.String()
		if err != nil {
			// Distinguish context cancel from real errors.
			if ctx.Err() != nil {
				return openai.ToolMessage("error: tool call was cancelled by user (Ctrl-C)", call.ID), false
			}
			result += "\n[exit: " + err.Error() + "]"
		}
		if result == "" {
			result = "(no output)"
		}
		return openai.ToolMessage(result, call.ID), false
	case <-ctx.Done():
		// Kill the entire process group so children don't outlive the cancel.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return openai.ToolMessage("error: tool call was cancelled by user (Ctrl-C)", call.ID), false
	}
}

func (a *agent) readFile(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "..."}`, call.ID)
	}

	data, err := os.ReadFile(args.Path)
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	a.rememberFile(args.Path)
	if len(data) == 0 {
		return openai.ToolMessage("(empty file)", call.ID)
	}
	return openai.ToolMessage(string(data), call.ID)
}

func (a *agent) writeFile(call openai.ChatCompletionMessageToolCall) (openai.ChatCompletionMessageParamUnion, bool) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "...", "content": "..."}`, call.ID), false
	}

	// Overwriting an existing file follows the same freshness rule as edit: you
	// must already know its current contents. Creating a new file is unrestricted.
	if _, err := os.Stat(args.Path); err == nil {
		if msg := a.checkFileFresh(args.Path); msg != "" {
			return openai.ToolMessage(msg, call.ID), false
		}
	}

	// Approval is handled by the TUI before this is called.

	if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID), false
	}
	a.rememberFile(args.Path)
	return openai.ToolMessage(fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), call.ID), false
}

func (a *agent) editFile(call openai.ChatCompletionMessageToolCall) (openai.ChatCompletionMessageParamUnion, bool) {
	var args struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" || args.OldString == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "...", "old_string": "...", "new_string": "..."}`, call.ID), false
	}

	if msg := a.checkFileFresh(args.Path); msg != "" {
		return openai.ToolMessage(msg, call.ID), false
	}

	data, err := os.ReadFile(args.Path)
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID), false
	}
	content := string(data)

	switch strings.Count(content, args.OldString) {
	case 1:
	case 0:
		return openai.ToolMessage("error: old_string not found in "+args.Path+"; read the file and retry with text that matches exactly", call.ID), false
	default:
		return openai.ToolMessage("error: old_string matches multiple times in "+args.Path+"; add surrounding context to make it unique", call.ID), false
	}

	// Approval is handled by the TUI before this is called.

	updated := strings.Replace(content, args.OldString, args.NewString, 1)
	if err := os.WriteFile(args.Path, []byte(updated), 0644); err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID), false
	}
	a.rememberFile(args.Path)
	return openai.ToolMessage("edited "+args.Path, call.ID), false
}

// ddgSearchRate limits how fast we hit DuckDuckGo.
var ddgSearchRate = time.NewTicker(800 * time.Millisecond)

func (a *agent) webSearch(ctx context.Context, call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Query string `json:"query"`
		Num   int    `json:"num,omitempty"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Query == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"query": "..."}`, call.ID)
	}
	if args.Num <= 0 {
		args.Num = 5
	}
	if args.Num > 10 {
		args.Num = 10
	}

	// Rate-limit to avoid triggering bot detection.
	select {
	case <-ddgSearchRate.C:
	case <-ctx.Done():
		return openai.ToolMessage("error: tool call was cancelled by user (Ctrl-C)", call.ID)
	}

	form := url.Values{"q": {args.Query}}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Lynx/2.9.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return openai.ToolMessage("error: tool call was cancelled by user (Ctrl-C)", call.ID)
		}
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	htmlBody := string(body)

	// Parse result links: class="result__a" href="..." > title <
	linkRe := regexp.MustCompile(`class="result__a"[^>]*href="([^"]*)"[^>]*>([^<]*)<`)
	// Parse result snippets: class="result__snippet" > ... <
	snippetRe := regexp.MustCompile(`class="result__snippet"[^>]*>(.*?)</`)

	linkMatches := linkRe.FindAllStringSubmatch(htmlBody, -1)
	snippetMatches := snippetRe.FindAllStringSubmatch(htmlBody, -1)

	count := args.Num
	if count > len(linkMatches) {
		count = len(linkMatches)
	}
	if count == 0 {
		return openai.ToolMessage("no results found for: "+args.Query, call.ID)
	}

	var results []string
	for i := 0; i < count; i++ {
		href := linkMatches[i][1]
		title := linkMatches[i][2]

		// Decode the DuckDuckGo redirect URL.
		realURL := decodeDDGRedirect(href)

		snippet := ""
		if i < len(snippetMatches) {
			snippet = cleanHTML(snippetMatches[i][1])
		}

		var b strings.Builder
		b.WriteString(fmt.Sprintf("%d. %s\n", i+1, strings.TrimSpace(title)))
		b.WriteString(fmt.Sprintf("   %s\n", realURL))
		if snippet != "" {
			b.WriteString(fmt.Sprintf("   %s\n", snippet))
		}
		results = append(results, b.String())
	}
	return openai.ToolMessage(strings.Join(results, "\n"), call.ID)
}

func (a *agent) webFetch(ctx context.Context, call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.URL == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"url": "..."}`, call.ID)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", args.URL, nil)
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	req.Header.Set("User-Agent", "Lynx/2.9.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return openai.ToolMessage("error: tool call was cancelled by user (Ctrl-C)", call.ID)
		}
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	defer resp.Body.Close()

	// Reject non-text content types.
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		mt := strings.SplitN(ct, ";", 2)[0]
		switch mt {
		case "text/html", "text/plain", "application/xhtml+xml", "application/xml", "text/xml":
		default:
			return openai.ToolMessage(fmt.Sprintf("error: unsupported content type %s — only text/* and */html are supported", mt), call.ID)
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024)) // 2MB max
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}

	text := htmlToText(string(body))
	limit := 50 * 1024
	if len(text) > limit {
		text = text[:limit] + fmt.Sprintf("\n\n... truncated at %d bytes (full page was %d bytes)", limit, len(text))
	}
	if text == "" {
		return openai.ToolMessage("(empty page — no readable text content)", call.ID)
	}
	return openai.ToolMessage(text, call.ID)
}

// htmlToText strips HTML down to readable plain text.
func htmlToText(html string) string {
	// Remove scripts and styles.
	s := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).ReplaceAllString(html, "")
	s = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`).ReplaceAllString(s, "")

	// Replace <br> and block-level tags with newlines.
	s = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`(?i)</?(p|div|h[1-6]|li|tr|article|section|header|footer|nav|main|aside|table|thead|tbody|tfoot|dl|dt|dd|pre|blockquote|hr|figure|figcaption)[^>]*>`).ReplaceAllString(s, "\n")

	// Remove all remaining tags.
	s = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(s, "")

	// Decode entities.
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#x27;", "'")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")

	// Collapse repeated whitespace and blank lines.
	lines := strings.Split(s, "\n")
	var out []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		// Collapse multiple spaces.
		line = regexp.MustCompile(`\s+`).ReplaceAllString(line, " ")
		out = append(out, line)
	}
	// Trim trailing blank lines.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// decodeDDGRedirect extracts the real URL from DuckDuckGo's redirect wrapper.
// DDG wraps result links as //duckduckgo.com/l/?uddg=ENCODED_URL&rut=...
func decodeDDGRedirect(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	// Handle protocol-relative URLs.
	if u.Scheme == "" && strings.HasPrefix(href, "//") {
		u, err = url.Parse("https:" + href)
		if err != nil {
			return href
		}
	}
	encoded := u.Query().Get("uddg")
	if encoded == "" {
		return href
	}
	decoded, err := url.QueryUnescape(encoded)
	if err != nil {
		return encoded
	}
	return decoded
}

// cleanHTML strips HTML tags and decodes common entities from snippet text.
func cleanHTML(s string) string {
	s = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#x27;", "'")
	s = strings.ReplaceAll(s, "&#39;", "'")
	return strings.TrimSpace(s)
}
