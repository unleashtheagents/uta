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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

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
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "decompose a goal, fan it out to agent providers, and synthesize the result",
		RunE: func(cmd *cobra.Command, args []string) error {
			var preSet []engine.SubtaskSpec
			var skipSynth bool
			var workflowGoal string

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
			if strings.TrimSpace(goal) == "" {
				return errors.New("--goal/-g is required (or pass via -f workflow.yaml, or as positional args)")
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			detections := app.Registry.DetectAll(cmd.Context())
			available := availableProviders(app.Registry.Names(), detections)
			if len(available) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no provider is installed on PATH. Try installing 'claude' or 'gemini' first.")
				os.Exit(3)
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
				Store:    app.Store,
				Blobs:    app.Blobs,
				Recorder: recorder,
				Bus:      bus,
				Registry: app.Registry,
			}
			sup := engine.New(deps)

			// Project context: subtasks see the project's root + shared context
			// dir as env vars, and the project root becomes the default workdir.
			var subtaskEnv []string
			if app.InProject() {
				subtaskEnv = []string{
					"UTA_PROJECT_ROOT=" + app.ProjectRoot,
					"UTA_CONTEXT_DIR=" + app.ContextDir,
					"UTA_PROJECT_NAME=" + app.ProjectName,
				}
				if workdir == "" {
					workdir = app.ProjectRoot
				}
			}

			result, runErr := sup.Run(ctx, engine.RunRequest{
				Goal:            goal,
				WorkerName:      workerName,
				PlannerName:     plannerName,
				SynthName:       synthName,
				MaxParallel:     maxParallel,
				MaxSubtasks:     maxSubtasks,
				SubtaskTimeout:  subtaskTimeout,
				RunTimeout:      runTimeout,
				FailFast:        failFast,
				PreApproveTools: preApprove,
				Workdir:         workdir,
				WorkflowPath:    workflowFile,
				PreSetSubtasks:  preSet,
				SkipSynthesis:   skipSynth,
				Strategy:        strategyFlag,
				Env:             subtaskEnv,
			})

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
					os.Exit(130)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "run failed: %v\n", runErr)
				if result.SessionID != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "session: %s\n", result.SessionID)
				}
				if result.Status == "failed" {
					os.Exit(4)
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
				os.Exit(5)
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
		_ = enc.Encode(ev)
	}
}
