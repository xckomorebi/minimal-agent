package main

import (
	"encoding/json"
	"fmt"
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
	APIKey         *string `json:"api_key,omitempty"`
	BaseURL        *string `json:"base_url,omitempty"`
	Model          *string `json:"model,omitempty"`
	Thinking       *bool   `json:"thinking,omitempty"`
	ThinkingEffort *string `json:"thinking_effort,omitempty"`
	AutoEdit       *bool   `json:"auto_edit,omitempty"`
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
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				fmt.Fprintf(os.Stderr, "config watcher error: %s\n", err.Error())
			}
		}
	}()
	return nil
}
