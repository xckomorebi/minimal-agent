package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openai/openai-go"
)

// handleCommandStr processes a command and returns a display string.
func (a *agent) handleCommandStr(cmd string) string {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return ""
	}
	switch parts[0] {
	case "save":
		if len(parts) > 1 {
			oldName := a.sessionName
			oldPath := sessionPath(oldName)
			a.sessionName = parts[1]
			a.sessionDirty = true
			os.Remove(oldPath)
		}
		if err := a.saveSession(); err != nil {
			return "save error: " + err.Error()
		}
		return fmt.Sprintf("saved %q (%d messages)", a.sessionName, len(a.history))
	case "resume":
		if len(parts) < 2 {
			return "usage: /resume <name>  (use /list-session to see saved sessions)"
		}
		name := parts[1]
		if err := a.loadSession(name); err != nil {
			return "load error: " + err.Error()
		}
		return fmt.Sprintf("loaded %q (%d messages)", name, len(a.history))
	case "new-session":
		name := ""
		if len(parts) > 1 {
			name = parts[1]
		} else {
			name = fmt.Sprintf("session-%s", time.Now().Format("20060102-150405"))
		}
		a.autoSave()
		a.history = []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(buildSystemMessage()),
		}
		a.sessionName = name
		a.sessionDirty = true
		return fmt.Sprintf("new session %q", name)
	case "list-session":
		names, err := listSessions()
		if err != nil {
			return "list error: " + err.Error()
		}
		if len(names) == 0 {
			return "(no saved sessions)"
		}
		var b strings.Builder
		for i, n := range names {
			if i > 0 {
				b.WriteString(", ")
			}
			if n == a.sessionName {
				b.WriteString("*" + n)
			} else {
				b.WriteString(n)
			}
		}
		return b.String()
	case "config":
		return a.handleConfigStr(parts[1:])
	default:
		return "unknown command: /" + parts[0]
	}
}

// handleConfigStr returns config info as a multi-line string.
func (a *agent) handleConfigStr(args []string) string {
	c := readGlobalCfg()
	if len(args) == 0 {
		model := a.effectiveModel()
		src := "(default)"
		if a.flagModel != "" {
			src = "(flag)"
		} else if a.config.Model != nil && *a.config.Model != "" {
			src = "(session)"
		} else if c != nil && c.Model != nil && *c.Model != "" {
			src = "(config file)"
		} else if os.Getenv("MA_MODEL") != "" {
			src = "(env)"
		}
		auto := onOff(a.autoEdit())
		think := onOff(a.thinking())
		effort := effortString(a.thinkingEffort())
		detail := onOff(a.thinkingDetail())
		return fmt.Sprintf("model     : %s %s\nauto-edit : %s\nthinking  : %s\neffort    : %s\ndetail    : %s",
			model, src, auto, think, effort, detail)
	}
	switch args[0] {
	case "model":
		if len(args) < 2 {
			return "usage: /config model <model-id>"
		}
		m := args[1]
		a.config.Model = &m
		a.sessionDirty = true
		return "model: " + m
	case "auto-edit":
		v := !a.autoEdit()
		a.config.AutoEdit = &v
		a.sessionDirty = true
		return "auto-edit: " + onOff(v)
	case "thinking":
		v := !a.thinking()
		a.config.Thinking = &v
		a.sessionDirty = true
		return "thinking: " + onOff(v)
	case "thinking-effort":
		if len(args) < 2 {
			return "usage: /config thinking-effort <low|medium|high>"
		}
		level := strings.ToLower(args[1])
		if level != "low" && level != "medium" && level != "high" {
			return "unknown effort level " + level + " (use low, medium, high)"
		}
		a.config.ThinkingEffort = &level
		a.sessionDirty = true
		return "thinking-effort: " + level
	case "thinking-detail":
		v := !a.thinkingDetail()
		a.config.ThinkingDetail = &v
		a.sessionDirty = true
		return "thinking-detail: " + onOff(v)
	default:
		return "unknown config key " + args[0] + "; try model, auto-edit, thinking, thinking-effort, thinking-detail"
	}
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
