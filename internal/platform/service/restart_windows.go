//go:build windows

package service

import (
	"context"
	"os"
	"sync/atomic"
	"time"
)

// scmStop holds the SCM handler's cancel function while Execute is
// running, so a restart request can bring the daemon down the same way a
// `net stop` does — through the run context — instead of killing the
// process from under it.
//
// Windows has no self-SIGTERM: signal_windows.go records that SIGTERM
// "exists as a constant … but is never delivered to a Windows process",
// which is why the previous implementation reached for os.Exit(1). That
// exit left the service without reporting SERVICE_STOPPED, so the SCM
// classed every model switch as a hard crash: it skipped the daemon's
// graceful teardown, burned one of the three recovery slots inside the
// 5-minute reset window, and wrote nothing an operator could read (#684).
var scmStop atomic.Pointer[context.CancelFunc]

// osRequestRestart stops the daemon so its supervisor can start it again.
//
// Under the SCM, cancelling the run context lets Execute return
// (true, RestartRequestedExitCode). x/sys maps that to
// Win32ExitCode = ERROR_SERVICE_SPECIFIC_ERROR with
// ServiceSpecificExitCode = 17, which is a failure — so the recovery
// actions applyRecoveryConfig installed restart the service, and
// `sc queryex` names the code rather than showing a bare crash.
//
// Run interactively there is no SCM to report to and nothing to restart
// us, so the exit code is all there is: same trade-off as a Unix host
// with no systemd.
func osRequestRestart() {
	// Match the Unix path: the management API's 202 has to flush before
	// the daemon goes away.
	time.Sleep(restartRequestDelay)
	if cancel := scmStop.Load(); cancel != nil {
		(*cancel)()
		return
	}
	os.Exit(RestartRequestedExitCode)
}
