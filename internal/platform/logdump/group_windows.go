//go:build windows

package logdump

import "os/exec"

// ownProcessGroup is a no-op on Windows. There is no POSIX process group to
// put the child in, and the equivalent — a Job object the whole tree is bound
// to — is more machinery than this one bounded read is worth. What Windows
// does get is the same WaitDelay in runServiceLog, which is the half that
// stops a surviving grandchild from holding the command open.
func ownProcessGroup(*exec.Cmd) {}

// killProcessGroup falls back to killing the direct child, which is what
// CommandContext would have done on its own.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
