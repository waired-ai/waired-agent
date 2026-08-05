package main

import (
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/setup"
)

func TestEngineInstallDecision(t *testing.T) {
	detected := setup.OllamaDetection{Installed: true, Path: "/somewhere/ollama"}
	none := setup.OllamaDetection{}
	cases := []struct {
		name           string
		goos           string
		elevated       bool
		det            setup.OllamaDetection
		bundledPresent bool
		optOut         bool
		incomplete     bool
		want           engineInstallAction
	}{
		// Opt-out wins on every OS.
		{"linux opt-out", "linux", true, none, false, true, false, engineActionSkipOptOut},
		{"windows opt-out", "windows", true, none, false, true, false, engineActionSkipOptOut},
		{"darwin opt-out", "darwin", true, none, false, true, false, engineActionSkipOptOut},

		// Linux: strict bundled presence; a PATH ollama does NOT count.
		{"linux bundled present", "linux", true, none, true, false, false, engineActionSkipPresent},
		{"linux PATH ollama does not count", "linux", true, detected, false, false, false, engineActionInstall},
		{"linux missing, root", "linux", true, none, false, false, false, engineActionInstall},
		{"linux missing, not root", "linux", false, none, false, false, false, engineActionSkipNotElevated},

		// macOS is Linux now (#492): the engine lives under the root-owned
		// state dir, so presence is the state-dir binary and installing needs
		// root. Both were different before — any DetectOllama hit counted, and
		// /Applications being admin-group-writable meant no elevation gate at
		// all — which is how a user's own unpinned Ollama could satisfy setup
		// and then get served through (#139).
		{"darwin bundled present", "darwin", true, none, true, false, false, engineActionSkipPresent},
		{"darwin foreign ollama does not count", "darwin", true, detected, false, false, false, engineActionInstall},
		{"darwin missing, root", "darwin", true, none, false, false, false, engineActionInstall},
		{"darwin missing, not root", "darwin", false, none, false, false, false, engineActionSkipNotElevated},

		// Windows: any detected install counts; needs an elevated token. The
		// last OS whose bundled install lives outside the state dir (#493).
		{"windows detected", "windows", true, detected, false, false, false, engineActionSkipPresent},
		{"windows missing, elevated", "windows", true, none, false, false, false, engineActionInstall},
		{"windows missing, not elevated", "windows", false, none, false, false, false, engineActionSkipNotElevated},
		// #190: bits with no completion receipt are repaired, not skipped.
		{"windows incomplete, elevated", "windows", true, detected, false, false, true, engineActionRepair},
		{"windows incomplete, not elevated", "windows", false, detected, false, false, true, engineActionSkipNotElevated},
		// incomplete is a Windows fact; pin that the DECISION ignores it
		// elsewhere, so a future caller cannot make the strict OSes repair on
		// a stray true.
		{"linux ignores the incomplete fact", "linux", true, none, true, false, true, engineActionSkipPresent},
		{"darwin ignores the incomplete fact", "darwin", true, none, true, false, true, engineActionSkipPresent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := engineInstallDecision(
				tc.goos, tc.elevated, tc.det,
				tc.bundledPresent, tc.optOut, tc.incomplete)
			if got != tc.want {
				t.Errorf("engineInstallDecision(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// #190: an install that failed after extraction leaves waired's own bits on
// disk with none of the configuration and, crucially, no marker. Recognising
// that state is what makes the next run repair instead of skipping forever.
func TestEngineIncomplete(t *testing.T) {
	const pf = `C:\Program Files`
	ours := func(p string) setup.OllamaDetection {
		return setup.OllamaDetection{Installed: true, Path: p}
	}

	cases := []struct {
		name string
		goos string
		det  setup.OllamaDetection
		pf   string
		want bool
	}{
		{"our dir, no marker", "windows", ours(`C:\Program Files\Ollama\ollama.exe`), pf, true},
		{"case and separator insensitive", "windows",
			ours(`c:/PROGRAM FILES/ollama/OLLAMA.EXE`), pf, true},
		{"our dir with marker is complete", "windows", setup.OllamaDetection{
			Installed: true, Path: `C:\Program Files\Ollama\ollama.exe`, WairedManaged: true,
		}, pf, false},
		// The user's own install, wherever it lives, is never "ours to fix".
		{"per-user OllamaSetup install", "windows",
			ours(`C:\Users\dev\AppData\Local\Programs\Ollama\ollama.exe`), pf, false},
		{"somewhere on PATH", "windows", ours(`D:\tools\ollama.exe`), pf, false},
		{"nothing detected", "windows", setup.OllamaDetection{}, pf, false},
		{"no ProgramFiles in the environment", "windows",
			ours(`C:\Program Files\Ollama\ollama.exe`), "", false},
		// Windows is the only OS that can end up extracted-but-unconfigured,
		// and since #492/#493 the only one that writes a marker at all.
		{"linux never incomplete", "linux", ours("/usr/local/bin/ollama"), pf, false},
		{"darwin never incomplete", "darwin",
			ours("/Library/Application Support/waired/runtimes/ollama/bin/ollama"), pf, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineIncomplete(tc.goos, tc.det, tc.pf); got != tc.want {
				t.Errorf("engineIncomplete(%s, %q) = %v, want %v",
					tc.goos, tc.det.Path, got, tc.want)
			}
		})
	}
}

func TestBundledEnginePath(t *testing.T) {
	// The two strict OSes answer with the daemon's own join, so init and the
	// daemon cannot disagree about where the engine is (#179).
	for _, tc := range []struct {
		goos     string
		stateDir string
	}{
		{"linux", "/var/lib/waired"},
		{"darwin", "/Library/Application Support/waired"},
	} {
		want := infruntime.BundledOllamaBinaryPath(tc.goos, tc.stateDir)
		if got := bundledEnginePath(tc.goos, tc.stateDir); got != want {
			t.Errorf("bundledEnginePath(%s) = %q, want %q", tc.goos, got, want)
		}
	}
	// Windows is the last global-install holdout (#493).
	if p := bundledEnginePath("windows", `C:\ProgramData\waired`); p != "" {
		t.Errorf("bundledEnginePath(windows) = %q, want empty (global install model)", p)
	}
}
