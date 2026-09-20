//go:build windows

package detach

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachedFlags is what the child is created with. CREATE_BREAKAWAY_FROM_JOB
// is the one that matters and the one that can fail: a Job Object created
// without JOB_OBJECT_LIMIT_BREAKAWAY_OK refuses it, and CreateProcess then
// fails outright rather than ignoring the flag. Measured on Windows 11
// (10.0.26200) on 2026-09-20: inside such a job the first start returns
// ERROR_ACCESS_DENIED.
const detachedFlags = windows.DETACHED_PROCESS |
	windows.CREATE_NEW_PROCESS_GROUP |
	windows.CREATE_BREAKAWAY_FROM_JOB

func configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= detachedFlags
}

// start runs the child, giving up the breakaway if the job forbids it.
//
// The retry runs a FRESH command rather than the one that failed:
// exec.Cmd is single use and answers a second Start with "exec: already
// started", so retrying in place looks like a fallback and is a second
// failure. Measured on a real host before it was written this way
// (waired-agent#1454).
//
// What the retry buys: in a job that refuses breakaway but does not kill its
// members when it closes, the plain start succeeds and the child lives. In a
// job that does kill on close the child dies with Claude Code either way, and
// the rows appear at the next launch — the behaviour before #1454.
func start(cmd *exec.Cmd) (*os.Process, error) {
	err := cmd.Start()
	if err == nil {
		return cmd.Process, nil
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, err
	}
	retry := withoutBreakaway(cmd)
	return started(retry, retry.Start())
}

// withoutBreakaway copies the fields Start is documented to set — this
// package has one caller and sets exactly these — into a command that does
// not ask to leave the job.
func withoutBreakaway(cmd *exec.Cmd) *exec.Cmd {
	retry := exec.Command(cmd.Path)
	retry.Args = cmd.Args
	retry.Env = cmd.Env
	retry.Dir = cmd.Dir
	retry.Stdin, retry.Stdout, retry.Stderr = cmd.Stdin, cmd.Stdout, cmd.Stderr
	flags := uint32(detachedFlags) &^ uint32(windows.CREATE_BREAKAWAY_FROM_JOB)
	if cmd.SysProcAttr != nil {
		flags = cmd.SysProcAttr.CreationFlags &^ uint32(windows.CREATE_BREAKAWAY_FROM_JOB)
	}
	retry.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	return retry
}
