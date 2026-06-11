package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/trajectory"
)

// newTestRenderer constructs a Renderer writing to an in-memory buffer with
// color forced off (via forceNoColor=true) and the writer not being a *os.File
// — so the autodetect TTY branch in NewRenderer also evaluates safely on
// CI/headless environments. Returns the buffer so callers can assert output.
func newTestRenderer(t *testing.T) (*Renderer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)
	if r.color {
		t.Fatalf("expected color disabled when forceNoColor=true")
	}
	return r, &buf
}

// TestNewRenderer_ColorRules covers the three ways color is disabled:
// 1) the caller passes forceNoColor=true (e.g. --no-color flag),
// 2) the NO_COLOR env var is set (https://no-color.org/), and
// 3) the writer is a *os.File whose FD is not a terminal.
// A plain bytes.Buffer satisfies neither the env nor TTY branch, so the only
// thing that flips color off there is forceNoColor.
func TestNewRenderer_ColorRules(t *testing.T) {
	t.Run("force_no_color", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		var buf bytes.Buffer
		r := NewRenderer(&buf, true)
		if r.color {
			t.Errorf("forceNoColor=true should disable color")
		}
	})
	t.Run("env_no_color", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		var buf bytes.Buffer
		r := NewRenderer(&buf, false)
		if r.color {
			t.Errorf("NO_COLOR env should disable color")
		}
	})
	t.Run("non_tty_file_falls_back", func(t *testing.T) {
		// Open a real file — its FD isn't a terminal in the test
		// process, so the isatty branch should clear color.
		t.Setenv("NO_COLOR", "")
		path := filepath.Join(t.TempDir(), "out.log")
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create temp file: %v", err)
		}
		t.Cleanup(func() { _ = f.Close() })
		r := NewRenderer(f, false)
		if r.color {
			t.Errorf("expected color disabled when writer is a non-TTY *os.File")
		}
	})
	t.Run("buffer_writer_stays_color", func(t *testing.T) {
		// A bytes.Buffer is neither a *os.File nor blocked by env, so when
		// the caller explicitly opts in (forceNoColor=false, NO_COLOR unset),
		// color stays on.
		t.Setenv("NO_COLOR", "")
		var buf bytes.Buffer
		r := NewRenderer(&buf, false)
		if !r.color {
			t.Errorf("expected color enabled for non-file writer with no env override")
		}
	})
}

// TestRenderer_ColorHelpersNoColor confirms the color helpers are pure
// passthroughs when r.color is false — no ANSI escapes leak into plain-text
// output (CI logs, pipes, etc.).
func TestRenderer_ColorHelpersNoColor(t *testing.T) {
	r, _ := newTestRenderer(t)
	cases := []struct {
		name string
		got  string
	}{
		{"green", r.green("x")},
		{"red", r.red("x")},
		{"yellow", r.yellow("x")},
		{"cyan", r.cyan("x")},
		{"bold", r.bold("x")},
		{"dim", r.dim("x")},
	}
	for _, c := range cases {
		if c.got != "x" {
			t.Errorf("%s with color off = %q, want %q", c.name, c.got, "x")
		}
		if strings.Contains(c.got, "\x1b[") {
			t.Errorf("%s with color off leaked ANSI escape: %q", c.name, c.got)
		}
	}
}

// TestRenderer_ColorHelpersWithColor exercises the wrapping branch so the
// ANSI escape codes are actually applied when color is enabled.
func TestRenderer_ColorHelpersWithColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)
	if !r.color {
		t.Skip("color autodetect disabled in this environment; skipping")
	}
	got := r.green("ok")
	if !strings.Contains(got, ansiGreen) || !strings.HasSuffix(got, ansiReset) {
		t.Errorf("green('ok') = %q, want it wrapped with ansiGreen+ansiReset", got)
	}
}

// TestShowGoal_FormatsAndTruncates verifies the goal banner: it strips
// newlines (collapses to a single line), truncates overlong goals to 95
// chars + ellipsis, and renders the worker= sub-line. We use NoColor so we
// can assert plain substrings rather than ANSI-wrapped fragments.
func TestShowGoal_FormatsAndTruncates(t *testing.T) {
	r, buf := newTestRenderer(t)
	long := strings.Repeat("a", 200) + "\nmore"
	r.ShowGoal(long, "claude-default")
	out := buf.String()

	// First line: → + bolded goal. The total goal portion must be ≤96 chars
	// (95 + ellipsis) and must not contain a literal newline mid-goal.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (goal + worker), got %d: %q", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "→ ") {
		t.Errorf("goal line should start with arrow, got %q", lines[0])
	}
	if !strings.HasSuffix(lines[0], "…") {
		t.Errorf("oversized goal should end with ellipsis, got %q", lines[0])
	}
	// Body of the goal (without arrow prefix) should be exactly 96 runes:
	// 95 'a' chars + the ellipsis rune.
	body := strings.TrimPrefix(lines[0], "→ ")
	if got := runeLen(body); got != 96 {
		t.Errorf("truncated goal rune length = %d, want 96", got)
	}
	if !strings.Contains(lines[1], "worker=claude-default") {
		t.Errorf("worker line missing worker name: %q", lines[1])
	}
}

// TestShowGoal_ShortGoalUntouched confirms short goals pass through as-is —
// no truncation, no ellipsis — so the common case stays readable.
func TestShowGoal_ShortGoalUntouched(t *testing.T) {
	r, buf := newTestRenderer(t)
	r.ShowGoal("ship it", "w")
	out := buf.String()
	if !strings.Contains(out, "→ ship it\n") {
		t.Errorf("expected verbatim short goal, got %q", out)
	}
	if strings.Contains(out, "…") {
		t.Errorf("short goal should not be truncated: %q", out)
	}
}

// TestHandle_PlanFlow walks the GoalReceived → PlanProposed → SubtaskStarted
// → SubtaskCompleted → SynthesisStarted → SynthesisCompleted → RunCompleted
// sequence to exercise the happy-path phase transitions: each end-of-phase
// closes with ✓, subtask events emit dots, and the run terminates with
// "Done in …". Plain-text mode (color off) keeps assertions simple.
func TestHandle_PlanFlow(t *testing.T) {
	r, buf := newTestRenderer(t)

	emit := func(k trajectory.Kind, payload any) {
		var raw json.RawMessage
		if payload != nil {
			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal %s: %v", k, err)
			}
			raw = b
		}
		r.handle(trajectory.Event{Kind: k, Ts: time.Now(), Payload: raw})
	}

	emit(trajectory.GoalReceived, nil)
	emit(trajectory.PlanProposed, map[string]any{
		"subtasks": []map[string]any{{"id": "a"}, {"id": "b"}},
		"source":   "workflow-yaml",
	})
	emit(trajectory.SubtaskStarted, nil)
	emit(trajectory.SubtaskCompleted, nil)
	emit(trajectory.SubtaskCompleted, nil)
	emit(trajectory.SynthesisStarted, nil)
	emit(trajectory.SynthesisCompleted, map[string]any{"chars": 1234, "mode": "summarize"})
	emit(trajectory.RunCompleted, map[string]any{"status": "ok", "subtasks": 2})

	out := buf.String()
	for _, want := range []string{
		"Planning",
		"2 subtasks (from workflow)",
		"Running 2 subtasks",
		"Synthesizing",
		"1234 chars",
		"Done in",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, out)
		}
	}
	// Two SubtaskCompleted events should each render a bullet marker.
	if got := strings.Count(out, "•"); got < 2 {
		t.Errorf("expected ≥2 subtask-completion markers, got %d\nout: %s", got, out)
	}
}

// TestHandle_PlanProposedUnparseable feeds a deliberately-malformed payload
// to the PlanProposed branch. The renderer must not panic; it should fall
// back to a warning-suffixed phase close and still open the "Running
// subtasks" phase so subsequent dots have a home.
func TestHandle_PlanProposedUnparseable(t *testing.T) {
	r, buf := newTestRenderer(t)
	r.handle(trajectory.Event{Kind: trajectory.GoalReceived, Ts: time.Now()})
	r.handle(trajectory.Event{
		Kind:    trajectory.PlanProposed,
		Ts:      time.Now(),
		Payload: json.RawMessage(`not-json-at-all`),
	})
	out := buf.String()
	if !strings.Contains(out, "plan payload unparseable") {
		t.Errorf("expected unparseable-plan warning, got:\n%s", out)
	}
	if !strings.Contains(out, "Running subtasks") {
		t.Errorf("expected fallback 'Running subtasks' phase, got:\n%s", out)
	}
}

// TestHandle_SynthesisCompletedUnparseable confirms graceful handling when
// the synthesis-completed payload can't be decoded. The renderer must close
// the current phase with a yellow warning suffix rather than panicking on
// the nil unmarshal target.
func TestHandle_SynthesisCompletedUnparseable(t *testing.T) {
	r, buf := newTestRenderer(t)
	r.handle(trajectory.Event{Kind: trajectory.SynthesisStarted, Ts: time.Now()})
	r.handle(trajectory.Event{
		Kind:    trajectory.SynthesisCompleted,
		Ts:      time.Now(),
		Payload: json.RawMessage(`{`),
	})
	out := buf.String()
	if !strings.Contains(out, "synth payload unparseable") {
		t.Errorf("expected synth payload warning, got:\n%s", out)
	}
}

// TestHandle_RunCompletedPartial verifies the "partial" branch — at least
// one subtask failed but the run reached completion. The output should
// surface the ◐ marker and word "Partial" with the subtask count so an
// operator scanning logs can tell at a glance.
func TestHandle_RunCompletedPartial(t *testing.T) {
	r, buf := newTestRenderer(t)
	r.handle(trajectory.Event{
		Kind:    trajectory.RunCompleted,
		Ts:      time.Now(),
		Payload: mustJSON(t, map[string]any{"status": "partial", "subtasks": 3}),
	})
	out := buf.String()
	if !strings.Contains(out, "◐") {
		t.Errorf("partial run should render ◐ marker; got: %s", out)
	}
	if !strings.Contains(out, "Partial: 3 subtasks") {
		t.Errorf("partial summary missing subtask count; got: %s", out)
	}
}

// TestHandle_RunFailed verifies the failure terminal: regardless of any
// open phase, the renderer closes it (without crashing) and prints a
// red ✗ "Run failed" banner.
func TestHandle_RunFailed(t *testing.T) {
	r, buf := newTestRenderer(t)
	// Open a phase first so the endPhaseLocked branch runs.
	r.handle(trajectory.Event{Kind: trajectory.GoalReceived, Ts: time.Now()})
	r.handle(trajectory.Event{Kind: trajectory.RunFailed, Ts: time.Now()})
	out := buf.String()
	if !strings.Contains(out, "✗") || !strings.Contains(out, "Run failed") {
		t.Errorf("expected failure banner, got:\n%s", out)
	}
}

// TestHandle_RunCancelled is the user-cancellation counterpart to RunFailed:
// yellow × marker and "Run cancelled" wording.
func TestHandle_RunCancelled(t *testing.T) {
	r, buf := newTestRenderer(t)
	r.handle(trajectory.Event{Kind: trajectory.GoalReceived, Ts: time.Now()})
	r.handle(trajectory.Event{Kind: trajectory.RunCancelled, Ts: time.Now()})
	out := buf.String()
	if !strings.Contains(out, "×") || !strings.Contains(out, "Run cancelled") {
		t.Errorf("expected cancellation banner, got:\n%s", out)
	}
}

// TestHandle_PlanFallback exercises the planner-output-unparseable bus event
// (distinct from a malformed PlanProposed payload). The renderer should emit
// the fallback warning and open a single-subtask phase.
func TestHandle_PlanFallback(t *testing.T) {
	r, buf := newTestRenderer(t)
	r.handle(trajectory.Event{Kind: trajectory.GoalReceived, Ts: time.Now()})
	r.handle(trajectory.Event{Kind: trajectory.PlanFallback, Ts: time.Now()})
	out := buf.String()
	if !strings.Contains(out, "planner output unparseable") {
		t.Errorf("expected planner-fallback warning, got:\n%s", out)
	}
	if !strings.Contains(out, "Running 1 subtask") {
		t.Errorf("expected fallback subtask phase, got:\n%s", out)
	}
}

// TestHandle_CapabilityGateDeniedAndFailures covers the in-phase markers for
// denied tool calls (yellow x) and failed subtasks (red !). Both depend on
// an active phase, so we open Planning first.
func TestHandle_CapabilityGateDeniedAndFailures(t *testing.T) {
	r, buf := newTestRenderer(t)
	r.handle(trajectory.Event{Kind: trajectory.GoalReceived, Ts: time.Now()})
	r.handle(trajectory.Event{Kind: trajectory.CapabilityGateDenied, Ts: time.Now()})
	r.handle(trajectory.Event{Kind: trajectory.SubtaskFailed, Ts: time.Now()})
	out := buf.String()
	if !strings.Contains(out, "x") {
		t.Errorf("expected capability-denied marker 'x', got:\n%s", out)
	}
	if !strings.Contains(out, "!") {
		t.Errorf("expected subtask-failed marker '!', got:\n%s", out)
	}
}

// TestHandle_DotMarkersIgnoredWithoutPhase guards the dotLocked early-return:
// when no phase is open, streaming events must not write anything (the
// renderer's contract is that dots only appear inside an active phase line).
func TestHandle_DotMarkersIgnoredWithoutPhase(t *testing.T) {
	r, buf := newTestRenderer(t)
	for _, k := range []trajectory.Kind{
		trajectory.SubtaskStdout,
		trajectory.SubtaskAssistantText,
		trajectory.SubtaskToolCall,
		trajectory.SubtaskToolResult,
		trajectory.SubtaskCompleted,
		trajectory.SubtaskFailed,
		trajectory.CapabilityGateDenied,
	} {
		r.handle(trajectory.Event{Kind: k, Ts: time.Now()})
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for dot events without an active phase, got %q", buf.String())
	}
}

// TestSubscribe_DrainsAndFinishes wires the renderer to a real Bus, publishes
// a minimal happy-path sequence, then shuts the bus down. The done channel
// must close, proving the goroutine drains cleanly and finish() runs.
func TestSubscribe_DrainsAndFinishes(t *testing.T) {
	r, buf := newTestRenderer(t)
	bus := trajectory.NewBus()
	done := r.Subscribe(bus)

	bus.Publish(trajectory.Event{Kind: trajectory.GoalReceived, Ts: time.Now()})
	bus.Publish(trajectory.Event{
		Kind:    trajectory.RunCompleted,
		Ts:      time.Now(),
		Payload: mustJSON(t, map[string]any{"status": "ok", "subtasks": 0}),
	})
	bus.Shutdown()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("renderer did not finish after bus shutdown")
	}
	if !strings.Contains(buf.String(), "Done in") {
		t.Errorf("expected terminal banner in output, got:\n%s", buf.String())
	}
}

// TestFinish_FlushesOpenPhase verifies finish() emits a trailing newline
// when the bus shuts down mid-phase (e.g. process killed) so the next
// terminal write doesn't smash into the dot line.
func TestFinish_FlushesOpenPhase(t *testing.T) {
	r, buf := newTestRenderer(t)
	r.handle(trajectory.Event{Kind: trajectory.GoalReceived, Ts: time.Now()})
	prefix := buf.String()
	r.finish()
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("finish() should flush a newline; output was %q", buf.String())
	}
	if r.phase != "" {
		t.Errorf("finish() should clear phase; got %q", r.phase)
	}
	if len(buf.String()) <= len(prefix) {
		t.Errorf("finish() wrote nothing despite open phase: before=%q after=%q", prefix, buf.String())
	}
}

// ---- helpers -------------------------------------------------------------

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func runeLen(s string) int { return len([]rune(s)) }
