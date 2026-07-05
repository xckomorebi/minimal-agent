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

	// Agent prefix: bold green.
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

	// Collapsed reasoning line.
	collapsedThinkStyle = lipgloss.NewStyle().
				Faint(true).
				Italic(true).
				Foreground(lipgloss.Color("5"))

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

	// Banner box.
	bannerBorderStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8"))

	// Status bar.
	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			Background(lipgloss.Color("0"))

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

func renderYou(line string) string {
	return youStyle.Render("you>") + " " + line
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
		Code: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color:           stringPtr("3"),
				BackgroundColor: stringPtr("8"),
				Prefix:          " ",
				Suffix:          " ",
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

func renderAgent(line string) string {
	return agentStyle.Render("agent>") + " " + line
}

func renderThinkStar(visible bool) string {
	if visible {
		return thinkStyle.Render("✦")
	}
	return " "
}

func renderReasoning(line string) string {
	return thinkStyle.Render("✦") + " " + reasonStyle.Render(line)
}

func renderCollapsedThinking(reasoning string) string {
	summary := "thought about it"
	// Render the star at full magenta (not faint) so it stays visible, then
	// the summary text dim/italic.
	return thinkStyle.Render("✦") + " " + reasonStyle.Render(summary)
}

func renderTool(name, detail string) string {
	return toolDotStyle.Render("●") + " " + toolDotStyle.Render(name) + " " + dimStyle.Render(detail)
}

func renderToolResult(result string) string {
	return dimStyle.Render("  " + result)
}

func renderError(msg string) string {
	return errStyle.Render("✗ " + msg)
}

func renderOK(msg string) string {
	return okStyle.Render("✓ " + msg)
}

func renderApproval(name, detail string) string {
	return approvalStyle.Render("run "+name+"?") + " " + dimStyle.Render(detail)
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// toolCallBrief extracts a short description from a tool call's arguments.
// Used when rendering tool calls in history.
func toolCallBrief(tc openai.ChatCompletionMessageToolCallParam) string {
	var args struct {
		Command  string `json:"command"`
		Path     string `json:"path"`
		Content  string `json:"content"`
		Query    string `json:"query"`
		URL      string `json:"url"`
		Question string `json:"question"`
	}
	json.Unmarshal([]byte(tc.Function.Arguments), &args)

	switch tc.Function.Name {
	case "bash":
		return "$ " + args.Command
	case "write":
		return fmt.Sprintf("%s (%d bytes)", relPath(args.Path), len(args.Content))
	case "edit", "read":
		return relPath(args.Path)
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
