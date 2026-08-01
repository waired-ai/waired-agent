//go:build linux

package logrotate

// defaultOps returns a zero ops on Linux, which makes Manage return
// without starting anything.
//
// Nothing to do: the packaged unit runs under systemd, which captures
// the agent's stdout/stderr into the journal, and journald applies its
// own size bounds (SystemMaxUse & co.). There is no file this process
// owns to rotate and no descriptor to re-point — the #331 failure mode
// (an external rotator renaming a file out from under a held fd) cannot
// arise.
func defaultOps() ops { return ops{} }
