//go:build linux

package trayhost

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/waired-ai/waired-agent/internal/platform/browser"
)

// Check probes the live session for an SNI host and classifies the desktop.
// Read-only: it makes one D-Bus property read and a few env lookups, never
// mutating anything.
func Check() Result {
	return evaluate(facts{
		hasDisplay:     browser.HasDisplay(),
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
		"back in on Wayland. `waired doctor --fix` does all of this for you."
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

func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland"
}

func detectDesktop() Desktop {
	if !browser.HasDisplay() {
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

// MenuLabels reports which dialect this session's tray menu has to be written
// in. Read-only: one D-Bus method call and one /proc read, both of which
// answer "spec" on any failure.
//
// The question is "who will draw our menu", and the honest way to ask it is to
// find out who owns the StatusNotifierWatcher name — the process our tray item
// is handed to. Environment variables cannot answer it: waired-tray is started
// from an XDG autostart entry, and on the fleet's GNOME host the running
// process's /proc/<pid>/environ carries XDG_RUNTIME_DIR and nothing else — no
// XDG_CURRENT_DESKTOP, no DESKTOP_SESSION. detectDesktop() would say "other"
// there and we would write the wrong markup on the one desktop we know needs
// the other kind (waired-agent#1100).
//
// A renderer we do not recognise gets spec-correct markup, which is the right
// default in both directions: KDE, XFCE, Waybar and snixembed all implement
// the spec, and a future host that does too needs no entry here.
func MenuLabels() MenuDialect { return menuDialectFor(sniHostComm()) }

// menuDialectFor maps the drawing process's name onto the dialect. Pure, so
// the table is testable without a session bus.
//
// gnome-shell is the whole list. Its AppIndicator extension runs inside the
// shell process, so the shell owning the watcher name IS the extension drawing
// the menu; both known uuids (appindicatorsupport@rgcjonas.gmail.com and
// Ubuntu's renamed ubuntu-appindicators@ubuntu.com) are the same code. GNOME's
// own "Status Icons" extension speaks XEmbed rather than SNI, so it never owns
// this name.
func menuDialectFor(comm string) MenuDialect {
	if comm == "gnome-shell" {
		return MenuDialectGnomeShell
	}
	return MenuDialectSpec
}

// sniHostComm returns the process name behind org.kde.StatusNotifierWatcher,
// or "" when the session bus, the name or the process cannot be read. Uses a
// private connection so a short-lived caller leaves no dangling bus name, the
// same shape as sniHostRegistered.
func sniHostComm() string {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()

	var pid uint32
	if err := conn.BusObject().Call(
		"org.freedesktop.DBus.GetConnectionUnixProcessID", 0,
		"org.kde.StatusNotifierWatcher",
	).Store(&pid); err != nil {
		return ""
	}
	// /proc/<pid>/comm is the 15-character command name — long enough for
	// "gnome-shell" (11) and for every other host name we might grow a case
	// for. cmdline would carry the full path but also the arguments, and the
	// name is what identifies the renderer.
	b, err := os.ReadFile(filepath.Join("/proc", strconv.FormatUint(uint64(pid), 10), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
