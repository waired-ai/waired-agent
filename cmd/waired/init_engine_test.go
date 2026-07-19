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
		want           engineInstallAction
	}{
		// Opt-out and reuse win on every OS.
		{"linux opt-out", "linux", true, none, agentconfig.OllamaSourceBundled, false, true, engineActionSkipOptOut},
		{"windows opt-out", "windows", true, none, agentconfig.OllamaSourceBundled, false, true, engineActionSkipOptOut},
		{"darwin opt-out", "darwin", false, none, agentconfig.OllamaSourceBundled, false, true, engineActionSkipOptOut},
		{"linux reuse", "linux", true, detected, agentconfig.OllamaSourceReuse, false, false, engineActionSkipReuse},
		{"windows reuse", "windows", true, detected, agentconfig.OllamaSourceReuse, false, false, engineActionSkipReuse},
		{"darwin reuse", "darwin", true, detected, agentconfig.OllamaSourceReuse, false, false, engineActionSkipReuse},

		// Linux: strict bundled presence; a PATH ollama does NOT count.
		{"linux bundled present", "linux", true, none, agentconfig.OllamaSourceBundled, true, false, engineActionSkipPresent},
		{"linux PATH ollama does not count", "linux", true, detected, agentconfig.OllamaSourceBundled, false, false, engineActionInstall},
		{"linux missing, root", "linux", true, none, agentconfig.OllamaSourceBundled, false, false, engineActionInstall},
		{"linux missing, not root", "linux", false, none, agentconfig.OllamaSourceBundled, false, false, engineActionSkipNotElevated},

		// Windows: any detected install counts; needs an elevated token.
		{"windows detected", "windows", true, detected, agentconfig.OllamaSourceBundled, false, false, engineActionSkipPresent},
		{"windows missing, elevated", "windows", true, none, agentconfig.OllamaSourceBundled, false, false, engineActionInstall},
		{"windows missing, not elevated", "windows", false, none, agentconfig.OllamaSourceBundled, false, false, engineActionSkipNotElevated},

		// macOS: any detected install counts; no elevation gate.
		{"darwin detected", "darwin", false, detected, agentconfig.OllamaSourceBundled, false, false, engineActionSkipPresent},
		{"darwin missing", "darwin", false, none, agentconfig.OllamaSourceBundled, false, false, engineActionInstall},

		// Empty source (pre-#188 configs) behaves as bundled.
		{"empty source is bundled", "windows", true, none, "", false, false, engineActionInstall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := engineInstallDecision(tc.goos, tc.elevated, tc.det, tc.source, tc.bundledPresent, tc.optOut)
			if got != tc.want {
				t.Errorf("engineInstallDecision(%s) = %v, want %v", tc.name, got, tc.want)
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
