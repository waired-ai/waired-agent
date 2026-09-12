//go:build linux || darwin

package logdump

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup puts the log-reading child in its own process group, so the
// kill that follows a cancelled context reaches anything IT started rather
// than only the direct child.
//
// `journalctl` and `log` are both capable of outliving the command that asked
// them a question — a pager, a stalled read of a corrupt journal — and
// CommandContext's default kill signals the direct child only. A survivor
// holds the write end of the pipe CombinedOutput is reading, so `waired logs`
// waits on a process nobody is waiting for. The same treatment the engine
// spawner and the Hugging Face downloader already give their children
// (internal/runtime/spawner_unix.go).
func ownProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup is the Cancel hook: signal the whole group, not just the
// leader. Negative pid is the POSIX "this process group" convention.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// Fall back to the single process: the group may already be gone,
		// and killing the leader is still better than killing nothing.
		return cmd.Process.Kill()
	}
	return nil
}
