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
type approvalAnswer struct {
	approved bool
	reason   string // non-empty when denied with a custom reason
}

type approvalReqMsg struct {
	name    string
	detail  string
	respond chan<- approvalAnswer
}
type turnDoneMsg struct{}
type turnErrMsg struct{ error }
type compactDoneMsg struct {
	result string // compact result to display
}

// questionMsg is sent by the agent when ask_user_question is invoked.
// The TUI displays the question and options, waits for user input,
// and sends the answer back through the respond channel.
type questionMsg struct {
	question   string
	options    []string
	allowOther bool
	respond    chan<- string
}

type tickMsg struct {
	time time.Time
	fast bool // true for spinner ticks (100ms), false for blink ticks (500ms)
}

// --- autocomplete ---

type autocompleteState struct {
	suggestions []string
	selected    int
}

// --- picker ---

type pickerItem struct {
	name    string
	current bool   // is this the current session?
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

	// Pending tool call (executing, shown with blinking dot).
	pendingToolName   string
	pendingToolDetail string

	// Thinking state.
	thinkingActive bool
	thinkingBuf    string
	starVisible    bool

	// Approval state.
	waitingApproval   bool
	approvalCh        chan<- approvalAnswer
	approvalSelected  int    // 0=yes, 1=no, 2=other
	approvalInput     string // custom denial reason
	approvalCursorPos int

	// Agent running state.
	agentRunning bool
	spinnerIdx   int
	dotIdx       int

	// quitConfirm is set after a first Ctrl-C on an idle, empty prompt; a
	// second Ctrl-C quits. Any other key clears it.
	quitConfirm bool

	// Approval block bookkeeping: where the block starts in committed (so it
	// can be folded to one outcome line once decided) and what is being
	// approved (for the denied outcome line).
	approvalStart  int
	approvalName   string
	approvalDetail string

	// Question state (ask_user_question tool).
	questionActive     bool
	questionText       string
	questionOptions    []string
	questionAllowOther bool
	questionCh         chan<- string
	questionSelected   int
	questionInput      string // custom text being typed by the user
	questionCursorPos  int    // cursor position within questionInput

	// Input lines submitted while a turn was running; dispatched in order
	// when the turn finishes. On error/cancel they are restored into the
	// textarea instead of auto-sent.
	queued []string

	// Autocomplete state.
	autocomplete autocompleteState

	// Picker state.
	picker pickerState

	// commandInterleaves maps a history index to command-output blocks that
	// were emitted at that point. They survive rebuildOutput (e.g. Ctrl-O)
	// and are interleaved at the correct chronological position. Cleared
	// on /resume and /new-session.
	commandInterleaves map[int][][]committedLine

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
	roleBanner      = "banner"
	roleUser        = "user"
	roleAgent       = "agent"       // agent text / thinking
	roleAgentTool   = "agentTool"   // tool call
	roleAgentResult = "agentResult" // tool result
	roleCommand     = "command"
)

// bottomPad is the number of blank lines kept below the newest message so it
// isn't glued to the input box and there's a little room to scroll past it.
const bottomPad = 4

// maxInputRows is the tallest the input box may grow to on screen before it
// starts scrolling its content internally.
const maxInputRows = 10

// maxLogicalLines caps how many newline-separated lines the input may hold. It
// only bounds logical lines (a paste guard); the visible height is maxInputRows.
const maxLogicalLines = 500

// spinnerFrames are the braille spinner animation frames for the running indicator.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

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
	ta.Placeholder = "type a message or /command...  (Ctrl-J for newline)"
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
	// MaxHeight also caps the number of logical lines the textarea will hold
	// (it refuses newlines past MaxHeight), so keep it generous. The *visible*
	// box height is capped separately at 10 rows by maxInputHeight(); taller
	// content scrolls within the box.
	ta.MaxHeight = maxLogicalLines
	ta.Focus()

	// Override keymap: plain Enter submits (handled in Update), Ctrl-J or
	// Alt-Enter inserts a newline. Plain "enter"/"ctrl+m" are removed from the
	// InsertNewline binding so they fall through to the submit handler; the
	// remaining keys are the reliable cross-terminal newline chords.
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j", "alt+enter")
	ta.KeyMap.InsertNewline.SetEnabled(true)

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
// textarea value. It always shows a popup when there are suggestions (never
// auto-applies a single match). Called on Tab and on every keystroke when not
// suppressed.
func (m *tuiModel) computeAutocomplete() {
	input := m.textarea.Value()

	// Try @file mention autocomplete first.
	if suggestions := autocompleteFileMention(input); len(suggestions) > 0 {
		m.autocomplete = autocompleteState{
			suggestions: suggestions,
			selected:    0,
		}
		return
	}

	// Try slash command autocomplete.
	suggestions := autocompleteCommand(input, len(input))
	if len(suggestions) > 0 {
		m.autocomplete = autocompleteState{
			suggestions: suggestions,
			selected:    0,
		}
		return
	}

	m.autocomplete = autocompleteState{}
}

// applyAutocomplete applies the currently-selected autocomplete suggestion to
// the textarea and dismisses the popup.
func (m *tuiModel) applyAutocomplete() {
	if len(m.autocomplete.suggestions) == 0 {
		return
	}
	choice := m.autocomplete.suggestions[m.autocomplete.selected]
	// Check whether this is a file-mention completion (input has active @).
	if autocompleteFileMention(m.textarea.Value()) != nil {
		m.applyFileCompletion(choice)
	} else {
		m.applyCompletion(choice)
	}
	m.autocomplete = autocompleteState{}
}

// applyFileCompletion replaces the @mention query at the end of the input
// with @<choice> and adds a trailing space.
func (m *tuiModel) applyFileCompletion(choice string) {
	input := m.textarea.Value()

	// Find the @ that starts the current mention (last @ preceded by
	// start-of-string or whitespace).
	atIdx := -1
	for i := len(input) - 1; i >= 0; i-- {
		if input[i] == '@' {
			if i == 0 || input[i-1] == ' ' || input[i-1] == '\t' || input[i-1] == '\n' {
				atIdx = i
				break
			}
		}
		if input[i] == ' ' || input[i] == '\t' || input[i] == '\n' {
			break
		}
	}
	if atIdx < 0 {
		return
	}

	newValue := input[:atIdx+1] + choice + " "
	m.textarea.SetValue(newValue)
	m.textarea.CursorEnd()
	m.resize()
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
	m.resize()
}

// updateLiveAutocomplete checks the current input for an active @mention or
// slash-command query and updates the autocomplete state accordingly. This
// enables live suggestions as the user types, without needing to press Tab.
// It never auto-applies a single match — the user must confirm with Tab/Enter.
func (m *tuiModel) updateLiveAutocomplete() {
	input := m.textarea.Value()

	// Try @file mention autocomplete.
	if suggestions := autocompleteFileMention(input); len(suggestions) > 0 {
		selected := m.autocomplete.selected
		if selected >= len(suggestions) {
			selected = 0
		}
		m.autocomplete = autocompleteState{
			suggestions: suggestions,
			selected:    selected,
		}
		m.updateViewportContent()
		return
	}

	// Try slash command autocomplete.
	if suggestions := autocompleteCommand(input, len(input)); len(suggestions) > 0 {
		selected := m.autocomplete.selected
		if selected >= len(suggestions) {
			selected = 0
		}
		m.autocomplete = autocompleteState{
			suggestions: suggestions,
			selected:    selected,
		}
		m.updateViewportContent()
		return
	}

	// No suggestions: clear popup if it was showing.
	if m.autocomplete.suggestions != nil {
		m.autocomplete = autocompleteState{}
		m.updateViewportContent()
	}
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
	m.resize()
	m.updateViewportContent()
}

// --- Bubble Tea interface ---

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, tickCmd(), fastTickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg{time: t, fast: false} })
}

func fastTickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg{time: t, fast: true} })
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
		// Size the input/viewport now so GotoBottom in rebuildOutput uses the
		// real height, not the placeholder 20.
		m.resize()
		m.rebuildOutput()
		return m, nil

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		// Any key other than Ctrl-C withdraws a pending quit confirmation.
		if msg.Type != tea.KeyCtrlC {
			m.quitConfirm = false
		}

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
			case tea.KeyRunes:
				s := string(msg.Runes)
				switch {
				case s == "k":
					if m.picker.selected > 0 {
						m.picker.selected--
					}
				case s == "j":
					if m.picker.selected < len(m.picker.items)-1 {
						m.picker.selected++
					}
				case len(s) == 1 && s[0] >= '1' && s[0] <= '9':
					// Number keys pick an item directly.
					if idx := int(s[0] - '1'); idx < len(m.picker.items) {
						selected := m.picker.items[idx].name
						m.picker = pickerState{}
						m.renderCommand("/resume " + selected)
						return m, nil
					}
				}
				m.updateViewportContent()
				return m, nil
			}
			return m, nil
		}

		// --- Autocomplete active: arrows to navigate, Tab/Enter to confirm, Esc to cancel ---
		if len(m.autocomplete.suggestions) > 0 {
			switch msg.Type {
			case tea.KeyTab:
				m.applyAutocomplete()
				m.updateViewportContent()
				return m, nil
			case tea.KeyShiftTab, tea.KeyUp, tea.KeyCtrlP:
				if m.autocomplete.selected > 0 {
					m.autocomplete.selected--
				} else {
					m.autocomplete.selected = len(m.autocomplete.suggestions) - 1
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyDown, tea.KeyCtrlN:
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

		// --- Approval mode: select yes/no/other, type reason, Enter/Ctrl-C ---
		if m.waitingApproval {
			switch msg.Type {
			case tea.KeyCtrlC:
				// Cancel the agent turn outright.
				m.cancel()
				m.ctx, m.cancel = context.WithCancel(context.Background())
				m.resolveApproval(approvalAnswer{approved: false})
				return m, waitForMsg(m.msgCh)
			case tea.KeyEscape:
				m.resolveApproval(approvalAnswer{approved: false})
				return m, waitForMsg(m.msgCh)
			case tea.KeyUp:
				if m.approvalSelected > 0 {
					m.approvalSelected--
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyDown:
				if m.approvalSelected < 2 {
					m.approvalSelected++
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyLeft:
				if m.approvalSelected == 2 && m.approvalCursorPos > 0 {
					m.approvalCursorPos--
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyRight:
				if m.approvalSelected == 2 && m.approvalCursorPos < runeLen(m.approvalInput) {
					m.approvalCursorPos++
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyHome:
				if m.approvalSelected == 2 {
					m.approvalCursorPos = 0
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyEnd:
				if m.approvalSelected == 2 {
					m.approvalCursorPos = runeLen(m.approvalInput)
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyEnter:
				m.resolveApproval(m.selectedApprovalAnswer())
				return m, waitForMsg(m.msgCh)
			case tea.KeyBackspace:
				if m.approvalSelected == 2 && m.approvalCursorPos > 0 {
					m.approvalInput = deleteRune(m.approvalInput, m.approvalCursorPos-1)
					m.approvalCursorPos--
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyDelete:
				if m.approvalSelected == 2 && m.approvalCursorPos < runeLen(m.approvalInput) {
					m.approvalInput = deleteRune(m.approvalInput, m.approvalCursorPos)
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyRunes:
				s := string(msg.Runes)
				// Number keys 1-3: select directly.
				if m.approvalSelected != 2 && len(s) == 1 && s[0] >= '1' && s[0] <= '3' {
					idx := int(s[0] - '1')
					m.approvalSelected = idx
					if idx < 2 {
						// Yes or No: submit immediately.
						m.resolveApproval(m.selectedApprovalAnswer())
						return m, waitForMsg(m.msgCh)
					}
					// "Other": select it so they can type.
					m.updateViewportContent()
					return m, nil
				}
				// y/n shortcuts: submit immediately.
				if m.approvalSelected != 2 {
					switch s {
					case "y", "Y":
						m.resolveApproval(approvalAnswer{approved: true})
						return m, waitForMsg(m.msgCh)
					case "n", "N":
						m.resolveApproval(approvalAnswer{approved: false})
						return m, waitForMsg(m.msgCh)
					}
				}
				// Typing into "other" input.
				if m.approvalSelected == 2 {
					m.approvalInput = insertRunes(m.approvalInput, m.approvalCursorPos, s)
					m.approvalCursorPos += len(msg.Runes)
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeySpace:
				if m.approvalSelected == 2 {
					m.approvalInput = insertRunes(m.approvalInput, m.approvalCursorPos, " ")
					m.approvalCursorPos++
				}
				m.updateViewportContent()
				return m, nil
			}
			return m, nil
		}

		// --- Question mode: navigate options, type answer, Enter/Ctrl-C ---
		if m.questionActive {
			otherIdx := len(m.questionOptions)
			editingOther := m.questionAllowOther && m.questionSelected == otherIdx

			switch msg.Type {
			case tea.KeyCtrlC, tea.KeyEscape:
				// Cancel the question.
				m.questionActive = false
				if m.questionCh != nil {
					m.questionCh <- ""
				}
				m.questionCh = nil
				m.updateViewportContent()
				return m, waitForMsg(m.msgCh)
			case tea.KeyUp:
				if m.questionSelected > 0 {
					m.questionSelected--
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyDown:
				maxIdx := otherIdx
				if !m.questionAllowOther {
					maxIdx--
				}
				if m.questionSelected < maxIdx {
					m.questionSelected++
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyLeft:
				if editingOther && m.questionCursorPos > 0 {
					m.questionCursorPos--
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyRight:
				if editingOther && m.questionCursorPos < runeLen(m.questionInput) {
					m.questionCursorPos++
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyHome:
				if editingOther {
					m.questionCursorPos = 0
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyEnd:
				if editingOther {
					m.questionCursorPos = runeLen(m.questionInput)
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyEnter:
				answer := m.selectedQuestionAnswer()
				m.questionActive = false
				if m.questionCh != nil {
					m.questionCh <- answer
				}
				m.questionCh = nil
				m.updateViewportContent()
				return m, waitForMsg(m.msgCh)
			case tea.KeyBackspace:
				if editingOther && m.questionCursorPos > 0 {
					m.questionInput = deleteRune(m.questionInput, m.questionCursorPos-1)
					m.questionCursorPos--
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyDelete:
				if editingOther && m.questionCursorPos < runeLen(m.questionInput) {
					m.questionInput = deleteRune(m.questionInput, m.questionCursorPos)
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeyRunes:
				s := string(msg.Runes)
				// Number keys 1-9: select option directly (only when not editing other text).
				if !editingOther && len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
					idx := int(s[0] - '1')
					maxIdx := otherIdx
					if !m.questionAllowOther {
						maxIdx--
					}
					if idx <= maxIdx {
						m.questionSelected = idx
						// Submit immediately on number key — but if on "other", let them type.
						if !m.questionAllowOther || idx < otherIdx {
							answer := m.selectedQuestionAnswer()
							m.questionActive = false
							if m.questionCh != nil {
								m.questionCh <- answer
							}
							m.questionCh = nil
							m.updateViewportContent()
							return m, waitForMsg(m.msgCh)
						}
						// On "other": just select it so they can start typing.
						m.updateViewportContent()
						return m, nil
					}
				}
				// Any printable char: type into the "other" input at cursor position.
				if m.questionAllowOther {
					if !editingOther {
						m.questionSelected = otherIdx
					}
					m.questionInput = insertRunes(m.questionInput, m.questionCursorPos, s)
					m.questionCursorPos += len(msg.Runes)
				}
				m.updateViewportContent()
				return m, nil
			case tea.KeySpace:
				// Space: type into the "other" input at cursor position.
				if m.questionAllowOther {
					if !editingOther {
						m.questionSelected = otherIdx
					}
					m.questionInput = insertRunes(m.questionInput, m.questionCursorPos, " ")
					m.questionCursorPos++
				}
				m.updateViewportContent()
				return m, nil
			default:
				return m, nil
			}
		}

		// Arrow keys, page up/down, home/end: scroll viewport (always, even during streaming).
		// But skip if autocomplete is showing (handled above).
		if msg.Type == tea.KeyUp || msg.Type == tea.KeyDown ||
			msg.Type == tea.KeyCtrlP || msg.Type == tea.KeyCtrlN ||
			msg.Type == tea.KeyPgUp || msg.Type == tea.KeyPgDown ||
			msg.Type == tea.KeyHome || msg.Type == tea.KeyEnd {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

		switch msg.Type {
		case tea.KeyEscape:
			// Esc cancels a running turn, same as Ctrl-C; a no-op when idle.
			if m.agentRunning {
				m.cancel()
				m.ctx, m.cancel = context.WithCancel(context.Background())
				m.flushStreaming()
				m.updateViewportContent()
				return m, waitForMsg(m.msgCh)
			}
			return m, nil

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
			// Not running, but the input has text: clear the input instead of
			// quitting, so a stray Ctrl-C doesn't end the session.
			if m.textarea.Value() != "" {
				m.textarea.Reset()
				m.resize()
				return m, nil
			}
			// Idle and empty input: require a second Ctrl-C so a stray one
			// doesn't end the session.
			if !m.quitConfirm {
				m.quitConfirm = true
				return m, nil
			}
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
			m.computeAutocomplete()
			m.updateViewportContent()
			return m, nil

		case tea.KeyEnter:
			// Alt-Enter inserts a newline instead of submitting. (Ctrl-J does the
			// same and arrives as KeyCtrlJ, handled by the default branch.)
			if msg.Alt {
				return m, m.feedTextarea(msg)
			}
			line := strings.TrimSpace(m.textarea.Value())
			m.textarea.Reset()
			m.resize()
			if line == "" {
				return m, nil
			}
			// While a turn is running, queue the line; it is dispatched when
			// the turn finishes (shown as "N queued" in the hint bar).
			if m.agentRunning {
				m.queued = append(m.queued, line)
				return m, nil
			}
			return m, m.dispatchInput(line)

		default:
			// Grow/shrink the input box as the content wraps or gains newlines.
			cmds = append(cmds, m.feedTextarea(msg))
			m.updateLiveAutocomplete()
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
		m.pendingToolName = msg.name
		m.pendingToolDetail = msg.detail
		m.updateViewportContent()
		return m, waitForMsg(m.msgCh)

	case diffDisplayMsg:
		for _, ln := range msg.lines {
			m.push(roleAgent, ln)
		}
		m.updateViewportContent()
		return m, waitForMsg(m.msgCh)

	case toolResultDisplayMsg:
		m.flushPendingTool()
		// Keep short results inline; skip verbose content.
		m.push(roleAgentResult, renderToolResult(truncateStr(msg.result, 120)))
		m.updateViewportContent()
		return m, waitForMsg(m.msgCh)

	case approvalReqMsg:
		m.flushStreaming()
		m.waitingApproval = true
		m.approvalCh = msg.respond
		m.approvalSelected = 0
		m.approvalInput = ""
		m.approvalCursorPos = 0
		m.commitApproval(msg.name, msg.detail)
		m.updateViewportContent()
		return m, nil

	case turnDoneMsg:
		m.flushStreaming()
		m.agentRunning = false
		m.agent.sessionDirty = true
		m.agent.autoSave()
		m.updateViewportContent()
		// Drain input queued during the turn: commands run inline; the first
		// queued message starts the next turn (the rest stay queued).
		for len(m.queued) > 0 {
			line := m.queued[0]
			m.queued = m.queued[1:]
			if cmd := m.dispatchInput(line); cmd != nil {
				return m, cmd
			}
		}
		return m, nil

	case turnErrMsg:
		m.flushStreaming()
		m.agentRunning = false
		// A canceled context means the user pressed Ctrl-C to abort the turn.
		if msg.error == context.Canceled {
			m.push(roleAgent, dimStyle.Render("  [canceled]"))
		} else {
			m.push(roleAgent, renderError(msg.Error()))
			if p := logPathIfWritten(); p != "" {
				m.push(roleAgent, dimStyle.Render("  details logged to "+p))
			}
		}
		// Save now so the user message (and any partial assistant+tools
		// already appended before cancel) is persisted.
		m.agent.sessionDirty = true
		m.agent.autoSave()
		// Don't auto-send input queued behind a failed/canceled turn; put it
		// back in the textarea so the user can edit or resend it.
		if len(m.queued) > 0 {
			m.textarea.SetValue(strings.Join(m.queued, "\n"))
			m.queued = nil
			m.textarea.CursorEnd()
			m.resize()
		}
		m.updateViewportContent()
		return m, nil

	case compactDoneMsg:
		m.flushStreaming()
		m.agentRunning = false
		m.commandInterleaves = nil
		m.bannerSeed = bannerLines(m.agent)
		m.rebuildOutput()
		m.push(roleCommand, dimStyle.Render("  "+msg.result))
		m.agent.sessionDirty = true
		m.agent.autoSave()
		m.updateViewportContent()
		return m, nil

	case questionMsg:
		m.flushStreaming()
		m.questionActive = true
		m.questionText = msg.question
		m.questionOptions = msg.options
		m.questionAllowOther = msg.allowOther
		m.questionCh = msg.respond
		m.questionSelected = 0
		m.questionInput = ""
		m.questionCursorPos = 0
		// If there are no options, default to allow_other=true (open-ended).
		if len(m.questionOptions) == 0 {
			m.questionAllowOther = true
		}
		// Commit the question to the viewport.
		m.push(roleAgent, questionStyle.Render("? "+msg.question))
		m.updateViewportContent()
		return m, nil

	case tickMsg:
		if !msg.fast {
			if m.thinkingActive || m.agentRunning || m.questionActive || m.waitingApproval {
				m.starVisible = !m.starVisible
			}
			if m.pendingToolName != "" {
				m.updateViewportContent()
			}
			if m.agentRunning {
				m.dotIdx = (m.dotIdx + 1) % 3
			}
		}
		if msg.fast && m.agentRunning {
			m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
		}
		if msg.fast {
			cmds = append(cmds, fastTickCmd())
		} else {
			cmds = append(cmds, tickCmd())
		}
	}

	return m, tea.Batch(cmds...)
}

// dispatchInput routes a submitted input line: slash commands execute
// immediately, anything else starts an agent turn. Returns the follow-up
// command (nil when nothing needs to be awaited).
func (m *tuiModel) dispatchInput(line string) tea.Cmd {
	if strings.HasPrefix(line, "/") {
		parts := strings.Fields(strings.TrimPrefix(line, "/"))
		if len(parts) == 0 {
			// Just "/" — show unknown command.
			m.renderCommand(line)
			return nil
		}
		if parts[0] == "resume" && len(parts) == 1 {
			m.openSessionPicker()
			return nil
		}
		m.renderCommand(line)
		if parts[0] == "compact" {
			return waitForMsg(m.msgCh)
		}
		return nil
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

	// Expand @mentions: @filepath and @filepath:LN inject file content as
	// separate content parts in the user message sent to the LLM, but the
	// TUI shows only what the user typed.
	msg := m.agent.expandAtMentions(line)
	m.agent.history = append(m.agent.history, msg)
	m.agent.sessionDirty = true

	// Generate a session summary asynchronously on the first user message.
	if !m.agent.summaryGenerated {
		m.agent.summaryGenerated = true
		go m.agent.generateSessionSummary(line)
	}

	go m.agent.doTurn(m.ctx)
	return waitForMsg(m.msgCh)
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

func (m *tuiModel) flushPendingTool() {
	if m.pendingToolName == "" {
		return
	}
	m.push(roleAgentTool, renderTool(m.pendingToolName, m.pendingToolDetail))
	m.pendingToolName = ""
	m.pendingToolDetail = ""
}

func (m *tuiModel) flushStreaming() {
	m.flushPendingTool()
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

	if m.pendingToolName != "" {
		prevRole := roleAgent
		if n := len(m.committed); n > 0 {
			prevRole = m.committed[n-1].role
		}
		if prevRole != roleAgentTool {
			gap := spacerGap(prevRole, roleAgentTool)
			for j := 0; j < gap; j++ {
				lines = append(lines, "")
			}
		}
		dot := "○"
		if m.starVisible {
			dot = "●"
		}
		lines = append(lines, renderToolWithDot(dot, m.pendingToolName, m.pendingToolDetail))
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
			bar := agentStyle.Render(agentBar) + " "
			cw := m.contentWidth()
			wrapWidth := cw - 2
			if wrapWidth < 20 {
				wrapWidth = 80
			}
			// Render streaming content through the same markdown pipeline as
			// committed text, so the display doesn't visibly reflow/recolor
			// when the turn ends. Very large buffers fall back to plain
			// wrapping to keep per-chunk render cost bounded.
			var streamed []string
			if len(m.streamingLine) <= 16384 {
				streamed = strings.Split(renderMarkdown(m.streamingLine, wrapWidth), "\n")
			} else {
				streamed = wordWrap(m.streamingLine, wrapWidth)
			}
			for _, ln := range streamed {
				lines = append(lines, bar+ln)
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

// resize sizes the input to its content (1 line when empty, growing as it
// wraps, capped) and gives the remaining height to the viewport. It must run in
// Update — not View — so the persisted textarea/viewport heights stay in sync
// with the content; otherwise the sticky-bottom math in updateViewportContent
// runs against a stale viewport height and the layout only reconciles on the
// next unrelated event.
//
// LineInfo().Height is the soft-wrapped visual row count of the current logical
// line; with multi-line input we also account for extra logical lines so the
// box grows for hard newlines too.
func (m *tuiModel) resize() {
	if !m.ready {
		return
	}
	rows := m.textarea.LineInfo().Height
	if lc := m.textarea.LineCount(); lc > rows {
		rows = lc
	}
	taHeight := min(max(1, rows), m.maxInputHeight())
	m.textarea.SetHeight(taHeight)
	m.viewport.Height = max(1, m.height-2-taHeight) // hint + separator + input area
}

// maxInputHeight is the tallest the input box may grow to: capped at 10 rows,
// but never taller than the window leaves room for.
func (m *tuiModel) maxInputHeight() int {
	return min(maxInputRows, max(1, m.height-2))
}

// feedTextarea forwards a content-changing key to the textarea. It first grows
// the textarea to its max height so the textarea's internal repositionView
// (which runs during Update) has enough room and never scrolls the first
// wrapped row out of view; resize then shrinks the box back to fit the content.
func (m *tuiModel) feedTextarea(msg tea.Msg) tea.Cmd {
	m.textarea.SetHeight(m.maxInputHeight())
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.resize()
	return cmd
}

func (m tuiModel) View() string {
	if !m.ready {
		return "initializing...\n"
	}

	var b strings.Builder

	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	// Key hints — dynamic based on state.
	ctrlCLabel := "Ctrl-C quit"
	if m.agentRunning {
		ctrlCLabel = "Esc cancel"
	} else if m.quitConfirm {
		ctrlCLabel = "Ctrl-C again to quit"
	}
	ctxPct := ""
	if t := m.agent.tokenUsage.Total; t > 0 {
		if cw := m.agent.contextWindow(); cw > 0 {
			ctxPct = fmt.Sprintf(" · %d%% ctx", t*100/cw)
		}
	}
	queued := ""
	if n := len(m.queued); n > 0 {
		queued = fmt.Sprintf(" · %d queued", n)
	}
	// Truncate to one row: a soft-wrapped hint line would break the fixed
	// hint+separator layout math and push the input area off-screen.
	hint := dimStyle.Render(fmt.Sprintf("%s · %s%s%s", ctrlCLabel, m.agent.effectiveModel(), ctxPct, queued))
	b.WriteString(lipgloss.NewStyle().MaxWidth(max(1, m.width-2)).Render(hint))
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

	// Input area. While a turn runs the spinner status occupies the input
	// box; typing is still possible (input gets queued) but not encouraged —
	// the status only yields to the textarea once the user starts typing.
	if m.waitingApproval {
		b.WriteString(m.renderApprovalInput())
	} else if m.questionActive {
		b.WriteString(m.renderQuestionInput())
	} else if m.agentRunning && m.textarea.Value() == "" {
		frame := spinnerFrames[m.spinnerIdx%len(spinnerFrames)]
		root := "generating"
		if m.thinkingActive {
			root = "thinking"
		} else if m.pendingToolName != "" {
			root = "running " + m.pendingToolName
		}
		dots := strings.Repeat(".", m.dotIdx+1)
		b.WriteString(dimStyle.Render("  " + frame + " " + root + dots))
	} else {
		b.WriteString(m.textarea.View())
	}

	return appStyle.Render(b.String())
}

// renderPicker renders the session picker popup as a bordered box.
func (m *tuiModel) renderPicker() string {
	// Determine box width: longest label + padding.
	maxLine := 0
	for _, it := range m.picker.items {
		label := it.summary
		if label == "" {
			label = it.name
		}
		lineLen := lipgloss.Width(label)
		if lineLen > maxLine {
			maxLine = lineLen
		}
	}
	// marker (2) + number prefix (3) + border/padding (4) + slack.
	boxWidth := min(maxLine+11, m.width-4)
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
		// Number prefix for the first 9 items — they can be picked directly
		// with the 1-9 keys.
		if i < 9 {
			marker += fmt.Sprintf("%d. ", i+1)
		} else {
			marker += "   "
		}
		var label string
		if it.summary != "" {
			label = it.summary
		} else {
			label = dimStyle.Render(it.name)
		}
		if it.current {
			label = lipgloss.NewStyle().Underline(true).Render(label)
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

// selectedQuestionAnswer returns the answer based on the current question state.
func (m *tuiModel) selectedQuestionAnswer() string {
	hasOptions := len(m.questionOptions) > 0
	otherIdx := len(m.questionOptions)

	if hasOptions && m.questionSelected < otherIdx {
		return m.questionOptions[m.questionSelected]
	}
	// Custom input or "other" selected.
	if m.questionInput != "" {
		return m.questionInput
	}
	// User hit Enter on "other..." without typing — return empty.
	return ""
}

// renderQuestionInput renders the question options and input area.
func (m *tuiModel) renderQuestionInput() string {
	var b strings.Builder

	selectStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	dimOptStyle := dimStyle

	for i, opt := range m.questionOptions {
		marker := "  "
		prefix := fmt.Sprintf("%d.", i+1)
		if i == m.questionSelected {
			marker = "> "
			b.WriteString(selectStyle.Render(marker + prefix + " " + opt))
		} else {
			b.WriteString(dimOptStyle.Render(marker + prefix + " " + opt))
		}
		b.WriteString("\n")
	}

	// "Other..." entry for custom input.
	if m.questionAllowOther {
		otherIdx := len(m.questionOptions)
		editingOther := m.questionSelected == otherIdx

		if editingOther {
			// Show the input with cursor when selected.
			before, at, after := splitAtCursor(m.questionInput, m.questionCursorPos)

			cursorChar := " "
			if m.starVisible {
				cursorChar = "▌"
			}
			cursor := selectStyle.Render(cursorChar)

			label := before + cursor + at + after
			if label == "" {
				label = cursor
			}
			b.WriteString(selectStyle.Render("> ") + label)
		} else {
			label := m.questionInput
			if label == "" {
				label = "other..."
			}
			b.WriteString(dimOptStyle.Render("  " + label))
		}
		b.WriteString("\n")
	}

	// Prompt line.
	if m.questionAllowOther {
		b.WriteString(dimStyle.Render("  type answer or select option (↑↓), Enter to confirm, Esc to cancel"))
	} else {
		b.WriteString(dimStyle.Render("  select option (↑↓ or 1-9), Enter to confirm, Esc to cancel"))
	}
	b.WriteString("\n")

	return b.String()
}

// renderApprovalInput renders the approval options and input area.
func (m *tuiModel) renderApprovalInput() string {
	var b strings.Builder

	selectStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	dimOptStyle := dimStyle

	items := []string{"1. Approve", "2. Deny", "3. Other (deny with reason)"}
	for i, label := range items {
		if i == m.approvalSelected {
			if i == 2 {
				// "Other" selected: show inline edit with cursor.
				before, at, after := splitAtCursor(m.approvalInput, m.approvalCursorPos)
				cursorChar := " "
				if m.starVisible {
					cursorChar = "▌"
				}
				cursor := selectStyle.Render(cursorChar)
				display := before + cursor + at + after
				if display == "" {
					display = cursor
				}
				b.WriteString(selectStyle.Render("> 3. deny reason: ") + display)
			} else {
				b.WriteString(selectStyle.Render("> " + label))
			}
		} else {
			b.WriteString(dimOptStyle.Render("  " + label))
		}
		b.WriteString("\n")
	}

	if m.approvalSelected == 2 {
		b.WriteString(dimStyle.Render("  type reason, Enter to submit, Esc to deny without reason"))
	} else {
		b.WriteString(dimStyle.Render("  ↑↓ or 1-3 or y/n, Enter to confirm, Esc to deny"))
	}
	b.WriteString("\n")

	return b.String()
}

// selectedApprovalAnswer returns the approval answer based on current state.
func (m *tuiModel) selectedApprovalAnswer() approvalAnswer {
	switch m.approvalSelected {
	case 0:
		return approvalAnswer{approved: true}
	case 1:
		return approvalAnswer{approved: false}
	default: // 2 = other
		return approvalAnswer{approved: false, reason: m.approvalInput}
	}
}

func (m *tuiModel) renderAutocomplete() string {
	// Determine if suggestions should be rendered vertically (file paths are
	// typically long; slash commands are short). Use vertical layout when any
	// suggestion exceeds 15 characters or there are more than 5 suggestions.
	vertical := len(m.autocomplete.suggestions) > 5
	if !vertical {
		for _, s := range m.autocomplete.suggestions {
			if len(s) > 15 {
				vertical = true
				break
			}
		}
	}

	if vertical {
		var lines []string
		for i, s := range m.autocomplete.suggestions {
			marker := "  "
			if i == m.autocomplete.selected {
				marker = "> "
			}
			var styled string
			if i == m.autocomplete.selected {
				styled = lipgloss.NewStyle().
					Foreground(lipgloss.Color("0")).
					Background(lipgloss.Color("6")).
					Render(s)
			} else {
				styled = dimStyle.Render(s)
			}
			lines = append(lines, "  "+marker+styled)
		}
		return strings.Join(lines, "\n") + "\n"
	}

	var parts []string
	for i, s := range m.autocomplete.suggestions {
		// Pad every item to the same visual width so the row doesn't shift
		// horizontally as the selection moves.
		var styled string
		if i == m.autocomplete.selected {
			styled = lipgloss.NewStyle().
				Foreground(lipgloss.Color("0")).
				Background(lipgloss.Color("6")).
				Render(" " + s + " ")
		} else {
			styled = dimStyle.Render(" " + s + " ")
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
			m.commitUser(userMessageText(msg))
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
			// Show assistant text content first, then tool calls.
			if text := msg.OfAssistant.Content.OfString.Value; text != "" {
				m.commitAgent(text)
			}
			if len(msg.OfAssistant.ToolCalls) > 0 {
				for _, tc := range msg.OfAssistant.ToolCalls {
					toolCallNames[tc.ID] = tc.Function.Name
					m.push(roleAgentTool, renderTool(tc.Function.Name, toolCallBrief(tc)))
				}
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
			m.push(roleAgentResult, renderToolResult(truncateStr(content, 120)))
		}
		// Interleave any command outputs that were emitted after this
		// history message.
		if m.commandInterleaves != nil {
			for _, block := range m.commandInterleaves[i] {
				m.committed = append(m.committed, block...)
			}
		}
	}
	// Append command outputs emitted after the last history message
	// (histIdx >= len(m.agent.history)).
	if m.commandInterleaves != nil {
		for i := len(m.agent.history); ; i++ {
			blocks, ok := m.commandInterleaves[i]
			if !ok {
				break
			}
			for _, block := range blocks {
				m.committed = append(m.committed, block...)
			}
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
	// and old messages are cleared. Also clear saved command outputs.
	switch cmdName {
	case "resume", "new-session", "clear":
		m.commandInterleaves = nil
		m.bannerSeed = bannerLines(m.agent)
		m.rebuildOutput()
	case "compact":
		m.commandInterleaves = nil
		m.agentRunning = true
	case "config":
		if strings.HasPrefix(cmd, "config thinking-detail") {
			m.rebuildOutput()
		}
	}

	// Remember the history length *before* we push anything, so command
	// output is interleaved at the right spot during rebuilds.
	histIdx := len(m.agent.history)

	// Echo the typed command as a user prompt, same as a normal message.
	m.commitUser(line)

	prevLen := len(m.committed)

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

	// Save the command output so it survives rebuildOutput (e.g. Ctrl-O).
	// Store it keyed by the history index at which it was emitted, so it
	// stays in the correct chronological position on rebuild.
	if saved := m.committed[prevLen:]; len(saved) > 0 {
		cp := make([]committedLine, len(saved))
		copy(cp, saved)
		if m.commandInterleaves == nil {
			m.commandInterleaves = make(map[int][][]committedLine)
		}
		m.commandInterleaves[histIdx] = append(m.commandInterleaves[histIdx], cp)
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
	prefix := "❯ "
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
	// Agent text carries a green gutter bar down every line — the speaker is
	// identified by the running color, not a prompt label.
	bar := agentStyle.Render(agentBar) + " "
	cw := m.contentWidth()
	wrapWidth := cw - 2
	if wrapWidth < 20 {
		wrapWidth = 80
	}
	rendered := renderMarkdown(text, wrapWidth)
	for _, line := range strings.Split(rendered, "\n") {
		m.push(roleAgent, bar+line)
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

// commitApproval pushes the approval subject. Bash commands get a
// GitHub-style code block (the Approve/Deny options below act as the
// question); other tools get an "Allow <name>?" line with the subject
// indented under it. The subject is shown verbatim — soft-wrapped to the
// viewport width but never reformatted — so what the user reads is exactly
// what will run.
func (m *tuiModel) commitApproval(name, detail string) {
	start := len(m.committed)
	// The preceding tool-call line shows the same subject as the block below
	// and is re-displayed after approval; drop it now so the command never
	// appears twice.
	if start > 0 && m.committed[start-1].role == roleAgentTool {
		start--
		m.committed = m.committed[:start]
	}
	m.approvalStart = start
	m.approvalName = name
	m.approvalDetail = detail

	if cmd, ok := strings.CutPrefix(detail, "$ "); ok {
		// Bash: a GitHub-style code block (dark slab, syntax highlight).
		// No question line — the Approve/Deny options right below are the
		// question.
		wrapWidth := m.contentWidth()
		if wrapWidth < 20 {
			wrapWidth = 80
		}
		for _, line := range renderShellBlock(cmd, wrapWidth) {
			m.push(roleAgent, line)
		}
		return
	}
	wrapWidth := m.contentWidth() - 2
	if wrapWidth < 20 {
		wrapWidth = 80
	}
	m.push(roleAgent, approvalStyle.Render("Allow "+name+"?"))
	for _, line := range wordWrap(detail, wrapWidth) {
		m.push(roleAgent, "  "+cmdTextStyle.Render(line))
	}
}

// foldApproval collapses the decided approval block to a single outcome line
// so it stops occupying transcript space.
func (m *tuiModel) foldApproval(answer approvalAnswer) {
	if m.approvalStart >= 0 && m.approvalStart <= len(m.committed) {
		m.committed = m.committed[:m.approvalStart]
	}
	m.approvalStart = -1
	if answer.approved {
		// The approved tool call is re-displayed (with its detail) right
		// after this line, so the outcome stays terse.
		m.push(roleAgent, okStyle.Render("✓")+" "+dimStyle.Render("approved"))
		return
	}
	// A denied call never runs, so keep what was denied in the transcript.
	brief := truncateStr(strings.Join(strings.Fields(m.approvalName+" "+m.approvalDetail), " "), 80)
	line := errStyle.Render("✗") + " " + dimStyle.Render("denied "+brief)
	if answer.reason != "" {
		line += " " + dimStyle.Render("— "+answer.reason)
	}
	m.push(roleAgent, line)
}

// resolveApproval folds the approval block into its outcome line and sends
// the answer to the waiting agent goroutine.
func (m *tuiModel) resolveApproval(answer approvalAnswer) {
	m.waitingApproval = false
	m.foldApproval(answer)
	if m.approvalCh != nil {
		select {
		case m.approvalCh <- answer:
		default:
		}
	}
	m.approvalCh = nil
	m.updateViewportContent()
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

// --- rune helpers ---

// Cursor positions in the inline inputs (approval reason, question "other"
// answer) are rune indices, not byte offsets, so multibyte input (CJK, emoji)
// moves and edits cleanly.

func runeLen(s string) int { return len([]rune(s)) }

func insertRunes(s string, pos int, ins string) string {
	r := []rune(s)
	if pos < 0 {
		pos = 0
	}
	if pos > len(r) {
		pos = len(r)
	}
	return string(r[:pos]) + ins + string(r[pos:])
}

func deleteRune(s string, pos int) string {
	r := []rune(s)
	if pos < 0 || pos >= len(r) {
		return s
	}
	return string(r[:pos]) + string(r[pos+1:])
}

// splitAtCursor splits s at rune position pos into (before, rune-at, after)
// for rendering an inline cursor.
func splitAtCursor(s string, pos int) (before, at, after string) {
	r := []rune(s)
	if pos < 0 {
		pos = 0
	}
	if pos > len(r) {
		pos = len(r)
	}
	before = string(r[:pos])
	if pos < len(r) {
		at = string(r[pos])
		after = string(r[pos+1:])
	}
	return
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
