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

// MgmtSocketEnvOverride is the env var that, when non-empty, replaces the
// computed management write endpoint on Linux/macOS (a unix-domain socket
// path). Mirrors EnvOverride for tests, the e2e harness, and dev runs
// where the daemon and the CLI/tray must agree on a non-default path.
// Ignored on Windows, which uses a fixed named-pipe name.
const MgmtSocketEnvOverride = "WAIRED_MGMT_SOCKET"

// MgmtEndpoint returns the local IPC endpoint the Local Management API
// serves mutating (write) requests on: a unix-domain socket path on
// Linux/macOS, or a named-pipe name (e.g. \\.\pipe\waired-mgmt) on
// Windows. Unlike the state dir it deliberately lives in a runtime
// directory that the desktop user can traverse — NOT under the 0700
// secrets-bearing state dir — because the tray and CLI run as the desktop
// user while the daemon runs as a service user (Linux waired, macOS root,
// Windows LocalSystem). Browsers and network peers cannot open a unix
// socket / named pipe, which is the point (waired#838).
//
// The path is not created here — platform/localipc.Listen creates the
// parent runtime dir. $WAIRED_MGMT_SOCKET overrides it on Linux/macOS.
func MgmtEndpoint(m Mode) string {
	return osMgmtEndpoint(m)
}
