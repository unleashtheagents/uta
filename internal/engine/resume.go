package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/budget"
	"github.com/unleashtheagents/uta/internal/engine/capability"
	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// ResumeRequest continues a prior orchestration: a new uta session is created
// but the underlying worker call goes to Resumable.ResumeHeadless against the
// provider session id captured on the prior run's last subtask. There is no
// planner step and no synthesis — this is one-shot follow-up.
//
// Policy enforcement (HITL, capability gates, budget caps) follows the same
// rules as RunRequest. Set via ApplyProfileToResume from the active
// MissionProfile; supervisor.Resume honors HITLTriggers, MaxTokens,
// MaxUSDCents, and PerCallMaxTokens identically to Run.
type ResumeRequest struct {
	PriorSessionID  string
	Goal            string
	WorkerName      string // optional override; defaults to prior session's worker
	SubtaskTimeout  time.Duration
	RunTimeout      time.Duration
	PreApproveTools []string
	Workdir         string
	Env             []string // additional env vars for the provider call

	// AllowedTools / DeniedTools are the capability-gate lists active for
	// the resumed turn. Same semantics as RunRequest's fields. Both empty
	// disables enforcement.
	AllowedTools []string
	DeniedTools  []string

	// ModeName is the active MissionProfile's name. Persisted on the new
	// session row so `uta mode list` and `uta dash` can attribute usage
	// per mode. Empty when the resume ran without a profile attached.
	ModeName string

	// HITL is the human-in-the-loop approver for tool-call gating during
	// the resumed turn. Nil disables HITL entirely (the gate is a no-op).
	// Mirrors RunRequest.HITL.
	HITL hitl.Approver

	// HITLTriggers, HITLTokenThreshold, HITLSeverity mirror RunRequest's
	// HITL config. Set by ApplyProfileToResume from the active profile.
	HITLTriggers       []string
	HITLTokenThreshold int
	HITLSeverity       string

	// MaxTokens / PerCallMaxTokens / MaxUSDCents mirror RunRequest. 0
	// disables the corresponding cap. ErrBudgetExceeded fires the same
	// way it does in Run when any cap is exceeded.
	MaxTokens        int64
	PerCallMaxTokens int64
	MaxUSDCents      int64

	// MCPConfigPath is an absolute path to a synthesized `.mcp.json`-style
	// file describing the MCP servers the resumed turn should see. Set by
	// ApplyProfileToResume from the active MissionProfile after probing,
	// or empty when no servers are wired. Forwarded onto RunOptions so
	// providers with CapMCP (claude, gemini) can pass it as --mcp-config.
	MCPConfigPath string
}

// Resume executes a follow-up turn. Returns the new session's RunResult.
func (s *Supervisor) Resume(ctx context.Context, req ResumeRequest) (RunResult, error) {
	if strings.TrimSpace(req.Goal) == "" {
		return RunResult{}, errors.New("--goal is required for resume")
	}
	prior, err := s.deps.Store.GetSession(req.PriorSessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunResult{}, fmt.Errorf("prior session not found: %s", req.PriorSessionID)
		}
		return RunResult{}, err
	}
	last, err := s.deps.Store.LastSubtask(req.PriorSessionID)
	if err != nil {
		return RunResult{}, fmt.Errorf("read prior subtask: %w", err)
	}
	if last.ProviderSessionID == "" {
		return RunResult{}, errors.New("prior session has no recorded provider session id; can't resume")
	}

	workerName := req.WorkerName
	if workerName == "" {
		workerName = prior.Worker
	}
	prov, ok := s.deps.Registry.Get(workerName)
	if !ok {
		return RunResult{}, fmt.Errorf("worker %q not registered", workerName)
	}
	resumable, ok := prov.(provider.Resumable)
	if !ok {
		return RunResult{}, fmt.Errorf("provider %q does not support resume", workerName)
	}

	if req.SubtaskTimeout <= 0 {
		req.SubtaskTimeout = 10 * time.Minute
	}
	if req.RunTimeout <= 0 {
		req.RunTimeout = 30 * time.Minute
	}

	ctx, cancel := context.WithTimeout(ctx, req.RunTimeout)
	defer cancel()

	newSession := uuid.NewString()
	meta, _ := json.Marshal(map[string]string{"resumed_from": req.PriorSessionID})
	if err := s.deps.Store.CreateSession(store.Session{
		ID:        newSession,
		Goal:      req.Goal,
		Worker:    workerName,
		Status:    "running",
		CreatedAt: time.Now(),
		MetaJSON:  string(meta),
		ModeName:  req.ModeName,
	}); err != nil {
		return RunResult{}, err
	}
	s.emit(newSession, "", trajectory.GoalReceived, map[string]any{
		"goal": req.Goal, "worker": workerName, "resumed_from": req.PriorSessionID,
		"prior_provider_session_id": last.ProviderSessionID,
	})

	subtaskID := uuid.NewString()
	promptRef, _ := s.deps.Blobs.Put([]byte(req.Goal), "txt")
	startedAt := time.Now()
	if err := s.deps.Store.CreateSubtask(store.Subtask{
		ID: subtaskID, SessionID: newSession, Ord: 0,
		Title: "resume turn", PromptRef: promptRef, Worker: workerName, Status: "running",
	}); err != nil {
		return RunResult{}, err
	}
	s.emit(newSession, subtaskID, trajectory.SubtaskStarted, map[string]any{
		"title": "resume turn", "worker": workerName, "resume_id": last.ProviderSessionID,
	})

	subCtx, subCancel := context.WithTimeout(ctx, req.SubtaskTimeout)
	defer subCancel()

	opts := provider.RunOptions{
		Workdir:         req.Workdir,
		Env:             req.Env,
		Timeout:         req.SubtaskTimeout,
		PreApproveTools: req.PreApproveTools,
		MCPConfigPath:   req.MCPConfigPath,
	}
	gate := capability.NewGate(req.AllowedTools, req.DeniedTools)
	// HITL state mirrors what Supervisor.Run installs: the approver,
	// trigger patterns, severity, and token threshold flow from the
	// request (which was populated from the active MissionProfile via
	// ApplyProfileToResume). Nil approver keeps the gate inert without
	// the caller having to know that detail.
	hState := &hitlState{
		approver:       req.HITL,
		triggers:       capability.NewGate(nil, req.HITLTriggers),
		severity:       req.HITLSeverity,
		tokenThreshold: req.HITLTokenThreshold,
		maxTokens:      req.MaxTokens,
	}
	subCtx = withHitlState(subCtx, hState)

	// Resource budget mirrors Supervisor.Run. Resume is a single provider
	// call, so the budget tracker only sees one charge — but the same
	// CheckPreCall / AddOutcome pair fires, the same trajectory events
	// are emitted (budget_warning at 80%, budget_exhausted at 100%), and
	// the resulting RunResult carries status="budget_exhausted" so the
	// CLI exits with the same code as a Run hitting the same cap.
	runBudget := &budget.Budget{
		MaxTokens:        req.MaxTokens,
		MaxUSDCents:      req.MaxUSDCents,
		PerCallMaxTokens: req.PerCallMaxTokens,
	}
	subCtx = withBudget(subCtx, runBudget)
	if preErr := runBudget.CheckPreCall(); preErr != nil {
		// A pre-call trip on resume means the budget arrived already
		// over the limit (only possible if the caller passed values
		// that don't make sense; defensive). Emit the event and abort
		// before we charge the provider.
		s.emitBudgetExhausted(newSession, subtaskID, runBudget, preErr)
		preErrTime := time.Now()
		s.dbErr(newSession, subtaskID, "update_subtask", s.deps.Store.UpdateSubtask(store.Subtask{
			ID: subtaskID, Status: "failed",
			StartedAt: &startedAt, CompletedAt: &preErrTime,
			Error: preErr.Error(), ErrorKind: "budget_exhausted",
		}))
		return s.terminateBudgetExhausted(newSession, preErr)
	}

	events := make(chan provider.Event, 64)
	drainDone := make(chan struct{})
	go func() {
		for ev := range events {
			s.publishProviderEvent(subCtx, newSession, subtaskID, ev, gate, hState)
		}
		close(drainDone)
	}()
	result, callErr := resumable.ResumeHeadless(subCtx, last.ProviderSessionID, req.Goal, opts, events)
	close(events)
	<-drainDone
	completedAt := time.Now()

	// Charge the call to the budget regardless of error. The provider may
	// have racked up tokens even on a partial failure. AddOutcome returns
	// (warning, cap-exceeded). On cap-exceeded, the budget sentinel
	// replaces callErr so the supervisor short-circuits to the
	// budget_exhausted terminator instead of the generic failed path.
	warn, capErr := runBudget.AddOutcome(budget.Usage{
		TokensIn:       result.TokensIn,
		TokensOut:      result.TokensOut,
		ApproxUSDCents: result.ApproxUSDCents,
	})
	if warn != nil {
		s.emitBudgetWarning(newSession, subtaskID, runBudget, warn)
	}
	if capErr != nil {
		s.emitBudgetExhausted(newSession, subtaskID, runBudget, capErr)
		callErr = capErr
	}

	rawRef := ""
	if len(result.RawOutput) > 0 {
		if r, perr := s.deps.Blobs.Put(result.RawOutput, providerBlobExt(workerName)); perr == nil {
			rawRef = r
		}
	}

	if callErr != nil {
		kind := classifyError(callErr, ctx)
		s.emit(newSession, subtaskID, trajectory.SubtaskFailed, map[string]any{"error": callErr.Error(), "kind": kind})
		s.dbErr(newSession, subtaskID, "update_subtask", s.deps.Store.UpdateSubtask(store.Subtask{
			ID: subtaskID, ProviderSessionID: result.SessionID, Status: "failed",
			StartedAt: &startedAt, CompletedAt: &completedAt,
			ResultText: Truncate(result.FinalText, 8000), RawOutputRef: rawRef,
			Error: callErr.Error(), ErrorKind: kind,
		}))
		if errors.Is(callErr, budget.ErrBudgetExceeded) {
			return s.terminateBudgetExhausted(newSession, callErr)
		}
		s.emit(newSession, "", trajectory.RunFailed, map[string]any{"reason": callErr.Error()})
		s.dbErr(newSession, "", "mark_session", s.deps.Store.MarkSession(newSession, "failed", ""))
		return RunResult{SessionID: newSession, Status: "failed"}, callErr
	}

	s.emit(newSession, subtaskID, trajectory.SubtaskCompleted, map[string]any{
		"chars": len(result.FinalText), "provider_session_id": result.SessionID,
		"tokens_in": result.TokensIn, "tokens_out": result.TokensOut, "usd_cents": result.ApproxUSDCents,
	})
	s.dbErr(newSession, subtaskID, "update_subtask", s.deps.Store.UpdateSubtask(store.Subtask{
		ID: subtaskID, ProviderSessionID: result.SessionID, Status: "completed",
		StartedAt: &startedAt, CompletedAt: &completedAt,
		ResultText: Truncate(result.FinalText, 8000), RawOutputRef: rawRef,
		MetaJSON: subtaskUsageMeta(result.TokensIn, result.TokensOut, result.ApproxUSDCents),
	}))
	finalRef, _ := s.deps.Blobs.Put([]byte(result.FinalText), "txt")
	s.dbErr(newSession, "", "mark_session", s.deps.Store.MarkSession(newSession, "completed", finalRef))
	s.emit(newSession, "", trajectory.RunCompleted, map[string]any{"status": "completed", "subtasks": 1})

	return RunResult{
		SessionID:   newSession,
		Status:      "completed",
		FinalAnswer: result.FinalText,
	}, nil
}
