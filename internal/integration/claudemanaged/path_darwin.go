//go:build darwin

package claudemanaged

// managedSettingsPath is the system-wide Claude Code managed-settings file on
// macOS.
func managedSettingsPath() string {
	return "/Library/Application Support/ClaudeCode/managed-settings.json"
}
