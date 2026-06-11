package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/eval"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

func newEvalCmd() *cobra.Command {
	var (
		suiteFile string
		worker    string
		format    string
	)
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "run a golden-goal eval suite and grade the outcomes",
		Long: `Execute every case in an eval suite (YAML) through the real
orchestrator and grade each against its assertions: contains /
not_contains, min_chars, max_usd_cents, max_tokens, an expected
terminal status, and an arbitrary shell gate (final answer on stdin
and in $UTA_EVAL_ANSWER).

A suite can pin a worker matrix (workers: [claude, gemini]) — every
case runs once per worker so provider regressions show up side by
side. Sessions are recorded normally: every cell's session id appears
in the report and is replayable via uta trajectory.

Exit codes: 0 all cells passed · 5 at least one failed.

Example suite: examples/evals/smoke.yaml. Run it:

  uta eval -f examples/evals/smoke.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			suite, err := eval.LoadSuite(suiteFile)
			if err != nil {
				return err
			}
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			fallback := worker
			if fallback == "" {
				dets := app.Registry.DetectAll(cmd.Context())
				avail := availableProviders(app.Registry.Names(), dets)
				if len(avail) == 0 {
					return fmt.Errorf("no provider on PATH; install claude or gemini, or pass --worker")
				}
				fallback = avail[0]
			}

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			runner := makeEvalRunner(app)
			onProgress := func(cell eval.CellResult) {
				icon := "PASS"
				if !cell.Passed {
					icon = "FAIL"
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "[eval] %-4s %s · %s · %s\n",
					icon, cell.Case, cell.Worker, cell.Elapsed.Round(time.Millisecond))
				for _, ch := range cell.Checks {
					if !ch.Passed {
						fmt.Fprintf(cmd.ErrOrStderr(), "       ✗ %s — %s\n", ch.Name, ch.Detail)
					}
				}
			}

			report := eval.RunSuite(ctx, suite, fallback, runner, onProgress)

			switch format {
			case "json":
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
			case "", "pretty":
				renderEvalReport(cmd, report)
			default:
				return fmt.Errorf("unknown format: %s", format)
			}
			if !report.AllPassed() {
				return exitWith(5)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&suiteFile, "file", "f", "evals.yaml", "eval suite YAML")
	cmd.Flags().StringVar(&worker, "worker", "", "fallback worker when neither suite nor case pins one")
	cmd.Flags().StringVar(&format, "format", "pretty", "output format: pretty|json")
	return cmd
}

// makeEvalRunner adapts the supervisor to eval.RunFunc. Each cell is a
// real recorded session — same store, same trajectory, same budget
// machinery as a hand-typed `uta run`.
func makeEvalRunner(app *App) eval.RunFunc {
	return func(ctx context.Context, c eval.Case, worker string) eval.RunOutcome {
		bus := trajectory.NewBus()
		defer bus.Shutdown()
		sup := engine.New(engine.Deps{
			Store: app.Store, Blobs: app.Blobs,
			Recorder: trajectory.NewRecorder(app.Store),
			Bus:      bus, Registry: app.Registry,
			Memory:     memory.NewFromEnv(app.Store.DB),
			Whiteboard: whiteboard.New(app.Store.DB),
		})
		timeout := c.Timeout
		if timeout == 0 {
			timeout = 10 * time.Minute
		}
		req := engine.RunRequest{
			Goal:           c.Goal,
			WorkerName:     worker,
			SubtaskTimeout: timeout,
			RunTimeout:     timeout + 5*time.Minute,
			Workdir:        c.Workdir,
			ModeName:       c.Mode,
		}
		if c.Mode != "" {
			if mode, merr := resolveModeByName(app, c.Mode); merr == nil && mode != nil {
				engine.ApplyProfile(&req, mode)
			}
		}
		res, err := sup.Run(ctx, req)
		out := eval.RunOutcome{
			SessionID:   res.SessionID,
			Status:      res.Status,
			FinalAnswer: res.FinalAnswer,
			Err:         err,
		}
		// Usage comes from the cost rollup the supervisor persists on
		// subtask meta_json — one query covers every subtask of the
		// session.
		if res.SessionID != "" {
			if in, outTok, cents, qerr := sessionUsage(app, res.SessionID); qerr == nil {
				out.TokensIn, out.TokensOut, out.USDCents = in, outTok, cents
			}
		}
		return out
	}
}

// sessionUsage sums the usage triplet across a session's subtasks.
func sessionUsage(app *App, sessionID string) (in, out, cents int64, err error) {
	row := app.Store.DB.QueryRow(`
		SELECT
		  COALESCE(SUM(CAST(json_extract(meta_json, '$.tokens_in')  AS INTEGER)), 0),
		  COALESCE(SUM(CAST(json_extract(meta_json, '$.tokens_out') AS INTEGER)), 0),
		  COALESCE(SUM(CAST(json_extract(meta_json, '$.usd_cents')  AS INTEGER)), 0)
		FROM subtasks WHERE session_id = ?`, sessionID)
	err = row.Scan(&in, &out, &cents)
	return in, out, cents, err
}

func renderEvalReport(cmd *cobra.Command, rep *eval.Report) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "suite %q: %d passed, %d failed\n\n", rep.Suite, rep.Passed, rep.Failed)
	fmt.Fprintf(out, "%-6s  %-28s  %-12s  %-10s  %s\n", "RESULT", "CASE", "WORKER", "ELAPSED", "SESSION")
	for _, cell := range rep.Cells {
		result := "pass"
		if !cell.Passed {
			result = "FAIL"
		}
		fmt.Fprintf(out, "%-6s  %-28s  %-12s  %-10s  %s\n",
			result, truncateLine(cell.Case, 28), cell.Worker,
			cell.Elapsed.Round(time.Millisecond), shortID(cell.SessionID))
		for _, ch := range cell.Checks {
			if !ch.Passed {
				fmt.Fprintf(out, "        ✗ %s — %s\n", ch.Name, ch.Detail)
			}
		}
	}
}
