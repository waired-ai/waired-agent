package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
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
		source         string
		bundledPresent bool
		optOut         bool
		incomplete     bool
		want           engineInstallAction
	}{
		// Opt-out and reuse win on every OS.
		{"linux opt-out", "linux", true, none, agentconfig.OllamaSourceBundled, false, true, false, engineActionSkipOptOut},
		{"windows opt-out", "windows", true, none, agentconfig.OllamaSourceBundled, false, true, false, engineActionSkipOptOut},
		{"darwin opt-out", "darwin", false, none, agentconfig.OllamaSourceBundled, false, true, false, engineActionSkipOptOut},
		{"linux reuse", "linux", true, detected, agentconfig.OllamaSourceReuse, false, false, false, engineActionSkipReuse},
		{"windows reuse", "windows", true, detected, agentconfig.OllamaSourceReuse, false, false, false, engineActionSkipReuse},
		{"darwin reuse", "darwin", true, detected, agentconfig.OllamaSourceReuse, false, false, false, engineActionSkipReuse},
		// Even a half-installed engine is left alone once the operator has
		// said "reuse my own": reuse is answered before we ever look at it.
		{"windows reuse beats incomplete", "windows", true, detected, agentconfig.OllamaSourceReuse, false, false, true, engineActionSkipReuse},

		// Linux: strict bundled presence; a PATH ollama does NOT count.
		{"linux bundled present", "linux", true, none, agentconfig.OllamaSourceBundled, true, false, false, engineActionSkipPresent},
		{"linux PATH ollama does not count", "linux", true, detected, agentconfig.OllamaSourceBundled, false, false, false, engineActionInstall},
		{"linux missing, root", "linux", true, none, agentconfig.OllamaSourceBundled, false, false, false, engineActionInstall},
		{"linux missing, not root", "linux", false, none, agentconfig.OllamaSourceBundled, false, false, false, engineActionSkipNotElevated},

		// Windows: any detected install counts; needs an elevated token.
		{"windows detected", "windows", true, detected, agentconfig.OllamaSourceBundled, false, false, false, engineActionSkipPresent},
		{"windows missing, elevated", "windows", true, none, agentconfig.OllamaSourceBundled, false, false, false, engineActionInstall},
		{"windows missing, not elevated", "windows", false, none, agentconfig.OllamaSourceBundled, false, false, false, engineActionSkipNotElevated},
		// #190: bits with no completion receipt are repaired, not skipped.
		{"windows incomplete, elevated", "windows", true, detected, agentconfig.OllamaSourceBundled, false, false, true, engineActionRepair},
		{"windows incomplete, not elevated", "windows", false, detected, agentconfig.OllamaSourceBundled, false, false, true, engineActionSkipNotElevated},

		// macOS: any detected install counts; no elevation gate.
		{"darwin detected", "darwin", false, detected, agentconfig.OllamaSourceBundled, false, false, false, engineActionSkipPresent},
		{"darwin missing", "darwin", false, none, agentconfig.OllamaSourceBundled, false, false, false, engineActionInstall},

		// Empty source (pre-#188 configs) behaves as bundled.
		{"empty source is bundled", "windows", true, none, "", false, false, false, engineActionInstall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := engineInstallDecision(
				tc.goos, tc.elevated, tc.det, tc.source,
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
		// The marker is written on macOS too, but only Windows ever ends up
		// in the extracted-but-unconfigured state this guards.
		{"linux never incomplete", "linux", ours("/usr/local/bin/ollama"), pf, false},
		{"darwin never incomplete", "darwin",
			ours("/Applications/Ollama.app/Contents/Resources/ollama"), pf, false},
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
	if p := bundledEnginePath("linux", "/var/lib/waired"); p == "" {
		t.Error("linux bundled path must be non-empty")
	}
	for _, goos := range []string{"windows", "darwin"} {
		if p := bundledEnginePath(goos, `C:\ProgramData\waired`); p != "" {
			t.Errorf("bundledEnginePath(%s) = %q, want empty (global install model)", goos, p)
		}
	}
}
