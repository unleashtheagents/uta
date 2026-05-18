package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// GateResult is the captured outcome of a Gate execution.
type GateResult struct {
	Cmd      string
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	Err      error // wraps timeout / cancel / spawn errors; nil if the process ran to completion (even if exit != 0)
}

// Passed reports whether the gate exited zero.
func (g *GateResult) Passed() bool { return g.Err == nil && g.ExitCode == 0 }

// CombinedOutputTail returns up to maxBytes from the end of stdout/stderr
// concatenated, suitable for feeding back to the producer agent on retry.
func (g *GateResult) CombinedOutputTail(maxBytes int) string {
	var b strings.Builder
	if g.Stdout != "" {
		b.WriteString("--- stdout ---\n")
		b.WriteString(g.Stdout)
		if g.Stdout[len(g.Stdout)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	if g.Stderr != "" {
		b.WriteString("--- stderr ---\n")
		b.WriteString(g.Stderr)
		if g.Stderr[len(g.Stderr)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	out := b.String()
	if maxBytes > 0 && len(out) > maxBytes {
		out = "...[truncated]...\n" + out[len(out)-maxBytes:]
	}
	return out
}

// runGate executes the gate's shell command in workdir with the supplied
// timeout (or context deadline if Timeout is zero). It returns a GateResult;
// the function itself does not error unless the spawn or kill failed.
func runGate(parent context.Context, gate *Gate, workdir string) GateResult {
	if gate == nil || gate.Cmd == "" {
		return GateResult{Err: errors.New("empty gate")}
	}
	ctx := parent
	var cancel context.CancelFunc
	if gate.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, gate.Timeout)
		defer cancel()
	}

	start := time.Now()
	var cmd *exec.Cmd
	if len(gate.Args) > 0 {
		cmd = exec.CommandContext(ctx, gate.Cmd, gate.Args...)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", gate.Cmd)
	}
	cmd.Dir = workdir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	res := GateResult{
		Cmd:      gate.Cmd,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}

	if runErr == nil {
		res.ExitCode = 0
		return res
	}

	if errors.Is(parent.Err(), context.Canceled) {
		res.Err = parent.Err()
		return res
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.Err = fmt.Errorf("gate timed out after %s", gate.Timeout)
		return res
	}

	// Process ran and exited non-zero. exec.ExitError carries the exit code.
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		res.ExitCode = ee.ExitCode()
		return res
	}

	// Could not spawn the process at all (binary not found, etc.).
	res.Err = runErr
	res.ExitCode = -1
	return res
}
