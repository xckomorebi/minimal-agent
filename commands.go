package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openai/openai-go"
)

// handleCommand processes a session-management command entered as "/cmd [arg]".
func (a *agent) handleCommand(cmd string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return
	}
	switch parts[0] {
	case "save":
		if len(parts) > 1 {
			oldName := a.sessionName
			oldPath := sessionPath(oldName)
			a.sessionName = parts[1]
			a.sessionDirty = true
			os.Remove(oldPath)
			fmt.Printf("  renamed %q -> %q\n", oldName, a.sessionName)
		}
		if err := a.saveSession(); err != nil {
			fmt.Println("  " + red("save error: "+err.Error()))
		} else {
			fmt.Printf("  saved %q (%d messages)\n", a.sessionName, len(a.history))
		}
	case "resume":
		if len(parts) < 2 {
			fmt.Println("  usage: /resume <name>  (use /list-session to see saved sessions)")
			return
		}
		name := parts[1]
		if err := a.loadSession(name); err != nil {
			fmt.Println("  " + red("load error: "+err.Error()))
		} else {
			fmt.Printf("  loaded %q (%d messages)\n", name, len(a.history))
			a.printHistory()
		}
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
		fmt.Printf("  new session %q\n", name)
	case "list-session":
		names, err := listSessions()
		if err != nil {
			fmt.Println("  " + red("list error: "+err.Error()))
			return
		}
		if len(names) == 0 {
			fmt.Println("  (no saved sessions)")
			return
		}
		for _, n := range names {
			if n == a.sessionName {
				fmt.Printf("  %s %s\n", green("*"), n)
			} else {
				fmt.Printf("    %s\n", n)
			}
		}
	case "config":
		a.handleConfig(parts[1:])
	default:
		fmt.Printf("  unknown command: /%s\n", parts[0])
	}
}

// handleConfig processes /config commands to view or change session settings.
func (a *agent) handleConfig(args []string) {
	c := readGlobalCfg()
	if len(args) == 0 {
		model := a.effectiveModel()
		modelSrc := dim("(default)")
		if a.flagModel != "" {
			modelSrc = dim("(flag)")
		} else if a.config.Model != nil && *a.config.Model != "" {
			modelSrc = dim("(session)")
		} else if c != nil && c.Model != nil && *c.Model != "" {
			modelSrc = dim("(config file)")
		} else if os.Getenv("MA_MODEL") != "" {
			modelSrc = dim("(env)")
		}
		fmt.Printf("  model          : %s %s\n", model, modelSrc)
		fmt.Printf("  auto-edit      : %s %s\n", onOff(a.autoEdit()), sourceLabel(a.config.AutoEdit != nil, c != nil && c.AutoEdit != nil))
		fmt.Printf("  thinking       : %s %s\n", onOff(a.thinking()), sourceLabel(a.config.Thinking != nil, c != nil && c.Thinking != nil))
		fmt.Printf("  thinking-effort: %s %s\n", effortString(a.thinkingEffort()), sourceLabel(a.config.ThinkingEffort != nil, c != nil && c.ThinkingEffort != nil))
		return
	}
	switch args[0] {
	case "model":
		if len(args) < 2 {
			fmt.Println("  usage: /config model <model-id>")
			return
		}
		m := args[1]
		a.config.Model = &m
		a.sessionDirty = true
		fmt.Printf("  model: %s\n", m)
	case "auto-edit":
		v := !a.autoEdit()
		a.config.AutoEdit = &v
		a.sessionDirty = true
		fmt.Printf("  auto-edit: %s\n", onOff(v))
	case "thinking":
		v := !a.thinking()
		a.config.Thinking = &v
		a.sessionDirty = true
		fmt.Printf("  thinking: %s\n", onOff(v))
	case "thinking-effort":
		if len(args) < 2 {
			fmt.Println("  usage: /config thinking-effort <low|medium|high>")
			return
		}
		level := strings.ToLower(args[1])
		if level != "low" && level != "medium" && level != "high" {
			fmt.Printf("  unknown effort level %q (use low, medium, or high)\n", args[1])
			return
		}
		a.config.ThinkingEffort = &level
		a.sessionDirty = true
		fmt.Printf("  thinking-effort: %s\n", level)
	default:
		fmt.Printf("  unknown config key %q; try model, auto-edit, thinking, or thinking-effort\n", args[0])
	}
}
