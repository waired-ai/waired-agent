//go:build darwin

package paths

import (
	"os"
	"path/filepath"
)

// macOS convention: ~/Library/Application Support/waired for per-user
// state, /Library/Application Support/waired for the system-wide install.
// The XDG fallback is intentionally NOT used here — darwin GUI apps
// expect Library/ layout and macOS service plumbing (launchd) writes
// state under /Library when running as a system daemon.
//
// AutoDetect keys on euid (the darwin analog of the Windows SCM probe):
// the waired-agent system LaunchDaemon runs as root and must resolve to
// the system dir, while a raw-binary dev run or the per-user tray (run
// as the human user) resolves to ~/Library. The service installer and
// `sudo waired init` select System explicitly via initStateDirMode.
func osStateDir(m Mode) string {
	switch m {
	case System:
		return "/Library/Application Support/waired"
	case Interactive:
		return userStateDir()
	case AutoDetect:
		if os.Geteuid() == 0 {
			return "/Library/Application Support/waired"
		}
		return userStateDir()
	default:
		return "/Library/Application Support/waired"
	}
}

func userStateDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "Library", "Application Support", "waired")
	}
	return "/Library/Application Support/waired"
}

// osMgmtEndpoint returns the management write-socket path. System uses
// /var/run/waired — root-writable, short enough for the 104-byte darwin
// sun_path limit, and (unlike the long, 0700 /Library/Application Support
// state dir) traversable by the desktop user. Interactive/dev keeps the
// socket beside the per-user state dir (same user as the daemon, so 0700
// is not a cross-user barrier there). AutoDetect keys on euid==0 like
// osStateDir. macOS has no XDG_RUNTIME_DIR.
func osMgmtEndpoint(m Mode) string {
	if v := os.Getenv(MgmtSocketEnvOverride); v != "" {
		return v
	}
	switch m {
	case System:
		return "/var/run/waired/mgmt.sock"
	case Interactive:
		return filepath.Join(userStateDir(), "run", "mgmt.sock")
	case AutoDetect:
		if os.Geteuid() == 0 {
			return "/var/run/waired/mgmt.sock"
		}
		return filepath.Join(userStateDir(), "run", "mgmt.sock")
	default:
		return "/var/run/waired/mgmt.sock"
	}
}
