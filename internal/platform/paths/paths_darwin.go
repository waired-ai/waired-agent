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
