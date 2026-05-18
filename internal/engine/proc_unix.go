//go:build !windows

package engine

import (
	"os/exec"
	"syscall"
)

// runCommand starts cmd as the leader of a new process group, runs it to
// completion, and arranges for context cancellation to signal the entire
// group. Without this, a timeout on a shell wrapper (`sh -c`) only kills the
// shell and leaves any children it spawned running as orphans.
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
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd.Run()
}
