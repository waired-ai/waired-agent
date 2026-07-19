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
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	return MgmtEndpointFor(m, StateDir(m))
}

// MgmtEndpointFor is MgmtEndpoint for an instance whose state dir is
// stateDir. When stateDir is NOT one of this OS's default state dirs — i.e.
// a dev or test instance started with $WAIRED_STATE_DIR — the endpoint moves
// beside it so two agents on one machine cannot collide on the machine-wide
// runtime path (waired#81). Resolution order:
//
//  1. $WAIRED_MGMT_SOCKET (Linux/macOS only)
//  2. an instance-specific endpoint derived from a NON-DEFAULT stateDir
//  3. the mode-derived default (/run/waired/mgmt.sock, \\.\pipe\waired-mgmt, …)
//
// Step 2 is deliberately gated on the state dir being non-default. Every
// caller that exports $WAIRED_STATE_DIR pointing AT the OS default — the
// install tests and the routing sentinel all do — must keep resolving to the
// same runtime endpoint the service daemon binds, because there the daemon is
// started by systemd/launchd/the SCM and never sees that variable at all.
// Dropping the gate silently moves only the client and breaks every leg.
//
// Daemon and client derive this independently, so a dev instance must export
// the same $WAIRED_STATE_DIR to both (the documented dev pattern).
func MgmtEndpointFor(m Mode, stateDir string) string {
	return osMgmtEndpoint(m, stateDir)
}

// InstanceMgmtEndpoint returns the instance-specific management endpoint
// derived from stateDir, or "" when stateDir is empty or is one of this OS's
// default state dirs. Clients use it to tell "this really is a separate
// instance" apart from "$WAIRED_STATE_DIR happens to name the default dir",
// which the install tests and routing sentinel all do.
func InstanceMgmtEndpoint(stateDir string) string {
	return osInstanceMgmtEndpoint(stateDir)
}

// nonDefaultStateDir returns stateDir cleaned when it is NOT one of this
// OS's conventional state dirs, else "". Compared against osStateDir, never
// StateDir: StateDir returns $WAIRED_STATE_DIR verbatim, so using it here
// would make every overridden dir compare equal to the "default" and the
// waired#81 branch would never fire.
func nonDefaultStateDir(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	for _, def := range []string{osStateDir(System), osStateDir(Interactive)} {
		if samePath(runtime.GOOS, stateDir, def) {
			return ""
		}
	}
	return filepath.Clean(stateDir)
}

// samePath compares two filesystem paths the way goos does: Windows path
// comparison is case-insensitive, the Unixes' is not.
func samePath(goos, a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// stateDirHash is a short, stable, filesystem- and pipe-name-safe digest of
// a state dir, for endpoints that cannot simply live inside it (a Windows
// pipe name, or a unix socket whose in-place path would overrun sun_path).
func stateDirHash(stateDir string) string {
	norm := filepath.Clean(stateDir)
	if runtime.GOOS == "windows" {
		norm = strings.ToLower(norm)
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:12]
}
