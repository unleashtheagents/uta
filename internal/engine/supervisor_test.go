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

	"github.com/unleashtheagents/uta/internal/budget"
	"github.com/unleashtheagents/uta/internal/hitl"
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

func TestSupervisor_Run_PresetSubtasks_UnregisteredWorkerOverride(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{name: "worker"}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:       "g",
		WorkerName: "worker",
		PreSetSubtasks: []SubtaskSpec{
			{ID: "s1", Title: "ok", Prompt: "p"},
			{ID: "s2", Title: "bad", Prompt: "p", Worker: "ghost"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected unregistered-subtask-worker error, got err=%v res=%+v", err, res)
	}
	if res.SessionID != "" {
		t.Fatalf("validation failure must not create a session; got id %q", res.SessionID)
	}
	sessions, err := deps.Store.ListSessions(10, 0, "")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("zombie session leaked into store: %+v", sessions)
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
		context.Canceled:         "cancelled",
		context.DeadlineExceeded: "timeout",
		errors.New("random"):     "unknown",
	}
	for err, want := range cases {
		if got := classifyError(err, nil); got != want {
			t.Errorf("classifyError(%v, nil): got %q want %q", err, got, want)
		}
	}

	// With a live (non-expired) run context, DeadlineExceeded still maps
	// to "timeout" — only the subtask-level deadline fired.
	liveCtx, liveCancel := context.WithCancel(context.Background())
	defer liveCancel()
	if got := classifyError(context.DeadlineExceeded, liveCtx); got != "timeout" {
		t.Errorf("subtask-only deadline: got %q want \"timeout\"", got)
	}

	// With a run context that has itself hit its deadline, the same
	// DeadlineExceeded is attributed to the run-wide timer instead.
	expiredCtx, expiredCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer expiredCancel()
	if got := classifyError(context.DeadlineExceeded, expiredCtx); got != "run_timeout" {
		t.Errorf("run-wide deadline: got %q want \"run_timeout\"", got)
	}

	// A user cancellation on the run context must NOT be reclassified as
	// run_timeout — only deadlines flip the bit. Wrap so we exercise
	// errors.Is.
	cancelledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if got := classifyError(context.Canceled, cancelledCtx); got != "cancelled" {
		t.Errorf("user-cancel with cancelled run ctx: got %q want \"cancelled\"", got)
	}
	if got := classifyError(context.DeadlineExceeded, cancelledCtx); got != "timeout" {
		t.Errorf("subtask deadline with user-cancelled run ctx: got %q want \"timeout\"", got)
	}

	// provider.ErrTimeout always maps to "timeout" even when the run
	// context is also past its deadline — provider-reported timeouts come
	// from the per-call timeout, not the run-wide one.
	if got := classifyError(fmt.Errorf("wrap: %w", provider.ErrTimeout), expiredCtx); got != "timeout" {
		t.Errorf("provider.ErrTimeout under expired run ctx: got %q want \"timeout\"", got)
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

// TestSupervisor_Run_BudgetExhausted_AbortsCleanly is the iter-14 acceptance
// test: a run with MaxUSDCents=10 against an expensive prompt aborts with
// ErrBudgetExceeded, the session is marked status=budget_exhausted, and a
// budget_exhausted trajectory event is recorded.
func TestSupervisor_Run_BudgetExhausted_AbortsCleanly(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			// Each call "costs" $0.20 — more than the entire $0.10 budget.
			return provider.RunResult{
				FinalText:      "expensive answer",
				ApproxUSDCents: 20,
			}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "expensive prompt",
		WorkerName:    "worker",
		SkipSynthesis: true,
		MaxUSDCents:   10, // $0.10 cap; a single 20¢ call must trip it.
		PreSetSubtasks: []SubtaskSpec{
			{ID: "s1", Title: "t", Prompt: "p"},
		},
	})
	if err == nil {
		t.Fatalf("expected ErrBudgetExceeded, got nil error and status=%q", res.Status)
	}
	if !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("err should wrap ErrBudgetExceeded, got %v", err)
	}
	if res.Status != "budget_exhausted" {
		t.Fatalf("status: got %q want budget_exhausted", res.Status)
	}

	deps.Bus.Shutdown()
	var sawExhausted bool
	for ev := range collected {
		if ev.Kind == trajectory.BudgetExhausted {
			sawExhausted = true
			if !strings.Contains(string(ev.Payload), "usd_cents") {
				t.Errorf("BudgetExhausted payload should name the tripped dimension, got %s", string(ev.Payload))
			}
		}
	}
	if !sawExhausted {
		t.Fatalf("expected a BudgetExhausted trajectory event, none recorded")
	}

	// Verify session row was marked with the new terminal status.
	sess, err := deps.Store.GetSession(res.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != "budget_exhausted" {
		t.Fatalf("session row status: got %q want budget_exhausted", sess.Status)
	}
}

// TestSupervisor_Run_PerCallBudgetExhausted_AbortsCleanly proves the
// per-call token cap is enforced end-to-end (not merely observed by the
// budget package's unit tests): a single oversized call must trip it,
// the run must terminate with status=budget_exhausted, and the
// trajectory event must name the per_call_tokens dimension.
func TestSupervisor_Run_PerCallBudgetExhausted_AbortsCleanly(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			// 130 tokens in one call — exceeds the 100-token per-call cap
			// while leaving the cumulative MaxTokens (1000) far untouched,
			// so only the per-call dimension can trip.
			return provider.RunResult{
				FinalText: "long answer",
				TokensIn:  80,
				TokensOut: 50,
			}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:             "expensive single call",
		WorkerName:       "worker",
		SkipSynthesis:    true,
		MaxTokens:        1000, // generous cumulative cap — must NOT be the dimension that trips
		PerCallMaxTokens: 100,  // single 130-token call trips this
		PreSetSubtasks: []SubtaskSpec{
			{ID: "s1", Title: "t", Prompt: "p"},
		},
	})
	if err == nil {
		t.Fatalf("expected ErrBudgetExceeded, got nil error and status=%q", res.Status)
	}
	if !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("err should wrap ErrBudgetExceeded, got %v", err)
	}
	if res.Status != "budget_exhausted" {
		t.Fatalf("status: got %q want budget_exhausted", res.Status)
	}

	deps.Bus.Shutdown()
	var sawPerCall bool
	for ev := range collected {
		if ev.Kind == trajectory.BudgetExhausted {
			if strings.Contains(string(ev.Payload), "per_call_tokens") {
				sawPerCall = true
			}
		}
	}
	if !sawPerCall {
		t.Fatalf("expected BudgetExhausted event tagged with per_call_tokens dimension")
	}
}

// TestSupervisor_Run_BudgetWarning_FiresAt80Pct verifies the 80%-of-cap
// warning surfaces on the trajectory bus.
func TestSupervisor_Run_BudgetWarning_FiresAt80Pct(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			// 80 tokens of a 100-token cap — crosses the 80% threshold
			// but does not exhaust.
			return provider.RunResult{FinalText: "ok", TokensIn: 40, TokensOut: 40}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "g",
		WorkerName:    "worker",
		SkipSynthesis: true,
		MaxTokens:     100,
		PreSetSubtasks: []SubtaskSpec{
			{ID: "s1", Title: "t", Prompt: "p"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed (warning is not a terminal state)", res.Status)
	}

	deps.Bus.Shutdown()
	var sawWarn bool
	for ev := range collected {
		if ev.Kind == trajectory.BudgetWarning {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Fatalf("expected BudgetWarning trajectory event when crossing 80%%")
	}
}

// TestSupervisor_Run_BudgetUncapped_NoEvents confirms the no-cap baseline:
// when every Max* is zero, no budget events are emitted and the run
// completes normally even with reported usage.
func TestSupervisor_Run_BudgetUncapped_NoEvents(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{FinalText: "ok", TokensIn: 5000, TokensOut: 5000, ApproxUSDCents: 9999}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "g",
		WorkerName:    "worker",
		SkipSynthesis: true,
		// No budget caps at all.
		PreSetSubtasks: []SubtaskSpec{{ID: "s1", Title: "t", Prompt: "p"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}

	deps.Bus.Shutdown()
	for ev := range collected {
		if ev.Kind == trajectory.BudgetWarning || ev.Kind == trajectory.BudgetExhausted {
			t.Errorf("unexpected budget event with uncapped run: %s", ev.Kind)
		}
	}
}

// TestSupervisor_Run_CapabilityGate_DeniesAndRewrites verifies the acceptance
// criterion: a 'dev' profile that denies Bash(curl *) rewrites a worker's
// tool_call event into a capability_gate_denied trajectory event. The
// denied tool_call must NOT also appear as a SubtaskToolCall — the gate
// owns that event.
func TestSupervisor_Run_CapabilityGate_DeniesAndRewrites(t *testing.T) {
	deps := newTestDeps(t)

	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			// Simulate a tool_call event the way claude emits one.
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"Bash","input":{"command":"curl https://example.com"}}`),
			}
			// And a benign one that should pass through.
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"Read","input":{"file_path":"/tmp/x"}}`),
			}
			return provider.RunResult{FinalText: "done"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "g",
		WorkerName:    "worker",
		SkipSynthesis: true,
		AllowedTools:  nil, // empty allow-list => only deny patterns enforced
		DeniedTools:   []string{"Bash(curl *)"},
		PreSetSubtasks: []SubtaskSpec{
			{ID: "s1", Title: "t", Prompt: "p"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	deps.Bus.Shutdown()

	var denied, toolCalls int
	for ev := range collected {
		switch ev.Kind {
		case trajectory.CapabilityGateDenied:
			denied++
			if !strings.Contains(string(ev.Payload), "Bash(curl *)") {
				t.Errorf("denial payload missing matched pattern: %s", string(ev.Payload))
			}
		case trajectory.SubtaskToolCall:
			toolCalls++
			if strings.Contains(string(ev.Payload), "curl") {
				t.Errorf("denied tool call leaked through as SubtaskToolCall: %s", string(ev.Payload))
			}
		}
	}
	if denied != 1 {
		t.Fatalf("CapabilityGateDenied events: got %d want 1", denied)
	}
	if toolCalls != 1 {
		t.Fatalf("SubtaskToolCall (allowed Read) events: got %d want 1", toolCalls)
	}
}

// TestSupervisor_Run_CapabilityGate_AllowListMissDenies verifies the
// allow-list semantic: with a non-empty AllowedTools, any tool call not on
// the list is denied by default even with no explicit deny pattern.
func TestSupervisor_Run_CapabilityGate_AllowListMissDenies(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"Bash","input":{"command":"ls"}}`),
			}
			return provider.RunResult{FinalText: "done"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err := New(deps).Run(context.Background(), RunRequest{
		Goal:           "g",
		WorkerName:     "worker",
		SkipSynthesis:  true,
		AllowedTools:   []string{"Read", "Edit"}, // Bash is NOT on the list
		PreSetSubtasks: []SubtaskSpec{{ID: "s1", Title: "t", Prompt: "p"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	deps.Bus.Shutdown()

	var denied int
	for ev := range collected {
		if ev.Kind == trajectory.CapabilityGateDenied {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("CapabilityGateDenied for allow-list miss: got %d want 1", denied)
	}
}

// TestSupervisor_Run_CapabilityGate_EmptyListsPassThrough is the no-restriction
// case: both lists empty => every tool call flows through unchanged as a
// SubtaskToolCall.
func TestSupervisor_Run_CapabilityGate_EmptyListsPassThrough(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"Bash","input":{"command":"curl https://x"}}`),
			}
			return provider.RunResult{FinalText: "done"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err := New(deps).Run(context.Background(), RunRequest{
		Goal:           "g",
		WorkerName:     "worker",
		SkipSynthesis:  true,
		PreSetSubtasks: []SubtaskSpec{{ID: "s1", Title: "t", Prompt: "p"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	deps.Bus.Shutdown()

	var denied, toolCalls int
	for ev := range collected {
		switch ev.Kind {
		case trajectory.CapabilityGateDenied:
			denied++
		case trajectory.SubtaskToolCall:
			toolCalls++
		}
	}
	if denied != 0 {
		t.Fatalf("no denials expected with empty gate; got %d", denied)
	}
	if toolCalls != 1 {
		t.Fatalf("expected 1 SubtaskToolCall passthrough; got %d", toolCalls)
	}
}

// TestSupervisor_Run_HITL_ToolTriggerApproved is the iter-16 happy-path
// acceptance: a tool_call matches a profile hitl_trigger pattern, the
// approver says yes, and the run completes normally with both
// hitl_requested and hitl_approved on the trajectory.
func TestSupervisor_Run_HITL_ToolTriggerApproved(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"Bash","input":{"command":"git push origin main"}}`),
			}
			return provider.RunResult{FinalText: "shipped"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	approver := &hitl.StaticApprover{Approve: true}
	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:           "g",
		WorkerName:     "worker",
		SkipSynthesis:  true,
		HITL:           approver,
		HITLTriggers:   []string{"Bash(* push *)"},
		HITLSeverity:   "high",
		PreSetSubtasks: []SubtaskSpec{{ID: "s1", Title: "t", Prompt: "p"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if approver.Calls() != 1 {
		t.Fatalf("approver call count: got %d want 1", approver.Calls())
	}
	deps.Bus.Shutdown()
	var requested, approved, denied, toolCalls int
	for ev := range collected {
		switch ev.Kind {
		case trajectory.HITLRequested:
			requested++
		case trajectory.HITLApproved:
			approved++
		case trajectory.HITLDenied:
			denied++
		case trajectory.SubtaskToolCall:
			toolCalls++
		}
	}
	if requested != 1 || approved != 1 {
		t.Fatalf("expected 1 HITLRequested + 1 HITLApproved, got req=%d approved=%d", requested, approved)
	}
	if denied != 0 {
		t.Fatalf("no HITLDenied expected on approve, got %d", denied)
	}
	if toolCalls != 1 {
		t.Fatalf("expected the tool_call to pass through after approval, got %d", toolCalls)
	}
}

// TestSupervisor_Run_HITL_ToolTriggerDenied is the iter-16 deny acceptance:
// the human says no, the run terminates with status=hitl_denied, the error
// wraps ErrHITLDenied, and a hitl_denied trajectory entry is recorded.
func TestSupervisor_Run_HITL_ToolTriggerDenied(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"Bash","input":{"command":"helm deploy prod"}}`),
			}
			return provider.RunResult{FinalText: "ok"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	approver := &hitl.StaticApprover{Approve: false, Reason: "operator pressed n"}
	res, runErr := New(deps).Run(context.Background(), RunRequest{
		Goal:           "g",
		WorkerName:     "worker",
		SkipSynthesis:  true,
		HITL:           approver,
		HITLTriggers:   []string{"Bash(* deploy *)"},
		HITLSeverity:   "critical",
		PreSetSubtasks: []SubtaskSpec{{ID: "s1", Title: "t", Prompt: "p"}},
	})
	if runErr == nil {
		t.Fatalf("expected ErrHITLDenied, got nil (status=%q)", res.Status)
	}
	if !errors.Is(runErr, ErrHITLDenied) {
		t.Fatalf("err should wrap ErrHITLDenied, got %v", runErr)
	}
	if res.Status != "hitl_denied" {
		t.Fatalf("status: got %q want hitl_denied", res.Status)
	}
	deps.Bus.Shutdown()
	var deniedEv int
	for ev := range collected {
		if ev.Kind == trajectory.HITLDenied {
			deniedEv++
			if !strings.Contains(string(ev.Payload), "operator pressed n") {
				t.Errorf("HITLDenied payload should carry the human's reason, got %s", string(ev.Payload))
			}
		}
	}
	if deniedEv != 1 {
		t.Fatalf("expected 1 HITLDenied trajectory event, got %d", deniedEv)
	}
}

// TestSupervisor_Run_HITL_NoTriggerLeavesCallsAlone verifies the HITL gate
// is inert when no patterns match the tool_call.
func TestSupervisor_Run_HITL_NoTriggerLeavesCallsAlone(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"Read","input":{"path":"README.md"}}`),
			}
			return provider.RunResult{FinalText: "ok"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	approver := &hitl.StaticApprover{Approve: true}
	res, err := New(deps).Run(context.Background(), RunRequest{
		Goal:           "g",
		WorkerName:     "worker",
		SkipSynthesis:  true,
		HITL:           approver,
		HITLTriggers:   []string{"Bash(* push *)"},
		PreSetSubtasks: []SubtaskSpec{{ID: "s1", Title: "t", Prompt: "p"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if approver.Calls() != 0 {
		t.Fatalf("approver should not have been consulted; got %d calls", approver.Calls())
	}
}

// TestSupervisor_Run_HITL_BudgetThresholdFiresOnce verifies the
// improve-mode style threshold gate: a single approval prompt fires when
// cumulative tokens cross the configured percent of MaxTokens.
func TestSupervisor_Run_HITL_BudgetThresholdFiresOnce(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			// 30 tokens per call — two calls puts us at 60 / 200 (30%
			// per call, 60% cumulative). The threshold of 25% trips on
			// the first call; the second must observe the one-shot flag
			// already set and skip the prompt.
			return provider.RunResult{FinalText: "ok", TokensIn: 15, TokensOut: 15}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	approver := &hitl.StaticApprover{Approve: true}
	_, err := New(deps).Run(context.Background(), RunRequest{
		Goal:               "g",
		WorkerName:         "worker",
		SkipSynthesis:      true,
		HITL:               approver,
		MaxTokens:          200,
		HITLTokenThreshold: 25,
		PreSetSubtasks: []SubtaskSpec{
			{ID: "s1", Title: "t1", Prompt: "p1"},
			{ID: "s2", Title: "t2", Prompt: "p2"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One-shot: even though both subtasks individually cross the threshold,
	// we should only consult the human once.
	if approver.Calls() != 1 {
		t.Fatalf("budget-threshold approver should fire once per run, got %d calls", approver.Calls())
	}
}

// TestSupervisor_Run_HITL_AsyncFileDropApproved is the iter-16 async
// integration test: the supervisor calls a *real* *hitl.Gate (not the
// StaticApprover stub) and we drop the .approved marker from a parallel
// goroutine. The run must observe the approval via the poll loop, emit
// hitl_approved, and complete normally. This closes the audit gap that
// asks for a test exercising the pending.json schema end-to-end.
func TestSupervisor_Run_HITL_AsyncFileDropApproved(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"Bash","input":{"command":"git push origin main"}}`),
			}
			return provider.RunResult{FinalText: "shipped"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	stateDir := t.TempDir()
	gate := &hitl.Gate{
		StateDir:   stateDir,
		ForceAsync: true,
		AsyncPoll:  10 * time.Millisecond,
	}

	// Approver goroutine: poll for the gate's pending.json pointer, then
	// drop a sibling .approved marker. The supervisor's call into the gate
	// should observe it and proceed.
	approvalDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			p, err := hitl.ReadPending(filepath.Join(stateDir, "hitl"))
			_ = p
			_ = err
			// Walk session directories — we don't know the session ID up
			// front because Supervisor.Run generates it.
			entries, _ := filepath.Glob(filepath.Join(stateDir, "hitl", "*"))
			for _, sessionDir := range entries {
				id, _ := hitl.FirstPending(sessionDir)
				if id != "" {
					approvalDone <- hitl.WriteApproval(sessionDir, id)
					return
				}
			}
			time.Sleep(15 * time.Millisecond)
		}
		approvalDone <- errors.New("no pending request appeared")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := New(deps).Run(ctx, RunRequest{
		Goal:           "g",
		WorkerName:     "worker",
		SkipSynthesis:  true,
		HITL:           gate,
		HITLTriggers:   []string{"Bash(* push *)"},
		HITLSeverity:   "high",
		PreSetSubtasks: []SubtaskSpec{{ID: "s1", Title: "t", Prompt: "p"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if approvErr := <-approvalDone; approvErr != nil {
		t.Fatalf("approval goroutine: %v", approvErr)
	}
}

// TestSupervisor_Run_HITL_AsyncFileDropDenied is the async denial
// counterpart: the operator drops a .denied marker and the run must
// terminate with status=hitl_denied + ErrHITLDenied. Together with the
// approve test above, this verifies the pending.json file-drop loop
// resumes-or-aborts as advertised.
func TestSupervisor_Run_HITL_AsyncFileDropDenied(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"Bash","input":{"command":"helm deploy prod"}}`),
			}
			return provider.RunResult{FinalText: "ok"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	stateDir := t.TempDir()
	gate := &hitl.Gate{StateDir: stateDir, ForceAsync: true, AsyncPoll: 10 * time.Millisecond}

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			entries, _ := filepath.Glob(filepath.Join(stateDir, "hitl", "*"))
			for _, sessionDir := range entries {
				id, _ := hitl.FirstPending(sessionDir)
				if id != "" {
					_ = hitl.WriteDenial(sessionDir, id, "operator nope")
					return
				}
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, runErr := New(deps).Run(ctx, RunRequest{
		Goal:           "g",
		WorkerName:     "worker",
		SkipSynthesis:  true,
		HITL:           gate,
		HITLTriggers:   []string{"Bash(* deploy *)"},
		HITLSeverity:   "critical",
		PreSetSubtasks: []SubtaskSpec{{ID: "s1", Title: "t", Prompt: "p"}},
	})
	if runErr == nil {
		t.Fatalf("expected ErrHITLDenied, got nil (status=%q)", res.Status)
	}
	if !errors.Is(runErr, ErrHITLDenied) {
		t.Fatalf("err should wrap ErrHITLDenied, got %v", runErr)
	}
	if res.Status != "hitl_denied" {
		t.Fatalf("status: got %q want hitl_denied", res.Status)
	}
	if !strings.Contains(runErr.Error(), "operator nope") {
		t.Errorf("err should carry operator reason, got %v", runErr)
	}
}
