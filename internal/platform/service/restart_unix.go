//go:build linux || darwin

package service

import (
	"os"
	"syscall"
	"time"
)

// osRequestRestart SIGTERMs our own pid so the daemon shuts down
// gracefully. main() then exits RestartRequestedExitCode, which the two
// Unix supervisors read differently:
//
//   - systemd: SuccessExitStatus=17 + RestartForceExitStatus=17, both
//     written by renderSystemdUnit — the unit never enters `failed` and
//     restarts even under Restart=on-failure (#347).
//   - launchd: KeepAlive{SuccessfulExit=false}, written by
//     renderLaunchDaemonPlist — any non-zero exit brings the job back,
//     so 17 works, but launchd has no per-exit-code key and cannot tell
//     it apart from a crash. RestartOnExitFor("darwin") states that.
//
// The sleep is the same one internal/management used before this moved:
// the management API answers 202 before the switch takes effect, and a
// SIGTERM that races the connection close loses the response.
func osRequestRestart() {
	time.Sleep(restartRequestDelay)
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}
