package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go"
)

// connectedMCPServer holds a connected MCP session and its discovered tools
// converted to OpenAI-compatible tool definitions.
type connectedMCPServer struct {
	config  mcpServerConfig
	session *mcp.ClientSession
	tools   []openai.ChatCompletionToolParam
}

// activeMCPServers is the list of successfully connected MCP servers.
// Populated at startup by connectMCPServers.
var activeMCPServers []*connectedMCPServer

// connectMCPServers connects to all configured MCP servers and discovers their tools.
// Tool definitions are added to externalTools so they appear in allTools().
// Failures are silent — a failing server doesn't prevent startup.
func connectMCPServers(ctx context.Context, configs []mcpServerConfig) {
	if len(configs) == 0 {
		return
	}

	for _, cfg := range configs {
		cs, err := connectOneMCPServer(ctx, cfg)
		if err != nil {
			slog.Debug("MCP server connection failed", "server", cfg.Name, "error", err)
			continue
		}
		slog.Debug("MCP server connected", "server", cfg.Name, "tools", len(cs.tools))
		activeMCPServers = append(activeMCPServers, cs)

		for _, t := range cs.tools {
			externalTools = append(externalTools, t)
		}
	}
}

// connectOneMCPServer connects to a single MCP server and discovers its tools.
func connectOneMCPServer(ctx context.Context, cfg mcpServerConfig) (*connectedMCPServer, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "minimal-agent", Version: Version}, nil)

	transport, err := mcpTransport(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create transport: %w", err)
	}

	// Connect with a timeout — some servers take a moment to init.
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	// Discover tools.
	toolsResult, err := session.ListTools(connectCtx, &mcp.ListToolsParams{})
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("list tools: %w", err)
	}

	cs := &connectedMCPServer{
		config:  cfg,
		session: session,
		tools:   convertMCPTools(cfg.Name, toolsResult.Tools),
	}

	return cs, nil
}

// mcpTransport creates the appropriate transport for the server config.
func mcpTransport(ctx context.Context, cfg mcpServerConfig) (mcp.Transport, error) {
	if cfg.URL != "" {
		// Streamable HTTP transport (online MCP server).
		return &mcp.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Timeout: 30 * time.Second},
		}, nil
	}

	if cfg.Command != "" {
		// Stdio transport (subprocess MCP server).
		cmd := exec.Command(cfg.Command, cfg.Args...)
		if cfg.Env != nil {
			cmd.Env = os.Environ()
			for k, v := range cfg.Env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		return &mcp.CommandTransport{Command: cmd}, nil
	}

	return nil, fmt.Errorf("server %q: either url or command must be set", cfg.Name)
}

// closeMCPServers closes all MCP server sessions.
func closeMCPServers() {
	for _, cs := range activeMCPServers {
		cs.session.Close()
	}
}

// convertMCPTools converts a list of MCP Tool objects to OpenAI tool definitions.
// Tool names are prefixed with "mcp__<serverName>__" to avoid collisions with
// built-in tools and between MCP servers. Double underscores are used as the
// separator because OpenAI function names must match ^[a-zA-Z0-9_-]+$ (dots are
// rejected); this follows the conventional MCP tool-naming scheme.
func convertMCPTools(serverName string, tools []*mcp.Tool) []openai.ChatCompletionToolParam {
	var out []openai.ChatCompletionToolParam
	for _, t := range tools {
		name := "mcp__" + serverName + "__" + t.Name
		desc := t.Description
		if desc == "" {
			desc = fmt.Sprintf("MCP tool %s from server %s", t.Name, serverName)
		}
		// Annotate description so the model knows where this tool comes from.
		desc = fmt.Sprintf("[MCP:%s] %s", serverName, desc)

		// Build the OpenAI function parameters from the tool's InputSchema.
		params := mcpSchemaToOpenAIParams(t.InputSchema)

		out = append(out, openai.ChatCompletionToolParam{
			Function: openai.FunctionDefinitionParam{
				Name:        name,
				Description: openai.String(desc),
				Parameters:  params,
			},
		})
	}
	return out
}

// mcpSchemaToOpenAIParams converts an MCP tool's InputSchema (JSON Schema)
// to the OpenAI function parameters format. Returns a default object schema
// if conversion isn't possible.
func mcpSchemaToOpenAIParams(schema any) openai.FunctionParameters {
	if schema == nil {
		return openai.FunctionParameters{
			"type":       "object",
			"properties": map[string]any{},
		}
	}

	sm, ok := schema.(map[string]any)
	if !ok {
		return openai.FunctionParameters{
			"type":       "object",
			"properties": map[string]any{},
		}
	}

	// The MCP spec says InputSchema is a JSON Schema object. OpenAI expects
	// {"type": "object", "properties": {...}, "required": [...]}.
	// We can pass the schema through directly — it should already have the
	// right shape. But ensure "type" is "object" at minimum.
	if _, hasType := sm["type"]; !hasType {
		sm["type"] = "object"
	}

	return openai.FunctionParameters(sm)
}

// findMCPTool locates the connected server and raw tool name for a prefixed
// tool name like "mcp__filesystem__read_file". Returns nil if not found.
func findMCPTool(prefixedName string) (*connectedMCPServer, string) {
	// Prefix format: "mcp__<server>__<tool>"
	rest, ok := strings.CutPrefix(prefixedName, "mcp__")
	if !ok {
		return nil, ""
	}
	// Split into server name and tool name (tool may contain "__").
	serverName, toolName, ok := strings.Cut(rest, "__")
	if !ok {
		return nil, ""
	}

	for _, cs := range activeMCPServers {
		if cs.config.Name == serverName {
			return cs, toolName
		}
	}
	return nil, ""
}

// parseMCPToolName extracts the server name and tool name from a prefixed
// MCP tool name like "mcp__filesystem__read_file". Returns empty strings if
// the name does not follow the MCP prefix convention.
func parseMCPToolName(prefixedName string) (server, tool string) {
	rest, ok := strings.CutPrefix(prefixedName, "mcp__")
	if !ok {
		return "", ""
	}
	server, tool, ok = strings.Cut(rest, "__")
	if !ok {
		return "", ""
	}
	return server, tool
}

// runMCPTool executes an MCP tool call and returns the result.
func (a *agent) runMCPTool(ctx context.Context, call openai.ChatCompletionMessageToolCall) openai.ChatCompletionMessageParamUnion {
	cs, toolName := findMCPTool(call.Function.Name)
	if cs == nil {
		return openai.ToolMessage("error: MCP tool not found: "+call.Function.Name, call.ID)
	}

	slog.Debug("MCP tool call", "server", cs.config.Name, "tool", toolName)

	// Parse arguments from the JSON string.
	var args any
	if call.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return openai.ToolMessage("error: invalid arguments: "+err.Error(), call.ID)
		}
	}

	result, err := cs.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return openai.ToolMessage("error: MCP tool call failed: "+err.Error(), call.ID)
	}

	// Extract text from the result content.
	var parts []string
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}

	text := strings.Join(parts, "\n")
	if text == "" {
		if result.IsError {
			text = "MCP tool returned an error with no text content"
		} else {
			text = "(no output)"
		}
	}
	return openai.ToolMessage(text, call.ID)
}

// mcpServerSummary returns a one-line summary of connected MCP servers for the
// system prompt, or empty string if none.
func mcpServerSummary() string {
	if len(activeMCPServers) == 0 {
		return ""
	}
	var parts []string
	for _, cs := range activeMCPServers {
		parts = append(parts, fmt.Sprintf("%s (%d tools)", cs.config.Name, len(cs.tools)))
	}
	return fmt.Sprintf("Connected MCP servers: %s.", strings.Join(parts, ", "))
}
