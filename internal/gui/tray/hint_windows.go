//go:build windows

package tray

// The agent-start command used to live here too, as a third per-OS copy of
// what internal/platform/service already owned. When #520 moved macOS to a
// system LaunchDaemon this copy kept naming the deleted per-user job; it is
// now service.StartHintFor(goos), table-tested across all three OSes.

// checkLogsHint is shown when the tunnel reports an error state and
// the user should look at the daemon's log to diagnose.
//
// It names the agent's own log file rather than the Event Log query it
// used to: internal/platform/logsink mirrors Warn and above to the
// 'waired-agent' Event Log source, so the query answers "was there an
// error" but never "what was the daemon doing" (#636). The file under the
// state dir holds the whole stream. ASCII only — a redirected PowerShell
// pipeline decodes child output with the console's ANSI code page.
func checkLogsHint() string {
	return `Check %ProgramData%\waired\logs\waired-agent.log`
}

// claudeEnableHint is the OS-correct command to route Claude Code through
// Waired's local gateway. Windows writes managed-settings under
// C:\Program Files\ClaudeCode, which needs elevation — there is no sudo, so
// the hint says to run it from an elevated (Administrator) shell (#650).
func claudeEnableHint() string {
	return "run `waired claude enable` as Administrator"
}
