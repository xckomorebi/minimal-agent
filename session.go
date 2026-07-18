package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openai/openai-go"
)

const sessionDir = ".ma/sessions"

// sessionConfig holds per-session overrides. Fields use pointers so that
// unset keys (nil) are omitted from JSON and fall through to global defaults.
type sessionConfig struct {
	Model          *string `json:"model,omitempty"`
	AutoEdit       *bool   `json:"auto_edit,omitempty"`
	Thinking       *bool   `json:"thinking,omitempty"`
	ThinkingEffort *string `json:"thinking_effort,omitempty"`
	ThinkingDetail *bool   `json:"thinking_detail,omitempty"`
	SendReasoning  *bool   `json:"send_reasoning,omitempty"`
	Stream         *bool   `json:"stream,omitempty"`
	MaxToolRounds  *int    `json:"max_tool_rounds,omitempty"`
	MaxRepeatCalls *int    `json:"max_repeat_calls,omitempty"`
}

// sessionFile is the top-level JSON structure stored in a session file.
type sessionFile struct {
	Config     sessionConfig                            `json:"config"`
	History    []openai.ChatCompletionMessageParamUnion `json:"history"`
	Summary    string                                   `json:"summary,omitempty"`
	TokenUsage tokenUsage                               `json:"token_usage"`
	FileMtimes map[string]time.Time                     `json:"file_mtimes,omitempty"`
}

// sessionPath returns the file path for a given session name.
func sessionPath(name string) string {
	return filepath.Join(sessionDir, name+".json")
}

// saveSession writes the current history to the session file.
func (a *agent) saveSession() error {
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return err
	}
	slog.Debug("saving session", "name", a.sessionName, "msg_count", len(a.history))
	sf := sessionFile{
		Config:      a.config,
		History:     a.history,
		Summary:     a.summary,
		TokenUsage:  a.tokenUsage,
		FileMtimes:  a.fileMtimes,
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(sessionPath(a.sessionName), data, 0644); err != nil {
		return err
	}
	a.sessionDirty = false
	return nil
}

// loadSession loads history and config from a session file. Returns an error
// if the session does not exist. Handles both the legacy format (bare JSON
// array) and the current format ({"config":..., "history":...}).
func (a *agent) loadSession(name string) error {
	data, err := os.ReadFile(sessionPath(name))
	if err != nil {
		return err
	}

	// Try new format first: {"config":..., "history":...}
	// We unmarshal history twice: once as []json.RawMessage to preserve
	// extra fields (like reasoning_content) that the SDK drops during
	// deserialization, and once as SDK types for the typed fields.
	var raw struct {
		Config     sessionConfig            `json:"config"`
		History    []json.RawMessage        `json:"history"`
		Summary    string                   `json:"summary"`
		TokenUsage tokenUsage               `json:"token_usage"`
		FileMtimes map[string]time.Time     `json:"file_mtimes"`
	}
	if err := json.Unmarshal(data, &raw); err == nil && raw.History != nil {
		// Unmarshal each raw message into the SDK type, then clean in
		// lockstep so indices stay aligned between raw and typed slices.
		type msgPair struct {
			raw  json.RawMessage
			typed openai.ChatCompletionMessageParamUnion
		}
		var pairs []msgPair
		for i, rawMsg := range raw.History {
			var m openai.ChatCompletionMessageParamUnion
			if err := json.Unmarshal(rawMsg, &m); err != nil {
				return fmt.Errorf("corrupt session %q at message %d: %w", name, i, err)
			}
			if isEmptyMessage(m) {
				continue
			}
			pairs = append(pairs, msgPair{raw: rawMsg, typed: m})
		}
		if len(pairs) == 0 {
			return fmt.Errorf("empty session %q", name)
		}

		a.config = raw.Config
		a.history = make([]openai.ChatCompletionMessageParamUnion, len(pairs))
		for i, p := range pairs {
			a.history[i] = p.typed
		}
		a.sessionName = name
		a.sessionDirty = false
		a.summary = raw.Summary
		a.tokenUsage = raw.TokenUsage
		a.fileMtimes = raw.FileMtimes
		if a.summary != "" {
			a.summaryGenerated = true
		}

		// Restore reasoning_content from the raw JSON. The SDK drops extra
		// fields during deserialization, so we extract reasoning_content from
		// the raw message and re-attach it via SetExtraFields. This also
		// rebuilds the in-memory reasonings map for TUI rendering.
		a.reasonings = nil
		for i, p := range pairs {
			if a.history[i].OfAssistant == nil {
				continue
			}
			var probe struct {
				ReasoningContent string `json:"reasoning_content"`
			}
			if json.Unmarshal(p.raw, &probe) == nil && probe.ReasoningContent != "" {
				a.history[i].OfAssistant.SetExtraFields(map[string]any{
					"reasoning_content": probe.ReasoningContent,
				})
				if a.reasonings == nil {
					a.reasonings = make(map[int]string)
				}
				a.reasonings[i] = probe.ReasoningContent
			}
		}
		slog.Debug("session loaded", "name", name, "msg_count", len(a.history), "summary", a.summary)
		return nil
	}

	// Fall back to legacy format: bare JSON array
	var hist []openai.ChatCompletionMessageParamUnion
	if err := json.Unmarshal(data, &hist); err != nil {
		return fmt.Errorf("corrupt session %q: %w", name, err)
	}
	if len(hist) == 0 {
		return fmt.Errorf("empty session %q", name)
	}
	a.history = cleanHistory(hist)
	a.sessionName = name
	a.sessionDirty = false
	return nil
}

// listSessions returns the names of all available sessions, sorted by
// modification time (newest first).
func listSessions() ([]string, error) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type se struct {
		name string
		mod  time.Time
	}
	var sessions []se
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		sessions = append(sessions, se{name, info.ModTime()})
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].mod.After(sessions[j].mod)
	})
	names := make([]string, len(sessions))
	for i, s := range sessions {
		names[i] = s.name
	}
	return names, nil
}

// resolveSession figures out which session to start with: the one given on the
// command line, or the most recent.
// Returns "" if no session exists and nothing was explicitly requested.
func resolveSession(explicit string) string {
	if explicit != "" {
		return explicit
	}
	names, _ := listSessions()
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// autoSave saves the session if it has changed since the last save.
// Errors go to stderr (for TUI visibility) and to the log file.
func (a *agent) autoSave() {
	if !a.sessionDirty {
		return
	}
	if err := a.saveSession(); err != nil {
		slog.Error("auto-save failed", "session", a.sessionName, "error", err)
		fmt.Fprintln(os.Stderr, "auto-save failed:", err)
	}
}

// sessionSummary reads just the summary field from a session file.
// Returns an empty string if no summary is set or the file can't be read.
func sessionSummary(name string) string {
	data, err := os.ReadFile(sessionPath(name))
	if err != nil {
		return ""
	}
	var sf sessionFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return ""
	}
	return sf.Summary
}
