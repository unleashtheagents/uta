package profileperf

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// fixtureBuilder is a tiny helper for stamping ordered events with monotonic
// timestamps starting from a fixed base. The "advance" duration moves the
// clock forward between events.
type fixtureBuilder struct {
	base time.Time
	seq  int64
	out  []store.EventRow
}

func newFixture() *fixtureBuilder {
	return &fixtureBuilder{base: time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)}
}

func (f *fixtureBuilder) at(d time.Duration) time.Time { return f.base.Add(d) }

func (f *fixtureBuilder) emit(t time.Time, kind trajectory.Kind, subtaskID string, payload map[string]any) {
	f.seq++
	raw, _ := json.Marshal(payload)
	if payload == nil {
		raw = []byte(`{}`)
	}
	f.out = append(f.out, store.EventRow{
		ID: f.seq, SessionID: "sess-test", SubtaskID: subtaskID, Seq: f.seq,
		Ts: t, Kind: string(kind), Payload: raw,
	})
}

// TestBuildSessionBreakdown_Comprehensive covers the acceptance case
// described in the idea: a run with 5 subtasks (one is failed) and 3
// gate executions, plus a planner + synthesis pair and a per-tool
// breakdown derived from SubtaskToolCall/Result pairing.
func TestBuildSessionBreakdown_Comprehensive(t *testing.T) {
	f := newFixture()

	// Planner: 200ms.
	f.emit(f.at(0), trajectory.PlanRequested, "", map[string]any{"planner": "claude"})
	f.emit(f.at(200*time.Millisecond), trajectory.PlanProposed, "", map[string]any{"subtasks": 5})

	// 5 subtasks. We run them sequentially in the fixture, so the
	// engine's parallel semantics don't matter for the breakdown math.
	startOffset := 200 * time.Millisecond
	for i, id := range []string{"st1", "st2", "st3", "st4", "st5"} {
		base := startOffset + time.Duration(i)*time.Second
		f.emit(f.at(base), trajectory.SubtaskStarted, id, map[string]any{
			"spec_id": "s" + string(rune('1'+i)),
			"title":   "subtask " + id,
			"worker":  "claude",
		})
		// One tool call inside each subtask: 100ms each.
		f.emit(f.at(base+200*time.Millisecond), trajectory.SubtaskToolCall, id, map[string]any{"name": "Read"})
		f.emit(f.at(base+300*time.Millisecond), trajectory.SubtaskToolResult, id, map[string]any{"tool_use_id": "x"})
		if i == 2 {
			// Third subtask fails.
			f.emit(f.at(base+800*time.Millisecond), trajectory.SubtaskFailed, id, map[string]any{
				"spec_id": "s3", "error": "boom",
			})
			continue
		}
		f.emit(f.at(base+800*time.Millisecond), trajectory.SubtaskCompleted, id, map[string]any{"spec_id": "s" + string(rune('1'+i))})
	}

	// 3 gate executions on subtask st1. Each gate is 150ms; 2 pass, 1 fail.
	gateBase := startOffset + 6*time.Second
	for i := 0; i < 3; i++ {
		ts := gateBase + time.Duration(i)*300*time.Millisecond
		f.emit(f.at(ts), trajectory.GateStarted, "st1", map[string]any{"cmd": "go test ./...", "attempt": i})
		evKind := trajectory.GatePassed
		if i == 1 {
			evKind = trajectory.GateFailed
		}
		f.emit(f.at(ts+150*time.Millisecond), evKind, "st1", map[string]any{"cmd": "go test ./...", "exit": 0})
	}

	// Synthesis: 500ms.
	synthStart := gateBase + 1500*time.Millisecond
	f.emit(f.at(synthStart), trajectory.SynthesisStarted, "", map[string]any{"synth": "default"})
	f.emit(f.at(synthStart+500*time.Millisecond), trajectory.SynthesisCompleted, "", map[string]any{"chars": 42})

	f.emit(f.at(synthStart+550*time.Millisecond), trajectory.RunCompleted, "", map[string]any{"status": "completed"})

	sess := store.Session{ID: "sess-test", Goal: "test", Worker: "claude", Status: "completed", ModeName: "dev"}
	b := BuildSessionBreakdown(sess, f.out)

	if got, want := b.Planner.Duration, 200*time.Millisecond; got != want {
		t.Errorf("planner duration: got %s want %s", got, want)
	}
	if got, want := b.Planner.Count, 1; got != want {
		t.Errorf("planner count: got %d want %d", got, want)
	}
	if got, want := len(b.Subtasks), 5; got != want {
		t.Errorf("subtasks count: got %d want %d", got, want)
	}
	if got, want := b.SubtasksTotal, 5*800*time.Millisecond; got != want {
		t.Errorf("subtasks total: got %s want %s", got, want)
	}
	// One failed subtask should be flagged.
	failed := 0
	for _, s := range b.Subtasks {
		if s.Status == "failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("expected exactly 1 failed subtask, got %d", failed)
	}
	if got, want := len(b.Gates), 3; got != want {
		t.Errorf("gates count: got %d want %d", got, want)
	}
	passed, gateFailed := 0, 0
	for _, g := range b.Gates {
		switch g.Status {
		case "passed":
			passed++
		case "failed":
			gateFailed++
		}
	}
	if passed != 2 || gateFailed != 1 {
		t.Errorf("gate passed/failed: got %d/%d want 2/1", passed, gateFailed)
	}
	if got, want := b.GatesTotal, 3*150*time.Millisecond; got != want {
		t.Errorf("gates total: got %s want %s", got, want)
	}
	if got, want := b.Synthesis.Duration, 500*time.Millisecond; got != want {
		t.Errorf("synthesis duration: got %s want %s", got, want)
	}
	// Provider tools: 5 Read calls, 100ms each → 500ms total.
	if len(b.ProviderTools) != 1 {
		t.Fatalf("provider tools entries: got %d want 1", len(b.ProviderTools))
	}
	if got, want := b.ProviderTools[0].Tool, "Read"; got != want {
		t.Errorf("provider tool name: got %q want %q", got, want)
	}
	if got, want := b.ProviderTools[0].Count, 5; got != want {
		t.Errorf("provider tool count: got %d want %d", got, want)
	}
	if got, want := b.ProviderTools[0].Duration, 5*100*time.Millisecond; got != want {
		t.Errorf("provider tool total: got %s want %s", got, want)
	}
	// CPU equivalent = planner + subtasks + synthesis + gates + tools.
	wantCPU := 200*time.Millisecond + 5*800*time.Millisecond + 500*time.Millisecond + 3*150*time.Millisecond
	if got := b.CPUEquivalent; got != wantCPU {
		t.Errorf("cpu-equivalent: got %s want %s", got, wantCPU)
	}
}

func TestBuildSessionBreakdown_RunningSubtask(t *testing.T) {
	f := newFixture()
	f.emit(f.at(0), trajectory.SubtaskStarted, "st1", map[string]any{"title": "hung"})
	// No SubtaskCompleted/Failed — emulates a session that was killed
	// mid-flight. The breakdown should assign (end - start) as the
	// duration and mark it as running.
	f.emit(f.at(2*time.Second), trajectory.SentinelAlert, "", map[string]any{})

	b := BuildSessionBreakdown(store.Session{ID: "x"}, f.out)
	if len(b.Subtasks) != 1 {
		t.Fatalf("expected 1 subtask phase, got %d", len(b.Subtasks))
	}
	if b.Subtasks[0].Status != "running" {
		t.Errorf("expected status=running, got %q", b.Subtasks[0].Status)
	}
	if got, want := b.Subtasks[0].Duration, 2*time.Second; got != want {
		t.Errorf("running subtask duration: got %s want %s", got, want)
	}
	if b.SentinelAlerts != 1 {
		t.Errorf("expected 1 sentinel alert, got %d", b.SentinelAlerts)
	}
}

func TestBuildSessionBreakdown_SynthesisSkip(t *testing.T) {
	f := newFixture()
	f.emit(f.at(0), trajectory.SynthesisCompleted, "", map[string]any{"mode": "skip", "chars": 0})
	b := BuildSessionBreakdown(store.Session{ID: "x"}, f.out)
	if b.Synthesis.Count != 1 {
		t.Errorf("expected synthesis count 1 (skip path), got %d", b.Synthesis.Count)
	}
	if b.Synthesis.Duration != 0 {
		t.Errorf("expected synthesis duration 0 on skip, got %s", b.Synthesis.Duration)
	}
}

func TestBuildSessionBreakdown_ExternalTools(t *testing.T) {
	f := newFixture()
	f.emit(f.at(0), trajectory.ToolStarted, "tt1", map[string]any{"tool": "slither"})
	f.emit(f.at(750*time.Millisecond), trajectory.ToolCompleted, "tt1", map[string]any{"tool": "slither", "findings": 3})
	f.emit(f.at(800*time.Millisecond), trajectory.ToolStarted, "tt2", map[string]any{"tool": "forge-test"})
	f.emit(f.at(900*time.Millisecond), trajectory.ToolFailed, "tt2", map[string]any{"tool": "forge-test", "error": "exit 1"})

	b := BuildSessionBreakdown(store.Session{ID: "x"}, f.out)
	if len(b.ExternalTools) != 2 {
		t.Fatalf("expected 2 external tools, got %d", len(b.ExternalTools))
	}
	if b.ToolsTotal != 850*time.Millisecond {
		t.Errorf("external tools total: got %s want 850ms", b.ToolsTotal)
	}
}

func TestBuildAggregate(t *testing.T) {
	mk := func(planner, sub, synth, gate time.Duration, tool string, calls int, toolDur time.Duration) Breakdown {
		return Breakdown{
			WallClock:         planner + sub + synth + gate,
			Planner:           Phase{Duration: planner, Count: 1},
			SubtasksTotal:     sub,
			Synthesis:         Phase{Duration: synth, Count: 1},
			GatesTotal:        gate,
			ProviderTools:     []ToolUsage{{Tool: tool, Count: calls, Duration: toolDur}},
			ProviderToolTotal: toolDur,
			CPUEquivalent:     planner + sub + synth + gate,
		}
	}
	in := []Breakdown{
		mk(1*time.Second, 5*time.Second, 200*time.Millisecond, 100*time.Millisecond, "Read", 3, 600*time.Millisecond),
		mk(2*time.Second, 10*time.Second, 300*time.Millisecond, 200*time.Millisecond, "Bash", 1, 4*time.Second),
		mk(500*time.Millisecond, 2*time.Second, 0, 0, "Read", 2, 400*time.Millisecond),
	}
	agg := BuildAggregate(in)
	if agg.Sessions != 3 {
		t.Errorf("sessions: got %d want 3", agg.Sessions)
	}
	if agg.PlannerTime != 3500*time.Millisecond {
		t.Errorf("planner total: got %s want 3.5s", agg.PlannerTime)
	}
	if agg.SubtasksTime != 17*time.Second {
		t.Errorf("subtasks total: got %s want 17s", agg.SubtasksTime)
	}
	// TopTools merges Read across sessions (3+2=5 calls, 1s total) and Bash (1 call, 4s).
	// Sorted by duration desc → Bash first.
	if len(agg.TopTools) != 2 {
		t.Fatalf("top tools: got %d want 2", len(agg.TopTools))
	}
	if agg.TopTools[0].Tool != "Bash" {
		t.Errorf("top tool[0]: got %q want Bash", agg.TopTools[0].Tool)
	}
	if agg.TopTools[1].Tool != "Read" || agg.TopTools[1].Count != 5 {
		t.Errorf("top tool[1]: got %+v want Read x5", agg.TopTools[1])
	}
}

func TestClassifyTool(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty defaults to builtin", "", ToolKindBuiltin},
		{"builtin Read", "Read", ToolKindBuiltin},
		{"builtin Bash", "Bash", ToolKindBuiltin},
		{"mcp Gmail search_threads", "mcp__claude_ai_Gmail__search_threads", ToolKindMCP},
		{"mcp short", "mcp__a__b", ToolKindMCP},
		// Partial prefixes must NOT be classified as MCP — only the exact
		// "mcp__" sentinel counts.
		{"single underscore is not mcp", "mcp_foo", ToolKindBuiltin},
		{"prefix-only no payload still mcp by convention", "mcp__", ToolKindMCP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyTool(tc.in); got != tc.want {
				t.Errorf("ClassifyTool(%q)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildSessionBreakdown_MCPSplit asserts that mixed builtin + MCP
// tool calls accumulate into the correct kind bucket so the renderer
// can show "how much of the tool budget went to MCP servers."
func TestBuildSessionBreakdown_MCPSplit(t *testing.T) {
	f := newFixture()
	// One subtask with 2 builtin calls (Read: 100ms each) and 1 MCP call
	// (Gmail search: 400ms).
	f.emit(f.at(0), trajectory.SubtaskStarted, "st1", map[string]any{"title": "ops"})
	f.emit(f.at(100*time.Millisecond), trajectory.SubtaskToolCall, "st1", map[string]any{"name": "Read"})
	f.emit(f.at(200*time.Millisecond), trajectory.SubtaskToolResult, "st1", nil)
	f.emit(f.at(300*time.Millisecond), trajectory.SubtaskToolCall, "st1", map[string]any{"name": "Read"})
	f.emit(f.at(400*time.Millisecond), trajectory.SubtaskToolResult, "st1", nil)
	f.emit(f.at(500*time.Millisecond), trajectory.SubtaskToolCall, "st1", map[string]any{"name": "mcp__claude_ai_Gmail__search_threads"})
	f.emit(f.at(900*time.Millisecond), trajectory.SubtaskToolResult, "st1", nil)
	f.emit(f.at(1*time.Second), trajectory.SubtaskCompleted, "st1", nil)

	b := BuildSessionBreakdown(store.Session{ID: "x"}, f.out)
	if got, want := b.BuiltinToolTotal, 200*time.Millisecond; got != want {
		t.Errorf("BuiltinToolTotal: got %s want %s", got, want)
	}
	if got, want := b.MCPToolTotal, 400*time.Millisecond; got != want {
		t.Errorf("MCPToolTotal: got %s want %s", got, want)
	}
	if got, want := b.ProviderToolTotal, 600*time.Millisecond; got != want {
		t.Errorf("ProviderToolTotal: got %s want %s", got, want)
	}
	var sawMCP, sawBuiltin bool
	for _, u := range b.ProviderTools {
		if u.Tool == "Read" && u.Kind == ToolKindBuiltin {
			sawBuiltin = true
		}
		if u.Tool == "mcp__claude_ai_Gmail__search_threads" && u.Kind == ToolKindMCP {
			sawMCP = true
		}
	}
	if !sawBuiltin || !sawMCP {
		t.Errorf("expected both builtin and mcp kinds in ProviderTools; got %+v", b.ProviderTools)
	}

	// Aggregate must also carry the split.
	agg := BuildAggregate([]Breakdown{b})
	if agg.BuiltinToolTime != 200*time.Millisecond {
		t.Errorf("agg.BuiltinToolTime: got %s want 200ms", agg.BuiltinToolTime)
	}
	if agg.MCPToolTime != 400*time.Millisecond {
		t.Errorf("agg.MCPToolTime: got %s want 400ms", agg.MCPToolTime)
	}
}

func TestRenderSession_Smoke(t *testing.T) {
	b := Breakdown{
		SessionID: "abcdef0123456789", Worker: "claude", Status: "completed", ModeName: "dev",
		WallClock:         12 * time.Second,
		Planner:           Phase{Duration: 1 * time.Second, Count: 1},
		Subtasks:          []SubtaskPhase{{SubtaskID: "st1", SpecID: "s1", Title: "one", Worker: "claude", Status: "completed", Duration: 4 * time.Second}},
		SubtasksTotal:     4 * time.Second,
		Synthesis:         Phase{Duration: 500 * time.Millisecond, Count: 1},
		Gates:             []GatePhase{{Cmd: "go test", Status: "passed", Duration: 200 * time.Millisecond}},
		GatesTotal:        200 * time.Millisecond,
		ProviderTools:     []ToolUsage{{Tool: "Read", Count: 4, Duration: 1 * time.Second}},
		ProviderToolTotal: 1 * time.Second,
		CPUEquivalent:     5*time.Second + 1*time.Second + 500*time.Millisecond + 200*time.Millisecond + 1*time.Second,
	}
	var buf bytes.Buffer
	RenderSession(&buf, b)
	out := buf.String()
	for _, want := range []string{"planner", "subtasks", "synthesis", "gates", "Read", "parallel-speedup"} {
		if !strings.Contains(out, want) {
			t.Errorf("render output missing %q\n%s", want, out)
		}
	}
}

func TestRenderAggregate_Smoke(t *testing.T) {
	agg := Aggregate{
		Sessions:      2,
		WallClock:     20 * time.Second,
		PlannerTime:   2 * time.Second,
		SubtasksTime:  15 * time.Second,
		SynthesisTime: 1 * time.Second,
		CPUEquivalent: 18 * time.Second,
		TopTools:      []ToolUsage{{Tool: "Read", Count: 10, Duration: 5 * time.Second}},
	}
	var buf bytes.Buffer
	RenderAggregate(&buf, agg, "dev", time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC))
	out := buf.String()
	for _, want := range []string{"aggregate", "mode=dev", "Top provider tools", "Read"} {
		if !strings.Contains(out, want) {
			t.Errorf("aggregate output missing %q\n%s", want, out)
		}
	}
}
