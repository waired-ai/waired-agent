//go:build darwin

package tray

// startAgentHint is the one-line command shown in the "agent not
// running" status row. After `waired-agent install` has run the
// LaunchAgent plist at ~/Library/LaunchAgents/com.waired.agent.plist,
// kicking the job back to life is one launchctl call away — no
// privilege escalation needed because the LaunchAgent lives in the
// user's gui/<uid> domain (see internal/platform/service/service_darwin.go).
//
// We expose the kickstart form rather than `launchctl load` because
// load/unload are deprecated in modern launchd (10.10+) in favour of
// the bootstrap/bootout pair used by Install/Uninstall; kickstart is
// the right hammer when the job is already bootstrapped and just
// needs to be (re-)started.
func startAgentHint() string {
	return "launchctl kickstart -k gui/$(id -u)/com.waired.agent"
}

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
