package main

import (
	"os"

	"golang.org/x/term"
)

// isTerminal returns true when f is a TTY-attached stream. Used by
// `waired link` and `waired doctor` to decide whether to show
// interactive prompts. `golang.org/x/term.IsTerminal` is the portable
// equivalent of the original unix.IoctlGetTermios probe; it works on
// Linux / macOS / Windows.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
