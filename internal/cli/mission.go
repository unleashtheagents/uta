package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/steer"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

func newMissionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mission",
		Short: "run steer programs — the agent programming language (v0 interpreter)",
		Long: `steer is the programming layer above the uta verbs (see Paper № 02):
inference is an effect, verification is a type, budget is a linear resource.
This is the v0 interpreter: 'uta mission run' walks a .steer program,
turning every agent fn call into a provider subtask, journaled to the
normal session/trajectory store. 'uta mission check' runs the static
checks without spending a token.`,
	}
	cmd.AddCommand(newMissionRunCmd(), newMissionCheckCmd())
	return cmd
}

// loadSteerProgram reads, parses, and checks one .steer file. Diagnostics
// (with source excerpts) go to errw; the returned bool is false when any
// hard error was reported.
func loadSteerProgram(path string, errw io.Writer) (*steer.Program, string, bool) {
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(errw, "error:", err)
		return nil, "", false
	}
	prog, pd := steer.Parse(filepath.Base(path), string(src))
	if pd != nil {
		fmt.Fprintln(errw, pd.Render())
		return nil, "", false
	}
	diags := steer.Check(prog, string(src))
	for _, d := range diags {
		fmt.Fprintln(errw, d.Render())
	}
	return prog, string(src), !steer.HasErrors(diags)
}

func newMissionCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <file.steer>",
		Short: "parse + static-check a steer program without running it",
		Example: `  uta mission check hello.steer
  # error: hello.steer:9:7: unknown function "gret" (did you mean "greet"?)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prog, _, ok := loadSteerProgram(args[0], cmd.ErrOrStderr())
			if !ok {
				return exitWith(2)
			}
			calls := countMissionCalls(prog)
			fmt.Fprintf(cmd.OutOrStdout(), "ok: mission %q — %d agent fn(s), %d call(s), budget %s\n",
				prog.Mission.Name, len(prog.Agents), calls, formatBudget(prog.Mission.Budget))
			return nil
		},
	}
}

func newMissionRunCmd() *cobra.Command {
	var (
		workerName  string
		workdir     string
		dryRun      bool
		callTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "run <file.steer>",
		Short: "interpret a steer program: agent calls become subtasks, emit becomes the answer",
		Example: `  # run the hello world (examples/hello.steer)
  uta mission run hello.steer

  # static plan only — no tokens spent
  uta mission run hello.steer --dry-run

  # a shebang makes .steer files executable:
  #   #!/usr/bin/env -S uta mission run
  ./hello.steer`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prog, _, ok := loadSteerProgram(args[0], cmd.ErrOrStderr())
			if !ok {
				return exitWith(2)
			}

			if dryRun {
				fmt.Fprint(cmd.OutOrStdout(), renderMissionPlan(prog))
				return nil
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			detections := app.Registry.DetectAll(cmd.Context())
			available := availableProviders(app.Registry.Names(), detections)
			if len(available) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), errNoProviders)
				return exitWith(3)
			}
			if workerName == "" {
				workerName = available[0]
			} else if !containsString(available, workerName) {
				det := detections[workerName]
				return fmt.Errorf("provider %q is not available: %s", workerName, firstNonEmptyStr(det.Notes, "not detected"))
			}

			if app.InProject() && workdir == "" {
				workdir = app.ProjectRoot
			}

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			bus := trajectory.NewBus()
			defer bus.Shutdown()
			noColor, _ := cmd.Flags().GetBool("no-color")
			rdr := NewRenderer(cmd.ErrOrStderr(), noColor)
			rdr.ShowGoal("mission "+prog.Mission.Name, workerName)
			renderDone := rdr.Subscribe(bus)

			sup := engine.New(engine.Deps{
				Store:    app.Store,
				Blobs:    app.Blobs,
				Recorder: trajectory.NewRecorder(app.Store),
				Bus:      bus,
				Registry: app.Registry,
			})

			start := time.Now()
			res, runErr := sup.RunMission(ctx, engine.MissionRequest{
				Program:       prog,
				SourceFile:    filepath.Base(args[0]),
				DefaultWorker: workerName,
				Available:     available,
				Workdir:       workdir,
				Env:           app.ProjectSubtaskEnv(),
				CallTimeout:   callTimeout,
			})

			bus.Shutdown()
			<-renderDone

			if runErr != nil {
				if errors.Is(runErr, context.Canceled) {
					fmt.Fprintln(cmd.ErrOrStderr(), "cancelled.")
					if res.SessionID != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "session: %s\n", shortID(res.SessionID))
					}
					return exitWith(130)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "mission failed: %v\n", runErr)
				if res.SessionID != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "session: %s (see 'uta trajectory %s')\n",
						shortID(res.SessionID), shortID(res.SessionID))
				}
				if res.Status == "budget_exhausted" {
					return exitWith(5)
				}
				return exitWith(4)
			}

			if res.FinalAnswer != "" {
				fmt.Fprintln(cmd.OutOrStdout(), res.FinalAnswer)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "\n[uta] mission %s session %s status=%s calls=%d duration=%s%s\n",
				prog.Mission.Name, shortID(res.SessionID), res.Status, len(res.Subtasks),
				time.Since(start).Round(time.Second), usageSummary(res.Subtasks))
			return nil
		},
	}
	cmd.Flags().StringVar(&workerName, "worker", "", "default worker for agent fns without a worker clause (defaults to the first detected provider)")
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory exposed to the workers (defaults to CWD / project root)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the execution plan without calling any agent")
	cmd.Flags().DurationVar(&callTimeout, "call-timeout", 10*time.Minute, "timeout per agent fn call")
	return cmd
}

// renderMissionPlan prints what a mission would do — calls in execution
// order with their worker sets and cost ceilings — without any inference.
func renderMissionPlan(prog *steer.Program) string {
	m := prog.Mission
	var b strings.Builder
	fmt.Fprintf(&b, "mission %s (%s)\n", m.Name, prog.File)
	fmt.Fprintf(&b, "budget: %s\n", formatBudget(m.Budget))
	fmt.Fprintln(&b, "plan:")
	n := 0
	emits := 0
	for _, st := range m.Stmts {
		switch s := st.(type) {
		case *steer.LetStmt:
			n = planExpr(&b, prog, s.Expr, n)
		case *steer.EmitStmt:
			n = planExpr(&b, prog, s.Expr, n)
			emits++
		}
	}
	if n == 0 {
		fmt.Fprintln(&b, "  (no agent calls)")
	}
	fmt.Fprintf(&b, "emits: %d\n", emits)
	return b.String()
}

func planExpr(b *strings.Builder, prog *steer.Program, e steer.Expr, n int) int {
	call, ok := e.(*steer.CallExpr)
	if !ok {
		return n
	}
	for _, a := range call.Args {
		n = planExpr(b, prog, a, n)
	}
	n++
	fn := prog.Agent(call.Name)
	worker := "(default worker)"
	if len(fn.Workers) == 1 {
		worker = fn.Workers[0]
	} else if len(fn.Workers) > 1 {
		worker = "any(" + strings.Join(fn.Workers, ", ") + ")"
	}
	ceiling := "uncapped"
	if fn.CostTokens > 0 {
		ceiling = fmt.Sprintf("<= %s tokens", formatTokens(fn.CostTokens))
	}
	fmt.Fprintf(b, "  c%d  %s  worker %s  costs %s\n", n, call.Name+"(…)", worker, ceiling)
	return n
}

func countMissionCalls(prog *steer.Program) int {
	n := 0
	var walk func(e steer.Expr)
	walk = func(e steer.Expr) {
		if c, ok := e.(*steer.CallExpr); ok {
			n++
			for _, a := range c.Args {
				walk(a)
			}
		}
	}
	for _, st := range prog.Mission.Stmts {
		switch s := st.(type) {
		case *steer.LetStmt:
			walk(s.Expr)
		case *steer.EmitStmt:
			walk(s.Expr)
		}
	}
	return n
}

func formatBudget(b *steer.BudgetDecl) string {
	if b == nil {
		return "(none)"
	}
	var parts []string
	if b.Tokens > 0 {
		parts = append(parts, formatTokens(b.Tokens)+" tokens")
	}
	if b.USDCents > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", float64(b.USDCents)/100))
	}
	if b.Deadline > 0 {
		parts = append(parts, b.Deadline.String())
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}

func formatTokens(n int64) string {
	if n%1000 == 0 && n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%d", n)
}
