package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/engine/capability"
	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/state"
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
	cmd.AddCommand(newMissionRunCmd(), newMissionCheckCmd(), newMissionTestCmd(), newMissionFmtCmd(), newMissionLspCmd())
	return cmd
}

// loadSteerProgram reads, parses, and checks one .steer file. Diagnostics
// (with source excerpts) go to errw; the returned bool is false when any
// hard error was reported. warnings counts the non-fatal diagnostics so
// callers can surface "ok, with N warning(s)". src is returned so callers
// can run the filesystem-dependent checks (steer.CheckPolicy) with
// excerpt-quality diagnostics.
func loadSteerProgram(path string, errw io.Writer) (prog *steer.Program, warnings int, src string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(errw, "error:", err)
		return nil, 0, "", false
	}
	src = string(data)
	prog, pd := steer.Parse(filepath.Base(path), src)
	if pd != nil {
		fmt.Fprintln(errw, pd.Render())
		return nil, 0, src, false
	}
	diags := steer.Check(prog, src)
	for _, d := range diags {
		fmt.Fprintln(errw, d.Render())
		if d.Warning {
			warnings++
		}
	}
	return prog, warnings, src, !steer.HasErrors(diags)
}

// resolveMissionPolicy resolves a mission's `policy P` declaration to the
// installed MissionProfile it names, and verifies every `with cap C from
// policy P` raise against the profiles' actual grants. Returns (nil, nil)
// when the program declares neither. Unknown names and unbacked raises
// come back as check-style Diags (with did-you-mean suggestions), not
// errors — callers render them and exit with the compile-failure code.
// Per-file profile load problems are warnings on errw, matching
// resolveActiveMode.
func resolveMissionPolicy(app *App, prog *steer.Program, src string, errw io.Writer) (*profile.MissionProfile, []steer.Diag) {
	if prog == nil || prog.Mission == nil {
		return nil, nil
	}
	raises := prog.Mission.CapRaises()
	if prog.Mission.Policy == nil && len(raises) == 0 {
		return nil, nil
	}
	profiles, perrs := profile.LoadAll(app.GlobalHome, app.ProjectRoot)
	for _, e := range perrs {
		fmt.Fprintln(errw, "warn: profile:", e)
	}
	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = p.Name
	}
	if d := steer.CheckPolicy(prog, src, names); d != nil {
		return nil, []steer.Diag{*d}
	}
	if len(raises) > 0 {
		grants := make(map[string][]string, len(profiles))
		for _, p := range profiles {
			grants[p.Name] = p.AllowedTools
		}
		// Grants match by the gate's pattern grammar, not string equality:
		// a policy allowing "Bash(git push *)" backs a raise of
		// "Bash(git push origin main)".
		if ds := steer.CheckCapRaises(prog, src, grants, capability.Subsumes); len(ds) > 0 {
			return nil, ds
		}
	}
	if prog.Mission.Policy == nil {
		return nil, nil
	}
	p, err := profile.Find(profiles, prog.Mission.Policy.Name)
	if err != nil {
		// CheckPolicy just matched the name against this same list, so a
		// miss here is unreachable; keep the Diag shape for uniformity.
		return nil, []steer.Diag{{File: prog.File, Pos: prog.Mission.Policy.Pos, Msg: err.Error()}}
	}
	return p, nil
}

// checkInlineSteer parses + checks steer source that never touched disk
// (the MCP tools receive program text inline). Diagnostics come back as
// structured values rather than rendered text.
func checkInlineSteer(source string) (*steer.Program, []map[string]any, bool) {
	toDiag := func(d steer.Diag) map[string]any {
		sev := "error"
		if d.Warning {
			sev = "warning"
		}
		return map[string]any{"line": d.Pos.Line, "col": d.Pos.Col, "severity": sev, "message": d.Msg}
	}
	prog, pd := steer.Parse("inline.steer", source)
	if pd != nil {
		return nil, []map[string]any{toDiag(*pd)}, false
	}
	diags := steer.Check(prog, source)
	out := make([]map[string]any, len(diags))
	for i, d := range diags {
		out[i] = toDiag(d)
	}
	return prog, out, !steer.HasErrors(diags)
}

// completeSteerFiles restricts shell completion for mission commands to
// .steer files.
func completeSteerFiles(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return []string{"steer"}, cobra.ShellCompDirectiveFilterFileExt
}

// missionCheckDiag / missionCheckResult are the machine-readable form of
// check diagnostics (`uta mission check --json`): one object per diagnostic
// plus an overall ok boolean, so editors and CI annotate without parsing
// caret excerpts. An LSP publishDiagnostics shim is a direct mapping.
type missionCheckDiag struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type missionCheckResult struct {
	OK          bool               `json:"ok"`
	Diagnostics []missionCheckDiag `json:"diagnostics"`
}

func newMissionCheckResult(ok bool, diags []steer.Diag) missionCheckResult {
	res := missionCheckResult{OK: ok, Diagnostics: make([]missionCheckDiag, 0, len(diags))}
	for _, d := range diags {
		sev := "error"
		if d.Warning {
			sev = "warning"
		}
		res.Diagnostics = append(res.Diagnostics, missionCheckDiag{
			File: d.File, Line: d.Pos.Line, Col: d.Pos.Col, Severity: sev, Message: d.Msg,
		})
	}
	return res
}

// checkSteerProgramJSON is loadSteerProgram's structured twin: the same
// read → parse → check pipeline, but diagnostics come back as values
// instead of rendered excerpts.
func checkSteerProgramJSON(path string) (prog *steer.Program, diags []steer.Diag, src string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, "", err
	}
	src = string(data)
	prog, pd := steer.Parse(filepath.Base(path), src)
	if pd != nil {
		return nil, []steer.Diag{*pd}, src, nil
	}
	return prog, steer.Check(prog, src), src, nil
}

// runMissionCheckJSON runs the same checks and exits with the same codes
// as the text path, but emits one JSON document on stdout instead of
// diagnostics on stderr.
func runMissionCheckJSON(cmd *cobra.Command, path string) error {
	prog, diags, src, err := checkSteerProgramJSON(path)
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
		return exitWith(2)
	}
	ok := prog != nil && !steer.HasErrors(diags)
	if ok && (prog.Mission.Policy != nil || len(prog.Mission.CapRaises()) > 0) {
		app, aerr := newApp(cmd.Context())
		if aerr != nil {
			return aerr
		}
		defer app.Close()
		if _, ds := resolveMissionPolicy(app, prog, src, cmd.ErrOrStderr()); len(ds) > 0 {
			diags = append(diags, ds...)
			ok = false
		}
	}
	if eerr := json.NewEncoder(cmd.OutOrStdout()).Encode(newMissionCheckResult(ok, diags)); eerr != nil {
		return eerr
	}
	if !ok {
		return exitWith(2)
	}
	return nil
}

func newMissionCheckCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "check <file.steer>",
		Short: "parse + static-check a steer program without running it",
		Example: `  uta mission check hello.steer
  # error: hello.steer:9:7: unknown function "gret" (did you mean "greet"?)

  # machine-readable diagnostics for editors / CI annotation
  uta mission check --json hello.steer`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSteerFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonOut {
				return runMissionCheckJSON(cmd, args[0])
			}
			prog, warnings, src, ok := loadSteerProgram(args[0], cmd.ErrOrStderr())
			if !ok {
				return exitWith(2)
			}
			// `policy P` and `with cap … from policy P` resolve against the
			// installed MissionProfiles — an unknown name or an unbacked
			// raise is a check-time diagnostic, not a runtime surprise. Only
			// this check needs an App (profile discovery).
			policySuffix := ""
			if prog.Mission.Policy != nil || len(prog.Mission.CapRaises()) > 0 {
				app, err := newApp(cmd.Context())
				if err != nil {
					return err
				}
				defer app.Close()
				pol, ds := resolveMissionPolicy(app, prog, src, cmd.ErrOrStderr())
				if len(ds) > 0 {
					for _, d := range ds {
						fmt.Fprintln(cmd.ErrOrStderr(), d.Render())
					}
					return exitWith(2)
				}
				if pol != nil {
					policySuffix = fmt.Sprintf(", policy %s", pol.Name)
				}
			}
			suffix := ""
			if warnings > 0 {
				suffix = fmt.Sprintf(", %d warning(s)", warnings)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: mission %q — %d agent fn(s), %d call(s), budget %s%s%s\n",
				prog.Mission.Name, len(prog.Agents), len(prog.Mission.Calls()), formatBudget(prog.Mission.Budget), policySuffix, suffix)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit diagnostics as JSON: {ok, diagnostics: [{file, line, col, severity, message}]}")
	return cmd
}

func newMissionRunCmd() *cobra.Command {
	var (
		workerName    string
		workdir       string
		dryRun        bool
		printJSONL    bool
		callTimeout   time.Duration
		resumeSession string
		maxParallel   int
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
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSteerFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			prog, _, src, ok := loadSteerProgram(args[0], cmd.ErrOrStderr())
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

			policy, pds := resolveMissionPolicy(app, prog, src, cmd.ErrOrStderr())
			if len(pds) > 0 {
				for _, d := range pds {
					fmt.Fprintln(cmd.ErrOrStderr(), d.Render())
				}
				return exitWith(2)
			}

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

			// --mode contributes env (model pins, credentials) and trajectory
			// attribution. Budget policy stays with the program text — "cost
			// is spoken in the sentence" — so a mode's token caps are NOT
			// merged over the mission's own budget declaration.
			mode, merr := resolveActiveMode(cmd, app)
			if merr != nil {
				return merr
			}
			var rr engine.RunRequest
			engine.ApplyProfile(&rr, mode)
			env := append(rr.Env, app.ProjectSubtaskEnv()...)

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			bus := trajectory.NewBus()
			defer bus.Shutdown()

			var liveDone chan struct{}
			var renderDone <-chan struct{}
			noColor, _ := cmd.Flags().GetBool("no-color")
			if printJSONL {
				liveDone = make(chan struct{})
				ch := bus.Subscribe(256)
				go streamJSONL(cmd.OutOrStdout(), ch, liveDone)
			} else {
				rdr := NewRenderer(cmd.ErrOrStderr(), noColor)
				rdr.ShowGoal("mission "+prog.Mission.Name, workerName)
				renderDone = rdr.Subscribe(bus)
			}

			sup := engine.New(engine.Deps{
				Store:    app.Store,
				Blobs:    app.Blobs,
				Recorder: trajectory.NewRecorder(app.Store),
				Bus:      bus,
				Registry: app.Registry,
			})

			if resumeSession != "" {
				resolved, rerr := app.Store.ResolveSessionID(resumeSession)
				if rerr != nil {
					return rerr
				}
				resumeSession = resolved
			}

			start := time.Now()
			res, runErr := sup.RunMission(ctx, engine.MissionRequest{
				Program:         prog,
				SourceFile:      filepath.Base(args[0]),
				DefaultWorker:   workerName,
				Available:       available,
				Workdir:         workdir,
				Env:             env,
				CallTimeout:     callTimeout,
				MaxParallel:     maxParallel,
				ModeName:        rr.ModeName,
				Policy:          policy,
				ResumeSessionID: resumeSession,
				HITLStateDir:    app.StateDir,
			})

			// Thread continuity: a mission is a session like any other, so
			// the active thread should point at it afterwards.
			if res.SessionID != "" {
				if terr := state.TouchActiveSession(app.StateDir, res.SessionID); terr != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "warn: update active thread:", terr)
				}
			}

			bus.Shutdown()
			if liveDone != nil {
				<-liveDone
			}
			if renderDone != nil {
				<-renderDone
			}

			if runErr != nil {
				// A gate suspension is a re-enterable state, not a failure:
				// point the operator at the approve + resume pair and exit 0.
				if errors.Is(runErr, engine.ErrMissionSuspended) {
					fmt.Fprintf(cmd.ErrOrStderr(), "\n[uta] mission %s suspended awaiting a human signature (session %s)\n",
						prog.Mission.Name, shortID(res.SessionID))
					fmt.Fprintf(cmd.ErrOrStderr(), "  approve: uta hitl approve %s\n", res.SessionID)
					fmt.Fprintf(cmd.ErrOrStderr(), "  deny:    uta hitl approve %s --deny\n", res.SessionID)
					fmt.Fprintf(cmd.ErrOrStderr(), "  resume:  uta mission run %s --resume-session %s\n", args[0], res.SessionID)
					return nil
				}
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

			if res.FinalAnswer != "" && !printJSONL {
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
	cmd.Flags().BoolVar(&printJSONL, "print-jsonl", false, "stream every trajectory event to stdout as JSONL")
	cmd.Flags().DurationVar(&callTimeout, "call-timeout", 10*time.Minute, "timeout per agent fn call")
	cmd.Flags().StringVar(&resumeSession, "resume-session", "", "replay a prior mission run from its journal: completed calls with matching prompts are recovered (not re-paid), only the frontier re-runs")
	cmd.Flags().IntVar(&maxParallel, "max-parallel", 4, "maximum concurrently running par branches")
	return cmd
}

// renderMissionPlan prints what a mission would do — calls in execution
// order with their worker sets and cost ceilings — without any inference.
func renderMissionPlan(prog *steer.Program) string {
	m := prog.Mission
	var b strings.Builder
	fmt.Fprintf(&b, "mission %s (%s)\n", m.Name, prog.File)
	if m.Policy != nil {
		fmt.Fprintf(&b, "policy: %s\n", m.Policy.Name)
	}
	fmt.Fprintf(&b, "budget: %s\n", formatBudget(m.Budget))
	fmt.Fprintln(&b, "plan:")
	calls := m.Calls()
	for i, call := range calls {
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
		fmt.Fprintf(&b, "  c%d  %s  worker %s  costs %s\n", i+1, call.Name+"(…)", worker, ceiling)
	}
	if len(calls) == 0 {
		fmt.Fprintln(&b, "  (no agent calls)")
	}
	emits := 0
	for _, st := range m.Stmts {
		if _, ok := st.(*steer.EmitStmt); ok {
			emits++
		}
	}
	fmt.Fprintf(&b, "emits: %d\n", emits)
	return b.String()
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
