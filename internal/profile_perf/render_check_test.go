package profileperf

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestRender_80ColMaxWidth proves both the single-session and aggregate
// reports stay within 80 columns even with verbose ops-mode inputs (long
// MCP tool names, long subtask titles, long gate commands). The audit
// specifically asked us to "run it and look at 80 columns" — this is the
// automated version of that.
func TestRender_80ColMaxWidth(t *testing.T) {
	check := func(t *testing.T, out string) {
		t.Helper()
		for i, line := range strings.Split(out, "\n") {
			if n := len(line); n > 80 {
				t.Errorf("line %d width=%d > 80: %q", i, n, line)
			}
		}
	}

	b := Breakdown{
		SessionID: "abcdef0123456789-extra",
		Goal:      "A reasonably long goal line that might span words but should stay under 100 chars total",
		Status:    "completed", Worker: "claude", ModeName: "ops",
		WallClock: 2*time.Minute + 13*time.Second,
		Planner:   Phase{Duration: 1300 * time.Millisecond, Count: 1},
		Subtasks: []SubtaskPhase{
			{SubtaskID: "stb12345abc", SpecID: "s1", Title: "Refactor latency profiling and ensure 80 column rendering for MCP-heavy ops mode", Worker: "claude", Status: "completed", Duration: 25 * time.Second},
			{SubtaskID: "stc67890def", SpecID: "s2", Title: "Add MCP/builtin split", Worker: "claude", Status: "failed", Duration: 14 * time.Second},
		},
		SubtasksTotal: 39 * time.Second,
		Synthesis:     Phase{Duration: 800 * time.Millisecond, Count: 1},
		Gates: []GatePhase{
			{SubtaskID: "stb12345abc", Cmd: "go test ./... -race && go build ./... && go vet ./... && echo done", Status: "passed", Duration: 11 * time.Second},
		},
		GatesTotal: 11 * time.Second,
		ProviderTools: []ToolUsage{
			{Tool: "mcp__claude_ai_Gmail__search_threads", Kind: "mcp", Count: 7, Duration: 9 * time.Second},
			{Tool: "Read", Kind: "builtin", Count: 14, Duration: 800 * time.Millisecond},
			{Tool: "Bash", Kind: "builtin", Count: 3, Duration: 200 * time.Millisecond},
		},
		ProviderToolTotal: 10 * time.Second,
		BuiltinToolTotal:  1 * time.Second,
		MCPToolTotal:      9 * time.Second,
		CPUEquivalent:     53 * time.Second,
	}
	var buf bytes.Buffer
	RenderSession(&buf, b)
	check(t, buf.String())

	// Aggregate view: long MCP tool names in the "Top provider tools"
	// table must also wrap cleanly.
	agg := BuildAggregate([]Breakdown{b})
	buf.Reset()
	RenderAggregate(&buf, agg, "ops", time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC))
	check(t, buf.String())
}
