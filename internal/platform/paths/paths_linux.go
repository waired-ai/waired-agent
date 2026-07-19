//go:build linux

package paths

import (
	"os"
	"path/filepath"
)

func osStateDir(m Mode) string {
	switch m {
	case System:
		return "/var/lib/waired"
	case AutoDetect, Interactive:
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			return filepath.Join(x, "waired")
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, ".config", "waired")
		}
		return ".waired"
	default:
		return ".waired"
	}
}

// osMgmtEndpoint returns the management write-socket path. It lives in a
// runtime dir traversable by the desktop user, never under the 0700 state
// dir. System uses /run/waired (created by the systemd unit's
// RuntimeDirectory=waired, owned by the waired user, mode 0755, and
// removed on stop). Interactive/dev uses the per-user XDG runtime dir.
//
// A non-default stateDir wins over both: /run/waired is machine-wide, so
// two instances would otherwise fight over one socket (waired#81).
func osMgmtEndpoint(m Mode, stateDir string) string {
	if v := os.Getenv(MgmtSocketEnvOverride); v != "" {
		return v
	}
	if p := osInstanceMgmtEndpoint(stateDir); p != "" {
		return p
	}
	switch m {
	case System:
		return "/run/waired/mgmt.sock"
	case AutoDetect, Interactive:
		if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
			return filepath.Join(x, "waired", "mgmt.sock")
		}
		return filepath.Join(os.TempDir(), "waired", "mgmt.sock")
	default:
		return "/run/waired/mgmt.sock"
	}
}
