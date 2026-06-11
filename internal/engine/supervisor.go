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
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/budget"
	"github.com/unleashtheagents/uta/internal/engine/capability"
	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

// ErrHITLDenied wraps every error returned when a human denied a HITL
// approval prompt. Callers use errors.Is to detect it.
var ErrHITLDenied = errors.New("hitl denied")

// Deps is the wired infrastructure a Supervisor needs. The CLI (or a TUI)
// constructs this once per process and reuses it across runs.
type Deps struct {
	Store    *store.Store
	Blobs    *store.Blobs
	Recorder *trajectory.Recorder
	Bus      *trajectory.Bus
	Registry *provider.Registry
	// Memory is the institutional-memory layer. Optional — when nil, the
	// supervisor skips fact writes and planner-prompt injection. CLI
	// front-ends wire this from store.DB; tests that don't care about
	// memory leave it nil.
	Memory *memory.Store
	// Whiteboard is the inter-agent shared scratchpad. Optional — when
	// nil, the supervisor skips the planner-prompt "shared notes from
	// other modes" injection. CLI front-ends wire this from store.DB.
	Whiteboard *whiteboard.Store
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

	// PreSetSubtasks, if non-empty, replaces the planner step entirely. The
	// supervisor fans these subtasks out directly. Used by uta.yaml workflows
	// that specify subtasks explicitly.
	PreSetSubtasks []SubtaskSpec

	// PriorOutcomes carries completed-subtask results recovered from a
	// crashed or cancelled earlier run (see BuildResumeRunRequest). They
	// are NOT re-executed — they're prepended to this run's outcomes
	// before synthesis so the final answer covers the whole original
	// plan, not just the re-run remainder.
	PriorOutcomes []SubtaskOutcome

	// ResumedFromSession, when non-empty, is the session id this run is
	// re-entering. Recorded on the new session's meta_json for
	// provenance ("which crashed run does this continue?").
	ResumedFromSession string

	// SkipSynthesis, if true, joins subtask outputs verbatim instead of
	// calling the synthesizer provider. Used when synthesis.mode=skip in YAML.
	SkipSynthesis bool

	// Strategy chooses the orchestration shape. "" or "fanout" runs every
	// subtask in parallel (the original v0.1.0 behavior). "dag" honors
	// SubtaskSpec.Needs and SubtaskSpec.Gate.
	Strategy string

	// Env is appended to os.Environ() for every provider invocation made
	// during this run. Used to inject UTA_PROJECT_ROOT / UTA_CONTEXT_DIR
	// when the run is project-scoped.
	Env []string

	// MaxWallSeconds, if > 0, is a hard wall-clock budget enforced in
	// Supervisor.Run via a timer that cancels the run context when
	// exceeded. Sourced from workflow YAML budget.max_wall_seconds.
	MaxWallSeconds int

	// TransportMaxRetries is the maximum number of additional attempts on
	// transport-level errors (ErrTransport) inside callProvider, on top of
	// the initial attempt. 0 or negative means "use default" (1 retry,
	// preserving the original v0.1.0 behavior).
	TransportMaxRetries int

	// AllowedTools / DeniedTools are the active MissionProfile's capability
	// gate lists. The supervisor's event drainer runs every tool_call event
	// through a Gate built from these and rewrites the event to
	// CapabilityGateDenied when the call matches a deny pattern (or misses
	// a non-empty allow-list). DeniedTools wins over AllowedTools on
	// collision. Empty lists disable enforcement — the call passes through
	// unchanged. Populated by engine.ApplyProfile.
	AllowedTools []string
	DeniedTools  []string

	// OnComplete is the active profile's chained-mode handoff list. The
	// supervisor itself does not chain (that's the caller's job via
	// engine.EvaluateHandoffs / a follow-up Run), but it records the
	// declared targets on session metadata so trajectory consumers can
	// reason about the chain. Populated by engine.ApplyProfile.
	OnComplete []profile.Handoff

	// MaxHandoffDepth caps the chained-mode walk that the caller drives
	// off OnComplete. 0 means "use the orchestrator default"
	// (profile.DefaultMaxHandoffDepth). The supervisor itself does not
	// enforce this — the value rides along so the chaining caller can
	// honor it without re-reading the profile. Populated by
	// engine.ApplyProfile.
	MaxHandoffDepth int

	// HandoffFrom is set on a chained Run to link the new session back to
	// the run that triggered the handoff. The supervisor writes this onto
	// the session's meta_json (as `{"handoff_from": <id>}`) and emits a
	// HandoffCompleted event keyed to the prior session once the chained
	// run terminates.
	HandoffFrom string

	// HandoffTargetMode names the MissionProfile that triggered this
	// chained Run, when HandoffFrom is set. Recorded on meta_json for
	// observability — the supervisor does not use it for any logic.
	HandoffTargetMode string

	// ModeName is the active MissionProfile's name. Persisted on the
	// session row so `uta mode list` and `uta dash` can attribute usage
	// per mode. Empty when the run had no profile attached. Populated by
	// engine.ApplyProfile from the profile's Name field.
	ModeName string

	// MemoryConsolidate, when true, causes the supervisor to write a
	// session_outcome fact to the institutional-memory store after the
	// run completes. The opt-in is sourced from the active profile's
	// `memory.consolidate` field via engine.ApplyProfile. No-op when the
	// supervisor's Memory dep is nil.
	MemoryConsolidate bool

	// MemoryTopK controls how many relevant prior facts are injected into
	// the planner prompt. 0 means "use default" (3). Negative disables
	// injection entirely. Sourced from the profile's `memory.top_k`.
	MemoryTopK int

	// MaxTokens is the cumulative cap on input+output tokens charged to
	// every provider call during this run. 0 disables. Sourced from the
	// active profile's policies.token_budget via engine.ApplyProfile.
	MaxTokens int64

	// MaxUSDCents is the cumulative cap on dollar spend, expressed in U.S.
	// cents (1 = $0.01). 0 disables. Sourced from the active profile's
	// policies.dollar_budget_cents via engine.ApplyProfile.
	MaxUSDCents int64

	// PerCallMaxTokens is the per-provider-call cap on input+output tokens.
	// 0 disables. Sourced from the active profile's policies.per_call_budget
	// via engine.ApplyProfile.
	PerCallMaxTokens int64

	// HITL is the human-in-the-loop approver consulted when a tool_call
	// matches HITLTriggers or when cumulative tokens cross
	// HITLTokenThreshold. Nil disables HITL entirely (the gate is a no-op).
	// Wired by the CLI per invocation; not sourced from the profile.
	HITL hitl.Approver

	// HITLTriggers are tool-pattern strings (same grammar as
	// AllowedTools / DeniedTools — e.g. "Bash(* push *)") that, when
	// matched against a provider's tool_call event, raise a HITL
	// approval prompt. Empty list disables tool-triggered HITL.
	// Sourced from the active profile's policies.hitl_triggers via
	// engine.ApplyProfile.
	HITLTriggers []string

	// HITLTokenThreshold is a percent-of-MaxTokens threshold (1-100). Once
	// cumulative token usage crosses this fraction of the budget, the
	// supervisor fires a one-shot HITL prompt asking the human whether to
	// keep going. 0 disables. Has no effect when MaxTokens is also 0.
	// Sourced from the active profile's policies.hitl_token_threshold.
	HITLTokenThreshold int

	// HITLSeverity is the severity tag attached to HITL request events
	// for this run. Sourced from the active profile's
	// policies.hitl_severity (so an "ops" profile can advertise
	// "medium" while an "audit" profile carries "high"). Cosmetic — does
	// not change whether the gate fires.
	HITLSeverity string

	// MCPConfigPath is an absolute path to a synthesized `.mcp.json`-style
	// file describing the MCP servers attached to the active profile.
	// Engine.ApplyMCPBridge populates it after probing succeeds; the
	// supervisor forwards it onto every provider RunOptions so providers
	// with CapMCP can wire their CLI to the same servers uta probed.
	// Empty when the active profile declared no MCP servers or every
	// probe failed.
	MCPConfigPath string
}

// RunResult is what the supervisor returns once a run is done (success or not).
type RunResult struct {
	SessionID   string
	Status      string // "completed" | "failed" | "partial" | "cancelled"
	FinalAnswer string
	Subtasks    []store.Subtask
}

// sessionMeta is the JSON shape persisted into sessions.meta_json. Kept
// open-ended via the json tags' omitempty so future fields can be added
// without breaking existing readers. The "handoff_from" / "handoff_target"
// fields mirror the resumed_from convention used by ResumeRequest so a
// single meta reader can recognize both chain types.
type sessionMeta struct {
	HandoffFrom   string `json:"handoff_from,omitempty"`
	HandoffTarget string `json:"handoff_target,omitempty"`
	// ResumedFromSession links a resume-run re-entry back to the
	// crashed/cancelled session whose plan it continues.
	ResumedFromSession string `json:"resumed_from_session,omitempty"`
}

// Supervisor runs one orchestration at a time. Reuse the same instance across
// many runs — it carries no per-run state.
type Supervisor struct {
	deps Deps
}

func New(deps Deps) *Supervisor { return &Supervisor{deps: deps} }

// budgetCtxKey is the context key that carries the per-run *budget.Budget
// from Supervisor.Run down into callProvider. We pin the budget to the run
// context (not to RunRequest) so concurrent subtask goroutines all read the
// same shared accumulator; the underlying Budget owns its own mutex.
type budgetCtxKey struct{}

func budgetFromContext(ctx context.Context) *budget.Budget {
	if ctx == nil {
		return nil
	}
	if b, ok := ctx.Value(budgetCtxKey{}).(*budget.Budget); ok {
		return b
	}
	return nil
}

// withBudget returns a derived context that carries the supplied Budget.
// Used internally by Supervisor.Run to fan the budget out to every
// provider-call goroutine.
func withBudget(ctx context.Context, b *budget.Budget) context.Context {
	return context.WithValue(ctx, budgetCtxKey{}, b)
}

// runCancelKey is the context key that carries the per-run cancel-cause
// hook. publishProviderEvent uses this to abort the run when a HITL
// approval is denied; callProvider uses it on the budget-threshold path
// for the same reason. Pinning the cancel to the context lets HITL
// integration stay invisible to the subtask + DAG strategy code paths.
type runCancelKey struct{}

// hitlStateKey carries the per-run *hitlState. We pin it to the context
// (rather than rebuilding one per callProvider invocation) so the
// budget-threshold one-shot atomic is shared across every concurrent
// subtask: the first subtask to cross the threshold consults the human,
// and the rest see the flag already set and skip the prompt.
type hitlStateKey struct{}

func hitlStateFromContext(ctx context.Context) *hitlState {
	if ctx == nil {
		return nil
	}
	if h, ok := ctx.Value(hitlStateKey{}).(*hitlState); ok {
		return h
	}
	return nil
}

func withHitlState(ctx context.Context, h *hitlState) context.Context {
	return context.WithValue(ctx, hitlStateKey{}, h)
}

func runCancelFromContext(ctx context.Context) context.CancelCauseFunc {
	if ctx == nil {
		return nil
	}
	if c, ok := ctx.Value(runCancelKey{}).(context.CancelCauseFunc); ok {
		return c
	}
	return nil
}

func withRunCancel(ctx context.Context, c context.CancelCauseFunc) context.Context {
	return context.WithValue(ctx, runCancelKey{}, c)
}

// hitlState bundles the per-run HITL machinery: the configured approver,
// the parsed trigger gate (built from RunRequest.HITLTriggers using the
// capability-pattern grammar), and the one-shot "we already fired the
// budget threshold for this run" flag. Built once per Run; the same value
// is consulted by every event-drain goroutine and every callProvider
// budget check, so concurrent access goes through the atomic field.
type hitlState struct {
	approver       hitl.Approver
	triggers       *capability.Gate
	severity       string
	tokenThreshold int   // percent of MaxTokens; 0 disables
	maxTokens      int64 // copied so the threshold check is lockless
	budgetFired    atomic.Bool
}

func (h *hitlState) enabled() bool {
	return h != nil && h.approver != nil
}

func (h *hitlState) toolTriggersEnabled() bool {
	return h.enabled() && h.triggers != nil && !h.triggers.Empty()
}

func (h *hitlState) budgetThresholdEnabled() bool {
	return h.enabled() && h.tokenThreshold > 0 && h.maxTokens > 0
}

// validateRunRequest performs all pre-flight checks on a RunRequest before
// any database session is created. Centralizing them here prevents "zombie"
// sessions that get persisted and immediately fail because of a configuration
// error that could have been caught up front — most notably a pre-set
// subtask whose Worker override refers to a provider that isn't in the
// registry (which runFanout/runDAG would otherwise silently paper over by
// falling back to the default worker).
func (s *Supervisor) validateRunRequest(req RunRequest) error {
	if strings.TrimSpace(req.Goal) == "" {
		return errors.New("goal is empty")
	}
	if req.WorkerName == "" {
		return errors.New("worker is required")
	}
	if _, ok := s.deps.Registry.Get(req.WorkerName); !ok {
		return fmt.Errorf("worker %q not registered", req.WorkerName)
	}
	plannerName := firstNonEmpty(req.PlannerName, req.WorkerName)
	if _, ok := s.deps.Registry.Get(plannerName); !ok {
		return fmt.Errorf("planner %q not registered", plannerName)
	}
	synthName := firstNonEmpty(req.SynthName, req.WorkerName)
	if _, ok := s.deps.Registry.Get(synthName); !ok {
		return fmt.Errorf("synthesizer %q not registered", synthName)
	}
	for i, spec := range req.PreSetSubtasks {
		if spec.Worker == "" {
			continue
		}
		if _, ok := s.deps.Registry.Get(spec.Worker); !ok {
			return fmt.Errorf("pre-set subtask %d (%q): worker %q not registered", i, spec.ID, spec.Worker)
		}
	}
	return nil
}

// Run executes the request end-to-end and persists everything to SQLite.
func (s *Supervisor) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := s.validateRunRequest(req); err != nil {
		return RunResult{}, err
	}
	plannerName := firstNonEmpty(req.PlannerName, req.WorkerName)
	planner, _ := s.deps.Registry.Get(plannerName)
	synthName := firstNonEmpty(req.SynthName, req.WorkerName)
	synth, _ := s.deps.Registry.Get(synthName)

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

	// Wrap with a cancel-cause hook so deep call sites (event-drain
	// goroutines, budget-threshold checks) can abort the run with a
	// specific sentinel error (ErrHITLDenied today; future hooks can
	// reuse the same channel).
	hitlCtx, hitlCancel := context.WithCancelCause(ctx)
	defer hitlCancel(nil)
	ctx = withRunCancel(hitlCtx, hitlCancel)

	// Per-run resource budget: any of the three caps non-zero installs a
	// shared accumulator on the run context. callProvider pre-flights each
	// call against the cumulative total, charges the call's reported usage
	// back to the budget, and aborts the run with ErrBudgetExceeded when a
	// cap is reached. The budget is intentionally separate from the
	// wall-clock guard below — both can be active.
	runBudget := &budget.Budget{
		MaxTokens:        req.MaxTokens,
		MaxUSDCents:      req.MaxUSDCents,
		PerCallMaxTokens: req.PerCallMaxTokens,
	}
	ctx = withBudget(ctx, runBudget)

	// Per-run HITL state: trigger gate built from profile patterns +
	// shared one-shot atomic for the budget-threshold prompt. Stored on
	// the context (rather than re-derived per callProvider invocation)
	// so concurrent subtasks observe the same "we already asked the
	// human" flag.
	runHITL := &hitlState{
		approver:       req.HITL,
		triggers:       capability.NewGate(nil, req.HITLTriggers),
		severity:       req.HITLSeverity,
		tokenThreshold: req.HITLTokenThreshold,
		maxTokens:      req.MaxTokens,
	}
	ctx = withHitlState(ctx, runHITL)

	sessionID := uuid.NewString()
	now := time.Now()
	metaJSON := ""
	if req.HandoffFrom != "" || req.ResumedFromSession != "" {
		mb, _ := json.Marshal(sessionMeta{
			HandoffFrom:        req.HandoffFrom,
			HandoffTarget:      req.HandoffTargetMode,
			ResumedFromSession: req.ResumedFromSession,
		})
		metaJSON = string(mb)
	}
	if err := s.deps.Store.CreateSession(store.Session{
		ID:           sessionID,
		Goal:         req.Goal,
		Worker:       req.WorkerName,
		Planner:      plannerName,
		Status:       "running",
		CreatedAt:    now,
		WorkflowPath: req.WorkflowPath,
		MetaJSON:     metaJSON,
		ModeName:     req.ModeName,
	}); err != nil {
		return RunResult{}, fmt.Errorf("create session: %w", err)
	}

	// Hard wall-clock budget: when the workflow specifies budget.max_wall_seconds,
	// a timer cancels the run context once the budget elapses. This is a
	// safety ceiling distinct from RunTimeout — it can be shorter (e.g.
	// 30m RunTimeout, 10m budget) to bound resource use independent of
	// the per-call timeout.
	if req.MaxWallSeconds > 0 {
		budgetDuration := time.Duration(req.MaxWallSeconds) * time.Second
		timer := time.AfterFunc(budgetDuration, func() {
			s.emit(sessionID, "", trajectory.SentinelAlert, map[string]any{
				"rule":           "budget_exhausted",
				"severity":       "critical",
				"message":        fmt.Sprintf("wall-clock budget of %ds exceeded — cancelling run", req.MaxWallSeconds),
				"budget_seconds": req.MaxWallSeconds,
			})
			cancel()
		})
		defer timer.Stop()
	}

	goalPayload := map[string]any{
		"goal": req.Goal, "worker": req.WorkerName, "planner": plannerName, "synth": synthName,
		"max_parallel": req.MaxParallel, "strategy": pickStrategy(req.Strategy, Plan{Subtasks: req.PreSetSubtasks}),
	}
	if req.HandoffFrom != "" {
		goalPayload["handoff_from"] = req.HandoffFrom
		if req.HandoffTargetMode != "" {
			goalPayload["handoff_target_mode"] = req.HandoffTargetMode
		}
	}
	s.emit(sessionID, "", trajectory.GoalReceived, goalPayload)

	// ----- Planning -----
	var plan Plan
	planFell := false
	if len(req.PreSetSubtasks) > 0 {
		plan = Plan{Subtasks: req.PreSetSubtasks}
		s.emit(sessionID, "", trajectory.PlanProposed, map[string]any{
			"subtasks": plan.Subtasks,
			"source":   "workflow-yaml",
		})
	} else {
		var planErr error
		plan, planErr = s.runPlanner(ctx, sessionID, planner, req)
		if planErr != nil {
			// Budget exhaustion is not a "the planner refused, fall back to
			// the goal" situation — the run has hit its hard ceiling, so we
			// surface it as a terminal failure instead of synthesizing more
			// requests we know will be rejected by CheckPreCall.
			if errors.Is(planErr, budget.ErrBudgetExceeded) {
				return s.terminateBudgetExhausted(sessionID, planErr)
			}
			if errors.Is(planErr, ErrHITLDenied) || errors.Is(context.Cause(ctx), ErrHITLDenied) {
				return s.terminateHITLDenied(sessionID, planErr)
			}
			s.emit(sessionID, "", trajectory.PlanFallback, map[string]any{"error": planErr.Error()})
			plan = FallbackPlan(req.Goal)
			planFell = true
		}
		s.emit(sessionID, "", trajectory.PlanProposed, map[string]any{
			"subtasks":      plan.Subtasks,
			"used_fallback": planFell,
		})
	}

	// ----- Strategy dispatch -----
	strategy := pickStrategy(req.Strategy, plan)
	var (
		outcomes  []SubtaskOutcome
		anyFailed bool
		abortedFF bool
		stratErr  error
	)
	switch strategy {
	case "dag":
		outcomes, anyFailed, abortedFF, stratErr = s.runDAG(ctx, sessionID, plan, req)
	default:
		outcomes, anyFailed, abortedFF, stratErr = s.runFanout(ctx, sessionID, plan, req)
	}

	if stratErr != nil {
		if errors.Is(stratErr, budget.ErrBudgetExceeded) {
			return s.terminateBudgetExhausted(sessionID, stratErr)
		}
		if errors.Is(stratErr, ErrHITLDenied) || errors.Is(context.Cause(ctx), ErrHITLDenied) {
			return s.terminateHITLDenied(sessionID, stratErr)
		}
		s.emit(sessionID, "", trajectory.RunFailed, map[string]any{"reason": stratErr.Error()})
		s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "failed", ""))
		subs, listErr := s.deps.Store.SubtaskListBySession(sessionID, 0, 0)
		s.dbErr(sessionID, "", "subtask_list", listErr)
		return RunResult{SessionID: sessionID, Status: "failed", Subtasks: subs}, stratErr
	}

	// Post-dispatch budget check: when subtasks were dispatched and one
	// (or several) tripped the cap, the strategy layer surfaces each one
	// as a failed outcome but does not propagate the sentinel. Examine the
	// budget directly — if any cumulative cap has been reached, terminate
	// the run with budget_exhausted status rather than letting it degrade
	// into a partial/completed result that hides the resource breach.
	if be := runBudget.CheckPreCall(); be != nil {
		return s.terminateBudgetExhausted(sessionID, be)
	}

	// Post-dispatch HITL check: a denied tool-call prompt cancels the
	// run via the context's cancel-cause hook. Detect it before the
	// generic cancellation branch below so the session is marked
	// "hitl_denied" rather than "cancelled".
	if cause := context.Cause(ctx); cause != nil && errors.Is(cause, ErrHITLDenied) {
		return s.terminateHITLDenied(sessionID, cause)
	}

	// Cancellation detection: distinguish user/timeout cancel from clean finish.
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(ctxErr, context.DeadlineExceeded) && !req.FailFast {
		s.emit(sessionID, "", trajectory.RunCancelled, map[string]any{"reason": ctxErr.Error()})
		s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "cancelled", ""))
		return RunResult{SessionID: sessionID, Status: "cancelled"}, ctxErr
	}

	if anyFailed && (req.FailFast || abortedFF) {
		s.emit(sessionID, "", trajectory.RunFailed, map[string]any{"reason": "fail-fast: a subtask failed"})
		s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "failed", ""))
		subs, listErr := s.deps.Store.SubtaskListBySession(sessionID, 0, 0)
		s.dbErr(sessionID, "", "subtask_list", listErr)
		return RunResult{SessionID: sessionID, Status: "failed", Subtasks: subs}, errors.New("a subtask failed (fail-fast)")
	}

	// Prepend recovered prior outcomes (resume-run path) so synthesis —
	// and the joined fallback — see the whole original plan's results,
	// not just the re-run remainder.
	if len(req.PriorOutcomes) > 0 {
		outcomes = append(append([]SubtaskOutcome(nil), req.PriorOutcomes...), outcomes...)
	}

	// ----- Synthesis -----
	finalText := ""
	finalRef := ""
	if req.SkipSynthesis {
		finalText = joinOutcomes(outcomes)
		ref, _ := s.deps.Blobs.Put([]byte(finalText), "txt")
		finalRef = ref
		s.emit(sessionID, "", trajectory.SynthesisCompleted, map[string]any{"mode": "skip", "chars": len(finalText)})
	} else {
		s.emit(sessionID, "", trajectory.SynthesisStarted, map[string]any{"synth": synthName})
		synthPrompt := RenderSynthPrompt(req.Goal, outcomes)
		synthCtx, synthCancel := context.WithTimeout(ctx, req.SubtaskTimeout)
		final, synthErr := s.callProvider(synthCtx, sessionID, "", synth, synthPrompt, req)
		synthCancel()
		if synthErr == nil {
			finalText = final.FinalText
			ref, _ := s.deps.Blobs.Put([]byte(finalText), "txt")
			finalRef = ref
			s.emit(sessionID, "", trajectory.SynthesisCompleted, map[string]any{"chars": len(finalText)})
		} else if errors.Is(synthErr, budget.ErrBudgetExceeded) {
			return s.terminateBudgetExhausted(sessionID, synthErr)
		} else {
			finalText = joinOutcomes(outcomes)
			ref, _ := s.deps.Blobs.Put([]byte(finalText), "txt")
			finalRef = ref
			s.emit(sessionID, "", trajectory.SynthesisCompleted, map[string]any{"error": synthErr.Error(), "fallback": "joined"})
		}
	}

	status := "completed"
	if anyFailed {
		status = "partial"
	}
	s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, status, finalRef))
	s.emit(sessionID, "", trajectory.RunCompleted, map[string]any{"status": status, "subtasks": len(plan.Subtasks)})

	subs, listErr := s.deps.Store.SubtaskListBySession(sessionID, 0, 0)
	s.dbErr(sessionID, "", "subtask_list", listErr)
	result := RunResult{SessionID: sessionID, Status: status, FinalAnswer: finalText, Subtasks: subs}
	if req.MemoryConsolidate {
		s.consolidateRunOutcome(sessionID, req, &result)
	}
	return result, nil
}

// runPlanner calls the planner and parses its output into a Plan.
func (s *Supervisor) runPlanner(ctx context.Context, sessionID string, planner provider.AgentProvider, req RunRequest) (Plan, error) {
	prompt := RenderPlannerPrompt(req.Goal, req.MaxSubtasks)
	if mem := s.relevantMemoryBlock(sessionID, req.Goal, req.MemoryTopK); mem != "" {
		prompt = mem + prompt
	}
	if wb := s.whiteboardBlock(sessionID); wb != "" {
		prompt = wb + prompt
	}
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
	if prov == nil {
		// Defensive: the dispatcher (runFanout/runDAG) couldn't resolve a
		// provider for this subtask. Fail the subtask explicitly rather
		// than panicking on prov.RunHeadless().
		errMsg := fmt.Sprintf("worker %q not registered", workerName)
		s.emit(sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
			"spec_id": spec.ID, "error": errMsg, "kind": "config",
		})
		s.dbErr(sessionID, subtaskID, "update_subtask", s.deps.Store.UpdateSubtask(store.Subtask{
			ID:          subtaskID,
			Status:      "failed",
			StartedAt:   &startedAt,
			CompletedAt: &startedAt,
			Error:       errMsg,
			ErrorKind:   "config",
		}))
		return SubtaskOutcome{
			ID:     spec.ID,
			Title:  spec.Title,
			Worker: workerName,
			Result: "subtask failed (config): " + errMsg,
			Failed: true,
		}
	}
	s.dbErr(sessionID, subtaskID, "update_subtask", s.deps.Store.UpdateSubtask(store.Subtask{
		ID:        subtaskID,
		Status:    "running",
		StartedAt: &startedAt,
	}))
	s.emit(sessionID, subtaskID, trajectory.SubtaskStarted, map[string]any{
		"spec_id": spec.ID, "title": spec.Title, "worker": workerName,
	})

	subCtx, cancel := context.WithTimeout(ctx, subtaskTimeout(spec, req))
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
		kind := classifyError(err, ctx)
		s.emit(sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
			"spec_id": spec.ID, "error": err.Error(), "kind": kind,
		})
		s.dbErr(sessionID, subtaskID, "update_subtask", s.deps.Store.UpdateSubtask(store.Subtask{
			ID:                subtaskID,
			ProviderSessionID: result.SessionID,
			Status:            "failed",
			StartedAt:         &startedAt,
			CompletedAt:       &completedAt,
			ResultText:        Truncate(result.FinalText, 8000),
			RawOutputRef:      rawRef,
			Error:             err.Error(),
			ErrorKind:         kind,
		}))
		return SubtaskOutcome{
			ID:     spec.ID,
			Title:  spec.Title,
			Worker: workerName,
			Result: fmt.Sprintf("subtask failed (%s): %v", kind, err),
			Failed: true,
		}
	}

	s.emit(sessionID, subtaskID, trajectory.SubtaskCompleted, map[string]any{
		"spec_id": spec.ID, "chars": len(result.FinalText), "provider_session_id": result.SessionID,
		"tokens_in": result.TokensIn, "tokens_out": result.TokensOut, "usd_cents": result.ApproxUSDCents,
	})
	s.dbErr(sessionID, subtaskID, "update_subtask", s.deps.Store.UpdateSubtask(store.Subtask{
		ID:                subtaskID,
		ProviderSessionID: result.SessionID,
		Status:            "completed",
		StartedAt:         &startedAt,
		CompletedAt:       &completedAt,
		ResultText:        Truncate(result.FinalText, 8000),
		RawOutputRef:      rawRef,
		MetaJSON:          subtaskUsageMeta(result.TokensIn, result.TokensOut, result.ApproxUSDCents),
	}))
	return SubtaskOutcome{
		ID:     spec.ID,
		Title:  spec.Title,
		Worker: workerName,
		Result: result.FinalText,
	}
}

// subtaskUsageMeta encodes per-call usage into the subtask meta_json so
// post-hoc analysis (`uta perf --cost`, `uta dash` cost panes) can
// aggregate it without re-reading provider raw output. Zero-valued
// counts still get persisted so a downstream count-distinct query can
// tell "we recorded zero" apart from "we never recorded."
func subtaskUsageMeta(tokensIn, tokensOut, usdCents int64) string {
	meta := map[string]any{
		"tokens_in":  tokensIn,
		"tokens_out": tokensOut,
		"usd_cents":  usdCents,
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// callProvider runs a provider with retries on ErrTransport. The retry budget
// comes from req.TransportMaxRetries (default 1, preserving original
// behavior); the value is also propagated onto RunOptions so providers can
// observe it if they choose to. Retries use exponential backoff
// (transportBackoff) so a rate-limited or flapping upstream gets progressively
// more breathing room. While the call is in flight, an internal goroutine
// drains the provider's event channel into uta's trajectory bus, tagging each
// event with the current session_id / subtask_id.
func (s *Supervisor) callProvider(ctx context.Context, sessionID, subtaskID string, prov provider.AgentProvider, prompt string, req RunRequest) (provider.RunResult, error) {
	runBudget := budgetFromContext(ctx)
	if err := runBudget.CheckPreCall(); err != nil {
		s.emitBudgetExhausted(sessionID, subtaskID, runBudget, err)
		return provider.RunResult{}, err
	}
	maxRetries := req.TransportMaxRetries
	if maxRetries <= 0 {
		maxRetries = 1
	}
	opts := provider.RunOptions{
		Workdir:         req.Workdir,
		Env:             req.Env,
		Timeout:         req.SubtaskTimeout,
		PreApproveTools: req.PreApproveTools,
		MaxRetries:      maxRetries,
		MCPConfigPath:   req.MCPConfigPath,
	}
	gate := capability.NewGate(req.AllowedTools, req.DeniedTools)
	hState := hitlStateFromContext(ctx)
	if hState == nil {
		// Defensive fallback for callers that don't go through Supervisor.Run
		// (e.g. Supervisor.Resume) — keep the gate inert.
		hState = &hitlState{triggers: capability.NewGate(nil, nil)}
	}
	for attempt := 0; attempt <= maxRetries; attempt++ {
		events := make(chan provider.Event, 64)
		drainDone := make(chan struct{})
		go func() {
			for ev := range events {
				s.publishProviderEvent(ctx, sessionID, subtaskID, ev, gate, hState)
			}
			close(drainDone)
		}()

		result, err := prov.RunHeadless(ctx, prompt, opts, events)
		close(events)
		<-drainDone

		// Charge usage to the budget regardless of err — the provider may
		// have racked up tokens even on a partial failure. AddOutcome
		// returns the trip signals; we surface them as trajectory events
		// and (on cap-exceeded) replace the call's error with the budget
		// sentinel so the supervisor short-circuits the run.
		if runBudget != nil {
			warn, capErr := runBudget.AddOutcome(budget.Usage{
				TokensIn:       result.TokensIn,
				TokensOut:      result.TokensOut,
				ApproxUSDCents: result.ApproxUSDCents,
			})
			if warn != nil {
				s.emitBudgetWarning(sessionID, subtaskID, runBudget, warn)
			}
			if capErr != nil {
				s.emitBudgetExhausted(sessionID, subtaskID, runBudget, capErr)
				return result, capErr
			}
		}

		// HITL token-threshold check: one-shot per run. When cumulative
		// usage crosses the configured fraction of the budget we pause
		// and ask the human whether to keep spending. Denial cancels
		// the run with ErrHITLDenied; approval proceeds normally.
		if hState.budgetThresholdEnabled() && runBudget != nil && !hState.budgetFired.Load() {
			used := runBudget.TotalTokens()
			trip := hState.maxTokens * int64(hState.tokenThreshold) / 100
			if trip > 0 && used >= trip {
				if hState.budgetFired.CompareAndSwap(false, true) {
					if denyErr := s.askHITL(ctx, sessionID, subtaskID, hState, hitl.Request{
						SessionID:  sessionID,
						SubtaskID:  subtaskID,
						Action:     "budget_threshold",
						Severity:   hState.severity,
						PromptText: fmt.Sprintf("Token budget %d%% consumed (used=%d, cap=%d). Continue?", hState.tokenThreshold, used, hState.maxTokens),
						Detail: map[string]any{
							"used_tokens":    used,
							"max_tokens":     hState.maxTokens,
							"percent_of_cap": hState.tokenThreshold,
						},
					}); denyErr != nil {
						return result, denyErr
					}
				}
			}
		}

		if err == nil {
			return result, nil
		}
		if errors.Is(err, provider.ErrTransport) && attempt < maxRetries {
			delay := transportBackoff(attempt)
			s.emit(sessionID, subtaskID, trajectory.SubtaskStdout, map[string]any{
				"retry_after_transport_error": err.Error(),
				"retry_delay_ms":              delay.Milliseconds(),
				"retry_attempt":               attempt + 1,
			})
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		return result, err
	}
	return provider.RunResult{}, errors.New("unreachable: retry loop exited without result")
}

// terminateHITLDenied finalizes a run that was cancelled by a human
// denying a HITL approval prompt. The session row is marked
// "hitl_denied" (a distinct terminal state from "failed"/"cancelled" so
// dashboards can highlight it), a run_failed trajectory event captures
// the reason, and the error wrapped in ErrHITLDenied is returned verbatim
// so callers can errors.Is(err, ErrHITLDenied).
func (s *Supervisor) terminateHITLDenied(sessionID string, cause error) (RunResult, error) {
	reason := "human denied approval"
	if cause != nil {
		reason = cause.Error()
	}
	s.emit(sessionID, "", trajectory.RunFailed, map[string]any{
		"reason": "hitl denied",
		"error":  reason,
	})
	s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "hitl_denied", ""))
	subs, listErr := s.deps.Store.SubtaskListBySession(sessionID, 0, 0)
	s.dbErr(sessionID, "", "subtask_list", listErr)
	if cause == nil {
		cause = ErrHITLDenied
	}
	return RunResult{SessionID: sessionID, Status: "hitl_denied", Subtasks: subs}, cause
}

// terminateBudgetExhausted finalizes a run that has hit a resource cap.
// The session row is marked with status="budget_exhausted" (a terminal
// state distinct from "failed" so dashboards can call it out separately),
// a run_failed trajectory event is emitted with the budget-specific
// reason, and the strategy error is returned verbatim so callers can
// errors.Is(err, budget.ErrBudgetExceeded). The caller must not have
// already marked the session.
func (s *Supervisor) terminateBudgetExhausted(sessionID string, cause error) (RunResult, error) {
	s.emit(sessionID, "", trajectory.RunFailed, map[string]any{
		"reason": "budget exhausted",
		"error":  cause.Error(),
	})
	s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "budget_exhausted", ""))
	subs, listErr := s.deps.Store.SubtaskListBySession(sessionID, 0, 0)
	s.dbErr(sessionID, "", "subtask_list", listErr)
	return RunResult{SessionID: sessionID, Status: "budget_exhausted", Subtasks: subs}, cause
}

// emitBudgetWarning records a budget_warning trajectory event when the
// cumulative usage crosses 80% of a cap. Fired at most once per dimension
// per run by the underlying Budget.
func (s *Supervisor) emitBudgetWarning(sessionID, subtaskID string, b *budget.Budget, w *budget.WarnSignal) {
	if b == nil || w == nil {
		return
	}
	s.emit(sessionID, subtaskID, trajectory.BudgetWarning, map[string]any{
		"dimension":       w.Kind,
		"used":            w.Used,
		"cap":             w.Cap,
		"total_tokens":    b.TotalTokens(),
		"total_usd_cents": b.TotalUSDCents(),
		"percent_of_cap":  80,
		"message":         fmt.Sprintf("%s budget 80%% consumed (used=%d, cap=%d)", w.Kind, w.Used, w.Cap),
	})
}

// emitBudgetExhausted records a budget_exhausted trajectory event when a
// cap trips. Used both by the pre-call check (cumulative cap already past
// when the call would have started) and the post-call AddOutcome path
// (per-call or cumulative cap reached by this call's usage).
func (s *Supervisor) emitBudgetExhausted(sessionID, subtaskID string, b *budget.Budget, err error) {
	info, _ := budget.InfoFromError(err)
	payload := map[string]any{
		"dimension": info.Kind,
		"used":      info.Used,
		"cap":       info.Cap,
		"message":   err.Error(),
	}
	if b != nil {
		payload["total_tokens"] = b.TotalTokens()
		payload["total_usd_cents"] = b.TotalUSDCents()
	}
	s.emit(sessionID, subtaskID, trajectory.BudgetExhausted, payload)
}

func (s *Supervisor) publishProviderEvent(ctx context.Context, sessionID, subtaskID string, ev provider.Event, gate *capability.Gate, hState *hitlState) {
	// Capability gate: rewrite tool_call events that match a profile
	// deny-pattern (or miss a non-empty allow-list) into
	// capability_gate_denied trajectory events. The underlying provider
	// has typically already invoked the tool by the time we see this
	// event — v1's role is to record + surface the denial so the worker
	// can react (and the sentinel can fire on repeated probes).
	if ev.Kind == provider.EventToolCall && !gate.Empty() {
		toolName, input := capability.ExtractToolNameAndInput(ev.Payload)
		decision, matched := gate.Inspect(toolName, input)
		if decision == capability.Deny {
			reason := "tool call not on profile allow-list"
			if matched != "" {
				reason = "matched profile deny pattern: " + matched
			}
			payload := map[string]any{
				"tool":    toolName,
				"input":   json.RawMessage(input),
				"pattern": matched,
				"reason":  reason,
			}
			s.emit(sessionID, subtaskID, trajectory.CapabilityGateDenied, payload)
			return
		}
	}
	// HITL trigger gate: tool_call events whose name/args match a profile
	// hitl_trigger pattern (e.g. "Bash(* push *)") raise an approval
	// prompt. We treat the trigger gate as a *deny*-style allow-list
	// (capability.Gate with no allow patterns + the triggers as denies),
	// so a match means "ask the human." Approval forwards the event
	// normally; denial cancels the run via the per-run cancel hook.
	if ev.Kind == provider.EventToolCall && hState.toolTriggersEnabled() {
		toolName, input := capability.ExtractToolNameAndInput(ev.Payload)
		decision, matched := hState.triggers.Inspect(toolName, input)
		if decision == capability.Deny {
			if denyErr := s.askHITL(ctx, sessionID, subtaskID, hState, hitl.Request{
				SessionID:  sessionID,
				SubtaskID:  subtaskID,
				Action:     "tool_call",
				Severity:   hState.severity,
				PromptText: fmt.Sprintf("Tool %q matched HITL trigger %q. Approve?", toolName, matched),
				Detail: map[string]any{
					"tool":    toolName,
					"input":   json.RawMessage(input),
					"pattern": matched,
				},
			}); denyErr != nil {
				return // run cancellation handles the rest
			}
		}
	}
	kind := mapProviderEventKind(ev.Kind)
	if kind == "" {
		return
	}
	if ev.Kind == provider.EventToolCall {
		toolName, input := capability.ExtractToolNameAndInput(ev.Payload)
		s.observeWhiteboardToolCall(sessionID, subtaskID, toolName, input)
	}
	s.emit(sessionID, subtaskID, kind, json.RawMessage(ev.Payload))
}

// askHITL emits hitl_requested, invokes the configured Approver, and
// translates the decision into trajectory events (hitl_approved /
// hitl_denied). On denial it pulls the per-run cancel hook out of context
// and trips it with ErrHITLDenied so the supervisor's strategy and
// synthesis layers short-circuit. Returns nil on approval (the caller
// proceeds), or a non-nil error on denial / context cancellation so
// call sites that have a meaningful error-return path (callProvider)
// can surface it.
func (s *Supervisor) askHITL(ctx context.Context, sessionID, subtaskID string, hState *hitlState, r hitl.Request) error {
	if !hState.enabled() {
		return nil
	}
	s.emit(sessionID, subtaskID, trajectory.HITLRequested, map[string]any{
		"action":   r.Action,
		"severity": r.Severity,
		"prompt":   r.PromptText,
		"detail":   r.Detail,
	})
	decision, err := hState.approver.Request(ctx, r)
	if err != nil && !decision.Approved {
		// Treat ctx.Err()-shaped failures as denial without an explicit
		// hitl_denied event — the supervisor will record the cancel
		// reason on its own.
		if cancelFn := runCancelFromContext(ctx); cancelFn != nil {
			cancelFn(fmt.Errorf("hitl approver error: %w", err))
		}
		return err
	}
	if decision.Approved {
		s.emit(sessionID, subtaskID, trajectory.HITLApproved, map[string]any{
			"action_id": decision.ActionID,
			"approver":  decision.Approver,
			"action":    r.Action,
		})
		return nil
	}
	s.emit(sessionID, subtaskID, trajectory.HITLDenied, map[string]any{
		"action_id": decision.ActionID,
		"approver":  decision.Approver,
		"action":    r.Action,
		"reason":    decision.Reason,
	})
	denyErr := fmt.Errorf("%s: %w", decision.Reason, ErrHITLDenied)
	if cancelFn := runCancelFromContext(ctx); cancelFn != nil {
		cancelFn(denyErr)
	}
	return denyErr
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

// dbErr surfaces a non-fatal database error on the trajectory bus. These
// writes are best-effort — a locked or wedged DB shouldn't fail the run —
// but silently swallowing the error hides real corruption from operators.
func (s *Supervisor) dbErr(sessionID, subtaskID, op string, err error) {
	if err == nil {
		return
	}
	s.emit(sessionID, subtaskID, trajectory.SubtaskStdout, map[string]any{
		"db_error": err.Error(),
		"op":       op,
	})
}

// transportBackoff returns the delay to wait before the (attempt+1)-th retry
// of an ErrTransport failure. It doubles from a 1s base — 1s, 2s, 4s, 8s …
// capped at 30s — so a struggling upstream gets progressively more breathing
// room without making the run wait forever.
func transportBackoff(attempt int) time.Duration {
	const (
		base    = 1 * time.Second
		ceiling = 30 * time.Second
	)
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= 30 {
		return ceiling
	}
	d := base << attempt
	if d <= 0 || d > ceiling {
		return ceiling
	}
	return d
}

// classifyError maps a callProvider error to a short telemetry tag stored
// on the subtask row and emitted in trajectory events. runCtx is the
// per-run context — when non-nil and itself past its deadline, a
// context.DeadlineExceeded from the call is attributed to the run-wide
// timeout ("run_timeout") rather than the per-subtask one ("timeout").
// provider.ErrTimeout always maps to "timeout" because the provider
// reports it from its own SubtaskTimeout, never from the run context.
func classifyError(err error, runCtx context.Context) string {
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
		if runCtx != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return "run_timeout"
		}
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// subtaskTimeout returns the effective per-subtask timeout: the spec's
// explicit override when set, otherwise the run-wide default. Lets mixed
// workloads (a fast LLM call alongside a long-running security scan) coexist
// in one DAG without having to widen the global timeout.
func subtaskTimeout(spec SubtaskSpec, req RunRequest) time.Duration {
	if spec.Timeout > 0 {
		return spec.Timeout
	}
	return req.SubtaskTimeout
}

// pickStrategy resolves the orchestration strategy. Explicit value wins; on
// "" we auto-detect: any subtask with Needs or Gate triggers "dag", else
// "fanout".
func pickStrategy(explicit string, plan Plan) string {
	if explicit != "" {
		return explicit
	}
	for _, s := range plan.Subtasks {
		if len(s.Needs) > 0 || s.Gate != nil {
			return "dag"
		}
	}
	return "fanout"
}

// runFanout is the v0.1.0 supervisor pattern: every subtask runs in parallel
// under MaxParallel. No deps, no gates. Returns (outcomes, anyFailed,
// abortedByFailFast, err) like runDAG.
func (s *Supervisor) runFanout(ctx context.Context, sessionID string, plan Plan, req RunRequest) ([]SubtaskOutcome, bool, bool, error) {
	outcomes := make([]SubtaskOutcome, len(plan.Subtasks))
	sem := make(chan struct{}, req.MaxParallel)
	var wg sync.WaitGroup
	anyFailed := false
	var mu sync.Mutex

	subCtx, cancelSubs := context.WithCancelCause(ctx)
	defer cancelSubs(nil)

	for i, spec := range plan.Subtasks {
		i, spec := i, spec
		subtaskWorkerName := req.WorkerName
		var subtaskWorker provider.AgentProvider
		if spec.Worker != "" {
			if p, ok := s.deps.Registry.Get(spec.Worker); ok {
				subtaskWorker = p
				subtaskWorkerName = spec.Worker
			}
		}
		if subtaskWorker == nil {
			subtaskWorker, _ = s.deps.Registry.Get(subtaskWorkerName)
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
			SpecID:    spec.ID,
			Title:     spec.Title,
			PromptRef: promptRef,
			Worker:    subtaskWorkerName,
			Status:    "pending",
		}); err != nil {
			outcomes[i] = SubtaskOutcome{ID: spec.ID, Title: spec.Title, Worker: subtaskWorkerName, Result: "subtask create failed: " + err.Error(), Failed: true}
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-subCtx.Done():
				result := "cancelled before start"
				if cause := context.Cause(subCtx); cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
					result = "cancelled before start: " + cause.Error()
				}
				outcomes[i] = SubtaskOutcome{ID: spec.ID, Title: spec.Title, Worker: subtaskWorkerName, Result: result, Failed: true}
				return
			}
			defer func() { <-sem }()

			outcome := s.runSubtask(subCtx, sessionID, subtaskID, spec, subtaskWorker, subtaskWorkerName, req)
			mu.Lock()
			outcomes[i] = outcome
			if outcome.Failed {
				anyFailed = true
				if req.FailFast {
					cancelSubs(fmt.Errorf("fail-fast: subtask %q failed: %s", spec.ID, outcome.Result))
				}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	abortedByFailFast := anyFailed && req.FailFast && subCtx.Err() != nil
	return outcomes, anyFailed, abortedByFailFast, nil
}
