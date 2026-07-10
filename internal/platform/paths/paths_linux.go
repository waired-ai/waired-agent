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
