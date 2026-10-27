package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/budget"
	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/profile"
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

	// Policy is the MissionProfile bound by the program's `policy P`
	// declaration, resolved by the caller (profile discovery reads the
	// filesystem; the engine stays a library). Only its capability
	// grants apply to a mission: AllowedTools / DeniedTools feed the
	// same capability.Gate mode profiles use, so every tool invocation
	// observed on a worker's event stream is inspected and denials are
	// journaled as capability_gate_denied trajectory events. Budget
	// stays with the program text — a policy's token caps are not
	// merged over the mission's own budget declaration. Required when
	// the program declares a policy; nil otherwise.
	Policy *profile.MissionProfile

	// ResumeSessionID replays a prior mission run from its journal: calls
	// whose rendered prompt matches a completed subtask of the prior
	// session are recovered (result reused, budget not re-charged); only
	// the frontier — the first mismatching or unfinished call onward —
	// executes. A crash, a budget stop, and a Ctrl-C are all the same
	// re-enterable state.
	ResumeSessionID string

	// HITL, when non-nil, answers `gate human` statements synchronously
	// (tests wire hitl.StaticApprover; a TTY front-end could wire the
	// production gate). When nil, a gate suspends the mission instead:
	// the walk stops with ErrMissionSuspended and a pending HITL request
	// under HITLStateDir, and a later `uta hitl approve` +
	// --resume-session carries the mission past it.
	HITL hitl.Approver

	// HITLStateDir is the state root under which pending gate requests
	// and their .approved/.denied markers live — the same layout the
	// `uta hitl` commands read (<HITLStateDir>/hitl/<session>/...).
	// Required for the gate suspension flow; ignored when HITL is set.
	HITLStateDir string
}

// ErrSchemaMismatch marks an agent response that failed its declared
// record schema even after the bounded retries. errors.Is-able.
var ErrSchemaMismatch = errors.New("schema mismatch")

// schemaRetries is the number of bounded repair attempts after an agent
// response fails its declared record schema — per the M1 contract, a
// malformed response retries twice, then fails with a typed error.
const schemaRetries = 2

// missionResultDisplayLimit is the character cap for the ResultText column —
// a display copy of the result; the full text lives in a blob (result_ref).
// recoverResult uses the same limit to decide whether a stored ResultText is
// definitely untruncated, so the truncation sites and the recovery guard must
// stay in lockstep.
const missionResultDisplayLimit = 8000

// ErrJudgeRejected marks a value that failed its k-of-n verification —
// fewer than K verifiers let it stand. errors.Is-able.
var ErrJudgeRejected = errors.New("judge rejected")

// ErrMissionSuspended marks a mission stopped at a `gate human` awaiting a
// human signature — a re-enterable state, not a failure. The session closes
// as awaiting_signature; `uta hitl approve` + --resume-session continues it.
// errors.Is-able.
var ErrMissionSuspended = errors.New("mission suspended awaiting human signature")

// ErrGateDenied marks a `gate human` a human refused to sign. errors.Is-able.
var ErrGateDenied = errors.New("gate denied")

// ErrGateTimedOut marks a `gate human "…" within D` resumed past its
// deadline with no answer on record — the mission fails typed instead of
// re-suspending, so `on GateTimedOut -> …` can compensate. errors.Is-able.
var ErrGateTimedOut = errors.New("gate timed out")

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
	// A mission that binds a policy must not run ungated: the profile is
	// resolved by the caller (mission check already validated the name),
	// so a nil Policy here is a wiring bug, not a condition to tolerate.
	if m.Policy != nil && req.Policy == nil {
		return RunResult{}, fmt.Errorf("mission %s declares `policy %s` but no MissionProfile was resolved (load the profile and set MissionRequest.Policy)", m.Name, m.Policy.Name)
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
	if req.Policy != nil {
		steerMeta["policy"] = req.Policy.Name
	}
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
		sup:        s,
		req:        req,
		sessionID:  sessionID,
		bindings:   map[string]string{},
		refinement: map[string]int{},
		journal:    journal,
		ord:        new(int64),
		recovered:  new(int64),
		sem:        make(chan struct{}, maxPar),
		unhealthy:  &sync.Map{},
	}

	var finalAnswer string
	if err := w.execStmts(ctx, m.Stmts, &finalAnswer); err != nil {
		if errors.Is(err, ErrMissionSuspended) {
			return s.suspendMission(sessionID, err)
		}
		return s.failMission(ctx, sessionID, w, err, &finalAnswer)
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

// suspendMission closes the session as awaiting_signature — a re-enterable
// state, not a failure. The pending HITL request is already on disk (written
// by evalGate); callers surface the approve/resume instructions.
func (s *Supervisor) suspendMission(sessionID string, err error) (RunResult, error) {
	s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "awaiting_signature", ""))
	subs, listErr := s.deps.Store.SubtaskListBySession(sessionID, 0, 0)
	s.dbErr(sessionID, "", "subtask_list", listErr)
	return RunResult{SessionID: sessionID, Status: "awaiting_signature", Subtasks: subs}, err
}

// failMission classifies the walk error and closes the session under the
// matching status: budget_exhausted, cancelled, or failed (which includes
// blowing the mission's duration budget). Before the session closes, a
// matching `on E -> …` handler runs its compensation statements against
// the walk's bindings; a `halt` in the handler consumes the error and the
// mission terminates cleanly instead.
func (s *Supervisor) failMission(ctx context.Context, sessionID string, w *missionWalk, err error, finalAnswer *string) (RunResult, error) {
	halted := s.runMissionHandler(ctx, w, err, finalAnswer)
	if halted {
		return s.haltMission(sessionID, err, *finalAnswer)
	}
	if errors.Is(err, budget.ErrBudgetExceeded) {
		res, e := s.terminateBudgetExhausted(sessionID, err)
		res.FinalAnswer = *finalAnswer
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

// errHalt signals a `halt` statement was reached. It never escapes the
// engine: the parser confines halt to on-handler bodies, and
// runMissionHandler consumes it as the clean-termination signal.
var errHalt = errors.New("mission halted")

// handlerTarget maps an `on E -> …` handler's checked error name to its
// errors.Is sentinel.
func handlerTarget(name string) error {
	switch name {
	case "BudgetExceeded":
		return budget.ErrBudgetExceeded
	case "GateDenied":
		return ErrGateDenied
	case "GateTimedOut":
		return ErrGateTimedOut
	case "SchemaMismatch":
		return ErrSchemaMismatch
	case "JudgeRejected":
		return ErrJudgeRejected
	}
	return nil
}

// runMissionHandler runs the first `on E -> …` handler whose typed error
// matches the walk failure, and reports whether it reached a `halt`. The
// compensation statements execute against the walk's bindings — the lets
// the failed walk already bound stay visible — in their own structural
// scope (on1.), so any agent call they make journals as a normal subtask.
// A compensation statement that itself fails stops the handler (the
// failing call journals its own subtask_failed) and the original error
// still closes the mission.
func (s *Supervisor) runMissionHandler(ctx context.Context, w *missionWalk, cause error, finalAnswer *string) bool {
	for i, h := range w.req.Program.Mission.Handlers {
		target := handlerTarget(h.ErrName)
		if target == nil || !errors.Is(cause, target) {
			continue
		}
		s.emit(w.sessionID, "", trajectory.HandlerTriggered, map[string]any{
			"on": h.ErrName, "line": h.Pos.Line, "error": cause.Error(),
		})
		hw := &missionWalk{
			sup: s, req: w.req, sessionID: w.sessionID,
			bindings: w.bindings, refinement: w.refinement, journal: w.journal,
			prefix: fmt.Sprintf("on%d.", i+1),
			ord:    w.ord, recovered: w.recovered, sem: w.sem,
			unhealthy:  w.unhealthy,
			raisedCaps: nil, // a handler runs at mission scope; no raise survives the failed block
		}
		herr := hw.execStmts(ctx, h.Body, finalAnswer)
		return errors.Is(herr, errHalt)
	}
	return false
}

// haltMission closes a mission whose on-handler ended with `halt`: a clean
// typed termination, not a failure. The close status preserves the walk
// error's classification — budget exhaustion still reads budget_exhausted
// on the session — but the error itself is consumed: the program declared
// this outcome handled, so callers get a nil error and the compensation
// emit as the final answer.
func (s *Supervisor) haltMission(sessionID string, cause error, finalAnswer string) (RunResult, error) {
	status := "halted"
	if errors.Is(cause, budget.ErrBudgetExceeded) {
		status = "budget_exhausted"
	}
	finalRef := ""
	if finalAnswer != "" {
		if ref, err := s.deps.Blobs.Put([]byte(finalAnswer), "txt"); err == nil {
			finalRef = ref
		}
	}
	s.emit(sessionID, "", trajectory.MissionHalted, map[string]any{
		"status": status, "reason": cause.Error(),
	})
	s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, status, finalRef))
	subs, listErr := s.deps.Store.SubtaskListBySession(sessionID, 0, 0)
	s.dbErr(sessionID, "", "subtask_list", listErr)
	return RunResult{SessionID: sessionID, Status: status, FinalAnswer: finalAnswer, Subtasks: subs}, nil
}

// missionWalk carries the interpreter state for one mission execution.
type missionWalk struct {
	sup       *Supervisor
	req       MissionRequest
	sessionID string
	bindings  map[string]string
	// refinement carries the runtime @k tags: binding name → K for every
	// let whose value passed `judge … require K of N` (directly or through
	// a retry wrapper). This is the runtime truth the M5 static checker
	// will merely predict. Shared with sub-walks that execute statements
	// (until rounds, on-handlers); par and judge branches never bind lets.
	refinement map[string]int
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

	// curLet names the let binding currently being evaluated, so evalJudge
	// can tag the JudgeCompleted journal event with the binding that
	// receives the judged (@k) value. Propagated through retry attempts and
	// par-for branches — a body judge produces the binding's per-item
	// values — but deliberately not into judge verifier branches.
	curLet string

	// verdictMode marks a judge-verifier scope: evalCall appends the
	// STANDS/REFUTED contract to the prompt instead of any schema.
	verdictMode bool
	judgeSeq    int // judge sequence within this scope
	gateSeq     int // gate sequence within this scope
	untilSeq    int // until sequence within this scope
	retrySeq    int // retry sequence within this scope

	// roundVals, when non-nil, collects the value of every let and emit
	// this walk executes — execUntil reads it to measure a round's yield.
	roundVals *[]string

	// raisedCaps are the `with cap` raises active in this scope, appended
	// to the policy's allow set when the call's capability gate is built.
	// Copy-on-raise: execWith swaps in a fresh slice, so par/judge branches
	// spawned inside the block read a stable set.
	raisedCaps []string
}

// execStmts interprets a statement list — the mission body or a `with cap`
// block body. finalAnswer receives the value of the last successful emit.
func (w *missionWalk) execStmts(ctx context.Context, stmts []steer.Stmt, finalAnswer *string) error {
	for _, st := range stmts {
		var err error
		switch stmt := st.(type) {
		case *steer.LetStmt:
			var val string
			w.curLet = stmt.Name
			val, err = w.eval(ctx, stmt.Expr)
			w.curLet = ""
			if err == nil {
				w.bindings[stmt.Name] = val
				if k := judgedLevel(stmt.Expr); k > 0 {
					w.refinement[stmt.Name] = k
				}
				w.noteRoundVal(val)
			}
		case *steer.EmitStmt:
			var val string
			val, err = w.eval(ctx, stmt.Expr)
			if err == nil {
				*finalAnswer = val
				w.noteRoundVal(val)
				w.sup.emit(w.sessionID, "", trajectory.MissionEmit, map[string]any{
					"mission": w.req.Program.Mission.Name, "chars": len(val),
				})
			}
		case *steer.GateStmt:
			err = w.evalGate(ctx, stmt)
		case *steer.WithStmt:
			err = w.execWith(ctx, stmt, finalAnswer)
		case *steer.UntilStmt:
			err = w.execUntil(ctx, stmt, finalAnswer)
		case *steer.HaltStmt:
			err = errHalt
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// judgedLevel reports the @k refinement a let binding gains from a
// successfully evaluated expression: K when the expression is a
// `judge … require K of N` — possibly wrapped in retries, since the value
// only arrives if some attempt's judge passed, or reached through a
// par-for body, since the join only arrives when every branch's judge
// passed — and 0 otherwise.
func judgedLevel(e steer.Expr) int {
	for {
		switch x := e.(type) {
		case *steer.JudgeExpr:
			return x.K
		case *steer.RetryExpr:
			e = x.Expr
		case *steer.ParForExpr:
			e = x.Body
		default:
			return 0
		}
	}
}

// noteRoundVal reports a produced value to the enclosing until loop's
// round accumulator, when one is active.
func (w *missionWalk) noteRoundVal(v string) {
	if w.roundVals != nil {
		*w.roundVals = append(*w.roundVals, v)
	}
}

// execWith interprets `with cap C from policy P { ... }`: the capability
// joins the walk's raised set for exactly the extent of the block, and the
// raise/drop pair is journaled so the trajectory shows which calls ran
// with elevated authority. The drop is recorded even when the block fails
// — the authority ends either way.
func (w *missionWalk) execWith(ctx context.Context, ws *steer.WithStmt, finalAnswer *string) error {
	prev := w.raisedCaps
	w.raisedCaps = append(append([]string{}, prev...), ws.Cap)
	w.sup.emit(w.sessionID, "", trajectory.CapRaised, map[string]any{
		"cap": ws.Cap, "policy": ws.Policy, "line": ws.Pos.Line,
	})
	err := w.execStmts(ctx, ws.Body, finalAnswer)
	w.raisedCaps = prev
	w.sup.emit(w.sessionID, "", trajectory.CapDropped, map[string]any{
		"cap": ws.Cap, "policy": ws.Policy, "line": ws.Pos.Line,
	})
	return err
}

// execUntil interprets `until dry(n) { ... }` — the discovery loop. The
// body runs in rounds; a round is dry when it yields nothing (every let and
// emit in it produced blank text) or repeats the previous round verbatim.
// The loop ends after n consecutive dry rounds, or when the mission budget
// is exhausted — for a discovery loop that is a terminating condition, not
// a failure: the yield gathered so far stays bound either way, so a
// following emit publishes partial results. Any other body error fails the
// mission as usual.
//
// After the loop, each of the body's top-level let names is rebound to its
// accumulated non-empty round values (joined like par branches) — during
// the loop each round sees its own fresh value, as within-round chaining
// expects.
func (w *missionWalk) execUntil(ctx context.Context, u *steer.UntilStmt, finalAnswer *string) error {
	w.untilSeq++
	untilID := w.untilSeq
	letAccum := map[string][]string{}
	prev, empty, rounds := "", 0, 0
	exit := "dry"
	for empty < u.N {
		rounds++
		var vals []string
		round := &missionWalk{
			sup: w.sup, req: w.req, sessionID: w.sessionID,
			bindings: w.bindings, refinement: w.refinement, journal: w.journal,
			prefix: fmt.Sprintf("%su%d.r%d.", w.prefix, untilID, rounds),
			ord:    w.ord, recovered: w.recovered, sem: w.sem,
			unhealthy:  w.unhealthy,
			raisedCaps: w.raisedCaps,
			roundVals:  &vals,
		}
		if err := round.execStmts(ctx, u.Body, finalAnswer); err != nil {
			if errors.Is(err, budget.ErrBudgetExceeded) {
				exit = "budget"
				break
			}
			return err
		}
		result := strings.TrimSpace(strings.Join(vals, "\n\n---\n\n"))
		if result == "" || result == prev {
			empty++
		} else {
			empty = 0
		}
		prev = result
		for _, st := range u.Body {
			ls, ok := st.(*steer.LetStmt)
			if !ok {
				continue
			}
			v := strings.TrimSpace(w.bindings[ls.Name])
			if v == "" {
				continue
			}
			if acc := letAccum[ls.Name]; len(acc) > 0 && acc[len(acc)-1] == v {
				continue // a repeated round adds nothing to the yield
			}
			letAccum[ls.Name] = append(letAccum[ls.Name], v)
		}
	}
	// Rebind every top-level let to the loop's accumulated yield — on the
	// budget path too, and to "" when the loop never yielded, so following
	// statements read partial (possibly empty) results instead of a stale
	// or missing binding.
	for _, st := range u.Body {
		if ls, ok := st.(*steer.LetStmt); ok {
			v := strings.Join(letAccum[ls.Name], "\n\n---\n\n")
			w.bindings[ls.Name] = v
			if v != "" {
				w.noteRoundVal(v)
			}
		}
	}
	w.sup.emit(w.sessionID, "", trajectory.UntilCompleted, map[string]any{
		"dry": u.N, "rounds": rounds, "exit": exit, "line": u.Pos.Line,
	})
	return nil
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
	case *steer.RetryExpr:
		return w.evalRetry(ctx, x)
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
				ResultText: Truncate(text, missionResultDisplayLimit), RawOutputRef: prior.RawOutputRef,
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
	// The mission's `policy P` profile installs its capability gate here:
	// callProvider builds a capability.Gate from these lists and runs every
	// observed tool_call through it, exactly as mode profiles do for
	// `uta run` — denials become capability_gate_denied journal events.
	// Active `with cap` raises extend the allow set for calls inside the
	// block only. They apply solely when a policy gate exists — appending
	// to an otherwise-empty allow list would *create* a deny-by-default
	// gate where none was bound. DeniedTools still wins on overlap: a
	// raise cannot un-deny what the policy explicitly forbids.
	if pol := w.req.Policy; pol != nil {
		carrier.AllowedTools = pol.AllowedTools
		if len(w.raisedCaps) > 0 {
			carrier.AllowedTools = append(append([]string{}, pol.AllowedTools...), w.raisedCaps...)
		}
		carrier.DeniedTools = pol.DeniedTools
	}
	callRole := provider.RoleWorker
	if w.verdictMode {
		callRole = provider.RoleJudge
	}
	callCtx, cancel := context.WithTimeout(withRole(ctx, callRole), w.req.CallTimeout)
	result, callErr := w.sup.callProvider(callCtx, w.sessionID, subtaskID, prov, prompt, carrier)
	cancel()

	// Schema enforcement with bounded retries: each rejection reason is
	// fed back verbatim so the model can repair its own output, up to
	// schemaRetries times. Every retry is a real provider call — it charges
	// the budget like any other, and the charge accumulates across attempts.
	if callErr == nil && structured {
		validated, verr := steer.ValidateRecord(recType, result.FinalText, isList)
		for attempt := 0; verr != nil && attempt < schemaRetries; attempt++ {
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
				break
			}
			validated, verr = steer.ValidateRecord(recType, result.FinalText, isList)
		}
		if callErr == nil && verr != nil {
			callErr = fmt.Errorf("response failed the %s schema after %d retries (%v): %w",
				recType.Name, schemaRetries, verr, ErrSchemaMismatch)
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
			ResultText: Truncate(result.FinalText, missionResultDisplayLimit), RawOutputRef: rawRef,
			Error: callErr.Error(), ErrorKind: kind,
		}))
		w.sup.emit(w.sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
			"spec_id": specID, "error": callErr.Error(), "kind": kind,
		})
		return "", callErr
	}

	// The full result text goes to a blob so a future --resume-session can
	// recover it verbatim (ResultText is display-truncated at
	// missionResultDisplayLimit chars).
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
		ResultText: Truncate(result.FinalText, missionResultDisplayLimit), RawOutputRef: rawRef,
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
			unhealthy:  w.unhealthy,
			raisedCaps: w.raisedCaps,
			curLet:     w.curLet,
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
		// Each verifier gets its own copy of the bindings, like evalParFor's
		// branches — the walks run concurrently and must not share the map.
		bindings := make(map[string]string, len(w.bindings))
		for k, v := range w.bindings {
			bindings[k] = v
		}
		branch := &missionWalk{
			sup: w.sup, req: w.req, sessionID: w.sessionID,
			bindings: bindings, journal: w.journal,
			prefix: fmt.Sprintf("%sj%d.v%d.", w.prefix, judgeID, i+1),
			ord:    w.ord, recovered: w.recovered, sem: w.sem,
			unhealthy:   w.unhealthy,
			verdictMode: true,
			raisedCaps:  w.raisedCaps,
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
	passed := stands >= j.K
	payload := map[string]any{
		"require": j.K, "of": j.N, "stands": stands, "refuted": refuted,
		"unclear": unclear, "verifier_errors": failed, "passed": passed,
	}
	if passed {
		// The @k tag the value carries onward — journal truth for M5.
		payload["refinement"] = j.K
		// The named lenses that let the value stand — journal truth for the
		// M8 provenance lattice (@ {Security, Repro}), whose static half
		// will predict this set the way M5's checker predicts @k.
		set := make(map[string]bool, len(j.By))
		for i, by := range j.By {
			if errs[i] == nil && verdicts[i] == steer.VerdictStands {
				set[steer.LensName(by)] = true
			}
		}
		lenses := make([]string, 0, len(set))
		for l := range set {
			lenses = append(lenses, l)
		}
		sort.Strings(lenses)
		payload["lenses"] = lenses
		if w.curLet != "" {
			payload["binding"] = w.curLet
		}
	}
	w.sup.emit(w.sessionID, "", trajectory.JudgeCompleted, payload)
	if stands < j.K {
		return "", fmt.Errorf("%d of %d verifier(s) let the value stand, require %d (refuted=%d, unclear=%d, errored=%d): %w",
			stands, j.N, j.K, refuted, unclear, failed, ErrJudgeRejected)
	}
	return val, nil
}

// evalRetry implements `retry n on E` — bounded, typed re-evaluation. The
// wrapped expression runs up to n+1 times; only a failure that errors.Is-
// matches the named sentinel triggers another attempt, anything else (and
// exhaustion) propagates the failure unchanged. Each attempt evaluates in
// its own structural scope (…t1.a2.…), so every attempt journals its own
// subtasks and --resume-session recovers completed attempts instead of
// re-paying them: the walk restarts at the newest attempt with journaled
// work, since every earlier attempt already failed.
func (w *missionWalk) evalRetry(ctx context.Context, r *steer.RetryExpr) (string, error) {
	target := retryTarget(r.ErrName)
	if target == nil {
		return "", fmt.Errorf("retry on unknown error %q at line %d (checker should have caught this)", r.ErrName, r.Pos.Line)
	}
	w.retrySeq++
	retryID := w.retrySeq
	attemptPrefix := func(attempt int) string {
		return fmt.Sprintf("%st%d.a%d.", w.prefix, retryID, attempt)
	}
	start := 1
	for a := r.N + 1; a >= 2; a-- {
		if w.journalHasPrefix(attemptPrefix(a)) {
			start = a
			break
		}
	}
	var lastErr error
	for attempt := start; attempt <= r.N+1; attempt++ {
		att := &missionWalk{
			sup: w.sup, req: w.req, sessionID: w.sessionID,
			bindings: w.bindings, journal: w.journal,
			prefix: attemptPrefix(attempt),
			ord:    w.ord, recovered: w.recovered, sem: w.sem,
			unhealthy:  w.unhealthy,
			raisedCaps: w.raisedCaps,
			curLet:     w.curLet,
		}
		val, err := att.eval(ctx, r.Expr)
		if err == nil {
			return val, nil
		}
		lastErr = err
		if !errors.Is(err, target) || attempt == r.N+1 {
			break
		}
		w.sup.emit(w.sessionID, "", trajectory.RetryAttempt, map[string]any{
			"on": r.ErrName, "attempt": attempt, "of": r.N + 1,
			"line": r.Pos.Line, "error": err.Error(),
		})
	}
	return "", lastErr
}

// journalHasPrefix reports whether the resume journal holds any completed
// subtask under the given structural spec-id prefix.
func (w *missionWalk) journalHasPrefix(prefix string) bool {
	for spec := range w.journal {
		if strings.HasPrefix(spec, prefix) {
			return true
		}
	}
	return false
}

// retryTarget maps a checked `retry n on E` error name to its errors.Is
// sentinel. BudgetExceeded is deliberately absent — the checker rejects it
// (the budget is already spent when it fires).
func retryTarget(name string) error {
	switch name {
	case "SchemaMismatch":
		return ErrSchemaMismatch
	case "JudgeRejected":
		return ErrJudgeRejected
	}
	return nil
}

// evalGate interprets `gate human "…" [within D]`. Five ways through it,
// tried in order:
//
//   - the prior journal (--resume-session) has this gate approved with the
//     same message — recovered like any completed call;
//   - `uta hitl approve <suspended-session>` left a .approved / .denied
//     marker in the suspended session's HITL dir — consumed here;
//   - the gate carries a `within D` deadline, the resumed pending file's
//     recorded deadline has passed, and no marker answered it — the typed
//     ErrGateTimedOut, catchable by `on GateTimedOut -> …`;
//   - a configured Approver (MissionRequest.HITL) answers synchronously;
//   - otherwise the gate writes a pending HITL request (with the absolute
//     deadline when `within D` is present) under the current session and
//     suspends the mission with ErrMissionSuspended.
//
// The gate's action id is its structural spec id, so a resumed run asks for
// the same marker the suspended run announced.
func (w *missionWalk) evalGate(ctx context.Context, g *steer.GateStmt) error {
	msg, err := w.eval(ctx, g.Message)
	if err != nil {
		return err
	}
	w.gateSeq++
	specID := fmt.Sprintf("%sg%d-gate", w.prefix, w.gateSeq)
	actionName := "gate_human"
	if len(g.Options) > 0 {
		actionName = "gate_choose"
	}

	// Replay: an approved gate on the prior journal stays approved. The
	// message plays the role of the prompt, so recoverResult's equality
	// guard applies unchanged — edit the gate text and it re-asks.
	if prior, ok := w.journal[specID]; ok {
		if prevRes, ok := w.recoverResult(prior, msg); ok {
			resLabel := "approved (recovered)"
			if strings.HasPrefix(prevRes, "chosen: ") {
				resLabel = prevRes + " (recovered)"
			}
			w.journalGate(specID, msg, "completed", resLabel)
			w.sup.emit(w.sessionID, "", trajectory.HITLApproved, map[string]any{
				"action": actionName, "action_id": specID, "recovered": true, "recovered_from": prior.ID,
			})
			atomic.AddInt64(w.recovered, 1)
			return nil
		}
	}

	if w.req.ResumeSessionID != "" && w.req.HITLStateDir != "" {
		dir := hitl.SessionDir(w.req.HITLStateDir, w.req.ResumeSessionID)
		if b, statErr := os.ReadFile(hitl.ApprovedPath(dir, specID)); statErr == nil {
			choice := strings.TrimSpace(string(b))
			consumeGateMarkers(dir, specID)
			if len(g.Options) > 0 {
				valid := false
				for _, opt := range g.Options {
					if opt == choice {
						valid = true
						break
					}
				}
				if !valid {
					reason := fmt.Sprintf("invalid choice %q (want one of: %s)", choice, strings.Join(g.Options, ", "))
					if choice == "" {
						reason = fmt.Sprintf("missing choice (want one of: %s)", strings.Join(g.Options, ", "))
					}
					w.journalGate(specID, msg, "failed", reason)
					w.sup.emit(w.sessionID, "", trajectory.HITLDenied, map[string]any{
						"action": actionName, "action_id": specID, "approver": "async", "reason": reason,
					})
					return fmt.Errorf("gate at %s:%d (%s): %s: %w",
						w.req.Program.File, g.Pos.Line, truncateArg(msg), reason, ErrGateDenied)
				}
				w.journalGate(specID, msg, "completed", "chosen: "+choice)
				w.sup.emit(w.sessionID, "", trajectory.HITLApproved, map[string]any{
					"action": actionName, "action_id": specID, "approver": "async", "option": choice,
				})
				return nil
			}
			w.journalGate(specID, msg, "completed", "approved")
			w.sup.emit(w.sessionID, "", trajectory.HITLApproved, map[string]any{
				"action": actionName, "action_id": specID, "approver": "async",
			})
			return nil
		}
		if b, readErr := os.ReadFile(hitl.DeniedPath(dir, specID)); readErr == nil {
			reason := strings.TrimSpace(string(b))
			if reason == "" {
				reason = "denied via file-drop"
			}
			consumeGateMarkers(dir, specID)
			w.journalGate(specID, msg, "failed", reason)
			w.sup.emit(w.sessionID, "", trajectory.HITLDenied, map[string]any{
				"action": actionName, "action_id": specID, "approver": "async", "reason": reason,
			})
			return fmt.Errorf("gate at %s:%d (%s): %s: %w",
				w.req.Program.File, g.Pos.Line, truncateArg(msg), reason, ErrGateDenied)
		}
		// No answer on record: an expired deadline fails the gate typed
		// instead of re-suspending. The deadline is the one journaled when
		// the mission suspended, so re-resuming never extends it.
		if exp, ok := pendingDeadline(dir, specID); g.Within > 0 && ok && time.Now().After(exp) {
			consumeGateMarkers(dir, specID)
			reason := "unanswered past deadline " + exp.Format(time.RFC3339)
			w.journalGate(specID, msg, "failed", reason)
			w.sup.emit(w.sessionID, "", trajectory.HITLDenied, map[string]any{
				"action": actionName, "action_id": specID, "reason": reason,
				"deadline": exp.Format(time.RFC3339),
			})
			return fmt.Errorf("gate at %s:%d (%s): %s: %w",
				w.req.Program.File, g.Pos.Line, truncateArg(msg), reason, ErrGateTimedOut)
		}
	}

	if w.req.HITL != nil {
		reqPayload := map[string]any{
			"action": actionName, "action_id": specID, "prompt": msg, "line": g.Pos.Line,
		}
		if len(g.Options) > 0 {
			reqPayload["options"] = g.Options
		}
		w.sup.emit(w.sessionID, "", trajectory.HITLRequested, reqPayload)
		decision, aerr := w.req.HITL.Request(ctx, hitl.Request{
			SessionID: w.sessionID, Action: actionName, Severity: "high", PromptText: msg,
		})
		if aerr != nil {
			return aerr
		}
		if decision.Approved {
			resLabel := "approved"
			if len(g.Options) > 0 {
				resLabel = "chosen: " + g.Options[0]
			}
			w.journalGate(specID, msg, "completed", resLabel)
			w.sup.emit(w.sessionID, "", trajectory.HITLApproved, map[string]any{
				"action": actionName, "action_id": decision.ActionID, "approver": decision.Approver,
			})
			return nil
		}
		reason := decision.Reason
		if reason == "" {
			reason = "denied"
		}
		w.journalGate(specID, msg, "failed", reason)
		w.sup.emit(w.sessionID, "", trajectory.HITLDenied, map[string]any{
			"action": actionName, "action_id": decision.ActionID, "approver": decision.Approver, "reason": reason,
		})
		return fmt.Errorf("gate at %s:%d (%s): %s: %w",
			w.req.Program.File, g.Pos.Line, truncateArg(msg), reason, ErrGateDenied)
	}

	if w.req.HITLStateDir == "" {
		return fmt.Errorf("gate human at %s:%d: no approver and no HITL state dir configured",
			w.req.Program.File, g.Pos.Line)
	}
	dir := hitl.SessionDir(w.req.HITLStateDir, w.sessionID)
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return fmt.Errorf("gate human: hitl dir: %w", mkErr)
	}
	// The answer deadline starts when the gate first suspends; a resumed
	// but still-unanswered gate carries the original deadline forward
	// instead of restarting the clock.
	var deadline string
	if g.Within > 0 {
		deadline = time.Now().UTC().Add(g.Within).Format(time.RFC3339)
		if w.req.ResumeSessionID != "" {
			priorDir := hitl.SessionDir(w.req.HITLStateDir, w.req.ResumeSessionID)
			if exp, ok := pendingDeadline(priorDir, specID); ok {
				deadline = exp.UTC().Format(time.RFC3339)
			}
		}
	}
	body, mErr := json.MarshalIndent(hitl.Pending{
		ActionID:    specID,
		SessionID:   w.sessionID,
		Action:      actionName,
		Severity:    "high",
		Prompt:      msg,
		RequestedAt: time.Now().UTC().Format(time.RFC3339),
		Deadline:    deadline,
		Options:     g.Options,
	}, "", "  ")
	if mErr != nil {
		return fmt.Errorf("gate human: marshal pending: %w", mErr)
	}
	if wErr := os.WriteFile(hitl.PendingPath(dir, specID), body, 0o644); wErr != nil {
		return fmt.Errorf("gate human: write pending: %w", wErr)
	}
	// The stable pointer lets `uta hitl approve <session>` work without
	// --action; the per-action file above is the authoritative record.
	_ = os.WriteFile(filepath.Join(dir, "pending.json"), body, 0o644)

	w.journalGate(specID, msg, "awaiting_signature", "")
	awaiting := map[string]any{
		"action": actionName, "action_id": specID, "prompt": msg, "line": g.Pos.Line, "suspended": true,
	}
	if deadline != "" {
		awaiting["deadline"] = deadline
	}
	if len(g.Options) > 0 {
		awaiting["options"] = g.Options
	}
	w.sup.emit(w.sessionID, "", trajectory.HITLRequested, awaiting)
	return fmt.Errorf("gate at %s:%d (%s): %w",
		w.req.Program.File, g.Pos.Line, truncateArg(msg), ErrMissionSuspended)
}

// journalGate writes the gate's subtask row. PromptRef carries the gate
// message so the resume replay applies the same prompt-equality guard as
// agent calls; ResultText records the decision.
func (w *missionWalk) journalGate(specID, msg, status, resultText string) {
	subtaskID := uuid.NewString()
	now := time.Now()
	promptRef, _ := w.sup.deps.Blobs.Put([]byte(msg), "txt")
	w.sup.dbErr(w.sessionID, subtaskID, "create_subtask", w.sup.deps.Store.CreateSubtask(store.Subtask{
		ID: subtaskID, SessionID: w.sessionID, Ord: int(atomic.AddInt64(w.ord, 1)) - 1, SpecID: specID,
		Title: "gate human (" + truncateArg(msg) + ")", PromptRef: promptRef,
		Worker: "human", Status: status,
	}))
	if status == "awaiting_signature" {
		return
	}
	w.sup.dbErr(w.sessionID, subtaskID, "update_subtask", w.sup.deps.Store.UpdateSubtask(store.Subtask{
		ID: subtaskID, Status: status, StartedAt: &now, CompletedAt: &now,
		ResultText: Truncate(resultText, missionResultDisplayLimit),
	}))
}

// pendingDeadline reads the answer deadline a suspended gate recorded on
// its pending file. ok is false when the file, its deadline field, or the
// timestamp parse is missing — an undated gate never times out.
func pendingDeadline(dir, actionID string) (time.Time, bool) {
	b, err := os.ReadFile(hitl.PendingPath(dir, actionID))
	if err != nil {
		return time.Time{}, false
	}
	var p hitl.Pending
	if json.Unmarshal(b, &p) != nil || p.Deadline == "" {
		return time.Time{}, false
	}
	exp, perr := time.Parse(time.RFC3339, p.Deadline)
	if perr != nil {
		return time.Time{}, false
	}
	return exp, true
}

// consumeGateMarkers clears one gate's marker and pending files after a
// resume consumes the decision, so re-resuming the same suspended session
// later asks again instead of replaying a stale answer.
func consumeGateMarkers(dir, actionID string) {
	_ = os.Remove(hitl.ApprovedPath(dir, actionID))
	_ = os.Remove(hitl.DeniedPath(dir, actionID))
	_ = os.Remove(hitl.PendingPath(dir, actionID))
	// Only clear the stable pointer when it still names *this* action.
	if b, err := os.ReadFile(filepath.Join(dir, "pending.json")); err == nil {
		var p hitl.Pending
		if json.Unmarshal(b, &p) == nil && p.ActionID == actionID {
			_ = os.Remove(filepath.Join(dir, "pending.json"))
		}
	}
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
	if len(prior.ResultText) < missionResultDisplayLimit { // definitely not truncated
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
