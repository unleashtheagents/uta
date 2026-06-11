package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
)

// drainEvents pulls every queued event off ch without blocking and returns
// them. Tests use it to verify which events parseClaudeLine emitted.
func drainEvents(ch chan provider.Event) []provider.Event {
	var out []provider.Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestBuildClaudeArgs_AppendsMCPConfig(t *testing.T) {
	opts := provider.RunOptions{
		Workdir:         "/work",
		PreApproveTools: []string{"Read", "Edit"},
		MCPConfigPath:   "/tmp/uta-mcp-abc.json",
	}
	got := buildClaudeArgs(opts, "")
	want := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--allowedTools", "Read,Edit",
		"--mcp-config", "/tmp/uta-mcp-abc.json",
		"--add-dir", "/work",
	}
	if len(got) != len(want) {
		t.Fatalf("args = %v\nwant %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestBuildClaudeArgs_OmitsMCPConfigWhenEmpty(t *testing.T) {
	got := buildClaudeArgs(provider.RunOptions{}, "")
	for _, a := range got {
		if a == "--mcp-config" {
			t.Fatalf("--mcp-config must not appear when MCPConfigPath is empty: %v", got)
		}
	}
}

func TestClaude_Name(t *testing.T) {
	c := &Claude{}
	if got := c.Name(); got != "claude" {
		t.Fatalf("Name() = %q, want %q", got, "claude")
	}
}

func TestClaude_ResumeHeadless_EmptySessionID(t *testing.T) {
	c := &Claude{}
	_, err := c.ResumeHeadless(context.Background(), "", "prompt", provider.RunOptions{}, nil)
	if err == nil {
		t.Fatal("ResumeHeadless with empty session id: want error, got nil")
	}
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("ResumeHeadless error = %v, want ErrUnsupported", err)
	}
}

func TestParseClaudeLine_SystemInitEmitsSessionID(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-abc"}`)

	sid, finalText, u := parseClaudeLine(line, ch)
	if sid != "sess-abc" {
		t.Fatalf("sessionID = %q, want %q", sid, "sess-abc")
	}
	if finalText != "" {
		t.Fatalf("finalText = %q, want empty", finalText)
	}
	if (u != claudeUsage{}) {
		t.Fatalf("usage = %+v, want zero", u)
	}
	evs := drainEvents(ch)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != provider.EventSessionID {
		t.Fatalf("event kind = %q, want session_id", evs[0].Kind)
	}
	var payload map[string]string
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["session_id"] != "sess-abc" {
		t.Fatalf("payload session_id = %q, want %q", payload["session_id"], "sess-abc")
	}
}

func TestParseClaudeLine_SystemInitWithoutSessionIDIsNoop(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"system","subtype":"init"}`)
	sid, _, _ := parseClaudeLine(line, ch)
	if sid != "" {
		t.Fatalf("sessionID = %q, want empty", sid)
	}
	if len(drainEvents(ch)) != 0 {
		t.Fatalf("want zero events when session_id missing")
	}
}

func TestParseClaudeLine_AssistantTextAndUsage(t *testing.T) {
	ch := make(chan provider.Event, 8)
	line := []byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"hello world"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}}`)

	sid, finalText, u := parseClaudeLine(line, ch)
	if sid != "" || finalText != "" {
		t.Fatalf("assistant message must not set sid/finalText, got %q/%q", sid, finalText)
	}
	if u.tokensIn != 15 { // 10 + 2 + 3
		t.Fatalf("tokensIn = %d, want 15", u.tokensIn)
	}
	if u.tokensOut != 5 {
		t.Fatalf("tokensOut = %d, want 5", u.tokensOut)
	}
	if u.gotResultTotal {
		t.Fatal("gotResultTotal must remain false for assistant message")
	}
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventAssistantText {
		t.Fatalf("events = %+v, want one assistant_text", evs)
	}
}

func TestParseClaudeLine_AssistantToolUse(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"bash","input":{"cmd":"ls"}}]}}`)

	parseClaudeLine(line, ch)
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventToolCall {
		t.Fatalf("events = %+v, want one tool_call", evs)
	}
	var payload map[string]any
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["name"] != "bash" {
		t.Fatalf("tool name = %v, want bash", payload["name"])
	}
}

func TestParseClaudeLine_UserToolResult(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"out"}]}}`)

	parseClaudeLine(line, ch)
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventToolResult {
		t.Fatalf("events = %+v, want one tool_result", evs)
	}
}

func TestParseClaudeLine_UserTextSuppressed(t *testing.T) {
	// Text blocks on user-role messages are prompt echoes; the parser must
	// not surface them as assistant text.
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"this was the prompt"}]}}`)

	parseClaudeLine(line, ch)
	if len(drainEvents(ch)) != 0 {
		t.Fatal("user-role text must not emit any event")
	}
}

func TestParseClaudeLine_ResultEnvelope(t *testing.T) {
	ch := make(chan provider.Event, 4)
	// 0.0125 USD = 1 cent after rounding.
	line := []byte(`{"type":"result","session_id":"sess-final","result":"the final answer","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":20},"total_cost_usd":0.0125}`)

	sid, finalText, u := parseClaudeLine(line, ch)
	if sid != "sess-final" {
		t.Fatalf("sessionID = %q, want sess-final", sid)
	}
	if finalText != "the final answer" {
		t.Fatalf("finalText = %q, want \"the final answer\"", finalText)
	}
	if u.tokensIn != 130 {
		t.Fatalf("tokensIn = %d, want 130", u.tokensIn)
	}
	if u.tokensOut != 50 {
		t.Fatalf("tokensOut = %d, want 50", u.tokensOut)
	}
	if u.usdCents != 1 {
		t.Fatalf("usdCents = %d, want 1", u.usdCents)
	}
	if !u.gotResultTotal {
		t.Fatal("gotResultTotal must be true on result envelope")
	}
}

func TestParseClaudeLine_MalformedJSONFallsBackToStdoutChunk(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`this is not json at all`)

	sid, finalText, u := parseClaudeLine(line, ch)
	if sid != "" || finalText != "" {
		t.Fatalf("malformed must yield empty sid/finalText, got %q/%q", sid, finalText)
	}
	if (u != claudeUsage{}) {
		t.Fatalf("usage = %+v, want zero", u)
	}
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventStdoutChunk {
		t.Fatalf("events = %+v, want one stdout_chunk", evs)
	}
}

func TestParseClaudeLine_UnknownTypeIsNoop(t *testing.T) {
	ch := make(chan provider.Event, 4)
	line := []byte(`{"type":"telemetry","foo":"bar"}`)
	sid, finalText, u := parseClaudeLine(line, ch)
	if sid != "" || finalText != "" || (u != claudeUsage{}) {
		t.Fatalf("unknown type must be inert, got sid=%q text=%q u=%+v", sid, finalText, u)
	}
	if len(drainEvents(ch)) != 0 {
		t.Fatal("unknown type must not emit events")
	}
}

func TestClaudeUsage_MergeResultEnvelopeReplaces(t *testing.T) {
	u := claudeUsage{tokensIn: 50, tokensOut: 30}
	u.merge(claudeUsage{tokensIn: 200, tokensOut: 80, usdCents: 7, gotResultTotal: true})
	if u.tokensIn != 200 || u.tokensOut != 80 || u.usdCents != 7 || !u.gotResultTotal {
		t.Fatalf("merge result envelope: got %+v", u)
	}
}

func TestClaudeUsage_MergePerMessageAccumulates(t *testing.T) {
	u := claudeUsage{tokensIn: 50, tokensOut: 30}
	u.merge(claudeUsage{tokensIn: 20, tokensOut: 10})
	if u.tokensIn != 70 || u.tokensOut != 40 || u.gotResultTotal {
		t.Fatalf("per-message merge: got %+v", u)
	}
}

func TestClaudeUsage_MergeIgnoredAfterResultEnvelope(t *testing.T) {
	u := claudeUsage{tokensIn: 100, tokensOut: 60, usdCents: 5, gotResultTotal: true}
	u.merge(claudeUsage{tokensIn: 99, tokensOut: 99})
	if u.tokensIn != 100 || u.tokensOut != 60 {
		t.Fatalf("post-result merge must be a no-op: got %+v", u)
	}
}

func TestClaudeUsageJSON_TotalInputTokens(t *testing.T) {
	u := &claudeUsageJSON{InputTokens: 100, CacheCreationInputTokens: 10, CacheReadInputTokens: 20}
	if got := u.totalInputTokens(); got != 130 {
		t.Fatalf("totalInputTokens = %d, want 130", got)
	}
}

func TestClaudeUsageJSON_TotalInputTokensNilSafe(t *testing.T) {
	var u *claudeUsageJSON
	if got := u.totalInputTokens(); got != 0 {
		t.Fatalf("nil totalInputTokens = %d, want 0", got)
	}
}

func TestUsageFromMessage(t *testing.T) {
	tests := []struct {
		name      string
		msg       string
		tokensIn  int64
		tokensOut int64
	}{
		{"empty", "", 0, 0},
		{"malformed", "not json", 0, 0},
		{"no usage block", `{"id":"m1","content":[]}`, 0, 0},
		{"with usage", `{"usage":{"input_tokens":10,"output_tokens":7,"cache_creation_input_tokens":1,"cache_read_input_tokens":2}}`, 13, 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := usageFromMessage(json.RawMessage(tc.msg))
			if got.tokensIn != tc.tokensIn || got.tokensOut != tc.tokensOut {
				t.Fatalf("usageFromMessage(%q) = %+v, want in=%d out=%d", tc.msg, got, tc.tokensIn, tc.tokensOut)
			}
		})
	}
}

func TestEmitClaudeMessage_MalformedIsSilent(t *testing.T) {
	ch := make(chan provider.Event, 4)
	emitClaudeMessage(json.RawMessage("not json"), ch, false)
	if len(drainEvents(ch)) != 0 {
		t.Fatal("malformed message envelope must not emit events")
	}
}

func TestEmitClaudeMessage_EmptyIsSilent(t *testing.T) {
	ch := make(chan provider.Event, 4)
	emitClaudeMessage(nil, ch, false)
	if len(drainEvents(ch)) != 0 {
		t.Fatal("empty message must not emit events")
	}
}

func TestEmitClaudeMessage_SkipsTextWhenIsUser(t *testing.T) {
	ch := make(chan provider.Event, 4)
	msg := json.RawMessage(`{"content":[{"type":"text","text":"x"},{"type":"tool_result","tool_use_id":"t1","content":"r"}]}`)
	emitClaudeMessage(msg, ch, true)
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Kind != provider.EventToolResult {
		t.Fatalf("isUser=true: want only tool_result, got %+v", evs)
	}
}
