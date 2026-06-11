package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/improve"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/state"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

// handoffDepthLimit picks the effective depth cap for a handoff chain.
// A profile may override the default via MissionProfile.MaxHandoffDepth;
// zero (the field's zero value) means "use the orchestrator default"
// from profile.DefaultMaxHandoffDepth. The profile loader enforces the
// upper bound (profile.MaxHandoffDepthHardLimit) at validation time, so
// nothing here needs to clamp.
func handoffDepthLimit(req engine.RunRequest) int {
	if req.MaxHandoffDepth > 0 {
		return req.MaxHandoffDepth
	}
	return profile.DefaultMaxHandoffDepth
}

func newRunCmd() *cobra.Command {
	var (
		goal           string
		workerName     string
		plannerName    string
		synthName      string
		maxParallel    int
		maxSubtasks    int
		subtaskTimeout time.Duration
		runTimeout     time.Duration
		failFast       bool
		yes            bool
		printAnswer    bool
		printJSONL     bool
		preApprove     []string
		workdir        string
		workflowFile   string
		strategyFlag   string
		maxWallSeconds int
		resumeSession  string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "decompose a goal, fan it out to agent providers, and synthesize the result",
		RunE: func(cmd *cobra.Command, args []string) error {
			var preSet []engine.SubtaskSpec
			var skipSynth bool
			var workflowGoal string
			var workflowEnv []string

			if workflowFile != "" {
				wf, err := config.LoadWorkflow(workflowFile)
				if err != nil {
					return err
				}
				workflowGoal = wf.Goal
				if workerName == "" {
					workerName = wf.Defaults.Worker
				}
				if plannerName == "" {
					plannerName = wf.Defaults.Planner
				}
				if synthName == "" {
					synthName = wf.Synthesis.Worker
				}
				if maxParallel == 0 || maxParallel == 4 { // 4 is the flag default; let YAML win if it sets something
					if wf.Defaults.MaxParallel > 0 {
						maxParallel = wf.Defaults.MaxParallel
					}
				}
				if maxSubtasks == 0 || maxSubtasks == 6 {
					if wf.Defaults.MaxSubtasks > 0 {
						maxSubtasks = wf.Defaults.MaxSubtasks
					}
				}
				if wf.Defaults.SubtaskTimeout > 0 {
					subtaskTimeout = wf.Defaults.SubtaskTimeout
				}
				if wf.Defaults.Timeout > 0 {
					runTimeout = wf.Defaults.Timeout
				}
				if workdir == "" {
					workdir = wf.Defaults.Workdir
				}
				if len(preApprove) == 0 {
					preApprove = wf.Defaults.PreApprove
				}
				if strings.EqualFold(wf.Synthesis.Mode, "skip") {
					skipSynth = true
				}
				if strategyFlag == "" {
					strategyFlag = wf.InferStrategy()
				}
				if wf.Budget.MaxWallSeconds > 0 {
					maxWallSeconds = wf.Budget.MaxWallSeconds
				}
				if len(wf.Env) > 0 {
					keys := make([]string, 0, len(wf.Env))
					for k := range wf.Env {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					for _, k := range keys {
						workflowEnv = append(workflowEnv, k+"="+wf.Env[k])
					}
				}
				for i, st := range wf.Subtasks {
					id := st.ID
					if id == "" {
						id = fmt.Sprintf("s%d", i+1)
					}
					title := st.Title
					if title == "" {
						title = id
					}
					spec := engine.SubtaskSpec{
						ID: id, Title: title, Prompt: st.Prompt, Worker: st.Worker, Needs: st.Needs,
						Timeout: st.Timeout,
					}
					if st.Gate != nil {
						spec.Gate = &engine.Gate{
							Cmd:           st.Gate.Cmd,
							Timeout:       st.Gate.Timeout,
							RetryProducer: st.Gate.RetryProducer,
							MaxRetries:    st.Gate.MaxRetries,
						}
					}
					preSet = append(preSet, spec)
				}
			}

			if strings.TrimSpace(goal) == "" && len(args) > 0 {
				goal = strings.Join(args, " ")
			}
			if strings.TrimSpace(goal) == "" {
				goal = workflowGoal
			}
			if strings.TrimSpace(goal) == "" && resumeSession == "" {
				return errors.New("--goal/-g is required (or pass via -f workflow.yaml, as positional args, or use --resume-session)")
			}
			if resumeSession != "" && (goal != "" || workflowFile != "") {
				return errors.New("--resume-session recovers the prior session's goal and plan; don't combine it with --goal or -f")
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			// Crash recovery: rebuild the request from the unfinished
			// session — completed subtasks become prior outcomes (not
			// re-run), the remainder re-executes. The recovered goal /
			// worker / mode flow into the normal path below so profile
			// preamble, MCP bridge, and rendering behave identically to
			// a fresh run.
			var priorOutcomes []engine.SubtaskOutcome
			if resumeSession != "" {
				recovered, info, rerr := engine.BuildResumeRunRequest(app.Store, app.Blobs, resumeSession)
				if rerr != nil {
					return rerr
				}
				goal = recovered.Goal
				if workerName == "" {
					workerName = recovered.WorkerName
				}
				if plannerName == "" {
					plannerName = recovered.PlannerName
				}
				preSet = recovered.PreSetSubtasks
				priorOutcomes = recovered.PriorOutcomes
				fmt.Fprintf(cmd.ErrOrStderr(),
					"[uta] resuming run %s (%s): %d completed subtask(s) recovered, %d to re-run\n",
					shortID(info.PriorSessionID), info.PriorStatus, info.Completed, info.Rerun)
			}

			detections := app.Registry.DetectAll(cmd.Context())
			available := availableProviders(app.Registry.Names(), detections)
			if len(available) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no provider is installed on PATH. Try installing 'claude' or 'gemini' first.")
				return exitWith(3)
			}

			if workerName == "" {
				workerName, err = chooseWorker(cmd.InOrStdin(), cmd.OutOrStdout(), available, yes)
				if err != nil {
					return err
				}
			}
			if !containsString(available, workerName) {
				det := detections[workerName]
				return fmt.Errorf("provider %q is not available: %s", workerName, firstNonEmptyStr(det.Notes, "not detected"))
			}

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			bus := trajectory.NewBus()
			defer bus.Shutdown()

			noColor, _ := cmd.Flags().GetBool("no-color")

			// Optional live JSONL pipe.
			var liveDone chan struct{}
			if printJSONL {
				liveDone = make(chan struct{})
				ch := bus.Subscribe(256)
				go streamJSONL(cmd.OutOrStdout(), ch, liveDone)
			}

			// Friendly progress to stderr unless we're streaming JSONL or -y CI mode chose silence.
			var renderDone <-chan struct{}
			if !printJSONL {
				rdr := NewRenderer(cmd.ErrOrStderr(), noColor)
				rdr.ShowGoal(goal, workerName)
				renderDone = rdr.Subscribe(bus)
			}

			recorder := trajectory.NewRecorder(app.Store)
			deps := engine.Deps{
				Store:      app.Store,
				Blobs:      app.Blobs,
				Recorder:   recorder,
				Bus:        bus,
				Registry:   app.Registry,
				Memory:     memory.NewFromEnv(app.Store.DB),
				Whiteboard: whiteboard.New(app.Store.DB),
			}
			sup := engine.New(deps)

			// Project context: subtasks see the project's root + shared context
			// dir as env vars, and the project root becomes the default workdir.
			// Workflow-declared env comes first so the project-context vars,
			// appended last, win on duplicate keys (later entries override
			// earlier ones in os/exec's env handling).
			subtaskEnv := append(workflowEnv, app.ProjectSubtaskEnv()...)
			if app.InProject() && workdir == "" {
				workdir = app.ProjectRoot
			}

			req := engine.RunRequest{
				Goal:               goal,
				WorkerName:         workerName,
				PlannerName:        plannerName,
				SynthName:          synthName,
				MaxParallel:        maxParallel,
				MaxSubtasks:        maxSubtasks,
				SubtaskTimeout:     subtaskTimeout,
				RunTimeout:         runTimeout,
				FailFast:           failFast,
				PreApproveTools:    preApprove,
				Workdir:            workdir,
				WorkflowPath:       workflowFile,
				PreSetSubtasks:     preSet,
				PriorOutcomes:      priorOutcomes,
				ResumedFromSession: resumeSession,
				SkipSynthesis:      skipSynth,
				Strategy:           strategyFlag,
				Env:                subtaskEnv,
				MaxWallSeconds:     maxWallSeconds,
				HITL: &hitl.Gate{
					StateDir: app.StateDir,
					Stdin:    cmd.InOrStdin(),
					Stderr:   cmd.ErrOrStderr(),
				},
			}

			mode, err := resolveActiveMode(cmd, app)
			if err != nil {
				return err
			}
			engine.ApplyProfile(&req, mode)
			if mode != nil && len(mode.MCPServers) > 0 {
				probes := engine.ApplyMCPBridge(ctx, &req, mode)
				for _, p := range probes {
					if diag := engine.FormatMCPProbeError(p); diag != "" {
						fmt.Fprintln(cmd.ErrOrStderr(), "warn:", diag)
					}
				}
				if req.MCPConfigPath != "" {
					defer os.Remove(req.MCPConfigPath)
				}
			}

			result, runErr := sup.Run(ctx, req)

			// Record this session against the active thread (if any) so
			// `uta resume` with no args can pick up where the user left
			// off. Best-effort — a state.json failure should not derail a
			// successful run.
			if result.SessionID != "" {
				if terr := state.TouchActiveSession(app.StateDir, result.SessionID); terr != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "warn: update active thread:", terr)
				}
			}

			// Handoff chain: when the active profile declares OnComplete
			// and the run finished cleanly, evaluate handoffs and chain a
			// follow-up Run for the first matching target. The chain
			// reuses the same bus + recorder so live subscribers see
			// every event from every link in chronological order.
			if runErr == nil && mode != nil && len(mode.OnComplete) > 0 {
				result = runHandoffChain(ctx, cmd, app, sup, mode, req, result)
			}

			// Per-mode retrospective: every Nth completed session in a
			// mode triggers a synthesis pass that writes a markdown under
			// <project>/.uta/context/retrospectives/. No-op when the
			// profile has retrospective_every: 0, or when not inside a
			// project.
			if runErr == nil && mode != nil && mode.RetrospectiveEvery > 0 && app.InProject() {
				maybeWriteRetrospective(ctx, cmd, app, recorder, bus, mode, req, result)
			}

			bus.Shutdown()
			if liveDone != nil {
				<-liveDone
			}
			if renderDone != nil {
				<-renderDone
			}

			if runErr != nil {
				if errors.Is(runErr, context.Canceled) {
					fmt.Fprintln(cmd.ErrOrStderr(), "cancelled.")
					return exitWith(130)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "run failed: %v\n", runErr)
				if result.SessionID != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "session: %s\n", result.SessionID)
				}
				if result.Status == "failed" {
					return exitWith(4)
				}
				return runErr
			}

			if printAnswer || (!printJSONL) {
				if result.FinalAnswer != "" {
					fmt.Fprintln(cmd.OutOrStdout(), result.FinalAnswer)
				}
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "\n[uta] session %s status=%s subtasks=%d\n",
				result.SessionID, result.Status, len(result.Subtasks))

			if result.Status == "partial" {
				return exitWith(5)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&goal, "goal", "g", "", "the goal to orchestrate")
	cmd.Flags().StringVar(&workerName, "worker", "", "default worker provider (claude|gemini|...)")
	cmd.Flags().StringVar(&plannerName, "planner", "", "planner provider (defaults to worker)")
	cmd.Flags().StringVar(&synthName, "synth", "", "synthesizer provider (defaults to worker)")
	cmd.Flags().IntVar(&maxParallel, "max-parallel", 4, "maximum concurrent subtasks")
	cmd.Flags().IntVar(&maxSubtasks, "max-subtasks", 6, "hard cap on subtasks the planner may propose")
	cmd.Flags().DurationVar(&subtaskTimeout, "subtask-timeout", 10*time.Minute, "per-subtask timeout")
	cmd.Flags().DurationVar(&runTimeout, "timeout", 30*time.Minute, "overall run timeout")
	cmd.Flags().BoolVar(&failFast, "fail-fast", false, "abort the run on the first subtask failure")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip interactive prompts; require --worker")
	cmd.Flags().BoolVar(&printAnswer, "print", false, "print the final answer to stdout (default if no other --print* is set)")
	cmd.Flags().BoolVar(&printJSONL, "print-jsonl", false, "stream every trajectory event to stdout as JSONL")
	cmd.Flags().StringSliceVar(&preApprove, "pre-approve", nil, "comma-separated tools the worker may use without prompting (provider-specific)")
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory exposed to the worker (defaults to CWD)")
	cmd.Flags().StringVarP(&workflowFile, "file", "f", "", "load a uta.yaml workflow file (flags can still override its fields)")
	cmd.Flags().StringVar(&strategyFlag, "strategy", "", "orchestration strategy: fanout (parallel, default) or dag (with needs+gates). Auto-detected from workflow YAML if unset.")
	cmd.Flags().StringVar(&resumeSession, "resume-session", "", "re-enter a crashed/cancelled session: completed subtasks are recovered (not re-paid), the remainder re-runs, synthesis covers the whole plan")

	return cmd
}

func availableProviders(names []string, detections map[string]provider.Detection) []string {
	out := []string{}
	for _, n := range names {
		if detections[n].Available {
			out = append(out, n)
		}
	}
	return out
}

func chooseWorker(in io.Reader, out io.Writer, available []string, yes bool) (string, error) {
	if yes {
		return "", errors.New("--worker is required when -y/--yes is set")
	}
	f, ok := in.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		return "", errors.New("--worker is required when stdin is not a TTY")
	}
	fmt.Fprintln(out, "Available providers:")
	for i, n := range available {
		fmt.Fprintf(out, "  [%d] %s\n", i+1, n)
	}
	fmt.Fprintf(out, "Pick one (1-%d, or name): ", len(available))
	br := bufio.NewReader(in)
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("no selection made")
	}
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(available) {
		return available[n-1], nil
	}
	for _, n := range available {
		if n == line {
			return n, nil
		}
	}
	return "", fmt.Errorf("unrecognized choice: %s", line)
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// signalContext returns a context that is cancelled on SIGINT/SIGTERM in
// addition to its parent.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-ctx.Done():
		case <-ch:
			cancel()
		}
		signal.Stop(ch)
	}()
	return ctx, cancel
}

func streamJSONL(w io.Writer, ch <-chan trajectory.Event, done chan struct{}) {
	defer close(done)
	enc := json.NewEncoder(w)
	for ev := range ch {
		if err := enc.Encode(ev); err != nil {
			// Sink closed (e.g. broken pipe). Stop encoding but keep
			// draining so the bus producer doesn't block.
			for range ch {
			}
			return
		}
	}
}

// handoffChainDeps groups the side-effects the chain loop performs so
// callers (tests in particular) can swap them out. The cli wires these
// to the real supervisor + git + filesystem; unit tests inject fakes
// that drive the chain without spinning up real providers.
type handoffChainDeps struct {
	// run executes one chained Run. In production this is sup.Run; in
	// tests it returns canned RunResult/error pairs.
	run func(ctx context.Context, req engine.RunRequest) (engine.RunResult, error)
	// changedFiles reports the files modified by the prior run (for
	// Handoff condition evaluation). Production: engine.GitChangedFiles.
	changedFiles func(ctx context.Context, workdir string) ([]string, error)
	// resolve looks up a profile by name. Production: profile.Find over
	// the loaded set.
	resolve engine.ProfileResolver
	// onSession is called for every chained session id, so callers can
	// e.g. record it against the active thread. Optional.
	onSession func(sessionID string)
	// emit groups the trajectory hooks. Production: *engine.Supervisor.
	emit handoffEmitter
	// out receives operator-visible chain messages ("handoff: dev ->
	// audit (depth 1, ...)"). Production: cmd.ErrOrStderr().
	out io.Writer
}

// handoffEmitter is the subset of *engine.Supervisor that the chain
// loop uses for trajectory events. Defined as an interface so tests
// can verify the exact emit sequence without poking at the bus.
type handoffEmitter interface {
	EvaluateHandoffs(priorSessionID string, handoffs []profile.Handoff, changedFiles []string, resolve engine.ProfileResolver) (*engine.HandoffMatch, error)
	EmitHandoffStarted(priorSessionID string, m *engine.HandoffMatch)
	EmitHandoffCompleted(priorSessionID, chainedSessionID, status string)
	EmitHandoffCancelled(priorSessionID, targetMode, reason string)
}

// runHandoffChain is the cli-facing wrapper that loads profiles, builds
// the production handoffChainDeps, and delegates to executeHandoffChain.
func runHandoffChain(
	ctx context.Context,
	cmd *cobra.Command,
	app *App,
	sup *engine.Supervisor,
	mode *profile.MissionProfile,
	baseReq engine.RunRequest,
	priorResult engine.RunResult,
) engine.RunResult {
	if priorResult.Status != "completed" {
		return priorResult
	}

	profiles, perrs := profile.LoadAll(app.GlobalHome, app.ProjectRoot)
	for _, e := range perrs {
		fmt.Fprintln(cmd.ErrOrStderr(), "warn: profile:", e)
	}
	deps := handoffChainDeps{
		run:          sup.Run,
		changedFiles: engine.GitChangedFiles,
		resolve: func(name string) (*profile.MissionProfile, error) {
			return profile.Find(profiles, name)
		},
		onSession: func(sessionID string) {
			if terr := state.TouchActiveSession(app.StateDir, sessionID); terr != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "warn: update active thread:", terr)
			}
		},
		emit: sup,
		out:  cmd.ErrOrStderr(),
	}
	return executeHandoffChain(ctx, deps, mode, baseReq, priorResult)
}

// executeHandoffChain is the testable core of the handoff walk. It
// dispatches chained mode invocations for each matching Handoff,
// reusing the same trajectory bus from the initial invocation — the
// only thing that changes between hops is the active MissionProfile
// (and therefore the env, allow-list, MCP servers, and the goal text
// the chained worker receives).
//
// Returns the result of the last chained run. On cancellation (Ctrl-C)
// emits HandoffCancelled on the prior session and returns the
// most-recent result. On a failed chained run emits HandoffCompleted
// with status "failed" and stops.
func executeHandoffChain(
	ctx context.Context,
	deps handoffChainDeps,
	mode *profile.MissionProfile,
	baseReq engine.RunRequest,
	priorResult engine.RunResult,
) engine.RunResult {
	depthLimit := handoffDepthLimit(baseReq)
	current := mode
	currentResult := priorResult
	for depth := 0; depth < depthLimit; depth++ {
		if current == nil || len(current.OnComplete) == 0 {
			return currentResult
		}
		if ctx.Err() != nil {
			return currentResult
		}

		changed, _ := deps.changedFiles(ctx, baseReq.Workdir)
		match, _ := deps.emit.EvaluateHandoffs(currentResult.SessionID, current.OnComplete, changed, deps.resolve)
		if match == nil {
			return currentResult
		}

		deps.emit.EmitHandoffStarted(currentResult.SessionID, match)
		fmt.Fprintf(deps.out,
			"\n[uta] handoff: %s -> %s (depth %d, %d files changed)\n",
			current.Name, match.Handoff.TargetMode, depth+1, len(match.ChangedFiles))

		chainedReq := engine.RunRequest{
			Goal:                match.Prompt,
			WorkerName:          baseReq.WorkerName,
			PlannerName:         baseReq.PlannerName,
			SynthName:           baseReq.SynthName,
			MaxParallel:         baseReq.MaxParallel,
			MaxSubtasks:         baseReq.MaxSubtasks,
			SubtaskTimeout:      baseReq.SubtaskTimeout,
			RunTimeout:          baseReq.RunTimeout,
			FailFast:            baseReq.FailFast,
			PreApproveTools:     baseReq.PreApproveTools,
			Workdir:             baseReq.Workdir,
			Env:                 baseReq.Env,
			MaxWallSeconds:      baseReq.MaxWallSeconds,
			TransportMaxRetries: baseReq.TransportMaxRetries,
			HandoffFrom:         currentResult.SessionID,
			HandoffTargetMode:   match.Handoff.TargetMode,
		}
		// ApplyProfile, called below, replaces the budget fields with the
		// chained mode's own policies — so we do not copy baseReq's caps
		// here. A profile without an explicit budget resets to "uncapped",
		// which matches what the chained mode's author asked for.
		engine.ApplyProfile(&chainedReq, match.TargetMode)
		if len(match.TargetMode.MCPServers) > 0 {
			probes := engine.ApplyMCPBridge(ctx, &chainedReq, match.TargetMode)
			for _, p := range probes {
				if diag := engine.FormatMCPProbeError(p); diag != "" {
					fmt.Fprintln(deps.out, "warn:", diag)
				}
			}
			if chainedReq.MCPConfigPath != "" {
				defer os.Remove(chainedReq.MCPConfigPath)
			}
		}

		chainedResult, chainedErr := deps.run(ctx, chainedReq)
		if chainedResult.SessionID != "" && deps.onSession != nil {
			deps.onSession(chainedResult.SessionID)
		}

		if chainedErr != nil {
			if errors.Is(chainedErr, context.Canceled) {
				deps.emit.EmitHandoffCancelled(currentResult.SessionID, match.Handoff.TargetMode, "user cancelled")
				return currentResult
			}
			deps.emit.EmitHandoffCompleted(currentResult.SessionID, chainedResult.SessionID, "failed")
			return chainedResult
		}

		deps.emit.EmitHandoffCompleted(currentResult.SessionID, chainedResult.SessionID, chainedResult.Status)
		current = match.TargetMode
		currentResult = chainedResult
		if currentResult.Status != "completed" {
			return currentResult
		}
	}
	fmt.Fprintf(deps.out, "[uta] handoff chain reached max depth %d; stopping.\n", depthLimit)
	return currentResult
}

// maybeWriteRetrospective is the cli-side wrapper around
// improve.MaybeRetrospective. Failures are logged as warnings (so a
// transient provider hiccup does not surface as a run failure) but a
// successful write is announced on stderr so the operator can spot the
// new artifact.
func maybeWriteRetrospective(
	ctx context.Context,
	cmd *cobra.Command,
	app *App,
	recorder *trajectory.Recorder,
	bus *trajectory.Bus,
	mode *profile.MissionProfile,
	req engine.RunRequest,
	result engine.RunResult,
) {
	res, err := improve.MaybeRetrospective(ctx, improve.RetroDeps{
		Store:    app.Store,
		Registry: app.Registry,
		Recorder: recorder,
		Bus:      bus,
	}, improve.RetroRequest{
		ModeName:       mode.Name,
		ProjectRoot:    app.ProjectRoot,
		WorkerName:     req.WorkerName,
		Every:          mode.RetrospectiveEvery,
		PromptTemplate: mode.RetrospectivePrompt,
		Workdir:        req.Workdir,
		Env:            req.Env,
		Timeout:        req.SubtaskTimeout,
		PriorSessionID: result.SessionID,
	})
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "warn: retrospective:", err)
		return
	}
	if res != nil && res.Triggered && res.Path != "" {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"[uta] retrospective for %q written to %s (after %d sessions)\n",
			mode.Name, res.Path, res.SessionCount)
	}
}
