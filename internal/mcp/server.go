package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alvinunreal/tmuxai/logger"
)

const (
	defaultProtocolVersion = "2025-03-26"
	defaultRequestTimeout  = 60 * time.Second
)

// Tool describes a single tool provided by an MCP server.
type Tool struct {
	Name          string
	Description   string
	InputSchema   json.RawMessage
	OutputSchema  json.RawMessage
	Annotations   map[string]any
	ServerName    string
	ServerDisplay string
}

// ToolResult represents the outcome of executing a tool.
type ToolResult struct {
	Text              string
	StructuredContent map[string]any
	IsError           bool
	RawContent        []ToolContent
}

// ToolContent mirrors the MCP tool content block structure.
type ToolContent struct {
	Type string          `json:"type"`
	Text string          `json:"text,omitempty"`
	Any  json.RawMessage `json:"-"`
}

func (c *ToolContent) UnmarshalJSON(data []byte) error {
	type alias ToolContent
	aux := &struct {
		*alias
	}{
		alias: (*alias)(c),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	c.Any = append([]byte(nil), data...)
	return nil
}

type listToolsResult struct {
	Tools []toolPayload `json:"tools"`
}

type toolPayload struct {
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	InputSchema   json.RawMessage        `json:"inputSchema"`
	InputSchema2  json.RawMessage        `json:"input_schema"`
	OutputSchema  json.RawMessage        `json:"outputSchema"`
	OutputSchema2 json.RawMessage        `json:"output_schema"`
	Annotations   map[string]interface{} `json:"annotations"`
}

type callToolResult struct {
	Content           []ToolContent          `json:"content"`
	StructuredContent map[string]any         `json:"structuredContent"`
	IsError           bool                   `json:"isError"`
	ToolResultLegacy  map[string]interface{} `json:"toolResult"` // backwards compatibility
}

type Server struct {
	name        string
	cfg         ServerConfig
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	reader      *bufio.Reader
	stderr      io.ReadCloser
	pending     map[string]chan jsonrpcMessage
	pendingLock sync.Mutex
	nextID      int64
	closed      chan struct{}
	once        sync.Once

	Instructions string
	Tools        []Tool
}

func NewServer(ctx context.Context, name string, cfg ServerConfig, clientName, clientVersion string) (*Server, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("mcp server %s has empty command", name)
	}

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	cmd.Env = os.Environ()
	for key, value := range cfg.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", key, value))
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe for %s: %w", name, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe for %s: %w", name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe for %s: %w", name, err)
	}

	server := &Server{
		name:    name,
		cfg:     cfg,
		cmd:     cmd,
		stdin:   stdin,
		reader:  bufio.NewReader(stdout),
		stderr:  stderr,
		pending: make(map[string]chan jsonrpcMessage),
		closed:  make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start MCP server %s: %w", name, err)
	}

	go server.consumeStdErr()
	go server.listen()

	if err := server.initialize(ctx, clientName, clientVersion); err != nil {
		server.Close()
		return nil, err
	}

	return server, nil
}

func (s *Server) consumeStdErr() {
	scanner := bufio.NewScanner(s.stderr)
	for scanner.Scan() {
		logger.Debug("[MCP:%s STDERR] %s", s.name, scanner.Text())
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		logger.Error("[MCP:%s] error reading stderr: %v", s.name, err)
	}
}

func (s *Server) listen() {
	for {
		frame, err := readFrame(s.reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logger.Error("[MCP:%s] read error: %v", s.name, err)
			}
			s.closePendingWithError(err)
			close(s.closed)
			return
		}

		var msg jsonrpcMessage
		if err := json.Unmarshal(frame, &msg); err != nil {
			logger.Error("[MCP:%s] failed to decode message: %v", s.name, err)
			continue
		}

		if msg.ID != nil {
			idStr, err := idToString(msg.ID)
			if err != nil {
				logger.Error("[MCP:%s] invalid response id: %v", s.name, err)
				continue
			}
			s.pendingLock.Lock()
			respCh, ok := s.pending[idStr]
			if ok {
				delete(s.pending, idStr)
			}
			s.pendingLock.Unlock()
			if ok {
				respCh <- msg
			} else {
				logger.Debug("[MCP:%s] received response for unknown id %s", s.name, idStr)
			}
			continue
		}

		if msg.Method != "" {
			s.handleNotification(msg)
		}
	}
}

func (s *Server) initialize(ctx context.Context, clientName, clientVersion string) error {
	params := map[string]interface{}{
		"protocolVersion": defaultProtocolVersion,
		"capabilities": map[string]interface{}{
			"tools":       map[string]interface{}{},
			"resources":   map[string]interface{}{},
			"prompts":     map[string]interface{}{},
			"logging":     map[string]interface{}{},
			"sampling":    map[string]interface{}{},
			"elicitation": map[string]interface{}{},
		},
		"clientInfo": map[string]interface{}{
			"name":    clientName,
			"version": clientVersion,
		},
	}

	var initResp struct {
		ProtocolVersion string                 `json:"protocolVersion"`
		Instructions    string                 `json:"instructions"`
		Capabilities    map[string]interface{} `json:"capabilities"`
		ServerInfo      map[string]interface{} `json:"serverInfo"`
	}

	if err := s.call(ctx, "initialize", params, &initResp); err != nil {
		return fmt.Errorf("initialize failed for MCP server %s: %w", s.name, err)
	}

	s.Instructions = strings.TrimSpace(initResp.Instructions)

	// Send initialized notification
	if err := s.notify("notifications/initialized", map[string]interface{}{}); err != nil {
		logger.Error("[MCP:%s] failed to send initialized notification: %v", s.name, err)
	}

	// List tools
	var tools listToolsResult
	if err := s.call(ctx, "tools/list", map[string]interface{}{}, &tools); err != nil {
		return fmt.Errorf("tools/list failed for MCP server %s: %w", s.name, err)
	}

	for _, payload := range tools.Tools {
		input := payload.InputSchema
		if len(input) == 0 {
			input = payload.InputSchema2
		}
		output := payload.OutputSchema
		if len(output) == 0 {
			output = payload.OutputSchema2
		}

		s.Tools = append(s.Tools, Tool{
			Name:          payload.Name,
			Description:   payload.Description,
			InputSchema:   input,
			OutputSchema:  output,
			Annotations:   payload.Annotations,
			ServerName:    s.name,
			ServerDisplay: displayName(payload.Name, s.name),
		})
	}

	return nil
}

func (s *Server) handleNotification(msg jsonrpcMessage) {
	switch msg.Method {
	case "notifications/logMessage":
		var payload struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(msg.Params, &payload); err == nil {
			logger.Debug("[MCP:%s LOG][%s] %s", s.name, payload.Level, payload.Message)
		}
	default:
		logger.Debug("[MCP:%s] ignoring notification %s", s.name, msg.Method)
	}
}

func (s *Server) call(ctx context.Context, method string, params interface{}, result interface{}) error {
	id := atomic.AddInt64(&s.nextID, 1)
	payload, err := marshalRequest(id, method, params)
	if err != nil {
		return err
	}

	respCh := make(chan jsonrpcMessage, 1)

	s.pendingLock.Lock()
	select {
	case <-s.closed:
		s.pendingLock.Unlock()
		return errors.New("server not running")
	default:
	}
	s.pending[strconv.FormatInt(id, 10)] = respCh
	s.pendingLock.Unlock()

	if err := writeFrame(s.stdin, payload); err != nil {
		s.removePending(strconv.FormatInt(id, 10))
		return err
	}

	timeout := defaultRequestTimeout
	if s.cfg.Timeout > 0 {
		timeout = time.Duration(s.cfg.Timeout) * time.Second
	}

	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}

	select {
	case <-ctx.Done():
		s.removePending(strconv.FormatInt(id, 10))
		return ctx.Err()
	case <-timer:
		s.removePending(strconv.FormatInt(id, 10))
		return fmt.Errorf("request %s timed out", method)
	case msg := <-respCh:
		if msg.Error != nil {
			return fmt.Errorf("mcp error %d: %s", msg.Error.Code, msg.Error.Message)
		}
		if result == nil {
			return nil
		}
		if len(msg.Result) == 0 {
			return fmt.Errorf("empty result for method %s", method)
		}
		if err := json.Unmarshal(msg.Result, result); err != nil {
			return fmt.Errorf("failed to decode result for method %s: %w", method, err)
		}
		return nil
	}
}

func (s *Server) notify(method string, params interface{}) error {
	payload, err := marshalNotification(method, params)
	if err != nil {
		return err
	}
	return writeFrame(s.stdin, payload)
}

func (s *Server) removePending(id string) {
	s.pendingLock.Lock()
	delete(s.pending, id)
	s.pendingLock.Unlock()
}

func (s *Server) closePendingWithError(err error) {
	s.pendingLock.Lock()
	defer s.pendingLock.Unlock()
	for id, ch := range s.pending {
		delete(s.pending, id)
		ch <- jsonrpcMessage{
			Error: &jsonrpcError{
				Code:    -1,
				Message: err.Error(),
			},
		}
	}
}

func (s *Server) CallTool(ctx context.Context, name string, args map[string]interface{}) (*ToolResult, error) {
	params := map[string]interface{}{
		"name":      name,
		"arguments": args,
	}

	var result callToolResult
	if err := s.call(ctx, "tools/call", params, &result); err != nil {
		return nil, err
	}

	// Backwards compatibility: some servers return toolResult with top level string.
	if result.Content == nil && len(result.ToolResultLegacy) > 0 {
		if text, ok := result.ToolResultLegacy["output"].(string); ok {
			return &ToolResult{
				Text:    text,
				IsError: false,
			}, nil
		}
	}

	text := strings.Builder{}
	for _, block := range result.Content {
		switch block.Type {
		case "text", "output_text":
			text.WriteString(block.Text)
			if !strings.HasSuffix(block.Text, "\n") {
				text.WriteString("\n")
			}
		default:
			if len(block.Any) > 0 {
				text.WriteString(string(block.Any))
				text.WriteString("\n")
			}
		}
	}

	return &ToolResult{
		Text:              strings.TrimSpace(text.String()),
		StructuredContent: result.StructuredContent,
		IsError:           result.IsError,
		RawContent:        result.Content,
	}, nil
}

func (s *Server) Close() {
	s.once.Do(func() {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
}

func displayName(toolName, serverName string) string {
	if serverName == "" {
		return toolName
	}
	return fmt.Sprintf("%s::%s", serverName, toolName)
}
