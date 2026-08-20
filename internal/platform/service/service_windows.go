//go:build windows

package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

func newManager() Manager { return &windowsManager{} }

// Installed reports whether the SCM service is registered. Used by
// `waired init` to decide whether auto-starting the agent is possible, and
// to tell "the service is registered but not answering" apart from "no
// agent here at all" (#175).
func Installed() bool { return installedNamed(ServiceName) }

// installedNamed is Installed's testable core.
//
// It deliberately does NOT use mgr.Connect(): that opens the SCM with
// SC_MANAGER_ALL_ACCESS (x/sys windows/svc/mgr/mgr.go), which requires
// Administrator. A non-elevated `waired init` therefore saw a perfectly
// well-registered service as absent, and #175's "the service is installed
// but isn't responding" diagnosis could never fire on Windows. Asking only
// for SC_MANAGER_CONNECT + SERVICE_QUERY_STATUS is a read that any
// authenticated user is granted, which is all a presence check needs.
func installedNamed(name string) bool {
	scm, err := windows.OpenSCManager(nil, nil, installedSCMAccess)
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseServiceHandle(scm) }()
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	svcHandle, err := windows.OpenService(scm, namePtr, installedServiceAccess)
	if err != nil {
		return false
	}
	_ = windows.CloseServiceHandle(svcHandle)
	return true
}

// The access rights installedNamed asks for. Named (rather than inline) so
// a test can assert no privileged bit creeps back in — adding one would
// silently restore the elevation requirement that hid #175 on Windows.
const (
	installedSCMAccess     uint32 = windows.SC_MANAGER_CONNECT
	installedServiceAccess uint32 = windows.SERVICE_QUERY_STATUS
)

// FixStateOwnership is a no-op on Windows: the SCM service runs as
// LocalSystem and reads %ProgramData%\waired, which an elevated
// `waired init` can already write — there is no separate service user to
// chown to.
func FixStateOwnership(string) error { return nil }

// osDispatchInteractive runs the SCM dispatcher when svc.IsWindowsService
// is true (i.e. the binary was started by the SCM with no explicit
// subcommand). On the interactive desktop path it returns
// handled=false so main() proceeds with normal startup.
func osDispatchInteractive(args []string, run RunHook) (bool, int) {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		fmt.Fprintln(os.Stderr, ServiceName+": svc.IsWindowsService:", err)
		return true, 1
	}
	if !isSvc {
		return false, 0
	}
	if err := svc.Run(ServiceName, &svcHandler{args: args, run: run}); err != nil {
		writeEventlogError(fmt.Sprintf("svc.Run failed: %v", err))
		return true, 1
	}
	return true, 0
}

// svcHandler bridges the Go runtime to the Windows Service Control
// Manager. Execute is invoked on its own goroutine by svc.Run; our job
// is to start the daemon, then translate svc.Stop / svc.Shutdown
// requests into a context cancellation that run observes.
type svcHandler struct {
	args []string
	run  RunHook
}

const acceptedControls = svc.AcceptStop | svc.AcceptShutdown

func (h *svcHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Publish the stop lever so a restart request can bring the daemon
	// down through this context instead of killing the process (#684).
	// Cleared on the way out so a late request cannot cancel a context
	// nobody is running on.
	scmStop.Store(&cancel)
	defer scmStop.Store(nil)

	done := make(chan error, 1)
	go func() { done <- h.run(ctx, h.args) }()

	status <- svc.Status{State: svc.Running, Accepts: acceptedControls}

	for {
		select {
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case err := <-done:
					if err != nil {
						writeEventlogError(fmt.Sprintf("run() returned error during stop: %v", err))
					}
				case <-time.After(20 * time.Second):
					writeEventlogError("run() did not exit within 20s of stop; SCM may report timeout")
				}
				// No Stopped push here either — see the exit arm below.
				// Reporting it costs nothing on this path (a `net stop` IS
				// exit 0) but leaving one copy in the file is how it got
				// copied into the arm where it matters.
				return false, 0
			default:
				writeEventlogError(fmt.Sprintf("unexpected control request #%d", req.Cmd))
			}
		case err := <-done:
			if err != nil {
				writeEventlogError(fmt.Sprintf("run() exited: %v", err))
			}
			// Nothing reports Stopped here. x/sys does it on the way out
			// of serviceMain, and ONLY that report carries the exit code:
			// updateStatus derives Win32ExitCode from Execute's return
			// values, never from the svc.Status pushed down this channel
			// (which has a Win32ExitCode field it does not read). A
			// Stopped pushed from in here therefore reaches the SCM as
			// SetServiceStatus(SERVICE_STOPPED, dwWin32ExitCode = 0) —
			// a clean, deliberate stop — and the SCM finalises the
			// service on the FIRST Stopped it sees. The second report
			// never lands: the status handle is invalid once Stopped is
			// reported, so even `sc queryex` keeps showing the 0.
			//
			// That is #855: recovery actions run when the process dies
			// without reporting Stopped, or reports it with a non-zero
			// dwWin32ExitCode. A leading zero left nothing to recover
			// from, so the agent asked for a restart, said so in the
			// event log, and the SCM left the host down until someone
			// started it by hand.
			//
			// A restart the agent asked for is reported AS ITSELF (#684).
			// svcSpecificEC=true makes x/sys set
			// Win32ExitCode = ERROR_SERVICE_SPECIFIC_ERROR and
			// ServiceSpecificExitCode = 17, so `sc queryex` and the
			// service diagnostics can name it instead of showing the
			// bare failure a plain exit 1 produces. It is still a
			// non-zero stop, which is what makes the recovery actions
			// installed by applyRecoveryConfig fire — with
			// SetRecoveryActionsOnNonCrashFailures(true) (#315) doing
			// the work, since this is a clean STOPPED, not a crash.
			if RestartRequested() {
				writeEventlogError(fmt.Sprintf(
					"the agent asked to be restarted; reporting exit %d so the SCM restarts it",
					RestartRequestedExitCode))
				return true, RestartRequestedExitCode
			}
			return false, 1
		}
	}
}

// windowsManager talks to the Service Control Manager.
type windowsManager struct{}

// applyRecoveryConfig tells the SCM to restart the agent when it dies: three
// tries with backoff, counter reset after five minutes of uptime.
//
// The second call is the one that was missing. By default the SCM only runs
// recovery actions when a service terminates *without* reporting
// SERVICE_STOPPED — a hard crash. Our svc.Handler does the polite thing on a
// fatal error: it reports Stopped and exits 1 (see Execute). The SCM classes
// that as a clean, deliberate stop, so none of the three restarts above ever
// fired for the failure mode most likely to happen in the field. Windows was
// alone in this: systemd has Restart=always and launchd has
// KeepAlive{SuccessfulExit=false}, both of which already cover a nonzero exit.
//
// Neither call is fatal — a service with no recovery policy still runs, and
// still starts at boot. Errors go to stderr because install runs interactively.
func applyRecoveryConfig(s *mgr.Service) {
	if err := s.SetRecoveryActions(
		[]mgr.RecoveryAction{
			{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
			{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
			{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		},
		uint32((5 * time.Minute).Seconds()),
	); err != nil {
		fmt.Fprintf(os.Stderr, "warning: SetRecoveryActions: %v\n", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		fmt.Fprintf(os.Stderr, "warning: SetRecoveryActionsOnNonCrashFailures: %v\n", err)
	}
}

func (m *windowsManager) Install(cfg Config) error {
	svcArgs := []string{"-state-dir=" + cfg.StateDir}
	if cfg.MgmtAddr != "" {
		svcArgs = append(svcArgs, "-mgmt="+cfg.MgmtAddr)
	}
	svcArgs = append(svcArgs, cfg.ExtraArgs...)

	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w (run as Administrator?)", err)
	}
	defer scm.Disconnect()

	// An install over an existing service still reconciles the recovery
	// configuration before refusing. Install is how every upgrade path
	// re-runs (install.ps1 re-invokes it), and a machine that was set up
	// before a recovery-policy change would otherwise keep the old policy
	// forever — the service is only ever created once. Registration itself
	// still requires an uninstall, since CreateService cannot rewrite the
	// ImagePath of a running service.
	if s, err := scm.OpenService(ServiceName); err == nil {
		applyRecoveryConfig(s)
		s.Close()
		return fmt.Errorf("service %q is already installed; run `%s uninstall` first",
			ServiceName, ServiceName)
	}

	s, err := scm.CreateService(ServiceName, cfg.Binary, mgr.Config{
		DisplayName:      DisplayName,
		Description:      Description,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ServiceStartName: "LocalSystem",
		ErrorControl:     mgr.ErrorNormal,
	}, svcArgs...)
	if err != nil {
		return fmt.Errorf("CreateService: %w", err)
	}
	defer s.Close()

	applyRecoveryConfig(s)

	// Register an Event Log source so writes from inside the SCM
	// dispatcher (stderr is closed there) show up under "Windows Logs
	// > Application".
	//
	// Error|Warning and not |Info: logsink tees only Warn+ to this source
	// (internal/platform/logsink, waired-agent#764), so declaring Info was
	// a claim nothing backed. TypesSupported is advisory metadata —
	// ReportEvent is not filtered by it — and InstallAsEventCreate returns
	// before writing it on a source that already exists, so this narrowing
	// reaches freshly registered sources only. It fixes the statement, not
	// any behaviour.
	if err := eventlog.InstallAsEventCreate(ServiceName,
		eventlog.Error|eventlog.Warning); err != nil {
		if !eventlogSourceExists(err) {
			fmt.Fprintf(os.Stderr, "warning: eventlog.InstallAsEventCreate: %v\n", err)
		}
	}

	// Ensure the state dir + secrets subdir exist with a tight DACL
	// via platform/secrets. platform/secrets applies a restrictive
	// DACL via SetNamedSecurityInfo.
	if err := secrets.SecureDir(cfg.StateDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: SecureDir(%s): %v\n", cfg.StateDir, err)
	}
	if err := secrets.SecureDir(cfg.StateDir + `\secrets`); err != nil {
		fmt.Fprintf(os.Stderr, "warning: SecureDir(secrets): %v\n", err)
	}

	return nil
}

func (m *windowsManager) Uninstall() error {
	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w (run as Administrator?)", err)
	}
	defer scm.Disconnect()
	s, err := scm.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service %q not installed", ServiceName)
	}
	defer s.Close()

	if status, err := s.Query(); err == nil && status.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			fmt.Fprintf(os.Stderr, "warning: stop before delete: %v\n", err)
		}
		_ = waitForStateChange(s, svc.Stopped, 10*time.Second)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	_ = eventlog.Remove(ServiceName)
	return nil
}

func (m *windowsManager) Start(extraArgs []string) error {
	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer scm.Disconnect()
	s, err := scm.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	if err := s.Start(extraArgs...); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return waitForStateChange(s, svc.Running, 20*time.Second)
}

func (m *windowsManager) Stop() error {
	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer scm.Disconnect()
	s, err := scm.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("send Stop: %w", err)
	}
	return waitForStateChange(s, svc.Stopped, 20*time.Second)
}

func waitForStateChange(s *mgr.Service, target svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State == target {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("service did not reach state %d within %s", target, timeout)
}

// writeEventlogError is a best-effort log helper used while the SCM
// dispatcher owns stderr. Silently no-ops on any error.
func writeEventlogError(msg string) {
	elog, err := eventlog.Open(ServiceName)
	if err != nil {
		return
	}
	defer elog.Close()
	_ = elog.Error(1, msg)
}

// eventlogSourceExists reports whether an InstallAsEventCreate failure is
// the benign "this source is already registered" one, which a reinstall
// over a surviving source always produces.
//
// Matched on the message, not with errors.Is: x/sys builds this error with
// a bare errors.New carrying the full registry path
// (golang.org/x/sys/windows/svc/eventlog.Install), exports no sentinel to
// compare against, and *errorString implements neither Is nor Unwrap. The
// local sentinel this replaced could therefore never match, so the warning
// it was meant to suppress printed on every reinstall.
func eventlogSourceExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "registry key already exists")
}
