//go:build darwin

package tray

// The agent-start command used to live here too, as a third per-OS copy of
// what internal/platform/service already owned. When #520 moved macOS to a
// system LaunchDaemon this copy kept naming the deleted per-user job; it is
// now service.StartHintFor(goos), table-tested across all three OSes.

// checkLogsHint surfaces the canonical macOS unified-log query that
// shows the agent's last few minutes of output. The `process` matcher
// is more reliable than `sender` because the LaunchAgent's process
// name is `waired-agent` (no `sender` field is set unless the binary
// explicitly registers an os_log subsystem). For tail-style
// inspection users can also `tail -f ~/Library/Logs/waired-agent.err.log`,
// which is the StandardErrorPath set in the plist.
func checkLogsHint() string {
	return "log show --predicate 'process == \"waired-agent\"' --last 5m"
}

// claudeEnableHint is the OS-correct command to route Claude Code through
// Waired's local gateway. macOS writes managed-settings under
// /Library/Application Support/ClaudeCode, which needs root — hence sudo,
// same as Linux (#650).
func claudeEnableHint() string {
	return "run `sudo waired claude enable`"
}
