//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

func newManager() Manager { return &windowsManager{} }

// Installed reports whether the SCM service is registered. Used by
// `waired init` to decide whether auto-starting the agent is possible.
func Installed() bool {
	scm, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer scm.Disconnect()
	s, err := scm.OpenService(ServiceName)
	if err != nil {
		return false
	}
	s.Close()
	return true
}

// StartHint is the manual command shown when init cannot (or is told not
// to) auto-start the agent.
func StartHint() string { return "Start-Service " + ServiceName }

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
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			default:
				writeEventlogError(fmt.Sprintf("unexpected control request #%d", req.Cmd))
			}
		case err := <-done:
			if err != nil {
				writeEventlogError(fmt.Sprintf("run() exited: %v", err))
			}
			status <- svc.Status{State: svc.Stopped}
			return false, 1
		}
	}
}

// windowsManager talks to the Service Control Manager.
type windowsManager struct{}

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

	if s, err := scm.OpenService(ServiceName); err == nil {
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

	// Recovery actions: 3 restarts with backoff. Counter resets after
	// 5 minutes of uptime.
	if err := s.SetRecoveryActions(
		[]mgr.RecoveryAction{
			{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
			{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
			{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		},
		uint32((5 * time.Minute).Seconds()),
	); err != nil {
		// Not fatal — the service can still be managed manually.
		fmt.Fprintf(os.Stderr, "warning: SetRecoveryActions: %v\n", err)
	}

	// Register an Event Log source so writes from inside the SCM
	// dispatcher (stderr is closed there) show up under "Windows Logs
	// > Application".
	if err := eventlog.InstallAsEventCreate(ServiceName,
		eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		if !errors.Is(err, errEventlogExists) {
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

// errEventlogExists is the magic errno eventlog returns when the
// source is already registered. We tolerate it on reinstall.
var errEventlogExists = errors.New("registry key already exists")
