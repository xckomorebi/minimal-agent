// A minimal, runnable agent: an OpenAI Chat Completions tool-calling loop with
// `bash`, `read`, `write`, and `edit` tools, built on the official openai-go SDK.
//
// Responses are streamed over SSE. Commands that change state require interactive
// approval: `write` and `edit` always prompt, and `bash` prompts when the model
// sets its `requires_approval` parameter. `read` never prompts.
//
// Configuration (flags override environment):
//
//	API key : MA_API_KEY  or  -ma-api-key
//	Base URL: MA_BASE_URL or  -url   (default https://api.openai.com/v1)
//	Model   : MA_MODEL or -model (default gpt-4o)
//
// Run:
//
//	export MA_API_KEY=sk-...
//	go run .            # then type a request, Ctrl-D / "exit" to quit
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// ---- agent ---------------------------------------------------------------

type agent struct {
	client  openai.Client
	model   string
	tools   []openai.ChatCompletionToolParam
	history []openai.ChatCompletionMessageParamUnion
	in      *bufio.Scanner // shared stdin, also used for approval prompts
}

// runTurn streams the model's response, printing text as it arrives, and
// resolves any tool calls — looping until a message has no tool calls.
func (a *agent) runTurn(ctx context.Context) error {
	for {
		stream := a.client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
			Model:    openai.ChatModel(a.model),
			Messages: a.history,
			Tools:    a.tools,
		})

		acc := openai.ChatCompletionAccumulator{}
		printed := false
		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)
			if len(chunk.Choices) == 0 {
				continue
			}
			if delta := chunk.Choices[0].Delta.Content; delta != "" {
				if !printed {
					fmt.Print("\nagent> ")
					printed = true
				}
				fmt.Print(delta)
			}
		}
		if printed {
			fmt.Println()
		}
		if err := stream.Err(); err != nil {
			return err
		}
		if len(acc.Choices) == 0 {
			return fmt.Errorf("empty response (no choices)")
		}

		msg := acc.Choices[0].Message
		a.history = append(a.history, msg.ToParam())

		if len(msg.ToolCalls) == 0 {
			return nil // turn complete
		}
		for _, call := range msg.ToolCalls {
			a.history = append(a.history, a.runTool(call))
		}
	}
}

// runTool dispatches a single tool call to its handler and returns a tool result.
func (a *agent) runTool(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	switch call.Function.Name {
	case "bash":
		return a.runBash(call)
	case "read":
		return a.readFile(call)
	case "write":
		return a.writeFile(call)
	case "edit":
		return a.editFile(call)
	default:
		return openai.ToolMessage("error: unknown tool: "+call.Function.Name, call.ID)
	}
}

// runBash runs a shell command, prompting for approval when the model flags it.
func (a *agent) runBash(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Command          string `json:"command"`
		RequiresApproval bool   `json:"requires_approval"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Command == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"command": "..."}`, call.ID)
	}

	fmt.Println("\n  $ " + args.Command)
	if args.RequiresApproval && !a.approve() {
		fmt.Println("  (denied)")
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

// readFile returns the contents of a file. Reading never requires approval.
func (a *agent) readFile(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "..."}`, call.ID)
	}

	fmt.Println("\n  read " + args.Path)
	data, err := os.ReadFile(args.Path)
	if err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	if len(data) == 0 {
		return openai.ToolMessage("(empty file)", call.ID)
	}
	return openai.ToolMessage(string(data), call.ID)
}

// writeFile writes (or overwrites) a file. Writing always requires approval.
func (a *agent) writeFile(call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Path == "" {
		return openai.ToolMessage(`error: invalid tool input; expected {"path": "...", "content": "..."}`, call.ID)
	}

	fmt.Printf("\n  write %s (%d bytes)\n", args.Path, len(args.Content))
	if !a.approve() {
		fmt.Println("  (denied)")
		return openai.ToolMessage("error: the user denied permission to write this file", call.ID)
	}

	if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	return openai.ToolMessage(fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), call.ID)
}

// editFile replaces an exact, unique occurrence of old_string with new_string in
// an existing file. If old_string is missing or matches more than once, it returns
// an error so the model can re-read and retry with more context. Always prompts.
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
	case 1: // the unique match we require
	case 0:
		return openai.ToolMessage("error: old_string not found in "+args.Path+"; read the file and retry with text that matches exactly", call.ID)
	default:
		return openai.ToolMessage("error: old_string matches multiple times in "+args.Path+"; add surrounding context to make it unique", call.ID)
	}

	fmt.Println("\n  edit " + args.Path)
	printDiff(args.OldString, args.NewString)
	if !a.approve() {
		fmt.Println("  (denied)")
		return openai.ToolMessage("error: the user denied permission to edit this file", call.ID)
	}

	updated := strings.Replace(content, args.OldString, args.NewString, 1)
	if err := os.WriteFile(args.Path, []byte(updated), 0644); err != nil {
		return openai.ToolMessage("error: "+err.Error(), call.ID)
	}
	return openai.ToolMessage("edited "+args.Path, call.ID)
}

// printDiff shows the removed and added lines of an edit for the approval prompt.
func printDiff(oldString, newString string) {
	for line := range strings.SplitSeq(oldString, "\n") {
		fmt.Println("  - " + line)
	}
	for line := range strings.SplitSeq(newString, "\n") {
		fmt.Println("  + " + line)
	}
}

// approve asks the user to confirm the pending command. Anything other than an
// explicit yes (including EOF) is treated as a denial.
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

// ---- main ----------------------------------------------------------------

func main() {
	apiKey := flag.String("ma-api-key", os.Getenv("MA_API_KEY"), "MA API key (or MA_API_KEY)")
	baseURL := flag.String("url", envOr("MA_BASE_URL", "https://api.openai.com/v1"), "API base URL (or MA_BASE_URL)")
	model := flag.String("model", envOr("MA_MODEL", "gpt-4o"), "model id")
	flag.Parse()

	if *apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: no API key; set MA_API_KEY or pass -ma-api-key")
		os.Exit(1)
	}

	// The SDK joins the request path onto the base URL, so keep a trailing slash
	// to preserve any path prefix (e.g. ".../maas/v1").
	url := strings.TrimRight(*baseURL, "/") + "/"

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	a := &agent{
		client: openai.NewClient(
			option.WithAPIKey(*apiKey),
			option.WithBaseURL(url),
		),
		model: *model,
		in:    scanner,
		tools: []openai.ChatCompletionToolParam{
			toolDef("bash", "Run a shell command with bash -c and return its combined stdout/stderr.",
				prop("command", "string", "the shell command to run"),
				prop("requires_approval", "boolean", "whether this command needs explicit user approval before running. Set true for anything destructive, irreversible, or state-changing (writes, deletes, moves, installs, network calls, git push, etc.); set false for read-only inspection (ls, cat, grep, git status, etc.)."),
			),
			toolDef("read", "Read and return the full contents of a file at the given path.",
				prop("path", "string", "path to the file to read"),
			),
			toolDef("write", "Write (creating or overwriting) a file with the given content. Use this for new files; use edit to modify an existing file. Always prompts the user for approval.",
				prop("path", "string", "path to the file to write"),
				prop("content", "string", "the full content to write to the file"),
			),
			toolDef("edit", "Modify an existing file by replacing an exact, unique occurrence of old_string with new_string. old_string must match the file byte-for-byte (including whitespace) and appear exactly once; include enough surrounding context to make it unique. Always prompts the user for approval.",
				prop("path", "string", "path to the file to edit"),
				prop("old_string", "string", "the exact existing text to replace; must be unique within the file"),
				prop("new_string", "string", "the replacement text"),
			),
		},
		history: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("You are a concise CLI coding agent. Use the bash, read, write, and edit tools to inspect and act on the system. Prefer edit over write when changing an existing file. Keep answers short."),
		},
	}

	fmt.Printf("minimal agent (model=%s, url=%s)\nType a request; Ctrl-D or \"exit\" to quit.\n", a.model, url)

	ctx := context.Background()
	for {
		fmt.Print("\nyou> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}

		a.history = append(a.history, openai.UserMessage(line))
		if err := a.runTurn(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "error: "+err.Error())
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "input error: "+err.Error())
	}
}

// property describes one tool input field: its JSON Schema plus whether it is required.
type property struct {
	name   string
	schema map[string]string
}

// prop builds a required tool-input property with the given type and description.
func prop(name, typ, description string) property {
	return property{name: name, schema: map[string]string{"type": typ, "description": description}}
}

// toolDef assembles a function tool from a name, description, and its properties
// (all of which are marked required).
func toolDef(name, description string, props ...property) openai.ChatCompletionToolParam {
	properties := map[string]any{}
	required := make([]string, 0, len(props))
	for _, p := range props {
		properties[p.name] = p.schema
		required = append(required, p.name)
	}
	return openai.ChatCompletionToolParam{
		Function: openai.FunctionDefinitionParam{
			Name:        name,
			Description: openai.String(description),
			Parameters: openai.FunctionParameters{
				"type":       "object",
				"properties": properties,
				"required":   required,
			},
		},
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
