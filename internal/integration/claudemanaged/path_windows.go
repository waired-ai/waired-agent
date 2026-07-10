//go:build windows

package claudemanaged

import (
	"os"
	"path/filepath"
)

// managedSettingsPath is the system-wide Claude Code managed-settings file on
// Windows: %ProgramFiles%\ClaudeCode\managed-settings.json (ProgramFiles
// resolved from the environment, falling back to the conventional location).
func managedSettingsPath() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "ClaudeCode", "managed-settings.json")
}
