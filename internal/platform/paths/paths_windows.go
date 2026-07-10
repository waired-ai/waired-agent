//go:build windows

package paths

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/svc"
)

// osStateDir picks the conventional Windows state directory:
//
//   - System (or AutoDetect under the SCM): %ProgramData%\waired —
//     LocalSystem can reliably write here and the directory survives
//     user-profile changes. Default ACL leaves it readable for any
//     local user; service install applies a stricter DACL via
//     platform/secrets.SecureDir on the secrets/ subdir.
//   - Interactive (or AutoDetect outside the SCM): %AppData%\waired —
//     per-user roaming profile.
//
// $WAIRED_STATE_DIR has already been consulted by the caller.
func osStateDir(m Mode) string {
	detected := m
	if detected == AutoDetect {
		if isSvc, _ := svc.IsWindowsService(); isSvc {
			detected = System
		} else {
			detected = Interactive
		}
	}
	switch detected {
	case System:
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "waired")
		}
		return `C:\ProgramData\waired`
	case Interactive:
		if ad := os.Getenv("AppData"); ad != "" {
			return filepath.Join(ad, "waired")
		}
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "waired")
		}
		return `C:\ProgramData\waired`
	default:
		return `C:\ProgramData\waired`
	}
}
