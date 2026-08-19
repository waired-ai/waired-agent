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

// EngineInstallCommand spells the AI-engine install command the way the
// operator has to invoke it on this OS, as a bare command phrase meant
// to be quoted (`...`) inside a sentence the caller writes.
//
// Hint is the wrong shape for that: it phrases a RE-run of something the
// user already tried, so "re-run `...` from an elevated prompt" reads
// as an instruction to repeat a step that never happened when the
// sentence is an offer rather than an error.
//
// Every OS now writes a directory an ordinary user does not own, so the
// command is elevated everywhere; Windows says it in its own idiom
// rather than with a sudo it has no command for (waired#752).
func EngineInstallCommand() string { return EngineInstallCommandFor(runtime.GOOS) }

// EngineInstallCommandFor is the testable core, keyed on goos.
func EngineInstallCommandFor(goos string) string {
	if goos == "windows" {
		return "waired runtimes install ollama (from an elevated prompt)"
	}
	return "sudo waired runtimes install ollama"
}
