//go:build linux

package claudemanaged

// managedSettingsPath is the system-wide Claude Code managed-settings file on
// Linux. Owned by root; the waired init / `waired claude enable` root phase
// writes it.
func managedSettingsPath() string { return "/etc/claude-code/managed-settings.json" }
