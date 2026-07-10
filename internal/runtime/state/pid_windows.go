//go:build windows

package state

import (
	"golang.org/x/sys/windows"
)

// stillActive is the Win32 STILL_ACTIVE constant returned by
// GetExitCodeProcess for a process that has not yet exited (0x103). A
// process that actually returned exit code 259 will be misreported as
// alive — accepted: such collisions are not part of our reliability
// budget (the agent re-enrolls on next start anyway).
const stillActive = 259

// pidAlive returns true if the given pid is a live process visible to
// us. On Windows the signal-0 trick doesn't work (no SIGTERM), so we
// open the process for query rights and probe its exit code instead.
// OpenProcess returns ERROR_INVALID_PARAMETER (or similar) for a pid
// that no longer exists, which we map to "dead".
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
