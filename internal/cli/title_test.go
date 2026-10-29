package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestFormatWarpAgentOSC777(t *testing.T) {
	got := formatWarpAgentOSC777(warpAgentEvent{
		Event:     "stop",
		SessionID: "abc12345",
		Cwd:       "/tmp/work",
		Query:     "plan and fan out",
	})
	want := "\x1b]777;warp://cli-agent;{\"v\":1,\"agent\":\"uta\",\"event\":\"stop\",\"session_id\":\"abc12345\",\"cwd\":\"/tmp/work\",\"query\":\"plan and fan out\"}\x07"
	if got != want {
		t.Fatalf("formatWarpAgentOSC777 = %q, want %q", got, want)
	}
}

func TestEmitWarpAgentEventNonTTYIsNoop(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "WarpTerminal")
	var buf bytes.Buffer
	emitWarpAgentEvent(&buf, warpAgentEvent{Event: "stop"})
	if buf.Len() != 0 {
		t.Fatalf("expected non-TTY writer to stay byte-clean, got %q", buf.String())
	}
}

func TestFormatWarpAgentOSC777TruncatesLongFields(t *testing.T) {
	long := strings.Repeat("x", 800)
	got := formatWarpAgentOSC777(warpAgentEvent{
		Event:    "stop",
		Query:    long,
		Response: long,
	})
	if len(got) > 1200 {
		t.Fatalf("expected truncated OSC 777 frame, got len=%d", len(got))
	}
}
