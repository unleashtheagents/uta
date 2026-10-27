//go:build !windows

package cli

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// suppressEcho turns off terminal echo on f (a tty) and returns a restore
// function. While an agent turn is running the shell is not reading input,
// so anything the user types ahead would otherwise be echoed by the kernel
// into the middle of the streamed output; with echo off the queued input
// stays invisible until the next prompt reads and renders it. Returns a
// no-op when f is not a terminal.
func suppressEcho(w io.Writer) func() {
	f, ok := w.(*os.File)
	if !ok {
		return func() {}
	}
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return func() {}
	}
	old := *t
	t.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, t); err != nil {
		return func() {}
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlWriteTermios, &old) }
}

// restoreEchoDuring re-enables echo for the duration of an interactive
// prompt (the permissions gate) issued while a turn has echo suppressed,
// so the user sees what they answer. Returns the function that puts
// suppression back.
func restoreEchoDuring(w io.Writer) func() {
	f, ok := w.(*os.File)
	if !ok {
		return func() {}
	}
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return func() {}
	}
	if t.Lflag&unix.ECHO != 0 {
		return func() {} // echo already on; nothing to restore
	}
	old := *t
	t.Lflag |= unix.ECHO
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, t); err != nil {
		return func() {}
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlWriteTermios, &old) }
}
