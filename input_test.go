package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func setup(t *testing.T, w, h int) tea.Model {
	t.Helper()
	a := &agent{sessionName: "test"}
	var mm tea.Model = newTUIModel(a)
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return mm
}

func typeRune(mm tea.Model, r rune) tea.Model {
	mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return mm
}

func typeStr(mm tea.Model, s string) tea.Model {
	for _, r := range s {
		if r == '\n' {
			mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
			continue
		}
		mm = typeRune(mm, r)
	}
	return mm
}

// After the persisted-sizing fix, the persisted textarea height must reflect
// wrapping immediately (previously it only changed inside View on a value copy).
func TestPersistedResizeOnWrap(t *testing.T) {
	mm := setup(t, 40, 20)

	if got := mm.(tuiModel).textarea.Height(); got != 1 {
		t.Fatalf("empty input height = %d, want 1", got)
	}

	for range 50 {
		mm = typeRune(mm, 'x')
	}

	if got := mm.(tuiModel).textarea.Height(); got < 2 {
		t.Fatalf("wrapped input persisted height = %d, want >=2", got)
	}
	// Viewport must have shrunk to make room (H - hint - sep - taHeight).
	tm := mm.(tuiModel)
	if tm.viewport.Height != tm.height-2-tm.textarea.Height() {
		t.Fatalf("viewport height %d not in sync with textarea height %d",
			tm.viewport.Height, tm.textarea.Height())
	}
}

// Ctrl-J inserts a newline; the input grows and the value carries the newline.
func TestCtrlJInsertsNewline(t *testing.T) {
	mm := setup(t, 40, 20)
	mm = typeRune(mm, 'a')
	mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	mm = typeRune(mm, 'b')

	val := mm.(tuiModel).textarea.Value()
	if val != "a\nb" {
		t.Fatalf("value = %q, want %q", val, "a\nb")
	}
	if got := mm.(tuiModel).textarea.Height(); got < 2 {
		t.Fatalf("two-line input height = %d, want >=2", got)
	}
}

// Alt-Enter inserts a newline instead of submitting.
func TestAltEnterInsertsNewline(t *testing.T) {
	mm := setup(t, 40, 20)
	mm = typeRune(mm, 'a')
	mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	mm = typeRune(mm, 'b')

	val := mm.(tuiModel).textarea.Value()
	if val != "a\nb" {
		t.Fatalf("value = %q, want %q", val, "a\nb")
	}
	if mm.(tuiModel).agentRunning {
		t.Fatal("Alt-Enter should not submit / start a turn")
	}
}

// The reported bug: when a line wraps, the textarea's internal viewport
// scrolled the first row out of view. After the fix the first row must remain
// visible in the rendered box.
func TestFirstWrappedRowStaysVisible(t *testing.T) {
	mm := setup(t, 60, 20)
	// A line long enough to wrap to a second row at width 60.
	mm = typeStr(mm, "the quick brown fox jumps over the lazy dog and then keeps running far away")

	tm := mm.(tuiModel)
	if tm.textarea.Height() < 2 {
		t.Fatalf("expected wrapped box height >=2, got %d", tm.textarea.Height())
	}
	if !strings.Contains(tm.View(), "the quick brown fox") {
		t.Fatalf("first wrapped row was scrolled out of view; box:\n%s", tm.textarea.View())
	}
}

// More than maxInputRows logical lines must stay separate (no concatenation)
// and the box height must cap at maxInputRows.
func TestManyLinesCapAndStaySeparate(t *testing.T) {
	mm := setup(t, 60, 40)
	lines := make([]string, 16)
	for i := range lines {
		lines[i] = "row"
	}
	mm = typeStr(mm, strings.Join(lines, "\n"))

	tm := mm.(tuiModel)
	if got := tm.textarea.LineCount(); got != 16 {
		t.Fatalf("logical line count = %d, want 16 (newlines dropped/concatenated?)", got)
	}
	if got := tm.textarea.Height(); got != maxInputRows {
		t.Fatalf("box height = %d, want cap %d", got, maxInputRows)
	}
}

// Ctrl-C with non-empty input clears the text instead of quitting.
func TestCtrlCClearsNonEmptyInput(t *testing.T) {
	mm := setup(t, 40, 20)
	mm = typeStr(mm, "some draft message")

	mm, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	tm := mm.(tuiModel)
	if tm.textarea.Value() != "" {
		t.Fatalf("input not cleared: %q", tm.textarea.Value())
	}
	if tm.textarea.Height() != 1 {
		t.Fatalf("input height after clear = %d, want 1", tm.textarea.Height())
	}
	// Must not be the quit command.
	if isQuit(cmd) {
		t.Fatal("Ctrl-C with text should not quit the program")
	}
}

// Ctrl-C with empty input quits the program.
func TestCtrlCEmptyInputQuits(t *testing.T) {
	mm := setup(t, 40, 20)
	_, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !isQuit(cmd) {
		t.Fatal("Ctrl-C with empty input should quit")
	}
}

// isQuit reports whether cmd is tea.Quit (which returns a tea.QuitMsg).
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// Plain Enter submits and resets the input back to one line.
func TestPlainEnterSubmitsAndResets(t *testing.T) {
	mm := setup(t, 40, 20)
	for range 50 {
		mm = typeRune(mm, 'x')
	}
	mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})

	tm := mm.(tuiModel)
	if tm.textarea.Value() != "" {
		t.Fatalf("value after submit = %q, want empty", tm.textarea.Value())
	}
	if tm.textarea.Height() != 1 {
		t.Fatalf("height after submit = %d, want 1", tm.textarea.Height())
	}
	if !strings.Contains(tm.View(), "you>") {
		t.Fatal("submitted message not echoed to output")
	}
}
