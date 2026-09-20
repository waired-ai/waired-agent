//go:build windows

package detach

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// configure asks Windows for a child with no console of its own, in its own
// process group, and outside whatever Job Object the parent is in.
//
// CREATE_BREAKAWAY_FROM_JOB is the one that matters and the one that can
// fail: a Job Object created without JOB_OBJECT_LIMIT_BREAKAWAY_OK refuses
// it, and CreateProcess then fails outright rather than ignoring the flag.
// start below retries once without it.
func configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.DETACHED_PROCESS |
		windows.CREATE_NEW_PROCESS_GROUP |
		windows.CREATE_BREAKAWAY_FROM_JOB
}

// start runs the child, giving up the breakaway if the job forbids it.
//
// A child that could not break away dies when the job closes, which on this
// path means it dies with Claude Code. That is the behaviour before
// waired-agent#1454 — the rows appear at the next launch — so the retry wins
// the cases where the job permits breakaway and costs nothing in the cases
// where it does not.
func start(cmd *exec.Cmd) error {
	err := cmd.Start()
	if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	cmd.SysProcAttr.CreationFlags &^= uint32(windows.CREATE_BREAKAWAY_FROM_JOB)
	return cmd.Start()
}
