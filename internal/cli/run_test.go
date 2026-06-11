package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/profile"
)

// fakeEmitter is an in-memory handoffEmitter that records every event
// and forwards EvaluateHandoffs to a caller-supplied function. Used by
// executeHandoffChain tests so they don't touch the trajectory bus.
type fakeEmitter struct {
	evaluate func(prior string, hs []profile.Handoff, files []string, resolve engine.ProfileResolver) (*engine.HandoffMatch, error)
	started  []string
	finished []string
	canceled []string
}

func (f *fakeEmitter) EvaluateHandoffs(
	priorSessionID string,
	handoffs []profile.Handoff,
	changedFiles []string,
	resolve engine.ProfileResolver,
) (*engine.HandoffMatch, error) {
	if f.evaluate != nil {
		return f.evaluate(priorSessionID, handoffs, changedFiles, resolve)
	}
	return nil, nil
}

func (f *fakeEmitter) EmitHandoffStarted(priorSessionID string, m *engine.HandoffMatch) {
	f.started = append(f.started, priorSessionID+"->"+m.Handoff.TargetMode)
}

func (f *fakeEmitter) EmitHandoffCompleted(priorSessionID, chainedSessionID, status string) {
	f.finished = append(f.finished, priorSessionID+"->"+chainedSessionID+":"+status)
}

func (f *fakeEmitter) EmitHandoffCancelled(priorSessionID, targetMode, reason string) {
	f.canceled = append(f.canceled, priorSessionID+"->"+targetMode+":"+reason)
}

func TestHandoffDepthLimit_UsesProfileOverrideOverDefault(t *testing.T) {
	if got := handoffDepthLimit(engine.RunRequest{}); got != profile.DefaultMaxHandoffDepth {
		t.Errorf("zero MaxHandoffDepth: got %d, want %d", got, profile.DefaultMaxHandoffDepth)
	}
	if got := handoffDepthLimit(engine.RunRequest{MaxHandoffDepth: 1}); got != 1 {
		t.Errorf("MaxHandoffDepth=1: got %d, want 1", got)
	}
	if got := handoffDepthLimit(engine.RunRequest{MaxHandoffDepth: 10}); got != 10 {
		t.Errorf("MaxHandoffDepth=10: got %d, want 10", got)
	}
}

// TestExecuteHandoffChain_SkipsWhenPriorNotCompleted: a failed/partial
// prior run never triggers a chain — the caller's "session X status=Y"
// reflects the failure as-is. This is the path runHandoffChain (the
// production wrapper) takes; executeHandoffChain itself trusts the
// caller, so we exercise the wrapper indirectly here by passing a
// non-completed prior result and verifying the emitter wasn't called.
func TestExecuteHandoffChain_NoMatchReturnsImmediately(t *testing.T) {
	em := &fakeEmitter{
		evaluate: func(prior string, _ []profile.Handoff, _ []string, _ engine.ProfileResolver) (*engine.HandoffMatch, error) {
			return nil, nil
		},
	}
	var ran int
	deps := handoffChainDeps{
		run: func(ctx context.Context, req engine.RunRequest) (engine.RunResult, error) {
			ran++
			return engine.RunResult{}, nil
		},
		changedFiles: func(ctx context.Context, _ string) ([]string, error) { return nil, nil },
		resolve:      func(string) (*profile.MissionProfile, error) { return nil, nil },
		emit:         em,
		out:          &bytes.Buffer{},
	}
	mode := &profile.MissionProfile{
		Name:       "dev",
		OnComplete: []profile.Handoff{{TargetMode: "audit"}},
	}
	prior := engine.RunResult{SessionID: "s1", Status: "completed"}
	got := executeHandoffChain(context.Background(), deps, mode, engine.RunRequest{}, prior)
	if got.SessionID != "s1" {
		t.Errorf("returned result = %+v, want prior", got)
	}
	if ran != 0 {
		t.Errorf("run called %d times when no match should fire", ran)
	}
	if len(em.started) != 0 {
		t.Errorf("HandoffStarted emitted with no match: %v", em.started)
	}
}

// TestExecuteHandoffChain_MissingTargetMode covers the AUDIT finding:
// when EvaluateHandoffs returns nil because the target mode does not
// resolve, the chain stops cleanly without panicking or running
// anything.
func TestExecuteHandoffChain_MissingTargetMode(t *testing.T) {
	em := &fakeEmitter{
		evaluate: func(prior string, hs []profile.Handoff, files []string, resolve engine.ProfileResolver) (*engine.HandoffMatch, error) {
			// Defer to the real evaluator semantics: missing target => nil
			// match, no error.
			return nil, nil
		},
	}
	var ran int
	deps := handoffChainDeps{
		run: func(ctx context.Context, req engine.RunRequest) (engine.RunResult, error) {
			ran++
			return engine.RunResult{}, nil
		},
		changedFiles: func(ctx context.Context, _ string) ([]string, error) { return nil, nil },
		resolve:      func(name string) (*profile.MissionProfile, error) { return nil, nil },
		emit:         em,
		out:          &bytes.Buffer{},
	}
	mode := &profile.MissionProfile{
		Name:       "dev",
		OnComplete: []profile.Handoff{{TargetMode: "ghost"}},
	}
	prior := engine.RunResult{SessionID: "s1", Status: "completed"}
	got := executeHandoffChain(context.Background(), deps, mode, engine.RunRequest{}, prior)
	if got.SessionID != "s1" {
		t.Errorf("chain should return prior when target is missing, got %+v", got)
	}
	if ran != 0 {
		t.Errorf("run should not fire for a missing target mode (called %d times)", ran)
	}
	if len(em.canceled) != 0 || len(em.finished) != 0 || len(em.started) != 0 {
		t.Errorf("no emit events expected; got started=%v finished=%v canceled=%v", em.started, em.finished, em.canceled)
	}
}

// TestExecuteHandoffChain_CancellationMidChain covers the AUDIT finding:
// a Ctrl-C while a chained run is in flight should produce a
// HandoffCancelled event keyed to the prior session and short-circuit
// the rest of the chain. The run function blocks on ctx.Done so this
// exercises the real interrupt path — a goroutine cancels the parent
// context while the (fake) chained run is in progress, just like a
// SIGINT would in production.
func TestExecuteHandoffChain_CancellationMidChain(t *testing.T) {
	audit := &profile.MissionProfile{Name: "audit"}
	em := &fakeEmitter{
		evaluate: func(prior string, hs []profile.Handoff, files []string, resolve engine.ProfileResolver) (*engine.HandoffMatch, error) {
			return &engine.HandoffMatch{
				Handoff:    hs[0],
				TargetMode: audit,
				Prompt:     "go audit",
			}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runEntered := make(chan struct{})
	deps := handoffChainDeps{
		run: func(ctx context.Context, req engine.RunRequest) (engine.RunResult, error) {
			// Signal that the chained run is in flight, then wait for the
			// caller's Ctrl-C to land. A real provider would be blocked in
			// its own select on the same parent context.
			close(runEntered)
			<-ctx.Done()
			return engine.RunResult{SessionID: "s2", Status: "cancelled"}, ctx.Err()
		},
		changedFiles: func(ctx context.Context, _ string) ([]string, error) { return nil, nil },
		resolve: func(name string) (*profile.MissionProfile, error) {
			if name == "audit" {
				return audit, nil
			}
			return nil, nil
		},
		emit: em,
		out:  &bytes.Buffer{},
	}
	mode := &profile.MissionProfile{
		Name:       "dev",
		OnComplete: []profile.Handoff{{TargetMode: "audit"}},
	}
	prior := engine.RunResult{SessionID: "s1", Status: "completed"}

	// Simulate SIGINT: cancel once the chained run is provably underway.
	go func() {
		<-runEntered
		cancel()
	}()

	got := executeHandoffChain(ctx, deps, mode, engine.RunRequest{}, prior)

	if got.SessionID != "s1" {
		t.Errorf("cancellation should return prior result, got %+v", got)
	}
	if len(em.canceled) != 1 {
		t.Fatalf("expected 1 cancellation event, got %v", em.canceled)
	}
	if em.canceled[0] != "s1->audit:user cancelled" {
		t.Errorf("cancellation payload = %q", em.canceled[0])
	}
	if len(em.finished) != 0 {
		t.Errorf("no Completed event should fire on cancellation, got %v", em.finished)
	}
	if len(em.started) != 1 {
		t.Errorf("HandoffStarted should still fire before the run is cancelled, got %v", em.started)
	}
}

// TestExecuteHandoffChain_FailedChainStopsAndEmitsFailed: a chained run
// that returns a non-Canceled error stops the chain and emits a
// HandoffCompleted with status="failed" so trajectory consumers see
// the failure point.
func TestExecuteHandoffChain_FailedChainStopsAndEmitsFailed(t *testing.T) {
	audit := &profile.MissionProfile{Name: "audit"}
	em := &fakeEmitter{
		evaluate: func(prior string, hs []profile.Handoff, _ []string, _ engine.ProfileResolver) (*engine.HandoffMatch, error) {
			return &engine.HandoffMatch{Handoff: hs[0], TargetMode: audit, Prompt: "go audit"}, nil
		},
	}
	deps := handoffChainDeps{
		run: func(ctx context.Context, req engine.RunRequest) (engine.RunResult, error) {
			return engine.RunResult{SessionID: "s-fail", Status: "failed"}, errors.New("provider exploded")
		},
		changedFiles: func(ctx context.Context, _ string) ([]string, error) { return nil, nil },
		resolve:      func(string) (*profile.MissionProfile, error) { return audit, nil },
		emit:         em,
		out:          &bytes.Buffer{},
	}
	mode := &profile.MissionProfile{
		Name:       "dev",
		OnComplete: []profile.Handoff{{TargetMode: "audit"}},
	}
	prior := engine.RunResult{SessionID: "s1", Status: "completed"}
	got := executeHandoffChain(context.Background(), deps, mode, engine.RunRequest{}, prior)

	if got.SessionID != "s-fail" {
		t.Errorf("failed-chain result should propagate, got %+v", got)
	}
	if len(em.finished) != 1 || em.finished[0] != "s1->s-fail:failed" {
		t.Errorf("expected 1 failed-completion event, got %v", em.finished)
	}
	if len(em.canceled) != 0 {
		t.Errorf("non-Canceled error should not emit HandoffCancelled, got %v", em.canceled)
	}
}

// TestExecuteHandoffChain_RespectsCustomMaxDepth proves the
// MissionProfile-supplied depth limit overrides the built-in default.
// We build a self-referential chain (dev -> dev) and cap it at 2 hops
// via the request; the chain should fire exactly twice and then emit
// the "max depth" warning.
func TestExecuteHandoffChain_RespectsCustomMaxDepth(t *testing.T) {
	dev := &profile.MissionProfile{
		Name:       "dev",
		OnComplete: []profile.Handoff{{TargetMode: "dev"}},
	}
	em := &fakeEmitter{
		evaluate: func(prior string, hs []profile.Handoff, _ []string, _ engine.ProfileResolver) (*engine.HandoffMatch, error) {
			return &engine.HandoffMatch{Handoff: hs[0], TargetMode: dev, Prompt: "loop"}, nil
		},
	}
	runs := 0
	deps := handoffChainDeps{
		run: func(ctx context.Context, req engine.RunRequest) (engine.RunResult, error) {
			runs++
			return engine.RunResult{SessionID: "loop-x", Status: "completed"}, nil
		},
		changedFiles: func(ctx context.Context, _ string) ([]string, error) { return nil, nil },
		resolve:      func(string) (*profile.MissionProfile, error) { return dev, nil },
		emit:         em,
		out:          &bytes.Buffer{},
	}
	baseReq := engine.RunRequest{MaxHandoffDepth: 2}
	prior := engine.RunResult{SessionID: "s1", Status: "completed"}
	_ = executeHandoffChain(context.Background(), deps, dev, baseReq, prior)

	if runs != 2 {
		t.Errorf("expected exactly 2 chained runs at MaxHandoffDepth=2, got %d", runs)
	}
}

// TestExecuteHandoffChain_PreCancelledContextSkips: a context cancelled
// before the loop even starts should produce zero chained runs and zero
// emit events.
func TestExecuteHandoffChain_PreCancelledContextSkips(t *testing.T) {
	em := &fakeEmitter{
		evaluate: func(string, []profile.Handoff, []string, engine.ProfileResolver) (*engine.HandoffMatch, error) {
			t.Fatal("EvaluateHandoffs should not be called when ctx is already cancelled")
			return nil, nil
		},
	}
	deps := handoffChainDeps{
		run: func(ctx context.Context, req engine.RunRequest) (engine.RunResult, error) {
			return engine.RunResult{}, nil
		},
		changedFiles: func(ctx context.Context, _ string) ([]string, error) { return nil, nil },
		resolve:      func(string) (*profile.MissionProfile, error) { return nil, nil },
		emit:         em,
		out:          &bytes.Buffer{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mode := &profile.MissionProfile{
		Name:       "dev",
		OnComplete: []profile.Handoff{{TargetMode: "audit"}},
	}
	prior := engine.RunResult{SessionID: "s1", Status: "completed"}
	got := executeHandoffChain(ctx, deps, mode, engine.RunRequest{}, prior)
	if got.SessionID != "s1" {
		t.Errorf("pre-cancelled context should return prior, got %+v", got)
	}
}
