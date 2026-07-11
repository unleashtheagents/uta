package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/sentinel"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

// resolvePersonas merges engine.BuiltinPersonas with any user-defined personas
// loaded from ~/.uta/personas/ at app startup. User personas with the same
// id as a built-in override the built-in only when force: true.
func resolvePersonas(app *App) map[string]engine.CriticSpec {
	out := map[string]engine.CriticSpec{}
	for _, p := range engine.BuiltinPersonas {
		out[p.ID] = p
	}
	for _, up := range app.UserPersonas {
		_, exists := out[up.ID]
		if exists && !up.Force {
			continue
		}
		out[up.ID] = engine.CriticSpec{
			ID:     up.ID,
			Title:  up.Title,
			Prompt: up.Prompt,
			Worker: up.Worker,
		}
	}
	return out
}

func newAuditCmd() *cobra.Command {
	var (
		critics       []string
		iter          int
		stopWhen      string
		fix           bool
		extraTools    []string
		postGate      string
		toolsOnly     bool
		worker        string
		fileFlag      string
		workdir       string
		criticTimeout time.Duration
		toolTimeout   time.Duration
		reviseTimeout time.Duration
		runTimeout    time.Duration
		budgetTime    time.Duration
		preApprove    []string
		printJSONL    bool
		outputJSON    string
		outputSARIF   string
		listPersonas  bool
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
			if listPersonas {
				app, err := newApp(cmd.Context())
				if err != nil {
					return err
				}
				defer app.Close()
				printPersonaList(cmd.OutOrStdout(), app)
				return nil
			}

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
			// targetDir is the directory the auditor agents see as their
			// workdir. When the user passes a single contract file (e.g.
			// `uta audit ./Token.sol`) we audit its containing directory
			// and surface the file path via the UTA_AUDIT_FILE env var.
			targetDir := absPath
			isFileTarget := !st.IsDir()
			if isFileTarget {
				targetDir = filepath.Dir(absPath)
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			mode, err := resolveActiveMode(cmd, app)
			if err != nil {
				return err
			}
			// When --mode is set and the user did not pass --critic, the
			// profile's personas become the critic list. This matches the
			// "lets users override personas without rewriting flags"
			// behavior the audit profile is designed for.
			if len(critics) == 0 && mode != nil && len(mode.Personas) > 0 {
				critics = mode.Personas
			}

			selected, err := resolveCritics(app, critics)
			if err != nil {
				return err
			}
			if toolsOnly {
				selected = nil
			}
			if mode != nil && app.InProject() {
				selected, err = applyVibeToCritics(app.ProjectRoot, mode.Name, selected)
				if err != nil {
					return err
				}
			}
			tools := buildToolSpecs(extraTools, toolTimeout)
			// Profile-declared tools are appended after --tool flags so a
			// user can still extend the audit profile's bundle without
			// having to override it wholesale.
			if mode != nil && len(mode.Tools) > 0 {
				tools = append(tools, buildToolSpecs(mode.Tools, toolTimeout)...)
			}
			if !toolsOnly && len(selected) == 0 && len(tools) == 0 {
				return errors.New("nothing to run: pass --critic, --tool, or omit --tools-only")
			}
			if toolsOnly && len(tools) == 0 {
				return errors.New("--tools-only requires at least one --tool")
			}

			if worker == "" && !toolsOnly {
				detections := app.Registry.DetectAll(cmd.Context())
				avail := availableProviders(app.Registry.Names(), detections)
				if len(avail) == 0 {
					return errNoProviders
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
				if postGate != "" {
					reviser.Gate = &engine.Gate{Cmd: postGate, Timeout: toolTimeout}
				}
			}

			if workdir == "" {
				if app.InProject() {
					workdir = app.ProjectRoot
				} else {
					workdir = targetDir
				}
			}

			// Build env vars (project + audit target paths).
			env := []string{
				"UTA_AUDIT_TARGET=" + absPath,
			}
			if isFileTarget {
				env = append(env, "UTA_AUDIT_FILE="+absPath)
			}
			if app.InProject() {
				env = append(env,
					"UTA_PROJECT_ROOT="+app.ProjectRoot,
					"UTA_CONTEXT_DIR="+app.ContextDir,
					"UTA_PROJECT_NAME="+app.ProjectName,
				)
			}

			inputSummary := "the codebase rooted at " + absPath
			if isFileTarget {
				inputSummary = "the contract at " + absPath
			}
			req := engine.ReflectorRequest{
				Goal:            "Audit " + absPath + " for security and correctness issues.",
				InputSummary:    inputSummary,
				Critics:         selected,
				Tools:           tools,
				ToolsOnly:       toolsOnly,
				Reviser:         reviser,
				MaxIterations:   iter,
				StopWhen:        cond,
				DefaultWorker:   worker,
				CriticTimeout:   criticTimeout,
				ReviseTimeout:   reviseTimeout,
				RunTimeout:      runTimeout,
				PreApproveTools: preApprove,
				Workdir:         workdir,
				Env:             env,
				WorkflowPath:    fileFlag,
				ContextDir:      app.ContextDir,
			}
			// Apply the active MissionProfile's policies: env merge,
			// capability gates, HITL triggers, budget caps, memory
			// consolidation, mode name. Identical preamble shape to
			// `uta run --mode` and `uta resume --mode`. Replaces the
			// previous ad-hoc env-merge + intersection that only
			// covered two of the policy axes.
			engine.ApplyProfileToReflector(&req, mode)

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			// MCP bridge: probe each MCP server declared on the active
			// mode and write a synthesized config the provider can
			// consume via --mcp-config. Mirrors `uta run` so critics see
			// the same toolset the mode profile advertises.
			if mode != nil && len(mode.MCPServers) > 0 {
				probes := engine.ApplyMCPBridgeToReflector(ctx, &req, mode)
				for _, p := range probes {
					if diag := engine.FormatMCPProbeError(p); diag != "" {
						fmt.Fprintln(cmd.ErrOrStderr(), "warn:", diag)
					}
				}
				if req.MCPConfigPath != "" {
					defer os.Remove(req.MCPConfigPath)
				}
			}

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
				Store:      app.Store,
				Blobs:      app.Blobs,
				Recorder:   recorder,
				Bus:        bus,
				Registry:   app.Registry,
				Memory:     memory.NewFromEnv(app.Store.DB),
				Whiteboard: whiteboard.New(app.Store.DB),
			})

			// Sentinel: watcher subscribes to the trajectory bus, can cancel
			// the run via the context if a critical alert fires. Persists its
			// alerts via the same recorder the supervisor uses.
			sent := sentinel.NewSentinel(sentinel.Config{
				BudgetWallClock: budgetTime,
				PublishOnBus:    bus,
				Recorder:        recorder,
				Cancel:          cancel,
			})
			go sent.Watch(ctx, bus)

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
					return exitWith(130)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "audit failed: %v\n", runErr)
				if result.SessionID != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "session: %s\n", result.SessionID)
				}
				return exitWith(4)
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
				if outputSARIF != "" {
					sarif := engine.ToSARIF(result.FinalFindings, absPath)
					data, _ := json.MarshalIndent(sarif, "", "  ")
					if outputSARIF == "-" {
						cmd.OutOrStdout().Write(data)
						cmd.OutOrStdout().Write([]byte("\n"))
					} else {
						if err := os.WriteFile(outputSARIF, data, 0o644); err != nil {
							return err
						}
						fmt.Fprintf(cmd.ErrOrStderr(), "SARIF written to %s\n", outputSARIF)
					}
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
				return exitWith(5)
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&critics, "critic", nil, "critic personas to run (repeatable). Defaults to: trail-of-bits, openzeppelin-style")
	cmd.Flags().IntVar(&iter, "iter", 3, "max iterations of the audit loop")
	cmd.Flags().StringVar(&stopWhen, "stop-when", "no_high_findings", "stop condition: no_high_findings|no_med_or_above|no_findings|max_iterations")
	cmd.Flags().BoolVar(&fix, "fix", false, "enable the reviser step (otherwise audit-only, one pass)")
	cmd.Flags().StringArrayVar(&extraTools, "tool", nil,
		"external analysis tool to run each iteration; produces findings in the same report.\n"+
			"Built-in adapters when name matches: 'slither', 'mythril', 'forge-test', 'aderyn'.\n"+
			"Otherwise "+
			"the value is run as a raw shell command (use id=cmd to set an id, e.g. 'lint=npm run lint').\n"+
			"Each --tool is one verbatim string; safe for commas/quotes — repeat the flag for multiple tools.")
	cmd.Flags().StringVar(&postGate, "post-gate", "", "shell command run after each reviser revision; nonzero exit aborts the audit (replaces v0.4.0's tools-as-gates)")
	cmd.Flags().BoolVar(&toolsOnly, "tools-only", false, "skip LLM critics; run only --tool adapters")
	cmd.Flags().StringVar(&worker, "worker", "", "default worker provider for critics + reviser (auto-pick if unset)")
	cmd.Flags().StringVarP(&fileFlag, "file", "f", "", "workflow YAML override (reserved; uses defaults in v0.4.x)")
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory exposed to critics + tools + reviser (defaults to project root, then audit target)")
	cmd.Flags().DurationVar(&criticTimeout, "critic-timeout", 10*time.Minute, "per-critic timeout")
	cmd.Flags().DurationVar(&toolTimeout, "tool-timeout", 10*time.Minute, "per-tool timeout (slither/mythril/forge can be slow on large repos)")
	cmd.Flags().DurationVar(&reviseTimeout, "revise-timeout", 15*time.Minute, "per-revise timeout")
	cmd.Flags().DurationVar(&runTimeout, "timeout", 2*time.Hour, "overall audit timeout")
	cmd.Flags().StringArrayVar(&preApprove, "pre-approve", nil, "tools the worker may use without prompting (provider-specific). Repeat the flag.")
	cmd.Flags().BoolVar(&printJSONL, "print-jsonl", false, "stream every trajectory event to stdout as JSONL")
	cmd.Flags().StringVar(&outputJSON, "output-json", "", "write the final FindingsReport JSON to this path ('-' for stdout)")
	cmd.Flags().StringVar(&outputSARIF, "output-sarif", "", "also write a SARIF 2.1.0 report to this path ('-' for stdout)")
	cmd.Flags().DurationVar(&budgetTime, "budget-time", 0, "wall-clock budget. The Sentinel emits a warning at 80% and cancels the run at 100%.")
	cmd.Flags().BoolVar(&listPersonas, "list-personas", false, "list known critic personas (built-ins + ~/.uta/personas/) and exit")
	return cmd
}

func printPersonaList(w io.Writer, app *App) {
	reg := resolvePersonas(app)
	ids := make([]string, 0, len(reg))
	for id := range reg {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	userIDs := map[string]bool{}
	for _, up := range app.UserPersonas {
		userIDs[up.ID] = true
	}
	fmt.Fprintln(w, "Available critic personas:")
	for _, id := range ids {
		p := reg[id]
		src := "built-in"
		if userIDs[id] {
			src = "user"
		}
		title := p.Title
		if title == "" {
			title = "(no title)"
		}
		fmt.Fprintf(w, "  %-22s %-8s %s\n", id, "["+src+"]", title)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Add your own by dropping a YAML into ~/.uta/personas/ — see `uta persona example`.")
}

// buildToolSpecs parses --tool flag values into ToolSpec structs.
// Forms accepted:
//
//	<known-id>                    -> built-in adapter, default args (any name
//	                                 returned by engine.KnownBuiltinTools, plus
//	                                 the "forge" alias for "forge-test")
//	any-other-string              -> raw shell command, id derived from binary name
//	id=cmd args...                -> raw shell command with explicit id
func buildToolSpecs(values []string, timeout time.Duration) []engine.ToolSpec {
	out := make([]engine.ToolSpec, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		var id, cmd string
		if eq := strings.IndexByte(v, '='); eq > 0 {
			id = strings.TrimSpace(v[:eq])
			cmd = strings.TrimSpace(v[eq+1:])
		} else {
			cmd = v
		}
		spec := engine.ToolSpec{ID: id, Timeout: timeout}
		// If the cmd is exactly one of the built-in adapter IDs (and no
		// explicit id= prefix), let ResolveToolSpec fill in defaults.
		if id == "" && isBuiltinToolID(cmd) {
			spec.ID = strings.ToLower(cmd)
			// Cmd, Args, Adapter filled by ResolveToolSpec
		} else {
			// Run as a shell command. Default to the "findings" adapter so
			// any tool whose stdout contains {"findings":[...]} (the same
			// shape critics produce) gets its findings parsed automatically.
			// Pure-output tools just silently report zero findings.
			spec.Cmd = cmd
			spec.ShellMode = true
			if spec.ID == "" {
				if fields := strings.Fields(cmd); len(fields) > 0 {
					spec.ID = fields[0]
				} else {
					spec.ID = "tool"
				}
			}
			spec.Adapter = "findings"
		}
		out = append(out, spec)
	}
	return out
}

// isBuiltinToolID reports whether name matches one of the engine's built-in
// tool adapter IDs (slither, mythril, forge-test, aderyn, ...) or the
// "forge" alias. Compared case-insensitively so user input like "Slither"
// still resolves to the built-in adapter.
func isBuiltinToolID(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	if name == "forge" {
		// alias for forge-test; both are recognized by ResolveToolSpec.
		return true
	}
	for _, known := range engine.KnownBuiltinTools() {
		if name == known {
			return true
		}
	}
	return false
}

// applyVibeToCritics reads <project>/.uta/profiles/<mode>.vibe.md when
// present and appends its contents as a `## Current vibe` section to
// every critic's Prompt. Returns the (possibly-mutated) slice unchanged
// when no vibe file exists, so callers can call this unconditionally.
func applyVibeToCritics(projectRoot, modeName string, in []engine.CriticSpec) ([]engine.CriticSpec, error) {
	if len(in) == 0 || projectRoot == "" || modeName == "" {
		return in, nil
	}
	body, err := profile.ReadVibe(projectRoot, modeName)
	if err != nil {
		return in, err
	}
	section := profile.VibePromptSection(body)
	if section == "" {
		return in, nil
	}
	out := make([]engine.CriticSpec, len(in))
	for i, c := range in {
		prompt := strings.TrimRight(c.Prompt, "\n")
		c.Prompt = prompt + "\n\n" + section
		out[i] = c
	}
	return out, nil
}

func resolveCritics(app *App, ids []string) ([]engine.CriticSpec, error) {
	registry := resolvePersonas(app)
	if len(ids) == 0 {
		// Default selection: trail-of-bits + openzeppelin-style. Keeps the
		// v0.4 default behavior even though we now ship more personas.
		out := []engine.CriticSpec{}
		for _, id := range []string{"trail-of-bits", "openzeppelin-style"} {
			if c, ok := registry[id]; ok {
				out = append(out, c)
			}
		}
		return out, nil
	}
	out := make([]engine.CriticSpec, 0, len(ids))
	var missing []string
	for _, id := range ids {
		if c, ok := registry[id]; ok {
			out = append(out, c)
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) > 0 {
		known := make([]string, 0, len(registry))
		for id := range registry {
			known = append(known, id)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("unknown critic(s): %s. Available: %s", strings.Join(missing, ","), strings.Join(known, ", "))
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

func printFindings(w io.Writer, r *engine.FindingsReport) {
	fmt.Fprintf(w, "iteration %d · %d finding(s)\n\n", r.Iteration, len(r.Findings))
	for _, f := range r.Findings {
		fmt.Fprintf(w, "[%s] %s · %s\n", strings.ToUpper(string(f.Severity)), f.Critic, f.Title)
		if f.File != "" {
			loc := f.File
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Fprintf(w, "  at %s\n", loc)
		}
		if f.Body != "" {
			body := strings.TrimSpace(f.Body)
			if len(body) > 400 {
				body = body[:400] + "…"
			}
			fmt.Fprintf(w, "  %s\n", strings.ReplaceAll(body, "\n", "\n  "))
		}
		fmt.Fprintln(w)
	}
}
