//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unreadableStateDir builds an enrolled-looking state dir the current
// (non-root) user cannot read — the shape of /var/lib/waired on a
// service install.
func unreadableStateDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"),
		[]byte(`{"device_id":"did_x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return dir
}

// TestRunStatusBodyPermissionDenied pins the fix for the original bug
// report: a state dir the user cannot read must NOT come back as "Not
// enrolled" — it is a permission problem with a sudo hint.
func TestRunStatusBodyPermissionDenied(t *testing.T) {
	dir := unreadableStateDir(t)
	err := runStatusBody(defaultMgmtURL, dir, false, "")
	if err == nil {
		t.Fatal("runStatusBody succeeded against an unreadable state dir")
	}
	if !strings.Contains(err.Error(), "permission denied reading state in "+dir) ||
		!strings.Contains(err.Error(), "sudo waired status") {
		t.Errorf("err = %v, want permission-denied + sudo hint", err)
	}
	if strings.Contains(err.Error(), "Not enrolled") {
		t.Errorf("err = %v, must not claim not-enrolled", err)
	}
}

func TestRunAuthStatusBodyPermissionDenied(t *testing.T) {
	dir := unreadableStateDir(t)
	err := runAuthStatusBody(dir)
	if err == nil {
		t.Fatal("runAuthStatusBody succeeded against an unreadable state dir")
	}
	if !strings.Contains(err.Error(), "permission denied reading state in "+dir) ||
		!strings.Contains(err.Error(), "sudo waired auth status") {
		t.Errorf("err = %v, want permission-denied + sudo hint", err)
	}
}
