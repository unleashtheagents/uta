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
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// DiscussionAgent is one of the two debate participants: a provider from the
// registry plus an optional persona framing (free text; the CLI resolves
// ~/.uta/personas/ ids to their prompt before building the request).
type DiscussionAgent struct {
	Provider string
	Persona  string
}

// DiscussionRequest drives Supervisor.Discuss: two agents alternate turns on
// a topic for Rounds rounds (a round = one turn per agent), then an optional
// moderator pass synthesizes the outcome. Context threading is stateless —
// each turn's prompt embeds the transcript so far — so it works with any
// provider, including declarative ones without reliable resume support.
type DiscussionRequest struct {
	Topic  string
	Agents [2]DiscussionAgent
	// Rounds is the number of full rounds (default 3, max 10). Total turns
	// is Rounds*2; the last round asks each agent for a closing statement.
	Rounds int

	TurnTimeout time.Duration // per-turn provider timeout (default 5m)
	RunTimeout  time.Duration // overall wall clock (default 30m)

	Workdir string
	Env     []string

	// SynthWorker moderates the final synthesis (defaults to Agents[0]'s
	// provider). SkipSynthesis disables the moderator pass entirely.
	SynthWorker   string
	SkipSynthesis bool

	// Profile-sourced knobs, copied by the caller from an ApplyProfile'd
	// RunRequest so --mode env + budget policies apply to discussions too.
	ModeName            string
	MaxTokens           int64
	MaxUSDCents         int64
	PerCallMaxTokens    int64
	AllowedTools        []string
	DeniedTools         []string
	TransportMaxRetries int
}

// DiscussionTurn is one completed turn of the debate.
type DiscussionTurn struct {
	Round    int    `json:"round"` // 1-based
	AgentIdx int    `json:"agent_idx"`
	Agent    string `json:"agent"` // provider name
	Persona  string `json:"persona,omitempty"`
	Text     string `json:"text"`
}

// DiscussionResult is what Discuss returns, success or not.
type DiscussionResult struct {
	SessionID string           `json:"session_id"`
	Status    string           `json:"status"` // completed | failed | cancelled | budget_exhausted
	Turns     []DiscussionTurn `json:"turns"`
	Synthesis string           `json:"synthesis,omitempty"`
	TokensIn  int64            `json:"tokens_in"`
	TokensOut int64            `json:"tokens_out"`
	USDCents  int64            `json:"usd_cents"`
}

const (
	discussionDefaultRounds = 3
	discussionMaxRounds     = 10
	// discussionResponseWords bounds each turn so transcripts stay readable
	// and the quadratic transcript-replay token growth stays cheap.
	discussionResponseWords = 500
)

func (s *Supervisor) validateDiscussionRequest(req *DiscussionRequest) error {
	if strings.TrimSpace(req.Topic) == "" {
		return errors.New("topic is empty")
	}
	for i, a := range req.Agents {
		if a.Provider == "" {
			return fmt.Errorf("agent %d: provider is required", i+1)
		}
		if _, ok := s.deps.Registry.Get(a.Provider); !ok {
			return fmt.Errorf("agent %d: provider %q not registered", i+1, a.Provider)
		}
	}
	if req.Rounds <= 0 {
		req.Rounds = discussionDefaultRounds
	}
	if req.Rounds > discussionMaxRounds {
		return fmt.Errorf("rounds must be <= %d (got %d) — transcript replay makes very long debates expensive", discussionMaxRounds, req.Rounds)
	}
	if req.TurnTimeout <= 0 {
		req.TurnTimeout = 5 * time.Minute
	}
	if req.RunTimeout <= 0 {
		req.RunTimeout = 30 * time.Minute
	}
	if req.SynthWorker == "" {
		req.SynthWorker = req.Agents[0].Provider
	}
	if !req.SkipSynthesis {
		if _, ok := s.deps.Registry.Get(req.SynthWorker); !ok {
			return fmt.Errorf("synthesizer %q not registered", req.SynthWorker)
		}
	}
	return nil
}

// Discuss runs a structured two-agent debate and persists it as a normal
// session: each turn is a subtask row (ord = turn index), the moderator
// synthesis is the session's final answer, and everything streams onto the
// trajectory bus — so sessions/trajectory/perf/exportdb work unchanged.
func (s *Supervisor) Discuss(ctx context.Context, req DiscussionRequest) (DiscussionResult, error) {
	if err := s.validateDiscussionRequest(&req); err != nil {
		return DiscussionResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, req.RunTimeout)
	defer cancel()

	runBudget := &budget.Budget{
		MaxTokens:        req.MaxTokens,
		MaxUSDCents:      req.MaxUSDCents,
		PerCallMaxTokens: req.PerCallMaxTokens,
	}
	ctx = withBudget(ctx, runBudget)

	// carrier adapts the discussion knobs onto the RunRequest shape that
	// callProvider consumes — this buys retries, budget charging, capability
	// gates, and provider-event publishing without duplicating any of it.
	carrier := RunRequest{
		Workdir:             req.Workdir,
		Env:                 req.Env,
		SubtaskTimeout:      req.TurnTimeout,
		AllowedTools:        req.AllowedTools,
		DeniedTools:         req.DeniedTools,
		TransportMaxRetries: req.TransportMaxRetries,
	}

	sessionID := uuid.NewString()
	meta, _ := json.Marshal(map[string]any{
		"discussion": map[string]any{
			"agents": []map[string]string{
				{"provider": req.Agents[0].Provider, "persona": req.Agents[0].Persona},
				{"provider": req.Agents[1].Provider, "persona": req.Agents[1].Persona},
			},
			"rounds": req.Rounds,
		},
	})
	if err := s.deps.Store.CreateSession(store.Session{
		ID:        sessionID,
		Goal:      "discussion: " + req.Topic,
		Worker:    req.Agents[0].Provider + "+" + req.Agents[1].Provider,
		Status:    "running",
		CreatedAt: time.Now(),
		MetaJSON:  string(meta),
		ModeName:  req.ModeName,
	}); err != nil {
		return DiscussionResult{}, fmt.Errorf("create session: %w", err)
	}

	s.emit(sessionID, "", trajectory.GoalReceived, map[string]any{
		"goal":       "discussion: " + req.Topic,
		"discussion": true,
		"agents":     []string{req.Agents[0].Provider, req.Agents[1].Provider},
		"rounds":     req.Rounds,
		"worker":     req.Agents[0].Provider + "+" + req.Agents[1].Provider,
	})

	res := DiscussionResult{SessionID: sessionID}
	totalTurns := req.Rounds * 2

	for t := 0; t < totalTurns; t++ {
		agentIdx := t % 2
		round := t/2 + 1
		agent := req.Agents[agentIdx]
		prov, _ := s.deps.Registry.Get(agent.Provider)
		label := discussionAgentLabel(agent)

		prompt := renderDiscussionTurnPrompt(req, agentIdx, round, res.Turns)
		promptRef, _ := s.deps.Blobs.Put([]byte(prompt), "txt")
		subtaskID := uuid.NewString()
		specID := fmt.Sprintf("r%d-a%d-%s", round, agentIdx+1, agent.Provider)
		startedAt := time.Now()

		s.dbErr(sessionID, subtaskID, "create_subtask", s.deps.Store.CreateSubtask(store.Subtask{
			ID: subtaskID, SessionID: sessionID, Ord: t, SpecID: specID,
			Title: fmt.Sprintf("round %d — %s", round, label), PromptRef: promptRef,
			Worker: agent.Provider, Status: "running", StartedAt: &startedAt,
		}))
		s.emit(sessionID, subtaskID, trajectory.DiscussionTurnStarted, map[string]any{
			"round": round, "rounds": req.Rounds, "turn": t + 1, "turns": totalTurns,
			"agent": agent.Provider, "agent_idx": agentIdx, "persona": agent.Persona,
		})

		turnCtx, turnCancel := context.WithTimeout(ctx, req.TurnTimeout)
		result, err := s.callProvider(turnCtx, sessionID, subtaskID, prov, prompt, carrier)
		turnCancel()
		completedAt := time.Now()

		res.TokensIn += result.TokensIn
		res.TokensOut += result.TokensOut
		res.USDCents += result.ApproxUSDCents

		rawRef := ""
		if len(result.RawOutput) > 0 {
			if ref, perr := s.deps.Blobs.Put(result.RawOutput, providerBlobExt(agent.Provider)); perr == nil {
				rawRef = ref
			}
		}

		if err != nil {
			kind := classifyError(err, ctx)
			s.dbErr(sessionID, subtaskID, "update_subtask", s.deps.Store.UpdateSubtask(store.Subtask{
				ID: subtaskID, ProviderSessionID: result.SessionID, Status: "failed",
				StartedAt: &startedAt, CompletedAt: &completedAt,
				ResultText: Truncate(result.FinalText, 8000), RawOutputRef: rawRef,
				Error: err.Error(), ErrorKind: kind,
			}))
			s.emit(sessionID, subtaskID, trajectory.SubtaskFailed, map[string]any{
				"spec_id": specID, "error": err.Error(), "kind": kind,
			})
			if errors.Is(err, budget.ErrBudgetExceeded) {
				r, e := s.terminateBudgetExhausted(sessionID, err)
				res.Status = r.Status
				return res, e
			}
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				s.emit(sessionID, "", trajectory.RunCancelled, map[string]any{"reason": err.Error()})
				s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "cancelled", ""))
				res.Status = "cancelled"
				return res, err
			}
			s.emit(sessionID, "", trajectory.RunFailed, map[string]any{
				"reason": fmt.Sprintf("turn %d (%s) failed: %v", t+1, label, err),
			})
			s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "failed", ""))
			res.Status = "failed"
			return res, fmt.Errorf("turn %d (%s) failed (%s): %w", t+1, label, kind, err)
		}

		s.dbErr(sessionID, subtaskID, "update_subtask", s.deps.Store.UpdateSubtask(store.Subtask{
			ID: subtaskID, ProviderSessionID: result.SessionID, Status: "completed",
			StartedAt: &startedAt, CompletedAt: &completedAt,
			ResultText: Truncate(result.FinalText, 8000), RawOutputRef: rawRef,
			MetaJSON: subtaskUsageMeta(result.TokensIn, result.TokensOut, result.ApproxUSDCents),
		}))

		turn := DiscussionTurn{
			Round: round, AgentIdx: agentIdx, Agent: agent.Provider,
			Persona: agent.Persona, Text: strings.TrimSpace(result.FinalText),
		}
		res.Turns = append(res.Turns, turn)
		s.emit(sessionID, subtaskID, trajectory.DiscussionTurnDone, map[string]any{
			"round": round, "turn": t + 1, "turns": totalTurns,
			"agent": agent.Provider, "agent_idx": agentIdx, "persona": agent.Persona,
			"chars": len(turn.Text), "text": turn.Text,
			"tokens_in": result.TokensIn, "tokens_out": result.TokensOut, "usd_cents": result.ApproxUSDCents,
		})
	}

	// ----- Moderator synthesis -----
	finalRef := ""
	if !req.SkipSynthesis {
		synth, _ := s.deps.Registry.Get(req.SynthWorker)
		s.emit(sessionID, "", trajectory.SynthesisStarted, map[string]any{"synth": req.SynthWorker})
		synthCtx, synthCancel := context.WithTimeout(ctx, req.TurnTimeout)
		final, synthErr := s.callProvider(synthCtx, sessionID, "", synth, renderDiscussionSynthPrompt(req, res.Turns), carrier)
		synthCancel()
		res.TokensIn += final.TokensIn
		res.TokensOut += final.TokensOut
		res.USDCents += final.ApproxUSDCents
		switch {
		case synthErr == nil:
			res.Synthesis = strings.TrimSpace(final.FinalText)
			if ref, perr := s.deps.Blobs.Put([]byte(res.Synthesis), "txt"); perr == nil {
				finalRef = ref
			}
			s.emit(sessionID, "", trajectory.SynthesisCompleted, map[string]any{"chars": len(res.Synthesis)})
		case errors.Is(synthErr, budget.ErrBudgetExceeded):
			r, e := s.terminateBudgetExhausted(sessionID, synthErr)
			res.Status = r.Status
			return res, e
		case errors.Is(synthErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
			s.emit(sessionID, "", trajectory.RunCancelled, map[string]any{"reason": synthErr.Error()})
			s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "cancelled", ""))
			res.Status = "cancelled"
			return res, synthErr
		default:
			// A failed moderator shouldn't throw away a completed debate —
			// the transcript is the product; synthesis is a bonus.
			s.emit(sessionID, "", trajectory.SynthesisCompleted, map[string]any{
				"error": synthErr.Error(), "fallback": "transcript-only",
			})
		}
	}

	res.Status = "completed"
	s.dbErr(sessionID, "", "mark_session", s.deps.Store.MarkSession(sessionID, "completed", finalRef))
	s.emit(sessionID, "", trajectory.DiscussionCompleted, map[string]any{
		"rounds": req.Rounds, "turns": len(res.Turns), "status": res.Status,
		"tokens_in": res.TokensIn, "tokens_out": res.TokensOut, "usd_cents": res.USDCents,
	})
	s.emit(sessionID, "", trajectory.RunCompleted, map[string]any{
		"status": res.Status, "subtasks": len(res.Turns),
	})
	return res, nil
}

// discussionAgentLabel renders a stable human label for an agent: provider
// name plus persona when set — "gemini (skeptic)".
func discussionAgentLabel(a DiscussionAgent) string {
	if a.Persona != "" {
		// Personas can be multi-line prompts; label with the first few words.
		p := strings.Fields(a.Persona)
		if len(p) > 4 {
			p = p[:4]
		}
		return fmt.Sprintf("%s (%s)", a.Provider, strings.Join(p, " "))
	}
	return a.Provider
}

// renderDiscussionTurnPrompt builds the stateless per-turn prompt: identity,
// persona framing, the transcript so far, and round-appropriate marching
// orders (open / rebut / close).
func renderDiscussionTurnPrompt(req DiscussionRequest, agentIdx, round int, transcript []DiscussionTurn) string {
	agent := req.Agents[agentIdx]
	other := req.Agents[1-agentIdx]
	var b strings.Builder

	fmt.Fprintf(&b, "You are participating in a structured debate between two AI agents.\n\n")
	fmt.Fprintf(&b, "Topic: %s\n\n", req.Topic)
	fmt.Fprintf(&b, "You are Agent %d, running on %q.", agentIdx+1, agent.Provider)
	if agent.Persona != "" {
		fmt.Fprintf(&b, " Your assigned role/perspective:\n%s\n", agent.Persona)
	} else {
		fmt.Fprintf(&b, " Argue the position you genuinely find most defensible.\n")
	}
	fmt.Fprintf(&b, "Your counterpart is Agent %d, running on %q.\n\n", 2-agentIdx, other.Provider)
	fmt.Fprintf(&b, "This is round %d of %d.\n\n", round, req.Rounds)

	if len(transcript) == 0 {
		b.WriteString("Open the debate: state your position on the topic with your strongest, most concrete arguments.\n")
	} else {
		b.WriteString("Transcript so far:\n\n")
		for _, t := range transcript {
			fmt.Fprintf(&b, "--- Round %d — Agent %d (%s) ---\n%s\n\n", t.Round, t.AgentIdx+1, t.Agent, t.Text)
		}
		if round == req.Rounds {
			b.WriteString("This is your CLOSING statement: summarize where you and your counterpart genuinely agree, where you still disagree and why, and give your final position. Do not introduce brand-new arguments.\n")
		} else {
			b.WriteString("Respond to your counterpart's strongest points: rebut what you disagree with (with specifics), explicitly concede anything they got right, and advance your position with new arguments or evidence. Do not repeat yourself.\n")
		}
	}
	fmt.Fprintf(&b, "\nRules: reply with your debate contribution only — no preamble, no meta commentary about being an AI. Keep it under %d words.\n", discussionResponseWords)
	return b.String()
}

// renderDiscussionSynthPrompt builds the moderator's synthesis prompt.
func renderDiscussionSynthPrompt(req DiscussionRequest, transcript []DiscussionTurn) string {
	var b strings.Builder
	b.WriteString("You are the impartial moderator of a debate between two AI agents. Synthesize the transcript below.\n\n")
	fmt.Fprintf(&b, "Topic: %s\n\nTranscript:\n\n", req.Topic)
	for _, t := range transcript {
		fmt.Fprintf(&b, "--- Round %d — Agent %d (%s) ---\n%s\n\n", t.Round, t.AgentIdx+1, t.Agent, t.Text)
	}
	b.WriteString(`Produce a synthesis in exactly this structure (markdown):

## Points of agreement
## Points of disagreement
For each: what each side claims, and whose argument is stronger and why.
## Open questions
What neither side resolved.
## Moderator's verdict
Your own concise, reasoned judgment on the topic, informed by (but not deferring to) the debate.

Be specific — cite the agents' actual arguments, not generic observations.
`)
	return b.String()
}
