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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ClientInfo is what the client advertises in the initialize handshake.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// DefaultClientInfo is what NewStdioClient / NewClient advertise unless
// the caller overrides via Client.ClientInfo before Initialize.
var DefaultClientInfo = ClientInfo{Name: "uta", Version: "0.1.0"}

// Client speaks JSON-RPC 2.0 to an MCP server over a duplex byte stream.
// Two construction shapes exist:
//
//   - NewStdioClient launches a subprocess and pipes stdin/stdout (the
//     production case — `npx`, `uvx`, a downloaded binary, etc.).
//   - NewClient wraps a pre-built reader/writer pair, used by tests that
//     wire an in-process Server through io.Pipe pairs.
//
// All requests are correlated by a monotonically-increasing integer id;
// responses are routed back to the originating caller by a background
// read loop. Notifications (id-less messages) are dropped — this client
// is a pure caller for now, no progress / log subscriptions.
type Client struct {
	ClientInfo ClientInfo

	reader  io.ReadCloser
	writer  io.WriteCloser
	scanner *bufio.Scanner
	enc     *json.Encoder

	// cmd is set only when this client owns a subprocess we must shut down.
	cmd *exec.Cmd

	// mu protects state — pending map, closed flag, nextID, serverInfo.
	// Critical sections are O(map-op); the lock is never held across I/O.
	mu       sync.Mutex
	nextID   int64
	pending  map[int64]chan *Message
	closed   bool
	readErr  error
	readDone chan struct{}

	// writeMu serializes access to the encoder. It is held during the
	// blocking write to the transport. Keeping it separate from mu means
	// readLoop can always dispatch responses even while another goroutine
	// is stalled mid-write — without this split, concurrent CallTool
	// deadlocks: writer holds mu → reader can't dispatch → server can't
	// drain its response pipe → server can't read next request → writer
	// can't finish → forever.
	writeMu sync.Mutex

	serverInfo struct {
		name    string
		version string
	}
}

// NewClient wraps an existing read/write pair. The caller is responsible
// for the lifetime of the underlying streams; Close() will close both.
// Use this when injecting an in-process Server via io.Pipe (tests) or any
// other duplex transport.
func NewClient(r io.ReadCloser, w io.WriteCloser) *Client {
	c := &Client{
		ClientInfo: DefaultClientInfo,
		reader:     r,
		writer:     w,
		scanner:    bufio.NewScanner(r),
		enc:        json.NewEncoder(w),
		pending:    map[int64]chan *Message{},
		readDone:   make(chan struct{}),
	}
	c.scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	go c.readLoop()
	return c
}

// NewStdioClient launches the given command and returns a Client connected
// to its stdin/stdout. Stderr is forwarded to os.Stderr so server-side
// diagnostics (e.g. an OAuth flow message from the Gmail server) surface
// to the operator instead of being silently swallowed.
//
// Returns an error if the process fails to start. A successful return
// does NOT imply the server has completed its handshake — call
// Initialize before using ListTools / CallTool.
func NewStdioClient(ctx context.Context, command string, args []string, env map[string]string) (*Client, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("mcp client: command is empty")
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), envMapToSlice(env)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp client stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp client stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp client start %q: %w", command, err)
	}
	c := &Client{
		ClientInfo: DefaultClientInfo,
		reader:     stdout,
		writer:     stdin,
		scanner:    bufio.NewScanner(stdout),
		enc:        json.NewEncoder(stdin),
		cmd:        cmd,
		pending:    map[int64]chan *Message{},
		readDone:   make(chan struct{}),
	}
	c.scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	go c.readLoop()
	return c, nil
}

// readLoop pumps JSON-RPC responses off the reader and dispatches each
// reply to whichever pending caller is keyed by its id. When the reader
// closes (EOF, broken pipe, parent context cancellation), all pending
// callers receive a closed channel so they unblock with an error rather
// than hanging on a dead transport.
func (c *Client) readLoop() {
	defer close(c.readDone)
	for c.scanner.Scan() {
		line := c.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		// Notifications (no id) are dropped — this client is a pure caller.
		if len(m.ID) == 0 || string(m.ID) == "null" {
			continue
		}
		// IDs we sent are always integers; tolerate JSON-encoded variants.
		id, ok := parseID(m.ID)
		if !ok {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[id]
		if ok {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if ok {
			ch <- &m
		}
	}
	if err := c.scanner.Err(); err != nil {
		c.mu.Lock()
		c.readErr = err
		c.mu.Unlock()
	}
	c.failPending(errors.New("mcp client: connection closed"))
}

func (c *Client) failPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[int64]chan *Message{}
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- &Message{Error: &RPCError{Code: CodeInternalError, Message: err.Error()}}:
		default:
		}
		close(ch)
	}
}

// parseID accepts either a bare JSON number or a quoted string-form integer.
// Servers should echo back the same shape we sent (int), but tolerate both.
func parseID(raw json.RawMessage) (int64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, false
	}
	if s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// call sends one request and blocks until the matching response arrives
// (or ctx is cancelled, or the connection closes).
func (c *Client) call(ctx context.Context, method string, params any) (*Message, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("mcp client: closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	idJSON, _ := json.Marshal(id)
	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			c.dropPending(id)
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		paramsRaw = b
	}
	req := Message{JSONRPC: "2.0", ID: idJSON, Method: method, Params: paramsRaw}

	// Encode under writeMu (NOT mu) so readLoop can keep dispatching even
	// if this write blocks on a slow/full pipe. Run it in a goroutine so a
	// genuinely stuck transport doesn't outlive ctx.
	encDone := make(chan error, 1)
	go func() {
		c.writeMu.Lock()
		err := c.enc.Encode(&req)
		c.writeMu.Unlock()
		encDone <- err
	}()
	select {
	case encErr := <-encDone:
		if encErr != nil {
			c.dropPending(id)
			return nil, fmt.Errorf("mcp client: write %s: %w", method, encErr)
		}
	case <-ctx.Done():
		c.dropPending(id)
		return nil, ctx.Err()
	}

	select {
	case <-ctx.Done():
		c.dropPending(id)
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok || resp == nil {
			return nil, errors.New("mcp client: connection closed before reply")
		}
		return resp, nil
	}
}

func (c *Client) dropPending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// notify sends a JSON-RPC notification (no id, no response). Used for
// the post-initialize "notifications/initialized" message the server
// expects before it considers itself ready.
func (c *Client) notify(method string, params any) error {
	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		paramsRaw = b
	}
	msg := Message{JSONRPC: "2.0", Method: method, Params: paramsRaw}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.enc.Encode(&msg)
}

// Initialize performs the MCP handshake. Must be called once before any
// other request method. The returned error wraps the server's RPC error
// when the handshake itself failed; transport errors propagate as-is.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      c.ClientInfo,
	}
	resp, err := c.call(ctx, "initialize", params)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("mcp initialize: %s (code=%d)", resp.Error.Message, resp.Error.Code)
	}
	var r initializeResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return fmt.Errorf("mcp initialize result: %w", err)
	}
	c.mu.Lock()
	c.serverInfo.name = r.ServerInfo.Name
	c.serverInfo.version = r.ServerInfo.Version
	c.mu.Unlock()
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		return fmt.Errorf("mcp initialized notification: %w", err)
	}
	return nil
}

// ServerInfo returns the (name, version) advertised by the server in the
// initialize response. Empty strings before Initialize has run.
func (c *Client) ServerInfo() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInfo.name, c.serverInfo.version
}

// ListTools queries the server's tool registry. Returns the tools in the
// order advertised by the server.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	resp, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp tools/list: %s (code=%d)", resp.Error.Message, resp.Error.Code)
	}
	var body struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		return nil, fmt.Errorf("mcp tools/list result: %w", err)
	}
	return body.Tools, nil
}

// CallTool invokes one tool. args is forwarded verbatim — callers may
// pass nil for the no-arg case, in which case the server sees `{}`.
//
// A tool that reports IsError=true is NOT returned as a Go error; the
// distinction matters because tool-level failures are part of the
// LLM-visible execution trace, whereas transport errors are not. Inspect
// ToolResult.IsError on the returned value.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	if strings.TrimSpace(name) == "" {
		return ToolResult{}, errors.New("mcp tools/call: name is empty")
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	params := map[string]any{"name": name, "arguments": args}
	resp, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return ToolResult{}, err
	}
	if resp.Error != nil {
		return ToolResult{}, fmt.Errorf("mcp tools/call %s: %s (code=%d)", name, resp.Error.Message, resp.Error.Code)
	}
	var r ToolResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return ToolResult{}, fmt.Errorf("mcp tools/call result: %w", err)
	}
	return r, nil
}

// Close terminates the connection. For subprocess-backed clients this
// also signals the server to exit (closing stdin causes most MCP
// servers to shut down cleanly) and waits up to a few seconds for the
// process to reap before returning. Idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	// Closing the writer signals EOF to the server; most MCP servers
	// shut down cleanly on stdin EOF.
	_ = c.writer.Close()
	_ = c.reader.Close()

	// Drain the read loop so callers know all goroutines are gone.
	select {
	case <-c.readDone:
	case <-time.After(2 * time.Second):
	}

	if c.cmd != nil {
		done := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			// Stdin EOF didn't trigger a clean exit. Send os.Interrupt
			// before resorting to a hard kill — stateful MCP servers
			// may flush local state on SIGINT but get corrupted when
			// killed mid-write. On Windows, Signal(os.Interrupt) is a
			// no-op; the follow-up Kill still fires after the timeout.
			_ = c.cmd.Process.Signal(os.Interrupt)
			select {
			case <-done:
			case <-time.After(1 * time.Second):
				_ = c.cmd.Process.Kill()
				<-done
			}
		}
	}
	c.failPending(errors.New("mcp client: closed"))
	return nil
}

// envMapToSlice flattens a map into "K=V" strings in a stable order.
// Empty input returns nil so callers can range over the result safely.
func envMapToSlice(in map[string]string) []string {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(in))
	for _, k := range keys {
		out = append(out, k+"="+in[k])
	}
	return out
}
