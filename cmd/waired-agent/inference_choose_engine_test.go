//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
)

// fakeVLLMVenv lays down the on-disk shape VLLMInstaller.Active() checks:
// <stateDir>/runtimes/vllm/current -> <version>, with
// <version>/.venv/bin/python present. That is enough to make
// engineViable("vllm") return true once CUDA is also reported.
func fakeVLLMVenv(t *testing.T, stateDir string) {
	t.Helper()
	base := filepath.Join(stateDir, "runtimes", "vllm")
	binDir := filepath.Join(base, "0.11.0", ".venv", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "python"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("0.11.0", filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
}

// chooseEngineProfiler builds a Profiler whose GPU + engine detection are
// seeded so chooseEngine's viability checks are deterministic on a
// GPU-less CI host.
func chooseEngineProfiler(t *testing.T, cuda, ollamaInstalled bool) *hardware.Profiler {
	t.Helper()
	return hardware.NewProfiler(t.TempDir(),
		hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
			return nil, hardware.Accelerators{CUDA: cuda}, nil
		}),
		hardware.WithEngineVersion(func(_ context.Context, name string) (bool, string) {
			if name == "ollama" {
				return ollamaInstalled, "0.30.0"
			}
			return false, ""
		}),
	)
}

// preferred_engine="vllm" on a viable host is the explicit opt-in (#557):
// chooseEngine returns vllm with a "preference" provenance.
func TestChooseEngine_PreferredVLLM_OptsIn(t *testing.T) {
	stateDir := t.TempDir()
	fakeVLLMVenv(t, stateDir)
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, true, true)
	cfg := agentconfig.InferenceConfig{PreferredEngine: catalog.RuntimeVLLM, AllowAutoFallback: true}

	d, err := chooseEngine(context.Background(), store, prof, cfg, stateDir)
	if err != nil {
		t.Fatalf("chooseEngine: %v", err)
	}
	if d.Engine != catalog.RuntimeVLLM || d.Source != "preference" {
		t.Fatalf("got engine=%q source=%q, want vllm/preference", d.Engine, d.Source)
	}
}

// The default since #557 landed (router.VLLMAutoSelectable=true): a fully
// vLLM-capable host (NVIDIA + CUDA + installed venv) with NO explicit
// preference auto-picks vLLM. This is the #153 behaviour change — the
// regression lock that keeps the new default on.
func TestChooseEngine_NoPreference_AutoPicksVLLM(t *testing.T) {
	stateDir := t.TempDir()
	fakeVLLMVenv(t, stateDir)
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, true, true)
	cfg := agentconfig.InferenceConfig{AllowAutoFallback: true} // no PreferredEngine

	d, err := chooseEngine(context.Background(), store, prof, cfg, stateDir)
	if err != nil {
		t.Fatalf("chooseEngine: %v", err)
	}
	if d.Engine != catalog.RuntimeVLLM {
		t.Fatalf("got engine=%q, want vllm (auto-select on by default, #153)", d.Engine)
	}
}

// The gate is a var, not a hard wire: gated off, the same capable host
// stays on Ollama. Pins that an operator/build can still opt out.
func TestChooseEngine_AutoSelectGatedOff_StaysOllama(t *testing.T) {
	old := router.VLLMAutoSelectable
	router.VLLMAutoSelectable = false
	t.Cleanup(func() { router.VLLMAutoSelectable = old })

	stateDir := t.TempDir()
	fakeVLLMVenv(t, stateDir)
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, true, true)
	cfg := agentconfig.InferenceConfig{AllowAutoFallback: true}

	d, err := chooseEngine(context.Background(), store, prof, cfg, stateDir)
	if err != nil {
		t.Fatalf("chooseEngine: %v", err)
	}
	if d.Engine != catalog.RuntimeOllama {
		t.Fatalf("got engine=%q, want ollama (auto-select gated off)", d.Engine)
	}
}

// Auto-select is advisory, not a force: with the gate on but NO vLLM venv
// installed, engineViable declines vLLM and the chain falls straight
// through to Ollama. A capable host is only switched once the venv exists.
func TestChooseEngine_AutoPickVLLM_NoVenv_StaysOllama(t *testing.T) {
	stateDir := t.TempDir() // capable hardware below, but no venv laid down
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, true, true) // CUDA present, ollama installed
	cfg := agentconfig.InferenceConfig{AllowAutoFallback: true}

	d, err := chooseEngine(context.Background(), store, prof, cfg, stateDir)
	if err != nil {
		t.Fatalf("chooseEngine: %v", err)
	}
	if d.Engine != catalog.RuntimeOllama {
		t.Fatalf("got engine=%q, want ollama (no vLLM venv, auto-pick declines)", d.Engine)
	}
}

// A preferred engine that isn't viable (no CUDA / no venv) falls back to a
// viable Ollama when AllowAutoFallback is set, rather than failing boot.
func TestChooseEngine_PreferredVLLM_NotViable_FallsBack(t *testing.T) {
	stateDir := t.TempDir() // no venv laid down
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, false, true) // no CUDA
	cfg := agentconfig.InferenceConfig{PreferredEngine: catalog.RuntimeVLLM, AllowAutoFallback: true}

	d, err := chooseEngine(context.Background(), store, prof, cfg, stateDir)
	if err != nil {
		t.Fatalf("chooseEngine: %v", err)
	}
	if d.Engine != catalog.RuntimeOllama {
		t.Fatalf("got engine=%q, want ollama fallback", d.Engine)
	}
}
