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

	// activeMu guards the in-flight request table used for cancellation.
	// Keys are the canonical (whitespace-trimmed) JSON form of the request
	// id, so a number id `7` and the cancellation params' `"requestId": 7`
	// both normalize to `7` and find each other.
	activeMu sync.Mutex
	active   map[string]context.CancelFunc

	// writeMu serializes encoder writes so concurrent tool-call goroutines
	// and the main read loop don't interleave bytes on the wire.
	writeMu sync.Mutex
}

// NewServer constructs an empty server. The advertised name and version
// appear in the initialize response.
func NewServer(name, version string) *Server {
	return &Server{
		name:     name,
		version:  version,
		handlers: map[string]Handler{},
		active:   map[string]context.CancelFunc{},
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
//
// tools/call requests run in their own goroutine so the read loop stays
// responsive — a `notifications/cancelled` arriving mid-call can be observed
// and acted on immediately. Other methods are fast enough to dispatch inline.
// Serve waits for any in-flight tool calls to finish before returning so the
// caller doesn't leak goroutines.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "")
	// writeErr captures the first failure from enc.Encode. Once set, further
	// writes are skipped (the client is unreachable) and the read loop exits
	// rather than silently processing requests whose responses can't be
	// delivered. Guarded by writeMu, which already serializes encoder access.
	var writeErr error
	write := func(msg *Message) {
		if msg == nil {
			return
		}
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		if writeErr != nil {
			return
		}
		if err := enc.Encode(msg); err != nil {
			writeErr = err
		}
	}
	peekWriteErr := func() error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		return writeErr
	}
	var wg sync.WaitGroup
	for sc.Scan() {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		default:
		}
		if err := peekWriteErr(); err != nil {
			wg.Wait()
			return err
		}
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			// We can't reply with a real id; send a generic parse error.
			write(errorResponse(json.RawMessage("null"), CodeParseError, "parse error: "+err.Error()))
			continue
		}
		// Cancellable requests get their own goroutine. Registration must
		// happen *before* dispatch so a cancellation notification arriving on
		// the very next line still finds an entry to cancel.
		if msg.Method == "tools/call" && len(msg.ID) > 0 && string(msg.ID) != "null" {
			callCtx, cancel := context.WithCancel(ctx)
			s.registerActive(msg.ID, cancel)
			wg.Add(1)
			go func(m Message) {
				defer wg.Done()
				defer s.removeActive(m.ID)
				write(s.toolsCallResponse(callCtx, m.ID, m.Params))
			}(msg)
			continue
		}
		write(s.handle(ctx, msg))
	}
	wg.Wait()
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return peekWriteErr()
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

	case "notifications/cancelled", "$/cancelRequest":
		// Spec method is "notifications/cancelled"; "$/cancelRequest" is the
		// LSP-style alias some older clients still send.
		s.cancelByParams(msg.Params)
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
	b, err := json.Marshal(result)
	if err != nil {
		return errorResponse(id, CodeInternalError, "marshal initialize result: "+err.Error())
	}
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
	b, err := json.Marshal(body{Tools: tools})
	if err != nil {
		// Most realistic trigger: a registered Tool's InputSchema RawMessage
		// is not valid JSON, so its MarshalJSON rejects the bytes.
		return errorResponse(id, CodeInternalError, "marshal tools list: "+err.Error())
	}
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
	b, err := json.Marshal(res)
	if err != nil {
		return errorResponse(id, CodeInternalError, "marshal tool result: "+err.Error())
	}
	return &Message{JSONRPC: "2.0", ID: id, Result: b}
}

// registerActive records the cancel function for an in-flight request so a
// later notifications/cancelled can interrupt it.
func (s *Server) registerActive(id json.RawMessage, cancel context.CancelFunc) {
	key := idKey(id)
	if key == "" {
		cancel()
		return
	}
	s.activeMu.Lock()
	// If a client reuses an id while the prior request is still running,
	// cancel the stale one rather than silently leaking its cancel func.
	if prev, ok := s.active[key]; ok {
		prev()
	}
	s.active[key] = cancel
	s.activeMu.Unlock()
}

// removeActive clears the entry for a request that has finished on its own.
// Safe to call even if the entry is already gone (e.g. cancelByParams beat us).
func (s *Server) removeActive(id json.RawMessage) {
	key := idKey(id)
	if key == "" {
		return
	}
	s.activeMu.Lock()
	delete(s.active, key)
	s.activeMu.Unlock()
}

type cancelParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}

// cancelByParams looks up the active request named in a notifications/cancelled
// payload and invokes its context cancel. Unknown or already-finished ids are
// silently ignored — the spec treats cancellation as advisory.
func (s *Server) cancelByParams(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var p cancelParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	key := idKey(p.RequestID)
	if key == "" {
		return
	}
	s.activeMu.Lock()
	cancel, ok := s.active[key]
	if ok {
		delete(s.active, key)
	}
	s.activeMu.Unlock()
	if ok {
		cancel()
	}
}

// idKey normalizes a JSON-RPC id (number or quoted string) into a stable
// map key so the cancel notification's `requestId` matches the originating
// request's `id` regardless of encoding choices on either side.
func idKey(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return s
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
