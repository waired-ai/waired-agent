// Command waired-tray is the Waired desktop tray. It runs as the
// desktop user (not as the system daemon) and talks only to the
// loopback Local Management API at 127.0.0.1:9476 — never reads
// identity.json directly so it stays safely outside the daemon's
// privilege boundary. The OS-specific menu actions (login/logout,
// clipboard, browser-open, dialogs, notifications) live in
// actions_<os>.go and dialog_<os>.go inside internal/gui/tray.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/gui/tray"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/console"
	"github.com/waired-ai/waired-agent/internal/platform/logrotate"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/singleinstance"
)

func main() {
	// Same UTF-8 console treatment as the other two binaries (#629). The tray
	// normally has no console at all, in which case this is a no-op; it earns
	// its place for the `-h` / flag-error path, which a developer does run
	// from a shell.
	restoreConsole := console.SetOutputUTF8()

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "waired-tray:", err)
		restoreConsole()
		os.Exit(1)
	}
	restoreConsole()
}

func run(args []string) error {
	fs := flag.NewFlagSet("waired-tray", flag.ContinueOnError)
	mgmtURL := fs.String("mgmt", "http://"+management.DefaultListen,
		"Local Management API base URL")
	controlURL := fs.String("control", os.Getenv("WAIRED_CONTROL_URL"),
		"Control Plane URL used by the Sign in… action (defaults to $WAIRED_CONTROL_URL)")
	stateDir := fs.String("state-dir", defaultStateDir(),
		"directory holding identity.json (passed to `waired init` / `waired logout` via pkexec)")
	pollEvery := fs.Duration("poll-every", 5*time.Second,
		"polling interval for /v1/status and /v1/identity")
	logLevel := fs.String("log-level", "",
		"log verbosity: debug|info|warn|error (default info, or $WAIRED_LOG_LEVEL). Without this flag the tray follows the daemon's live level, so `waired config log-level <level>` toggles the service and the app together.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Single-instance guard (waired#807): the installer post-install
	// launch, the Start-menu shortcut, and the next-logon HKCU Run
	// autostart can all fire, and each launch would otherwise register
	// its own tray icon. A guard failure is non-fatal — log it and run
	// unguarded rather than refuse to start.
	release, ok, err := singleinstance.Acquire("waired-tray")
	if err != nil {
		fmt.Fprintln(os.Stderr, "waired-tray: single-instance guard:", err)
	}
	if !ok {
		return nil // another instance is already running; exit 0 silently
	}
	defer release()

	ctx, cancel := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer cancel()

	// Logging: a LevelVar-backed handler so the tray's verbosity can change
	// at runtime. Initial level: --log-level > $WAIRED_LOG_LEVEL >
	// $WAIRED_DEBUG > info. Unless --log-level pinned a level, follow the
	// daemon's live level (waired config log-level) so one toggle covers
	// both the service and the app. Before this the tray had no logger init
	// and could not be put into debug mode at all.
	logLevelVar := new(slog.LevelVar)
	logLevelVar.Set(agentconfig.ResolveLogLevel("", *logLevel, os.Getenv))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevelVar})))
	if !flagPassed(fs, "log-level") {
		go followDaemonLogLevel(ctx, *mgmtURL, logLevelVar, *pollEvery)
	}

	// Bound the tray's own launchd log files (#331). The macOS
	// LaunchAgent points stdout/stderr at ~/Library/Logs/waired-tray.*,
	// and until now nothing rotated them at all — the retired newsyslog
	// drop-in only ever covered the daemon's. No-op off darwin, where
	// the desktop autostart does not capture the streams to files.
	// The bound follows the level: debug is roughly an order of magnitude
	// more output, and the default 1 MB x 5 leaves ~90 minutes of it (#658).
	// logLevelVar tracks the daemon's live level, so this reads the same
	// value the handler above is filtering on.
	logrotate.Manage(ctx, logrotate.TrayTargets(runtime.GOOS, trayLogHome()),
		func() logrotate.Policy { return logrotate.PolicyForLevel(logLevelVar.Level()) },
		slog.Default())

	// A signal must end this process, not merely ask it to. tray.Run
	// leaves the GUI event loop when systray.Quit lands, but the loop
	// can be somewhere that never gets there: a signal delivered before
	// onReady is not watched at all (systray.Quit is one-shot and would
	// be spent on a backend that is not up yet — see tray.watchShutdown),
	// and a backend that failed to start blocks in its loop regardless.
	// Sitting on the user's screen as an unresponsive icon IS the defect
	// (waired-agent#1031/#1045), so leave anyway when the budget is up.
	trayDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		// Put the default disposition back now that the first signal has
		// been taken. signal.NotifyContext's stop calls signal.Stop, so a
		// SECOND SIGTERM — from an uninstaller that has run out of
		// patience, or from a user pressing Ctrl-C twice — kills this
		// process outright instead of being swallowed by a handler that
		// is already busy shutting down.
		cancel()
		select {
		case <-trayDone:
		case <-time.After(shutdownDeadline):
			// os.Exit skips `defer release()` and restoreConsole. The
			// kernel drops the single-instance flock when the process
			// exits (internal/platform/singleinstance), and the tray
			// normally has no console to restore.
			slog.Warn("waired-tray: event loop did not unwind within the shutdown budget; exiting",
				"budget", shutdownDeadline)
			os.Exit(0)
		}
	}()

	tray.Run(ctx, tray.Options{
		MgmtURL:    *mgmtURL,
		ControlURL: *controlURL,
		StateDir:   *stateDir,
		Version:    buildinfo.Version,
		BuildSHA:   buildinfo.BuildSHA,
		PollEvery:  *pollEvery,
	})
	close(trayDone)
	return nil
}

// shutdownDeadline is how long a signalled tray may take to leave its
// GUI event loop before the process exits regardless.
//
// Expressed against tray.ShutdownBudget rather than as a bare number:
// the margin over it is for the event loop's own unwinding, and pinning
// the relationship is what stops a later tightening here from killing a
// tray that is winding down correctly. packaging/install/uninstall.sh's
// TRAY_STOP_GRACE is in turn larger than this, so its SIGTERM is given
// the whole budget before it escalates.
const shutdownDeadline = tray.ShutdownBudget + 5*time.Second

// trayLogHome resolves the home directory the tray's log paths hang
// off, with the same precedence internal/platform/autostart used when
// it wrote those paths into the LaunchAgent plist ($HOME first, then the
// passwd entry). Matching it is what keeps the rotator and the plist
// pointed at the same files. An unresolvable home yields "", which
// TrayTargets turns into "nothing to rotate" rather than a guess.
func trayLogHome() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// defaultStateDir picks the canonical user-side state dir, except that
// if a system-mode daemon's identity.json is found on disk we prefer
// that path. The tray normally talks to a system-mode daemon, so this
// override keeps `waired-tray` working in the default-install case.
func defaultStateDir() string {
	if dir := os.Getenv(paths.EnvOverride); dir != "" {
		return dir
	}
	sys := paths.StateDir(paths.System)
	if _, err := os.Stat(sys + "/identity.json"); err == nil {
		return sys
	}
	return paths.StateDir(paths.Interactive)
}
