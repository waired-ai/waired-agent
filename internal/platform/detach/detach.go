// Package detach starts a child that outlives the process starting it.
//
// One caller so far: the second write of Claude Code's /model rows
// (waired-agent#1454). A SessionStart hook has to return in about a second —
// Claude Code waits for it before the session begins, and bounds it with a
// timeout — while the write that reaches the running session has to happen
// several seconds later, after Claude Code has armed its settings watch. The
// hook therefore starts a child and returns without it.
//
// Start does NOT redirect the child's standard streams; the caller sets them,
// and must, because inheriting them is the failure this exists to avoid. A
// child holding the write end of the hook's stdout pipe keeps that pipe open
// after the hook exits, and Claude Code waits for the EOF. A spawner that
// quietly pointed them at the null device would hide that from every caller
// after the first, and from its own tests.
package detach

import (
	"fmt"
	"os"
	"os/exec"
)

// Start runs cmd so that it survives its parent's exit, and does not wait for
// it. The caller does not call Wait either: the process is released, so the OS
// reaps it.
func Start(cmd *exec.Cmd) error {
	if cmd.Stdin == nil || cmd.Stdout == nil || cmd.Stderr == nil {
		return fmt.Errorf("detach: the child's standard streams must be set " +
			"(nil inherits the parent's, which is what keeps a hook's pipe open)")
	}
	configure(cmd)
	p, err := start(cmd)
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	return p.Release()
}

// started is what a successful start hands back. It is the process rather than
// the command because the Windows path may have to start a SECOND command to
// get one (see detach_windows.go), and the caller only ever needs to release
// whatever actually ran.
func started(cmd *exec.Cmd, err error) (*os.Process, error) {
	if err != nil {
		return nil, err
	}
	return cmd.Process, nil
}
