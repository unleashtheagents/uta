// Package engine houses the orchestration core. v1 implements only the
// supervisor pattern: a planner provider decomposes the goal into independent
// subtasks, each subtask runs in parallel against a worker provider, and a
// synthesizer provider produces the final answer.
//
// All behavior is library-shaped — no flag parsing, no stdout — so a TUI or
// MCP-server mode can reuse it directly.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// Deps is the wired infrastructure a Supervisor needs. The CLI (or a TUI)
// constructs this once per process and reuses it across runs.
type Deps struct {
	Store    *store.Store
	Blobs    *store.Blobs
	Recorder *trajectory.Recorder
	Bus      *trajectory.Bus
	Registry *provider.Registry
}

// RunRequest is what the user (via CLI flags or a YAML workflow) is asking
// the supervisor to do.
type RunRequest struct {
	Goal            string
	WorkerName      string        // required: the default worker for every subtask
	PlannerName     string        // optional: defaults to WorkerName
	SynthName       string        // optional: defaults to WorkerName
	MaxParallel     int           // default 4
	SubtaskTimeout  time.Duration // default 10m
	RunTimeout      time.Duration // default 30m
	FailFast        bool
	PreApproveTools []string
	Workdir         string
	MaxSubtasks     int // hard cap passed to the planner prompt (default 8)
	WorkflowPath    string
}

// RunResult is what the supervisor returns once a run is done (success or not).
type RunResult struct {
	SessionID   string
	Status      string // "completed" | "failed" | "partial" | "cancelled"
	FinalAnswer string
	Subtasks    []store.Subtask
}

// Supervisor runs one orchestration at a time. Reuse the same instance across
// many runs — it carries no per-run state.
type Supervisor struct {
	deps Deps
}

func New(deps Deps) *Supervisor { return &Supervisor{deps: deps} }

// Run executes the request end-to-end and persists everything to SQLite.
func (s *Supervisor) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if strings.TrimSpace(req.Goal) == "" {
		return RunResult{}, errors.New("goal is empty")
	}
	if req.WorkerName == "" {
		return RunResult{}, errors.New("worker is required")
	}
	worker, ok := s.deps.Registry.Get(req.WorkerName)
	if !ok {
		return RunResult{}, fmt.Errorf("worker %q not registered", req.WorkerName)
	}
	plannerName := firstNonEmpty(req.PlannerName, req.WorkerName)
	planner, ok := s.deps.Registry.Get(plannerName)
	if !ok {
		return RunResult{}, fmt.Errorf("planner %q not registered", plannerName)
	}
	synthName := firstNonEmpty(req.SynthName, req.WorkerName)
	synth, ok := s.deps.Registry.Get(synthName)
	if !ok {
		return RunResult{}, fmt.Errorf("synthesizer %q not registered", synthName)
	}

	if req.MaxParallel <= 0 {
		req.MaxParallel = 4
	}
	if req.SubtaskTimeout <= 0 {
		req.SubtaskTimeout = 10 * time.Minute
	}
	if req.RunTimeout <= 0 {
		req.RunTimeout = 30 * time.Minute
	}
	if req.MaxSubtasks <= 0 {
		req.MaxSubtasks = 8
	}

	ctx, cancel := context.WithTimeout(ctx, req.RunTimeout)
	defer cancel()

	sessionID := uuid.NewString()
	now := time.Now()
	if err := s.deps.Store.CreateSession(store.Session{
		ID:           sessionID,
		Goal:         req.Goal,
		Worker:       req.WorkerName,
		Planner:      plannerName,
		Status:       "running",
		CreatedAt:    now,
		WorkflowPath: req.WorkflowPath,
	}); err != nil {
		return RunResult{}, fmt.Errorf("create session: %w", err)
	}

	s.emit(sessionID, "", trajectory.GoalReceived, map[string]any{
		"goal": req.Goal, "worker": req.WorkerName, "planner": plannerName, "synth": synthName,
		"max_parallel": req.MaxParallel,
	})

	// ----- Planning -----
	plan, planErr := s.runPlanner(ctx, sessionID, planner, req)
	planFell := false
	if planErr != nil {
		s.emit(sessionID, "", trajectory.PlanFallback, map[string]any{"error": planErr.Error()})
		plan = FallbackPlan(req.Goal)
		planFell = true
	}
	s.emit(sessionID, "", trajectory.PlanProposed, map[string]any{
		"subtasks":     plan.Subtasks,
		"used_fallback": planFell,
	})

	// ----- Fan-out -----
	outcomes := make([]SubtaskOutcome, len(plan.Subtasks))
	sem := make(chan struct{}, req.MaxParallel)
	var wg sync.WaitGroup
	anyFailed := false
	var failedMu sync.Mutex

	for i, spec := range plan.Subtasks {
		i, spec := i, spec
		// Resolve the worker for this subtask (allow per-subtask override).
		subtaskWorker := worker
		subtaskWorkerName := req.WorkerName
		if spec.Worker != "" {
			if p, ok := s.deps.Registry.Get(spec.Worker); ok {
				subtaskWorker = p
				subtaskWorkerName = spec.Worker
			}
		}
		subtaskID := uuid.NewString()
		promptRef, err := s.deps.Blobs.Put([]byte(spec.Prompt), "txt")
		if err != nil {
			promptRef = ""
		}
		if err := s.deps.Store.CreateSubtask(store.Subtask{
			ID:        subtaskID,
			SessionID: sessionID,
			Ord:       i,
			Title:     spec.Title,
			PromptRef: promptRef,
			Worker:    subtaskWorkerName,
			Status:    "pending",
		}); err != nil {
			outcomes[i] = SubtaskOutcome{ID: spec.ID, Title: spec.Title, Result: "subtask create failed: " + err.Error(), Failed: true}
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				outcomes[i] = SubtaskOutcome{ID: spec.ID, Title: spec.Title, Result: "cancelled before start", Failed: true}
				return
			}
			defer func() { <-sem }()

			outcome := s.runSubtask(ctx, sessionID, subtaskID, spec, subtaskWorker, subtaskWorkerName, req)
			outcomes[i] = outcome
			if outcome.Failed {
				failedMu.Lock()
				anyFailed = true
				failedMu.Unlock()
				if req.FailFast {
					cancel()
				}
			}
		}()
	}
	wg.Wait()

	// Cancellation detection: distinguish user/timeout cancel from clean finish.
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(ctxErr, context.DeadlineExceeded) && !req.FailFast {
		s.emit(sessionID, "", trajectory.RunCancelled, map[string]any{"reason": ctxErr.Error()})
		_ = s.deps.Store.MarkSession(sessionID, "cancelled", "")
		return RunResult{SessionID: sessionID, Status: "cancelled"}, ctxErr
	}

	if anyFailed && req.FailFast {
		s.emit(sessionID, "", trajectory.RunFailed, map[string]any{"reason": "fail-fast: a subtask failed"})
		_ = s.deps.Store.MarkSession(sessionID, "failed", "")
		subs, _ := s.deps.Store.SubtaskListBySession(sessionID)
		return RunResult{SessionID: sessionID, Status: "failed", Subtasks: subs}, errors.New("a subtask failed (fail-fast)")
	}

	// ----- Synthesis -----
	s.emit(sessionID, "", trajectory.SynthesisStarted, map[string]any{"synth": synthName})
	synthPrompt := RenderSynthPrompt(req.Goal, outcomes)
	synthCtx, synthCancel := context.WithTimeout(ctx, req.SubtaskTimeout)
	defer synthCancel()
	final, synthErr := s.callProvider(synthCtx, sessionID, "", synth, synthPrompt, req)

	finalText := ""
	finalRef := ""
	if synthErr == nil {
		finalText = final.FinalText
		ref, _ := s.deps.Blobs.Put([]byte(finalText), "txt")
		finalRef = ref
	} else {
		// Synthesis failure isn't fatal — fall back to a join of subtask outputs.
		var b strings.Builder
		for _, o := range outcomes {
			fmt.Fprintf(&b, "## %s (%s)\n\n%s\n\n", o.Title, o.ID, o.Result)
		}
		finalText = strings.TrimSpace(b.String())
		ref, _ := s.deps.Blobs.Put([]byte(finalText), "txt")
		finalRef = ref
		s.emit(sessionID, "", trajectory.SynthesisCompleted, map[string]any{"error": synthErr.Error(), "fallback": "joined"})
	}
	if synthErr == nil {
		s.emit(sessionID, "", trajectory.SynthesisCompleted, map[string]any{"chars": len(finalText)})
	}

	status := "completed"
	if anyFailed {
		status = "partial"
	}
	_ = s.deps.Store.MarkSession(sessionID, status, finalRef)
	s.emit(sessionID, "", trajectory.RunCompleted, map[string]any{"status": status, "subtasks": len(plan.Subtasks)})

	subs, _ := s.deps.Store.SubtaskListBySession(sessionID)
	return RunResult{SessionID: sessionID, Status: status, FinalAnswer: finalText, Subtasks: subs}, nil
}

// runPlanner calls the planner and parses its output into a Plan.
func (s *Supervisor) runPlanner(ctx context.Context, sessionID string, planner provider.AgentProvider, req RunRequest) (Plan, error) {
	prompt := RenderPlannerPrompt(req.Goal, req.MaxSubtasks)
	s.emit(sessionID, "", trajectory.PlanRequested, map[string]any{"planner": planner.Name(), "chars": len(prompt)})

	plannerCtx, cancel := context.WithTimeout(ctx, req.SubtaskTimeout)
	defer cancel()

	result, err := s.callProvider(plannerCtx, sessionID, "", planner, prompt, req)
	if err != nil {
		return Plan{}, err
	}
	return ParsePlan(result.FinalText)
}

// runSubtask runs one subtask and returns its SubtaskOutcome. It records all
// trajectory events and updates the subtask row.
func (s *Supervisor) runSubtask(ctx context.Context, sessionID, subtaskID string, spec SubtaskSpec, prov provider.AgentProvider, workerName string, req RunRequest) SubtaskOutcome {
	startedAt := time.Now()
	if err := s.deps.Store.UpdateSubtask(store.Subtask{
		ID:        subtaskID,
		Status:    "running",
		StartedAt: &startedAt,
	}); err != nil {
		// Best-effort; an update miss isn't fatal for the run.
	}
	s.emit(sessionID, subtaskID, trajectory.SubtaskStarted, map[string]any{
		"spec_id": spec.ID, "title": spec.Title, "worker": workerName,
	})

	subCtx, cancel := context.WithTimeout(ctx, req.SubtaskTimeout)
	defer cancel()

	result, err := s.callProvider(subCtx, sessionID, subtaskID, prov, spec.Prompt, req)
	completedAt := time.Now()

	rawRef := ""
	if len(result.RawOutput) > 0 {
		if ref, perr := s.deps.Blobs.Put(result.RawOutput, providerBlobExt(workerName)); perr == nil {
			rawRef = ref
		}
	}

	if err != nil {
		kind := classifyError(err)
		s.emit(sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
			"spec_id": spec.ID, "error": err.Error(), "kind": kind,
		})
		_ = s.deps.Store.UpdateSubtask(store.Subtask{
			ID:                subtaskID,
			ProviderSessionID: result.SessionID,
			Status:            "failed",
			StartedAt:         &startedAt,
			CompletedAt:       &completedAt,
			ResultText:        truncate(result.FinalText, 8000),
			RawOutputRef:      rawRef,
			Error:             err.Error(),
			ErrorKind:         kind,
		})
		return SubtaskOutcome{
			ID:     spec.ID,
			Title:  spec.Title,
			Result: fmt.Sprintf("subtask failed (%s): %v", kind, err),
			Failed: true,
		}
	}

	s.emit(sessionID, subtaskID, trajectory.SubtaskCompleted, map[string]any{
		"spec_id": spec.ID, "chars": len(result.FinalText), "provider_session_id": result.SessionID,
	})
	_ = s.deps.Store.UpdateSubtask(store.Subtask{
		ID:                subtaskID,
		ProviderSessionID: result.SessionID,
		Status:            "completed",
		StartedAt:         &startedAt,
		CompletedAt:       &completedAt,
		ResultText:        truncate(result.FinalText, 8000),
		RawOutputRef:      rawRef,
	})
	return SubtaskOutcome{
		ID:     spec.ID,
		Title:  spec.Title,
		Result: result.FinalText,
	}
}

// callProvider runs a provider once with a single retry on ErrTransport.
// While the call is in flight, an internal goroutine drains the provider's
// event channel into uta's trajectory bus, tagging each event with the
// current session_id / subtask_id.
func (s *Supervisor) callProvider(ctx context.Context, sessionID, subtaskID string, prov provider.AgentProvider, prompt string, req RunRequest) (provider.RunResult, error) {
	opts := provider.RunOptions{
		Workdir:         req.Workdir,
		Timeout:         req.SubtaskTimeout,
		PreApproveTools: req.PreApproveTools,
	}
	for attempt := 0; attempt < 2; attempt++ {
		events := make(chan provider.Event, 64)
		drainDone := make(chan struct{})
		go func() {
			for ev := range events {
				s.publishProviderEvent(sessionID, subtaskID, ev)
			}
			close(drainDone)
		}()

		result, err := prov.RunHeadless(ctx, prompt, opts, events)
		close(events)
		<-drainDone

		if err == nil {
			return result, nil
		}
		if errors.Is(err, provider.ErrTransport) && attempt == 0 {
			s.emit(sessionID, subtaskID, trajectory.SubtaskStdout, map[string]any{
				"retry_after_transport_error": err.Error(),
			})
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		return result, err
	}
	return provider.RunResult{}, errors.New("unreachable: retry loop exited without result")
}

func (s *Supervisor) publishProviderEvent(sessionID, subtaskID string, ev provider.Event) {
	kind := mapProviderEventKind(ev.Kind)
	if kind == "" {
		return
	}
	s.emit(sessionID, subtaskID, kind, json.RawMessage(ev.Payload))
}

func mapProviderEventKind(k provider.EventKind) trajectory.Kind {
	switch k {
	case provider.EventAssistantText:
		return trajectory.SubtaskAssistantText
	case provider.EventToolCall:
		return trajectory.SubtaskToolCall
	case provider.EventToolResult:
		return trajectory.SubtaskToolResult
	case provider.EventStdoutChunk:
		return trajectory.SubtaskStdout
	case provider.EventError:
		return trajectory.SubtaskStdout
	case provider.EventSessionID:
		// Captured in the subtask row (provider_session_id) instead of as a trajectory event.
		return ""
	}
	return ""
}

// emit persists a trajectory event synchronously via the recorder and also
// publishes it on the bus for any optional live subscribers.
func (s *Supervisor) emit(sessionID, subtaskID string, kind trajectory.Kind, payload any) {
	var raw json.RawMessage
	switch v := payload.(type) {
	case nil:
		raw = json.RawMessage("{}")
	case json.RawMessage:
		raw = v
	case []byte:
		raw = json.RawMessage(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			b = []byte(`{"_marshal_error":true}`)
		}
		raw = json.RawMessage(b)
	}
	seq := s.deps.Recorder.AllocSeq(sessionID)
	ev := trajectory.Event{
		SessionID: sessionID,
		SubtaskID: subtaskID,
		Seq:       seq,
		Ts:        time.Now(),
		Kind:      kind,
		Payload:   raw,
	}
	s.deps.Recorder.PersistSync(ev)
	if s.deps.Bus != nil {
		s.deps.Bus.Publish(ev)
	}
}

func classifyError(err error) string {
	switch {
	case errors.Is(err, provider.ErrAuth):
		return "auth"
	case errors.Is(err, provider.ErrQuota):
		return "quota"
	case errors.Is(err, provider.ErrTimeout):
		return "timeout"
	case errors.Is(err, provider.ErrTransport):
		return "transport"
	case errors.Is(err, provider.ErrWorkerFailed):
		return "worker"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "unknown"
}

func providerBlobExt(workerName string) string {
	switch workerName {
	case "claude", "gemini":
		return workerName + ".jsonl"
	}
	return "txt"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... [truncated]"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
