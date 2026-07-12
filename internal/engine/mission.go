package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/budget"
	"github.com/unleashtheagents/uta/internal/provider"
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
	// MaxParallel caps concurrently running par branches (default 4).
	MaxParallel int

	ModeName            string
	TransportMaxRetries int

	// ResumeSessionID replays a prior mission run from its journal: calls
	// whose rendered prompt matches a completed subtask of the prior
	// session are recovered (result reused, budget not re-charged); only
	// the frontier — the first mismatching or unfinished call onward —
	// executes. A crash, a budget stop, and a Ctrl-C are all the same
	// re-enterable state.
	ResumeSessionID string
}

// ErrSchemaMismatch marks an agent response that failed its declared
// record schema even after the bounded retry. errors.Is-able.
var ErrSchemaMismatch = errors.New("schema mismatch")

// ErrJudgeRejected marks a value that failed its k-of-n verification —
// fewer than K verifiers let it stand. errors.Is-able.
var ErrJudgeRejected = errors.New("judge rejected")

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

	// Load the prior journal for --resume-session: completed subtasks keyed
	// by spec id, matched at call time by prompt content.
	journal := map[string]store.Subtask{}
	if req.ResumeSessionID != "" {
		prior, err := s.deps.Store.SubtaskListBySession(req.ResumeSessionID, 0, 0)
		if err != nil {
			return RunResult{}, fmt.Errorf("load prior session journal: %w", err)
		}
		for _, st := range prior {
			if st.Status == "completed" {
				journal[st.SpecID] = st
			}
		}
	}

	sessionID := uuid.NewString()
	steerMeta := map[string]any{"mission": m.Name, "file": req.SourceFile}
	if req.ResumeSessionID != "" {
		steerMeta["resumed_from"] = req.ResumeSessionID
	}
	meta, _ := json.Marshal(map[string]any{"steer": steerMeta})
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

	maxPar := req.MaxParallel
	if maxPar <= 0 {
		maxPar = 4
	}
	w := &missionWalk{
		sup:       s,
		req:       req,
		sessionID: sessionID,
		bindings:  map[string]string{},
		journal:   journal,
		ord:       new(int64),
		recovered: new(int64),
		sem:       make(chan struct{}, maxPar),
		unhealthy: &sync.Map{},
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
		"calls":        atomic.LoadInt64(w.ord),
		"recovered":    atomic.LoadInt64(w.recovered),
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
	// journal holds the prior run's completed subtasks (by spec id) when
	// resuming.
	journal map[string]store.Subtask

	// prefix is the structural path for spec ids: "" at the top level
	// ("c1-greet"), "p1.b2." inside branch 2 of the first par
	// ("p1.b2.c1-greet"). Structural keying keeps --resume-session
	// deterministic regardless of goroutine scheduling.
	prefix string
	seq    int // call sequence within this scope; scopes are single-goroutine
	parSeq int // par sequence within this scope

	ord       *int64        // global subtask ordinal (atomic; claim order)
	recovered *int64        // recovered-call count (atomic)
	sem       chan struct{} // caps concurrently running par branches
	// unhealthy remembers providers that failed on infrastructure (auth,
	// quota, dead transport) this mission; any() resolution benches them.
	unhealthy *sync.Map

	// verdictMode marks a judge-verifier scope: evalCall appends the
	// STANDS/REFUTED contract to the prompt instead of any schema.
	verdictMode bool
	judgeSeq    int // judge sequence within this scope
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
	case *steer.ParForExpr:
		return w.evalParFor(ctx, x)
	case *steer.JudgeExpr:
		return w.evalJudge(ctx, x)
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
	// A record return type is a contract: the schema instruction is the
	// one line of type system the model ever sees, and the response is
	// validated (with one feedback retry) before the value flows onward.
	// In a judge-verifier scope the verdict contract replaces it (the
	// checker forbids structured returns on verifiers).
	recType, isList, structured := w.req.Program.SchemaFor(fn)
	switch {
	case w.verdictMode:
		prompt += "\n\n" + steer.VerdictInstruction
	case structured:
		prompt += "\n\n" + steer.SchemaInstruction(recType, isList)
	}

	w.seq++
	specID := fmt.Sprintf("%sc%d-%s", w.prefix, w.seq, fn.Name)

	// Replay: a completed prior call with the same spec id AND the same
	// rendered prompt is recovered instead of re-run. Prompt equality is
	// the correctness guard — if the program (or an upstream result)
	// changed, the prompt changes, and the frontier starts here.
	if prior, ok := w.journal[specID]; ok {
		if text, ok := w.recoverResult(prior, prompt); ok {
			now := time.Now()
			subtaskID := uuid.NewString()
			recMeta, _ := json.Marshal(map[string]any{"recovered_from": prior.ID})
			w.sup.dbErr(w.sessionID, subtaskID, "create_subtask", w.sup.deps.Store.CreateSubtask(store.Subtask{
				ID: subtaskID, SessionID: w.sessionID, Ord: int(atomic.AddInt64(w.ord, 1)) - 1, SpecID: specID,
				Title: callTitle(fn, call, args) + " (recovered)", PromptRef: prior.PromptRef,
				Worker: prior.Worker, Status: "completed", StartedAt: &now, CompletedAt: &now,
				ResultText: Truncate(text, 8000), RawOutputRef: prior.RawOutputRef,
				MetaJSON: string(recMeta),
			}))
			w.sup.emit(w.sessionID, subtaskID, trajectory.SubtaskCompleted, map[string]any{
				"spec_id": specID, "recovered": true, "recovered_from": prior.ID,
			})
			atomic.AddInt64(w.recovered, 1)
			return text, nil
		}
	}

	candidates, err := missionWorkerCandidates(fn, w.req.DefaultWorker, w.req.Available)
	if err != nil {
		return "", &errCallFailed{fn: fn.Name, pos: call.Pos, file: w.req.Program.File, err: err}
	}
	// Providers that already failed on infrastructure this mission go to
	// the back of the line: still a last resort, never the first choice.
	ordered := make([]string, 0, len(candidates))
	var benched []string
	for _, c := range candidates {
		if w.isUnhealthy(c) {
			benched = append(benched, c)
		} else {
			ordered = append(ordered, c)
		}
	}
	ordered = append(ordered, benched...)

	var lastErr error
	for i, workerName := range ordered {
		text, callErr := w.callOnce(ctx, fn, call, args, specID, prompt, workerName, recType, isList, structured)
		if callErr == nil {
			return text, nil
		}
		lastErr = callErr
		// Failover is for infrastructure failures only (auth, quota,
		// exhausted transport). Semantic failures — the agent worked and
		// failed, schema mismatches, budget, cancellation — would fail the
		// same way anywhere, so they don't burn a second provider.
		if i == len(ordered)-1 || !failoverEligible(callErr) {
			break
		}
		w.markUnhealthy(workerName)
		w.sup.emit(w.sessionID, "", trajectory.ProviderFailover, map[string]any{
			"spec_id": specID, "from": workerName, "to": ordered[i+1], "reason": callErr.Error(),
		})
	}
	return "", &errCallFailed{fn: fn.Name, pos: call.Pos, file: w.req.Program.File, err: lastErr}
}

// failoverEligible reports whether a call failure is an infrastructure
// problem another provider in the worker set could plausibly not have.
func failoverEligible(err error) bool {
	return errors.Is(err, provider.ErrAuth) ||
		errors.Is(err, provider.ErrQuota) ||
		errors.Is(err, provider.ErrTransport)
}

// callOnce executes one provider attempt for a call: its own subtask row,
// schema enforcement, and cost ceiling. Returns the raw failure (no call
// site wrapper) so evalCall's failover loop can classify it.
func (w *missionWalk) callOnce(ctx context.Context, fn *steer.AgentFn, call *steer.CallExpr, args map[string]string, specID, prompt, workerName string, recType *steer.RecordType, isList, structured bool) (string, error) {
	prov, ok := w.sup.deps.Registry.Get(workerName)
	if !ok {
		return "", fmt.Errorf("provider %q not in registry", workerName)
	}

	subtaskID := uuid.NewString()
	promptRef, _ := w.sup.deps.Blobs.Put([]byte(prompt), "txt")
	startedAt := time.Now()
	w.sup.dbErr(w.sessionID, subtaskID, "create_subtask", w.sup.deps.Store.CreateSubtask(store.Subtask{
		ID: subtaskID, SessionID: w.sessionID, Ord: int(atomic.AddInt64(w.ord, 1)) - 1, SpecID: specID,
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

	// Schema enforcement with one bounded retry: the rejection reason is
	// fed back verbatim so the model can repair its own output. The retry
	// is a real provider call — it charges the budget like any other.
	if callErr == nil && structured {
		validated, verr := steer.ValidateRecord(recType, result.FinalText, isList)
		if verr != nil {
			w.sup.emit(w.sessionID, subtaskID, trajectory.SubtaskStdout, map[string]any{
				"schema_retry": verr.Error(), "spec_id": specID,
			})
			retryPrompt := prompt + "\n\nYour previous response was rejected: " + verr.Error() +
				"\nRespond again with ONLY the JSON described above."
			retryCtx, retryCancel := context.WithTimeout(ctx, w.req.CallTimeout)
			retryResult, retryErr := w.sup.callProvider(retryCtx, w.sessionID, subtaskID, prov, retryPrompt, carrier)
			retryCancel()
			retryResult.TokensIn += result.TokensIn
			retryResult.TokensOut += result.TokensOut
			retryResult.ApproxUSDCents += result.ApproxUSDCents
			result = retryResult
			if retryErr != nil {
				callErr = retryErr
			} else if validated, verr = steer.ValidateRecord(recType, result.FinalText, isList); verr != nil {
				callErr = fmt.Errorf("response failed the %s schema after retry (%v): %w",
					recType.Name, verr, ErrSchemaMismatch)
			}
		}
		if callErr == nil {
			result.FinalText = validated
		}
	}
	completedAt := time.Now()

	// The fn's `costs <=` clause is a per-call ceiling, checked post-hoc
	// (providers don't pre-declare usage). Done here rather than through
	// the shared budget's PerCallMaxTokens so concurrent par branches with
	// different ceilings can't race. The cumulative charge stands — the
	// spend happened — but the call is a typed budget failure.
	if callErr == nil && fn.CostTokens > 0 {
		if used := result.TokensIn + result.TokensOut; used > fn.CostTokens {
			callErr = fmt.Errorf("per-call costs ceiling exceeded (used=%d, cap=%d): %w",
				used, fn.CostTokens, budget.ErrBudgetExceeded)
		}
	}

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
		return "", callErr
	}

	// The full result text goes to a blob so a future --resume-session can
	// recover it verbatim (ResultText is display-truncated at 8000 chars).
	finalText := strings.TrimSpace(result.FinalText)
	resultRef := ""
	if finalText != "" {
		if ref, perr := w.sup.deps.Blobs.Put([]byte(finalText), "txt"); perr == nil {
			resultRef = ref
		}
	}
	callMeta, _ := json.Marshal(map[string]any{
		"tokens_in": result.TokensIn, "tokens_out": result.TokensOut,
		"usd_cents": result.ApproxUSDCents, "result_ref": resultRef,
	})
	w.sup.dbErr(w.sessionID, subtaskID, "update_subtask", w.sup.deps.Store.UpdateSubtask(store.Subtask{
		ID: subtaskID, ProviderSessionID: result.SessionID, Status: "completed",
		StartedAt: &startedAt, CompletedAt: &completedAt,
		ResultText: Truncate(result.FinalText, 8000), RawOutputRef: rawRef,
		MetaJSON: string(callMeta),
	}))
	w.sup.emit(w.sessionID, subtaskID, trajectory.SubtaskCompleted, map[string]any{
		"spec_id": specID, "tokens_in": result.TokensIn, "tokens_out": result.TokensOut,
		"usd_cents": result.ApproxUSDCents,
	})
	return finalText, nil
}

// evalParFor runs the body once per item, concurrently (bounded by the
// walk's semaphore). Each branch gets its own single-goroutine walk scope
// with a structural spec-id prefix, so journaling and resume stay
// deterministic no matter how the scheduler interleaves branches. The
// expression's value is the branch results joined in item order.
func (w *missionWalk) evalParFor(ctx context.Context, pf *steer.ParForExpr) (string, error) {
	items := make([]string, len(pf.Items))
	for i, it := range pf.Items {
		v, err := w.eval(ctx, it)
		if err != nil {
			return "", err
		}
		items[i] = v
	}

	w.parSeq++
	parID := w.parSeq
	results := make([]string, len(items))
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	for i := range items {
		bindings := make(map[string]string, len(w.bindings)+1)
		for k, v := range w.bindings {
			bindings[k] = v
		}
		bindings[pf.Var] = items[i]
		branch := &missionWalk{
			sup: w.sup, req: w.req, sessionID: w.sessionID,
			bindings: bindings, journal: w.journal,
			prefix: fmt.Sprintf("%sp%d.b%d.", w.prefix, parID, i+1),
			ord:    w.ord, recovered: w.recovered, sem: w.sem,
			unhealthy: w.unhealthy,
		}
		wg.Add(1)
		go func(i int, bw *missionWalk) {
			defer wg.Done()
			bw.sem <- struct{}{}
			defer func() { <-bw.sem }()
			results[i], errs[i] = bw.eval(ctx, pf.Body)
		}(i, branch)
	}
	wg.Wait()

	// Budget exhaustion outranks other failures — it must reach
	// failMission as itself so the session closes as budget_exhausted.
	for _, err := range errs {
		if err != nil && errors.Is(err, budget.ErrBudgetExceeded) {
			return "", err
		}
	}
	for i, err := range errs {
		if err != nil {
			return "", fmt.Errorf("par branch %d (%s=%q): %w", i+1, pf.Var, truncateArg(items[i]), err)
		}
	}
	return strings.Join(results, "\n\n---\n\n"), nil
}

func truncateArg(s string) string {
	if len(s) > 32 {
		return s[:31] + "…"
	}
	return s
}

// evalJudge implements `judge V by f(...), g(...) require K of N`: the
// verifiers run concurrently, each in verdict mode with the judged value
// appended as its final argument. The value passes through when at least
// K verifiers answer STANDS; unclear responses count as refuted.
func (w *missionWalk) evalJudge(ctx context.Context, j *steer.JudgeExpr) (string, error) {
	val, err := w.eval(ctx, j.Value)
	if err != nil {
		return "", err
	}

	w.judgeSeq++
	judgeID := w.judgeSeq
	verdicts := make([]steer.Verdict, len(j.By))
	errs := make([]error, len(j.By))
	var wg sync.WaitGroup
	for i, by := range j.By {
		// The judged value rides in as the verifier's final argument.
		synthetic := &steer.CallExpr{
			Pos:  by.Pos,
			Name: by.Name,
			Args: append(append([]steer.Expr{}, by.Args...), &steer.StringLit{Pos: by.Pos, Value: val}),
		}
		branch := &missionWalk{
			sup: w.sup, req: w.req, sessionID: w.sessionID,
			bindings: w.bindings, journal: w.journal,
			prefix: fmt.Sprintf("%sj%d.v%d.", w.prefix, judgeID, i+1),
			ord:    w.ord, recovered: w.recovered, sem: w.sem,
			unhealthy:   w.unhealthy,
			verdictMode: true,
		}
		wg.Add(1)
		go func(i int, bw *missionWalk, call *steer.CallExpr) {
			defer wg.Done()
			bw.sem <- struct{}{}
			defer func() { <-bw.sem }()
			resp, err := bw.evalCall(ctx, call)
			if err != nil {
				errs[i] = err
				return
			}
			verdicts[i] = steer.ParseVerdict(resp)
		}(i, branch, synthetic)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil && errors.Is(err, budget.ErrBudgetExceeded) {
			return "", err
		}
	}
	stands, refuted, unclear, failed := 0, 0, 0, 0
	for i := range j.By {
		switch {
		case errs[i] != nil:
			failed++ // a dead verifier does not count toward K (RFC open question Q3, resolved conservatively)
		case verdicts[i] == steer.VerdictStands:
			stands++
		case verdicts[i] == steer.VerdictUnclear:
			unclear++
		default:
			refuted++
		}
	}
	w.sup.emit(w.sessionID, "", trajectory.JudgeCompleted, map[string]any{
		"require": j.K, "of": j.N, "stands": stands, "refuted": refuted,
		"unclear": unclear, "verifier_errors": failed, "passed": stands >= j.K,
	})
	if stands < j.K {
		return "", fmt.Errorf("%d of %d verifier(s) let the value stand, require %d (refuted=%d, unclear=%d, errored=%d): %w",
			stands, j.N, j.K, refuted, unclear, failed, ErrJudgeRejected)
	}
	return val, nil
}

// recoverResult decides whether a prior subtask can stand in for the call
// about to be made: the recorded prompt must equal the rendered prompt,
// and the full result text must be retrievable (result_ref blob first,
// untruncated ResultText as fallback).
func (w *missionWalk) recoverResult(prior store.Subtask, prompt string) (string, bool) {
	recorded, ok := w.blobString(prior.PromptRef)
	if !ok || recorded != prompt {
		return "", false
	}
	var meta struct {
		ResultRef string `json:"result_ref"`
	}
	if prior.MetaJSON != "" {
		_ = json.Unmarshal([]byte(prior.MetaJSON), &meta)
	}
	if text, ok := w.blobString(meta.ResultRef); ok {
		return text, true
	}
	if len(prior.ResultText) < 8000 { // definitely not truncated
		return prior.ResultText, true
	}
	return "", false
}

func (w *missionWalk) blobString(ref string) (string, bool) {
	if ref == "" {
		return "", false
	}
	var buf strings.Builder
	if err := w.sup.deps.Blobs.Get(ref, &buf); err != nil {
		return "", false
	}
	return buf.String(), true
}

// missionWorkerCandidates returns the ordered provider candidates for one
// agent fn call: the fn's declared worker set filtered to available
// providers (declaration order — predictability is a feature), or the
// mission default. The error spells out what was asked for and what is
// actually available.
func missionWorkerCandidates(fn *steer.AgentFn, defaultWorker string, available []string) ([]string, error) {
	avail := map[string]bool{}
	for _, a := range available {
		avail[a] = true
	}
	if len(fn.Workers) == 0 {
		if defaultWorker == "" {
			return nil, fmt.Errorf("agent fn %q has no worker clause and no default worker is set", fn.Name)
		}
		if !avail[defaultWorker] {
			return nil, fmt.Errorf("default worker %q is not available (detected: %s)", defaultWorker, strings.Join(available, ", "))
		}
		return []string{defaultWorker}, nil
	}
	var out []string
	for _, wname := range fn.Workers {
		if avail[wname] {
			out = append(out, wname)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("none of %q's declared workers (%s) are available (detected: %s)",
			fn.Name, strings.Join(fn.Workers, ", "), strings.Join(available, ", "))
	}
	return out, nil
}

func (w *missionWalk) isUnhealthy(worker string) bool {
	_, bad := w.unhealthy.Load(worker)
	return bad
}

func (w *missionWalk) markUnhealthy(worker string) { w.unhealthy.Store(worker, true) }

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
