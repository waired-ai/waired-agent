//go:build windows

package management

import (
	"os"
	"time"
)

// DefaultRestartScheduler exits non-zero: on Windows the SCM Recovery
// Actions configured at install time
// (cmd/waired-agent/service_windows.go) restart the service when the
// process exits with a non-zero code. os.Exit(1) hits that recovery
// path. Running waired-agent.exe interactively (not under the SCM)
// will just terminate the daemon — same trade-off as Unix without
// systemd.
func DefaultRestartScheduler() {
	time.Sleep(time.Second)
	os.Exit(1)
}
