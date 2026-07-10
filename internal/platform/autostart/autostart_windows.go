//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// defaultRunKeyPath is the Windows-canonical "run at user logon"
// registry location. HKCU avoids requiring admin to toggle and keeps
// the registration scoped to the current user.
const defaultRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

type windowsManager struct {
	appName string
	// keyPath is exposed for tests via NewForTest so unit tests can
	// point at an isolated sub-key (HKCU\Software\Waired\TestAutostart)
	// and not pollute the real Run key. Empty falls back to defaultRunKeyPath.
	keyPath string
}

func newManager(appName string) Manager {
	return &windowsManager{appName: appName}
}

// NewForTest returns a Manager that writes to keyPath under HKCU
// instead of the canonical Run key. Intended for unit tests; not
// re-exported in autostart.go.
func NewForTest(appName, keyPath string) Manager {
	return &windowsManager{appName: appName, keyPath: keyPath}
}

func (m *windowsManager) effectiveKeyPath() string {
	if m.keyPath != "" {
		return m.keyPath
	}
	return defaultRunKeyPath
}

func (m *windowsManager) validateName() error {
	if m.appName == "" {
		return errors.New("autostart: empty appName")
	}
	if strings.ContainsAny(m.appName, `\/`) {
		return fmt.Errorf("autostart: appName %q must not contain path separators", m.appName)
	}
	return nil
}

// quoteCommand renders the registry value text. Paths that may
// contain spaces (the canonical install dir is C:\Program Files\Waired\)
// MUST be quoted, otherwise CreateProcess interprets the first space
// as the end of the program. Each argument is wrapped in quotes too
// for the same reason; existing inner quotes are escaped with
// backslash per the standard Win32 cmdline convention.
func quoteCommand(programPath string, args []string) string {
	parts := []string{quoteArg(programPath)}
	for _, a := range args {
		parts = append(parts, quoteArg(a))
	}
	return strings.Join(parts, " ")
}

func quoteArg(a string) string {
	if !strings.ContainsAny(a, " \t\"") {
		return a
	}
	return `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
}

func (m *windowsManager) Enable(programPath string, args []string) error {
	if err := m.validateName(); err != nil {
		return err
	}
	if programPath == "" {
		return errors.New("autostart: empty programPath")
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, m.effectiveKeyPath(), registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open Run key: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(m.appName, quoteCommand(programPath, args)); err != nil {
		return fmt.Errorf("autostart: write %s: %w", m.appName, err)
	}
	return nil
}

func (m *windowsManager) Disable() error {
	if err := m.validateName(); err != nil {
		return err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, m.effectiveKeyPath(), registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("autostart: open Run key: %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(m.appName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("autostart: delete %s: %w", m.appName, err)
	}
	return nil
}

func (m *windowsManager) IsEnabled() (bool, error) {
	if err := m.validateName(); err != nil {
		return false, err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, m.effectiveKeyPath(), registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("autostart: open Run key: %w", err)
	}
	defer k.Close()
	_, _, err = k.GetStringValue(m.appName)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
