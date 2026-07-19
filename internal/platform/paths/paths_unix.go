//go:build linux || darwin

package paths

import (
	"os"
	"path/filepath"
)

// sunPathBudget caps how long an instance socket path may get. The kernel's
// sockaddr_un.sun_path is 108 bytes on Linux and only 104 on darwin, and
// bind() fails outright past it. 100 leaves room for the NUL and keeps one
// threshold for both Unixes rather than a per-OS constant that only one of
// them ever exercises.
const sunPathBudget = 100

// osInstanceMgmtEndpoint returns the management write socket for an instance
// whose state dir is stateDir (already known to be non-default), or "" when
// stateDir is a default one.
//
// The socket normally sits directly inside the state dir: discoverable
// (`ls $WAIRED_STATE_DIR`), removed with it, and — because a non-default
// state dir means a dev/test instance where daemon and client are the same
// user — reachable despite the state dir's own restrictive mode. Only when
// that would overrun sun_path does it fall back to a hashed path under the
// temp dir, which is short regardless of how deep the state dir is.
func osInstanceMgmtEndpoint(stateDir string) string {
	dir := nonDefaultStateDir(stateDir)
	if dir == "" {
		return ""
	}
	if p := filepath.Join(dir, "mgmt.sock"); len(p) <= sunPathBudget {
		return p
	}
	return filepath.Join(os.TempDir(), "waired-"+stateDirHash(dir), "mgmt.sock")
}
