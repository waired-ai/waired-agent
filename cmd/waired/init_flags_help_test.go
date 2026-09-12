package main

import (
	"strings"
	"testing"
)

// PRODUCT CONTRACT (waired-agent#1300): --no-browser's help says the
// second thing it does.
//
// The name implies one behaviour — don't launch a browser — and the help
// described only that. The flag is also read by terminalDrivenFromTheStart
// (setup_executor.go), so it turns off the browser SETUP as well: no
// three-minute grace waiting for one to start, no takeover offer, and
// every question asked in the terminal. An operator who passed it to
// avoid a browser popping up on a headless box got a different flow from
// the one they asked for, and nothing said so.
//
// The test reads the registered flag rather than a constant, because the
// string a person sees is the one cobra prints.
func TestInitFlags_NoBrowserHelpSaysItDrivesSetupInTheTerminal(t *testing.T) {
	f := newInitCmd().Flags().Lookup("no-browser")
	if f == nil {
		t.Fatal("no --no-browser flag")
	}
	for _, want := range []string{
		"print the URL and code instead",
		"terminal",
	} {
		if !strings.Contains(f.Usage, want) {
			t.Errorf("--no-browser help = %q, want it to mention %q", f.Usage, want)
		}
	}
}
