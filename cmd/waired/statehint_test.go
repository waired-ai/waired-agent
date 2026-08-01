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

// TestUnreadableSystemStateNoticeAt: a status query against an unreadable
// SYSTEM dir is informational (waired#751), and against any other
// unreadable dir it is not — an explicit --state-dir that cannot be read
// is a real error, and calling it "enrolled system-wide" would send the
// operator to elevate a prompt that would still fail.
//
// Reachable since waired-agent#313 made an elevated CLI target the System
// dir: "elevated" does not imply "can read the service's ACL'd tree" —
// a filtered/basic token (runas /trustlevel:0x20000) still reports
// TokenIsElevated.
func TestUnreadableSystemStateNoticeAt(t *testing.T) {
	const sys = `C:\ProgramData\waired`
	notice, ok := unreadableSystemStateNoticeAt(sys, sys, "waired status", "windows")
	if !ok {
		t.Fatal("an unreadable System dir did not produce a notice")
	}
	for _, want := range []string{sys, "elevation", "waired status"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing %q: %q", want, notice)
		}
	}
	if _, ok := unreadableSystemStateNoticeAt(`D:\elsewhere`, sys, "waired status", "windows"); ok {
		t.Error("an unreadable explicit --state-dir was reported as a system-wide enrollment")
	}
}
