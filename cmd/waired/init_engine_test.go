package main

import (
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// TestEngineInstallDecision is a cross-OS PARITY table, and after #492/#493
// its point is that every row answers the same way. "Present" is the
// waired-managed binary under the state dir on all three; installing needs
// elevation on all three.
//
// The asymmetry it replaces was real and load-bearing for as long as the
// Windows and macOS engines lived at global well-known paths: there, "any
// DetectOllama hit counts" was the only workable rule — and it is exactly
// how a user's own unpinned Ollama could satisfy setup and then be served
// through (#139).
func TestEngineInstallDecision(t *testing.T) {
	cases := []struct {
		name           string
		goos           string
		elevated       bool
		bundledPresent bool
		optOut         bool
		want           engineInstallAction
	}{
		{"linux opt-out", "linux", true, false, true, engineActionSkipOptOut},
		{"windows opt-out", "windows", true, false, true, engineActionSkipOptOut},
		{"darwin opt-out", "darwin", true, false, true, engineActionSkipOptOut},

		{"linux bundled present", "linux", true, true, false, engineActionSkipPresent},
		{"windows bundled present", "windows", true, true, false, engineActionSkipPresent},
		{"darwin bundled present", "darwin", true, true, false, engineActionSkipPresent},

		{"linux missing, elevated", "linux", true, false, false, engineActionInstall},
		{"windows missing, elevated", "windows", true, false, false, engineActionInstall},
		{"darwin missing, elevated", "darwin", true, false, false, engineActionInstall},

		{"linux missing, not elevated", "linux", false, false, false, engineActionSkipNotElevated},
		{"windows missing, not elevated", "windows", false, false, false, engineActionSkipNotElevated},
		{"darwin missing, not elevated", "darwin", false, false, false, engineActionSkipNotElevated},

		// Presence outranks elevation: an unelevated run on a host that
		// already has the engine reports done, not permission_denied.
		{"present beats unelevated", "windows", false, true, false, engineActionSkipPresent},
		// And opt-out outranks presence, so a host told not to run models
		// says so rather than quietly reporting an engine it will not use.
		{"opt-out beats presence", "linux", true, true, true, engineActionSkipOptOut},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := engineInstallDecision(tc.goos, tc.elevated, tc.bundledPresent, tc.optOut)
			if got != tc.want {
				t.Errorf("engineInstallDecision(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// bundledEnginePath IS the daemon's join, so init and the daemon cannot
// disagree about where the engine is — which is what #179 was.
func TestBundledEnginePath(t *testing.T) {
	for _, tc := range []struct {
		goos     string
		stateDir string
	}{
		{"linux", "/var/lib/waired"},
		{"darwin", "/Library/Application Support/waired"},
		{"windows", `C:\ProgramData\waired`},
	} {
		want := infruntime.BundledOllamaBinaryPath(tc.goos, tc.stateDir)
		got := bundledEnginePath(tc.goos, tc.stateDir)
		if got != want {
			t.Errorf("bundledEnginePath(%s) = %q, want %q", tc.goos, got, want)
		}
		if got == "" {
			t.Errorf("bundledEnginePath(%s) is empty; every OS installs under the state dir now", tc.goos)
		}
	}
}
