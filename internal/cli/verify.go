package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"time"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

// verifyMaxTail bounds how much of a failing command's output is fed back
// to the worker — enough to locate the failure, small enough to leave room
// for a weak local model's context window.
const verifyMaxTail = 6000

// verifyRunner is the pure decision core of `--verify`: run a check, and on
// failure ask a repair turn to fix it, up to maxRepairs times. The two side
// effects (running the command, running a repair turn) are function fields
// so tests drive the loop without a shell or a provider.
type verifyRunner struct {
	cmdLabel   string
	maxRepairs int
	// verify runs the check once, returning its exit code and an output
	// tail suitable for feeding back to the worker.
	verify func(ctx context.Context) (exitCode int, outputTail string)
	// repair runs one worker turn to fix the failure. Returns an error only
	// on an infrastructure failure (the turn couldn't run at all).
	repair func(ctx context.Context, failure string) error
	out    io.Writer // operator-facing progress
}

// run drives verify → (repair → verify)* and reports whether the check
// ultimately passes. Batch-friendly: with latency free, more repair
// attempts is pure upside for a local model.
func (v verifyRunner) run(ctx context.Context) bool {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			fmt.Fprintln(v.out, "verify: cancelled.")
			return false
		}
		code, tail := v.verify(ctx)
		if code == 0 {
			if attempt == 0 {
				fmt.Fprintf(v.out, "[uta] verify passed: %s\n", v.cmdLabel)
			} else {
				fmt.Fprintf(v.out, "[uta] verify passed after %d repair attempt(s): %s\n", attempt, v.cmdLabel)
			}
			return true
		}
		if attempt >= v.maxRepairs {
			fmt.Fprintf(v.out, "[uta] verify still failing after %d repair attempt(s): %s (exit %d)\n",
				v.maxRepairs, v.cmdLabel, code)
			return false
		}
		fmt.Fprintf(v.out, "[uta] verify failed (exit %d) — repairing, attempt %d/%d…\n",
			code, attempt+1, v.maxRepairs)
		if err := v.repair(ctx, tail); err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintln(v.out, "verify: repair cancelled.")
				return false
			}
			fmt.Fprintf(v.out, "[uta] verify: repair turn failed to run: %v\n", err)
			return false
		}
	}
}

// verifyConfig carries what the production verify+repair loop needs from the
// run command's scope.
type verifyConfig struct {
	cmd           string
	maxRepairs    int
	workdir       string
	worker        string
	env           []string
	timeout       time.Duration
	noColor       bool
	app           *App
	stdin         io.Reader
	out           io.Writer
	errw          io.Writer
	preApprove    []string
	allowedTools  []string
	deniedTools   []string
	mcpConfigPath string
	modeName      string
}

// runVerifyRepair is the production wiring: it executes cfg.cmd in the
// workdir as the check, and runs one preset single-subtask worker turn
// (no planner, no synthesis) as the repair — so the loop stays cheap even
// on a slow local model. Returns true when the check ultimately passes.
func runVerifyRepair(ctx context.Context, cfg verifyConfig) bool {
	v := verifyRunner{
		cmdLabel:   cfg.cmd,
		maxRepairs: cfg.maxRepairs,
		out:        cfg.errw,
		verify: func(ctx context.Context) (int, string) {
			return execVerifyCommand(ctx, cfg.cmd, cfg.workdir, cfg.timeout)
		},
		repair: func(ctx context.Context, failure string) error {
			return runRepairTurn(ctx, cfg, failure)
		},
	}
	return v.run(ctx)
}

// execVerifyCommand runs the check in workdir and returns its exit code and
// the tail of its combined output. A command that cannot start at all
// reports a synthetic non-zero code so the loop treats it as a failure to
// be repaired (or, more likely, surfaced).
func execVerifyCommand(ctx context.Context, cmdline, workdir string, timeout time.Duration) (int, string) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, flag := "/bin/sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	c := exec.CommandContext(cctx, shell, flag, cmdline)
	c.Dir = workdir
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()

	tail := buf.String()
	if len(tail) > verifyMaxTail {
		tail = "…(truncated)…\n" + tail[len(tail)-verifyMaxTail:]
	}
	if err == nil {
		return 0, tail
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), tail
	}
	// Couldn't launch (bad command, timeout): non-zero with the reason.
	return 1, tail + "\n[uta] could not run verify command: " + err.Error()
}

// runRepairTurn runs exactly one worker turn whose job is to make the
// failing command pass. It's a preset single subtask with synthesis
// skipped — the worker re-reads the (now-broken) files from disk and fixes
// them, which is why this works for stateless local editors like aider.
func runRepairTurn(ctx context.Context, cfg verifyConfig, failure string) error {
	bus := trajectory.NewBus()
	rdr := NewRenderer(cfg.errw, cfg.noColor)
	rdr.ShowGoal("repair: make `"+cfg.cmd+"` pass", cfg.worker)
	renderDone := rdr.Subscribe(bus)

	sup := engine.New(engine.Deps{
		Store:      cfg.app.Store,
		Blobs:      cfg.app.Blobs,
		Recorder:   trajectory.NewRecorder(cfg.app.Store),
		Bus:        bus,
		Registry:   cfg.app.Registry,
		Memory:     memory.NewFromEnv(cfg.app.Store.DB),
		Whiteboard: whiteboard.New(cfg.app.Store.DB),
	})

	prompt := fmt.Sprintf(
		"The verification command `%s` is failing. Its output:\n\n%s\n\n"+
			"Edit the code in the working directory so this command passes. "+
			"Make the smallest change that fixes it; do not disable or delete the check itself.",
		cfg.cmd, failure)

	_, err := sup.Run(ctx, engine.RunRequest{
		Goal:            "repair: make `" + cfg.cmd + "` pass",
		WorkerName:      cfg.worker,
		MaxParallel:     1,
		SubtaskTimeout:  cfg.timeout,
		Workdir:         cfg.workdir,
		Env:             cfg.env,
		PreApproveTools: cfg.preApprove,
		AllowedTools:    cfg.allowedTools,
		DeniedTools:     cfg.deniedTools,
		MCPConfigPath:   cfg.mcpConfigPath,
		ModeName:        cfg.modeName,
		SkipSynthesis:   true,
		PreSetSubtasks: []engine.SubtaskSpec{{
			ID: "repair", Title: "fix failing verification", Prompt: prompt,
		}},
		HITL: &hitl.Gate{StateDir: cfg.app.StateDir, Stdin: cfg.stdin, Stderr: cfg.errw},
	})

	bus.Shutdown()
	<-renderDone
	return err
}
