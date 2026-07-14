package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// enrolledSysDir writes a minimal identity.json into a fresh temp dir and
// returns it — a stand-in for a readable, enrolled SYSTEM state dir.
func enrolledSysDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity.json"),
		[]byte(`{"device_id":"did_x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestResolveSystemFallbackAt covers the portable outcomes of the System-dir
// fallback: a readable enrolled dir renders, an absent dir and a
// resolved==system override both decline. The permission → notice path needs
// an actually-unreadable dir and lives in statehint_perm_unix_test.go.
func TestResolveSystemFallbackAt(t *testing.T) {
	t.Run("readable enrolled system dir → render", func(t *testing.T) {
		sys := enrolledSysDir(t)
		dir, id, notice := resolveSystemFallbackAt(t.TempDir(), sys, "waired status", "windows")
		if id == nil {
			t.Fatalf("want non-nil identity from a readable enrolled dir; got dir=%q notice=%q", dir, notice)
		}
		if dir != sys {
			t.Errorf("dir = %q, want the system dir %q", dir, sys)
		}
		if notice != "" {
			t.Errorf("notice = %q, want empty on the render path", notice)
		}
	})

	t.Run("absent system dir → not enrolled", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "gone")
		dir, id, notice := resolveSystemFallbackAt(t.TempDir(), gone, "waired status", "linux")
		if id != nil || dir != "" || notice != "" {
			t.Errorf(`want ("", nil, "") for an absent dir; got (%q, %v, %q)`, dir, id, notice)
		}
	})

	t.Run("resolved == system (override) → no fallback", func(t *testing.T) {
		same := enrolledSysDir(t) // even enrolled: identical paths ⇒ no distinct system dir
		dir, id, notice := resolveSystemFallbackAt(same, same, "waired status", "linux")
		if id != nil || dir != "" || notice != "" {
			t.Errorf(`want ("", nil, "") when resolved==system; got (%q, %v, %q)`, dir, id, notice)
		}
	})
}

// TestSystemEnrolledElevationNotice pins the OS-aware wording of the
// "enrolled system-wide, needs elevation" notice across all three GOOS values
// (the CLAUDE.md 3-value table-test rule for runtime.GOOS-routed copy).
func TestSystemEnrolledElevationNotice(t *testing.T) {
	const sys = `/var/lib/waired`
	for _, tc := range []struct {
		goos string
		want string
	}{
		{"linux", "Run `sudo waired status`"},
		{"darwin", "Run `sudo waired status`"},
		{"windows", "Administrator"},
	} {
		got := systemEnrolledElevationNotice(sys, "waired status", tc.goos)
		if !strings.Contains(got, "enrolled system-wide") {
			t.Errorf("goos=%s: notice = %q, want it to mention enrolled system-wide", tc.goos, got)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("goos=%s: notice = %q, want it to contain %q", tc.goos, got, tc.want)
		}
	}
}
