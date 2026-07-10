// Package trayhost reports whether the current desktop session can render a
// StatusNotifierItem (SNI) system-tray icon — the protocol waired-tray speaks
// via fyne.io/systray on Linux.
//
// SNI only *publishes* a tray item onto the session bus; a separate **SNI host**
// (a.k.a. StatusNotifierHost) must be present to actually draw it. KDE Plasma
// ships a host built in. GNOME does not: its legacy XEmbed tray was removed in
// GNOME 3.26 and it never shipped an SNI host, so on GNOME the icon renders only
// when an AppIndicator host extension is installed and enabled
// (appindicatorsupport@rgcjonas.gmail.com, or Ubuntu's
// ubuntu-appindicators@ubuntu.com). MATE cannot render SNI at all. This package
// powers the `waired doctor` finding that tells the operator when the tray icon
// will silently never appear, and what to do about it (issue #493).
//
// Check is per-OS: the real probe lives in trayhost_linux.go (session-bus query
// + desktop-environment detection); every other platform returns NotApplicable
// (the tray host is a Linux-desktop concern only).
package trayhost

// Status is the high-level verdict of Check.
type Status int

const (
	// NotApplicable means the question doesn't apply here: a non-Linux OS, or
	// a Linux host with no graphical session (a headless server). Callers
	// should emit no finding.
	NotApplicable Status = iota
	// HostPresent means an SNI host is registered on the session bus, so the
	// waired-tray icon will render.
	HostPresent
	// NoHost means there is a graphical session but no SNI host, so the icon
	// will not appear until one is installed/enabled. Hint explains the fix.
	NoHost
	// Unsupported means the desktop cannot render SNI tray icons at all (MATE).
	// Hint explains the alternative.
	Unsupported
)

// Desktop is the detected desktop environment.
type Desktop int

const (
	DesktopUnknown Desktop = iota
	DesktopNone            // no graphical session
	DesktopGNOME
	DesktopKDE
	DesktopMATE
	DesktopOther
)

func (d Desktop) String() string {
	switch d {
	case DesktopGNOME:
		return "GNOME"
	case DesktopKDE:
		return "KDE"
	case DesktopMATE:
		return "MATE"
	case DesktopOther:
		return "other"
	case DesktopNone:
		return "none"
	default:
		return "unknown"
	}
}

// Result is the outcome of Check.
type Result struct {
	Status  Status
	Desktop Desktop
	Wayland bool
	// Hint is a one-line, actionable message for NoHost / Unsupported; empty
	// otherwise.
	Hint string
}
