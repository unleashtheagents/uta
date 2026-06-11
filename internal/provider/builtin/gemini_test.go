package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
)

func TestGemini_Name(t *testing.T) {
	g := &Gemini{}
	if got := g.Name(); got != "gemini" {
		t.Fatalf("Name() = %q, want %q", got, "gemini")
	}
}

func TestBuildGeminiArgs_AppendsMCPConfig(t *testing.T) {
	got := buildGeminiArgs("the prompt", "", provider.RunOptions{
		MCPConfigPath: "/tmp/uta-mcp-abc.json",
	})
	want := []string{
		"-p", "the prompt", "--output-format", "stream-json",
		"--mcp-config", "/tmp/uta-mcp-abc.json",
	}
	if len(got) != len(want) {
		t.Fatalf("args = %v\nwant %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestBuildGeminiArgs_OmitsMCPConfigWhenEmpty(t *testing.T) {
	got := buildGeminiArgs("p", "", provider.RunOptions{})
	for _, a := range got {
		if a == "--mcp-config" {
			t.Fatalf("--mcp-config must not appear when MCPConfigPath is empty: %v", got)
		}
	}
}

func TestBuildGeminiArgs_ResumeFlagBeforeMCPConfig(t *testing.T) {
	// Order matters for tests but more importantly: when a session is
	// being resumed AND an MCP config is supplied, both must be present
	// in the right shape. The -r pointer should precede --mcp-config so
	// the resume happens against the same MCP toolset the original run
	// had access to.
	got := buildGeminiArgs("p", "sess-1", provider.RunOptions{
		MCPConfigPath: "/tmp/cfg.json",
	})
	mcpIdx, resIdx := -1, -1
	for i, a := range got {
		if a == "--mcp-config" {
			mcpIdx = i
		}
		if a == "-r" {
			resIdx = i
		}
	}
	if resIdx == -1 || mcpIdx == -1 {
		t.Fatalf("missing -r or --mcp-config in %v", got)
	}
	if resIdx > mcpIdx {
		t.Errorf("expected -r (%d) before --mcp-config (%d) in %v", resIdx, mcpIdx, got)
	}
}

func TestGemini_ResumeHeadless_EmptySessionID(t *testing.T) {
	g := &Gemini{}
	_, err := g.ResumeHeadless(context.Background(), "", "prompt", provider.RunOptions{}, nil)
	if err == nil {
		t.Fatal("ResumeHeadless with empty session id: want error, got nil")
	}
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("ResumeHeadless error = %v, want ErrUnsupported", err)
	}
}

func TestParseGeminiLine_InitEmitsSessionID(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"init","session_id":"g-1","model":"gemini-2.0"}`)

	sid, text := parseGeminiLine(line, ch)
	if sid != "g-1" {
		t.Fatalf("sessionID = %q, want g-1", sid)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventSessionID {
		t.Fatalf("events = %+v, want one session_id", evs)
	}
}

func TestParseGeminiLine_AssistantMessageEmitsText(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"message","role":"assistant","content":"the reply"}`)

	sid, text := parseGeminiLine(line, ch)
	if sid != "" {
		t.Fatalf("sessionID = %q, want empty (no session id on message)", sid)
	}
	if text != "the reply" {
		t.Fatalf("text = %q, want \"the reply\"", text)
	}
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventAssistantText {
		t.Fatalf("events = %+v, want one assistant_text", evs)
	}
}

func TestParseGeminiLine_UserMessageSuppressed(t *testing.T) {
	// User-role messages are echoes of the prompt; the parser must not
	// surface them as assistant text or the gatherer ends up parsing its
	// own prompt as the model's response.
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"message","role":"user","content":"this was the original prompt"}`)

	sid, text := parseGeminiLine(line, ch)
	if sid != "" || text != "" {
		t.Fatalf("user message must yield empty sid/text, got %q/%q", sid, text)
	}
	if len(drainEvents(ch)) != 0 {
		t.Fatal("user-role message must not emit any event")
	}
}

func TestParseGeminiLine_ToolCallEmitted(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"tool_call","name":"ls","args":{"path":"/"}}`)

	parseGeminiLine(line, ch)
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventToolCall {
		t.Fatalf("events = %+v, want one tool_call", evs)
	}
}

func TestParseGeminiLine_ToolResultEmitted(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"tool_result","output":"x"}`)

	parseGeminiLine(line, ch)
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventToolResult {
		t.Fatalf("events = %+v, want one tool_result", evs)
	}
}

func TestParseGeminiLine_ErrorEventEmitted(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"error","message":"boom"}`)

	parseGeminiLine(line, ch)
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventError {
		t.Fatalf("events = %+v, want one error", evs)
	}
}

func TestParseGeminiLine_ResultEventIsTerminalNoop(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"result","status":"success","stats":{"tokens":42}}`)

	sid, text := parseGeminiLine(line, ch)
	if sid != "" || text != "" {
		t.Fatalf("result must yield empty sid/text, got %q/%q", sid, text)
	}
	if len(drainEvents(ch)) != 0 {
		t.Fatal("result event must not emit any event")
	}
}

func TestParseGeminiLine_MalformedJSONFallsBackToStdoutChunk(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`not json at all`)

	sid, text := parseGeminiLine(line, ch)
	if sid != "" || text != "" {
		t.Fatalf("malformed must yield empty sid/text, got %q/%q", sid, text)
	}
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventStdoutChunk {
		t.Fatalf("events = %+v, want one stdout_chunk", evs)
	}
}

func TestParseGeminiLine_UnknownTypeFallsBackToStdoutChunk(t *testing.T) {
	// Unknown event types are recorded as stdout but must NOT contribute to
	// the final answer text — being conservative here keeps echoes out of
	// downstream parsers.
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"some_future_event","payload":1}`)

	sid, text := parseGeminiLine(line, ch)
	if sid != "" || text != "" {
		t.Fatalf("unknown type must yield empty sid/text, got %q/%q", sid, text)
	}
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventStdoutChunk {
		t.Fatalf("events = %+v, want one stdout_chunk", evs)
	}
}

func TestParseGeminiLine_SessionIDOnNonInitEvent(t *testing.T) {
	// The session-id extraction happens before the type switch so it can
	// be picked up off any event that carries it.
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"tool_call","session_id":"sess-mid","name":"ls"}`)

	sid, _ := parseGeminiLine(line, ch)
	if sid != "sess-mid" {
		t.Fatalf("sessionID = %q, want sess-mid", sid)
	}
	evs := drainEvents(ch)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (session_id + tool_call)", len(evs))
	}
	if evs[0].Kind != provider.EventSessionID || evs[1].Kind != provider.EventToolCall {
		t.Fatalf("event kinds = [%q, %q], want [session_id, tool_call]", evs[0].Kind, evs[1].Kind)
	}
}

func TestPickString_FirstNonEmptyMatch(t *testing.T) {
	m := map[string]any{"a": "", "b": "found", "c": "later"}
	v, ok := pickString(m, "a", "b", "c")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if v != "found" {
		t.Fatalf("v = %q, want \"found\"", v)
	}
}

func TestPickString_NoMatchWhenAllAbsentOrEmpty(t *testing.T) {
	m := map[string]any{"x": 42, "y": "", "z": nil}
	if _, ok := pickString(m, "x", "y", "z", "missing"); ok {
		t.Fatal("ok = true, want false (no string-typed non-empty values)")
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"abcd", 1},      // exact 4 chars
		{"abcde", 2},     // round up
		{"abcdefgh", 2},  // 8/4
		{"abcdefghi", 3}, // 9/4 → 2 + 1 (round up)
	}
	for _, tc := range tests {
		got := estimateTokens(tc.in)
		if got != tc.want {
			t.Fatalf("estimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// pickString must tolerate non-string types in the map without panicking.
func TestPickString_IgnoresNonStringValues(t *testing.T) {
	m := map[string]any{"a": 1, "b": true, "c": "ok"}
	v, ok := pickString(m, "a", "b", "c")
	if !ok || v != "ok" {
		t.Fatalf("pickString = (%q, %v), want (\"ok\", true)", v, ok)
	}
}

// JSON round-trip on the tool_call payload should preserve the original
// envelope shape so downstream consumers (recorder, gatherer) can interpret
// gemini-specific fields without further coordination.
func TestParseGeminiLine_ToolCallPayloadIsFullEnvelope(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"tool_call","name":"bash","args":{"cmd":"ls -la"}}`)

	parseGeminiLine(line, ch)
	evs := drainEvents(ch)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	var payload map[string]any
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["name"] != "bash" {
		t.Fatalf("payload.name = %v, want bash", payload["name"])
	}
	if payload["type"] != "tool_call" {
		t.Fatalf("payload.type = %v, want tool_call", payload["type"])
	}
}
