//go:build windows

package cli

import "io"

// Echo suppression is a POSIX-termios affair; on Windows the console does
// its own line editing and the stakes (stray echo mid-stream) are the
// same but the fix differs. No-op for now.
func suppressEcho(io.Writer) func() { return func() {} }

func restoreEchoDuring(io.Writer) func() { return func() {} }
