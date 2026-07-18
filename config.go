package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

const globalConfigDir = ".ma"
const globalConfigFile = "settings.json"

// globalConfig holds settings from ~/.ma/settings.json. Fields use pointers
// so that unset keys (nil) are omitted from JSON and fall through to lower layers.
type globalConfig struct {
	APIKey         *string           `json:"api_key,omitempty"`
	BaseURL        *string           `json:"base_url,omitempty"`
	Model          *string           `json:"model,omitempty"`
	Thinking       *bool             `json:"thinking,omitempty"`
	ThinkingEffort *string           `json:"thinking_effort,omitempty"`
	ThinkingDetail *bool             `json:"thinking_detail,omitempty"`
	SendReasoning  *bool             `json:"send_reasoning,omitempty"`
	AutoEdit       *bool             `json:"auto_edit,omitempty"`
	ContextWindow  *int64            `json:"context_window,omitempty"`
	Stream         *bool             `json:"stream,omitempty"`
	MaxToolRounds  *int              `json:"max_tool_rounds,omitempty"`
	MaxRepeatCalls *int              `json:"max_repeat_calls,omitempty"`
	HTTPHeaders    map[string]string `json:"extra_http_headers,omitempty"`
	MCPServers     []mcpServerConfig `json:"mcp_servers,omitempty"`
	Profile        *string           `json:"profile,omitempty"`
	Profiles       map[string]profileConfig `json:"profiles,omitempty"`
}

// profileConfig is a named bundle of provider settings. When a profile is
// active (via the "profile" key or -profile flag), its non-nil fields override
// the top-level globalConfig fields of the same name.
type profileConfig struct {
	APIKey         *string           `json:"api_key,omitempty"`
	BaseURL        *string           `json:"base_url,omitempty"`
	Model          *string           `json:"model,omitempty"`
	Thinking       *bool             `json:"thinking,omitempty"`
	ThinkingEffort *string           `json:"thinking_effort,omitempty"`
	ThinkingDetail *bool             `json:"thinking_detail,omitempty"`
	SendReasoning  *bool             `json:"send_reasoning,omitempty"`
	AutoEdit       *bool             `json:"auto_edit,omitempty"`
	ContextWindow  *int64            `json:"context_window,omitempty"`
	Stream         *bool             `json:"stream,omitempty"`
	MaxToolRounds  *int              `json:"max_tool_rounds,omitempty"`
	MaxRepeatCalls *int              `json:"max_repeat_calls,omitempty"`
	HTTPHeaders    map[string]string `json:"extra_http_headers,omitempty"`
}

// resolvedProfile returns the profileConfig for the active profile, or nil if
// no profile is active or the named profile doesn't exist.
func (cfg *globalConfig) resolvedProfile() *profileConfig {
	if cfg == nil || cfg.Profile == nil || *cfg.Profile == "" {
		return nil
	}
	if p, ok := cfg.Profiles[*cfg.Profile]; ok {
		return &p
	}
	return nil
}

// profileAPIKey resolves the effective API key, consulting the active profile first.
func (cfg *globalConfig) profileAPIKey() string {
	if p := cfg.resolvedProfile(); p != nil && p.APIKey != nil {
		return *p.APIKey
	}
	if cfg.APIKey != nil {
		return *cfg.APIKey
	}
	return ""
}

// profileBaseURL resolves the effective base URL, consulting the active profile first.
func (cfg *globalConfig) profileBaseURL() string {
	if p := cfg.resolvedProfile(); p != nil && p.BaseURL != nil {
		return *p.BaseURL
	}
	if cfg.BaseURL != nil {
		return *cfg.BaseURL
	}
	return ""
}

// profileModel resolves the effective model, consulting the active profile first.
func (cfg *globalConfig) profileModel() string {
	if p := cfg.resolvedProfile(); p != nil && p.Model != nil {
		return *p.Model
	}
	if cfg.Model != nil {
		return *cfg.Model
	}
	return ""
}

// profileHTTPHeaders merges the active profile's headers over the global ones.
func (cfg *globalConfig) profileHTTPHeaders() map[string]string {
	out := make(map[string]string)
	maps.Copy(out, cfg.HTTPHeaders)
	if p := cfg.resolvedProfile(); p != nil {
		maps.Copy(out, p.HTTPHeaders)
	}
	return out
}

// mcpServerConfig describes an MCP server to connect to on startup.
// Either Command (for stdio) or URL (for streamable HTTP) must be set.
type mcpServerConfig struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
}

var (
	globalCfg *globalConfig
	globalMu  sync.RWMutex
)

func readGlobalCfg() *globalConfig {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalCfg
}

func globalConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, globalConfigDir, globalConfigFile), nil
}

func loadGlobalConfig() *globalConfig {
	path, err := globalConfigPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg globalConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	return &cfg
}

func startConfigWatcher() error {
	path, err := globalConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return err
	}

	go func() {
		defer w.Close()
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Clean(ev.Name) != filepath.Clean(path) {
					continue
				}
				if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
					globalMu.Lock()
					globalCfg = nil
					globalMu.Unlock()
					continue
				}
				if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) {
					globalMu.Lock()
					globalCfg = loadGlobalConfig()
					globalMu.Unlock()
					slog.Debug("config file reloaded", "path", path)
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				slog.Error("config watcher error", "error", err)
				fmt.Fprintf(os.Stderr, "config watcher error: %s\n", err.Error())
			}
		}
	}()
	return nil
}
