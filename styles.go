package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/lipgloss"
	"github.com/openai/openai-go"
)

// agentBar is the gutter glyph prefixed (in green) to every line of agent
// output.
const agentBar = "▎"

func boolPtr(b bool) *bool       { return &b }
func uintPtr(u uint) *uint       { return &u }
func stringPtr(s string) *string { return &s }

var (
	// Base style for the application.
	appStyle = lipgloss.NewStyle().
			PaddingLeft(1).PaddingRight(1)

	// You (user) prefix: bold cyan.
	youStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("6"))

	// Slash command text in history: bright blue.
	cmdTextStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("12"))

	// Agent gutter: bold green. Agent messages are marked by a colored bar
	// running down the left of the block, not a prompt label.
	agentStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("2"))

	// Thinking star: magenta, blinking controlled by toggle.
	thinkStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("5"))

	// Reasoning text: dim italic.
	reasonStyle = lipgloss.NewStyle().
			Faint(true).
			Italic(true)

	// Tool dot and label: bold yellow.
	toolDotStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("3"))

	// Error: bold red.
	errStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("1"))

	// Success: bold green.
	okStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("2"))

	// Dim / muted text.
	dimStyle = lipgloss.NewStyle().
			Faint(true)

	// Input prompt.
	inputPromptStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("6"))

	// Approval prompt.
	approvalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("3"))

	// Question text.
	questionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("6"))

	// Diff added line: green.
	diffAddStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("2"))

	// Diff removed line: red.
	diffDelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("1"))

	// Diff hunk header: cyan.
	diffHunkStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("6"))
)

// diffLines renders a unified diff between old and new full-file content as
// styled visual lines (indented to align under the tool call). Returns nil
// when the content is identical. A new file (empty oldContent) is shown as all
// additions.
func diffLines(oldContent, newContent string) []string {
	split := func(s string) []string {
		if s == "" {
			return []string{""}
		}
		return strings.Split(s, "\n")
	}
	oldLines := split(oldContent)
	newLines := split(newContent)

	var out []string
	// gutter renders a 4-wide line-number column (blank for no number).
	gutter := func(n int) string {
		if n <= 0 {
			return "    "
		}
		return fmt.Sprintf("%4d", n)
	}
	add := func(n int, s string) {
		out = append(out, "  "+dimStyle.Render(gutter(n))+" "+diffAddStyle.Render("+"+s))
	}
	del := func(n int, s string) {
		out = append(out, "  "+dimStyle.Render(gutter(n))+" "+diffDelStyle.Render("-"+s))
	}
	ctx := func(n int, s string) {
		out = append(out, "  "+dimStyle.Render(gutter(n))+" "+dimStyle.Render(" "+s))
	}
	hunk := func(s string) { out = append(out, "  "+diffHunkStyle.Render(s)) }

	// New file: show every line as an addition.
	if oldContent == "" {
		hunk(fmt.Sprintf("@@ -0,0 +1,%d @@", len(newLines)))
		for i, line := range newLines {
			add(i+1, line)
		}
		return out
	}

	// Common prefix.
	commonPrefix := 0
	for commonPrefix < len(oldLines) && commonPrefix < len(newLines) &&
		oldLines[commonPrefix] == newLines[commonPrefix] {
		commonPrefix++
	}
	// Common suffix (after the prefix).
	commonSuffix := 0
	oi := len(oldLines) - 1
	ni := len(newLines) - 1
	for commonSuffix < len(oldLines)-commonPrefix && commonSuffix < len(newLines)-commonPrefix &&
		oldLines[oi] == newLines[ni] {
		commonSuffix++
		oi--
		ni--
	}

	// Identical content: nothing to show.
	if commonPrefix == len(oldLines) && commonPrefix == len(newLines) {
		return nil
	}

	ctxBefore := min(3, commonPrefix)
	ctxAfter := min(3, commonSuffix)

	startLine := commonPrefix - ctxBefore + 1
	oldCount := (len(oldLines) - commonPrefix - commonSuffix) + ctxBefore + ctxAfter
	newCount := (len(newLines) - commonPrefix - commonSuffix) + ctxBefore + ctxAfter
	hunk(fmt.Sprintf("@@ -%d,%d +%d,%d @@", startLine, oldCount, startLine, newCount))

	// Walk with parallel old/new line counters. Context lines advance both and
	// show the new-file number; removals show the old number; additions the new.
	oldLn := commonPrefix - ctxBefore + 1
	newLn := commonPrefix - ctxBefore + 1

	// Context before the change.
	for i := commonPrefix - ctxBefore; i < commonPrefix; i++ {
		ctx(newLn, oldLines[i])
		oldLn++
		newLn++
	}
	// Removed lines.
	for i := commonPrefix; i < len(oldLines)-commonSuffix; i++ {
		del(oldLn, oldLines[i])
		oldLn++
	}
	// Added lines.
	for i := commonPrefix; i < len(newLines)-commonSuffix; i++ {
		add(newLn, newLines[i])
		newLn++
	}
	// Context after the change.
	for i := len(oldLines) - commonSuffix; i < len(oldLines)-commonSuffix+ctxAfter && i < len(oldLines); i++ {
		ctx(newLn, oldLines[i])
		oldLn++
		newLn++
	}
	return out
}

// bannerLines renders the intro banner as a bordered box with a highlighted
// title chip, returned as individual visual lines for the committed history.
func bannerLines(a *agent) []string {
	titleChip := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("0")).
		Background(lipgloss.Color("6")).
		Padding(0, 1).
		Render("minimal-agent")

	label := lipgloss.NewStyle().Faint(true)
	row := func(k, v string) string {
		return label.Render(fmt.Sprintf("%-8s ", k)) + v
	}

	info := lipgloss.JoinVertical(lipgloss.Left,
		titleChip,
		"",
		row("model", a.effectiveModel()),
		row("version", Version),
		row("session", a.sessionName),
	)

	// A little robot mascot for the brand.
	mascot := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("6")).
		Render(" ╭───╮\n │o o│\n │ ▿ │\n ╰┬─┬╯")

	body := lipgloss.JoinHorizontal(lipgloss.Center, mascot, "   ", info)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("8")).
		Padding(1, 2).
		Render(body)

	lines := strings.Split(box, "\n")
	return lines
}

// Markdown renderer, cached by wrap width. The TUI update loop is
// single-goroutine, so no locking is needed.
var (
	mdRenderer      *glamour.TermRenderer
	mdRendererWidth int
)

// minimalMarkdownStyle returns a style config with soft, light colors —
// no harsh reds or dark backgrounds.
func minimalMarkdownStyle() ansi.StyleConfig {
	m := uint(2)
	return ansi.StyleConfig{
		Document: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				BlockPrefix: "\n",
				BlockSuffix: "\n",
			},
			Margin: &m,
		},
		BlockQuote: ansi.StyleBlock{
			Indent:      uintPtr(1),
			IndentToken: stringPtr("│ "),
		},
		Heading: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				BlockSuffix: "\n",
				Color:       stringPtr("6"),
				Bold:        boolPtr(true),
			},
		},
		H1: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "# ",
			},
		},
		H2: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "## ",
			},
		},
		H3: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "### ",
			},
		},
		H4: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "#### ",
			},
		},
		H5: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "##### ",
			},
		},
		H6: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "###### ",
			},
		},
		Strikethrough: ansi.StylePrimitive{
			CrossedOut: boolPtr(true),
		},
		Emph: ansi.StylePrimitive{
			Italic: boolPtr(true),
		},
		Strong: ansi.StylePrimitive{
			Bold: boolPtr(true),
		},
		HorizontalRule: ansi.StylePrimitive{
			Color:  stringPtr("8"),
			Format: "\n────────\n",
		},
		Item: ansi.StylePrimitive{
			BlockPrefix: "• ",
		},
		Enumeration: ansi.StylePrimitive{
			BlockPrefix: ". ",
		},
		Task: ansi.StyleTask{
			Ticked:   "[✓] ",
			Unticked: "[ ] ",
		},
		// Inline code: colored foreground only. A background slab (color 8)
		// reads as mid-gray on light terminal themes and hurts readability.
		Code: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color: stringPtr("3"),
			},
		},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{
					Faint: boolPtr(true),
				},
				Margin: &m,
			},
		},
		Link: ansi.StylePrimitive{
			Color:     stringPtr("12"),
			Underline: boolPtr(true),
		},
		LinkText: ansi.StylePrimitive{
			Color: stringPtr("6"),
			Bold:  boolPtr(true),
		},
		Image: ansi.StylePrimitive{
			Color:     stringPtr("14"),
			Underline: boolPtr(true),
		},
		ImageText: ansi.StylePrimitive{
			Color:  stringPtr("8"),
			Format: "Image: {{.text}} →",
		},
		DefinitionDescription: ansi.StylePrimitive{
			BlockPrefix: "\n🠶 ",
		},
	}
}

// renderMarkdown renders agent text as terminal markdown, wrapped to width.
// On any failure it falls back to the raw text so output is never lost.
func renderMarkdown(text string, width int) string {
	if width <= 0 {
		width = 80
	}
	if mdRenderer == nil || mdRendererWidth != width {
		noMargin := uint(0)
		cfg := minimalMarkdownStyle()
		cfg.Document.Margin = &noMargin
		r, err := glamour.NewTermRenderer(
			glamour.WithStyles(cfg),
			glamour.WithWordWrap(width),
		)
		if err != nil {
			return text
		}
		mdRenderer = r
		mdRendererWidth = width
	}
	out, err := mdRenderer.Render(text)
	if err != nil {
		return text
	}
	return strings.Trim(out, "\n")
}

func renderThinkStar(visible bool) string {
	if visible {
		return thinkStyle.Render("✦")
	}
	return " "
}

func renderCollapsedThinking(reasoning string) string {
	summary := "thought about it"
	if words := len(strings.Fields(reasoning)); words > 0 {
		summary = fmt.Sprintf("thought about it (%d words · Ctrl-O to expand)", words)
	}
	// Render the star at full magenta (not faint) so it stays visible, then
	// the summary text dim/italic.
	return thinkStyle.Render("✦") + " " + reasonStyle.Render(summary)
}

func renderTool(name, detail string) string {
	return renderToolWithDot("●", name, detail)
}

// renderToolWithDot renders a tool-call line with the given status dot (the
// pending-tool display blinks ●/○). The detail is flattened to one line and
// truncated: the viewport does not soft-wrap, so an untruncated multi-line
// command would otherwise be clipped or break the layout.
func renderToolWithDot(dot, name, detail string) string {
	detail = truncateStr(strings.Join(strings.Fields(detail), " "), 100)
	return toolDotStyle.Render(dot) + " " + toolDotStyle.Render(name) + " " + dimStyle.Render(detail)
}

func renderToolResult(result string) string {
	lines := strings.Split(result, "\n")
	for i, line := range lines {
		// Elbow connector visually attaches the result to its tool line.
		conn := "    "
		if i == 0 {
			conn = "  └ "
		}
		lines[i] = dimStyle.Render(conn + line)
	}
	return strings.Join(lines, "\n")
}

func renderError(msg string) string {
	return errStyle.Render("✗ " + msg)
}

// Renderer for highlighted shell blocks in approval prompts, cached by wrap
// width like mdRenderer. It is the same glamour dependency; the only config
// difference is a chroma theme on code blocks so ```bash fences get syntax
// highlighting instead of the faint style used for agent-output code.
var (
	shellRenderer      *glamour.TermRenderer
	shellRendererWidth int
)

// highlightShell renders a shell command as a ```bash markdown code block.
// The "gruvbox" theme forces a color on every token (plain text included),
// which is required because the block sits on the forced dark slab of
// renderShellBlock — theme-following colors could vanish there. Its warm
// palette (orange operators/builtins, yellow strings) is also the closest
// chroma match to the render-markdown.nvim look. Falls back to the raw text
// on any error.
func highlightShell(cmd string, width int) string {
	if width <= 0 {
		width = 80
	}
	if shellRenderer == nil || shellRendererWidth != width {
		noMargin := uint(0)
		cfg := minimalMarkdownStyle()
		cfg.Document.Margin = &noMargin
		cfg.CodeBlock.Margin = &noMargin
		cfg.CodeBlock.StylePrimitive.Faint = boolPtr(false)
		cfg.CodeBlock.Theme = "gruvbox"
		r, err := glamour.NewTermRenderer(
			glamour.WithStyles(cfg),
			glamour.WithWordWrap(width),
		)
		if err != nil {
			return cmd
		}
		shellRenderer = r
		shellRendererWidth = width
	}
	out, err := shellRenderer.Render("```bash\n" + cmd + "\n```")
	if err != nil {
		return cmd
	}
	// Strip glamour's right-padding and the blank padded lines it emits
	// around the block.
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " ")
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// The approval shell block is a forced dark slab (GitHub-style code block),
// so everything drawn on it uses fixed 256-palette colors instead of the
// theme-following ANSI colors used elsewhere: the slab looks the same on
// light and dark terminals.
const shellBlockBGSeq = "\x1b[48;5;236m"

// Header glyph and language tag stay muted gray, like an editor's code-block
// header — the command below is the colorful part.
var shellBlockHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

// shellBlockLine puts content on the slab, padded to blockW with one space
// of horizontal padding. Chroma/lipgloss reset the background at every SGR
// reset, so the background is re-armed after each one.
func shellBlockLine(content string, blockW int) string {
	pad := max(blockW-1-lipgloss.Width(content), 1)
	body := strings.ReplaceAll(content, "\x1b[0m", "\x1b[0m"+shellBlockBGSeq)
	return shellBlockBGSeq + " " + body + strings.Repeat(" ", pad) + "\x1b[0m"
}

// renderShellBlock renders a shell command as a GitHub-style code block: a
// header row with a terminal glyph and language tag, then the command
// syntax-highlighted on a dark background. The command is soft-wrapped to
// fit but never reformatted — what the user reads is exactly what will run.
func renderShellBlock(name, cmd string, width int) []string {
	if width < 20 {
		width = 80
	}
	inner := width - 2 // one space of slab padding each side
	wrapped := strings.Join(wordWrap(cmd, inner), "\n")
	body := strings.Split(highlightShell(wrapped, inner), "\n")

	// The slab spans the full wrap width, like an editor code block.
	header := shellBlockHeaderStyle.Render(">_ " + name)
	out := make([]string, 0, len(body)+1)
	out = append(out, shellBlockLine(header, width))
	for _, ln := range body {
		out = append(out, shellBlockLine(ln, width))
	}
	return out
}

// truncateStr shortens s to maxLen runes (not bytes), so multibyte text is
// never cut mid-character.
func truncateStr(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "..."
}

// toolCallBrief extracts a short description from a tool call's arguments.
// Used when rendering tool calls in history.
func toolCallBrief(tc openai.ChatCompletionMessageToolCallParam) string {
	var args struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Offset   *int   `json:"offset,omitempty"`
		Limit    *int   `json:"limit,omitempty"`
		Content  string `json:"content"`
		Query    string `json:"query"`
		URL      string `json:"url"`
		Question string `json:"question"`
	}
	json.Unmarshal([]byte(tc.Function.Arguments), &args)

	switch tc.Function.Name {
	case detectedShell.name:
		return "$ " + args.Command
	case "write":
		return fmt.Sprintf("%s (%d bytes)", relPath(args.FilePath), len(args.Content))
	case "edit":
		return relPath(args.FilePath)
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
		return detail
	case "web-search":
		return args.Query
	case "web-fetch":
		return args.URL
	case "ask_user_question":
		return fmt.Sprintf("? %s", truncateStr(args.Question, 60))
	default:
		return ""
	}
}
