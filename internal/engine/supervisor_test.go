package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// fakeProvider is a minimal AgentProvider used to drive Supervisor tests
// without spawning real CLIs. RunHeadless delegates to the configurable hook
// so each test can return predetermined results, errors, or count invocations.
type fakeProvider struct {
	name string
	run  func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error)
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Detect(ctx context.Context) provider.Detection {
	return provider.Detection{Available: true, BinaryPath: "/fake/" + f.name, Version: "test"}
}

func (f *fakeProvider) RunHeadless(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	if f.run == nil {
		return provider.RunResult{FinalText: "ok"}, nil
	}
	return f.run(ctx, prompt, opts, events)
}

// newTestDeps builds an isolated Deps with a real SQLite store, blobs dir,
// recorder, bus, and empty registry. Callers register fake providers as needed.
func newTestDeps(t *testing.T) Deps {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return Deps{
		Store:    s,
		Blobs:    store.NewBlobs(dir),
		Recorder: trajectory.NewRecorder(s),
		Bus:      trajectory.NewBus(),
		Registry: provider.NewRegistry(),
	}
}

func TestSupervisor_Run_EmptyGoal(t *testing.T) {
	deps := newTestDeps(t)
	_, err := New(deps).Run(context.Background(), RunRequest{WorkerName: "anything"})
	if err == nil || !strings.Contains(err.Error(), "goal") {
		t.Fatalf("expected goal-is-empty error, got %v", err)
	}
}

func TestSupervisor_Run_MissingWorker(t *testing.T) {
	deps := newTestDeps(t)
	_, err := New(deps).Run(context.Background(), RunRequest{Goal: "do something"})
	if err == nil || !strings.Contains(err.Error(), "worker is required") {
		t.Fatalf("expected worker-required error, got %v", err)
	}
}

func TestSupervisor_Run_UnregisteredWorker(t *testing.T) {
	deps := newTestDeps(t)
	_, err := New(deps).Run(context.Background(), RunRequest{
		Goal: "g", WorkerName: "nobody",
	})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected not-registered error, got %v", err)
	}
}

func TestSupervisor_Run_PresetSubtasks_HappyPath(t *testing.T) {
	deps := newTestDeps(t)
	var calls atomic.Int32
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			calls.Add(1)
			return provider.RunResult{FinalText: "did: " + prompt, SessionID: "prov-sess"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "test goal",
		WorkerName:    "worker",
		SkipSynthesis: true,
		PreSetSubtasks: []SubtaskSpec{
			{ID: "s1", Title: "one", Prompt: "task one"},
			{ID: "s2", Title: "two", Prompt: "task two"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if len(res.Subtasks) != 2 {
		t.Fatalf("subtasks: got %d want 2", len(res.Subtasks))
	}
	for _, st := range res.Subtasks {
		if st.Status != "completed" {
			t.Fatalf("subtask %s status: got %q want completed", st.ID, st.Status)
		}
		if st.ProviderSessionID != "prov-sess" {
			t.Fatalf("subtask %s provider session id: got %q want prov-sess", st.ID, st.ProviderSessionID)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("provider call count: got %d want 2 (synthesis skipped)", got)
	}
	if !strings.Contains(res.FinalAnswer, "task one") || !strings.Contains(res.FinalAnswer, "task two") {
		t.Fatalf("FinalAnswer should contain both subtask outputs, got %q", res.FinalAnswer)
	}
}

func TestSupervisor_Run_SynthesizerCalledWhenNotSkipped(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{FinalText: "worker output"}, nil
		},
	}
	var synthCalls atomic.Int32
	synth := &fakeProvider{
		name: "synth",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			synthCalls.Add(1)
			if !strings.Contains(prompt, "Original goal") {
				return provider.RunResult{}, fmt.Errorf("synth prompt missing goal: %s", prompt)
			}
			return provider.RunResult{FinalText: "synthesized answer"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if err := deps.Registry.Register(synth, false); err != nil {
		t.Fatalf("register synth: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:       "the goal",
		WorkerName: "worker",
		SynthName:  "synth",
		PreSetSubtasks: []SubtaskSpec{
			{ID: "s1", Title: "t", Prompt: "p"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if res.FinalAnswer != "synthesized answer" {
		t.Fatalf("FinalAnswer: got %q want %q", res.FinalAnswer, "synthesized answer")
	}
	if got := synthCalls.Load(); got != 1 {
		t.Fatalf("synth calls: got %d want 1", got)
	}
}

func TestSupervisor_Run_PlannerHappyPath(t *testing.T) {
	deps := newTestDeps(t)
	planner := &fakeProvider{
		name: "planner",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{
				FinalText: `{"subtasks":[{"id":"a","title":"alpha","prompt":"do alpha"},{"id":"b","title":"beta","prompt":"do beta"}]}`,
			}, nil
		},
	}
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{FinalText: "subtask result"}, nil
		},
	}
	if err := deps.Registry.Register(planner, false); err != nil {
		t.Fatalf("register planner: %v", err)
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "build something",
		WorkerName:    "worker",
		PlannerName:   "planner",
		SkipSynthesis: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if len(res.Subtasks) != 2 {
		t.Fatalf("subtasks: got %d want 2 (planner decomposed into two)", len(res.Subtasks))
	}
}

func TestSupervisor_Run_PlannerFallback(t *testing.T) {
	deps := newTestDeps(t)
	planner := &fakeProvider{
		name: "planner",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{FinalText: "the model refused"}, nil
		},
	}
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{FinalText: "fallback worker output"}, nil
		},
	}
	if err := deps.Registry.Register(planner, false); err != nil {
		t.Fatalf("register planner: %v", err)
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "g",
		WorkerName:    "worker",
		PlannerName:   "planner",
		SkipSynthesis: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	// Fallback plan has exactly one subtask that restates the goal.
	if len(res.Subtasks) != 1 {
		t.Fatalf("expected fallback plan with 1 subtask, got %d", len(res.Subtasks))
	}
}

func TestSupervisor_Run_PartialOnSubtaskFailure(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			if strings.Contains(prompt, "fail") {
				return provider.RunResult{}, fmt.Errorf("nope: %w", provider.ErrWorkerFailed)
			}
			return provider.RunResult{FinalText: "fine"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "g",
		WorkerName:    "worker",
		SkipSynthesis: true,
		PreSetSubtasks: []SubtaskSpec{
			{ID: "ok", Title: "ok", Prompt: "good"},
			{ID: "bad", Title: "bad", Prompt: "fail please"},
		},
	})
	if err != nil {
		t.Fatalf("Run (should not error without FailFast): %v", err)
	}
	if res.Status != "partial" {
		t.Fatalf("status: got %q want partial (one subtask failed without FailFast)", res.Status)
	}
	var sawFailed bool
	for _, st := range res.Subtasks {
		if st.Status == "failed" {
			sawFailed = true
			if st.ErrorKind != "worker" {
				t.Fatalf("expected error_kind=worker, got %q", st.ErrorKind)
			}
		}
	}
	if !sawFailed {
		t.Fatalf("expected one subtask row marked failed, none found in %+v", res.Subtasks)
	}
}

func TestSupervisor_Run_FailFastAbortsRun(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{}, fmt.Errorf("crash: %w", provider.ErrWorkerFailed)
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "g",
		WorkerName:    "worker",
		FailFast:      true,
		SkipSynthesis: true,
		PreSetSubtasks: []SubtaskSpec{
			{ID: "s1", Title: "t", Prompt: "p"},
		},
	})
	if err == nil {
		t.Fatalf("expected fail-fast error, got nil")
	}
	if res.Status != "failed" {
		t.Fatalf("status: got %q want failed", res.Status)
	}
}

func TestSupervisor_Run_RunTimeoutCancelsContext(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			// Block until the parent context is cancelled (timeout).
			<-ctx.Done()
			return provider.RunResult{}, fmt.Errorf("ctx done: %w", ctx.Err())
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	start := time.Now()
	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "g",
		WorkerName:    "worker",
		RunTimeout:    50 * time.Millisecond,
		SkipSynthesis: true,
		PreSetSubtasks: []SubtaskSpec{
			{ID: "s1", Title: "t", Prompt: "p"},
		},
	})
	// We expect the run to terminate quickly with the subtask reported failed
	// (the worker returns an error after context cancellation).
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Run did not honor RunTimeout: took %s", time.Since(start))
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("Run terminal error: %v", err)
	}
	if res.SessionID == "" {
		t.Fatalf("expected a session id even on cancellation")
	}
}

func TestPickStrategy(t *testing.T) {
	plain := Plan{Subtasks: []SubtaskSpec{{ID: "a"}}}
	if got := pickStrategy("", plain); got != "fanout" {
		t.Fatalf("plain plan default: got %q want fanout", got)
	}
	withDeps := Plan{Subtasks: []SubtaskSpec{{ID: "a"}, {ID: "b", Needs: []string{"a"}}}}
	if got := pickStrategy("", withDeps); got != "dag" {
		t.Fatalf("plan with needs default: got %q want dag", got)
	}
	withGate := Plan{Subtasks: []SubtaskSpec{{ID: "a", Gate: &Gate{Cmd: "true"}}}}
	if got := pickStrategy("", withGate); got != "dag" {
		t.Fatalf("plan with gate default: got %q want dag", got)
	}
	// Explicit value wins regardless of plan shape.
	if got := pickStrategy("fanout", withDeps); got != "fanout" {
		t.Fatalf("explicit fanout: got %q want fanout", got)
	}
}

func TestSubtaskTimeout_SpecOverridesRequest(t *testing.T) {
	// Spec.Timeout > 0 wins.
	got := subtaskTimeout(SubtaskSpec{Timeout: 2 * time.Second}, RunRequest{SubtaskTimeout: 30 * time.Second})
	if got != 2*time.Second {
		t.Fatalf("spec override: got %v want 2s", got)
	}
	// Spec.Timeout zero falls back to request default.
	got = subtaskTimeout(SubtaskSpec{}, RunRequest{SubtaskTimeout: 30 * time.Second})
	if got != 30*time.Second {
		t.Fatalf("fallback: got %v want 30s", got)
	}
}

// TestSupervisor_Run_PerSubtaskTimeout verifies that a SubtaskSpec with a
// short Timeout is honored independently of RunRequest.SubtaskTimeout: a
// worker that sleeps past the per-subtask budget gets cancelled even when
// the run-wide default would have let it run to completion.
func TestSupervisor_Run_PerSubtaskTimeout(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			select {
			case <-ctx.Done():
				return provider.RunResult{}, fmt.Errorf("worker cancelled: %w", ctx.Err())
			case <-time.After(2 * time.Second):
				return provider.RunResult{FinalText: "never"}, nil
			}
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:           "test goal",
		WorkerName:     "worker",
		SkipSynthesis:  true,
		SubtaskTimeout: 30 * time.Second, // generous default
		PreSetSubtasks: []SubtaskSpec{
			{ID: "fast", Title: "fast", Prompt: "p", Timeout: 50 * time.Millisecond},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Subtasks) != 1 {
		t.Fatalf("subtasks: got %d want 1", len(res.Subtasks))
	}
	if got, want := res.Subtasks[0].Status, "failed"; got != want {
		t.Fatalf("subtask status: got %q want %q (per-subtask timeout should fire)", got, want)
	}
	if got, want := res.Subtasks[0].ErrorKind, "timeout"; got != want {
		t.Fatalf("subtask error kind: got %q want %q", got, want)
	}
}

func TestClassifyError(t *testing.T) {
	cases := map[error]string{
		fmt.Errorf("wrap: %w", provider.ErrAuth):         "auth",
		fmt.Errorf("wrap: %w", provider.ErrQuota):        "quota",
		fmt.Errorf("wrap: %w", provider.ErrTimeout):      "timeout",
		fmt.Errorf("wrap: %w", provider.ErrTransport):    "transport",
		fmt.Errorf("wrap: %w", provider.ErrWorkerFailed): "worker",
		context.Canceled:        "cancelled",
		context.DeadlineExceeded: "timeout",
		errors.New("random"):     "unknown",
	}
	for err, want := range cases {
		if got := classifyError(err); got != want {
			t.Errorf("classifyError(%v): got %q want %q", err, got, want)
		}
	}
}

func TestTransportBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{-1, 1 * time.Second}, // negative clamps to attempt=0
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second},  // capped
		{20, 30 * time.Second}, // capped, no overflow
	}
	for _, c := range cases {
		if got := transportBackoff(c.attempt); got != c.want {
			t.Errorf("transportBackoff(%d): got %s want %s", c.attempt, got, c.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Fatalf("got %q want a", got)
	}
	if got := firstNonEmpty("", "b"); got != "b" {
		t.Fatalf("got %q want b", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q want empty", got)
	}
}
