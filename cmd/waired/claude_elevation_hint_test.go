package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// managedPathFor is the managed-settings path each OS writes, so the messages
// below are checked with the path a person on that OS actually sees.
var managedPathFor = map[string]string{
	"linux":   "/etc/claude-code/managed-settings.json",
	"darwin":  "/Library/Application Support/ClaudeCode/managed-settings.json",
	"windows": `C:\Program Files\ClaudeCode\managed-settings.json`,
}

// wrappedWritePermission is the error `waired claude enable` gets when an
// existing managed-settings file cannot be rewritten: claudemanaged wraps
// secrets.WriteFile, which wraps the *PathError from os.CreateTemp.
func wrappedWritePermission(path string) error {
	pathErr := &os.PathError{Op: "createtemp", Path: path + ".tmp.*", Err: fs.ErrPermission}
	return fmt.Errorf("claudemanaged: write %s: %w", path,
		fmt.Errorf("secrets: create temp in %s: %w", "dir", pathErr))
}

// TestClaudeEnableErrorNamesTheCommandOnce is the output of an un-elevated
// `waired claude enable` that could not write, as main prints it: the error,
// then one hint that names the command to run elevated. Before
// waired-agent#1419 the named hint never printed (os.IsPermission cannot see
// through the wrapping above) and main's generic "(permission denied: re-run
// with sudo)" stood in for it; a plain errors.Is swap would have printed both.
//
// PIN: product contract, waired-agent#1419.
func TestClaudeEnableErrorNamesTheCommandOnce(t *testing.T) {
	for goos, path := range managedPathFor {
		t.Run(goos, func(t *testing.T) {
			err := wrappedWritePermission(path)

			got := friendlyErrorFor(goos, false, claudeEnableErrorFor(goos, false, err, path))
			want := "waired claude enable: " + err.Error() + "\n  (writing " + path + " needs elevation — " +
				elevationHintFor(goos, "waired claude enable") + ")"
			if got != want {
				t.Errorf("not elevated:\n got %q\nwant %q", got, want)
			}

			// Elevated, the same error is one elevation does not fix — a
			// Windows file held open past the rename retry, an immutable
			// file on a Unix — so there is no hint of either kind.
			got = friendlyErrorFor(goos, true, claudeEnableErrorFor(goos, true, err, path))
			if want := "waired claude enable: " + err.Error(); got != want {
				t.Errorf("elevated:\n got %q\nwant %q", got, want)
			}
		})
	}

	// Anything that is not a permission error keeps its chain, so a caller
	// can still ask what it was.
	parse := errors.New("claudemanaged: settings file is not readable JSON")
	if err := claudeEnableErrorFor("linux", false, parse, managedPathFor["linux"]); !errors.Is(err, parse) {
		t.Errorf("claudeEnableErrorFor(parse error) = %v, want it to wrap the cause", err)
	}
}

// TestManagedWriteWarning is the warning `waired init` prints when routing or
// the context-window top-up could not write managed settings. Both run only
// elevated, and both used to append "run `sudo waired claude enable`" to any
// error at all — advice to a process that already has what it suggests
// (waired-agent#1419).
//
// PIN: product contract, waired-agent#1419.
func TestManagedWriteWarning(t *testing.T) {
	for goos, path := range managedPathFor {
		t.Run(goos, func(t *testing.T) {
			perm := wrappedWritePermission(path)
			parse := errors.New("claudemanaged: settings file is not readable JSON")
			what := "write Claude Code managed settings"

			if got, want := managedWriteWarning(goos, true, what, perm),
				"Warning: couldn't write Claude Code managed settings ("+perm.Error()+")."; got != want {
				t.Errorf("elevated, permission:\n got %q\nwant %q", got, want)
			}
			if got, want := managedWriteWarning(goos, true, what, parse),
				"Warning: couldn't write Claude Code managed settings ("+parse.Error()+")."; got != want {
				t.Errorf("elevated, parse:\n got %q\nwant %q", got, want)
			}
			if got, want := managedWriteWarning(goos, false, what, perm),
				"Warning: couldn't write Claude Code managed settings ("+perm.Error()+"). "+
					capitalize(elevationHintFor(goos, "waired claude enable"))+"."; got != want {
				t.Errorf("not elevated, permission:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// TestClaudeDisableToleratedWarning is the line an un-elevated `waired claude
// disable` prints when it cannot edit the machine-wide file and carries on
// with this user's own settings. The hint is a sentence of its own, so it
// starts with a capital; it used to read "…). run `sudo waired claude
// disable`. Continuing …".
//
// PIN: record of today's behaviour, with the capital added for
// waired-agent#1419.
func TestClaudeDisableToleratedWarning(t *testing.T) {
	for goos, path := range managedPathFor {
		t.Run(goos, func(t *testing.T) {
			err := wrappedWritePermission(path)
			want := "Warning: couldn't remove " + path + " (" + err.Error() + "). " +
				capitalize(elevationHintFor(goos, "waired claude disable")) +
				". Continuing with the per-user cleanup."
			if got := claudeDisableToleratedWarning(goos, path, err); got != want {
				t.Errorf("\n got %q\nwant %q", got, want)
			}
			if !strings.Contains(want, ". Run `sudo ") && !strings.Contains(want, ". Re-run `") {
				t.Errorf("the hint does not start a sentence: %q", want)
			}
		})
	}
}
