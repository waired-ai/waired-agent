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

// EngineInstallCommand is the AI-engine install command, and NOTHING
// else: exactly the characters that can be typed or pasted into a
// shell. Callers quote it; the elevation a Windows host needs is
// EngineInstallElevationNote, which goes OUTSIDE the quotes.
//
// It used to carry that note inside itself, which put prose where a
// command was promised — `waired runtimes install ollama (from an
// elevated prompt)` inside backticks, and the tray copied that whole
// string to the clipboard, so pasting it could only fail (#852,
// observed on pc-dell-premium).
//
// Hint is the wrong shape here too: it phrases a RE-run of something
// already attempted, which reads as repeating a step that never
// happened when the sentence is an offer rather than an error.
//
// Every OS now writes a directory an ordinary user does not own, so the
// command is elevated everywhere; Windows has no sudo to say it with
// (waired#752), which is why the note exists separately at all.
func EngineInstallCommand() string { return EngineInstallCommandFor(runtime.GOOS) }

// EngineInstallCommandFor is the testable core, keyed on goos.
func EngineInstallCommandFor(goos string) string {
	if goos == "windows" {
		return "waired runtimes install ollama"
	}
	return "sudo waired runtimes install ollama"
}

// EngineInstallElevationNote is how the operator has to be running the
// shell for that command to work, or "" where the command says it
// itself. Written as a prepositional phrase so it can be appended to a
// sentence: "run `<cmd>`" + ", " + "from an elevated prompt".
func EngineInstallElevationNote() string { return EngineInstallElevationNoteFor(runtime.GOOS) }

// EngineInstallElevationNoteFor is the testable core, keyed on goos.
func EngineInstallElevationNoteFor(goos string) string {
	if goos == "windows" {
		return "from an elevated prompt"
	}
	// sudo is IN the command, so there is nothing left to say.
	return ""
}
