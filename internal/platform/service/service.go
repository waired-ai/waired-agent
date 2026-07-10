// Package service is the OS-portable interface to the platform's
// service manager: SCM on Windows, systemd on Linux, launchd (stub
// only) on macOS.
//
// Callers (today only cmd/waired-agent) hand the package their
// foreground entrypoint via RunHook plus the install-time options in
// Config, and the package handles:
//
//   - `<binary> install [-state-dir=...] [-binary=...] [-mgmt=...] [-user=...]`
//   - `<binary> uninstall`
//   - `<binary> start`
//   - `<binary> stop`
//   - `<binary> debug` / `<binary> run`  (stripped, fall through to RunHook)
//   - SCM auto-detection on Windows (no subcommand + running under SCM
//     → enter the SCM dispatcher, invoke RunHook from the svc.Handler)
//
// Outside of those subcommands Dispatch returns handled=false so the
// caller proceeds with normal foreground daemon startup.
package service

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// ServiceName / DisplayName / Description are the identity strings
// registered with the OS service manager. Hard-coded because changing
// them would require an uninstall+reinstall cycle.
const (
	ServiceName = "waired-agent"
	DisplayName = "Waired Agent"
	Description = "Waired private inference overlay agent (WireGuard + Ollama gateway)."
)

// Config bundles the install-time options accepted by every backend.
// Backends ignore fields they cannot honour (e.g. User is a no-op on
// Windows because the SCM uses ServiceStartName=LocalSystem).
type Config struct {
	// StateDir is baked into the registered command line so the
	// service runs with --state-dir=<StateDir> even when started
	// from the init system with a stripped env.
	StateDir string

	// Binary is the absolute path of the agent executable to register.
	// Empty means "use the current process's executable path".
	Binary string

	// User is the unprivileged service user (Linux only — passed
	// through to systemd's User=). Empty means run as root.
	User string

	// MgmtAddr is an optional --mgmt override.
	MgmtAddr string

	// ExtraArgs are additional command-line tokens appended to the
	// service's registered ExecStart / ImagePath, after -state-dir and
	// -mgmt. Used by deployments (e.g. testnet VMs) that need the
	// service to start with --bypass-cp-iam, --force-relay,
	// --fallback-after=Xs and similar runtime flags. Linux: appended to
	// the systemd unit's ExecStart. Windows: appended to the SCM
	// CreateService args slice.
	ExtraArgs []string
}

// Manager controls the OS-native service: register/unregister and
// start/stop. Each OS provides its own implementation via newManager()
// (see service_{linux,windows,darwin,stub}.go).
type Manager interface {
	Install(cfg Config) error
	Uninstall() error
	Start(extraArgs []string) error
	Stop() error
}

// RunHook is the daemon's foreground entrypoint. Dispatch either
// invokes it directly (interactive run) or, on Windows under the SCM,
// from inside the SCM handler goroutine.
type RunHook func(ctx context.Context, args []string) error

// Dispatch consumes leading service sub-commands from args and routes
// them through the OS service manager. Returns handled=true with the
// process exit code when the sub-command (or SCM dispatch on Windows)
// fully handles the invocation; returns handled=false when the caller
// should proceed with its normal startup path.
func Dispatch(args []string, run RunHook) (handled bool, rc int) {
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return runSubcommand("install", args[1:], func() error {
				return installCommand(args[1:])
			})
		case "uninstall":
			return runSubcommand("uninstall", nil, func() error {
				// Best-effort deregister from the Control Plane before the
				// service unit goes away, so the device stops lingering in the
				// admin device list. Never blocks the uninstall.
				deregisterOnUninstall()
				return newManager().Uninstall()
			})
		case "start":
			return runSubcommand("start", args[1:], func() error {
				return newManager().Start(args[1:])
			})
		case "stop":
			return runSubcommand("stop", nil, func() error {
				return newManager().Stop()
			})
		case "debug", "run":
			// Strip the subcommand and let the caller proceed with
			// normal startup. Mutating os.Args is the cleanest way to
			// make flag.Parse-in-run see the rest of the args.
			os.Args = append([]string{os.Args[0]}, args[1:]...)
			return false, 0
		}
	}
	return osDispatchInteractive(args, run)
}

// installCommand parses install flags and invokes the OS manager.
func installCommand(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "",
		"directory holding identity.json + secrets/* + cache/* "+
			"(empty means platform default, baked into ImagePath args)")
	binary := fs.String("binary", "",
		"exe path to register (defaults to the current process exe)")
	mgmtAddr := fs.String("mgmt", "",
		"override the loopback management bind (passed through to the service)")
	user := fs.String("user", "",
		"service user (Linux only). Empty means root.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Positional args after `--` are baked into the service's command
	// line. Example:
	//   waired-agent install -state-dir=C:\ProgramData\waired -- --bypass-cp-iam --force-relay
	// Caller can append any flag the foreground daemon understands and
	// it will be picked up on every service start.
	cfg := Config{
		StateDir:  *stateDir,
		Binary:    *binary,
		User:      *user,
		MgmtAddr:  *mgmtAddr,
		ExtraArgs: fs.Args(),
	}

	// If --binary was omitted, fill in the current process's exe.
	if cfg.Binary == "" {
		p, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate own exe: %w", err)
		}
		cfg.Binary = p
	}

	// Sanity check the binary exists at the registered path.
	if _, err := os.Stat(cfg.Binary); err != nil {
		return fmt.Errorf("service binary %q: %w", cfg.Binary, err)
	}

	if cfg.StateDir == "" {
		cfg.StateDir = paths.StateDir(paths.System)
	}
	// Normalise to an absolute path so the systemd unit / SCM ImagePath
	// arg works regardless of cwd at install time.
	if abs, err := filepath.Abs(cfg.StateDir); err == nil {
		cfg.StateDir = abs
	}

	return newManager().Install(cfg)
}

// Restart stops (best-effort) then starts the registered service. A general
// service-lifecycle helper for callers that need the daemon to re-read on-disk
// state after a config change.
func Restart() error {
	m := newManager()
	_ = m.Stop() // ignore: the service may already be stopped
	return m.Start(nil)
}

// StartInstalled starts the already-registered waired-agent service via
// the OS service manager (systemctl / launchctl / SCM). It only starts —
// the enable-at-boot bit is set by Install — so it is safe to call right
// after a fresh enroll (the unit no longer crash-loops once an identity
// exists). Best-effort by contract: callers (`waired init`) log the error
// and fall back to StartHint(). On an unsupported OS it returns the stub
// error. Guard with Installed() to avoid noise on raw-binary dev installs.
func StartInstalled() error {
	return newManager().Start(nil)
}

// runSubcommand wraps a sub-command invocation: report success/failure
// and translate to a process exit code.
func runSubcommand(name string, _ []string, fn func() error) (bool, int) {
	if err := fn(); err != nil {
		fmt.Fprintf(os.Stderr, "%s %s: %v\n", ServiceName, name, err)
		return true, 1
	}
	fmt.Printf("%s service %sed.\n", ServiceName, name)
	return true, 0
}
