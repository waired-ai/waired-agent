//go:build linux || darwin

package state

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive returns true if the given pid is a live process visible to
// us. On Unix we use the signal-0 liveness probe: ESRCH means the pid
// is gone, EPERM means it exists but we cannot signal it (which still
// counts as alive for our purposes).
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return false
		}
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == syscall.EPERM {
			return true
		}
		return false
	}
	return true
}
