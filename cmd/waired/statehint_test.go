package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemStateNoticeAt(t *testing.T) {
	enrolledSys := func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "identity.json"),
			[]byte(`{"device_id":"did_x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("enrolled system-wide", func(t *testing.T) {
		got := systemStateNoticeAt(t.TempDir(), enrolledSys(t), "waired status", "linux", 1000)
		if !strings.Contains(got, "enrolled system-wide") ||
			!strings.Contains(got, "Run `sudo waired status`") {
			t.Errorf("notice = %q, want enrolled-system-wide + sudo hint", got)
		}
	})

	t.Run("system dir unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses permission checks")
		}
		sys := enrolledSys(t)
		if err := os.Chmod(sys, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sys, 0o700) })
		got := systemStateNoticeAt(t.TempDir(), sys, "waired status", "linux", 1000)
		if !strings.Contains(got, "permission denied") ||
			!strings.Contains(got, "run `sudo waired status`") {
			t.Errorf("notice = %q, want permission-denied + sudo hint", got)
		}
	})

	t.Run("windows wording", func(t *testing.T) {
		got := systemStateNoticeAt(t.TempDir(), enrolledSys(t), "waired status", "windows", -1)
		if !strings.Contains(got, "Administrator") {
			t.Errorf("notice = %q, want Administrator wording on windows", got)
		}
	})

	t.Run("suppressed", func(t *testing.T) {
		resolved := t.TempDir()
		for name, tc := range map[string]struct {
			resolved, sys, goos string
			euid                int
		}{
			"system dir absent → genuinely not enrolled": {
				resolved, filepath.Join(t.TempDir(), "gone"), "linux", 1000},
			"resolved == system (WAIRED_STATE_DIR override)": {
				resolved, resolved, "linux", 1000},
			"root already reads the system dir": {
				resolved, enrolledSys(t), "linux", 0},
		} {
			if got := systemStateNoticeAt(tc.resolved, tc.sys, "waired status", tc.goos, tc.euid); got != "" {
				t.Errorf("%s: notice = %q, want \"\"", name, got)
			}
		}
	})
}
