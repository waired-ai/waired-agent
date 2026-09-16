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
// error must NOT abort, so the un-elevated per-user phase of uninstall.ps1 still
// scrubs the invoking user's ~/.claude; the elevated phase removes the
// admin-owned managed-settings file itself.
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
			if got := managedRemoveIsFatal(tc.err); got != tc.want {
				t.Fatalf("managedRemoveIsFatal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
