package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/sentinel"
	"github.com/unleashtheagents/uta/internal/state"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

func newPRCmd() *cobra.Command {
	var (
		goal           string
		workerName     string
		branchName     string
		prTitle        string
		verifyCmd      string
		verifyRetries  int
		draft          bool
		dryRun         bool
		preApprove     []string
		workdir        string
		subtaskTimeout time.Duration
		runTimeout     time.Duration
	)

	cmd := &cobra.Command{
		Use:   "pr",
		Short: "run an autonomous dev mission on a feature branch and create a Pull Request",
		Long: `Creates a git branch, orchestrates the goal with verification gates,
commits the changes, and submits a Pull Request via gh pr create with a
structured PR description generated from the trajectory.

Examples:
  # Autonomous feature branch + PR creation
  uta pr -g "add rate limiting middleware" --verify 'go test ./...'

  # Custom branch and draft PR
  uta pr -g "refactor error handling" --branch feat/errors --draft --verify 'go test ./...'

  # Dry run: run the mission and branch, but print the proposed PR instead of calling gh
  uta pr -g "add unit tests for internal/paths" --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(goal) == "" && len(args) > 0 {
				goal = strings.Join(args, " ")
			}
			if strings.TrimSpace(goal) == "" {
				return errors.New("--goal/-g is required (or pass as positional arguments)")
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			if workdir == "" {
				if app.InProject() {
					workdir = app.ProjectRoot
				} else {
					workdir, _ = os.Getwd()
				}
			}

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			// Verify git repository exists in workdir
			if _, err := execGit(ctx, workdir, "rev-parse", "--git-dir"); err != nil {
				return fmt.Errorf("workdir %q is not a git repository: %w", workdir, err)
			}

			// Auto-generate branch name if empty
			if branchName == "" {
				slug := slugifyGoal(goal)
				if slug == "" {
					slug = "feature"
				}
				branchName = "uta/" + slug
			}
			if strings.HasPrefix(branchName, "-") {
				return fmt.Errorf("invalid branch name %q: must not start with '-'", branchName)
			}

			// Auto-generate PR title if empty
			if prTitle == "" {
				prTitle = "uta: " + goal
			}

			// Create and checkout branch
			fmt.Fprintf(cmd.ErrOrStderr(), "[uta pr] switching to branch %q...\n", branchName)
			if _, err := execGit(ctx, workdir, "checkout", "-B", branchName); err != nil {
				return fmt.Errorf("git checkout -B %s: %w", branchName, err)
			}

			// Set up provider
			if workerName == "" {
				detections := app.Registry.DetectAll(ctx)
				avail := availableProviders(app.Registry.Names(), detections)
				if len(avail) == 0 {
					return errNoProviders
				}
				workerName = avail[0]
			}

			bus := trajectory.NewBus()
			defer bus.Shutdown()

			noColor, _ := cmd.Flags().GetBool("no-color")
			rdr := NewRenderer(cmd.ErrOrStderr(), noColor)
			rdr.ShowGoal("PR mission: "+goal, workerName)
			renderDone := rdr.Subscribe(bus)

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

			subtaskEnv := app.ProjectSubtaskEnv()

			req := engine.RunRequest{
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

			mode, merr := resolveActiveMode(cmd, app)
			if merr != nil {
				return merr
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

			sent := sentinel.NewSentinel(sentinel.Config{
				BudgetWallClock: runTimeout,
				PublishOnBus:    bus,
				Recorder:        recorder,
				Cancel:          cancel,
			})
			go sent.Watch(ctx, bus)

			result, runErr := sup.Run(ctx, req)
			if result.SessionID != "" {
				_ = state.TouchActiveSession(app.StateDir, result.SessionID)
			}

			bus.Shutdown()
			<-renderDone

			if runErr != nil {
				return fmt.Errorf("PR mission failed: %w", runErr)
			}

			// Run verify repair loop if --verify is specified
			if strings.TrimSpace(verifyCmd) != "" {
				ok := runVerifyRepair(ctx, verifyConfig{
					cmd:           verifyCmd,
					maxRepairs:    verifyRetries,
					workdir:       workdir,
					worker:        workerName,
					env:           req.Env,
					timeout:       subtaskTimeout,
					noColor:       noColor,
					app:           app,
					stdin:         cmd.InOrStdin(),
					out:           cmd.OutOrStdout(),
					errw:          cmd.ErrOrStderr(),
					preApprove:    req.PreApproveTools,
					allowedTools:  req.AllowedTools,
					deniedTools:   req.DeniedTools,
					mcpConfigPath: req.MCPConfigPath,
					modeName:      req.ModeName,
				})
				if !ok {
					return exitWith(6)
				}
			}

			// Commit uncommitted changes
			statusOut, _ := execGit(ctx, workdir, "status", "--porcelain")
			if strings.TrimSpace(statusOut) != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "[uta pr] committing workspace changes...")
				if _, err := execGit(ctx, workdir, "add", "-A"); err != nil {
					return fmt.Errorf("git add: %w", err)
				}
				commitMsg := fmt.Sprintf("uta: %s\n\nOrchestrated session: %s", goal, result.SessionID)
				if _, err := execGit(ctx, workdir, "commit", "-m", commitMsg); err != nil {
					return fmt.Errorf("git commit: %w", err)
				}
			}

			// Build PR Body
			prBody := buildPRBody(goal, result, verifyCmd)

			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "\n[uta pr] Dry-run completed. Proposed PR:")
				fmt.Fprintf(cmd.OutOrStdout(), "Branch: %s\nTitle:  %s\nDraft:  %t\n\nBody:\n%s\n", branchName, prTitle, draft, prBody)
				return nil
			}

			// Push branch & create PR via gh CLI
			fmt.Fprintf(cmd.ErrOrStderr(), "[uta pr] pushing branch %s to remote...\n", branchName)
			if out, err := execGit(ctx, workdir, "push", "-u", "origin", branchName); err != nil {
				return fmt.Errorf("git push origin %s failed: %w (%s)", branchName, err, out)
			}

			fmt.Fprintln(cmd.ErrOrStderr(), "[uta pr] creating Pull Request via gh...")
			ghArgs := []string{"pr", "create", "--title", prTitle, "--body", prBody, "--head", branchName}
			if draft {
				ghArgs = append(ghArgs, "--draft")
			}

			ghCmd := exec.CommandContext(ctx, "gh", ghArgs...)
			ghCmd.Dir = workdir
			ghCmd.Stdout = cmd.OutOrStdout()
			ghCmd.Stderr = cmd.ErrOrStderr()
			if err := ghCmd.Run(); err != nil {
				return fmt.Errorf("gh pr create failed: %w (is GitHub CLI installed and authenticated?)", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&goal, "goal", "g", "", "the goal for the PR mission")
	cmd.Flags().StringVar(&workerName, "worker", "", "worker provider (claude|gemini|...)")
	cmd.Flags().StringVar(&branchName, "branch", "", "git feature branch name (defaults to uta/<slug>)")
	cmd.Flags().StringVar(&prTitle, "title", "", "Pull Request title (defaults to 'uta: <goal>')")
	cmd.Flags().StringVar(&verifyCmd, "verify", "", "verification test/build command run before PR creation")
	cmd.Flags().IntVar(&verifyRetries, "verify-retries", 2, "max repair attempts when --verify keeps failing")
	cmd.Flags().BoolVar(&draft, "draft", false, "create Pull Request as draft")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "run mission and commit, but print proposed PR instead of calling gh")
	cmd.Flags().StringSliceVar(&preApprove, "pre-approve", nil, "tools allowed without prompting")
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory (defaults to project root)")
	cmd.Flags().DurationVar(&subtaskTimeout, "subtask-timeout", 10*time.Minute, "per-subtask timeout")
	cmd.Flags().DurationVar(&runTimeout, "timeout", 30*time.Minute, "overall run timeout")

	return cmd
}

func execGit(ctx context.Context, dir string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = dir
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	err := c.Run()
	return strings.TrimSpace(out.String()), err
}

func slugifyGoal(g string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9]+`)
	slug := strings.ToLower(re.ReplaceAllString(g, "-"))
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = slug[:40]
		slug = strings.TrimRight(slug, "-")
	}
	return slug
}

func buildPRBody(goal string, res engine.RunResult, verifyCmd string) string {
	var sb strings.Builder
	sb.WriteString("## Objective\n\n")
	sb.WriteString(strings.TrimSpace(goal))
	sb.WriteString("\n\n## Subtasks Executed\n\n")

	if len(res.Subtasks) == 0 {
		sb.WriteString("- Direct goal execution\n")
	} else {
		for _, st := range res.Subtasks {
			statusMark := "✓"
			if st.Status == "failed" {
				statusMark = "✗"
			} else if st.Status == "skipped" {
				statusMark = "-"
			}
			sb.WriteString(fmt.Sprintf("- [%s] **%s** (%s)\n", statusMark, st.Title, st.Worker))
		}
	}

	if verifyCmd != "" {
		sb.WriteString("\n## Verification\n\nPassed verification gate: `" + verifyCmd + "`\n")
	}

	sb.WriteString(fmt.Sprintf("\n---\n*Session ID: `%s` · Orchestrated autonomously by [uta](https://github.com/unleashtheagents/uta)*\n", res.SessionID))
	return sb.String()
}
