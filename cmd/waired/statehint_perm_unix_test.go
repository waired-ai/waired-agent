//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveSystemFallbackAt_PermissionDenied: a locked (chmod 0) enrolled
// System dir yields the informational elevation notice, not a render — the
// standard-user path on a service install. Unix-only because os.Chmod(dir, 0)
// is a no-op on Windows; there the same locked-dir case is exercised by the
// NTFS DACL in the installtest #751 contract.
func TestResolveSystemFallbackAt_PermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	sys := t.TempDir()
	if err := os.WriteFile(filepath.Join(sys, "identity.json"),
		[]byte(`{"device_id":"did_x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sys, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sys, 0o700) })

	dir, id, notice := resolveSystemFallbackAt(t.TempDir(), sys, "waired status", "linux")
	if id != nil || dir != "" {
		t.Fatalf("want no render on a locked dir; got dir=%q id=%v", dir, id)
	}
	if !strings.Contains(notice, "enrolled system-wide") ||
		!strings.Contains(notice, "Run `sudo waired status`") {
		t.Errorf("notice = %q, want enrolled-system-wide + sudo hint", notice)
	}
}
