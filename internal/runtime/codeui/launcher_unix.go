//go:build !windows

package codeui

import (
	"os"
	"syscall"
)

// detachSysProcAttr starts the serve host in its own session (setsid) so it has
// no controlling terminal and survives the launching shell / SSH logout.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// signalStop asks the serve host to shut down gracefully (SIGTERM); the host's
// signal handler cancels its context, stopping opencode + the proxy.
func signalStop(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
}
