package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openai/openai-go"
)

const sessionDir = ".ma-sessions"

// sessionConfig holds per-session overrides. Fields use pointers so that
// unset keys (nil) are omitted from JSON and fall through to global defaults.
type sessionConfig struct {
	Model          *string `json:"model,omitempty"`
	AutoEdit       *bool   `json:"auto_edit,omitempty"`
	Thinking       *bool   `json:"thinking,omitempty"`
	ThinkingEffort *string `json:"thinking_effort,omitempty"`
	ThinkingDetail *bool   `json:"thinking_detail,omitempty"`
	ContextWindow  *int64  `json:"context_window,omitempty"`
	Stream         *bool   `json:"stream,omitempty"`
}

// sessionFile is the top-level JSON structure stored in a session file.
type sessionFile struct {
	Config     sessionConfig                            `json:"config"`
	History    []openai.ChatCompletionMessageParamUnion `json:"history"`
	Summary    string                                   `json:"summary,omitempty"`
	TokenUsage tokenUsage                               `json:"token_usage,omitempty"`
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
	sf := sessionFile{
		Config:     a.config,
		History:    a.history,
		Summary:    a.summary,
		TokenUsage: a.tokenUsage,
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
	var sf sessionFile
	if err := json.Unmarshal(data, &sf); err == nil && sf.History != nil {
		a.config = sf.Config
		a.history = cleanHistory(sf.History)
		if len(a.history) == 0 {
			return fmt.Errorf("empty session %q", name)
		}
		a.sessionName = name
		a.sessionDirty = false
		a.summary = sf.Summary
		a.tokenUsage = sf.TokenUsage
		if a.summary != "" {
			a.summaryGenerated = true
		}
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
// command line, the one from MA_SESSION env, or the most recent.
// Returns "" if no session exists and nothing was explicitly requested.
func resolveSession(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("MA_SESSION"); env != "" {
		return env
	}
	names, _ := listSessions()
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// autoSave saves the session if it has changed since the last save.
// Silent in TUI mode — errors go to stderr.
func (a *agent) autoSave() {
	if !a.sessionDirty {
		return
	}
	if err := a.saveSession(); err != nil {
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
