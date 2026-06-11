package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/budget"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// fakeResumableProvider satisfies both AgentProvider and provider.Resumable.
// Tests configure resume to capture or stub out the ResumeHeadless behavior.
type fakeResumableProvider struct {
	fakeProvider
	resume func(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error)
}

func (f *fakeResumableProvider) ResumeHeadless(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	if f.resume == nil {
		return provider.RunResult{FinalText: "resumed: " + prompt, SessionID: sessionID}, nil
	}
	return f.resume(ctx, sessionID, prompt, opts, events)
}

// seedPriorSession inserts a session + last subtask with the supplied
// provider session id so Resume has something to recover from.
func seedPriorSession(t *testing.T, deps Deps, worker, providerSessionID string) string {
	t.Helper()
	priorID := uuid.NewString()
	if err := deps.Store.CreateSession(store.Session{
		ID:        priorID,
		Goal:      "prior goal",
		Worker:    worker,
		Status:    "completed",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed prior session: %v", err)
	}
	if err := deps.Store.CreateSubtask(store.Subtask{
		ID:        uuid.NewString(),
		SessionID: priorID,
		Ord:       0,
		Title:     "prior",
		PromptRef: "",
		Worker:    worker,
		Status:    "completed",
	}); err != nil {
		t.Fatalf("seed prior subtask: %v", err)
	}
	if providerSessionID != "" {
		// UpdateSubtask requires the row to already exist; we look it up via
		// LastSubtask to grab the id we just inserted.
		last, err := deps.Store.LastSubtask(priorID)
		if err != nil {
			t.Fatalf("look up seeded subtask: %v", err)
		}
		if err := deps.Store.UpdateSubtask(store.Subtask{
			ID:                last.ID,
			ProviderSessionID: providerSessionID,
			Status:            "completed",
		}); err != nil {
			t.Fatalf("set provider session id on seeded subtask: %v", err)
		}
	}
	return priorID
}

func TestSupervisor_Resume_EmptyGoal(t *testing.T) {
	deps := newTestDeps(t)
	_, err := New(deps).Resume(context.Background(), ResumeRequest{PriorSessionID: "anything"})
	if err == nil || !strings.Contains(err.Error(), "goal") {
		t.Fatalf("expected goal-required error, got %v", err)
	}
}

func TestSupervisor_Resume_PriorSessionNotFound(t *testing.T) {
	deps := newTestDeps(t)
	_, err := New(deps).Resume(context.Background(), ResumeRequest{
		PriorSessionID: "does-not-exist",
		Goal:           "follow up",
	})
	if err == nil || !strings.Contains(err.Error(), "prior session not found") {
		t.Fatalf("expected prior-session-not-found error, got %v", err)
	}
}

func TestSupervisor_Resume_NoProviderSessionID(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeResumableProvider{fakeProvider: fakeProvider{name: "worker"}}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	priorID := seedPriorSession(t, deps, "worker", "")

	_, err := New(deps).Resume(context.Background(), ResumeRequest{
		PriorSessionID: priorID,
		Goal:           "follow up",
	})
	if err == nil || !strings.Contains(err.Error(), "no recorded provider session id") {
		t.Fatalf("expected no-provider-session-id error, got %v", err)
	}
}

func TestSupervisor_Resume_WorkerNotRegistered(t *testing.T) {
	deps := newTestDeps(t)
	priorID := seedPriorSession(t, deps, "ghost", "prov-sess-1")

	_, err := New(deps).Resume(context.Background(), ResumeRequest{
		PriorSessionID: priorID,
		Goal:           "follow up",
	})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected worker-not-registered error, got %v", err)
	}
}

func TestSupervisor_Resume_WorkerNotResumable(t *testing.T) {
	deps := newTestDeps(t)
	// Plain fakeProvider does NOT implement Resumable.
	worker := &fakeProvider{name: "worker"}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	priorID := seedPriorSession(t, deps, "worker", "prov-sess-1")

	_, err := New(deps).Resume(context.Background(), ResumeRequest{
		PriorSessionID: priorID,
		Goal:           "follow up",
	})
	if err == nil || !strings.Contains(err.Error(), "does not support resume") {
		t.Fatalf("expected does-not-support-resume error, got %v", err)
	}
}

func TestSupervisor_Resume_HappyPath(t *testing.T) {
	deps := newTestDeps(t)
	var (
		calls        atomic.Int32
		gotSessionID atomic.Value
		gotPrompt    atomic.Value
	)
	worker := &fakeResumableProvider{
		fakeProvider: fakeProvider{name: "worker"},
		resume: func(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			calls.Add(1)
			gotSessionID.Store(sessionID)
			gotPrompt.Store(prompt)
			return provider.RunResult{FinalText: "continued answer", SessionID: "prov-sess-2"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	priorID := seedPriorSession(t, deps, "worker", "prov-sess-1")

	res, err := New(deps).Resume(context.Background(), ResumeRequest{
		PriorSessionID: priorID,
		Goal:           "what about edge case X?",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if res.SessionID == "" || res.SessionID == priorID {
		t.Fatalf("expected a new session id distinct from prior, got %q (prior %q)", res.SessionID, priorID)
	}
	if res.FinalAnswer != "continued answer" {
		t.Fatalf("FinalAnswer: got %q want %q", res.FinalAnswer, "continued answer")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("ResumeHeadless calls: got %d want 1", got)
	}
	if got, _ := gotSessionID.Load().(string); got != "prov-sess-1" {
		t.Fatalf("ResumeHeadless got session id %q, want %q (the prior subtask's provider session id)", got, "prov-sess-1")
	}
	if got, _ := gotPrompt.Load().(string); got != "what about edge case X?" {
		t.Fatalf("ResumeHeadless got prompt %q, want the new goal verbatim", got)
	}

	// Verify the new session was persisted with resumed_from metadata and a
	// single completed subtask carrying the new provider session id.
	newSess, err := deps.Store.GetSession(res.SessionID)
	if err != nil {
		t.Fatalf("GetSession new: %v", err)
	}
	if newSess.Status != "completed" {
		t.Fatalf("persisted session status: got %q want completed", newSess.Status)
	}
	if !strings.Contains(newSess.MetaJSON, priorID) {
		t.Fatalf("new session meta should reference prior session id %q, got %q", priorID, newSess.MetaJSON)
	}
	subs, err := deps.Store.SubtaskListBySession(res.SessionID, 0, 0)
	if err != nil {
		t.Fatalf("SubtaskListBySession: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("subtasks: got %d want 1 (resume creates exactly one)", len(subs))
	}
	if subs[0].Status != "completed" {
		t.Fatalf("subtask status: got %q want completed", subs[0].Status)
	}
	if subs[0].ProviderSessionID != "prov-sess-2" {
		t.Fatalf("subtask provider session id: got %q want prov-sess-2 (from the resume call result)", subs[0].ProviderSessionID)
	}
}

func TestSupervisor_Resume_WorkerOverride(t *testing.T) {
	deps := newTestDeps(t)

	// "old" was the worker on the prior session; it must NOT be called.
	oldCalls := atomic.Int32{}
	oldWorker := &fakeResumableProvider{
		fakeProvider: fakeProvider{name: "old"},
		resume: func(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			oldCalls.Add(1)
			return provider.RunResult{FinalText: "old"}, nil
		},
	}
	// "new" is the requested override; this is the one we expect to run.
	newCalls := atomic.Int32{}
	newWorker := &fakeResumableProvider{
		fakeProvider: fakeProvider{name: "new"},
		resume: func(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			newCalls.Add(1)
			return provider.RunResult{FinalText: "new answer"}, nil
		},
	}
	if err := deps.Registry.Register(oldWorker, false); err != nil {
		t.Fatalf("register old: %v", err)
	}
	if err := deps.Registry.Register(newWorker, false); err != nil {
		t.Fatalf("register new: %v", err)
	}
	priorID := seedPriorSession(t, deps, "old", "prov-sess-1")

	res, err := New(deps).Resume(context.Background(), ResumeRequest{
		PriorSessionID: priorID,
		Goal:           "follow up",
		WorkerName:     "new",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if oldCalls.Load() != 0 {
		t.Fatalf("override ignored: old worker was invoked %d times", oldCalls.Load())
	}
	if newCalls.Load() != 1 {
		t.Fatalf("override worker calls: got %d want 1", newCalls.Load())
	}

	newSess, err := deps.Store.GetSession(res.SessionID)
	if err != nil {
		t.Fatalf("GetSession new: %v", err)
	}
	if newSess.Worker != "new" {
		t.Fatalf("persisted worker on new session: got %q want %q (override should take effect)", newSess.Worker, "new")
	}
}

func TestSupervisor_Resume_HeadlessErrorMarksSessionFailed(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeResumableProvider{
		fakeProvider: fakeProvider{name: "worker"},
		resume: func(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{}, fmt.Errorf("kaboom: %w", provider.ErrWorkerFailed)
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	priorID := seedPriorSession(t, deps, "worker", "prov-sess-1")

	res, err := New(deps).Resume(context.Background(), ResumeRequest{
		PriorSessionID: priorID,
		Goal:           "follow up",
	})
	if err == nil {
		t.Fatalf("expected ResumeHeadless error to surface, got nil")
	}
	if res.Status != "failed" {
		t.Fatalf("status: got %q want failed", res.Status)
	}
	if res.SessionID == "" {
		t.Fatalf("expected a session id on failed resume")
	}

	newSess, err := deps.Store.GetSession(res.SessionID)
	if err != nil {
		t.Fatalf("GetSession new: %v", err)
	}
	if newSess.Status != "failed" {
		t.Fatalf("persisted status: got %q want failed", newSess.Status)
	}
	subs, err := deps.Store.SubtaskListBySession(res.SessionID, 0, 0)
	if err != nil {
		t.Fatalf("SubtaskListBySession: %v", err)
	}
	if len(subs) != 1 || subs[0].Status != "failed" {
		t.Fatalf("expected one failed subtask, got %+v", subs)
	}
	if subs[0].ErrorKind != "worker" {
		t.Fatalf("subtask error kind: got %q want %q", subs[0].ErrorKind, "worker")
	}
}

// TestSupervisor_Resume_BudgetExhausted_AbortsCleanly proves the budget
// pre/post-flight wiring in Resume mirrors what Supervisor.Run does: a
// resume turn whose provider reports more tokens than MaxTokens permits
// must abort with status="budget_exhausted" and emit a budget_exhausted
// trajectory event — not a generic "failed".
//
// This is the supervisor-side enforcement of STATUS-AGENTIC-OS.md
// Theme A's deferred piece (item 14 "budget enforcement missing in
// uta resume").
func TestSupervisor_Resume_BudgetExhausted_AbortsCleanly(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	worker := &fakeResumableProvider{
		fakeProvider: fakeProvider{name: "worker"},
		resume: func(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			// 200 tokens reported — exceeds the 100-token MaxTokens cap.
			return provider.RunResult{
				FinalText: "continued",
				SessionID: "prov-sess-2",
				TokensIn:  120,
				TokensOut: 80,
			}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	priorID := seedPriorSession(t, deps, "worker", "prov-sess-1")

	res, err := New(deps).Resume(context.Background(), ResumeRequest{
		PriorSessionID: priorID,
		Goal:           "follow up",
		MaxTokens:      100, // intentionally small — the call will exceed
	})
	if err == nil {
		t.Fatalf("expected ErrBudgetExceeded, got nil")
	}
	if !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
	if res.Status != "budget_exhausted" {
		t.Fatalf("Status: got %q want budget_exhausted", res.Status)
	}

	newSess, gerr := deps.Store.GetSession(res.SessionID)
	if gerr != nil {
		t.Fatalf("GetSession: %v", gerr)
	}
	if newSess.Status != "budget_exhausted" {
		t.Fatalf("persisted status: got %q want budget_exhausted", newSess.Status)
	}

	// Drain a few events to confirm budget_exhausted was emitted.
	deps.Bus.Shutdown()
	sawExhausted := false
	for ev := range collected {
		if ev.Kind == trajectory.BudgetExhausted {
			sawExhausted = true
			break
		}
	}
	if !sawExhausted {
		t.Fatalf("expected a budget_exhausted trajectory event, found none")
	}
}
