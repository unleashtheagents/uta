package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// frame wraps one JSON body in base-protocol framing.
func frame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

// serveScript feeds the framed bodies to Serve in one shot and returns
// every frame the server wrote, decoded.
func serveScript(t *testing.T, s *Server, bodies ...string) []message {
	t.Helper()
	var in strings.Builder
	for _, b := range bodies {
		in.WriteString(frame(b))
	}
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(in.String()), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return decodeFrames(t, &out)
}

func decodeFrames(t *testing.T, r io.Reader) []message {
	t.Helper()
	br := bufio.NewReader(r)
	var msgs []message
	for {
		payload, err := readFrame(br)
		if errors.Is(err, io.EOF) {
			return msgs
		}
		if err != nil {
			t.Fatalf("readFrame: %v", err)
		}
		var m message
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("unmarshal frame %s: %v", payload, err)
		}
		msgs = append(msgs, m)
	}
}

type publishParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

func decodePublish(t *testing.T, m message) publishParams {
	t.Helper()
	if m.Method != "textDocument/publishDiagnostics" {
		t.Fatalf("method = %q, want textDocument/publishDiagnostics", m.Method)
	}
	var p publishParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatalf("decode publish params: %v", err)
	}
	return p
}

// oneDiag is a stub DiagnoseFunc: any non-empty document gets exactly one
// error pinned at line 4, empty documents are clean.
func oneDiag(uri, text string) []Diagnostic {
	if text == "" {
		return nil
	}
	pos := Position{Line: 4, Character: 7}
	return []Diagnostic{{
		Range:    Range{Start: pos, End: pos},
		Severity: SeverityError,
		Source:   "test",
		Message:  "broken: " + text,
	}}
}

func TestServe_InitializeHandshake(t *testing.T) {
	s := NewServer("uta-steer", "v0.0.0-test", oneDiag)
	msgs := serveScript(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	)
	if len(msgs) != 1 {
		t.Fatalf("got %d frames, want 1 (initialize response only)", len(msgs))
	}
	resp := msgs[0]
	if string(resp.ID) != "1" || resp.Error != nil {
		t.Fatalf("initialize response = %+v", resp)
	}
	var result struct {
		Capabilities struct {
			TextDocumentSync int `json:"textDocumentSync"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if result.Capabilities.TextDocumentSync != 1 {
		t.Errorf("textDocumentSync = %d, want 1 (full)", result.Capabilities.TextDocumentSync)
	}
	if result.ServerInfo.Name != "uta-steer" {
		t.Errorf("serverInfo.name = %q", result.ServerInfo.Name)
	}
}

func TestServe_DidOpenPublishesDiagnostics(t *testing.T) {
	s := NewServer("t", "v", oneDiag)
	msgs := serveScript(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///m.steer","languageId":"steer","version":1,"text":"bad"}}}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	)
	if len(msgs) != 2 {
		t.Fatalf("got %d frames, want 2 (initialize response + publish)", len(msgs))
	}
	p := decodePublish(t, msgs[1])
	if p.URI != "file:///m.steer" {
		t.Errorf("uri = %q", p.URI)
	}
	if len(p.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want 1", p.Diagnostics)
	}
	d := p.Diagnostics[0]
	if d.Range.Start.Line != 4 || d.Range.Start.Character != 7 {
		t.Errorf("range start = %+v, want 4:7", d.Range.Start)
	}
	if d.Severity != SeverityError || d.Message != "broken: bad" {
		t.Errorf("diagnostic = %+v", d)
	}
}

func TestServe_DidChangeRepublishesAndDidCloseClears(t *testing.T) {
	s := NewServer("t", "v", oneDiag)
	msgs := serveScript(t, s,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///m.steer","text":"bad"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///m.steer","version":2},"contentChanges":[{"text":""}]}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didClose","params":{"textDocument":{"uri":"file:///m.steer"}}}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	)
	if len(msgs) != 3 {
		t.Fatalf("got %d frames, want 3 publishes", len(msgs))
	}
	if p := decodePublish(t, msgs[0]); len(p.Diagnostics) != 1 {
		t.Errorf("didOpen publish = %+v, want 1 diagnostic", p.Diagnostics)
	}
	// The edit fixed the document — the republish must be an empty array
	// (not null) so the editor clears the squiggle.
	if p := decodePublish(t, msgs[1]); p.Diagnostics == nil || len(p.Diagnostics) != 0 {
		t.Errorf("didChange publish = %+v, want empty non-nil", p.Diagnostics)
	}
	if p := decodePublish(t, msgs[2]); p.Diagnostics == nil || len(p.Diagnostics) != 0 {
		t.Errorf("didClose publish = %+v, want empty non-nil", p.Diagnostics)
	}
}

func TestServe_Formatting(t *testing.T) {
	s := NewServer("t", "v", oneDiag)
	// Stub formatter: upper-cases the document, declines "bad".
	s.SetFormatter(func(uri, text string) (string, bool) {
		if text == "bad" {
			return "", false
		}
		return strings.ToUpper(text), true
	})
	msgs := serveScript(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///m.steer","text":"a\nbc"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"textDocument/formatting","params":{"textDocument":{"uri":"file:///m.steer"},"options":{"tabSize":2,"insertSpaces":true}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///m.steer"},"contentChanges":[{"text":"bad"}]}}`,
		`{"jsonrpc":"2.0","id":3,"method":"textDocument/formatting","params":{"textDocument":{"uri":"file:///m.steer"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///m.steer"},"contentChanges":[{"text":"OK"}]}}`,
		`{"jsonrpc":"2.0","id":4,"method":"textDocument/formatting","params":{"textDocument":{"uri":"file:///m.steer"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"textDocument/formatting","params":{"textDocument":{"uri":"file:///never-opened.steer"}}}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	)
	if len(msgs) != 8 {
		t.Fatalf("got %d frames, want 8 (init + 3 publishes + 4 formatting responses)", len(msgs))
	}

	var init struct {
		Capabilities struct {
			DocumentFormattingProvider bool `json:"documentFormattingProvider"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(msgs[0].Result, &init); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if !init.Capabilities.DocumentFormattingProvider {
		t.Error("documentFormattingProvider not advertised")
	}

	var edits []TextEdit
	if err := json.Unmarshal(msgs[2].Result, &edits); err != nil {
		t.Fatalf("decode formatting result %s: %v", msgs[2].Result, err)
	}
	if len(edits) != 1 || edits[0].NewText != "A\nBC" {
		t.Fatalf("edits = %+v, want one whole-document edit to %q", edits, "A\nBC")
	}
	want := Range{Start: Position{0, 0}, End: Position{Line: 1, Character: 2}}
	if edits[0].Range != want {
		t.Errorf("edit range = %+v, want %+v", edits[0].Range, want)
	}

	// Declined format, already-canonical document, and never-opened
	// document all answer null. (msgs 3 and 5 are the didChange publishes.)
	for _, m := range []message{msgs[4], msgs[6], msgs[7]} {
		if string(m.Result) != "null" || m.Error != nil {
			t.Errorf("response id %s = %+v, want null result", m.ID, m)
		}
	}
}

func TestServe_FormattingWithoutFormatter(t *testing.T) {
	s := NewServer("t", "v", oneDiag)
	msgs := serveScript(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"textDocument/formatting","params":{"textDocument":{"uri":"file:///m.steer"}}}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	)
	if len(msgs) != 1 || msgs[0].Error == nil || msgs[0].Error.Code != codeMethodNotFound {
		t.Fatalf("formatting without a formatter = %+v, want method-not-found", msgs)
	}
}

func TestServe_ShutdownThenExit(t *testing.T) {
	s := NewServer("t", "v", oneDiag)
	msgs := serveScript(t, s,
		`{"jsonrpc":"2.0","id":9,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
		// Anything after exit must never be processed.
		`{"jsonrpc":"2.0","id":10,"method":"shutdown"}`,
	)
	if len(msgs) != 1 {
		t.Fatalf("got %d frames, want 1 (shutdown response only)", len(msgs))
	}
	if string(msgs[0].ID) != "9" || string(msgs[0].Result) != "null" || msgs[0].Error != nil {
		t.Errorf("shutdown response = %+v", msgs[0])
	}
}

func TestServe_UnknownRequestAndNotification(t *testing.T) {
	s := NewServer("t", "v", oneDiag)
	msgs := serveScript(t, s,
		`{"jsonrpc":"2.0","method":"$/setTrace","params":{"value":"off"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"textDocument/hover","params":{}}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	)
	if len(msgs) != 1 {
		t.Fatalf("got %d frames, want 1 (unknown notification stays silent)", len(msgs))
	}
	if msgs[0].Error == nil || msgs[0].Error.Code != codeMethodNotFound {
		t.Errorf("unknown request response = %+v, want method-not-found", msgs[0])
	}
}

func TestServe_ClientHangupIsClean(t *testing.T) {
	s := NewServer("t", "v", oneDiag)
	// Reader closes with no shutdown/exit — a killed editor. Serve must
	// return nil, not an EOF error.
	var out bytes.Buffer
	in := frame(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if err := s.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatalf("Serve after hang-up: %v", err)
	}
}

func TestServe_OverInMemoryPipe(t *testing.T) {
	// Interleaved request/response over a real pipe, the way an editor
	// drives the server.
	s := NewServer("t", "v", oneDiag)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), inR, outW) }()
	br := bufio.NewReader(outR)

	send := func(body string) {
		t.Helper()
		if _, err := io.WriteString(inW, frame(body)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	recv := func() message {
		t.Helper()
		payload, err := readFrame(br)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m message
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return m
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if resp := recv(); string(resp.ID) != "1" {
		t.Fatalf("initialize response id = %s", resp.ID)
	}
	send(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///m.steer","text":"bad"}}}`)
	if p := decodePublish(t, recv()); len(p.Diagnostics) != 1 || p.Diagnostics[0].Message != "broken: bad" {
		t.Fatalf("publish = %+v", p)
	}
	send(`{"jsonrpc":"2.0","id":2,"method":"shutdown"}`)
	if resp := recv(); string(resp.ID) != "2" {
		t.Fatalf("shutdown response id = %s", resp.ID)
	}
	send(`{"jsonrpc":"2.0","method":"exit"}`)
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	inW.Close()
	outW.Close()
}
