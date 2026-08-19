package elevation

import (
	"strings"
	"testing"
)

// Product contract (waired-agent#852, observed on pc-dell-premium): what
// a surface quotes as the install command must be exactly what can be
// typed or pasted. The elevation a Windows host needs is prose and lives
// outside the quotes.
//
// It used to live inside: EngineInstallCommand returned "waired runtimes
// install ollama (from an elevated prompt)", so `waired models ls
// --detail` rendered prose inside backticks and the tray copied the
// parenthetical to the clipboard, where pasting it could only fail.
func TestEngineInstallCommandIsOnlyACommand(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			cmd := EngineInstallCommandFor(goos)
			if !strings.HasSuffix(cmd, "waired runtimes install ollama") {
				t.Errorf("command = %q, want it to end with the bare command", cmd)
			}
			// Prose markers. A command line has no parentheses and no
			// sentence in it.
			for _, banned := range []string{"(", ")", "prompt", "elevated", "re-run"} {
				if strings.Contains(cmd, banned) {
					t.Errorf("command %q carries prose (%q); it must be paste-able as-is", cmd, banned)
				}
			}
		})
	}
}

// The elevation is still said — just beside the command, and only where
// the command cannot say it itself.
func TestEngineInstallElevationNote(t *testing.T) {
	for goos, want := range map[string]string{
		"linux":   "",
		"darwin":  "",
		"windows": "from an elevated prompt",
	} {
		if got := EngineInstallElevationNoteFor(goos); got != want {
			t.Errorf("%s: note = %q, want %q", goos, got, want)
		}
	}

	// On a host with sudo the command carries it, so a caller appending
	// the note would say it twice.
	if !strings.HasPrefix(EngineInstallCommandFor("linux"), "sudo ") {
		t.Error("the unix command must carry its own elevation")
	}
	// Windows has no sudo to say it with (waired#752), which is the
	// whole reason the note exists.
	if strings.Contains(EngineInstallCommandFor("windows"), "sudo") {
		t.Error("the windows command must not name a sudo it has no command for")
	}
}
