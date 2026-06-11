package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

// TestSupervisor_Run_WhiteboardInjectsCrossModeNote is the iter-18 acceptance:
// an ops-mode run writes a "blocker" note for dev; the next dev-mode run sees
// it injected into its planner prompt context.
func TestSupervisor_Run_WhiteboardInjectsCrossModeNote(t *testing.T) {
	deps := newTestDeps(t)
	deps.Whiteboard = whiteboard.New(deps.Store.DB)

	// Simulate the ops-mode write that preceded this run.
	if _, err := deps.Whiteboard.Set(whiteboard.Entry{
		Key:           "blocker:auth-rewrite",
		ValueJSON:     `{"note":"freeze merges until legal signs off"}`,
		AuthorMode:    "ops",
		AuthorSession: "sess-ops-1",
	}); err != nil {
		t.Fatalf("seed whiteboard: %v", err)
	}

	var (
		mu              sync.Mutex
		capturedPrompts []string
	)
	planner := &fakeProvider{
		name: "planner",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			mu.Lock()
			capturedPrompts = append(capturedPrompts, prompt)
			mu.Unlock()
			return provider.RunResult{
				FinalText: `{"subtasks":[{"id":"a","title":"a","prompt":"do a"}]}`,
			}, nil
		},
	}
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{FinalText: "ok"}, nil
		},
	}
	if err := deps.Registry.Register(planner, false); err != nil {
		t.Fatalf("register planner: %v", err)
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	_, err := New(deps).Run(context.Background(), RunRequest{
		Goal:          "ship the auth refactor",
		WorkerName:    "worker",
		PlannerName:   "planner",
		ModeName:      "dev",
		SkipSynthesis: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(capturedPrompts) == 0 {
		t.Fatal("planner was not called")
	}
	got := capturedPrompts[0]
	if !strings.Contains(got, "## Shared whiteboard") {
		t.Fatalf("planner prompt missing whiteboard block:\n%s", got)
	}
	if !strings.Contains(got, "blocker:auth-rewrite") {
		t.Fatalf("planner prompt missing whiteboard key:\n%s", got)
	}
	if !strings.Contains(got, "ops") {
		t.Fatalf("planner prompt missing author mode:\n%s", got)
	}
	if !strings.Contains(got, "freeze merges") {
		t.Fatalf("planner prompt missing whiteboard value preview:\n%s", got)
	}
}

// TestSupervisor_Run_WhiteboardBlock_EmptyWhenNoEntries confirms the block
// is skipped entirely when nothing has been written. The planner prompt
// stays identical to the no-whiteboard baseline.
func TestSupervisor_Run_WhiteboardBlock_EmptyWhenNoEntries(t *testing.T) {
	deps := newTestDeps(t)
	deps.Whiteboard = whiteboard.New(deps.Store.DB)

	var capturedPrompt string
	planner := &fakeProvider{
		name: "planner",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			capturedPrompt = prompt
			return provider.RunResult{
				FinalText: `{"subtasks":[{"id":"a","title":"a","prompt":"do a"}]}`,
			}, nil
		},
	}
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{FinalText: "ok"}, nil
		},
	}
	if err := deps.Registry.Register(planner, false); err != nil {
		t.Fatalf("register planner: %v", err)
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	_, err := New(deps).Run(context.Background(), RunRequest{
		Goal: "g", WorkerName: "worker", PlannerName: "planner", SkipSynthesis: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(capturedPrompt, "## Shared whiteboard") {
		t.Fatalf("did not expect whiteboard block when board is empty:\n%s", capturedPrompt)
	}
}

// TestSupervisor_Run_WhiteboardToolCall_EmitsSemanticEvents verifies that a
// worker invoking one of the uta_whiteboard_* MCP tools causes the supervisor
// to emit a parallel WhiteboardSet / WhiteboardGet trajectory event in
// addition to the generic SubtaskToolCall. Without this, the WhiteboardSet
// kind has no producer and observability surfaces have to parse tool_call
// payloads to find whiteboard activity.
func TestSupervisor_Run_WhiteboardToolCall_EmitsSemanticEvents(t *testing.T) {
	deps := newTestDeps(t)
	deps.Whiteboard = whiteboard.New(deps.Store.DB)

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
				Payload: []byte(`{"name":"uta_whiteboard_set","input":{"key":"blocker:x","value_json":"{\"note\":\"freeze\"}","author_mode":"ops"}}`),
			}
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"uta_whiteboard_get","input":{"key":"blocker:x"}}`),
			}
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"name":"uta_whiteboard_list","input":{}}`),
			}
			// A non-whiteboard tool call to confirm we don't over-emit.
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
		Goal:           "g",
		WorkerName:     "worker",
		SkipSynthesis:  true,
		PreSetSubtasks: []SubtaskSpec{{ID: "s1", Title: "t", Prompt: "p"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	deps.Bus.Shutdown()

	var sets, gets, toolCalls int
	for ev := range collected {
		switch ev.Kind {
		case trajectory.WhiteboardSet:
			sets++
			if !strings.Contains(string(ev.Payload), `"source":"tool_call"`) {
				t.Errorf("WhiteboardSet missing source=tool_call: %s", string(ev.Payload))
			}
		case trajectory.WhiteboardGet:
			gets++
			if !strings.Contains(string(ev.Payload), `"source":"tool_call"`) {
				t.Errorf("WhiteboardGet missing source=tool_call: %s", string(ev.Payload))
			}
		case trajectory.SubtaskToolCall:
			toolCalls++
		}
	}
	if sets != 1 {
		t.Errorf("WhiteboardSet events: got %d want 1", sets)
	}
	if gets != 2 {
		t.Errorf("WhiteboardGet events (get + list): got %d want 2", gets)
	}
	if toolCalls != 4 {
		t.Errorf("SubtaskToolCall passthrough: got %d want 4", toolCalls)
	}
}

// TestSupervisor_Run_WhiteboardBlock_EmitsPlannerInjectEvent locks in the
// observability contract for the planner-side read: when whiteboardBlock
// rendered content into the planner prompt, a WhiteboardGet trajectory
// event is emitted with source=planner_inject and the rendered keys, so
// retrospectives can attribute cross-mode reads even when no worker tool
// call is involved. Without this assertion, the planner-inject path can
// silently lose its emit and the only observable difference is a missing
// log line nobody is grepping for.
func TestSupervisor_Run_WhiteboardBlock_EmitsPlannerInjectEvent(t *testing.T) {
	deps := newTestDeps(t)
	deps.Whiteboard = whiteboard.New(deps.Store.DB)
	if _, err := deps.Whiteboard.Set(whiteboard.Entry{
		Key:        "blocker:auth",
		ValueJSON:  `{"note":"freeze"}`,
		AuthorMode: "ops",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	planner := &fakeProvider{
		name: "planner",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{
				FinalText: `{"subtasks":[{"id":"a","title":"a","prompt":"do a"}]}`,
			}, nil
		},
	}
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{FinalText: "ok"}, nil
		},
	}
	if err := deps.Registry.Register(planner, false); err != nil {
		t.Fatalf("register planner: %v", err)
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	_, err := New(deps).Run(context.Background(), RunRequest{
		Goal: "g", WorkerName: "worker", PlannerName: "planner", SkipSynthesis: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	deps.Bus.Shutdown()

	var plannerInjectSeen bool
	for ev := range collected {
		if ev.Kind != trajectory.WhiteboardGet {
			continue
		}
		if !strings.Contains(string(ev.Payload), `"source":"planner_inject"`) {
			continue
		}
		plannerInjectSeen = true
		if !strings.Contains(string(ev.Payload), "blocker:auth") {
			t.Errorf("planner_inject payload missing seeded key: %s", string(ev.Payload))
		}
	}
	if !plannerInjectSeen {
		t.Fatalf("expected at least one WhiteboardGet with source=planner_inject")
	}
}

// TestSupervisor_Run_WhiteboardToolCall_UnrelatedToolNoEmit guards the
// no-op path: tool_call events that don't name a uta_whiteboard_* tool
// must not trigger any WhiteboardSet / WhiteboardGet semantic emit.
// Without this guard, every tool_call would inflate whiteboard metrics.
func TestSupervisor_Run_WhiteboardToolCall_UnrelatedToolNoEmit(t *testing.T) {
	deps := newTestDeps(t)
	deps.Whiteboard = whiteboard.New(deps.Store.DB)

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
			// A bogus payload missing a "name" field — exercise the
			// extract-fail path. Must not crash and must not emit.
			events <- provider.Event{
				Kind:    provider.EventToolCall,
				Payload: []byte(`{"input":{"key":"x"}}`),
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

	for ev := range collected {
		if ev.Kind == trajectory.WhiteboardSet || ev.Kind == trajectory.WhiteboardGet {
			t.Fatalf("unexpected %s event for non-whiteboard tool call: %s", ev.Kind, string(ev.Payload))
		}
	}
}
