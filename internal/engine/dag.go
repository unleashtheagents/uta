package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// runDAG executes a plan whose subtasks have dependencies (Needs) and may
// have verification gates. Wave-by-wave execution: each wave is the set of
// not-yet-run subtasks whose deps are all completed. Within a wave, subtasks
// run in parallel under MaxParallel. A failed subtask cascades — its
// transitive dependents get marked skipped.
//
// Returns (outcomes, anyFailed, abortedByFailFast, err).
func (s *Supervisor) runDAG(ctx context.Context, sessionID string, plan Plan, req RunRequest) ([]SubtaskOutcome, bool, bool, error) {
	if err := validateDAG(plan); err != nil {
		return nil, true, true, fmt.Errorf("invalid DAG: %w", err)
	}

	n := len(plan.Subtasks)
	outcomes := make([]SubtaskOutcome, n)
	dbIDs := make([]string, n)
	workers := make([]string, n)
	provs := make([]provider.AgentProvider, n)

	for i, spec := range plan.Subtasks {
		subID := uuid.NewString()
		dbIDs[i] = subID

		workerName := req.WorkerName
		var prov provider.AgentProvider
		if spec.Worker != "" {
			if p, ok := s.deps.Registry.Get(spec.Worker); ok {
				prov = p
				workerName = spec.Worker
			}
		}
		if prov == nil {
			if p, ok := s.deps.Registry.Get(workerName); ok {
				prov = p
			}
		}
		workers[i] = workerName
		provs[i] = prov

		promptRef, _ := s.deps.Blobs.Put([]byte(spec.Prompt), "txt")
		_ = s.deps.Store.CreateSubtask(store.Subtask{
			ID: subID, SessionID: sessionID, Ord: i,
			Title: spec.Title, PromptRef: promptRef, Worker: workerName, Status: "pending",
		})
	}

	completed := map[string]bool{}
	failed := map[string]bool{}
	skipped := map[string]bool{}
	anyFailed := false

	var sem chan struct{}
	if req.MaxParallel > 0 {
		sem = make(chan struct{}, req.MaxParallel)
	}

	for {
		// Find all runnable in this wave; mark skips for tasks with failed deps.
		var wave []int
		allDone := true
		for i, spec := range plan.Subtasks {
			if completed[spec.ID] || failed[spec.ID] || skipped[spec.ID] {
				continue
			}
			allDone = false

			depBlocked := false
			depPending := false
			for _, dep := range spec.Needs {
				if failed[dep] || skipped[dep] {
					depBlocked = true
					break
				}
				if !completed[dep] {
					depPending = true
				}
			}
			if depBlocked {
				skipped[spec.ID] = true
				outcomes[i] = SubtaskOutcome{
					ID: spec.ID, Title: spec.Title, Failed: true,
					Result: "skipped: an upstream dependency failed",
				}
				s.emit(sessionID, dbIDs[i], trajectory.SubtaskSkipped, map[string]any{
					"reason": "upstream_failed", "deps": spec.Needs,
				})
				now := time.Now()
				_ = s.deps.Store.UpdateSubtask(store.Subtask{
					ID:          dbIDs[i],
					Status:      "skipped",
					CompletedAt: &now,
					Error:       "upstream dependency failed",
					ErrorKind:   "blocked",
				})
				continue
			}
			if depPending {
				continue
			}
			wave = append(wave, i)
		}

		if allDone {
			break
		}
		if len(wave) == 0 {
			// Defensive — validateDAG already caught cycles, but if we got
			// here something's off.
			return outcomes, true, false, fmt.Errorf("DAG execution stuck: no runnable subtasks remain")
		}

		// Execute the wave concurrently.
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, idx := range wave {
			i := idx
			wg.Add(1)
			go func() {
				defer wg.Done()
				if sem != nil {
					select {
					case sem <- struct{}{}:
					case <-ctx.Done():
						return
					}
					defer func() { <-sem }()
				}
				outcome := s.runDAGSubtask(ctx, sessionID, dbIDs[i], plan.Subtasks[i], provs[i], workers[i], req)
				mu.Lock()
				outcomes[i] = outcome
				if outcome.Failed {
					failed[plan.Subtasks[i].ID] = true
					anyFailed = true
				} else {
					completed[plan.Subtasks[i].ID] = true
				}
				mu.Unlock()
			}()
		}
		wg.Wait()

		if anyFailed && req.FailFast {
			// Cascade skips for everything still pending.
			for i, spec := range plan.Subtasks {
				if completed[spec.ID] || failed[spec.ID] || skipped[spec.ID] {
					continue
				}
				skipped[spec.ID] = true
				outcomes[i] = SubtaskOutcome{
					ID: spec.ID, Title: spec.Title, Failed: true,
					Result: "skipped: fail-fast triggered by a sibling failure",
				}
				s.emit(sessionID, dbIDs[i], trajectory.SubtaskSkipped, map[string]any{"reason": "fail_fast"})
				now := time.Now()
				_ = s.deps.Store.UpdateSubtask(store.Subtask{
					ID: dbIDs[i], Status: "skipped", CompletedAt: &now,
					Error: "fail-fast triggered", ErrorKind: "blocked",
				})
			}
			return outcomes, anyFailed, true, nil
		}
	}

	return outcomes, anyFailed, false, nil
}

// runDAGSubtask runs one subtask, then (if present) the gate, with optional
// retry-producer loop on gate failure.
func (s *Supervisor) runDAGSubtask(ctx context.Context, sessionID, subtaskID string, spec SubtaskSpec, prov provider.AgentProvider, workerName string, req RunRequest) SubtaskOutcome {
	startedAt := time.Now()
	_ = s.deps.Store.UpdateSubtask(store.Subtask{ID: subtaskID, Status: "running", StartedAt: &startedAt})
	s.emit(sessionID, subtaskID, trajectory.SubtaskStarted, map[string]any{
		"spec_id": spec.ID, "title": spec.Title, "worker": workerName, "needs": spec.Needs,
	})

	maxRetries := 0
	if spec.Gate != nil && spec.Gate.RetryProducer {
		maxRetries = spec.Gate.MaxRetries
		if maxRetries <= 0 {
			maxRetries = 1
		}
	}

	var lastResult provider.RunResult
	var lastErr error
	var gateOutput string

	for attempt := 0; attempt <= maxRetries; attempt++ {
		prompt := spec.Prompt
		if attempt > 0 && gateOutput != "" {
			prompt = spec.Prompt +
				"\n\n---\nYour previous attempt failed the verification gate (`" + spec.Gate.Cmd + "`). Captured output:\n\n" +
				gateOutput +
				"\n\nFix the underlying issue and produce a corrected result. Do not just restate the task."
			s.emit(sessionID, subtaskID, trajectory.ProducerRetried, map[string]any{
				"attempt": attempt, "max_retries": maxRetries, "gate_cmd": spec.Gate.Cmd,
			})
		}

		subCtx, subCancel := context.WithTimeout(ctx, req.SubtaskTimeout)
		res, err := s.callProvider(subCtx, sessionID, subtaskID, prov, prompt, req)
		subCancel()
		lastResult = res
		lastErr = err

		if err != nil {
			break
		}

		if spec.Gate == nil {
			break
		}

		s.emit(sessionID, subtaskID, trajectory.GateStarted, map[string]any{
			"cmd": spec.Gate.Cmd, "attempt": attempt,
		})
		gateRes := runGate(ctx, spec.Gate, req.Workdir)
		evKind := trajectory.GateFailed
		if gateRes.Passed() {
			evKind = trajectory.GatePassed
		}
		s.emit(sessionID, subtaskID, evKind, map[string]any{
			"cmd":          spec.Gate.Cmd,
			"exit":         gateRes.ExitCode,
			"duration_ms":  gateRes.Duration.Milliseconds(),
			"stdout_tail":  truncate(gateRes.Stdout, 2000),
			"stderr_tail":  truncate(gateRes.Stderr, 2000),
			"err":          errString(gateRes.Err),
		})

		if gateRes.Passed() {
			break
		}

		gateOutput = gateRes.CombinedOutputTail(4000)

		if attempt == maxRetries {
			lastErr = fmt.Errorf("gate %q failed after %d attempt(s): exit %d", spec.Gate.Cmd, attempt+1, gateRes.ExitCode)
			break
		}
	}

	completedAt := time.Now()
	rawRef := ""
	if len(lastResult.RawOutput) > 0 {
		if ref, perr := s.deps.Blobs.Put(lastResult.RawOutput, providerBlobExt(workerName)); perr == nil {
			rawRef = ref
		}
	}

	if lastErr != nil {
		kind := classifyError(lastErr)
		s.emit(sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
			"spec_id": spec.ID, "error": lastErr.Error(), "kind": kind,
		})
		_ = s.deps.Store.UpdateSubtask(store.Subtask{
			ID: subtaskID, ProviderSessionID: lastResult.SessionID, Status: "failed",
			StartedAt: &startedAt, CompletedAt: &completedAt,
			ResultText: truncate(lastResult.FinalText, 8000), RawOutputRef: rawRef,
			Error: lastErr.Error(), ErrorKind: kind,
		})
		return SubtaskOutcome{
			ID: spec.ID, Title: spec.Title,
			Result: fmt.Sprintf("failed (%s): %v", kind, lastErr), Failed: true,
		}
	}

	s.emit(sessionID, subtaskID, trajectory.SubtaskCompleted, map[string]any{
		"spec_id": spec.ID, "chars": len(lastResult.FinalText), "provider_session_id": lastResult.SessionID,
	})
	_ = s.deps.Store.UpdateSubtask(store.Subtask{
		ID: subtaskID, ProviderSessionID: lastResult.SessionID, Status: "completed",
		StartedAt: &startedAt, CompletedAt: &completedAt,
		ResultText: truncate(lastResult.FinalText, 8000), RawOutputRef: rawRef,
	})
	return SubtaskOutcome{ID: spec.ID, Title: spec.Title, Result: lastResult.FinalText}
}

// validateDAG checks the plan for missing dep references and cycles.
func validateDAG(plan Plan) error {
	idx := map[string]int{}
	for i, s := range plan.Subtasks {
		if s.ID == "" {
			return fmt.Errorf("subtask %d has empty id", i)
		}
		if _, dup := idx[s.ID]; dup {
			return fmt.Errorf("duplicate subtask id %q", s.ID)
		}
		idx[s.ID] = i
	}
	for _, s := range plan.Subtasks {
		for _, dep := range s.Needs {
			if _, ok := idx[dep]; !ok {
				return fmt.Errorf("subtask %q needs %q which does not exist", s.ID, dep)
			}
		}
	}
	// DFS cycle detection.
	color := map[string]int{} // 0=white 1=gray 2=black
	var dfs func(id string, path []string) error
	dfs = func(id string, path []string) error {
		switch color[id] {
		case 1:
			return fmt.Errorf("cycle: %s -> %s", strings.Join(append(path, id), " -> "), id)
		case 2:
			return nil
		}
		color[id] = 1
		for _, dep := range plan.Subtasks[idx[id]].Needs {
			if err := dfs(dep, append(path, id)); err != nil {
				return err
			}
		}
		color[id] = 2
		return nil
	}
	for _, s := range plan.Subtasks {
		if err := dfs(s.ID, nil); err != nil {
			return err
		}
	}
	return nil
}

func errString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}
