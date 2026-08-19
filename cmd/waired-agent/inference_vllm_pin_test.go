package main

import (
	"context"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// Ollama parity for the vLLM pin (#843). Until the converge shipped
// there was no warning at all for vLLM: a venv could sit several
// releases behind the pin and `waired status` / `waired runtimes ls`
// said nothing, on a host where the parser names and serve flags this
// build emits were read out of the pinned release.
func TestVLLMVersionWarning(t *testing.T) {
	cases := []struct {
		name      string
		installed string
		warn      bool
	}{
		{"venv matches the pin", infruntime.VLLMPinnedVersion, false},
		{"venv is behind", "0.20.0", true},
		// Exact match, so ahead is a mismatch too: this build was tested
		// against the pin, not against anything newer.
		{"venv is ahead", "0.25.0", true},
		// Absence of data, not evidence of mismatch — the same rule as
		// ollamaVersionWarning's unknown live version.
		{"no venv", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := vllmVersionWarning(tc.installed)
			if (got != "") != tc.warn {
				t.Errorf("vllmVersionWarning(%q) = %q, want warning=%v", tc.installed, got, tc.warn)
			}
		})
	}
}

// The advice has to be the converge verb. `runtimes install vllm` is
// what this used to require, and it prompts for a ~6 GB confirmation the
// converge has already earned — and on a host that is merely behind, the
// question it asks ("install a venv here?") has already been answered.
func TestVLLMVersionWarning_PointsAtTheConvergeVerb(t *testing.T) {
	got := vllmVersionWarning("0.20.0")
	if !strings.Contains(got, "runtimes upgrade vllm") {
		t.Errorf("warning = %q, want it to name `waired runtimes upgrade vllm`", got)
	}
	if strings.Contains(got, "runtimes install vllm") {
		t.Errorf("warning = %q, want it not to send the user to the install prompt", got)
	}
	for _, want := range []string{"0.20.0", infruntime.VLLMPinnedVersion} {
		if !strings.Contains(got, want) {
			t.Errorf("warning = %q, want it to name %q", got, want)
		}
	}
}

// And the wire entry actually carries it. Testing the function alone
// leaves the field assignment unguarded — the whole defect in #843 was a
// rule that existed nowhere the user could see it.
func TestRuntimeStatus_VLLMCarriesThePinAndItsWarning(t *testing.T) {
	ctx := context.Background()
	reg := infruntime.NewRegistry()
	reg.Register(fakeAdapter{name: "vllm"})
	p := &agentInferenceProvider{registry: reg, vllmUsable: func() bool { return true }}

	behind := hardware.Profile{}
	behind.Engines.VLLM = hardware.EngineInfo{Installed: true, Version: "0.20.0"}
	got := p.runtimeStatusFor(ctx, "vllm", behind)
	if got.PinnedVersion != infruntime.VLLMPinnedVersion {
		t.Errorf("PinnedVersion = %q, want %q", got.PinnedVersion, infruntime.VLLMPinnedVersion)
	}
	if got.VersionWarning == "" {
		t.Error("a venv behind the pin reports no warning: this is the state #843 was filed for")
	}

	atPin := hardware.Profile{}
	atPin.Engines.VLLM = hardware.EngineInfo{Installed: true, Version: infruntime.VLLMPinnedVersion}
	if w := p.runtimeStatusFor(ctx, "vllm", atPin).VersionWarning; w != "" {
		t.Errorf("VersionWarning = %q on a venv at the pin, want none", w)
	}
}
