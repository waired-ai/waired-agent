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
	"syscall"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/gui/tray"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/singleinstance"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "waired-tray:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("waired-tray", flag.ContinueOnError)
	mgmtURL := fs.String("mgmt", "http://"+management.DefaultListen,
		"Local Management API base URL")
	controlURL := fs.String("control", os.Getenv("WAIRED_CONTROL_URL"),
		"Control Plane URL used by the Log in… action (defaults to $WAIRED_CONTROL_URL)")
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
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

	tray.Run(ctx, tray.Options{
		MgmtURL:    *mgmtURL,
		ControlURL: *controlURL,
		StateDir:   *stateDir,
		Version:    buildinfo.Version,
		BuildSHA:   buildinfo.BuildSHA,
		PollEvery:  *pollEvery,
	})
	return nil
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
