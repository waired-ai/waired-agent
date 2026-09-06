//go:build linux

package tray

import (
	"slices"
	"testing"
)

// TestLogoutPkexecArgsCarryTheStateDir pins the argv the polkit prompt
// authorizes.
//
// waired-agent#1269 was reported on macOS, but the state-dir misreading behind
// it is not OS-specific: /var/lib/waired is created 0700 root here too, so the
// desktop-user app was equally unable to see it and equally likely to hand
// this argv the per-user directory instead. `waired logout` on a directory
// with no identity exits 0 having removed nothing, so the failure is silent on
// this OS as well. Cross-OS parity (CLAUDE.md): the fix is checked on all
// three, not only on the one the report came from.
func TestLogoutPkexecArgsCarryTheStateDir(t *testing.T) {
	got := logoutPkexecArgs("/var/lib/waired")
	want := []string{"pkexec", "waired", "logout", "--yes", "--state-dir", "/var/lib/waired"}
	if !slices.Equal(got, want) {
		t.Errorf("logoutPkexecArgs =\n %q\nwant\n %q", got, want)
	}
}

// The state dir must be its own argv entry. pkexec takes a program and
// arguments rather than a shell string, so a directory holding a space must
// arrive whole and unquoted.
func TestLogoutPkexecArgsKeepASpacedPathInOneEntry(t *testing.T) {
	got := logoutPkexecArgs("/srv/waired state")
	if n := len(got); n != 6 {
		t.Fatalf("logoutPkexecArgs produced %d entries (%q), want 6", n, got)
	}
	if got[5] != "/srv/waired state" {
		t.Errorf("state dir entry = %q, want it whole and unquoted", got[5])
	}
}
