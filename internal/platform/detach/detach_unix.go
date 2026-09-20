//go:build linux || darwin

package detach

import (
	"os/exec"
	"syscall"
)

// configure puts the child in its own process group, so a signal aimed at the
// group the parent belongs to does not reach it. Same shape as
// internal/platform/logdump/group_unix.go: never replace a SysProcAttr the
// caller set, only add to it.
//
// The process group is what stops a group-directed signal; being reparented
// to init when the parent exits is what lets the child keep running.
func configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func start(cmd *exec.Cmd) error { return cmd.Start() }
