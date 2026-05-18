// Package mcp implements a minimal MCP (Model Context Protocol) server over
// stdio. The wire is JSON-RPC 2.0 with line-delimited messages on stdin/
// stdout. Spec: https://spec.modelcontextprotocol.io.
//
// Scope of this package:
//   - initialize          (handshake, capabilities)
//   - notifications/initialized  (no-op ack)
//   - tools/list          (advertise registered tools)
//   - tools/call          (execute one)
//   - ping                (keep-alive)
//
// Resources and prompts are out of scope for v0.7.0 — they can land later
// without breaking the wire shape.
//
// The package is provider-agnostic: a Server is a registry of typed Tool
// handlers. Whoever wires the server (cmd/uta/serve.go) decides which
// uta-specific tools to expose.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// ProtocolVersion is what we advertise. MCP has bumped versions across the
// year — 2024-11-05 and 2025-03-26 are widely supported by clients today.
const ProtocolVersion = "2024-11-05"

// JSON-RPC 2.0 message envelope. Either id or method (or both) determines
// shape — request has id+method, notification has only method, response has
// id + (result|error).
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // null|number|string per JSON-RPC 2.0
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError mirrors the JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC 2.0 + MCP-specific codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// MCP-specific (advisory; clients tolerate plain JSON-RPC codes).
	CodeToolError = -32000
)

// Tool is one capability advertised to clients.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"` // JSON Schema draft-07; use json.RawMessage so the operator can paste it verbatim
}

// ToolResult is what a Handler returns. Content is the structured payload
// MCP clients render to the user / model; IsError flags execution failures
// distinct from protocol-level errors.
type ToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Content is one chunk inside a ToolResult. Only "text" type is implemented
// for v0.7.0; "image" / "resource" can be added without breaking callers.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextResult is the common case: a single text block.
func TextResult(s string) ToolResult {
	return ToolResult{Content: []Content{{Type: "text", Text: s}}}
}

// ErrorResult marks the tool as failed in a way the model can see and react
// to (distinct from a transport error).
func ErrorResult(s string) ToolResult {
	return ToolResult{Content: []Content{{Type: "text", Text: s}}, IsError: true}
}

// Handler is the function a registered Tool runs.
type Handler func(ctx context.Context, args json.RawMessage) ToolResult

// Server is a registry of tools served over a JSON-RPC stdio connection.
type Server struct {
	name    string
	version string

	mu          sync.RWMutex
	tools       []Tool
	handlers    map[string]Handler
	initialized bool
}

// NewServer constructs an empty server. The advertised name and version
// appear in the initialize response.
func NewServer(name, version string) *Server {
	return &Server{
		name:     name,
		version:  version,
		handlers: map[string]Handler{},
	}
}

// RegisterTool adds a tool to the registry. Re-registering the same name
// replaces the previous handler.
func (s *Server) RegisterTool(t Tool, h Handler) error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("tool name is required")
	}
	if h == nil {
		return errors.New("tool handler is nil")
	}
	if len(t.InputSchema) == 0 {
		// Provide a permissive default schema.
		t.InputSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Replace existing entry with the same name.
	replaced := false
	for i := range s.tools {
		if s.tools[i].Name == t.Name {
			s.tools[i] = t
			replaced = true
			break
		}
	}
	if !replaced {
		s.tools = append(s.tools, t)
	}
	s.handlers[t.Name] = h
	return nil
}

// ToolCount returns the number of currently registered tools.
func (s *Server) ToolCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tools)
}

// Serve reads JSON-RPC messages from r (one per line), dispatches them, and
// writes responses to w. Returns when r is closed, ctx is done, or an
// unrecoverable error occurs.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "")
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			// We can't reply with a real id; send a generic parse error.
			_ = enc.Encode(errorResponse(json.RawMessage("null"), CodeParseError, "parse error: "+err.Error()))
			continue
		}
		resp := s.handle(ctx, msg)
		if resp == nil {
			continue // notification — no response per JSON-RPC spec
		}
		_ = enc.Encode(resp)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// handle dispatches one incoming message. Returns the response to write, or
// nil for notifications.
func (s *Server) handle(ctx context.Context, msg Message) *Message {
	isNotification := len(msg.ID) == 0 || string(msg.ID) == "null"
	switch msg.Method {
	case "initialize":
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		return s.initializeResponse(msg.ID, msg.Params)

	case "notifications/initialized", "initialized":
		// Notification — no response.
		return nil

	case "ping":
		return &Message{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage(`{}`)}

	case "tools/list":
		return s.toolsListResponse(msg.ID)

	case "tools/call":
		return s.toolsCallResponse(ctx, msg.ID, msg.Params)

	case "shutdown":
		return &Message{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage(`{}`)}

	default:
		if isNotification {
			return nil
		}
		return errorResponse(msg.ID, CodeMethodNotFound, "method not found: "+msg.Method)
	}
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

func (s *Server) initializeResponse(id, paramsRaw json.RawMessage) *Message {
	// We don't strictly need the client params, but parsing them is good
	// hygiene + lets us echo back a sensible protocolVersion negotiation.
	var p initializeParams
	if len(paramsRaw) > 0 {
		_ = json.Unmarshal(paramsRaw, &p)
	}
	result := initializeResult{
		ProtocolVersion: ProtocolVersion,
		Capabilities: map[string]any{
			"tools": map[string]any{
				"listChanged": false,
			},
		},
	}
	result.ServerInfo.Name = s.name
	result.ServerInfo.Version = s.version
	b, _ := json.Marshal(result)
	return &Message{JSONRPC: "2.0", ID: id, Result: b}
}

func (s *Server) toolsListResponse(id json.RawMessage) *Message {
	s.mu.RLock()
	tools := make([]Tool, len(s.tools))
	copy(tools, s.tools)
	s.mu.RUnlock()
	type body struct {
		Tools []Tool `json:"tools"`
	}
	b, _ := json.Marshal(body{Tools: tools})
	return &Message{JSONRPC: "2.0", ID: id, Result: b}
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) toolsCallResponse(ctx context.Context, id, paramsRaw json.RawMessage) (resp *Message) {
	var p toolCallParams
	if err := json.Unmarshal(paramsRaw, &p); err != nil {
		return errorResponse(id, CodeInvalidParams, "tools/call: "+err.Error())
	}
	s.mu.RLock()
	handler, ok := s.handlers[p.Name]
	s.mu.RUnlock()
	if !ok {
		return errorResponse(id, CodeMethodNotFound, "unknown tool: "+p.Name)
	}
	// Tool-level failures are returned as ToolResult.IsError rather than
	// JSON-RPC errors, per MCP convention. A panicking handler shouldn't
	// kill the server — and must not silently return nil either, since
	// handle's caller treats a nil response as a notification and writes
	// nothing, leaving the client hanging forever on its request id.
	defer func() {
		if r := recover(); r != nil {
			resp = errorResponse(id, CodeInternalError, fmt.Sprintf("tool panicked: %v", r))
		}
	}()
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	res := handler(ctx, args)
	b, _ := json.Marshal(res)
	return &Message{JSONRPC: "2.0", ID: id, Result: b}
}

func errorResponse(id json.RawMessage, code int, message string) *Message {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &Message{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: message},
	}
}

// ArgError helps tool handlers report "you sent garbage" without losing the
// distinction from "uta couldn't reach claude."
func ArgError(format string, a ...any) ToolResult {
	return ErrorResult(fmt.Sprintf("invalid arguments: "+format, a...))
}
