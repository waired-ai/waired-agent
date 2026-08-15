package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// PRODUCT CONTRACT — waired-agent#800. Only absence is repaired: a file
// that is present is never overwritten, and a file that cannot be read is
// never treated as absent.
//
// The last clause is #778's lesson one layer over. There, an EACCES from
// os.Stat answered the same as "not installed", so a complete vLLM engine
// read as missing and init waited forever. Here the same conflation would
// hide a permissions change behind a write that cannot help.
func TestDecideIdentityRepair(t *testing.T) {
	for _, tc := range []struct {
		name    string
		statErr error
		haveID  bool
		want    identityAction
	}{
		{"file present, identity held", nil, true, identityNoAction},
		{"file present, no identity held", nil, false, identityNoAction},
		{"absent and we hold the truth", fs.ErrNotExist, true, identityRestore},
		{"absent on an unenrolled daemon", fs.ErrNotExist, false, identityNoAction},
		{"unreadable, identity held", fs.ErrPermission, true, identityReport},
		{"unreadable, no identity", fs.ErrPermission, false, identityReport},
		{"some other error", errors.New("i/o error"), true, identityReport},
		// The real shape: os.Stat wraps its error in *PathError, so the
		// rule has to unwrap. A test that only passed the sentinel would
		// pass while production took the default arm.
		{"wrapped ENOENT from a real stat", &fs.PathError{Op: "stat", Err: fs.ErrNotExist}, true, identityRestore},
		{"wrapped EACCES from a real stat", &fs.PathError{Op: "stat", Err: fs.ErrPermission}, true, identityReport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideIdentityRepair(tc.statErr, tc.haveID); got != tc.want {
				t.Fatalf("decideIdentityRepair(%v, %v) = %v, want %v",
					tc.statErr, tc.haveID, got, tc.want)
			}
		})
	}
}

// The rule has to agree with what os.Stat actually returns, not with what
// a sentinel looks like. CLAUDE.md §Test discipline: a seam whose real
// input is never exercised is a seam nobody has tested.
func TestDecideIdentityRepair_AgainstRealStat(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "identity.json")
	if err := os.WriteFile(present, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := os.Stat(present)
	if got := decideIdentityRepair(err, true); got != identityNoAction {
		t.Errorf("a file that exists: got %v, want identityNoAction", got)
	}

	_, err = os.Stat(filepath.Join(dir, "gone.json"))
	if got := decideIdentityRepair(err, true); got != identityRestore {
		t.Errorf("a file that does not exist: got %v, want identityRestore", got)
	}
}
