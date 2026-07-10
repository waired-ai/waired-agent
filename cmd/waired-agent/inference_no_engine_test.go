package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// fakeAdapter is a minimal runtime.Adapter used to exercise
// hasUsableEngine's name-based branching without spawning a process.
type fakeAdapter struct{ name string }

func (f fakeAdapter) Name() string                             { return f.name }
func (f fakeAdapter) EnsureRunning(context.Context) error      { return nil }
func (f fakeAdapter) Health(context.Context) infruntime.Health { return infruntime.Health{} }
func (f fakeAdapter) Stop(context.Context) error               { return nil }
func (f fakeAdapter) BaseURL() string                          { return "" }

// TestHasUsableEngine pins the no_engine derivation: a local engine
// (ollama / vllm) only counts when its binary is actually installed
// (the adapter is registered unconditionally at boot), while an external
// openai-compat adapter is always usable. This is the fix for #188 —
// before it, a registered-but-uninstalled ollama suppressed no_engine,
// so the tray never offered the "Install Ollama" prompt.
func TestHasUsableEngine(t *testing.T) {
	ollamaInstalled := hardware.Profile{}
	ollamaInstalled.Engines.Ollama = hardware.EngineInfo{Installed: true, Version: "0.24.0"}

	vllmInstalled := hardware.Profile{}
	vllmInstalled.Engines.VLLM = hardware.EngineInfo{Installed: true, Version: "0.11.0"}

	none := hardware.Profile{}

	regWith := func(names ...string) *infruntime.Registry {
		r := infruntime.NewRegistry()
		for _, n := range names {
			r.Register(fakeAdapter{name: n})
		}
		return r
	}

	yes := func() bool { return true }
	no := func() bool { return false }

	cases := []struct {
		name         string
		reg          *infruntime.Registry
		hw           hardware.Profile
		ollamaUsable func() bool
		want         bool
	}{
		// ollamaUsable resolver (bundled-aware) wins over the PATH-based
		// profiler when wired.
		{"ollama resolver says usable", regWith("ollama"), none, yes, true},
		{"ollama resolver says not usable", regWith("ollama"), ollamaInstalled, no, false},
		// nil resolver (unit-test style) falls back to the profiler flag.
		{"nil resolver, profiler installed", regWith("ollama"), ollamaInstalled, nil, true},
		{"nil resolver, profiler not installed", regWith("ollama"), none, nil, false},
		{"vllm registered and installed", regWith("vllm"), vllmInstalled, no, true},
		{"vllm registered but not installed", regWith("vllm"), none, no, false},
		{"external adapter always usable", regWith("lan-gpu"), none, no, true},
		{"ollama unusable + external usable", regWith("ollama", "lan-gpu"), none, no, true},
		{"empty registry", regWith(), none, no, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasUsableEngine(tc.reg, tc.hw, tc.ollamaUsable); got != tc.want {
				t.Errorf("hasUsableEngine = %v, want %v", got, tc.want)
			}
		})
	}
}
