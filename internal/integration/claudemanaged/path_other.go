//go:build !linux && !darwin && !windows

package claudemanaged

// managedSettingsPath returns "" on platforms with no known Claude Code
// managed-settings location; Write then reports ErrUnsupportedOS and the
// integration degrades to skills-only.
func managedSettingsPath() string { return "" }
