package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/state"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/whiteboard"
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
		Use:   "resume [session-id]",
		Short: "continue a prior run with a follow-up goal",
		Long: `Resume a prior session by id. If no session id is given, picks up the
active thread's last session (see 'uta thread current'). Pass --goal with
the follow-up instructions.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if goal == "" {
				return errors.New("--goal is required")
			}
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			sessionID, threadName, err := resolveResumeSession(args, app.StateDir)
			if err != nil {
				return err
			}
			if threadName != "" {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"[uta] resuming active thread %q session %s\n",
					threadName, sessionID)
			}

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
				Memory:     memory.NewFromEnv(app.Store.DB),
				Whiteboard: whiteboard.New(app.Store.DB),
			}
			sup := engine.New(deps)

			subtaskEnv := app.ProjectSubtaskEnv()
			if app.InProject() && workdir == "" {
				workdir = app.ProjectRoot
			}

			mode, merr := resolveActiveMode(cmd, app)
			if merr != nil {
				return merr
			}
			req := engine.ResumeRequest{
				PriorSessionID:  sessionID,
				Goal:            goal,
				WorkerName:      workerName,
				SubtaskTimeout:  subtaskTimeout,
				RunTimeout:      runTimeout,
				PreApproveTools: preApprove,
				Workdir:         workdir,
				Env:             subtaskEnv,
				HITL: &hitl.Gate{
					StateDir: app.StateDir,
					Stdin:    cmd.InOrStdin(),
					Stderr:   cmd.ErrOrStderr(),
				},
			}
			// Apply the active MissionProfile's policies (env, capability
			// gates, HITL triggers, budget caps, mode name). Identical
			// preamble to `uta run` — without this call, --mode would
			// only set ModeName for trajectory attribution and silently
			// drop everything else.
			engine.ApplyProfileToResume(&req, mode)
			// MCP bridge: probe each MCP server declared on the active
			// mode and write a synthesized config the provider can
			// consume via --mcp-config. Mirrors what `uta run` does so
			// resume turns see the same toolset the original run did.
			if mode != nil && len(mode.MCPServers) > 0 {
				probes := engine.ApplyMCPBridgeToResume(ctx, &req, mode)
				for _, p := range probes {
					if diag := engine.FormatMCPProbeError(p); diag != "" {
						fmt.Fprintln(cmd.ErrOrStderr(), "warn:", diag)
					}
				}
				if req.MCPConfigPath != "" {
					defer os.Remove(req.MCPConfigPath)
				}
			}
			result, err := sup.Resume(ctx, req)
			if result.SessionID != "" {
				if terr := state.TouchActiveSession(app.StateDir, result.SessionID); terr != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "warn: update active thread:", terr)
				}
			}
			bus.Shutdown()
			<-renderDone
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return exitWith(130)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "resume failed: %v\n", err)
				if result.SessionID != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "session: %s\n", result.SessionID)
				}
				return exitWith(4)
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

// resolveResumeSession picks the session id that `uta resume` should
// continue. An explicit positional arg wins; otherwise the active
// thread's last session is used. Returns the session id, the active
// thread name (empty when resolved from an explicit arg), or an error
// when neither path yielded a session.
func resolveResumeSession(args []string, stateDir string) (string, string, error) {
	if len(args) == 1 {
		id := strings.TrimSpace(args[0])
		if id == "" {
			return "", "", errors.New("session id argument is empty")
		}
		return id, "", nil
	}
	idx, err := state.Load(stateDir)
	if err != nil {
		return "", "", fmt.Errorf("load thread state: %w", err)
	}
	t := idx.Active()
	if t == nil || t.ActiveSessionID == "" {
		return "", "", errors.New("no session id given and no active thread with a last session (see 'uta thread current')")
	}
	return t.ActiveSessionID, t.Name, nil
}
