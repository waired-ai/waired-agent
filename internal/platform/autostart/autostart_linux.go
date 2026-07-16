//go:build linux

package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// linuxDisplayName is the user-visible `Name=` written into the runtime
// autostart .desktop entry. It is intentionally decoupled from appName:
// appName ("waired-tray") remains the .desktop *filename* basename (and,
// on Windows/macOS, the HKCU Run value / LaunchAgent label that installed
// systems depend on), while end users should only ever see "Waired"
// (waired#810). Keep this in sync with the static entries under build/.
const linuxDisplayName = "Waired"

type linuxManager struct {
	appName string
}

func newManager(appName string) Manager {
	return linuxManager{appName: appName}
}

// desktopPath returns the XDG autostart spec location for this app.
// $XDG_CONFIG_HOME or ~/.config takes precedence; we never write to
// /etc/xdg/autostart (system-wide), which would require root.
func (m linuxManager) desktopPath() (string, error) {
	if m.appName == "" {
		return "", errors.New("autostart: empty appName")
	}
	if strings.ContainsAny(m.appName, "/\\") {
		return "", fmt.Errorf("autostart: appName %q must not contain path separators", m.appName)
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("autostart: UserHomeDir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "autostart", m.appName+".desktop"), nil
}

func (m linuxManager) Enable(programPath string, args []string) error {
	if programPath == "" {
		return errors.New("autostart: empty programPath")
	}
	path, err := m.desktopPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	exec := programPath
	for _, a := range args {
		exec += " " + a
	}
	body := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=%s
Exec=%s
X-GNOME-Autostart-enabled=true
NoDisplay=false
Terminal=false
`, linuxDisplayName, exec)
	return os.WriteFile(path, []byte(body), 0o644)
}

func (m linuxManager) Disable() error {
	path, err := m.desktopPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m linuxManager) IsEnabled() (bool, error) {
	path, err := m.desktopPath()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
