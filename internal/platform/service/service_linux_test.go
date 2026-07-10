//go:build linux

package service

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInstalledDetectsAllUnitDirs guards the #335-regression fix: Installed()
// must return true when the unit lives in the .deb location
// (/lib/systemd/system), not only the CLI-managed /etc/systemd/system.
// Before the fix it stat'd /etc only, so every .deb install reported
// "not installed" → FixStateOwnership no-op'd → a root `waired init` left the
// identity/secrets root-owned and the waired daemon could not read them.
func TestInstalledDetectsAllUnitDirs(t *testing.T) {
	etc := t.TempDir()
	lib := t.TempDir()
	usrLib := t.TempDir()
	orig := linuxUnitDirs
	linuxUnitDirs = []string{etc, lib, usrLib}
	t.Cleanup(func() { linuxUnitDirs = orig })

	if Installed() {
		t.Fatal("Installed() = true with no unit file present")
	}

	// The .deb ships the unit to /lib/systemd/system only.
	debUnit := filepath.Join(lib, ServiceName+".service")
	if err := os.WriteFile(debUnit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Installed() {
		t.Errorf("Installed() = false with the unit at the .deb dir %s", debUnit)
	}

	// Removing the only unit → false again.
	if err := os.Remove(debUnit); err != nil {
		t.Fatal(err)
	}
	if Installed() {
		t.Error("Installed() = true after removing the only unit file")
	}

	// The CLI-managed location still counts.
	cliUnit := filepath.Join(etc, ServiceName+".service")
	if err := os.WriteFile(cliUnit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Installed() {
		t.Errorf("Installed() = false with the unit at the CLI dir %s", cliUnit)
	}
}
