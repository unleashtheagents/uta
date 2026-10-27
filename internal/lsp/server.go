// Package lsp implements a minimal Language Server Protocol server over
// stdio: JSON-RPC 2.0 with Content-Length framing (the LSP base protocol)
// and just enough of the method surface for push diagnostics —
// initialize/initialized, textDocument/didOpen|didChange|didClose, and
// shutdown/exit — plus textDocument/formatting when a FormatFunc is
// wired. Spec: https://microsoft.github.io/language-server-protocol/.
//
// The package knows nothing about steer: a Server is a DiagnoseFunc plus
// framing. Whoever wires the server (internal/cli's `uta mission lsp`)
// decides what the diagnostics mean — the same registry posture as
// internal/mcp. Documents sync in full-text mode (TextDocumentSyncKind
// Full), so every didOpen/didChange hands DiagnoseFunc the whole source;
// the server keeps only the latest full text per open document, because
// formatting requests carry a URI but no text.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// LSP DiagnosticSeverity values.
const (
	SeverityError   = 1
	SeverityWarning = 2
)

// maxContentLength bounds a single frame so a corrupt Content-Length
// header can't ask for an absurd allocation. 16 MiB matches the mcp
// server's scanner ceiling.
const maxContentLength = 16 * 1024 * 1024

// Position and Range use the protocol's zero-based lines and characters —
// converting from 1-based compiler positions is the caller's job.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Diagnostic is one squiggle: the subset of the protocol's Diagnostic
// shape that push-only diagnostics need.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// TextEdit is one replacement. Formatting responses here always carry a
// single edit spanning the whole document — the standard shape for
// full-file formatters.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// DiagnoseFunc computes the full diagnostic set for one document state.
// It runs on every didOpen and didChange, so it should be pure and cheap
// (a parse + static check, never inference or I/O).
type DiagnoseFunc func(uri, text string) []Diagnostic

// FormatFunc renders the canonical form of one document state. ok=false
// means the document cannot be formatted as-is (parse errors, constructs
// the formatter would drop) — the request answers null and the buffer
// stays untouched. Same purity bar as DiagnoseFunc.
type FormatFunc func(uri, text string) (formatted string, ok bool)

// Server serves push diagnostics for one editor connection.
type Server struct {
	name     string
	version  string
	diagnose DiagnoseFunc
	format   FormatFunc
	docs     map[string]string
}

// NewServer constructs a server. The name and version appear in the
// initialize response's serverInfo.
func NewServer(name, version string, diagnose DiagnoseFunc) *Server {
	return &Server{name: name, version: version, diagnose: diagnose, docs: map[string]string{}}
}

// SetFormatter enables textDocument/formatting: the initialize response
// advertises documentFormattingProvider and requests answer with one
// whole-document TextEdit (or null when f declines or nothing changes).
// Call before Serve.
func (s *Server) SetFormatter(f FormatFunc) { s.format = f }

// message is the JSON-RPC 2.0 envelope; id+method is a request, method
// alone a notification, id + (result|error) a response.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

// Serve reads framed JSON-RPC messages from r, dispatches them, and writes
// responses and publishDiagnostics notifications to w. It returns on the
// exit notification, when r is closed (an editor that dies without the
// shutdown/exit dance is a clean hang-up, not an error), when ctx is done,
// or on an unrecoverable read/write error. Everything runs on the calling
// goroutine — diagnostics are synchronous, so there is nothing to overlap.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, err := readFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		var msg message
		if err := json.Unmarshal(payload, &msg); err != nil {
			if werr := writeFrame(w, errorResponse(nil, codeParseError, "parse error: "+err.Error())); werr != nil {
				return werr
			}
			continue
		}
		if msg.Method == "exit" {
			return nil
		}
		for _, out := range s.handle(msg) {
			if err := writeFrame(w, out); err != nil {
				return err
			}
		}
	}
}

// handle dispatches one message and returns the frames to write, in order.
// Notifications that carry no reaction return nil.
func (s *Server) handle(msg message) []*message {
	isNotification := len(msg.ID) == 0 || string(msg.ID) == "null"
	switch msg.Method {
	case "initialize":
		return []*message{s.initializeResponse(msg.ID)}

	case "initialized":
		return nil

	case "textDocument/didOpen":
		var p struct {
			TextDocument struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"textDocument"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil
		}
		s.docs[p.TextDocument.URI] = p.TextDocument.Text
		return []*message{s.publish(p.TextDocument.URI, p.TextDocument.Text)}

	case "textDocument/didChange":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil || len(p.ContentChanges) == 0 {
			return nil
		}
		// Full sync (advertised in initialize): the last change carries
		// the entire document.
		text := p.ContentChanges[len(p.ContentChanges)-1].Text
		s.docs[p.TextDocument.URI] = text
		return []*message{s.publish(p.TextDocument.URI, text)}

	case "textDocument/didClose":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil
		}
		delete(s.docs, p.TextDocument.URI)
		// Publish an empty set: editors keep stale squiggles unless told
		// the closed document is clean.
		return []*message{publishMessage(p.TextDocument.URI, nil)}

	case "textDocument/formatting":
		if s.format == nil {
			// Not advertised, so a spec-abiding client never sends it.
			return []*message{errorResponse(msg.ID, codeMethodNotFound, "method not found: "+msg.Method)}
		}
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		null := &message{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage("null")}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return []*message{null}
		}
		text, open := s.docs[p.TextDocument.URI]
		if !open {
			return []*message{null}
		}
		formatted, ok := s.format(p.TextDocument.URI, text)
		if !ok || formatted == text {
			return []*message{null}
		}
		b, err := json.Marshal([]TextEdit{{Range: wholeDocumentRange(text), NewText: formatted}})
		if err != nil {
			return []*message{errorResponse(msg.ID, codeInternalError, "marshal edits: "+err.Error())}
		}
		return []*message{{JSONRPC: "2.0", ID: msg.ID, Result: b}}

	case "shutdown":
		return []*message{{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage("null")}}

	default:
		if isNotification {
			// $/-prefixed and other unknown notifications are ignorable by spec.
			return nil
		}
		return []*message{errorResponse(msg.ID, codeMethodNotFound, "method not found: "+msg.Method)}
	}
}

func (s *Server) initializeResponse(id json.RawMessage) *message {
	// TextDocumentSyncKind.Full — the client sends whole documents, so
	// DiagnoseFunc never needs incremental patching.
	caps := map[string]any{"textDocumentSync": 1}
	if s.format != nil {
		caps["documentFormattingProvider"] = true
	}
	result := map[string]any{
		"capabilities": caps,
		"serverInfo":   map[string]any{"name": s.name, "version": s.version},
	}
	b, err := json.Marshal(result)
	if err != nil {
		return errorResponse(id, codeInternalError, "marshal initialize result: "+err.Error())
	}
	return &message{JSONRPC: "2.0", ID: id, Result: b}
}

// publish runs the diagnose function over one document state and wraps the
// result in a publishDiagnostics notification.
func (s *Server) publish(uri, text string) *message {
	var diags []Diagnostic
	if s.diagnose != nil {
		diags = s.diagnose(uri, text)
	}
	return publishMessage(uri, diags)
}

func publishMessage(uri string, diags []Diagnostic) *message {
	if diags == nil {
		// The spec wants an array; a JSON null would trip strict clients.
		diags = []Diagnostic{}
	}
	params, err := json.Marshal(map[string]any{"uri": uri, "diagnostics": diags})
	if err != nil {
		return errorResponse(nil, codeInternalError, "marshal diagnostics: "+err.Error())
	}
	return &message{JSONRPC: "2.0", Method: "textDocument/publishDiagnostics", Params: params}
}

// wholeDocumentRange spans text from its first character through its
// last, so one TextEdit replaces the entire document. Character offsets
// are UTF-16 code units, the protocol's default position encoding.
func wholeDocumentRange(text string) Range {
	last := text
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		last = text[i+1:]
	}
	n := 0
	for _, r := range last {
		n++
		if r > 0xFFFF {
			n++ // surrogate pair
		}
	}
	return Range{End: Position{Line: strings.Count(text, "\n"), Character: n}}
}

// readFrame reads one base-protocol frame: header lines terminated by an
// empty line, then exactly Content-Length bytes of payload. Header names
// are case-insensitive; everything but Content-Length is ignored.
func readFrame(br *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && strings.TrimSpace(line) != "" {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("lsp: malformed header %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q", strings.TrimSpace(value))
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("lsp: frame missing Content-Length header")
	}
	if length > maxContentLength {
		return nil, fmt.Errorf("lsp: Content-Length %d exceeds %d-byte limit", length, maxContentLength)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeFrame(w io.Writer, msg *message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func errorResponse(id json.RawMessage, code int, msg string) *message {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &message{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}
