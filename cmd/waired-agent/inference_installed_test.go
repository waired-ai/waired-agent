package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// TestRuntimeStatusInstalledAsksTheHost pins the fix for #852: the
// INSTALLED field answers the host, not the registry.
//
// It used to be the literal `true` for every registered adapter, and the
// adapters are registered unconditionally at boot — so `waired runtimes
// ls` printed INSTALLED yes on a Windows host whose daemon had logged
// "bundled ollama not installed (expected at ...)" on the same boot.
// Three surfaces read the field and were all wrong there: that column,
// the `if !r.Installed` skip in `waired status`, and install.sh's
// waired_engine_installed, whose done banner therefore always claimed
// the engine was installed.
func TestRuntimeStatusInstalledAsksTheHost(t *testing.T) {
	ctx := context.Background()
	yes := func() bool { return true }
	no := func() bool { return false }

	reg := infruntime.NewRegistry()
	reg.Register(fakeAdapter{name: "ollama"})
	reg.Register(fakeAdapter{name: "vllm"})

	ollamaOnProfile := hardware.Profile{}
	ollamaOnProfile.Engines.Ollama = hardware.EngineInfo{Installed: true, Version: "0.32.13"}

	cases := []struct {
		name         string
		engine       string
		hw           hardware.Profile
		ollamaUsable func() bool
		vllmUsable   func() bool
		want         bool
	}{
		// The observed defect state: nothing on the host, adapters
		// registered anyway.
		{"no engine at all", "ollama", hardware.Profile{}, no, no, false},
		{"no engine at all, vllm entry", "vllm", hardware.Profile{}, no, no, false},
		{"ollama resolvable", "ollama", hardware.Profile{}, yes, no, true},
		{"vllm venv active", "vllm", hardware.Profile{}, no, yes, true},
		// The resolver is the answer, not a hint: a stale profile saying
		// the engine is here does not override a resolver that cannot
		// find it. That direction is the whole defect.
		{"resolver says no, cached profile says yes", "ollama", ollamaOnProfile, no, no, false},
		// No resolver wired (a fixture constructing the provider
		// directly) still falls back to the profile, so those keep
		// working.
		{"no resolver, profile installed", "ollama", ollamaOnProfile, nil, nil, true},
		{"no resolver, profile empty", "ollama", hardware.Profile{}, nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &agentInferenceProvider{
				registry:     reg,
				ollamaUsable: tc.ollamaUsable,
				vllmUsable:   tc.vllmUsable,
			}
			got := p.runtimeStatusFor(ctx, tc.engine, tc.hw)
			if got.Installed != tc.want {
				t.Errorf("Installed = %v, want %v", got.Installed, tc.want)
			}
		})
	}
}

// TestRuntimeStatusInstalledAndVersionAgree keeps the two columns of
// `waired runtimes ls` from contradicting each other the way the report
// did — INSTALLED yes next to an empty VERSION, on a host with no
// engine. VERSION was the honest one; a version can still be absent on
// an installed engine (never probed), but "installed no" must never
// carry one.
func TestRuntimeStatusInstalledAndVersionAgree(t *testing.T) {
	ctx := context.Background()
	reg := infruntime.NewRegistry()
	reg.Register(fakeAdapter{name: "ollama"})

	p := &agentInferenceProvider{
		registry:     reg,
		ollamaUsable: func() bool { return false },
	}
	got := p.runtimeStatusFor(ctx, "ollama", hardware.Profile{})
	if got.Installed {
		t.Fatalf("Installed = true on a host with no resolvable engine")
	}
	if got.Version != "" {
		t.Errorf("Version = %q on an engine reported not installed", got.Version)
	}
}

// TestEngineUsableOnHostIsOneRule pins the extraction that made
// hasUsableEngine and the Installed field ask the same question, so
// subsystem_state and the INSTALLED column cannot disagree about the
// same host.
func TestEngineUsableOnHostIsOneRule(t *testing.T) {
	no := func() bool { return false }
	yes := func() bool { return true }

	if engineUsableOnHost("lan-gpu", hardware.Profile{}, yes, yes) {
		t.Error("an engine kind this rule does not know reads as installed")
	}

	reg := infruntime.NewRegistry()
	reg.Register(fakeAdapter{name: "ollama"})
	for _, tc := range []struct {
		name   string
		usable func() bool
	}{{"usable", yes}, {"not usable", no}} {
		t.Run(tc.name, func(t *testing.T) {
			one := engineUsableOnHost("ollama", hardware.Profile{}, tc.usable, nil)
			any := hasUsableEngine(reg, hardware.Profile{}, tc.usable, nil)
			if one != any {
				t.Errorf("engineUsableOnHost = %v but hasUsableEngine = %v for the same host", one, any)
			}
		})
	}
}
