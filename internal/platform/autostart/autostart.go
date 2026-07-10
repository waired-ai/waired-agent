// Package autostart owns the per-user "start on login" registration
// for the waired-tray. The agent itself runs as a system service
// (SCM / systemd), so its autostart story lives in
// internal/platform/service; this package only handles the tray,
// which must launch in the desktop user's context.
//
// Backends:
//   - Linux:   writes ~/.config/autostart/<appName>.desktop (XDG spec).
//   - Windows: writes HKCU\Software\Microsoft\Windows\CurrentVersion\Run.
//   - Darwin:  stub (macOS LaunchAgent integration is a follow-up).
//
// All operations are idempotent: Enable on an already-enabled entry
// overwrites the value (so a binary that moved still gets relaunched),
// Disable on a missing entry is a no-op success.
package autostart

// Manager toggles the platform-specific "run on login" registration.
type Manager interface {
	// Enable registers programPath (with optional args) to launch at
	// user login. If a registration already exists for the manager's
	// app name, it is replaced rather than duplicated.
	Enable(programPath string, args []string) error

	// Disable removes the registration for the manager's app name.
	// Missing registration is treated as success.
	Disable() error

	// IsEnabled reports whether a registration currently exists for
	// the manager's app name.
	IsEnabled() (bool, error)
}

// New returns the default Manager for the current OS. appName is the
// stable identifier under which the registration is filed (the
// .desktop filename on Linux, the Run-key value name on Windows).
// It must be filesystem-safe (no path separators); the convention is
// the binary's basename without extension (e.g. "waired-tray").
func New(appName string) Manager {
	return newManager(appName)
}
