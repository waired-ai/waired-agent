// Package notification surfaces short, transient OS-level toast
// notifications. Used by the tray to confirm state transitions
// (connected → disconnected, OpenCode reconfigure success/failure)
// without modal dialogs.
//
// Backends are best-effort: Linux uses `notify-send` (libnotify),
// which may be absent on minimal desktops; Windows uses
// Shell_NotifyIcon balloon tips, which the Action Center may
// surface as toast on Windows 10+; macOS is a stub for now.
// A failed Notify is silent — callers MUST NOT treat it as an error
// condition, since notifications are advisory.
package notification

// Level is a coarse classification used to pick an icon and the
// urgency hint passed to libnotify. Backends without distinct icons
// map all levels to a single style.
type Level int

const (
	Info Level = iota
	Warning
	Error
)

// Notifier displays a transient toast notification. Implementations
// are safe to use from a single goroutine; concurrent calls are not
// guaranteed serialised.
type Notifier interface {
	// Notify shows title + body at the given level. Returns nil even
	// when the underlying backend is missing or fails to render —
	// callers do not have a fallback path, and forcing them to handle
	// notify errors would clutter the call sites. Backends MAY return
	// an error for programmer-error inputs (empty title) only.
	Notify(title, body string, level Level) error
}

// New returns the default Notifier for the current OS.
func New() Notifier {
	return newNotifier()
}
