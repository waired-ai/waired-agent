package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
)

// TestManagedRemoveIsFatal covers the tolerate-vs-abort decision `claude
// disable` makes on a managed-settings.Remove() error (waired#754). A permission
// error must NOT abort an un-elevated run, so the un-elevated per-user phase of
// uninstall.ps1 still scrubs the invoking user's ~/.claude; the elevated phase
// removes the admin-owned managed-settings file itself.
//
// An elevated run is that phase, so a permission error there is a failure: it
// used to be tolerated too, and `sudo waired claude disable` exited 0 over a
// file still pointing Claude Code at the loopback gateway (waired-agent#1419).
func TestManagedRemoveIsFatal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not fatal", nil, false},
		{"permission tolerated", fs.ErrPermission, false},
		{"wrapped permission tolerated", &os.PathError{Op: "open", Path: "x", Err: fs.ErrPermission}, false},
		// secrets.WriteFile's shape, which a rewrite returns (waired-agent#1409).
		{"fmt-wrapped permission tolerated", fmt.Errorf("secrets: create temp in %s: %w", "dir",
			&os.PathError{Op: "createtemp", Path: "dir", Err: fs.ErrPermission}), false},
		{"fmt-wrapped other error is fatal", fmt.Errorf("secrets: rename: %w", errors.New("disk full")), true},
		{"other error is fatal", errors.New("disk full"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedRemoveIsFatal(tc.err, false); got != tc.want {
				t.Fatalf("managedRemoveIsFatal(%v, not elevated) = %v, want %v", tc.err, got, tc.want)
			}
			// Elevated, every error is fatal; nil still is not.
			if got, want := managedRemoveIsFatal(tc.err, true), tc.err != nil; got != want {
				t.Fatalf("managedRemoveIsFatal(%v, elevated) = %v, want %v", tc.err, got, want)
			}
		})
	}
}
