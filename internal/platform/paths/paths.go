// Package paths is the single source of truth for resolving the waired
// agent's on-disk state directory and its conventional subdirectories
// (secrets/, cache/) across Linux, Windows, and macOS.
//
// Previous to this package each command rolled its own resolver — the
// waired-agent daemon, the waired-tray binary, the integration package,
// and the agentconfig defaults each had subtly different XDG / HOME /
// ProgramData logic. They are now all routed through StateDir.
//
// The $WAIRED_STATE_DIR environment variable, when non-empty, always
// wins and is returned verbatim. After that resolution is delegated to
// the per-OS osStateDir function.
package paths

import (
	"os"
	"path/filepath"
)

// Mode selects between per-user (Interactive) and system-wide (System)
// state directory conventions.
type Mode int

const (
	// AutoDetect picks System on Windows when running under the SCM,
	// otherwise Interactive. Linux/Darwin currently always pick
	// Interactive in AutoDetect — system mode is selected explicitly by
	// the service installer.
	AutoDetect Mode = iota

	// Interactive returns the per-user state dir (e.g. ~/.config/waired
	// on Linux, %AppData%\waired on Windows, ~/Library/Application
	// Support/waired on macOS).
	Interactive

	// System returns the system-wide state dir (e.g. /var/lib/waired on
	// Linux, %ProgramData%\waired on Windows). Used by service installs.
	System
)

// EnvOverride is the env var name that, when non-empty, replaces the
// computed StateDir entirely. Exported so callers can document it.
const EnvOverride = "WAIRED_STATE_DIR"

// StateDir returns the canonical state directory for the current OS and
// the requested mode. $WAIRED_STATE_DIR (if non-empty) overrides
// everything else and is returned verbatim. The returned path is not
// guaranteed to exist on disk.
func StateDir(m Mode) string {
	if v := os.Getenv(EnvOverride); v != "" {
		return v
	}
	return osStateDir(m)
}

// SecretsDir returns <stateDir>/secrets. The directory is not created
// by this call — use platform/secrets.SecureDir for that.
func SecretsDir(stateDir string) string {
	return filepath.Join(stateDir, "secrets")
}

// CacheDir returns <stateDir>/cache.
func CacheDir(stateDir string) string {
	return filepath.Join(stateDir, "cache")
}
