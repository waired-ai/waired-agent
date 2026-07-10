//go:build linux

package tray

// startAgentHint is the one-line command shown in the "agent not
// running" status row. systemd-managed install is the canonical
// Linux deployment shape (.deb post-install registers the unit).
func startAgentHint() string {
	return "sudo systemctl start waired-agent"
}

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
