//go:build linux || darwin

package management

import (
	"os"
	"syscall"
	"time"
)

// DefaultRestartScheduler SIGTERMs our own pid so the daemon shuts
// down gracefully. cmd/waired-agent wraps this to record the restart
// intent and exit with code 17, which the packaged systemd unit pairs
// with RestartForceExitStatus=17 — a model-switch restart works under
// Restart=on-failure while plain exit 0 / `systemctl stop` still stay
// down (issue #347: before that contract the clean exit 0 left the
// daemon dead after a switch).
func DefaultRestartScheduler() {
	// Sleep first so the HTTP 202 response flushes before SIGTERM
	// races the connection close.
	time.Sleep(time.Second)
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}
