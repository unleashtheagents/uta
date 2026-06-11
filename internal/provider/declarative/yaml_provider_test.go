package declarative

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/provider"
)

// newStreamJSONProvider builds a Provider configured for stream-json parsing
// with sensible defaults so each test only needs to override what it cares
// about. The descriptor is intentionally minimal: parseStreamJSON does not
// touch the invocation, detect, or binary fields.
func newStreamJSONProvider(t *testing.T, mutate func(*config.ProviderDescriptor)) *Provider {
	t.Helper()
	desc := &config.ProviderDescriptor{
		Name:   "test",
		Binary: "echo",
		Detect: config.ProviderDescriptorDetect{
			Args:         []string{"--version"},
			VersionRegex: `(\d+\.\d+)`,
		},
		Invocation: config.ProviderDescriptorInvocation{
			Argv: []string{"--prompt", "{{prompt}}"},
		},
		Output: config.ProviderDescriptorOutput{
			Format:         "stream-json",
			SessionIDField: "session_id",
			FinalTextField: "final_text",
			TextField:      "text",
			EventDispatch: map[string]string{
				"assistant": "assistant_text",
				"tool_use":  "tool_call",
				"tool_out":  "tool_result",
				"err":       "error",
			},
		},
	}
	if mutate != nil {
		mutate(desc)
	}
	p, err := New(desc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// drainEvents reads everything currently available on ch until it would block,
// then returns the snapshot. Callers should close the channel before invoking
// this helper so the range terminates cleanly.
func drainEvents(ch chan provider.Event) []provider.Event {
	var out []provider.Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestParseStreamJSON_HappyPath(t *testing.T) {
	p := newStreamJSONProvider(t, nil)
	input := strings.Join([]string{
		`{"type":"session","session_id":"sess-123"}`,
		`{"type":"assistant","text":"hello world"}`,
		`{"type":"tool_use","name":"Read","input":{"path":"x"}}`,
		`{"type":"tool_out","output":"ok"}`,
		`{"type":"final","final_text":"all done"}`,
	}, "\n") + "\n"

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 32)

	sessionID, finalText := p.parseStreamJSON(strings.NewReader(input), &raw, &mu, events)
	close(events)

	if sessionID != "sess-123" {
		t.Errorf("session id: got %q, want %q", sessionID, "sess-123")
	}
	if finalText != "all done" {
		t.Errorf("final text: got %q, want %q", finalText, "all done")
	}
	if !bytes.Equal(raw.Bytes(), []byte(input)) {
		t.Errorf("raw buffer mismatch:\n got: %q\nwant: %q", raw.String(), input)
	}

	got := drainEvents(events)
	// Expected kinds in stream order: session_id (from session line),
	// assistant_text (dispatched), tool_call, tool_result. The "final" line
	// has no dispatch entry and no matching TextField, so emits nothing.
	wantKinds := []provider.EventKind{
		provider.EventSessionID,
		provider.EventAssistantText,
		provider.EventToolCall,
		provider.EventToolResult,
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("event count: got %d, want %d (events=%v)", len(got), len(wantKinds), got)
	}
	for i, ev := range got {
		if ev.Kind != wantKinds[i] {
			t.Errorf("event[%d]: kind=%q, want %q", i, ev.Kind, wantKinds[i])
		}
	}

	// session_id event payload should carry the id.
	var sidPayload map[string]string
	if err := json.Unmarshal(got[0].Payload, &sidPayload); err != nil {
		t.Fatalf("unmarshal session_id payload: %v", err)
	}
	if sidPayload["session_id"] != "sess-123" {
		t.Errorf("session_id payload: got %q, want %q", sidPayload["session_id"], "sess-123")
	}
}

func TestParseStreamJSON_MalformedJSONEmitsStdoutChunk(t *testing.T) {
	p := newStreamJSONProvider(t, nil)
	// First line is well-formed; second is garbage; third is well-formed
	// again — ensures the scanner keeps going after a parse error.
	input := "{\"type\":\"assistant\",\"text\":\"first\"}\nthis is not json {{{\n{\"type\":\"assistant\",\"text\":\"third\"}\n"

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 16)

	sessionID, finalText := p.parseStreamJSON(strings.NewReader(input), &raw, &mu, events)
	close(events)

	if sessionID != "" {
		t.Errorf("session id: got %q, want empty", sessionID)
	}
	if finalText != "" {
		t.Errorf("final text: got %q, want empty", finalText)
	}

	got := drainEvents(events)
	if len(got) != 3 {
		t.Fatalf("event count: got %d, want 3 (events=%v)", len(got), got)
	}
	if got[0].Kind != provider.EventAssistantText {
		t.Errorf("event[0] kind: got %q, want %q", got[0].Kind, provider.EventAssistantText)
	}
	if got[1].Kind != provider.EventStdoutChunk {
		t.Errorf("event[1] kind: got %q, want %q (malformed line should fall back to stdout_chunk)", got[1].Kind, provider.EventStdoutChunk)
	}
	// The stdout_chunk payload is the raw bytes of the malformed line.
	if !bytes.Contains(got[1].Payload, []byte("this is not json")) {
		t.Errorf("stdout_chunk payload missing malformed text: %q", string(got[1].Payload))
	}
	if got[2].Kind != provider.EventAssistantText {
		t.Errorf("event[2] kind: got %q, want %q", got[2].Kind, provider.EventAssistantText)
	}
}

func TestParseStreamJSON_SkipsBlankLines(t *testing.T) {
	p := newStreamJSONProvider(t, nil)
	input := "\n   \n{\"type\":\"assistant\",\"text\":\"only one\"}\n\n\t\n"

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 16)

	p.parseStreamJSON(strings.NewReader(input), &raw, &mu, events)
	close(events)

	got := drainEvents(events)
	if len(got) != 1 {
		t.Fatalf("event count: got %d, want 1 (blank lines should be skipped). events=%v", len(got), got)
	}
	if got[0].Kind != provider.EventAssistantText {
		t.Errorf("kind: got %q, want %q", got[0].Kind, provider.EventAssistantText)
	}
	// Blank lines still tee through to raw buffer (they are line content).
	if !bytes.Contains(raw.Bytes(), []byte("only one")) {
		t.Errorf("raw buffer missing payload: %q", raw.String())
	}
}

func TestParseStreamJSON_MissingOptionalFields(t *testing.T) {
	// Descriptor with no session_id_field / final_text_field configured.
	// Lines that happen to contain those keys should not cause panics, and
	// the returned session/final values should remain empty.
	p := newStreamJSONProvider(t, func(d *config.ProviderDescriptor) {
		d.Output.SessionIDField = ""
		d.Output.FinalTextField = ""
		d.Output.EventDispatch = nil
	})
	input := `{"type":"assistant","text":"hi","session_id":"ignored","final_text":"also-ignored"}` + "\n"

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 8)

	sessionID, finalText := p.parseStreamJSON(strings.NewReader(input), &raw, &mu, events)
	close(events)

	if sessionID != "" {
		t.Errorf("session id: got %q, want empty (no session_id_field configured)", sessionID)
	}
	if finalText != "" {
		t.Errorf("final text: got %q, want empty (no final_text_field configured)", finalText)
	}
	// With no dispatch entry for "assistant", the TextField fallback ("text")
	// should fire and emit an assistant_text event.
	got := drainEvents(events)
	if len(got) != 1 {
		t.Fatalf("event count: got %d, want 1 (TextField fallback). events=%v", len(got), got)
	}
	if got[0].Kind != provider.EventAssistantText {
		t.Errorf("kind: got %q, want %q", got[0].Kind, provider.EventAssistantText)
	}
}

func TestParseStreamJSON_TextFieldFallback(t *testing.T) {
	// No dispatch entry for "msg" type and SessionID present elsewhere.
	// Because TextField defaults to "text", the line should still emit an
	// assistant_text event via the fallback branch.
	p := newStreamJSONProvider(t, func(d *config.ProviderDescriptor) {
		d.Output.EventDispatch = map[string]string{} // empty: forces fallback
		d.Output.TextField = "text"
	})
	input := `{"type":"msg","text":"fallback content"}` + "\n"

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 8)

	p.parseStreamJSON(strings.NewReader(input), &raw, &mu, events)
	close(events)

	got := drainEvents(events)
	if len(got) != 1 {
		t.Fatalf("event count: got %d, want 1", len(got))
	}
	if got[0].Kind != provider.EventAssistantText {
		t.Errorf("kind: got %q, want %q", got[0].Kind, provider.EventAssistantText)
	}
	var payload map[string]string
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["text"] != "fallback content" {
		t.Errorf("payload text: got %q, want %q", payload["text"], "fallback content")
	}
}

func TestParseStreamJSON_DispatchSuppressesTextFallback(t *testing.T) {
	// When a line matches event_dispatch, parseStreamJSON `continue`s and
	// must NOT also emit a TextField fallback event for the same line.
	p := newStreamJSONProvider(t, nil)
	input := `{"type":"assistant","text":"once only"}` + "\n"

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 8)

	p.parseStreamJSON(strings.NewReader(input), &raw, &mu, events)
	close(events)

	got := drainEvents(events)
	if len(got) != 1 {
		t.Fatalf("event count: got %d, want 1 (dispatch should suppress fallback). events=%v", len(got), got)
	}
}

func TestParseStreamJSON_NonStringSessionIDIgnored(t *testing.T) {
	// session_id_field present but value is a number, not a string. The
	// type assertion in parseStreamJSON should fail silently — session
	// remains empty, no session_id event emitted.
	p := newStreamJSONProvider(t, nil)
	input := `{"type":"assistant","text":"x","session_id":42}` + "\n"

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 8)

	sid, _ := p.parseStreamJSON(strings.NewReader(input), &raw, &mu, events)
	close(events)

	if sid != "" {
		t.Errorf("session id: got %q, want empty (non-string value should be ignored)", sid)
	}
	got := drainEvents(events)
	for _, ev := range got {
		if ev.Kind == provider.EventSessionID {
			t.Errorf("unexpected session_id event for non-string value: %v", ev)
		}
	}
}

func TestParseStreamJSON_NilEventsChannelDoesNotPanic(t *testing.T) {
	// emit() must be a no-op when the channel is nil. Confirm parseStreamJSON
	// still extracts session/final values from a healthy stream.
	p := newStreamJSONProvider(t, nil)
	input := strings.Join([]string{
		`{"session_id":"sess-9"}`,
		`{"type":"assistant","text":"hello"}`,
		`{"final_text":"done"}`,
	}, "\n") + "\n"

	var raw bytes.Buffer
	var mu sync.Mutex

	sid, ft := p.parseStreamJSON(strings.NewReader(input), &raw, &mu, nil)
	if sid != "sess-9" {
		t.Errorf("session id: got %q, want %q", sid, "sess-9")
	}
	if ft != "done" {
		t.Errorf("final text: got %q, want %q", ft, "done")
	}
}

func TestParseStreamJSON_LastSessionIDWins(t *testing.T) {
	// Multiple session_id lines: each non-empty value should update the
	// returned sessionID. The CLI may emit a placeholder then a real id.
	p := newStreamJSONProvider(t, nil)
	input := strings.Join([]string{
		`{"session_id":"first"}`,
		`{"session_id":"second"}`,
	}, "\n") + "\n"

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 16)

	sid, _ := p.parseStreamJSON(strings.NewReader(input), &raw, &mu, events)
	close(events)

	if sid != "second" {
		t.Errorf("session id: got %q, want %q", sid, "second")
	}
	got := drainEvents(events)
	sidCount := 0
	for _, ev := range got {
		if ev.Kind == provider.EventSessionID {
			sidCount++
		}
	}
	if sidCount != 2 {
		t.Errorf("session_id event count: got %d, want 2", sidCount)
	}
}

func TestParseStreamJSON_EmptyStringSessionIDIgnored(t *testing.T) {
	// An empty session_id value should not be reported (the check is
	// `ok && v != ""`).
	p := newStreamJSONProvider(t, nil)
	input := `{"session_id":""}` + "\n"

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 8)

	sid, _ := p.parseStreamJSON(strings.NewReader(input), &raw, &mu, events)
	close(events)

	if sid != "" {
		t.Errorf("session id: got %q, want empty", sid)
	}
	for _, ev := range drainEvents(events) {
		if ev.Kind == provider.EventSessionID {
			t.Errorf("unexpected session_id event for empty value: %v", ev)
		}
	}
}

func TestParseStreamJSON_UnknownDispatchKindFallsThrough(t *testing.T) {
	// mapEventKind returns EventStdoutChunk for unknown labels — verify the
	// dispatch path still emits something and uses the catch-all kind.
	p := newStreamJSONProvider(t, func(d *config.ProviderDescriptor) {
		d.Output.EventDispatch = map[string]string{"weird": "made_up_kind"}
	})
	input := `{"type":"weird","payload":1}` + "\n"

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 8)

	p.parseStreamJSON(strings.NewReader(input), &raw, &mu, events)
	close(events)

	got := drainEvents(events)
	if len(got) != 1 {
		t.Fatalf("event count: got %d, want 1", len(got))
	}
	if got[0].Kind != provider.EventStdoutChunk {
		t.Errorf("kind: got %q, want %q (unknown dispatch label maps to stdout_chunk)", got[0].Kind, provider.EventStdoutChunk)
	}
}

// readerThatErrorsMidStream is an io.Reader that returns one good chunk then
// a non-EOF error. We use it to confirm parseStreamJSON tolerates a stream
// that ends abruptly: bufio.Scanner flushes its remaining buffer as a final
// token, and parseStreamJSON must not panic on the malformed trailing line.
type readerThatErrorsMidStream struct {
	data []byte
	pos  int
	done bool
}

func (r *readerThatErrorsMidStream) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	if r.pos >= len(r.data) {
		r.done = true
	}
	return n, nil
}

func TestParseStreamJSON_HandlesReaderError(t *testing.T) {
	p := newStreamJSONProvider(t, nil)
	// One complete line followed by an unterminated partial line; the
	// reader will return io.ErrUnexpectedEOF after delivering the bytes.
	complete := `{"type":"assistant","text":"complete"}` + "\n"
	partial := `{"type":"assistant","text":"partial` // no newline, no closing brace
	r := &readerThatErrorsMidStream{data: []byte(complete + partial)}

	var raw bytes.Buffer
	var mu sync.Mutex
	events := make(chan provider.Event, 16)

	// Must not panic. The first (complete) line should always be emitted as
	// a valid assistant_text event regardless of how the scanner handles the
	// trailing partial chunk.
	p.parseStreamJSON(r, &raw, &mu, events)
	close(events)

	got := drainEvents(events)
	if len(got) == 0 {
		t.Fatalf("expected at least the complete line to be emitted, got 0 events")
	}
	if got[0].Kind != provider.EventAssistantText {
		t.Errorf("first event kind: got %q, want %q", got[0].Kind, provider.EventAssistantText)
	}
	// The complete line must contain the expected text.
	if !bytes.Contains(got[0].Payload, []byte("complete")) {
		t.Errorf("first event payload missing %q: %s", "complete", string(got[0].Payload))
	}
}

// newRenderProvider builds the smallest descriptor renderArgv needs. Detect
// fields are still required by New() but never consulted on this path.
func newRenderProvider(t *testing.T, mutate func(*config.ProviderDescriptor)) *Provider {
	t.Helper()
	desc := &config.ProviderDescriptor{
		Name:   "test",
		Binary: "echo",
		Detect: config.ProviderDescriptorDetect{
			Args:         []string{"--version"},
			VersionRegex: `(\d+\.\d+)`,
		},
		Invocation: config.ProviderDescriptorInvocation{
			Argv: []string{"-p", "{{prompt}}", "--workdir", "{{workdir}}"},
		},
	}
	if mutate != nil {
		mutate(desc)
	}
	p, err := New(desc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestRenderArgv_AppendsMCPConfigWhenPathSet(t *testing.T) {
	p := newRenderProvider(t, func(d *config.ProviderDescriptor) {
		d.Invocation.MCPConfigArgv = []string{"--mcp-config", "{{mcp_config}}"}
	})
	got := p.renderArgv("hi", "", provider.RunOptions{
		Workdir:       "/work",
		MCPConfigPath: "/tmp/uta-mcp.json",
		ExtraArgs:     []string{"--extra"},
	})
	want := []string{"-p", "hi", "--workdir", "/work", "--mcp-config", "/tmp/uta-mcp.json", "--extra"}
	if len(got) != len(want) {
		t.Fatalf("argv length: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRenderArgv_OmitsMCPConfigWhenPathEmpty(t *testing.T) {
	// Even when the descriptor declares an MCP flag shape, an empty
	// MCPConfigPath must NOT inject the flag — otherwise a "--mcp-config "
	// argv would land on the CLI with an empty value and confuse the CLI's
	// own arg parser (claude treats an empty path as a missing file error).
	p := newRenderProvider(t, func(d *config.ProviderDescriptor) {
		d.Invocation.MCPConfigArgv = []string{"--mcp-config", "{{mcp_config}}"}
	})
	got := p.renderArgv("hi", "", provider.RunOptions{Workdir: "/w"})
	for _, a := range got {
		if strings.Contains(a, "mcp") {
			t.Errorf("argv must not contain any mcp arg when MCPConfigPath is empty, got %v", got)
			break
		}
	}
}

func TestRenderArgv_NoMCPConfigArgvLeavesFlagDropped(t *testing.T) {
	// A descriptor that never declares mcp_config_argv must not somehow
	// surface MCPConfigPath through the regular argv template (the var is
	// available but unused — a YAML author who doesn't ask for it gets
	// nothing).
	p := newRenderProvider(t, nil)
	got := p.renderArgv("hi", "", provider.RunOptions{
		Workdir:       "/w",
		MCPConfigPath: "/tmp/uta-mcp.json",
	})
	for _, a := range got {
		if strings.Contains(a, "uta-mcp.json") {
			t.Errorf("argv leaked MCPConfigPath despite empty MCPConfigArgv: %v", got)
		}
	}
}

func TestRenderArgv_ResumeArgvReplacesBase(t *testing.T) {
	// MCPConfigArgv must still be appended to the resume-mode argv, not
	// just the base argv — otherwise a resumed run loses access to MCP
	// servers entirely.
	p := newRenderProvider(t, func(d *config.ProviderDescriptor) {
		d.Invocation.ResumeArgv = []string{"--resume", "{{session_id}}", "-p", "{{prompt}}"}
		d.Invocation.MCPConfigArgv = []string{"--mcp", "{{mcp_config}}"}
	})
	got := p.renderArgv("hi", "sess-7", provider.RunOptions{MCPConfigPath: "/tmp/x.json"})
	want := []string{"--resume", "sess-7", "-p", "hi", "--mcp", "/tmp/x.json"}
	if len(got) != len(want) {
		t.Fatalf("argv length: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
