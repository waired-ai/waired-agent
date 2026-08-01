//go:build linux

package tray

// The agent-start command used to live here too, as a third per-OS copy of
// what internal/platform/service already owned. When #520 moved macOS to a
// system LaunchDaemon this copy kept naming the deleted per-user job; it is
// now service.StartHintFor(goos), table-tested across all three OSes.

// checkLogsHint is shown when the tunnel reports an error state and
// the user should look at the daemon's log to diagnose.
func checkLogsHint() string {
	return "Check `journalctl -u waired-agent`"
}

// claudeEnableHint is the OS-correct command to route Claude Code through
// Waired's local gateway. Linux/macOS need root (sudo); Windows needs an
// elevated shell. Surfaced in the tray's Claude status + routing submenu so
// the caveat text matches the platform (#650).
func claudeEnableHint() string {
	return "run `sudo waired claude enable`"
}
