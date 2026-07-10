//go:build darwin

package autostart

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
)

// darwinManager registers the tray as a per-user LaunchAgent so it
// runs at login. The agent daemon's autostart story is handled by
// internal/platform/service.service_darwin (a separate LaunchAgent
// labelled com.waired.agent); this package only takes care of the
// tray, labelled com.waired.tray.<appName> where <appName> lets a
// single host theoretically register two trays (e.g. a dev build
// alongside a release build). In practice appName is always
// "waired-tray".

type darwinManager struct {
	appName string
}

func newManager(appName string) Manager {
	return darwinManager{appName: appName}
}

// runLaunchctlFn is overridden in tests so we can assert the argv
// without actually exec-ing launchctl (which would mutate the user's
// real session).
var runLaunchctlFn = runLaunchctlReal

func (m darwinManager) label() string {
	// Use a reverse-DNS form for the launchd label; including the
	// appName lets dev/release builds coexist on one machine.
	return "com.waired.tray." + m.appName
}

func (m darwinManager) plistPath() (string, error) {
	if m.appName == "" {
		return "", errors.New("autostart: empty appName")
	}
	home, err := userHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", m.label()+".plist"), nil
}

func (m darwinManager) Enable(programPath string, args []string) error {
	if programPath == "" {
		return errors.New("autostart: empty programPath")
	}
	plistPath, err := m.plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(plistPath), err)
	}

	body := renderTrayPlist(m.label(), programPath, args)
	if err := os.WriteFile(plistPath, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", plistPath, err)
	}

	uid, err := currentUID()
	if err != nil {
		return err
	}
	// Idempotent: bootout any stale registration first (ignore error
	// — a missing job is fine), then bootstrap the fresh plist.
	_, _, _ = runLaunchctlFn([]string{"bootout", fmt.Sprintf("gui/%d/%s", uid, m.label())})
	if _, stderr, err := runLaunchctlFn([]string{
		"bootstrap", fmt.Sprintf("gui/%d", uid), plistPath,
	}); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w (stderr=%q)", err, string(bytes.TrimSpace(stderr)))
	}
	return nil
}

func (m darwinManager) Disable() error {
	plistPath, err := m.plistPath()
	if err != nil {
		return err
	}
	uid, uidErr := currentUID()
	if uidErr == nil {
		// Best-effort: a missing job is fine.
		_, _, _ = runLaunchctlFn([]string{"bootout", fmt.Sprintf("gui/%d/%s", uid, m.label())})
	}
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", plistPath, err)
	}
	return nil
}

func (m darwinManager) IsEnabled() (bool, error) {
	plistPath, err := m.plistPath()
	if err != nil {
		return false, err
	}
	// File presence is the persistent signal — even if a previous
	// `launchctl bootout` ran in the current session, RunAtLoad will
	// fire on next login as long as the plist file is there.
	// Mirrors the Linux model where .desktop file presence == enabled.
	if _, err := os.Stat(plistPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// renderTrayPlist emits a minimal LaunchAgent plist for the tray:
// RunAtLoad=true (so it starts at login), KeepAlive=false (Quit menu
// must actually quit; we do not want launchd to respawn it), and a
// dedicated log path under $HOME/Library/Logs.
func renderTrayPlist(label, programPath string, args []string) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")

	writeKeyString(&b, "Label", label)

	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	b.WriteString("    <string>")
	_ = xml.EscapeText(&b, []byte(programPath))
	b.WriteString("</string>\n")
	for _, a := range args {
		b.WriteString("    <string>")
		_ = xml.EscapeText(&b, []byte(a))
		b.WriteString("</string>\n")
	}
	b.WriteString("  </array>\n")

	writeKeyBool(&b, "RunAtLoad", true)
	writeKeyBool(&b, "KeepAlive", false)
	writeKeyString(&b, "ProcessType", "Interactive")
	if home, err := userHome(); err == nil {
		writeKeyString(&b, "StandardOutPath", filepath.Join(home, "Library", "Logs", "waired-tray.out.log"))
		writeKeyString(&b, "StandardErrorPath", filepath.Join(home, "Library", "Logs", "waired-tray.err.log"))
	}

	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func writeKeyString(b *bytes.Buffer, key, value string) {
	b.WriteString("  <key>")
	_ = xml.EscapeText(b, []byte(key))
	b.WriteString("</key>\n  <string>")
	_ = xml.EscapeText(b, []byte(value))
	b.WriteString("</string>\n")
}

func writeKeyBool(b *bytes.Buffer, key string, value bool) {
	b.WriteString("  <key>")
	_ = xml.EscapeText(b, []byte(key))
	b.WriteString("</key>\n  ")
	if value {
		b.WriteString("<true/>\n")
	} else {
		b.WriteString("<false/>\n")
	}
}

func currentUID() (int, error) {
	u, err := user.Current()
	if err != nil {
		return 0, fmt.Errorf("user.Current: %w", err)
	}
	return strconv.Atoi(u.Uid)
}

func userHome() (string, error) {
	if h := os.Getenv("HOME"); h != "" {
		return h, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

func runLaunchctlReal(args []string) ([]byte, []byte, error) {
	cmd := exec.Command("/bin/launchctl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
