//go:build linux

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLinuxEnableWritesWairedDisplayName pins waired#810: the runtime
// autostart entry must show "Waired" to the user (Name=) while keeping the
// appName-derived filename (waired-tray.desktop) that installed systems and
// the Disable() path depend on.
func TestLinuxEnableWritesWairedDisplayName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	mgr := newManager("waired-tray")
	if err := mgr.Enable("/opt/waired/bin/waired-tray", []string{"--tray"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// Filename basename stays appName-derived.
	path := filepath.Join(tmp, "autostart", "waired-tray.desktop")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected %s to be written: %v", path, err)
	}
	body := string(raw)

	if !strings.Contains(body, "\nName=Waired\n") {
		t.Errorf("autostart entry Name= must be the user-facing \"Waired\"; got:\n%s", body)
	}
	if strings.Contains(body, "Name=waired-tray") {
		t.Errorf("autostart entry must not surface the internal binary name to users; got:\n%s", body)
	}
	if !strings.Contains(body, "Exec=/opt/waired/bin/waired-tray --tray") {
		t.Errorf("Exec line missing/incorrect; got:\n%s", body)
	}
}
