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

func (s Status) String() string {
	switch s {
	case HostPresent:
		return "host-present"
	case NoHost:
		return "no-host"
	case Unsupported:
		return "unsupported"
	default:
		return "not-applicable"
	}
}

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

// MenuDialect says how a menu label has to be WRITTEN so that the process
// drawing it renders the text we meant.
//
// The dbusmenu `label` property is markup, not text: "two consecutive
// underscore characters `__` are displayed as a single underscore, any
// remaining underscore characters are not displayed at all, the first of
// those remaining underscore characters (unless it is the last character in
// the string) indicates that the following character is the access key"
// (com.canonical.dbusmenu.xml). Escaping is therefore mandatory for any label
// carrying an underscore — a model tag, an email local part, a home
// directory, a device id (waired-agent#1100).
//
// The complication is that one widely deployed renderer does not implement
// that rule, so "the escaped string" is not one string. This type is how the
// caller says which one it needs; internal/gui/tray's escapeMenuLabel is the
// table that turns it into characters.
type MenuDialect int

const (
	// MenuDialectSpec is the specification's own rule, and the answer
	// whenever we do not positively recognise a renderer that departs from
	// it. It is what Plasma implements (plasma-workspace vendors
	// libdbusmenuqt/utils.cpp's swapMnemonicChar unchanged), what
	// libdbusmenu-gtk implements — and therefore what xfce4-panel, Waybar
	// and snixembed do — and what Chromium and KF6 emit from the writing
	// side. Nothing is lost by defaulting here: a renderer that follows the
	// spec draws spec-correct markup correctly.
	MenuDialectSpec MenuDialect = iota

	// MenuDialectGnomeShell is gnome-shell-extension-appindicator, the only
	// SNI host GNOME has. Its dbusMenu.js does
	//
	//	propertyGet('label').replace(/_([^_])/, '$1')
	//
	// with no `g` flag, so it deletes exactly one underscore — the first one
	// followed by a non-underscore — and never collapses `__`. That line has
	// been byte-identical since the extension's first commit in 2013 and is
	// shipped unpatched by Debian and Ubuntu, and ubuntu-appindicators is the
	// same upstream code under a renamed uuid, so this is not a version-skew
	// window: it is what GNOME does.
	MenuDialectGnomeShell
)

func (d MenuDialect) String() string {
	if d == MenuDialectGnomeShell {
		return "gnome-shell"
	}
	return "spec"
}
