//go:build !windows

package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processAlive reports whether a process with the given pid currently exists.
// Signal 0 performs the kernel existence check without delivering anything;
// EPERM also implies the pid is live (we just lack permission to signal it).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}

// waitForFile polls until path is non-empty or the deadline expires.
func waitForFile(path string, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return b, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, fmt.Errorf("file %s never populated within %s", path, timeout)
}

// TestRunCommand_KillsGrandchildOnContextCancel exercises the core promise of
// runCommand on Unix: when the context is cancelled, the entire process group
// dies, not just the shell wrapper. The script forks a long-running sleep into
// the background and writes its pid to a file; cancelling the context should
// reap that pid along with the leader.
func TestRunCommand_KillsGrandchildOnContextCancel(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "child.pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Background a sleep, record its pid, then wait so the leader stays alive
	// until the context is cancelled. Without process-group kill, killing sh
	// here would leave the sleep child orphaned to init.
	script := fmt.Sprintf(`sleep 30 & echo $! > %s; wait`, pidFile)
	cmd := exec.CommandContext(ctx, "sh", "-c", script)

	done := make(chan error, 1)
	go func() { done <- runCommand(cmd) }()

	pidBytes, err := waitForFile(pidFile, 5*time.Second)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("child pid never appeared: %v", err)
	}
	childPid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", pidBytes, err)
	}
	if !processAlive(childPid) {
		t.Fatalf("child pid %d not alive before cancel", childPid)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Last-ditch: kill the child so we don't leak from a flaky test.
		_ = syscall.Kill(childPid, syscall.SIGKILL)
		t.Fatal("runCommand did not return within 5s of context cancel")
	}

	// SIGKILL is asynchronous and the kernel needs a moment to reap; poll.
	gone := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(childPid) {
			gone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gone {
		_ = syscall.Kill(childPid, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d survived context cancel — process group kill did not propagate", childPid)
	}
}

// TestRunCommand_SetpgidLeader verifies the leader runs in its own process
// group (pgid == pid), which is the precondition for the negative-pid signal
// used by Cancel to fan out to every descendant.
func TestRunCommand_SetpgidLeader(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "leader.pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Record the shell's own pid, then sleep so we can inspect it.
	script := fmt.Sprintf(`echo $$ > %s; sleep 30`, pidFile)
	cmd := exec.CommandContext(ctx, "sh", "-c", script)

	done := make(chan error, 1)
	go func() { done <- runCommand(cmd) }()

	pidBytes, err := waitForFile(pidFile, 5*time.Second)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("leader pid never appeared: %v", err)
	}
	leaderPid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse leader pid %q: %v", pidBytes, err)
	}

	pgid, err := syscall.Getpgid(leaderPid)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("getpgid(%d): %v", leaderPid, err)
	}
	if pgid != leaderPid {
		cancel()
		<-done
		t.Fatalf("expected pgid == pid (Setpgid=true), got pid=%d pgid=%d", leaderPid, pgid)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(leaderPid, syscall.SIGKILL)
		t.Fatal("runCommand did not return within 5s of context cancel")
	}
}

// TestRunCommand_PropagatesExitError checks that a non-zero exit from the
// child surfaces as an *exec.ExitError, which the callers (tools.go, gate.go)
// rely on to detect tool failures.
func TestRunCommand_PropagatesExitError(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 7")
	err := runCommand(cmd)
	if err == nil {
		t.Fatal("expected non-nil error from exit 7")
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if ee.ExitCode() != 7 {
		t.Fatalf("expected exit code 7, got %d", ee.ExitCode())
	}
}

// TestRunCommand_CleanExit verifies the happy path: a short-lived command
// returns nil and runCommand does not interfere with normal completion.
func TestRunCommand_CleanExit(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "true")
	if err := runCommand(cmd); err != nil {
		t.Fatalf("expected nil error from 'true', got %v", err)
	}
}
