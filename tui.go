package main

import (
	"context"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// --- Bubble Tea messages from agent goroutine ---

type reasoningMsg string
type contentMsg string
type toolCallDisplayMsg struct {
	name   string
	detail string
}
type toolResultDisplayMsg struct {
	result string
}
type diffDisplayMsg struct {
	lines []string
}
type approvalReqMsg struct {
	name    string
	detail  string
	respond chan<- bool
}
type turnDoneMsg struct{}
type turnErrMsg struct{ error }

type tickMsg time.Time

// --- model ---

type tuiModel struct {
	agent *agent

	viewport viewport.Model
	textarea textarea.Model

	// Committed output lines, each tagged with the role that produced it so a
	// blank spacer is inserted only when the role changes.
	committed []committedLine

	// Streaming state.
	streamingLine string
	streamingKind string // "reasoning" or "content" or ""

	// Thinking state.
	thinkingActive bool
	thinkingBuf    string
	starVisible    bool

	// Approval state.
	waitingApproval bool
	approvalCh      chan<- bool

	// Agent running state.
	agentRunning bool

	// Message channel for agent → TUI communication.
	msgCh chan tea.Msg

	// Context for cancellation.
	ctx    context.Context
	cancel context.CancelFunc

	// Window.
	width  int
	height int
	ready  bool

	bannerSeed []string
}

// Roles tag committed lines. A blank spacer is inserted only between lines of
// different roles, so a single multi-line block (banner, wrapped message, an
// agent turn's reasoning+tools+response) stays visually contiguous.
const (
	roleBanner       = "banner"
	roleUser         = "user"
	roleAgent        = "agent"       // agent text / thinking
	roleAgentTool    = "agentTool"   // tool call
	roleAgentResult  = "agentResult" // tool result
	roleCommand      = "command"
)

// spacerGap returns the number of blank lines between two roles.
func spacerGap(prev, cur string) int {
	if prev == cur {
		return 0
	}
	// User → any agent role: 1 blank.
	if prev == roleUser && isAgentRole(cur) {
		return 1
	}
	// Any agent role → user: 2 blanks.
	if isAgentRole(prev) && cur == roleUser {
		return 2
	}
	// Tool call → tool result: tight, 0 gap.
	if prev == roleAgentTool && cur == roleAgentResult {
		return 0
	}
	// All other role transitions: 1 blank.
	return 1
}

func isAgentRole(r string) bool {
	return r == roleAgent || r == roleAgentTool || r == roleAgentResult
}

type committedLine struct {
	role string
	text string
}

// push appends one or more lines under a single role.
func (m *tuiModel) push(role string, texts ...string) {
	for _, t := range texts {
		m.committed = append(m.committed, committedLine{role: role, text: t})
	}
}

func bannerToCommitted(lines []string) []committedLine {
	out := make([]committedLine, len(lines))
	for i, l := range lines {
		out[i] = committedLine{role: roleBanner, text: l}
	}
	return out
}

func newTUIModel(a *agent) tuiModel {
	ta := textarea.New()
	ta.Placeholder = "type a message or /command..."
	ta.SetPromptFunc(2, func(lineIdx int) string {
		return "│ "
	})
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	ta.BlurredStyle.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 10
	ta.Focus()

	// Override keymap: Enter submits, Shift+Enter for newline.
	ta.KeyMap.InsertNewline.SetEnabled(false)

	vp := viewport.New(80, 20)
	vp.Style = lipgloss.NewStyle().PaddingLeft(1)

	ctx, cancel := context.WithCancel(context.Background())

	ch := make(chan tea.Msg, 256)
	a.msgCh = ch

	bannerSeed := bannerLines(a)

	return tuiModel{
		agent:      a,
		viewport:   vp,
		textarea:   ta,
		committed:  bannerToCommitted(bannerSeed),
		bannerSeed: bannerSeed,
		msgCh:      ch,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// --- Bubble Tea interface ---

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.ready = true
		}
		m.viewport.Width = max(1, msg.Width-4)
		m.textarea.SetWidth(max(1, msg.Width-4))
		m.rebuildOutput()
		return m, nil

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		if m.waitingApproval {
			switch msg.Type {
			case tea.KeyRunes:
				switch string(msg.Runes) {
				case "y", "Y", "yes":
					m.waitingApproval = false
					if m.approvalCh != nil {
						m.approvalCh <- true
					}
					m.approvalCh = nil
					return m, waitForMsg(m.msgCh)
				case "n", "N", "no":
					m.waitingApproval = false
					m.agentRunning = false
					if m.approvalCh != nil {
						m.approvalCh <- false
					}
					m.approvalCh = nil
					return m, nil
				}
			case tea.KeyEnter, tea.KeyEscape:
				m.waitingApproval = false
				m.agentRunning = false
				if m.approvalCh != nil {
					m.approvalCh <- false
				}
				m.approvalCh = nil
				return m, nil
			}
			return m, nil
		}

		// Arrow keys: scroll viewport (always, even during streaming).
		if msg.Type == tea.KeyUp || msg.Type == tea.KeyDown {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

		switch msg.Type {
		case tea.KeyCtrlC:
			m.cancel()
			m.agent.autoSave()
			return m, tea.Quit

		case tea.KeyEnter:
			if m.agentRunning {
				return m, nil
			}
			line := strings.TrimSpace(m.textarea.Value())
			m.textarea.Reset()
			if line == "" {
				return m, nil
			}

			if strings.HasPrefix(line, "/") {
				m.renderCommand(line)
				return m, nil
			}

			// User message.
			m.commitUser(line)
			m.updateViewportContent()

			m.agentRunning = true
			m.thinkingActive = false
			m.thinkingBuf = ""
			m.streamingLine = ""
			m.streamingKind = ""
			m.starVisible = true

			m.agent.history = append(m.agent.history, userMessage(line))
			m.agent.sessionDirty = true

			go m.agent.doTurn(m.ctx)
			return m, waitForMsg(m.msgCh)

		default:
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(msg)
			cmds = append(cmds, cmd)
		}

	// --- Agent events ---

	case reasoningMsg:
		// If content was streaming, commit it and reset.
		if m.streamingKind == "content" {
			m.commitAgent(m.streamingLine)
			m.streamingLine = ""
			m.streamingKind = ""
		}
		// Start a fresh thinking block when entering reasoning from a
		// non-reasoning state (new turn, or after tool-call flush).
		if !m.thinkingActive {
			m.thinkingBuf = ""
			m.thinkingActive = true
		}
		m.thinkingBuf += string(msg)
		m.streamingLine = m.thinkingBuf
		m.streamingKind = "reasoning"
		m.updateViewportContent()
		return m, waitForMsg(m.msgCh)

	case contentMsg:
		if m.thinkingActive {
			m.thinkingActive = false
			m.push(roleAgent, renderCollapsedThinking(m.thinkingBuf))
			m.thinkingBuf = ""
			m.streamingLine = ""
			m.streamingKind = ""
		}
		m.streamingLine += string(msg)
		m.streamingKind = "content"
		m.updateViewportContent()
		return m, waitForMsg(m.msgCh)

	case toolCallDisplayMsg:
		m.flushStreaming()
		m.push(roleAgentTool, renderTool(msg.name, msg.detail))
		m.updateViewportContent()
		return m, waitForMsg(m.msgCh)

	case diffDisplayMsg:
		for _, ln := range msg.lines {
			m.push(roleAgent, ln)
		}
		m.updateViewportContent()
		return m, waitForMsg(m.msgCh)

	case toolResultDisplayMsg:
		// Keep short results inline; skip verbose content.
		short := msg.result
		if len(short) > 120 {
			short = short[:120] + "..."
		}
		m.push(roleAgentResult, renderToolResult(short))
		m.updateViewportContent()
		return m, waitForMsg(m.msgCh)

	case approvalReqMsg:
		m.flushStreaming()
		m.waitingApproval = true
		m.approvalCh = msg.respond
		m.push(roleAgent, renderApproval(msg.name, msg.detail))
		m.updateViewportContent()
		return m, nil

	case turnDoneMsg:
		m.flushStreaming()
		m.agentRunning = false
		m.agent.sessionDirty = true
		m.agent.autoSave()
		m.updateViewportContent()
		return m, nil

	case turnErrMsg:
		m.flushStreaming()
		m.agentRunning = false
		m.push(roleAgent, renderError(msg.Error()))
		m.updateViewportContent()
		return m, nil

	case tickMsg:
		if m.thinkingActive || m.agentRunning {
			m.starVisible = !m.starVisible
		}
		cmds = append(cmds, tickCmd())
	}

	return m, tea.Batch(cmds...)
}

func waitForMsg(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return turnDoneMsg{}
		}
		return msg
	}
}

func (m *tuiModel) flushStreaming() {
	if m.streamingLine == "" {
		return
	}
	switch m.streamingKind {
	case "content":
		m.commitAgent(m.streamingLine)
	case "reasoning":
		m.push(roleAgent, renderCollapsedThinking(m.thinkingBuf))
		m.thinkingBuf = ""
		m.thinkingActive = false
	}
	m.streamingLine = ""
	m.streamingKind = ""
}

func (m *tuiModel) updateViewportContent() {
	var lines []string
	for i, cl := range m.committed {
		if i > 0 {
			gap := spacerGap(m.committed[i-1].role, cl.role)
			for j := 0; j < gap; j++ {
				lines = append(lines, "")
			}
		}
		lines = append(lines, cl.text)
	}

	if m.streamingLine != "" {
		// Streaming reasoning/content is part of the agent turn. Determine the
		// effective preceding role for spacing.
		prevRole := roleAgent // default: no gap
		if n := len(m.committed); n > 0 {
			prevRole = m.committed[n-1].role
		}
		var streamRole string
		switch m.streamingKind {
		case "reasoning":
			streamRole = roleAgent
		case "content":
			streamRole = roleAgent
		default:
			streamRole = roleAgent
		}
		if prevRole != streamRole {
			gap := spacerGap(prevRole, streamRole)
			for j := 0; j < gap; j++ {
				lines = append(lines, "")
			}
		}
		switch m.streamingKind {
		case "reasoning":
			prefix := renderThinkStar(m.starVisible) + " "
			indent := strings.Repeat(" ", lipgloss.Width(prefix))
			cw := m.contentWidth()
			wrapWidth := cw - lipgloss.Width(prefix)
			if wrapWidth < 20 {
				wrapWidth = 80
			}
			for i, ln := range wordWrap(m.streamingLine, wrapWidth) {
				if i == 0 {
					lines = append(lines, prefix+reasonStyle.Render(ln))
				} else {
					lines = append(lines, indent+reasonStyle.Render(ln))
				}
			}
		case "content":
			prefix := agentStyle.Render("agent>") + " "
			indent := strings.Repeat(" ", lipgloss.Width(prefix))
			cw := m.contentWidth()
			wrapWidth := cw - lipgloss.Width(prefix)
			if wrapWidth < 20 {
				wrapWidth = 80
			}
			for i, ln := range wordWrap(m.streamingLine, wrapWidth) {
				if i == 0 {
					lines = append(lines, prefix+ln)
				} else {
					lines = append(lines, indent+ln)
				}
			}
		default:
			lines = append(lines, m.streamingLine)
		}
	}

	content := strings.Join(lines, "\n")
	m.viewport.SetContent(content)
	m.viewport.GotoBottom()
}

func (m tuiModel) View() string {
	if !m.ready {
		return "initializing...\n"
	}

	// Size the input to its content (1 line when empty, growing as it wraps,
	// capped) instead of a fixed height, then give the rest to the viewport.
	// LineInfo().Height is the soft-wrapped visual row count; LineCount() is
	// logical lines (always 1 here since newlines are disabled), so it would
	// leave the textarea 1 row tall and scroll wrapped text out of view.
	taHeight := min(max(1, m.textarea.LineInfo().Height), 10)
	taHeight = min(taHeight, max(1, m.height-2))
	m.textarea.SetHeight(taHeight)
	m.viewport.Height = max(1, m.height-1-taHeight) // separator + textarea

	var b strings.Builder

	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	// Separator.
	b.WriteString(dimStyle.Render(strings.Repeat("─", m.width)))
	b.WriteString("\n")

	// Input area.
	if m.waitingApproval {
		b.WriteString(dimStyle.Render("  press y/N ..."))
	} else if m.agentRunning {
		b.WriteString(dimStyle.Render("  ..."))
	} else {
		b.WriteString(m.textarea.View())
	}

	return appStyle.Render(b.String())
}

func (m *tuiModel) rebuildOutput() {
	m.committed = bannerToCommitted(m.bannerSeed)
	toolCallNames := map[string]string{} // tool call ID → name
	for _, msg := range m.agent.history {
		if msg.OfUser != nil {
			m.commitUser(msg.OfUser.Content.OfString.Value)
		}
		if msg.OfAssistant != nil {
			// Show tool calls with brief detail.
			if len(msg.OfAssistant.ToolCalls) > 0 {
				for _, tc := range msg.OfAssistant.ToolCalls {
					toolCallNames[tc.ID] = tc.Function.Name
					m.push(roleAgentTool, renderTool(tc.Function.Name, toolCallBrief(tc)))
				}
				continue
			}
			if text := msg.OfAssistant.Content.OfString.Value; text != "" {
				m.commitAgent(text)
			}
		}
		if msg.OfTool != nil {
			// Skip "read" tool results — too verbose.
			tcID := ""
			if tc := msg.GetToolCallID(); tc != nil {
				tcID = *tc
			}
			if toolCallNames[tcID] == "read" {
				continue
			}
			// Show brief tool result preview, same as streaming.
			content := msg.OfTool.Content.OfString.Value
			short := content
			if len(short) > 120 {
				short = short[:120] + "..."
			}
			m.push(roleAgentResult, renderToolResult(short))
		}
	}
	m.updateViewportContent()
}

// --- command rendering ---

func (m *tuiModel) renderCommand(line string) {
	// Echo the typed command as a user prompt, same as a normal message.
	m.commitUser(line)

	cmd := strings.TrimPrefix(line, "/")
	result := m.agent.handleCommandStr(cmd)

	parts := strings.Fields(cmd)
	cmdName := ""
	if len(parts) > 0 {
		cmdName = parts[0]
	}

	for _, ln := range strings.Split(result, "\n") {
		if ln == "" {
			continue
		}
		if cmdName == "config" && strings.Contains(ln, ":") {
			// Split "key : value" and color the key.
			idx := strings.Index(ln, ":")
			key := ln[:idx]
			rest := ln[idx+1:]
			m.push(roleCommand,
				dimStyle.Render("  ")+
					inputPromptStyle.Render(key)+
					dimStyle.Render(":"+rest))
		} else {
			m.push(roleCommand, dimStyle.Render("  "+ln))
		}
	}
	m.updateViewportContent()
}

// --- message committing ---

func (m *tuiModel) contentWidth() int {
	w := m.viewport.Width - 1
	if w < 40 {
		w = 80
	}
	return w
}

func (m *tuiModel) commitUser(text string) {
	prefix := "you> "
	indent := strings.Repeat(" ", lipgloss.Width(prefix))
	cw := m.contentWidth()
	wrapWidth := cw - lipgloss.Width(prefix)
	if wrapWidth < 20 {
		wrapWidth = 80
	}
	for i, line := range wordWrap(text, wrapWidth) {
		if i == 0 {
			m.push(roleUser, youStyle.Render(prefix)+line)
		} else {
			m.push(roleUser, indent+line)
		}
	}
}

func (m *tuiModel) commitAgent(text string) {
	prefix := "agent> "
	indent := strings.Repeat(" ", lipgloss.Width(prefix))
	cw := m.contentWidth()
	wrapWidth := cw - lipgloss.Width(prefix)
	if wrapWidth < 20 {
		wrapWidth = 80
	}
	rendered := renderMarkdown(text, wrapWidth)
	for i, line := range strings.Split(rendered, "\n") {
		if i == 0 {
			m.push(roleAgent, agentStyle.Render(prefix)+line)
		} else {
			m.push(roleAgent, indent+line)
		}
	}
}

// --- word wrap ---

func wordWrap(text string, width int) []string {
	if width <= 0 {
		width = 80
	}
	var lines []string
	for _, para := range strings.Split(text, "\n") {
		if para == "" {
			lines = append(lines, "")
			continue
		}
		wrapped := lipgloss.NewStyle().Width(width).Render(para)
		for _, ln := range strings.Split(wrapped, "\n") {
			lines = append(lines, strings.TrimRight(ln, " "))
		}
	}
	return lines
}
