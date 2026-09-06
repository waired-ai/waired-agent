package identity

import (
	"os"
	"path/filepath"
	"testing"
)

// seedEnrollment lays down a full enrollment under dir and returns the paths
// that RemoveEnrollment must delete and the ones it must leave.
func seedEnrollment(t *testing.T, dir string) (removed, kept []string) {
	t.Helper()
	p, err := PathsFor(dir)
	if err != nil {
		t.Fatalf("PathsFor: %v", err)
	}
	removed = []string{p.Identity, p.MachineKey, p.NodeKey, p.AccessToken, p.RefreshToken}
	kept = []string{p.NetworkMap, p.TokenMeta, p.ControlSigningPubKey}
	for _, path := range append(append([]string{}, removed...), kept...) {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	return removed, kept
}

// TestRemoveEnrollmentDeletesTheWholeEnrollment pins the list.
//
// Product contract: sign-out removes everything the device's identity is made
// of. The list has been corrected twice — #261 added the refresh token after
// finding it survived a logout, waired#1277 dropped the gateway token with the
// credential — which is why it lives here rather than in each caller.
func TestRemoveEnrollmentDeletesTheWholeEnrollment(t *testing.T) {
	dir := t.TempDir()
	removed, kept := seedEnrollment(t, dir)

	if err := RemoveEnrollment(dir); err != nil {
		t.Fatalf("RemoveEnrollment: %v", err)
	}
	for _, path := range removed {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived the sign-out (stat err = %v)", filepath.Base(path), err)
		}
	}
	// cache/* is deliberately left: recoverable from the control plane, and
	// worthless without the secrets.
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s should have been left alone: %v", filepath.Base(path), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets")); !os.IsNotExist(err) {
		t.Errorf("emptied secrets/ was not pruned (stat err = %v)", err)
	}
}

// Sign-out is idempotent by design — the uninstaller reruns it, and a machine
// that was never enrolled must not fail.
func TestRemoveEnrollmentIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	seedEnrollment(t, dir)
	if err := RemoveEnrollment(dir); err != nil {
		t.Fatalf("first RemoveEnrollment: %v", err)
	}
	if err := RemoveEnrollment(dir); err != nil {
		t.Errorf("second RemoveEnrollment: %v", err)
	}
	if err := RemoveEnrollment(t.TempDir()); err != nil {
		t.Errorf("RemoveEnrollment on a never-enrolled dir: %v", err)
	}
}

// A removal must never CREATE the state dir tree — PathsUnder, not PathsFor.
// A machine that was never enrolled would otherwise come out of a sign-out
// with a freshly minted, empty state dir, which then reads to every other
// surface as "installed here".
func TestRemoveEnrollmentDoesNotCreateTheStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-enrolled")
	if err := RemoveEnrollment(dir); err != nil {
		t.Fatalf("RemoveEnrollment: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("state dir was created by a removal (stat err = %v)", err)
	}
}

func TestRemoveEnrollmentRejectsAnEmptyStateDir(t *testing.T) {
	if err := RemoveEnrollment(""); err == nil {
		t.Error("RemoveEnrollment(\"\") = nil, want an error rather than a removal rooted at the filesystem root")
	}
}
