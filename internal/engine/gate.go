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
	const (
		stdoutHdr   = "--- stdout ---\n"
		stderrHdr   = "--- stderr ---\n"
		truncMarker = "...[truncated]...\n"
	)

	// Assemble the ordered list of chunks once so we can both size the
	// Builder and, if truncating, skip the prefix that would be discarded.
	var chunks [6]string
	n := 0
	total := 0
	addSection := func(hdr, body string) {
		if body == "" {
			return
		}
		chunks[n] = hdr
		total += len(hdr)
		n++
		chunks[n] = body
		total += len(body)
		n++
		if body[len(body)-1] != '\n' {
			chunks[n] = "\n"
			total++
			n++
		}
	}
	addSection(stdoutHdr, g.Stdout)
	addSection(stderrHdr, g.Stderr)

	if maxBytes > 0 && total > maxBytes {
		var b strings.Builder
		b.Grow(len(truncMarker) + maxBytes)
		b.WriteString(truncMarker)
		skip := total - maxBytes
		for i := 0; i < n; i++ {
			c := chunks[i]
			if skip >= len(c) {
				skip -= len(c)
				continue
			}
			b.WriteString(c[skip:])
			skip = 0
		}
		return b.String()
	}

	var b strings.Builder
	b.Grow(total)
	for i := 0; i < n; i++ {
		b.WriteString(chunks[i])
	}
	return b.String()
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

	runErr := runCommand(cmd)
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
