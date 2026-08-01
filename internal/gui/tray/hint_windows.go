//go:build windows

package tray

// The agent-start command used to live here too, as a third per-OS copy of
// what internal/platform/service already owned. When #520 moved macOS to a
// system LaunchDaemon this copy kept naming the deleted per-user job; it is
// now service.StartHintFor(goos), table-tested across all three OSes.

// checkLogsHint is shown when the tunnel reports an error state and
// the user should look at the daemon's log to diagnose. The
// Phase W-1.5 logsink_windows wires waired-agent into the Windows
// Event Log under the 'waired-agent' source.
func checkLogsHint() string {
	return "Get-WinEvent -ProviderName waired-agent -MaxEvents 30"
}

// claudeEnableHint is the OS-correct command to route Claude Code through
// Waired's local gateway. Windows writes managed-settings under
// C:\Program Files\ClaudeCode, which needs elevation — there is no sudo, so
// the hint says to run it from an elevated (Administrator) shell (#650).
func claudeEnableHint() string {
	return "run `waired claude enable` as Administrator"
}
