package elevation

import (
	"fmt"
	"runtime"
)

// Hint phrases the platform-appropriate "re-run elevated" advice for a
// command. cmdline is the suggested command ("waired init"); empty means
// the generic phrasing. On Unix that is `sudo <cmd>`; on Windows there is
// no sudo, so it names an elevated (Administrator) prompt — a bare
// `sudo waired …` printed on Windows was waired#752.
//
// It lives here (rather than in cmd/waired) so the daemon binary and the
// engine runtime — which cannot import cmd/waired's package main — share
// the exact same wording as the CLI's cmd/waired.elevationHint.
func Hint(cmdline string) string {
	return HintFor(runtime.GOOS, cmdline)
}

// HintFor is the testable core of Hint, keyed on goos so a single table
// test can cover windows / linux / darwin.
func HintFor(goos, cmdline string) string {
	if goos == "windows" {
		if cmdline == "" {
			return "re-run from an elevated (Administrator) prompt"
		}
		return fmt.Sprintf("re-run `%s` from an elevated (Administrator) prompt", cmdline)
	}
	if cmdline == "" {
		return "re-run with sudo"
	}
	return fmt.Sprintf("run `sudo %s`", cmdline)
}
