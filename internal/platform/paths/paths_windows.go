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
//     user-profile changes. On a service install the whole dir
//     (identity.json included, not just secrets/) is locked to
//     SYSTEM + Administrators via platform/secrets.SecureDir — see
//     service/service_windows.go — so a non-elevated user cannot read
//     it. A status query falls back to an informational notice there
//     rather than reading the enrollment (waired#751).
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

// osMgmtEndpoint returns the management write endpoint, a named pipe.
// There is no unix-socket-style filesystem node on Windows; the service
// install runs one daemon per machine so a fixed name is fine there, and
// the pipe DACL (see internal/platform/localipc) restricts connect to local
// principals. m is unused because the pipe namespace is machine-global, not
// per-user, and $WAIRED_MGMT_SOCKET does not apply on Windows.
//
// A non-default stateDir (a dev/test instance) gets its own pipe name, since
// the namespace being machine-global is exactly what makes two instances
// collide (waired#81). The name cannot live "inside" the state dir the way a
// unix socket does, so it carries a short digest of it instead.
func osMgmtEndpoint(m Mode, stateDir string) string {
	_ = m
	if p := osInstanceMgmtEndpoint(stateDir); p != "" {
		return p
	}
	return `\\.\pipe\waired-mgmt`
}

// osInstanceMgmtEndpoint is the Windows half of InstanceMgmtEndpoint. A pipe
// name cannot live "inside" the state dir the way a unix socket does, so it
// carries a short digest of the state dir instead.
func osInstanceMgmtEndpoint(stateDir string) string {
	dir := nonDefaultStateDir(stateDir)
	if dir == "" {
		return ""
	}
	return `\\.\pipe\waired-mgmt-` + stateDirHash(dir)
}
