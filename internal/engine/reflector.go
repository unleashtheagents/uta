package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// CriticSpec is one auditor persona run during a Reflector iteration.
type CriticSpec struct {
	ID     string // stable identifier
	Title  string // display name
	Prompt string // appended after the standard auditor preamble
	Worker string // provider name; defaults to ReflectorRequest.DefaultWorker
}

// ReviserSpec is the optional fixer step run after findings aggregate.
type ReviserSpec struct {
	Prompt string // appended after the standard reviser preamble
	Worker string
	Gate   *Gate // optional verification gate after each revise (e.g. forge test)
}

// StopCondition decides when the Reflector loop terminates.
type StopCondition string

const (
	StopAtMaxIterations StopCondition = "max_iterations"
	StopAtNoHigh        StopCondition = "no_high_findings"
	StopAtNoMedOrAbove  StopCondition = "no_med_or_above"
	StopAtNoFindings    StopCondition = "no_findings"
)

// ReflectorRequest configures a producer → critics → revise loop.
type ReflectorRequest struct {
	Goal          string        // overall objective; appears in critic preamble
	InputSummary  string        // e.g. "the Solidity code under ./contracts"
	Critics       []CriticSpec  // ≥1 unless Tools is non-empty
	Tools         []ToolSpec    // external analysis tools that join the findings stream
	Reviser       *ReviserSpec  // optional; nil = audit-only (single iteration)
	MaxIterations int           // hard ceiling; default 3
	StopWhen      StopCondition // default StopAtNoHigh
	ToolsOnly     bool          // when true, skip critics and run only tools each iter

	DefaultWorker  string
	MaxParallel    int           // critic parallelism within an iteration
	CriticTimeout  time.Duration
	ReviseTimeout  time.Duration
	RunTimeout     time.Duration

	PreApproveTools []string
	Workdir         string
	Env             []string
	WorkflowPath    string

	// ContextDir is where iteration findings get persisted. Empty when not
	// running inside a project — findings are then only available via the
	// trajectory.
	ContextDir string
}

// ReflectorResult summarizes the run.
type ReflectorResult struct {
	SessionID         string
	Status            string // "completed" | "failed" | "cancelled" | "partial"
	Iterations        int
	StoppedBecause    string
	FinalFindings     *FindingsReport
	HistoryRefs       []string // blob refs for findings.<N>.json
	FindingsLatestRef string   // ctx path to latest findings.json (if in project)
}

const criticPreambleTmpl = `You are an auditor running in the uta orchestration engine.

Goal of this audit: %s

Material under review: %s

Workdir for any file inspection: %s

Return ONLY a single JSON object with this shape — no prose, no markdown, no code fences:

{"findings": [
  {
    "id": "<short stable identifier>",
    "severity": "HIGH" | "MEDIUM" | "LOW" | "INFO",
    "title": "<one-line summary>",
    "body": "<expanded explanation; what is wrong and why it matters>",
    "file": "<relative path; optional>",
    "line": <integer; optional>,
    "snippet": "<minimal offending code; optional>"
  }
]}

If you find nothing, return {"findings": []} — do not invent issues.

Your specific lens:
%s`

const reviserPreambleTmpl = `You are the reviser in an audit loop. The findings below were produced by
auditors reviewing this codebase. Apply concrete fixes to the source files,
preserving public API where possible. Be specific. After your edits the same
auditors will re-run.

Goal: %s

Workdir: %s

Findings to address (severity-sorted):
%s

Your reviser charter:
%s`

// RunReflector executes the audit / reflect loop end-to-end. Sessions and
// subtasks are persisted to the supervisor's store. Each iteration writes a
// FindingsReport to the project context directory if one is set.
func (s *Supervisor) RunReflector(ctx context.Context, req ReflectorRequest) (ReflectorResult, error) {
	if !req.ToolsOnly && len(req.Critics) == 0 && len(req.Tools) == 0 {
		return ReflectorResult{}, errors.New("reflector: at least one critic or tool is required")
	}
	if req.ToolsOnly && len(req.Tools) == 0 {
		return ReflectorResult{}, errors.New("reflector: tools-only mode requires at least one tool")
	}
	if strings.TrimSpace(req.InputSummary) == "" {
		return ReflectorResult{}, errors.New("reflector: input_summary is required")
	}
	if req.MaxIterations <= 0 {
		req.MaxIterations = 3
	}
	if req.StopWhen == "" {
		req.StopWhen = StopAtNoHigh
	}
	if req.DefaultWorker == "" && !req.ToolsOnly {
		return ReflectorResult{}, errors.New("reflector: default_worker is required (unless tools-only)")
	}
	if req.MaxParallel <= 0 {
		req.MaxParallel = 4
	}
	if req.CriticTimeout <= 0 {
		req.CriticTimeout = 10 * time.Minute
	}
	if req.ReviseTimeout <= 0 {
		req.ReviseTimeout = 15 * time.Minute
	}
	if req.RunTimeout <= 0 {
		req.RunTimeout = 2 * time.Hour
	}

	if req.DefaultWorker != "" {
		if _, ok := s.deps.Registry.Get(req.DefaultWorker); !ok {
			return ReflectorResult{}, fmt.Errorf("reflector: default worker %q not registered", req.DefaultWorker)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, req.RunTimeout)
	defer cancel()

	sessionID := uuid.NewString()
	if err := s.deps.Store.CreateSession(store.Session{
		ID:           sessionID,
		Goal:         req.Goal,
		Worker:       req.DefaultWorker,
		Status:       "running",
		CreatedAt:    time.Now(),
		WorkflowPath: req.WorkflowPath,
		MetaJSON:     reflectorMeta(req),
	}); err != nil {
		return ReflectorResult{}, fmt.Errorf("create session: %w", err)
	}

	s.emit(sessionID, "", trajectory.GoalReceived, map[string]any{
		"goal": req.Goal, "worker": req.DefaultWorker, "strategy": "reflector",
		"critics":        len(req.Critics),
		"tools":          len(req.Tools),
		"tools_only":     req.ToolsOnly,
		"max_iterations": req.MaxIterations,
		"stop_when":      string(req.StopWhen),
		"reviser":        req.Reviser != nil,
	})

	res := ReflectorResult{SessionID: sessionID}
	ord := 0 // sequential subtask ord across iterations

	for iter := 1; iter <= req.MaxIterations; iter++ {
		s.emit(sessionID, "", trajectory.IterationStarted, map[string]any{"iteration": iter})

		// --- run critics + tools in parallel ---
		perCritic := map[string][]Finding{}
		var perCriticMu sync.Mutex
		sem := make(chan struct{}, req.MaxParallel)
		var wg sync.WaitGroup
		anyCriticErr := false

		// Critics first (unless we're in tools-only mode).
		critics := req.Critics
		if req.ToolsOnly {
			critics = nil
		}
		for _, c := range critics {
			c := c
			ord++
			subtaskID := uuid.NewString()
			worker := c.Worker
			if worker == "" {
				worker = req.DefaultWorker
			}
			prov, ok := s.deps.Registry.Get(worker)
			if !ok {
				anyCriticErr = true
				s.emit(sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
					"critic": c.ID, "error": "worker not registered", "worker": worker,
				})
				continue
			}
			prompt := fmt.Sprintf(criticPreambleTmpl, req.Goal, req.InputSummary, req.Workdir, c.Prompt)
			promptRef, _ := s.deps.Blobs.Put([]byte(prompt), "txt")
			_ = s.deps.Store.CreateSubtask(store.Subtask{
				ID: subtaskID, SessionID: sessionID, Ord: ord,
				Title: fmt.Sprintf("iter %d · critic %s", iter, c.ID),
				PromptRef: promptRef, Worker: worker, Status: "running",
			})
			s.emit(sessionID, subtaskID, trajectory.CriticStarted, map[string]any{
				"iteration": iter, "critic": c.ID, "worker": worker,
			})

			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				start := time.Now()
				subCtx, subCancel := context.WithTimeout(ctx, req.CriticTimeout)
				result, err := s.callProvider(subCtx, sessionID, subtaskID, prov, prompt, RunRequest{
					Workdir: req.Workdir, Env: req.Env, SubtaskTimeout: req.CriticTimeout,
					PreApproveTools: req.PreApproveTools,
				})
				subCancel()
				done := time.Now()

				rawRef := ""
				if len(result.RawOutput) > 0 {
					if ref, perr := s.deps.Blobs.Put(result.RawOutput, providerBlobExt(worker)); perr == nil {
						rawRef = ref
					}
				}

				if err != nil {
					perCriticMu.Lock()
					anyCriticErr = true
					perCriticMu.Unlock()
					kind := classifyError(err)
					s.emit(sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
						"critic": c.ID, "error": err.Error(), "kind": kind,
					})
					_ = s.deps.Store.UpdateSubtask(store.Subtask{
						ID: subtaskID, ProviderSessionID: result.SessionID, Status: "failed",
						StartedAt: &start, CompletedAt: &done,
						ResultText: truncate(result.FinalText, 8000), RawOutputRef: rawRef,
						Error: err.Error(), ErrorKind: kind,
					})
					return
				}

				findings, parseErr := ParseFindings(result.FinalText)
				if parseErr != nil {
					perCriticMu.Lock()
					anyCriticErr = true
					perCriticMu.Unlock()
					s.emit(sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
						"critic": c.ID, "error": parseErr.Error(), "kind": "parse",
					})
					_ = s.deps.Store.UpdateSubtask(store.Subtask{
						ID: subtaskID, ProviderSessionID: result.SessionID, Status: "failed",
						StartedAt: &start, CompletedAt: &done,
						ResultText: truncate(result.FinalText, 8000), RawOutputRef: rawRef,
						Error: parseErr.Error(), ErrorKind: "parse",
					})
					return
				}

				perCriticMu.Lock()
				perCritic[c.ID] = findings
				perCriticMu.Unlock()
				s.emit(sessionID, subtaskID, trajectory.CriticCompleted, map[string]any{
					"critic": c.ID, "findings": len(findings),
				})
				_ = s.deps.Store.UpdateSubtask(store.Subtask{
					ID: subtaskID, ProviderSessionID: result.SessionID, Status: "completed",
					StartedAt: &start, CompletedAt: &done,
					ResultText: truncate(result.FinalText, 8000), RawOutputRef: rawRef,
				})
			}()
		}

		// --- launch tools in the same wave ---
		for _, t := range req.Tools {
			spec := ResolveToolSpec(t)
			if spec.Workdir == "" {
				spec.Workdir = req.Workdir
			}
			ord++
			subtaskID := uuid.NewString()
			promptRef, _ := s.deps.Blobs.Put(
				[]byte(fmt.Sprintf("tool: %s %s\n(adapter: %s)\n", spec.Cmd, strings.Join(spec.Args, " "), spec.Adapter)),
				"txt",
			)
			_ = s.deps.Store.CreateSubtask(store.Subtask{
				ID: subtaskID, SessionID: sessionID, Ord: ord,
				Title:     fmt.Sprintf("iter %d · tool %s", iter, spec.ID),
				PromptRef: promptRef, Worker: "tool:" + spec.ID, Status: "running",
			})
			s.emit(sessionID, subtaskID, trajectory.ToolStarted, map[string]any{
				"iteration": iter, "tool": spec.ID, "cmd": spec.Cmd, "adapter": spec.Adapter,
			})

			wg.Add(1)
			specCopy := spec
			subID := subtaskID
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				start := time.Now()
				result := runTool(ctx, specCopy)
				done := time.Now()

				var rawRef string
				if len(result.Stdout) > 0 {
					if r, perr := s.deps.Blobs.Put([]byte(result.Stdout), "txt"); perr == nil {
						rawRef = r
					}
				}

				if result.Err != nil {
					perCriticMu.Lock()
					anyCriticErr = true
					perCriticMu.Unlock()
					s.emit(sessionID, subID, trajectory.ToolFailed, map[string]any{
						"tool":  specCopy.ID,
						"error": result.Err.Error(),
						"exit":  result.ExitCode,
					})
					_ = s.deps.Store.UpdateSubtask(store.Subtask{
						ID: subID, Status: "failed",
						StartedAt: &start, CompletedAt: &done,
						ResultText:  truncate(result.Stdout, 8000),
						RawOutputRef: rawRef,
						Error:       result.Err.Error(),
						ErrorKind:   "tool",
					})
					return
				}

				perCriticMu.Lock()
				perCritic[specCopy.ID] = result.Findings
				perCriticMu.Unlock()
				s.emit(sessionID, subID, trajectory.ToolCompleted, map[string]any{
					"tool":        specCopy.ID,
					"findings":    len(result.Findings),
					"exit":        result.ExitCode,
					"duration_ms": result.Duration.Milliseconds(),
				})
				_ = s.deps.Store.UpdateSubtask(store.Subtask{
					ID: subID, Status: "completed",
					StartedAt: &start, CompletedAt: &done,
					ResultText:   truncate(result.Stdout, 8000),
					RawOutputRef: rawRef,
				})
			}()
		}

		wg.Wait()

		// --- aggregate ---
		report := Aggregate(iter, perCritic)
		ref, _ := s.persistFindings(req.ContextDir, &report)
		if ref != "" {
			res.HistoryRefs = append(res.HistoryRefs, ref)
			res.FindingsLatestRef = filepath.Join(req.ContextDir, "findings.json")
		}
		res.FinalFindings = &report
		res.Iterations = iter
		s.emit(sessionID, "", trajectory.FindingsAggregated, map[string]any{
			"iteration": iter, "stats": report.Stats, "highest": string(report.HighestSeverity()),
			"critics_failed": anyCriticErr,
		})
		s.emit(sessionID, "", trajectory.IterationCompleted, map[string]any{
			"iteration": iter, "stats": report.Stats,
		})

		// --- stop conditions ---
		stop, why := shouldStop(req.StopWhen, &report, iter, req.MaxIterations)
		if stop {
			res.StoppedBecause = why
			break
		}
		if req.Reviser == nil {
			res.StoppedBecause = "no_reviser"
			break
		}

		// --- revise ---
		if revErr := s.runReviser(ctx, sessionID, &ord, iter, &report, req); revErr != nil {
			s.emit(sessionID, "", trajectory.RunFailed, map[string]any{"reason": revErr.Error(), "iteration": iter})
			_ = s.deps.Store.MarkSession(sessionID, "failed", "")
			res.Status = "failed"
			return res, revErr
		}
	}

	if res.StoppedBecause == "" {
		res.StoppedBecause = "max_iterations"
	}
	if res.Status == "" {
		res.Status = "completed"
	}
	finalRef := ""
	if res.FinalFindings != nil {
		if b, err := json.MarshalIndent(res.FinalFindings, "", "  "); err == nil {
			if r, perr := s.deps.Blobs.Put(b, "json"); perr == nil {
				finalRef = r
			}
		}
	}
	_ = s.deps.Store.MarkSession(sessionID, res.Status, finalRef)
	s.emit(sessionID, "", trajectory.ReflectorCompleted, map[string]any{
		"iterations": res.Iterations, "stopped_because": res.StoppedBecause,
		"final_stats": func() map[string]int {
			if res.FinalFindings != nil {
				return res.FinalFindings.Stats
			}
			return nil
		}(),
	})
	s.emit(sessionID, "", trajectory.RunCompleted, map[string]any{
		"status": res.Status, "iterations": res.Iterations,
	})
	return res, nil
}

func (s *Supervisor) runReviser(ctx context.Context, sessionID string, ord *int, iter int, report *FindingsReport, req ReflectorRequest) error {
	*ord++
	subtaskID := uuid.NewString()
	worker := req.Reviser.Worker
	if worker == "" {
		worker = req.DefaultWorker
	}
	prov, ok := s.deps.Registry.Get(worker)
	if !ok {
		return fmt.Errorf("reviser worker %q not registered", worker)
	}

	findingsBlob, _ := json.MarshalIndent(report, "", "  ")
	prompt := fmt.Sprintf(reviserPreambleTmpl, req.Goal, req.Workdir, string(findingsBlob), req.Reviser.Prompt)
	promptRef, _ := s.deps.Blobs.Put([]byte(prompt), "txt")
	_ = s.deps.Store.CreateSubtask(store.Subtask{
		ID: subtaskID, SessionID: sessionID, Ord: *ord,
		Title: fmt.Sprintf("iter %d · revise", iter),
		PromptRef: promptRef, Worker: worker, Status: "running",
	})
	s.emit(sessionID, subtaskID, trajectory.ReviseStarted, map[string]any{
		"iteration": iter, "worker": worker, "findings_to_address": len(report.Findings),
	})

	start := time.Now()
	subCtx, subCancel := context.WithTimeout(ctx, req.ReviseTimeout)
	result, err := s.callProvider(subCtx, sessionID, subtaskID, prov, prompt, RunRequest{
		Workdir: req.Workdir, Env: req.Env, SubtaskTimeout: req.ReviseTimeout,
		PreApproveTools: req.PreApproveTools,
	})
	subCancel()
	done := time.Now()

	rawRef := ""
	if len(result.RawOutput) > 0 {
		if r, perr := s.deps.Blobs.Put(result.RawOutput, providerBlobExt(worker)); perr == nil {
			rawRef = r
		}
	}

	if err != nil {
		kind := classifyError(err)
		s.emit(sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
			"iteration": iter, "error": err.Error(), "kind": kind,
		})
		_ = s.deps.Store.UpdateSubtask(store.Subtask{
			ID: subtaskID, ProviderSessionID: result.SessionID, Status: "failed",
			StartedAt: &start, CompletedAt: &done,
			ResultText: truncate(result.FinalText, 8000), RawOutputRef: rawRef,
			Error: err.Error(), ErrorKind: kind,
		})
		return err
	}

	// Optional gate after revise (e.g., forge test).
	if req.Reviser.Gate != nil {
		s.emit(sessionID, subtaskID, trajectory.GateStarted, map[string]any{
			"cmd": req.Reviser.Gate.Cmd, "iteration": iter,
		})
		gr := runGate(ctx, req.Reviser.Gate, req.Workdir)
		evKind := trajectory.GateFailed
		if gr.Passed() {
			evKind = trajectory.GatePassed
		}
		s.emit(sessionID, subtaskID, evKind, map[string]any{
			"cmd": req.Reviser.Gate.Cmd, "exit": gr.ExitCode,
			"duration_ms": gr.Duration.Milliseconds(),
			"stdout_tail": truncate(gr.Stdout, 2000),
			"stderr_tail": truncate(gr.Stderr, 2000),
		})
		if !gr.Passed() {
			// Mark revise as failed if its post-gate didn't pass; the next
			// audit iteration will likely catch the regression too.
			s.emit(sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
				"iteration": iter, "kind": "revise_gate_failed",
			})
			_ = s.deps.Store.UpdateSubtask(store.Subtask{
				ID: subtaskID, ProviderSessionID: result.SessionID, Status: "failed",
				StartedAt: &start, CompletedAt: &done,
				ResultText: truncate(result.FinalText, 8000), RawOutputRef: rawRef,
				Error: fmt.Sprintf("reviser gate %q failed (exit %d)", req.Reviser.Gate.Cmd, gr.ExitCode),
				ErrorKind: "gate",
			})
			return fmt.Errorf("reviser gate failed at iteration %d", iter)
		}
	}

	s.emit(sessionID, subtaskID, trajectory.ReviseCompleted, map[string]any{
		"iteration": iter, "chars": len(result.FinalText),
	})
	_ = s.deps.Store.UpdateSubtask(store.Subtask{
		ID: subtaskID, ProviderSessionID: result.SessionID, Status: "completed",
		StartedAt: &start, CompletedAt: &done,
		ResultText: truncate(result.FinalText, 8000), RawOutputRef: rawRef,
	})
	return nil
}

// persistFindings writes a FindingsReport to:
//   - <contextDir>/findings.<iter>.json   (history)
//   - <contextDir>/findings.json          (latest, mirrors the most recent iter)
// Returns the absolute path to the history file (or "" if contextDir is empty).
func (s *Supervisor) persistFindings(contextDir string, rep *FindingsReport) (string, error) {
	if contextDir == "" {
		return "", nil
	}
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", err
	}
	histPath := filepath.Join(contextDir, fmt.Sprintf("findings.%d.json", rep.Iteration))
	if err := os.WriteFile(histPath, data, 0o644); err != nil {
		return "", err
	}
	latest := filepath.Join(contextDir, "findings.json")
	if err := os.WriteFile(latest, data, 0o644); err != nil {
		return histPath, err
	}
	return histPath, nil
}

func shouldStop(cond StopCondition, rep *FindingsReport, iter, max int) (bool, string) {
	if iter >= max {
		return true, "max_iterations"
	}
	switch cond {
	case StopAtMaxIterations:
		// keep going until max
		return false, ""
	case StopAtNoFindings:
		if len(rep.Findings) == 0 {
			return true, "no_findings"
		}
	case StopAtNoMedOrAbove:
		if rep.HighestSeverity().Rank() < SevMedium.Rank() {
			return true, "no_med_or_above"
		}
	case StopAtNoHigh, "":
		if rep.HighestSeverity().Rank() < SevHigh.Rank() {
			return true, "no_high_findings"
		}
	}
	return false, ""
}

func reflectorMeta(req ReflectorRequest) string {
	meta := map[string]any{
		"strategy":       "reflector",
		"critics":        len(req.Critics),
		"max_iterations": req.MaxIterations,
		"stop_when":      string(req.StopWhen),
		"has_reviser":    req.Reviser != nil,
	}
	b, _ := json.Marshal(meta)
	return string(b)
}
