package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// The `(engine kind, venv present) -> action` table #339's Proposed item 3
// asks for, at the level the daemon actually decides it: what the boot rule
// says about this host NOW versus what this process booted with.
//
// A record of today's behaviour, not a product contract — the ratifying
// source for the rule is #339 itself (Proposed item 2), and #304 for the
// ollama half it must not disturb. Untagged, so it runs on the darwin and
// windows legs too even though bootstrapVLLM is Linux-only.
func TestDecideEngineStart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		current      string
		reChosen     string
		reChoiceOK   bool
		liveEngineUp bool
		want         string
	}{
		{
			name: "booted on vLLM with the venv still there: start vLLM",
			// The pre-#339 happy path, reached now through the same
			// decision as everything else.
			current: catalog.RuntimeVLLM, reChosen: catalog.RuntimeVLLM, reChoiceOK: true,
			want: engineStartVLLM,
		},
		{
			name: "THE #339 BAR: a venv installed after boot is adopted",
			// The host in the issue title. It booted with no venv, so
			// chooseEngine could only answer ollama and the engine kind was
			// frozen there; the executor then installed the venv and every
			// trigger since reached the ollama arm. Nothing is up, so
			// switching costs nothing.
			current: catalog.RuntimeOllama, reChosen: catalog.RuntimeVLLM, reChoiceOK: true,
			want: engineStartVLLM,
		},
		{
			name: "an engine-less boot adopts the venv the same way",
			// chooseEngine answered no-engine, which servingEngine() reads
			// as ollama. Same transition, from the other starting point.
			current: catalog.RuntimeOllama, reChosen: catalog.RuntimeVLLM, reChoiceOK: true,
			want: engineStartVLLM,
		},
		{
			name:    "THE #304 BAR: an ollama binary installed after boot still starts ollama",
			current: catalog.RuntimeOllama, reChosen: catalog.RuntimeOllama, reChoiceOK: true,
			want: engineStartOllama,
		},
		{
			name: "nothing installed yet: leave the caller's latch open",
			// Not a failure. errEngineNotInstalled, and the next trigger
			// asks again.
			current: catalog.RuntimeOllama, reChosen: "", reChoiceOK: true,
			want: engineStartNone,
		},
		{
			name: "a serving ollama is never switched out from under itself",
			// A venv appearing on a host that is already answering requests
			// is not an emergency. The switch waits for a restart, which is
			// where an engine change has always happened.
			current: catalog.RuntimeOllama, reChosen: catalog.RuntimeVLLM, reChoiceOK: true,
			liveEngineUp: true, want: engineStartOllama,
		},
		{
			name:    "a serving vLLM is never switched out from under itself either",
			current: catalog.RuntimeVLLM, reChosen: catalog.RuntimeOllama, reChoiceOK: true,
			liveEngineUp: true, want: engineStartVLLM,
		},
		{
			name: "liveEngineUp wins even when the live rule found nothing",
			// The rule reads the state dir; a venv being briefly
			// unreadable must not stop a running engine's own start path.
			current: catalog.RuntimeVLLM, reChosen: "", reChoiceOK: true,
			liveEngineUp: true, want: engineStartVLLM,
		},
		{
			name: "the live rule cannot answer: keep the boot decision",
			// Strict mode refusing to fall back, or a unit fixture with no
			// engineChoice wired. Returning none here would refuse the
			// ollama arm, which has its own ollamaUsable() check to make.
			current: catalog.RuntimeOllama, reChosen: "", reChoiceOK: false,
			want: engineStartOllama,
		},
		{
			name:    "the live rule cannot answer on a vLLM host either",
			current: catalog.RuntimeVLLM, reChosen: "", reChoiceOK: false,
			want: engineStartVLLM,
		},
		{
			name: "the answer is symmetric: vLLM demotes to ollama when the venv is gone",
			// The same rule read in the other direction. Reachable only
			// with nothing up, and only when chooseEngine no longer finds
			// the venv — which is what a restart would conclude too.
			current: catalog.RuntimeVLLM, reChosen: catalog.RuntimeOllama, reChoiceOK: true,
			want: engineStartOllama,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decideEngineStart(tc.current, tc.reChosen, tc.reChoiceOK, tc.liveEngineUp)
			if got != tc.want {
				t.Errorf("decideEngineStart(%q, %q, ok=%v, up=%v) = %q, want %q",
					tc.current, tc.reChosen, tc.reChoiceOK, tc.liveEngineUp, got, tc.want)
			}
		})
	}
}

// The constants must stay the catalog's engine names: the answer is
// compared against servingEngine() and handed to adoptEngine, so a
// divergence would silently stop matching rather than fail to compile.
func TestEngineStartConstantsAreCatalogRuntimes(t *testing.T) {
	if engineStartOllama != catalog.RuntimeOllama {
		t.Errorf("engineStartOllama = %q, want %q", engineStartOllama, catalog.RuntimeOllama)
	}
	if engineStartVLLM != catalog.RuntimeVLLM {
		t.Errorf("engineStartVLLM = %q, want %q", engineStartVLLM, catalog.RuntimeVLLM)
	}
	if engineStartNone == catalog.RuntimeOllama || engineStartNone == catalog.RuntimeVLLM {
		t.Errorf("engineStartNone = %q collides with an engine name", engineStartNone)
	}
}
