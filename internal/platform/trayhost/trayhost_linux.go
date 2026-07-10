//go:build linux

package trayhost

import (
	"os"
	"strings"

	"github.com/godbus/dbus/v5"
)

// Check probes the live session for an SNI host and classifies the desktop.
// Read-only: it makes one D-Bus property read and a few env lookups, never
// mutating anything.
func Check() Result {
	return evaluate(facts{
		hasDisplay:     hasDisplay(),
		hostRegistered: sniHostRegistered(),
		desktop:        detectDesktop(),
		wayland:        isWayland(),
	})
}

// facts are the raw inputs evaluate turns into a Result. Split out so the
// decision matrix is pure and unit-testable without a live D-Bus session.
type facts struct {
	hasDisplay     bool
	hostRegistered bool
	desktop        Desktop
	wayland        bool
}

const (
	gnomeHintNoHost = "GNOME has no built-in tray; install an AppIndicator host extension " +
		"(`sudo apt install gnome-shell-extension-appindicator`), enable it " +
		"(`gnome-extensions enable appindicatorsupport@rgcjonas.gmail.com`), then log out and " +
		"back in on Wayland. `waired init` does this automatically when it detects GNOME."
	mateHint = "this desktop can't render StatusNotifierItem tray icons; " +
		"use GNOME (with the AppIndicator extension) or KDE Plasma to see the waired-tray icon."
	genericHintNoHost = "no system-tray (SNI) host detected; the waired-tray icon may not appear. " +
		"On GNOME install an AppIndicator extension; KDE Plasma has one built in."
)

// evaluate maps gathered facts to a Result. Pure: see facts.
func evaluate(f facts) Result {
	if !f.hasDisplay {
		return Result{Status: NotApplicable, Desktop: f.desktop, Wayland: f.wayland}
	}
	if f.hostRegistered {
		return Result{Status: HostPresent, Desktop: f.desktop, Wayland: f.wayland}
	}
	switch f.desktop {
	case DesktopGNOME:
		return Result{Status: NoHost, Desktop: f.desktop, Wayland: f.wayland, Hint: gnomeHintNoHost}
	case DesktopMATE:
		return Result{Status: Unsupported, Desktop: f.desktop, Wayland: f.wayland, Hint: mateHint}
	default:
		// KDE with no host is unusual (it ships one), but treat any
		// graphical session without an SNI host the same way.
		return Result{Status: NoHost, Desktop: f.desktop, Wayland: f.wayland, Hint: genericHintNoHost}
	}
}

// parseDesktop classifies an XDG_CURRENT_DESKTOP value (e.g. "ubuntu:GNOME",
// "KDE", "MATE", "X-Cinnamon"). Matching is case-insensitive and tolerant of
// the colon-separated multi-value form freedesktop allows.
func parseDesktop(xdgCurrentDesktop string) Desktop {
	if xdgCurrentDesktop == "" {
		return DesktopUnknown
	}
	v := strings.ToLower(xdgCurrentDesktop)
	switch {
	case strings.Contains(v, "gnome"):
		return DesktopGNOME
	case strings.Contains(v, "kde"):
		return DesktopKDE
	case strings.Contains(v, "mate"):
		return DesktopMATE
	default:
		return DesktopOther
	}
}

// hasDisplay reports whether a graphical session is present. A headless SSH
// server has neither DISPLAY (X11) nor WAYLAND_DISPLAY — mirrors
// internal/platform/browser.HasDisplay (kept local to avoid importing the
// browser package for one predicate).
func hasDisplay() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland"
}

func detectDesktop() Desktop {
	if !hasDisplay() {
		return DesktopNone
	}
	d := parseDesktop(os.Getenv("XDG_CURRENT_DESKTOP"))
	if d == DesktopUnknown {
		// XDG_CURRENT_DESKTOP can be unset under bare X11 / minimal sessions;
		// XDG_SESSION_DESKTOP is the secondary hint.
		d = parseDesktop(os.Getenv("XDG_SESSION_DESKTOP"))
	}
	if d == DesktopUnknown {
		return DesktopOther
	}
	return d
}

// sniHostRegistered reads org.kde.StatusNotifierWatcher's
// IsStatusNotifierHostRegistered property on the session bus. True means some
// host (KDE's panel, GNOME's AppIndicator extension, …) is registered and will
// draw our tray item. Any failure — no session bus, no watcher on the bus
// (i.e. no host at all), property read error — is reported as "not registered",
// which is the conservative answer the caller acts on. A private connection is
// used and closed so the short-lived CLI leaves no dangling bus name.
func sniHostRegistered() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()

	obj := conn.Object("org.kde.StatusNotifierWatcher", dbus.ObjectPath("/StatusNotifierWatcher"))
	v, err := obj.GetProperty("org.kde.StatusNotifierWatcher.IsStatusNotifierHostRegistered")
	if err != nil {
		return false
	}
	registered, ok := v.Value().(bool)
	return ok && registered
}
