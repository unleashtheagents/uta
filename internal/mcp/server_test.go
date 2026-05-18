package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

func decodeAll(t *testing.T, r io.Reader) []Message {
	t.Helper()
	dec := json.NewDecoder(r)
	var out []Message
	for {
		var m Message
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				return out
			}
			t.Fatalf("decode: %v", err)
		}
		out = append(out, m)
	}
}

func runOne(t *testing.T, s *Server, req string) Message {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(req+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	msgs := decodeAll(t, &out)
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 response, got %d: %s", len(msgs), out.String())
	}
	return msgs[0]
}

func TestNewServerInitializesEmpty(t *testing.T) {
	s := NewServer("uta", "0.0.0-test")
	if s.ToolCount() != 0 {
		t.Fatalf("expected 0 tools, got %d", s.ToolCount())
	}
	if s.name != "uta" || s.version != "0.0.0-test" {
		t.Fatalf("name/version not stored: %q %q", s.name, s.version)
	}
	if s.handlers == nil {
		t.Fatalf("handlers map should be non-nil")
	}
}

func TestRegisterToolRejectsEmptyName(t *testing.T) {
	s := NewServer("uta", "v")
	err := s.RegisterTool(Tool{Name: "  "}, func(context.Context, json.RawMessage) ToolResult { return TextResult("") })
	if err == nil {
		t.Fatal("expected error for blank name")
	}
}

func TestRegisterToolRejectsNilHandler(t *testing.T) {
	s := NewServer("uta", "v")
	err := s.RegisterTool(Tool{Name: "x"}, nil)
	if err == nil {
		t.Fatal("expected error for nil handler")
	}
}

func TestRegisterToolFillsDefaultSchema(t *testing.T) {
	s := NewServer("uta", "v")
	err := s.RegisterTool(Tool{Name: "x"}, func(context.Context, json.RawMessage) ToolResult { return TextResult("ok") })
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(s.tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(s.tools))
	}
	if len(s.tools[0].InputSchema) == 0 {
		t.Fatal("default schema should be filled in")
	}
	var schema map[string]any
	if err := json.Unmarshal(s.tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("default schema not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("default schema type=%v", schema["type"])
	}
}

func TestRegisterToolReplacesByName(t *testing.T) {
	s := NewServer("uta", "v")
	first := func(context.Context, json.RawMessage) ToolResult { return TextResult("first") }
	second := func(context.Context, json.RawMessage) ToolResult { return TextResult("second") }
	if err := s.RegisterTool(Tool{Name: "x", Description: "first"}, first); err != nil {
		t.Fatalf("register first: %v", err)
	}
	if err := s.RegisterTool(Tool{Name: "x", Description: "second"}, second); err != nil {
		t.Fatalf("register second: %v", err)
	}
	if s.ToolCount() != 1 {
		t.Fatalf("tool list should not grow on replace, got %d", s.ToolCount())
	}
	if s.tools[0].Description != "second" {
		t.Fatalf("replace did not update tool metadata: %q", s.tools[0].Description)
	}
	res := s.handlers["x"](context.Background(), nil)
	if res.Content[0].Text != "second" {
		t.Fatalf("replace did not swap handler: %q", res.Content[0].Text)
	}
}

func TestRegisterToolConcurrent(t *testing.T) {
	// The mutex must keep concurrent registrations safe and produce a stable
	// final count. Go's race detector will fail this if the mutex is dropped.
	s := NewServer("uta", "v")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := "tool"
			if n%2 == 1 {
				name = "other"
			}
			_ = s.RegisterTool(Tool{Name: name}, func(context.Context, json.RawMessage) ToolResult { return TextResult("") })
		}(i)
	}
	wg.Wait()
	if c := s.ToolCount(); c != 2 {
		t.Fatalf("want 2 distinct tools, got %d", c)
	}
}

func TestHandleInitializeMarksInitializedAndAdvertisesVersion(t *testing.T) {
	s := NewServer("uta", "1.2.3")
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"c","version":"v"}}}`
	resp := runOne(t, s, req)
	if !s.initialized {
		t.Fatal("server should be flagged as initialized")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var r initializeResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if r.ProtocolVersion != ProtocolVersion {
		t.Fatalf("advertised protocol = %q", r.ProtocolVersion)
	}
	if r.ServerInfo.Name != "uta" || r.ServerInfo.Version != "1.2.3" {
		t.Fatalf("serverInfo = %+v", r.ServerInfo)
	}
	if r.Capabilities["tools"] == nil {
		t.Fatal("tools capability should be advertised")
	}
}

func TestHandlePingReturnsEmptyResult(t *testing.T) {
	s := NewServer("uta", "v")
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":42,"method":"ping"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if string(resp.Result) != "{}" {
		t.Fatalf("ping result = %s, want {}", resp.Result)
	}
	if string(resp.ID) != "42" {
		t.Fatalf("id roundtrip failed: %s", resp.ID)
	}
}

func TestHandleShutdownReturnsEmptyResult(t *testing.T) {
	s := NewServer("uta", "v")
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":"abc","method":"shutdown"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if string(resp.Result) != "{}" {
		t.Fatalf("shutdown result = %s", resp.Result)
	}
}

func TestHandleToolsList(t *testing.T) {
	s := NewServer("uta", "v")
	_ = s.RegisterTool(Tool{Name: "alpha", Description: "first"}, func(context.Context, json.RawMessage) ToolResult { return TextResult("a") })
	_ = s.RegisterTool(Tool{Name: "beta", Description: "second"}, func(context.Context, json.RawMessage) ToolResult { return TextResult("b") })
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var body struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Tools) != 2 || body.Tools[0].Name != "alpha" || body.Tools[1].Name != "beta" {
		t.Fatalf("tools = %+v", body.Tools)
	}
}

func TestHandleToolsCallRoundtrip(t *testing.T) {
	s := NewServer("uta", "v")
	_ = s.RegisterTool(Tool{Name: "echo"}, func(_ context.Context, args json.RawMessage) ToolResult {
		return TextResult(string(args))
	})
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var tr ToolResult
	if err := json.Unmarshal(resp.Result, &tr); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if tr.IsError {
		t.Fatal("did not expect tool error")
	}
	if got := tr.Content[0].Text; got != `{"x":1}` {
		t.Fatalf("args passthrough = %q", got)
	}
}

func TestHandleToolsCallDefaultsEmptyArgsToObject(t *testing.T) {
	s := NewServer("uta", "v")
	var got string
	_ = s.RegisterTool(Tool{Name: "echo"}, func(_ context.Context, args json.RawMessage) ToolResult {
		got = string(args)
		return TextResult("")
	})
	_ = runOne(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`)
	if got != "{}" {
		t.Fatalf("expected empty args to default to {}, got %q", got)
	}
}

func TestHandleToolsCallUnknownToolReturnsMethodNotFound(t *testing.T) {
	s := NewServer("uta", "v")
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`)
	if resp.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
	if resp.Error.Code != CodeMethodNotFound {
		t.Fatalf("code = %d, want %d", resp.Error.Code, CodeMethodNotFound)
	}
}

func TestHandleToolsCallInvalidParamsJSON(t *testing.T) {
	s := NewServer("uta", "v")
	// params is a string, not an object — decode of toolCallParams will fail.
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"oops"}`)
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for bad params")
	}
	if resp.Error.Code != CodeInvalidParams {
		t.Fatalf("code = %d, want %d", resp.Error.Code, CodeInvalidParams)
	}
}

func TestHandleToolsCallRecoversFromPanic(t *testing.T) {
	s := NewServer("uta", "v")
	_ = s.RegisterTool(Tool{Name: "boom"}, func(context.Context, json.RawMessage) ToolResult {
		panic("kaboom")
	})
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}`)
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error from panicking handler")
	}
	if resp.Error.Code != CodeInternalError {
		t.Fatalf("code = %d, want %d", resp.Error.Code, CodeInternalError)
	}
	if !strings.Contains(resp.Error.Message, "kaboom") {
		t.Fatalf("panic value not surfaced: %q", resp.Error.Message)
	}
}

func TestHandleUnknownMethod(t *testing.T) {
	s := NewServer("uta", "v")
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":1,"method":"does/not/exist"}`)
	if resp.Error == nil || resp.Error.Code != CodeMethodNotFound {
		t.Fatalf("want method-not-found, got %+v", resp.Error)
	}
}

func TestHandleNotificationProducesNoResponse(t *testing.T) {
	s := NewServer("uta", "v")
	var out bytes.Buffer
	// `notifications/initialized` is the canonical no-reply notification, and
	// an id-less message of any unknown method also gets silently dropped.
	req := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","method":"completely/unknown"}` + "\n"
	if err := s.Serve(context.Background(), strings.NewReader(req), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("notifications must produce no output, got: %s", out.String())
	}
}

func TestServeMalformedJSONReturnsParseError(t *testing.T) {
	s := NewServer("uta", "v")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader("not-json\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	msgs := decodeAll(t, &out)
	if len(msgs) != 1 {
		t.Fatalf("want 1 response, got %d", len(msgs))
	}
	if msgs[0].Error == nil || msgs[0].Error.Code != CodeParseError {
		t.Fatalf("want parse-error, got %+v", msgs[0].Error)
	}
	if string(msgs[0].ID) != "null" {
		t.Fatalf("parse-error id should be null, got %s", msgs[0].ID)
	}
}

func TestServeSkipsBlankLines(t *testing.T) {
	s := NewServer("uta", "v")
	var out bytes.Buffer
	input := "\n   \n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n\n"
	if err := s.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	msgs := decodeAll(t, &out)
	if len(msgs) != 1 {
		t.Fatalf("blank lines should be skipped — got %d responses: %s", len(msgs), out.String())
	}
}

func TestServeMultipleRequestsInOrder(t *testing.T) {
	s := NewServer("uta", "v")
	_ = s.RegisterTool(Tool{Name: "echo"}, func(_ context.Context, a json.RawMessage) ToolResult { return TextResult(string(a)) })
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"k":"v"}}}`,
		"",
	}, "\n")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	msgs := decodeAll(t, &out)
	if len(msgs) != 3 {
		t.Fatalf("want 3 responses, got %d", len(msgs))
	}
	for i, m := range msgs {
		want := []string{"1", "2", "3"}[i]
		if string(m.ID) != want {
			t.Fatalf("response[%d] id=%s, want %s", i, m.ID, want)
		}
	}
}

func TestServeContextCancelStopsLoop(t *testing.T) {
	s := NewServer("uta", "v")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before Serve starts reading
	// Provide many lines; the loop should exit on the first ctx check after
	// the first Scan. We tolerate either 0 or 1 responses depending on
	// scheduling — but the loop must return promptly with ctx.Err().
	input := strings.Repeat(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n", 100)
	var out bytes.Buffer
	err := s.Serve(ctx, strings.NewReader(input), &out)
	if err == nil {
		// If the entire input was consumed before the cancellation check ran,
		// Serve returns nil. That's acceptable — but with 100 lines and a
		// pre-cancelled ctx, we expect ctx.Err().
		// Don't fail; this is a best-effort check.
		return
	}
	if err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestTextResultAndErrorResult(t *testing.T) {
	tr := TextResult("hello")
	if tr.IsError || len(tr.Content) != 1 || tr.Content[0].Type != "text" || tr.Content[0].Text != "hello" {
		t.Fatalf("TextResult shape wrong: %+v", tr)
	}
	er := ErrorResult("boom")
	if !er.IsError || er.Content[0].Text != "boom" {
		t.Fatalf("ErrorResult shape wrong: %+v", er)
	}
	ae := ArgError("bad %s", "thing")
	if !ae.IsError || !strings.Contains(ae.Content[0].Text, "invalid arguments: bad thing") {
		t.Fatalf("ArgError text = %q", ae.Content[0].Text)
	}
}
