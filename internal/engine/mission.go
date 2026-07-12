package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/budget"
	"github.com/unleashtheagents/uta/internal/steer"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// MissionRequest drives Supervisor.RunMission — the v0 steer interpreter's
// back half. The front half (parse + static check) lives in internal/steer;
// callers hand a checked Program here and the supervisor walks it, turning
// every agent fn call into a provider subtask and journaling everything to
// the normal session/trajectory store. "steer is to the uta verbs what C is
// to assembly" — this is the part that touches the machine.
type MissionRequest struct {
	Program    *steer.Program
	SourceFile string // for session metadata / display

	// DefaultWorker executes agent fns that declare no worker clause.
	// Available is the CLI's detected-available provider list, used to
	// resolve `worker any(a, b)` sets in declaration order.
	DefaultWorker string
	Available     []string

	Workdir string
	Env     []string

	// CallTimeout bounds each individual agent fn call (default 10m).
	// The mission's own duration budget, if declared, bounds the whole walk.
	CallTimeout time.Duration

	ModeName            string
	TransportMaxRetries int
}

// errCallFailed wraps a provider failure with the call site so the mission
// error reads like a stack frame: `greet("world") at hello.steer:13`.
type errCallFailed struct {
	fn   string
	pos  steer.Pos
	file string
	err  error
}

func (e *errCallFailed) Error() string {
	return fmt.Sprintf("%s() at %s:%d failed: %v", e.fn, e.file, e.pos.Line, e.err)
}
func (e *errCallFailed) Unwrap() error { return e.err }

// RunMission interprets one checked steer program. The mission becomes a
// session, each agent fn call becomes a subtask (with prompt + raw output
// blobs and usage meta), and the last emit becomes the session's final
// answer — so sessions/trajectory/perf/exportdb work unchanged.
func (s *Supervisor) RunMission(ctx context.Context, req MissionRequest) (RunResult, error) {
	if req.Program == nil || req.Program.Mission == nil {
		return RunResult{}, errors.New("mission: no program (parse + check before running)")
	}
	m := req.Program.Mission
	if req.CallTimeout <= 0 {
		req.CallTimeout = 10 * time.Minute
	}

	// The duration budget is the mission's deadline; token/$ ceilings are
	// charged through the shared budget so callProvider enforces them on
	// every call — exhaustion is a typed, catchable event, not an invoice.
	if m.Budget != nil && m.Budget.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.Budget.Deadline)
		defer cancel()
	}
	runBudget := &budget.Budget{}
	if m.Budget != nil {
		runBudget.MaxTokens = m.Budget.Tokens
		runBudget.MaxUSDCents = m.Budget.USDCents
	}
	ctx = withBudget(ctx, runBudget)

	sessionID := uuid.NewString()
	meta, _ := json.Marshal(map[string]any{
		"steer": map[string]any{"mission": m.Name, "file": req.SourceFile},
	})
	if err := s.deps.Store.CreateSession(store.Session{
		ID:        sessionID,
		Goal:      "mission " + m.Name + " (" + req.SourceFile + ")",
		Worker:    req.DefaultWorker,
		Status:    "running",
		CreatedAt: time.Now(),
		MetaJSON:  string(meta),
		ModeName:  req.ModeName,
	}); err != nil {
		return RunResult{}, fmt.Errorf("create session: %w", err)
	}
	s.emit(sessionID, "", trajectory.GoalReceived, map[string]any{
		"goal":    "mission " + m.Name,
		"mission": m.Name,
		"file":    req.SourceFile,
		"worker":  req.DefaultWorker,
	})
	// A mission's plan is static — the program text. Announcing it up front
	// gives live renderers real per-call progress and puts the plan on the
	// trajectory record before any inference happens.
	calls := m.Calls()
	planned := make([]map[string]any, len(calls))
	for i, c := range calls {
		planned[i] = map[string]any{
			"id":    fmt.Sprintf("c%d", i+1),
			"title": c.Name,
			"line":  c.Pos.Line,
		}
	}
	s.emit(sessionID, "", trajectory.PlanProposed, map[string]any{
		"subtasks": planned,
		"source":   "steer",
	})

	w := &missionWalk{
		sup:       s,
		req:       req,
		sessionID: sessionID,
		bindings:  map[string]string{},
	}

	var finalAnswer string
	for _, st := range m.Stmts {
		var err error
		switch stmt := st.(type) {
		case *steer.LetStmt:
			w.bindings[stmt.Name], err = w.eval(ctx, stmt.Expr)
		case *steer.EmitStmt:
			var val string
			val, err = w.eval(ctx, stmt.Expr)
			if err == nil {
				finalAnswer = val
				s.emit(sessionID, "", trajectory.MissionEmit, map[string]any{
					"mission": m.Name, "chars": len(val),
				})
			}
		}
		if err != nil {
			return s.failMission(ctx, sessionID, w, err)
		}
	}

	finalRef := ""
	if finalAnswer != "" {
		if ref, err := s.deps.Blobs.Put([]byte(finalAnswer), "txt"); err == nil {
			finalRef = ref
		}
	}
	s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "completed", finalRef))
	s.emit(sessionID, "", trajectory.RunCompleted, map[string]any{
		"status":       "completed",
		"mission":      m.Name,
		"calls":        w.calls,
		"total_tokens": runBudget.TotalTokens(),
		"usd_cents":    runBudget.TotalUSDCents(),
	})
	subs, listErr := s.deps.Store.SubtaskListBySession(sessionID, 0, 0)
	s.dbErr(sessionID, "", "subtask_list", listErr)
	return RunResult{SessionID: sessionID, Status: "completed", FinalAnswer: finalAnswer, Subtasks: subs}, nil
}

// failMission classifies the walk error and closes the session under the
// matching status: budget_exhausted, cancelled, or failed (which includes
// blowing the mission's duration budget).
func (s *Supervisor) failMission(ctx context.Context, sessionID string, w *missionWalk, err error) (RunResult, error) {
	if errors.Is(err, budget.ErrBudgetExceeded) {
		res, e := s.terminateBudgetExhausted(sessionID, err)
		return res, e
	}
	status := "failed"
	if errors.Is(err, context.Canceled) {
		status = "cancelled"
		s.emit(sessionID, "", trajectory.RunCancelled, map[string]any{"reason": err.Error()})
	} else {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			err = fmt.Errorf("mission duration budget exceeded: %w", err)
		}
		s.emit(sessionID, "", trajectory.RunFailed, map[string]any{"reason": err.Error()})
	}
	s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, status, ""))
	subs, listErr := s.deps.Store.SubtaskListBySession(sessionID, 0, 0)
	s.dbErr(sessionID, "", "subtask_list", listErr)
	return RunResult{SessionID: sessionID, Status: status, Subtasks: subs}, err
}

// missionWalk carries the interpreter state for one mission execution.
type missionWalk struct {
	sup       *Supervisor
	req       MissionRequest
	sessionID string
	bindings  map[string]string
	calls     int // subtask ordinal counter
}

func (w *missionWalk) eval(ctx context.Context, e steer.Expr) (string, error) {
	switch x := e.(type) {
	case *steer.StringLit:
		return x.Value, nil
	case *steer.Ident:
		v, ok := w.bindings[x.Name]
		if !ok {
			return "", fmt.Errorf("unknown name %q at line %d (checker should have caught this)", x.Name, x.Pos.Line)
		}
		return v, nil
	case *steer.CallExpr:
		return w.evalCall(ctx, x)
	default:
		return "", fmt.Errorf("unsupported expression at line %d", e.(interface{ exprPos() steer.Pos }).exprPos().Line)
	}
}

func (w *missionWalk) evalCall(ctx context.Context, call *steer.CallExpr) (string, error) {
	fn := w.req.Program.Agent(call.Name)
	if fn == nil {
		return "", fmt.Errorf("unknown function %q (checker should have caught this)", call.Name)
	}

	args := make(map[string]string, len(fn.Params))
	for i, p := range fn.Params {
		v, err := w.eval(ctx, call.Args[i])
		if err != nil {
			return "", err
		}
		args[p.Name] = v
	}
	prompt, err := fn.RenderPrompt(args)
	if err != nil {
		return "", err
	}

	workerName, err := resolveMissionWorker(fn, w.req.DefaultWorker, w.req.Available)
	if err != nil {
		return "", &errCallFailed{fn: fn.Name, pos: call.Pos, file: w.req.Program.File, err: err}
	}
	prov, ok := w.sup.deps.Registry.Get(workerName)
	if !ok {
		return "", &errCallFailed{fn: fn.Name, pos: call.Pos, file: w.req.Program.File,
			err: fmt.Errorf("provider %q not in registry", workerName)}
	}

	// The fn's `costs <=` clause is a per-call ceiling; the walk is
	// sequential, so re-pointing the shared budget's per-call cap between
	// calls is safe and lets callProvider enforce + journal it uniformly.
	if b := budgetFromContext(ctx); b != nil {
		b.PerCallMaxTokens = fn.CostTokens
	}

	w.calls++
	subtaskID := uuid.NewString()
	specID := fmt.Sprintf("c%d-%s", w.calls, fn.Name)
	promptRef, _ := w.sup.deps.Blobs.Put([]byte(prompt), "txt")
	startedAt := time.Now()
	w.sup.dbErr(w.sessionID, subtaskID, "create_subtask", w.sup.deps.Store.CreateSubtask(store.Subtask{
		ID: subtaskID, SessionID: w.sessionID, Ord: w.calls - 1, SpecID: specID,
		Title: callTitle(fn, call, args), PromptRef: promptRef,
		Worker: workerName, Status: "running", StartedAt: &startedAt,
	}))
	w.sup.emit(w.sessionID, subtaskID, trajectory.SubtaskStarted, map[string]any{
		"spec_id": specID, "title": callTitle(fn, call, args), "worker": workerName,
		"line": call.Pos.Line, "cost_ceiling_tokens": fn.CostTokens,
	})

	carrier := RunRequest{
		Workdir:             w.req.Workdir,
		Env:                 w.req.Env,
		SubtaskTimeout:      w.req.CallTimeout,
		TransportMaxRetries: w.req.TransportMaxRetries,
	}
	callCtx, cancel := context.WithTimeout(ctx, w.req.CallTimeout)
	result, callErr := w.sup.callProvider(callCtx, w.sessionID, subtaskID, prov, prompt, carrier)
	cancel()
	completedAt := time.Now()

	rawRef := ""
	if len(result.RawOutput) > 0 {
		if ref, perr := w.sup.deps.Blobs.Put(result.RawOutput, providerBlobExt(workerName)); perr == nil {
			rawRef = ref
		}
	}

	if callErr != nil {
		kind := classifyError(callErr, ctx)
		w.sup.dbErr(w.sessionID, subtaskID, "update_subtask", w.sup.deps.Store.UpdateSubtask(store.Subtask{
			ID: subtaskID, ProviderSessionID: result.SessionID, Status: "failed",
			StartedAt: &startedAt, CompletedAt: &completedAt,
			ResultText: Truncate(result.FinalText, 8000), RawOutputRef: rawRef,
			Error: callErr.Error(), ErrorKind: kind,
		}))
		w.sup.emit(w.sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
			"spec_id": specID, "error": callErr.Error(), "kind": kind,
		})
		return "", &errCallFailed{fn: fn.Name, pos: call.Pos, file: w.req.Program.File, err: callErr}
	}

	w.sup.dbErr(w.sessionID, subtaskID, "update_subtask", w.sup.deps.Store.UpdateSubtask(store.Subtask{
		ID: subtaskID, ProviderSessionID: result.SessionID, Status: "completed",
		StartedAt: &startedAt, CompletedAt: &completedAt,
		ResultText: Truncate(result.FinalText, 8000), RawOutputRef: rawRef,
		MetaJSON: subtaskUsageMeta(result.TokensIn, result.TokensOut, result.ApproxUSDCents),
	}))
	w.sup.emit(w.sessionID, subtaskID, trajectory.SubtaskCompleted, map[string]any{
		"spec_id": specID, "tokens_in": result.TokensIn, "tokens_out": result.TokensOut,
		"usd_cents": result.ApproxUSDCents,
	})
	return strings.TrimSpace(result.FinalText), nil
}

// resolveMissionWorker picks the provider for one agent fn call: the fn's
// worker set in declaration order, else the mission default. The error
// spells out what was asked for and what is actually available.
func resolveMissionWorker(fn *steer.AgentFn, defaultWorker string, available []string) (string, error) {
	avail := map[string]bool{}
	for _, a := range available {
		avail[a] = true
	}
	if len(fn.Workers) == 0 {
		if defaultWorker == "" {
			return "", fmt.Errorf("agent fn %q has no worker clause and no default worker is set", fn.Name)
		}
		if !avail[defaultWorker] {
			return "", fmt.Errorf("default worker %q is not available (detected: %s)", defaultWorker, strings.Join(available, ", "))
		}
		return defaultWorker, nil
	}
	for _, wname := range fn.Workers {
		if avail[wname] {
			return wname, nil
		}
	}
	return "", fmt.Errorf("none of %q's declared workers (%s) are available (detected: %s)",
		fn.Name, strings.Join(fn.Workers, ", "), strings.Join(available, ", "))
}

func callTitle(fn *steer.AgentFn, call *steer.CallExpr, args map[string]string) string {
	if len(fn.Params) == 0 {
		return fn.Name + "()"
	}
	first := args[fn.Params[0].Name]
	if len(first) > 24 {
		first = first[:23] + "…"
	}
	suffix := ""
	if len(fn.Params) > 1 {
		suffix = ", …"
	}
	return fmt.Sprintf("%s(%q%s)", fn.Name, first, suffix)
}
