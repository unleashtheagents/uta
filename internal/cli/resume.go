package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

func newResumeCmd() *cobra.Command {
	var (
		goal           string
		workerName     string
		subtaskTimeout time.Duration
		runTimeout     time.Duration
		preApprove     []string
		workdir        string
	)
	cmd := &cobra.Command{
		Use:   "resume <session-id>",
		Short: "continue a prior run with a follow-up goal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if goal == "" {
				return errors.New("--goal is required")
			}
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			bus := trajectory.NewBus()
			defer bus.Shutdown()

			noColor, _ := cmd.Flags().GetBool("no-color")
			rdr := NewRenderer(cmd.ErrOrStderr(), noColor)
			rdr.ShowGoal(goal, workerName)
			renderDone := rdr.Subscribe(bus)

			recorder := trajectory.NewRecorder(app.Store)
			deps := engine.Deps{
				Store: app.Store, Blobs: app.Blobs, Recorder: recorder,
				Bus: bus, Registry: app.Registry,
			}
			sup := engine.New(deps)

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

			result, err := sup.Resume(ctx, engine.ResumeRequest{
				PriorSessionID:  args[0],
				Goal:            goal,
				WorkerName:      workerName,
				SubtaskTimeout:  subtaskTimeout,
				RunTimeout:      runTimeout,
				PreApproveTools: preApprove,
				Workdir:         workdir,
				Env:             subtaskEnv,
			})
			bus.Shutdown()
			<-renderDone
			if err != nil {
				if errors.Is(err, context.Canceled) {
					os.Exit(130)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "resume failed: %v\n", err)
				if result.SessionID != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "session: %s\n", result.SessionID)
				}
				os.Exit(4)
			}
			if result.FinalAnswer != "" {
				fmt.Fprintln(cmd.OutOrStdout(), result.FinalAnswer)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "\n[uta] session %s status=%s\n", result.SessionID, result.Status)
			return nil
		},
	}
	cmd.Flags().StringVarP(&goal, "goal", "g", "", "the follow-up goal (required)")
	cmd.Flags().StringVar(&workerName, "worker", "", "override worker (defaults to prior session's worker)")
	cmd.Flags().DurationVar(&subtaskTimeout, "subtask-timeout", 10*time.Minute, "per-turn timeout")
	cmd.Flags().DurationVar(&runTimeout, "timeout", 30*time.Minute, "overall timeout")
	cmd.Flags().StringSliceVar(&preApprove, "pre-approve", nil, "tools to pre-approve")
	cmd.Flags().StringVar(&workdir, "workdir", "", "worker working directory")
	return cmd
}
