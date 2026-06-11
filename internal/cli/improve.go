package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/improve"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/sentinel"
	"github.com/unleashtheagents/uta/internal/state"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

func newImproveCmd() *cobra.Command {
	var (
		verifyCmd       string
		worker          string
		gatherWorker    string
		budget          time.Duration
		perIdeaTimeout  time.Duration
		gatherTimeout   time.Duration
		gatherMaxIdeas  int
		gatherWhenEmpty bool
		gatherGoal      string
		maxIters        int
		maxRetries      int
		dryRun          bool
		preApprove      []string
	)
	cmd := &cobra.Command{
		Use:   "improve",
		Short: "run the self-improvement loop until budget / no-ideas / cancel",
		Long: `Pulls the highest-priority idea from the board, executes it as a single
DAG subtask whose post-edit gate is the supplied --verify command, records
the outcome, and loops. With --gather-when-empty, calls a gatherer worker
(defaults to gemini) automatically when the backlog dries up.

The loop is language-agnostic. Supply --verify as the command that means
"this codebase is still healthy" in your stack. Examples:

  Go        --verify 'go test ./...'
  Node      --verify 'npm test'
  Python    --verify 'pytest -q'
  Rust      --verify 'cargo test'
  Solidity  --verify 'forge test'
  Generic   --verify 'make verify'

uta enforces budget + the Sentinel watcher at the top, so a multi-hour
autonomous run is safe to leave unattended. State is persisted in the
project DB so killing and resuming the loop is a no-op (--resume just
means running the command again).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			if !dryRun && verifyCmd == "" {
				return errors.New("--verify is required (e.g. 'go test ./...', 'npm test', 'pytest -q', 'forge test'). Or use --dry-run.")
			}
			if _, ok := app.Registry.Get(worker); !ok {
				return fmt.Errorf("worker %q not registered. See `uta providers`.", worker)
			}
			if gatherWhenEmpty {
				if _, ok := app.Registry.Get(gatherWorker); !ok {
					return fmt.Errorf("gather-worker %q not registered. See `uta providers`.", gatherWorker)
				}
			}

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			bus := trajectory.NewBus()
			defer bus.Shutdown()

			noColor, _ := cmd.Flags().GetBool("no-color")

			var renderDone <-chan struct{}
			if isTTY(cmd.ErrOrStderr()) {
				rdr := NewRenderer(cmd.ErrOrStderr(), noColor)
				rdr.ShowGoal("self-improve loop", worker)
				renderDone = rdr.Subscribe(bus)
			}

			recorder := trajectory.NewRecorder(app.Store)
			sup := engine.New(engine.Deps{
				Store:      app.Store,
				Blobs:      app.Blobs,
				Recorder:   recorder,
				Bus:        bus,
				Registry:   app.Registry,
				Memory:     memory.NewFromEnv(app.Store.DB),
				Whiteboard: whiteboard.New(app.Store.DB),
			})

			// Sentinel watches the WHOLE loop's trajectory bus — every
			// iteration's events flow through it, so loop-detection and
			// error-rate spike rules accumulate across iterations.
			sent := sentinel.NewSentinel(sentinel.Config{
				BudgetWallClock: budget,
				PublishOnBus:    bus,
				Recorder:        recorder,
				Cancel:          cancel,
			})
			go sent.Watch(ctx, bus)

			workdir := app.ProjectRoot
			if workdir == "" {
				workdir, _ = os.Getwd()
			}
			env := []string{}
			if app.InProject() {
				env = []string{
					"UTA_PROJECT_ROOT=" + app.ProjectRoot,
					"UTA_CONTEXT_DIR=" + app.ContextDir,
					"UTA_PROJECT_NAME=" + app.ProjectName,
				}
			}

			mode, err := resolveActiveMode(cmd, app)
			if err != nil {
				return err
			}

			board := improve.NewBoard(app.Store)
			fmt.Fprintf(cmd.ErrOrStderr(),
				"improve loop · worker=%s · gather-worker=%s · verify=%q · budget=%s · gather-when-empty=%t\n",
				worker, gatherWorker, verifyCmd, budgetStr(budget), gatherWhenEmpty)

			start := time.Now()
			loopReq := improve.LoopRequest{
				Worker:          worker,
				GatherWorker:    gatherWorker,
				Workdir:         workdir,
				Env:             env,
				VerifyCommand:   verifyCmd,
				PerIdeaTimeout:  perIdeaTimeout,
				GatherTimeout:   gatherTimeout,
				Budget:          budget,
				MaxIterations:   maxIters,
				GatherWhenEmpty: gatherWhenEmpty,
				GatherGoal:      gatherGoal,
				GatherMaxIdeas:  gatherMaxIdeas,
				DryRun:          dryRun,
				MaxRetries:      maxRetries,
				PreApproveTools: preApprove,
				OnIterationSession: func(sessionID string) {
					if terr := state.TouchActiveSession(app.StateDir, sessionID); terr != nil {
						fmt.Fprintln(cmd.ErrOrStderr(), "warn: update active thread:", terr)
					}
					if mode != nil && mode.RetrospectiveEvery > 0 && app.InProject() {
						res, rerr := improve.MaybeRetrospective(ctx, improve.RetroDeps{
							Store:    app.Store,
							Registry: app.Registry,
							Recorder: recorder,
							Bus:      bus,
						}, improve.RetroRequest{
							ModeName:       mode.Name,
							ProjectRoot:    app.ProjectRoot,
							WorkerName:     worker,
							Every:          mode.RetrospectiveEvery,
							PromptTemplate: mode.RetrospectivePrompt,
							Workdir:        workdir,
							Env:            env,
							Timeout:        perIdeaTimeout,
							PriorSessionID: sessionID,
						})
						if rerr != nil {
							fmt.Fprintln(cmd.ErrOrStderr(), "warn: retrospective:", rerr)
						} else if res != nil && res.Triggered && res.Path != "" {
							fmt.Fprintf(cmd.ErrOrStderr(),
								"[uta] retrospective for %q written to %s (after %d sessions)\n",
								mode.Name, res.Path, res.SessionCount)
						}
					}
				},
			}
			// Apply the active MissionProfile's policies to the loop
			// request. Each iteration's inner RunRequest inherits these,
			// so capability gates / HITL triggers / budget caps fire on
			// every picked idea — identical preamble to `uta run --mode`.
			improve.ApplyProfile(&loopReq, mode)
			// MCP bridge: probe each MCP server declared on the active
			// mode and forward the materialized config path onto every
			// iteration's inner RunRequest. Mirrors `uta run --mode`
			// behavior so the improve loop's worker sees the same
			// toolset a one-shot run would.
			if mode != nil && len(mode.MCPServers) > 0 {
				cfgPath, patterns, probes := engine.ProbeAndWriteMCPConfig(ctx, mode)
				for _, p := range probes {
					if diag := engine.FormatMCPProbeError(p); diag != "" {
						fmt.Fprintln(cmd.ErrOrStderr(), "warn:", diag)
					}
				}
				loopReq.MCPConfigPath = cfgPath
				loopReq.PreApproveTools = append(loopReq.PreApproveTools, patterns...)
				if cfgPath != "" {
					defer os.Remove(cfgPath)
				}
			}
			res, runErr := improve.Loop(ctx, sup, board, app.Registry, loopReq)
			elapsed := time.Since(start)
			bus.Shutdown()
			if renderDone != nil {
				<-renderDone
			}

			if runErr != nil {
				if errors.Is(runErr, context.Canceled) {
					fmt.Fprintln(cmd.ErrOrStderr(), "cancelled.")
					return exitWith(130)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "improve failed: %v\n", runErr)
				return exitWith(4)
			}

			stats, _ := board.Stats()
			fmt.Fprintf(cmd.OutOrStdout(),
				"\n[uta improve] elapsed=%s iterations=%d done=%d failed=%d gathered=%d gather-calls=%d stopped=%s\n",
				elapsed.Round(time.Second), res.Iterations, res.IdeasCompleted, res.IdeasFailed,
				res.IdeasGathered, res.GatherCalls, res.StoppedBecause)
			fmt.Fprintf(cmd.OutOrStdout(), "board now: %v\n", stats)
			return nil
		},
	}
	cmd.Flags().StringVar(&verifyCmd, "verify", "",
		"verification command run after each iteration's edits. REQUIRED unless --dry-run.\n"+
			"Examples: 'go test ./...', 'npm test', 'pytest -q', 'cargo test', 'forge test', 'make verify'")
	cmd.Flags().StringVar(&worker, "worker", "claude", "provider that implements each idea")
	cmd.Flags().StringVar(&gatherWorker, "gather-worker", "gemini", "provider that gathers new ideas (used only with --gather-when-empty)")
	cmd.Flags().DurationVar(&budget, "budget", 0, "total wall-clock budget; 0 = no cap (use Ctrl-C to stop)")
	cmd.Flags().DurationVar(&perIdeaTimeout, "per-idea-timeout", 20*time.Minute, "per-iteration cap")
	cmd.Flags().DurationVar(&gatherTimeout, "gather-timeout", 10*time.Minute, "cap for one gather call")
	cmd.Flags().IntVar(&gatherMaxIdeas, "gather-max", 10, "max ideas per gather call")
	cmd.Flags().BoolVar(&gatherWhenEmpty, "gather-when-empty", false, "auto-call the gatherer when the backlog empties")
	cmd.Flags().StringVar(&gatherGoal, "gather-goal", "", "optional steering text appended to the gather prompt")
	cmd.Flags().IntVar(&maxIters, "max-iter", 0, "hard ceiling on iterations regardless of budget; 0 = no cap")
	cmd.Flags().IntVar(&maxRetries, "max-retries", 1, "verify-gate retries per iteration before marking failed")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "pick ideas + run them but SKIP the verify gate (smoke-test the loop)")
	cmd.Flags().StringArrayVar(&preApprove, "pre-approve", nil, "tools the worker may use without prompting (provider-specific)")
	return cmd
}

func budgetStr(d time.Duration) string {
	if d == 0 {
		return "unlimited"
	}
	return d.String()
}

func isTTY(w interface{}) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd())
}
