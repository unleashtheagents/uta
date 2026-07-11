//go:build !windows

package engine

import (
	"os/exec"
	"syscall"
	"time"
)

// termGracePeriod is how long a cancelled process group gets to shut down
// after SIGTERM before it is SIGKILLed. Long enough for a provider CLI to
// flush partial output and release locks; short enough that a wedged child
// doesn't hold up the whole run.
const termGracePeriod = 5 * time.Second

// runCommand starts cmd as the leader of a new process group, runs it to
// completion, and arranges for context cancellation to signal the entire
// group. Without this, a timeout on a shell wrapper (`sh -c`) only kills the
// shell and leaves any children it spawned running as orphans.
//
// Cancellation is graceful-then-forceful: SIGTERM first so well-behaved
// children can clean up, SIGKILL after termGracePeriod for the rest. The
// WaitDelay backstop makes cmd.Wait return even if a grandchild inherited
// stdout/stderr and never closes them.
func runCommand(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Setpgid: true means pgid == pid; signal the negative pid to
		// reach every process in the group.
		pgid := -cmd.Process.Pid
		err := syscall.Kill(pgid, syscall.SIGTERM)
		// Escalate later without blocking Cancel — os/exec calls Cancel
		// synchronously on context cancellation, and cmd.Wait is still
		// collecting the child.
		go func() {
			time.Sleep(termGracePeriod)
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		}()
		return err
	}
	// If the group ignores SIGTERM and something keeps the output pipes
	// open past the SIGKILL escalation, force Wait to give up.
	cmd.WaitDelay = termGracePeriod + 2*time.Second
	return cmd.Run()
}
