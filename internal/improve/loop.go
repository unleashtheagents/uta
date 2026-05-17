package improve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/provider"
)

// LoopRequest configures a self-improvement loop. Designed to be
// language-agnostic: the verify command, worker, and timeouts are operator-
// supplied. Common verify commands for different ecosystems:
//
//	Go        go test ./...
//	Node      npm test    (or)  pnpm test
//	Python    pytest -q
//	Rust      cargo test
//	Solidity  forge test
//	Generic   make verify (or whatever project ships)
//
// The loop itself is the same regardless.
type LoopRequest struct {
	Worker          string        // provider that implements each idea (default: claude)
	GatherWorker    string        // provider that gathers fresh ideas when the board is empty (default: gemini)
	Workdir         string        // workdir for every subtask (defaults to current working dir)
	Env             []string      // extra env for every subtask
	VerifyCommand   string        // gate command run after each iteration. REQUIRED — there is no default;
	                              // the loop is language-agnostic, so the operator must say what "passing" means.
	PerIdeaTimeout  time.Duration // per-iteration cap; default 20m
	GatherTimeout   time.Duration // cap for one gather call; default 10m
	Budget          time.Duration // total wall-clock budget; 0 = no cap
	MaxIterations   int           // hard ceiling on iterations regardless of budget; 0 = no cap
	GatherWhenEmpty bool          // call gather automatically when the board empties
	GatherGoal      string        // optional steering text passed to gather
	GatherMaxIdeas  int           // max ideas per gather call; default 10
	DryRun          bool          // pick + execute but skip the gate (smoke-test mode)
	MaxRetries      int           // gate retries per iteration; default 1
	PreApproveTools []string      // forwarded to providers
}

// LoopResult summarizes one improve invocation.
type LoopResult struct {
	Iterations      int
	IdeasCompleted  int
	IdeasFailed     int
	IdeasGathered   int
	GatherCalls     int
	StoppedBecause  string // "no_ideas" | "budget_exhausted" | "max_iterations" | "cancelled" | "error"
	Elapsed         time.Duration
	LastError       error
}

// Loop runs the picker → executor → recorder cycle until a stop condition is
// hit. It uses the supplied Supervisor for each idea's execution and Board
// for state. Cancelled cleanly via ctx.
func Loop(ctx context.Context, sup *engine.Supervisor, board *Board, reg *provider.Registry, req LoopRequest) (*LoopResult, error) {
	if sup == nil {
		return nil, errors.New("loop: supervisor is nil")
	}
	if board == nil {
		return nil, errors.New("loop: board is nil")
	}
	if reg == nil {
		return nil, errors.New("loop: registry is nil")
	}
	if req.Worker == "" {
		req.Worker = "claude"
	}
	if req.GatherWorker == "" {
		req.GatherWorker = "gemini"
	}
	if !req.DryRun && strings.TrimSpace(req.VerifyCommand) == "" {
		return nil, errors.New("loop: VerifyCommand is required (e.g. 'go test ./...', 'npm test', 'pytest -q', 'forge test') — pass --verify or set it in a workflow file")
	}
	if req.PerIdeaTimeout <= 0 {
		req.PerIdeaTimeout = 20 * time.Minute
	}
	if req.GatherTimeout <= 0 {
		req.GatherTimeout = 10 * time.Minute
	}
	if req.GatherMaxIdeas <= 0 {
		req.GatherMaxIdeas = 10
	}
	if req.MaxRetries < 0 {
		req.MaxRetries = 0
	}

	gatherer := &Gatherer{Registry: reg}

	res := &LoopResult{}
	start := time.Now()
	defer func() { res.Elapsed = time.Since(start) }()

	for {
		if err := ctx.Err(); err != nil {
			res.StoppedBecause = "cancelled"
			return res, nil
		}
		if req.Budget > 0 && time.Since(start) >= req.Budget {
			res.StoppedBecause = "budget_exhausted"
			return res, nil
		}
		if req.MaxIterations > 0 && res.Iterations >= req.MaxIterations {
			res.StoppedBecause = "max_iterations"
			return res, nil
		}

		idea, err := board.NextActionable()
		if errors.Is(err, sql.ErrNoRows) {
			// Try to gather more, if allowed and budget remains.
			if !req.GatherWhenEmpty {
				res.StoppedBecause = "no_ideas"
				return res, nil
			}
			gathered, gerr := runGatherStep(ctx, gatherer, board, reg, req)
			if gerr != nil {
				res.LastError = gerr
				res.StoppedBecause = "error"
				return res, gerr
			}
			res.GatherCalls++
			res.IdeasGathered += gathered
			if gathered == 0 {
				res.StoppedBecause = "no_ideas"
				return res, nil
			}
			continue
		}
		if err != nil {
			res.LastError = err
			res.StoppedBecause = "error"
			return res, err
		}

		// Execute this idea.
		res.Iterations++
		outcome, runErr := executeIdea(ctx, sup, board, req, idea)
		if runErr != nil {
			res.LastError = runErr
		}
		if outcome == StatusDone {
			res.IdeasCompleted++
		} else if outcome == StatusFailed {
			res.IdeasFailed++
		}
	}
}

func runGatherStep(ctx context.Context, g *Gatherer, board *Board, reg *provider.Registry, req LoopRequest) (int, error) {
	if _, ok := reg.Get(req.GatherWorker); !ok {
		return 0, fmt.Errorf("gatherer worker %q not registered", req.GatherWorker)
	}
	existing, _ := board.List(ListOptions{Limit: 200, Newest: true})

	gres, err := g.Run(ctx, GatherRequest{
		WorkerName: req.GatherWorker,
		Workdir:    req.Workdir,
		Env:        req.Env,
		Timeout:    req.GatherTimeout,
		MaxIdeas:   req.GatherMaxIdeas,
		Goal:       req.GatherGoal,
		Existing:   existing,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, idea := range gres.Ideas {
		if err := board.Insert(idea); err != nil {
			return n, fmt.Errorf("persist gathered idea: %w", err)
		}
		n++
	}
	return n, nil
}

// executeIdea drives one idea through the DAG: a single subtask whose prompt
// embodies the design + implementation, gated by GoTestCommand (or whatever
// the operator chose). retry_producer is enabled so a failing gate hands its
// output back to the worker for another pass.
//
// Returns the terminal status of the idea (Done | Failed) and any error.
func executeIdea(ctx context.Context, sup *engine.Supervisor, board *Board, req LoopRequest, idea *Idea) (Status, error) {
	now := time.Now()
	_ = board.SetStatus(idea.ID, StatusInProgress, SetStatusOpts{IncrementAttempts: true})

	// Build the one-shot DAG.
	prompt := renderImplementPrompt(idea, req.Workdir, req.VerifyCommand)
	var gate *engine.Gate
	if !req.DryRun {
		gate = &engine.Gate{
			Cmd:           req.VerifyCommand,
			Timeout:       req.PerIdeaTimeout,
			RetryProducer: true,
			MaxRetries:    req.MaxRetries,
		}
	}

	subtask := engine.SubtaskSpec{
		ID:     "impl-" + idea.ID,
		Title:  fmt.Sprintf("idea %s: %s", idea.ID[:min(8, len(idea.ID))], truncate(idea.Title, 60)),
		Prompt: prompt,
		Worker: req.Worker,
		Gate:   gate,
	}

	result, runErr := sup.Run(ctx, engine.RunRequest{
		Goal:            "Implement idea: " + idea.Title,
		WorkerName:      req.Worker,
		PlannerName:     req.Worker,
		SynthName:       req.Worker,
		MaxParallel:     1,
		MaxSubtasks:     1,
		SubtaskTimeout:  req.PerIdeaTimeout,
		RunTimeout:      req.PerIdeaTimeout + 5*time.Minute,
		PreApproveTools: req.PreApproveTools,
		Workdir:         req.Workdir,
		Env:             req.Env,
		PreSetSubtasks:  []engine.SubtaskSpec{subtask},
		SkipSynthesis:   true,
		Strategy:        "dag",
	})

	terminal := StatusFailed
	errMsg := ""
	summary := ""
	if runErr != nil {
		errMsg = runErr.Error()
	} else if result.Status == "completed" {
		// Inspect the lone subtask for its status.
		if len(result.Subtasks) == 1 {
			st := result.Subtasks[0]
			if st.Status == "completed" {
				terminal = StatusDone
				summary = fmt.Sprintf("completed in %s; prompt %d chars; result %d chars",
					stringifyDuration(st.CompletedAt, st.StartedAt),
					len(prompt), len(st.ResultText))
			} else {
				errMsg = st.Error
				if errMsg == "" {
					errMsg = "subtask status: " + st.Status
				}
			}
		}
	} else if result.Status == "partial" || result.Status == "failed" {
		errMsg = "supervisor reported status=" + result.Status
	}
	_ = now

	opts := SetStatusOpts{LastSession: result.SessionID}
	if errMsg != "" {
		opts.LastError = strPtr(truncate(errMsg, 4000))
	}
	if summary != "" {
		opts.Summary = strPtr(summary)
	}
	if err := board.SetStatus(idea.ID, terminal, opts); err != nil {
		return terminal, fmt.Errorf("persist outcome: %w", err)
	}
	return terminal, runErr
}

const implementPromptTmpl = `You are the implementer in the uta self-improvement loop.

Idea to apply
-------------
Title:    %s
Severity: %s
Source:   %s
Body:
%s

Working directory: %s
Verification command (will run automatically after your edits): %s

Procedure
---------
1. Read whatever files you need to confirm the idea is still applicable in
   this codebase. Language and toolchain agnostic — discover what's there
   before assuming.
2. Apply the smallest correct change that fully addresses the idea. Do NOT
   introduce unrelated edits. Do NOT push, commit, or run destructive shell
   commands.
3. After your edits the verification command will run automatically. If it
   fails you'll be handed its output for another attempt.
4. When you're confident the change is complete and the verification will
   pass, stop and report a one-paragraph summary of what you changed and why.

If on inspection the idea is no longer valid (e.g. the file moved, the
behavior changed, the idea was a hallucination, or it conflicts with a
constraint you can now see), say so plainly and make no edits. The
orchestrator will mark the idea as failed and move on — that's a valid
outcome, not a problem.`

func renderImplementPrompt(idea *Idea, workdir, verify string) string {
	if workdir == "" {
		workdir = "."
	}
	if verify == "" {
		verify = "(none — dry-run mode)"
	}
	body := strings.TrimSpace(idea.Body)
	if body == "" {
		body = "(no body — work from title alone)"
	}
	return fmt.Sprintf(implementPromptTmpl, idea.Title, idea.Severity, idea.Source, body, workdir, verify)
}

func stringifyDuration(end, startp interface{}) string {
	endT, ok1 := end.(*time.Time)
	startT, ok2 := startp.(*time.Time)
	if !ok1 || !ok2 || endT == nil || startT == nil {
		return "?"
	}
	return endT.Sub(*startT).Round(time.Second).String()
}

func strPtr(s string) *string { return &s }

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
