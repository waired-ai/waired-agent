package tray

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// autostartFirstRunMarker is a file whose EXISTENCE records that the
// tray has started at least once for this user.
//
// It exists to tell two states apart that were previously identical:
// "this user has never run Waired, so register the login item for them"
// and "this user ran Waired and switched the login item off". Both look
// like an absent LaunchAgent plist / absent HKCU Run value, and
// ensureAutostartOnFirstLaunch used to read that absence as the first
// one — so switching "Start Waired on login" off did not survive the
// next tray start, and an installer that restarted the tray would have
// silently overturned the choice on every update. install.sh's
// darwin_tray_autostart_notice names exactly that ambiguity as the
// reason a macOS update reports the login item rather than registering
// it (waired-agent#1046).
const autostartFirstRunMarker = "autostart-first-run"

// autostartFirstLaunchFacts is what the decision below is made from.
type autostartFirstLaunchFacts struct {
	// Applies: this OS registers a login item from the tray's own first
	// run (firstLaunchAutostartApplies).
	Applies bool
	// Enabled: a login item is already registered for this user.
	Enabled bool
	// HasRun: the marker is present, i.e. this user has started the tray
	// before, so an absent login item is a choice rather than a default.
	HasRun bool
}

// planFirstLaunchAutostart decides whether this launch registers the
// per-user login item.
//
// A string plan rather than a bool so the reason survives into the log
// and into the test table, the same shape install.sh's
// darwin_tray_launch_plan uses.
func planFirstLaunchAutostart(f autostartFirstLaunchFacts) string {
	switch {
	case !f.Applies:
		// Linux registers nothing here: the .deb already installed
		// /etc/xdg/autostart/waired-tray.desktop for every user.
		return "skip:not-applicable"
	case f.Enabled:
		return "skip:already-enabled"
	case f.HasRun:
		return "skip:user-decided"
	}
	return "register"
}

// autostartMarkerPath is where the marker lives: beside the
// single-instance lock, in the per-user interactive state dir, because
// that is the one directory the tray is guaranteed to be able to write
// as the desktop user (the caller's StateDir may be the root-owned
// system one).
func autostartMarkerPath() string {
	return filepath.Join(paths.StateDir(paths.Interactive), autostartFirstRunMarker)
}

// autostartHasRun reports whether the marker is there. An unreadable
// state dir answers false, which reproduces the behaviour that shipped
// before the marker existed rather than refusing to register at all.
func autostartHasRun() bool {
	_, err := os.Stat(autostartMarkerPath())
	return err == nil
}

// recordAutostartFirstRun writes the marker. Best-effort: a host where
// it cannot be written simply keeps the old ambiguity, which is worth a
// debug line and nothing more.
func recordAutostartFirstRun() {
	p := autostartMarkerPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		slog.Debug("tray: could not create the state dir for the autostart marker", "err", err)
		return
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		slog.Debug("tray: could not write the autostart first-run marker", "err", err)
	}
}
