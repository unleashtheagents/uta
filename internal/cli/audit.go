package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// Default critic personas that ship with `uta audit`. Two distinct lenses
// produce useful disagreement; users can add more via --critic.
var defaultCritics = []engine.CriticSpec{
	{
		ID:    "trail-of-bits",
		Title: "Trail-of-Bits style auditor",
		Prompt: `You are an auditor in the Trail of Bits tradition. Read every file under
review carefully. Focus on: arithmetic safety (over/underflow, rounding),
access control gaps, reentrancy, oracle manipulation, front-running, signature
malleability, denial-of-service vectors, upgradeability footguns, and any
deviation between code and stated intent.

Be conservative with severity:
- HIGH = exploitable now, real funds at risk, demonstrable scenario
- MEDIUM = exploitable under non-default conditions OR severe-but-bounded
- LOW = code smell with meaningful security implication
- INFO = correctness/clarity note without direct security impact

Never invent findings. If you are uncertain, lower the severity or omit.`,
	},
	{
		ID:    "openzeppelin-style",
		Title: "OpenZeppelin-style auditor",
		Prompt: `You are an auditor in the OpenZeppelin tradition. Focus on standards
conformance and battle-tested patterns: ERC-20 / ERC-721 / ERC-4626 invariants,
proper use of OZ libraries (or correct re-implementation), event emission
correctness, role-based access control, pausability and emergency procedures,
upgradeability storage layout, and reentrancy guards.

Severity is the same scale as Trail-of-Bits. Where in doubt, prefer
demonstrating the issue with a minimal pseudocode counterexample in the body.`,
	},
}

func newAuditCmd() *cobra.Command {
	var (
		critics        []string
		iter           int
		stopWhen       string
		fix            bool
		extraTools     []string
		worker         string
		fileFlag       string
		workdir        string
		criticTimeout  time.Duration
		reviseTimeout  time.Duration
		runTimeout     time.Duration
		preApprove     []string
		printJSONL     bool
		outputJSON     string
	)
	cmd := &cobra.Command{
		Use:   "audit [path]",
		Short: "audit a directory or codebase with multiple critic agents in a Reflector loop",
		Long: `Run an audit using the Reflector pattern: two (or more) auditor personas
review the material in parallel, findings are deduped and severity-sorted,
and (with --fix) a reviser agent applies fixes between iterations.

Default behavior (no flags): two critics — Trail-of-Bits style and
OpenZeppelin style — run once and write findings.json into the project
context directory. Pass --fix to enable the reviser loop; pass --iter N to
allow up to N revise-then-re-audit cycles.

When run inside a project (uta project init), per-iteration findings land
in <project>/.uta/context/findings.<N>.json with the latest mirrored to
findings.json. When run outside a project, findings are only available via
the recorded trajectory.

Critic identifiers can be passed via --critic <id> repeatedly; uta ships
'trail-of-bits' and 'openzeppelin-style'. Add custom personas by writing
your own workflow YAML and using 'uta run -f' instead.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) > 0 {
				path = args[0]
			}
			absPath, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			st, err := os.Stat(absPath)
			if err != nil {
				return fmt.Errorf("audit target: %w", err)
			}
			if !st.IsDir() {
				return fmt.Errorf("audit target must be a directory: %s", absPath)
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			selected, err := resolveCritics(critics)
			if err != nil {
				return err
			}

			if worker == "" {
				detections := app.Registry.DetectAll(cmd.Context())
				avail := availableProviders(app.Registry.Names(), detections)
				if len(avail) == 0 {
					return errors.New("no provider is installed on PATH. Try installing 'claude' or 'gemini' first.")
				}
				worker = avail[0]
			}

			cond, err := parseStopCondition(stopWhen)
			if err != nil {
				return err
			}

			var reviser *engine.ReviserSpec
			if fix {
				reviser = &engine.ReviserSpec{
					Prompt: `Be surgical. For each finding, apply the minimal fix that addresses the root
cause without changing public API or unrelated behavior. After every batch
of edits, prefer to keep the codebase compiling and passing existing tests.`,
				}
				// Note: tool gate adapters (forge test, npm test) attach here via
				// --tool flags. We pre-wire a single gate (joined by '&&') for v0.4.0;
				// a richer multi-gate story can come later.
				if len(extraTools) > 0 {
					reviser.Gate = &engine.Gate{
						Cmd:     strings.Join(extraTools, " && "),
						Timeout: criticTimeout,
					}
				}
			}

			if workdir == "" {
				if app.InProject() {
					workdir = app.ProjectRoot
				} else {
					workdir = absPath
				}
			}

			// Build env vars (project + audit target paths).
			env := []string{
				"UTA_AUDIT_TARGET=" + absPath,
			}
			if app.InProject() {
				env = append(env,
					"UTA_PROJECT_ROOT="+app.ProjectRoot,
					"UTA_CONTEXT_DIR="+app.ContextDir,
					"UTA_PROJECT_NAME="+app.ProjectName,
				)
			}

			req := engine.ReflectorRequest{
				Goal:           "Audit " + absPath + " for security and correctness issues.",
				InputSummary:   "the codebase rooted at " + absPath,
				Critics:        selected,
				Reviser:        reviser,
				MaxIterations:  iter,
				StopWhen:       cond,
				DefaultWorker:  worker,
				CriticTimeout:  criticTimeout,
				ReviseTimeout:  reviseTimeout,
				RunTimeout:     runTimeout,
				PreApproveTools: preApprove,
				Workdir:         workdir,
				Env:             env,
				WorkflowPath:    fileFlag,
				ContextDir:      app.ContextDir,
			}

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			bus := trajectory.NewBus()
			defer bus.Shutdown()

			noColor, _ := cmd.Flags().GetBool("no-color")
			var renderDone <-chan struct{}
			var jsonlDone chan struct{}
			if printJSONL {
				jsonlDone = make(chan struct{})
				ch := bus.Subscribe(256)
				go streamJSONL(cmd.OutOrStdout(), ch, jsonlDone)
			} else {
				rdr := NewRenderer(cmd.ErrOrStderr(), noColor)
				rdr.ShowGoal("audit "+absPath, worker)
				renderDone = rdr.Subscribe(bus)
			}

			recorder := trajectory.NewRecorder(app.Store)
			sup := engine.New(engine.Deps{
				Store:    app.Store,
				Blobs:    app.Blobs,
				Recorder: recorder,
				Bus:      bus,
				Registry: app.Registry,
			})

			result, runErr := sup.RunReflector(ctx, req)

			bus.Shutdown()
			if jsonlDone != nil {
				<-jsonlDone
			}
			if renderDone != nil {
				<-renderDone
			}

			if runErr != nil {
				if errors.Is(runErr, context.Canceled) {
					fmt.Fprintln(cmd.ErrOrStderr(), "cancelled.")
					os.Exit(130)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "audit failed: %v\n", runErr)
				if result.SessionID != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "session: %s\n", result.SessionID)
				}
				os.Exit(4)
			}

			// Print summary to stdout. Either compact human form, or JSON when -o
			// looks like a path or --output-json is set.
			if result.FinalFindings != nil {
				if outputJSON != "" {
					data, _ := json.MarshalIndent(result.FinalFindings, "", "  ")
					if outputJSON == "-" {
						cmd.OutOrStdout().Write(data)
						cmd.OutOrStdout().Write([]byte("\n"))
					} else {
						if err := os.WriteFile(outputJSON, data, 0o644); err != nil {
							return err
						}
						fmt.Fprintf(cmd.ErrOrStderr(), "findings written to %s\n", outputJSON)
					}
				} else {
					printFindings(cmd.OutOrStdout(), result.FinalFindings)
				}
			}

			fmt.Fprintf(cmd.ErrOrStderr(),
				"\n[uta audit] session %s · iterations=%d · stopped=%s · highest=%s · counts=%v\n",
				result.SessionID, result.Iterations, result.StoppedBecause,
				findingsHighest(result.FinalFindings),
				findingsCounts(result.FinalFindings),
			)

			// Exit non-zero when HIGH findings remain after all iterations.
			if result.FinalFindings != nil && result.FinalFindings.HighestSeverity() == engine.SevHigh {
				os.Exit(5)
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&critics, "critic", nil, "critic personas to run (repeatable). Defaults to: trail-of-bits, openzeppelin-style")
	cmd.Flags().IntVar(&iter, "iter", 3, "max iterations of the audit loop")
	cmd.Flags().StringVar(&stopWhen, "stop-when", "no_high_findings", "stop condition: no_high_findings|no_med_or_above|no_findings|max_iterations")
	cmd.Flags().BoolVar(&fix, "fix", false, "enable the reviser step (otherwise audit-only, one pass)")
	cmd.Flags().StringSliceVar(&extraTools, "tool", nil, "external tool commands to run after each revise (joined by &&); e.g. --tool 'forge test'")
	cmd.Flags().StringVar(&worker, "worker", "", "default worker provider for critics + reviser (auto-pick if unset)")
	cmd.Flags().StringVarP(&fileFlag, "file", "f", "", "workflow YAML override (reserved; uses defaults in v0.4.0)")
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory exposed to critics + reviser (defaults to project root, then audit target)")
	cmd.Flags().DurationVar(&criticTimeout, "critic-timeout", 10*time.Minute, "per-critic timeout")
	cmd.Flags().DurationVar(&reviseTimeout, "revise-timeout", 15*time.Minute, "per-revise timeout")
	cmd.Flags().DurationVar(&runTimeout, "timeout", 2*time.Hour, "overall audit timeout")
	cmd.Flags().StringSliceVar(&preApprove, "pre-approve", nil, "tools the worker may use without prompting (provider-specific)")
	cmd.Flags().BoolVar(&printJSONL, "print-jsonl", false, "stream every trajectory event to stdout as JSONL")
	cmd.Flags().StringVar(&outputJSON, "output-json", "", "write the final FindingsReport JSON to this path ('-' for stdout)")
	return cmd
}

func resolveCritics(ids []string) ([]engine.CriticSpec, error) {
	if len(ids) == 0 {
		return defaultCritics, nil
	}
	byID := map[string]engine.CriticSpec{}
	for _, c := range defaultCritics {
		byID[c.ID] = c
	}
	out := make([]engine.CriticSpec, 0, len(ids))
	var missing []string
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			out = append(out, c)
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) > 0 {
		known := make([]string, 0, len(byID))
		for id := range byID {
			known = append(known, id)
		}
		return nil, fmt.Errorf("unknown critic(s): %s. Built-in critics: %s", strings.Join(missing, ","), strings.Join(known, ","))
	}
	return out, nil
}

func parseStopCondition(s string) (engine.StopCondition, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "no_high_findings", "no-high":
		return engine.StopAtNoHigh, nil
	case "no_med_or_above", "no-med":
		return engine.StopAtNoMedOrAbove, nil
	case "no_findings", "none":
		return engine.StopAtNoFindings, nil
	case "max_iterations", "always":
		return engine.StopAtMaxIterations, nil
	}
	return "", fmt.Errorf("unknown stop-when: %q", s)
}

func findingsHighest(r *engine.FindingsReport) string {
	if r == nil {
		return "n/a"
	}
	return string(r.HighestSeverity())
}

func findingsCounts(r *engine.FindingsReport) map[string]int {
	if r == nil {
		return map[string]int{}
	}
	return r.Stats
}

func printFindings(w interface{ Write([]byte) (int, error) }, r *engine.FindingsReport) {
	fmt.Fprintf(w.(interface{ Write([]byte) (int, error) }), "iteration %d · %d finding(s)\n\n", r.Iteration, len(r.Findings))
	for _, f := range r.Findings {
		fmt.Fprintf(w.(interface{ Write([]byte) (int, error) }), "[%s] %s · %s\n", strings.ToUpper(string(f.Severity)), f.Critic, f.Title)
		if f.File != "" {
			loc := f.File
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Fprintf(w.(interface{ Write([]byte) (int, error) }), "  at %s\n", loc)
		}
		if f.Body != "" {
			body := strings.TrimSpace(f.Body)
			if len(body) > 400 {
				body = body[:400] + "…"
			}
			fmt.Fprintf(w.(interface{ Write([]byte) (int, error) }), "  %s\n", strings.ReplaceAll(body, "\n", "\n  "))
		}
		fmt.Fprintln(w.(interface{ Write([]byte) (int, error) }))
	}
}
