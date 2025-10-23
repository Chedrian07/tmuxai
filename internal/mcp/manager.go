package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/alvinunreal/tmuxai/logger"
)

// Options configures the MCP manager.
type Options struct {
	ClientName    string
	ClientVersion string
}

// Manager coordinates MCP servers and exposes helper functionality for the rest of TmuxAI.
type Manager struct {
	configPath string

	clientName    string
	clientVersion string

	mu      sync.RWMutex
	servers map[string]*Server
	tools   map[string]Tool

	toolPrompt string
}

// NewManager loads the MCP configuration and starts all enabled servers.
// Returns nil, nil when no configuration is found.
func NewManager(ctx context.Context, opts Options) (*Manager, error) {
	cfg, path, err := LoadConfig()
	if err != nil {
		if errors.Is(err, ErrConfigNotFound) {
			return nil, nil
		}
		return nil, err
	}

	if cfg == nil || len(cfg.Servers) == 0 {
		return nil, nil
	}

	if opts.ClientName == "" {
		opts.ClientName = "tmuxai"
	}
	if opts.ClientVersion == "" {
		opts.ClientVersion = "dev"
	}

	m := &Manager{
		configPath:    path,
		clientName:    opts.ClientName,
		clientVersion: opts.ClientVersion,
		servers:       make(map[string]*Server),
		tools:         make(map[string]Tool),
	}

	var successful int

	for name, serverCfg := range cfg.Servers {
		if serverCfg.Disabled {
			logger.Info("[MCP] Skipping disabled server %s", name)
			continue
		}

		server, err := NewServer(ctx, name, serverCfg, m.clientName, m.clientVersion)
		if err != nil {
			logger.Error("[MCP] Failed to start server %s: %v", name, err)
			continue
		}

		m.servers[name] = server
		for _, tool := range server.Tools {
			m.tools[tool.ServerName+"::"+tool.Name] = tool
		}
		successful++
		logger.Info("[MCP] Connected to server %s with %d tools", name, len(server.Tools))
	}

	if successful == 0 {
		return nil, fmt.Errorf("failed to start any MCP servers from %s", path)
	}

	return m, nil
}

// Close stops all managed servers.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, srv := range m.servers {
		logger.Debug("[MCP] Stopping server %s", name)
		srv.Close()
	}
}

// HasTools returns true when at least one MCP tool is available.
func (m *Manager) HasTools() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tools) > 0
}

// InstructionPrompt returns text that should be injected into the system prompt.
func (m *Manager) InstructionPrompt() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.toolPrompt != "" {
		return m.toolPrompt
	}

	var b strings.Builder
	b.WriteString("You can access Model Context Protocol (MCP) tools provided by the user environment.\n")
	b.WriteString("When you need to call one, respond with ONLY a single <MCPToolCall> XML block containing JSON like this:\n")
	b.WriteString("<MCPToolCall>{\"server\":\"SERVER_NAME\",\"tool\":\"TOOL_NAME\",\"arguments\":{...}}</MCPToolCall>\n")
	b.WriteString("Do not add any other text when requesting a tool call. After the tool runs, you will receive a new user message beginning with \"[MCP RESULT]\" that contains the outcome. Use it to continue your plan.\n")

	seen := make(map[string]struct{})
	for name, srv := range m.servers {
		instructions := strings.TrimSpace(srv.Instructions)
		if instructions == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		fmt.Fprintf(&b, "Server %s guidance:\n%s\n", name, instructions)
		seen[name] = struct{}{}
	}

	b.WriteString("Available MCP tools:\n")

	const maxTools = 80
	count := 0
	for _, tool := range m.tools {
		if count >= maxTools {
			b.WriteString("… (additional tools omitted)\n")
			break
		}

		desc := strings.TrimSpace(tool.Description)
		if desc == "" {
			desc = "No description provided."
		}
		if len(desc) > 260 {
			desc = desc[:257] + "…"
		}
		fmt.Fprintf(&b, "- %s::%s — %s\n", tool.ServerName, tool.Name, desc)
		count++
	}

	m.toolPrompt = b.String()
	return m.toolPrompt
}

// CallTool executes the named tool on the given server.
func (m *Manager) CallTool(ctx context.Context, serverName, toolName string, args map[string]interface{}) (*ToolResult, error) {
	m.mu.RLock()
	server, ok := m.servers[serverName]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown MCP server: %s", serverName)
	}

	if args == nil {
		args = map[string]interface{}{}
	}

	return server.CallTool(ctx, toolName, args)
}

// ToolDisplayName returns a human friendly name that includes the server prefix.
func (m *Manager) ToolDisplayName(serverName, toolName string) string {
	return fmt.Sprintf("%s::%s", serverName, toolName)
}

// ToolExists checks if the specified tool exists.
func (m *Manager) ToolExists(serverName, toolName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.tools[serverName+"::"+toolName]
	return ok
}

// FormatResultForModel produces the message content returned to the LLM after a tool call.
func (m *Manager) FormatResultForModel(serverName, toolName string, result *ToolResult, callErr error) string {
	var b strings.Builder
	b.WriteString("[MCP RESULT]\n")
	b.WriteString(fmt.Sprintf("server: %s\n", serverName))
	b.WriteString(fmt.Sprintf("tool: %s\n", toolName))

	if callErr != nil {
		b.WriteString("status: error\n")
		b.WriteString(fmt.Sprintf("error: %s\n", callErr.Error()))
		return b.String()
	}

	status := "success"
	if result != nil && result.IsError {
		status = "tool_reported_error"
	}
	b.WriteString(fmt.Sprintf("status: %s\n", status))

	if result == nil {
		b.WriteString("output: (no content)\n")
		return b.String()
	}

	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "(no textual output)"
	}
	if len(text) > 4000 {
		text = text[:4000] + "\n… (truncated)"
	}
	b.WriteString("output:\n")
	b.WriteString(text)
	b.WriteString("\n")

	if len(result.StructuredContent) > 0 {
		jsonBytes, err := json.MarshalIndent(result.StructuredContent, "", "  ")
		if err == nil {
			if len(jsonBytes) > 4000 {
				jsonBytes = jsonBytes[:4000]
			}
			b.WriteString("structured_output:\n")
			b.Write(jsonBytes)
			b.WriteString("\n")
		}
	}

	return strings.TrimSpace(b.String())
}
