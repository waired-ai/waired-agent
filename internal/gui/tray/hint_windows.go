//go:build windows

package tray

// startAgentHint is the one-line command shown in the "agent not
// running" status row. PowerShell Start-Service is the lowest-
// friction form for a user who has the Waired Agent SCM service
// installed (cmd/waired-agent install). It works from a
// non-elevated PowerShell as long as the user is in
// Administrators; if not, "Run as Administrator" is the obvious
// next step but the hint stays short on purpose.
func startAgentHint() string {
	return "Start-Service waired-agent"
}

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
