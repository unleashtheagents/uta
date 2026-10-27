package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
)

func TestAgy_Name(t *testing.T) {
	a := &Agy{}
	if got := a.Name(); got != "agy" {
		t.Fatalf("Name() = %q, want %q", got, "agy")
	}
}

func TestBuildAgyArgs_IncludesResumePreApproveAndMCP(t *testing.T) {
	got := buildAgyArgs("the prompt", "conv-42", provider.RunOptions{
		PreApproveTools: []string{"Bash", "Edit"},
		MCPConfigPath:   "/tmp/uta-mcp.json",
		ExtraArgs:       []string{"--model", "gemini-3-pro"},
	})
	want := []string{
		"-p", "the prompt", "--output-format", "stream-json",
		"--conversation", "conv-42",
		"--allowed-tools", "Bash,Edit",
		"--mcp-config", "/tmp/uta-mcp.json",
		"--model", "gemini-3-pro",
	}
	if len(got) != len(want) {
		t.Fatalf("args = %v\nwant %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAgy_ResumeHeadless_EmptySessionID(t *testing.T) {
	a := &Agy{}
	_, err := a.ResumeHeadless(context.Background(), "", "prompt", provider.RunOptions{}, nil)
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("ResumeHeadless empty sessionID error = %v, want ErrUnsupported", err)
	}
}

func TestParseAgyStream_AccumulatesTextUsageAndEventsWithoutAliasing(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"init","session_id":"agy-sess-1"}`,
		`{"type":"message","role":"user","content":"should be ignored"}`,
		`{"type":"delta","delta":"hello "}`,
		`{"type":"message","role":"assistant","content":"world"}`,
		`{"type":"tool_call","name":"run_command","args":{"cmd":"ls"}}`,
		`{"type":"tool_result","tool_result":{"ok":true}}`,
		`{"type":"result","input_tokens":120,"output_tokens":45}`,
	}, "\n")

	ch := make(chan provider.Event, 16)
	res := parseAgyStream(strings.NewReader(stream), ch)

	if res.SessionID != "agy-sess-1" {
		t.Errorf("SessionID = %q, want agy-sess-1", res.SessionID)
	}
	if res.FinalText != "hello world" {
		t.Errorf("FinalText = %q, want %q", res.FinalText, "hello world")
	}
	if res.TokensIn != 120 || res.TokensOut != 45 {
		t.Errorf("Tokens = (%d, %d), want (120, 45)", res.TokensIn, res.TokensOut)
	}

	evs := drainEvents(ch)
	if len(evs) != 5 {
		t.Fatalf("len(events) = %d, want 5: %+v", len(evs), evs)
	}
	for i, ev := range evs {
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			t.Errorf("event[%d] (%s) payload invalid JSON %q: %v", i, ev.Kind, string(ev.Payload), err)
		}
	}
}
