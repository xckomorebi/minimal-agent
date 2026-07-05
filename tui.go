package main

import (
	"context"
	"fmt"
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

// --- autocomplete ---

type autocompleteState struct {
	suggestions []string
	selected    int
}

// --- picker ---

type pickerItem struct {
	name    string
	current bool // is this the current session?
	summary string // brief one-line summary
}

type pickerState struct {
	items    []pickerItem
	selected int
}

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

	// Autocomplete state.
	autocomplete autocompleteState

	// Picker state.
	picker pickerState

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

// bottomPad is the number of blank lines kept below the newest message so it
// isn't glued to the input box and there's a little room to scroll past it.
const bottomPad = 4

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
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Faint(true).Italic(true)
	ta.BlurredStyle.Placeholder = lipgloss.NewStyle().Faint(true).Italic(true)
	ta.SetPromptFunc(2, func(lineIdx int) string {
		if lineIdx == 0 {
			return "▎ "
		}
		return "  "
	})
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	ta.BlurredStyle.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	ta.FocusedStyle.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
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

// --- Autocomplete & picker helpers ---

// computeAutocomplete populates the autocomplete state based on the current
// textarea value and cursor position. If there's exactly one match, it applies
// it immediately; otherwise it opens the suggestion popup.
func (m *tuiModel) computeAutocomplete() {
	input := m.textarea.Value()
	pos := len(input) // cursor is typically at end when Tab triggers autocomplete
	suggestions := autocompleteCommand(input, pos)
	if len(suggestions) == 0 {
		m.autocomplete = autocompleteState{}
		return
	}
	if len(suggestions) == 1 {
		m.applyCompletion(suggestions[0])
		m.autocomplete = autocompleteState{}
		return
	}
	m.autocomplete = autocompleteState{
		suggestions: suggestions,
		selected:    0,
	}
}

// applyAutocomplete applies the currently-selected autocomplete suggestion to
// the textarea and dismisses the popup.
func (m *tuiModel) applyAutocomplete() {
	if len(m.autocomplete.suggestions) == 0 {
		return
	}
	choice := m.autocomplete.suggestions[m.autocomplete.selected]
	m.applyCompletion(choice)
	m.autocomplete = autocompleteState{}
}

// applyCompletion replaces the last word before the cursor with the given
// completion string.
func (m *tuiModel) applyCompletion(choice string) {
	input := m.textarea.Value()
	pos := len(input) // cursor position

	// Find the end of the word before the cursor. We treat the input as
	// slash-command-structured: words are separated by spaces.
	upToCursor := input[:pos]
	afterCursor := input[pos:]

	// Find the start of the last word.
	lastSpace := strings.LastIndex(upToCursor, " ")
	wordStart := 0
	if lastSpace >= 0 {
		wordStart = lastSpace + 1
	}
	// Preserve the leading "/" when completing the first word of a slash command.
	if wordStart == 0 && strings.HasPrefix(input, "/") {
		wordStart = 1
	}

	// Build the new value: everything before the word + completion + after cursor.
	newValue := upToCursor[:wordStart] + choice + afterCursor
	m.textarea.SetValue(newValue)

	// Move cursor to end of the inserted completion.
	newPos := wordStart + len(choice)
	m.textarea.CursorEnd()
	// textarea.CursorEnd moves to the end; we can use SetCursor if needed.
	_ = newPos
}

// openSessionPicker opens a picker to select a saved session. Used by /resume
// when no session name is provided.
func (m *tuiModel) openSessionPicker() {
	names, err := listSessions()
	if err != nil || len(names) == 0 {
		// No sessions to pick; just show the normal usage message.
		m.commitUser("/resume")
		m.push(roleCommand, dimStyle.Render("  (no saved sessions)"))
		m.updateViewportContent()
		return
	}
	items := make([]pickerItem, len(names))
	for i, n := range names {
		items[i] = pickerItem{
			name:    n,
			current: n == m.agent.sessionName,
			summary: sessionSummary(n),
		}
	}
	m.picker = pickerState{
		items:    items,
		selected: 0,
	}
	// Clear the textarea so Enter doesn't leave a stray slash.
	m.textarea.Reset()
	m.updateViewportContent()
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
		// Size the viewport height now so GotoBottom in rebuildOutput uses
		// the real height, not the placeholder 20.
		taHeight := min(max(1, m.textarea.LineInfo().Height), 10)
		taHeight = min(taHeight, max(1, msg.Height-2))
		m.viewport.Height = max(1, msg.Height-2-taHeight)
		m.rebuildOutput()
		return m, nil

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		// --- Picker mode: arrow keys, enter, esc ---
		if len(m.picker.items) > 0 {
			switch msg.Type {
			case tea.KeyUp:
				if m.picker.selected > 0 {
					m.picker.selected--
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyDown:
				if m.picker.selected < len(m.picker.items)-1 {
					m.picker.selected++
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyEnter:
				// Execute the picked item.
				selected := m.picker.items[m.picker.selected].name
				m.picker = pickerState{}
				cmd := "/resume " + selected
				m.renderCommand(cmd)
				return m, nil
			case tea.KeyEscape, tea.KeyCtrlC:
				m.picker = pickerState{}
				m.updateViewportContent()
				return m, nil
			}
			return m, nil
		}

		// --- Autocomplete active: tab/arrows to navigate, enter to accept, esc to cancel ---
		if len(m.autocomplete.suggestions) > 0 {
			switch msg.Type {
			case tea.KeyTab:
				m.autocomplete.selected = (m.autocomplete.selected + 1) % len(m.autocomplete.suggestions)
				m.updateViewportContent()
				return m, nil
			case tea.KeyShiftTab, tea.KeyUp:
				if m.autocomplete.selected > 0 {
					m.autocomplete.selected--
				} else {
					m.autocomplete.selected = len(m.autocomplete.suggestions) - 1
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyDown:
				m.autocomplete.selected = (m.autocomplete.selected + 1) % len(m.autocomplete.suggestions)
				m.updateViewportContent()
				return m, nil
			case tea.KeyEnter:
				m.applyAutocomplete()
				m.updateViewportContent()
				return m, nil
			case tea.KeyEscape:
				m.autocomplete = autocompleteState{}
				m.updateViewportContent()
				return m, nil
			default:
				// Any other key dismisses autocomplete and is processed normally.
				m.autocomplete = autocompleteState{}
				m.updateViewportContent()
				// Fall through to normal key handling.
			}
		}

		// --- Approval mode: y/n/Ctrl-C ---
		if m.waitingApproval {
			switch msg.Type {
			case tea.KeyCtrlC:
				// Cancel the agent turn outright.
				m.waitingApproval = false
				m.cancel()
				m.ctx, m.cancel = context.WithCancel(context.Background())
				if m.approvalCh != nil {
					select {
					case m.approvalCh <- false:
					default:
					}
				}
				m.approvalCh = nil
				m.updateViewportContent()
				return m, waitForMsg(m.msgCh)
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
					if m.approvalCh != nil {
						m.approvalCh <- false
					}
					m.approvalCh = nil
					return m, waitForMsg(m.msgCh)
				}
			case tea.KeyEnter, tea.KeyEscape:
				m.waitingApproval = false
				if m.approvalCh != nil {
					m.approvalCh <- false
				}
				m.approvalCh = nil
				return m, waitForMsg(m.msgCh)
			}
			return m, nil
		}

		// Arrow keys, page up/down, home/end: scroll viewport (always, even during streaming).
		// But skip if autocomplete is showing (handled above).
		if msg.Type == tea.KeyUp || msg.Type == tea.KeyDown ||
			msg.Type == tea.KeyPgUp || msg.Type == tea.KeyPgDown ||
			msg.Type == tea.KeyHome || msg.Type == tea.KeyEnd {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

		switch msg.Type {
		case tea.KeyCtrlC:
			if m.agentRunning {
				// Cancel the current turn but keep the session alive.
				m.cancel()
				m.ctx, m.cancel = context.WithCancel(context.Background())
				m.flushStreaming()
				// turnErrMsg handler will push "[canceled]" and set agentRunning=false.
				m.updateViewportContent()
				return m, waitForMsg(m.msgCh)
			}
			// Not running: exit the program.
			m.cancel()
			m.agent.autoSave()
			return m, tea.Quit

		case tea.KeyCtrlO:
			v := !m.agent.thinkingDetail()
			m.agent.config.ThinkingDetail = &v
			m.agent.sessionDirty = true
			m.rebuildOutput()
			return m, nil

		case tea.KeyTab:
			if m.agentRunning {
				return m, nil
			}
			m.computeAutocomplete()
			m.updateViewportContent()
			return m, nil

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
				// Check for commands that should trigger the picker.
				parts := strings.Fields(strings.TrimPrefix(line, "/"))
				if len(parts) == 0 {
					// Just "/" — show unknown command.
					m.renderCommand(line)
					return m, nil
				}
				if parts[0] == "resume" && len(parts) == 1 {
					m.openSessionPicker()
					return m, nil
				}
				m.renderCommand(line)
				return m, nil
			}

			// User message.
			m.commitUser(line)
			m.updateViewportContent()
			// Submitting always jumps to the bottom so the new turn is followed,
			// even if the user was scrolled up reading history.
			m.viewport.GotoBottom()

			m.agentRunning = true
			m.thinkingActive = false
			m.thinkingBuf = ""
			m.streamingLine = ""
			m.streamingKind = ""
			m.starVisible = true

			m.agent.history = append(m.agent.history, userMessage(line))
			m.agent.sessionDirty = true

			// Generate a session summary asynchronously on the first user message.
			if !m.agent.summaryGenerated {
				m.agent.summaryGenerated = true
				go m.agent.generateSessionSummary(line)
			}

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
			if m.agent.thinkingDetail() {
				m.commitThinkingDetail(m.thinkingBuf)
			} else {
				m.push(roleAgent, renderCollapsedThinking(m.thinkingBuf))
			}
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
		// A canceled context means the user pressed Ctrl-C to abort the turn.
		if msg.error == context.Canceled {
			m.push(roleAgent, dimStyle.Render("  [canceled]"))
		} else {
			m.push(roleAgent, renderError(msg.Error()))
		}
		// Save now so the user message (and any partial assistant+tools
		// already appended before cancel) is persisted.
		m.agent.sessionDirty = true
		m.agent.autoSave()
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
		if m.agent.thinkingDetail() {
			m.commitThinkingDetail(m.thinkingBuf)
		} else {
			m.push(roleAgent, renderCollapsedThinking(m.thinkingBuf))
		}
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
			wrapped := wordWrap(m.streamingLine, wrapWidth)
			// Rolling window: only show last 10 lines in default mode.
			if !m.agent.thinkingDetail() && len(wrapped) > 10 {
				wrapped = wrapped[len(wrapped)-10:]
			}
			for i, ln := range wrapped {
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

	// Sticky bottom: was the user parked at the bottom *before* we change the
	// content? Capture it first, because SetContent (adding a streamed line)
	// makes the old offset no longer the bottom. Only re-pin to the bottom if
	// they were already there; if they've scrolled up to read history we never
	// touch the viewport, so their position is left exactly alone. When they
	// scroll back down to the bottom, AtBottom() becomes true again and follow
	// resumes on the next frame.
	wasAtBottom := m.viewport.AtBottom()

	content := strings.Join(lines, "\n")
	// A few blank lines below the newest content so it isn't glued to the input
	// box and there's a little room to scroll past it.
	content += strings.Repeat("\n", bottomPad)
	m.viewport.SetContent(content)

	if wasAtBottom {
		m.viewport.GotoBottom()
	}
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
	m.viewport.Height = max(1, m.height-2-taHeight) // hint + separator + textarea

	var b strings.Builder

	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	// Key hints — dynamic based on state.
	ctrlCLabel := "quit"
	if m.agentRunning {
		ctrlCLabel = "cancel"
	}
	thinkingLabel := "show thinking"
	if m.agent.thinkingDetail() {
		thinkingLabel = "hide thinking"
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf("Ctrl-C %s · Ctrl-O %s · %s · ↑↓ scroll", ctrlCLabel, thinkingLabel, m.agent.effectiveModel())))
	b.WriteString("\n")

	// Separator.
	b.WriteString(dimStyle.Render(strings.Repeat("─", m.width)))
	b.WriteString("\n")

	// --- Render picker or autocomplete ---
	if len(m.picker.items) > 0 {
		b.WriteString(m.renderPicker())
	} else if len(m.autocomplete.suggestions) > 0 {
		b.WriteString(m.renderAutocomplete())
	}

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

// renderPicker renders the session picker popup as a bordered box.
func (m *tuiModel) renderPicker() string {
	// Determine box width: longest name + summary + padding.
	maxLine := 0
	for _, it := range m.picker.items {
		lineLen := len(it.name)
		if it.summary != "" {
			lineLen += 3 + len(it.summary) // " ‒ summary"
		}
		if lineLen > maxLine {
			maxLine = lineLen
		}
	}
	boxWidth := min(maxLine+8, m.width-4)
	if boxWidth < 24 {
		boxWidth = 24
	}
	innerWidth := boxWidth - 4 // border + padding

	var lines []string
	// Title.
	title := dimStyle.Render("select session")
	lines = append(lines, title)

	// Items.
	for i, it := range m.picker.items {
		marker := "  "
		if i == m.picker.selected {
			marker = "> "
		}
		label := it.name
		if it.current {
			label = lipgloss.NewStyle().Underline(true).Render(label)
		}
		if it.summary != "" {
			label += dimStyle.Render(" ‒ " + it.summary)
		}
		line := marker + label
		// Pad to innerWidth.
		if lipgloss.Width(line) < innerWidth {
			line += strings.Repeat(" ", innerWidth-lipgloss.Width(line))
		}
		if i == m.picker.selected {
			line = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(line)
		} else {
			line = dimStyle.Render(line)
		}
		lines = append(lines, line)
	}

	body := strings.Join(lines, "\n")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("8")).
		Padding(0, 1).
		Width(boxWidth).
		Render(body)
	return box + "\n"
}

// renderAutocomplete renders the autocomplete suggestion bar.
func (m *tuiModel) renderAutocomplete() string {
	var parts []string
	for i, s := range m.autocomplete.suggestions {
		styled := s
		if i == m.autocomplete.selected {
			styled = lipgloss.NewStyle().
				Foreground(lipgloss.Color("0")).
				Background(lipgloss.Color("6")).
				Padding(0, 1).
				Render(s)
		} else {
			styled = dimStyle.Render(s)
		}
		parts = append(parts, styled)
	}
	return "  " + strings.Join(parts, dimStyle.Render(" │ ")) + "\n"
}

func (m *tuiModel) rebuildOutput() {
	m.committed = bannerToCommitted(m.bannerSeed)
	toolCallNames := map[string]string{} // tool call ID → name
	for i, msg := range m.agent.history {
		if msg.OfUser != nil {
			m.commitUser(msg.OfUser.Content.OfString.Value)
		}
		if msg.OfAssistant != nil {
			// If the agent was thinking before this message, render it.
			if reasoning, ok := m.agent.reasonings[i]; ok {
				if m.agent.thinkingDetail() {
					m.commitThinkingDetail(reasoning)
				} else {
					m.push(roleAgent, renderCollapsedThinking(reasoning))
				}
			}
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
	cmd := strings.TrimPrefix(line, "/")
	result := m.agent.handleCommandStr(cmd)

	parts := strings.Fields(cmd)
	cmdName := ""
	if len(parts) > 0 {
		cmdName = parts[0]
	}

	// Commands that replace the agent's history must also rebuild the TUI
	// committed lines from scratch, so the loaded/new-session messages appear
	// and old messages are cleared.
	switch cmdName {
	case "resume", "new-session":
		m.bannerSeed = bannerLines(m.agent)
		m.rebuildOutput()
	case "config":
		if strings.HasPrefix(cmd, "config thinking-detail") {
			m.rebuildOutput()
		}
	}

	// Echo the typed command as a user prompt, same as a normal message.
	m.commitUser(line)

	for _, ln := range strings.Split(result, "\n") {
		if ln == "" {
			m.push(roleCommand, "")
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
			m.commitCommand(ln)
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
	isCmd := strings.HasPrefix(text, "/")
	prefix := "you> "
	indent := strings.Repeat(" ", lipgloss.Width(prefix))
	cw := m.contentWidth()
	wrapWidth := cw - lipgloss.Width(prefix)
	if wrapWidth < 20 {
		wrapWidth = 80
	}
	for i, line := range wordWrap(text, wrapWidth) {
		styledLine := line
		if isCmd {
			styledLine = cmdTextStyle.Render(line)
		}
		if i == 0 {
			m.push(roleUser, youStyle.Render(prefix)+styledLine)
		} else {
			m.push(roleUser, indent+styledLine)
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

func (m *tuiModel) commitThinkingDetail(text string) {
	prefix := thinkStyle.Render("✦") + " "
	indent := strings.Repeat(" ", lipgloss.Width(prefix))
	cw := m.contentWidth()
	wrapWidth := cw - lipgloss.Width(prefix)
	if wrapWidth < 20 {
		wrapWidth = 80
	}
	for i, line := range wordWrap(text, wrapWidth) {
		if i == 0 {
			m.push(roleAgent, prefix+reasonStyle.Render(line))
		} else {
			m.push(roleAgent, indent+reasonStyle.Render(line))
		}
	}
}

func (m *tuiModel) commitCommand(text string) {
	prefix := dimStyle.Render("  ")
	indent := dimStyle.Render("  ")
	cw := m.contentWidth()
	wrapWidth := cw - lipgloss.Width("  ")
	if wrapWidth < 20 {
		wrapWidth = 80
	}
	for i, line := range wordWrap(text, wrapWidth) {
		if i == 0 {
			m.push(roleCommand, prefix+line)
		} else {
			m.push(roleCommand, indent+line)
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
