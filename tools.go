package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/openai/openai-go"
)

func (a *agent) runBash(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Command          string `json:"command"`
		RequiresApproval bool   `json:"requires_approval"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Command == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"command": "..."}`, call.ID)
	}

	fmt.Println("\n  " + toolDot() + toolLabel("bash") + " $ " + args.Command)
	if args.RequiresApproval && !a.approve() {
		fmt.Println("  " + red("(denied)"))
		return openai.ToolMessage("error: the user denied permission to run this command", call.ID)
	}

	out, err := exec.Command("bash", "-c", args.Command).CombinedOutput()
	result := string(out)
	if err != nil {
		result += "\n[exit: " + err.Error() + "]"
	}
	if result == "" {
		result = "(no output)"
	}
	return openai.ToolMessage(result, call.ID)
}

func (a *agent) readFile(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "..."}`, call.ID)
	}

	fmt.Println("\n  " + toolDot() + toolLabel("read") + " " + args.Path)
	data, err := os.ReadFile(args.Path)
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	if len(data) == 0 {
		return openai.ToolMessage("(empty file)", call.ID)
	}
	return openai.ToolMessage(string(data), call.ID)
}

func (a *agent) writeFile(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "...", "content": "..."}`, call.ID)
	}

	fmt.Printf("\n  %s%s %s (%d bytes)\n", toolDot(), toolLabel("write"), args.Path, len(args.Content))
	if !a.autoEdit() && !a.approve() {
		fmt.Println("  " + red("(denied)"))
		return openai.ToolMessage("error: the user denied permission to write this file", call.ID)
	}

	if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	return openai.ToolMessage(fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), call.ID)
}

func (a *agent) editFile(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" || args.OldString == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "...", "old_string": "...", "new_string": "..."}`, call.ID)
	}

	data, err := os.ReadFile(args.Path)
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	content := string(data)

	switch strings.Count(content, args.OldString) {
	case 1:
	case 0:
		return openai.ToolMessage("error: old_string not found in "+args.Path+"; read the file and retry with text that matches exactly", call.ID)
	default:
		return openai.ToolMessage("error: old_string matches multiple times in "+args.Path+"; add surrounding context to make it unique", call.ID)
	}

	fmt.Println("\n  " + toolDot() + toolLabel("edit") + " " + args.Path)
	printDiff(content, args.OldString, args.NewString)
	if !a.autoEdit() && !a.approve() {
		fmt.Println("  " + red("(denied)"))
		return openai.ToolMessage("error: the user denied permission to edit this file", call.ID)
	}

	updated := strings.Replace(content, args.OldString, args.NewString, 1)
	if err := os.WriteFile(args.Path, []byte(updated), 0644); err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	return openai.ToolMessage("edited "+args.Path, call.ID)
}

func printDiff(content, oldString, newString string) {
	idx := strings.Index(content, oldString)
	if idx < 0 {
		return
	}
	before := content[:idx]
	after := content[idx+len(oldString):]

	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	beforeLines := split(before)
	afterLines := split(after)
	oldLines := split(oldString)
	newLines := split(newString)

	// 1-indexed line where old_string starts in the file.
	matchLine := len(beforeLines) + 1

	// Find common prefix: lines shared at the start of old and new.
	commonPrefix := 0
	for commonPrefix < len(oldLines) && commonPrefix < len(newLines) &&
		oldLines[commonPrefix] == newLines[commonPrefix] {
		commonPrefix++
	}

	// Find common suffix (after the prefix).
	commonSuffix := 0
	oi := len(oldLines) - 1
	ni := len(newLines) - 1
	for commonSuffix < len(oldLines)-commonPrefix && commonSuffix < len(newLines)-commonPrefix &&
		oldLines[oi] == newLines[ni] {
		commonSuffix++
		oi--
		ni--
	}

	ctxBefore := min(3, len(beforeLines))
	ctxAfter := min(3, len(afterLines))

	// Unified diff header.
	oldCount := ctxBefore + len(oldLines) + ctxAfter
	newCount := ctxBefore + len(newLines) + ctxAfter
	fmt.Printf("@@ -%d,%d +%d,%d @@\n", matchLine-ctxBefore, oldCount, matchLine-ctxBefore, newCount)

	pad := func(ln int, marker, line string) {
		switch marker {
		case " ":
			fmt.Printf("%4d  %s\n", ln, line)
		case "-":
			fmt.Printf("%4d  \033[31m-%s\033[0m\n", ln, line)
		default:
			fmt.Printf("      \033[32m+%s\033[0m\n", line)
		}
	}

	// Context before.
	ln := matchLine - ctxBefore
	for _, line := range beforeLines[len(beforeLines)-ctxBefore:] {
		pad(ln, " ", line)
		ln++
	}

	// Common prefix (unchanged context embedded in old/new).
	for i := 0; i < commonPrefix; i++ {
		pad(ln, " ", oldLines[i])
		ln++
	}

	// Removed old lines.
	oldChgEnd := ln
	for i := commonPrefix; i < len(oldLines)-commonSuffix; i++ {
		pad(ln, "-", oldLines[i])
		ln++
		oldChgEnd = ln
	}

	// Added new lines (no line numbers — they don't exist in the old file).
	for i := commonPrefix; i < len(newLines)-commonSuffix; i++ {
		pad(0, "+", newLines[i])
	}

	// Common suffix (unchanged context embedded in old/new).
	ln = oldChgEnd
	for i := len(oldLines) - commonSuffix; i < len(oldLines); i++ {
		pad(ln, " ", oldLines[i])
		ln++
	}

	// Context after.
	for i := 0; i < ctxAfter && i < len(afterLines); i++ {
		pad(ln, " ", afterLines[i])
		ln++
	}
}

func (a *agent) approve() bool {
	fmt.Print("  run this command? [y/N] ")
	if !a.in.Scan() {
		fmt.Println()
		return false
	}
	switch strings.ToLower(strings.TrimSpace(a.in.Text())) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
