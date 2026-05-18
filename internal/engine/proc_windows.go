//go:build windows

package engine

import (
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// runCommand starts cmd inside a Windows Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, runs it to completion, and arranges
// for context cancellation to close the job handle. Closing the job
// terminates every process associated with it, including descendants of
// cmd, which prevents orphaned tool/gate children from leaking on timeout.
func runCommand(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}

	var (
		closeOnce sync.Once
		closeErr  error
	)
	closeJob := func() error {
		closeOnce.Do(func() {
			closeErr = windows.CloseHandle(job)
		})
		return closeErr
	}
	defer closeJob()

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return fmt.Errorf("set job object info: %w", err)
	}

	// On context cancellation, dropping our last handle to the job triggers
	// KILL_ON_JOB_CLOSE, which terminates the leader and every descendant
	// that has been assigned to the job.
	cmd.Cancel = func() error {
		return closeJob()
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	proc, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.PROCESS_SET_QUOTA,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("open process: %w", err)
	}
	defer windows.CloseHandle(proc)

	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("assign process to job: %w", err)
	}

	return cmd.Wait()
}
