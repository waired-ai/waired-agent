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

// chooseEngineProfiler builds a Profiler whose GPU detection is seeded
// so chooseEngine's CUDA gate is deterministic on a GPU-less CI host.
//
// It no longer decides whether OLLAMA is viable: since #179 that comes
// from engineInstalledOnHost — an on-disk resolution — so the tests
// below express "ollama is installed" by laying one down with
// fakeBundledOllama rather than by seeding the profiler's PATH probe.
// The engine-version seam is kept wired (the profile's Version field is
// still read elsewhere) but reports a fixed version.
func chooseEngineProfiler(t *testing.T, cuda bool) *hardware.Profiler {
	t.Helper()
	return hardware.NewProfiler(t.TempDir(),
		hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
			return nil, hardware.Accelerators{CUDA: cuda}, nil
		}),
		hardware.WithEngineVersion(func(_ context.Context, name string) (bool, string) {
			if name == "ollama" {
				return true, "0.30.0"
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
	prof := chooseEngineProfiler(t, true)
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
	prof := chooseEngineProfiler(t, true)
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
	fakeBundledOllama(t, stateDir)
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, true)
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
	fakeBundledOllama(t, stateDir)
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, true)
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
	fakeBundledOllama(t, stateDir)
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, false) // no CUDA
	cfg := agentconfig.InferenceConfig{PreferredEngine: catalog.RuntimeVLLM, AllowAutoFallback: true}

	d, err := chooseEngine(context.Background(), store, prof, cfg, stateDir)
	if err != nil {
		t.Fatalf("chooseEngine: %v", err)
	}
	if d.Engine != catalog.RuntimeOllama {
		t.Fatalf("got engine=%q, want ollama fallback", d.Engine)
	}
}

// THE #179 REGRESSION BAR at the boot-decision level. A Linux host whose
// engine waired installed itself — under the state dir, deliberately NOT
// on $PATH — must boot onto ollama. engineViable used to read the
// profiler's PATH probe, so this host chose "no-engine" at boot and then
// resolved a binary anyway: the two halves of the same daemon disagreed
// about whether an engine existed.
func TestChooseEngine_StateDirOllamaWithEmptyPATH(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "")
	stateDir := t.TempDir()
	fakeBundledOllama(t, stateDir)
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, false) // no GPU, so vLLM is out of the chain
	cfg := agentconfig.InferenceConfig{AllowAutoFallback: true}

	d, err := chooseEngine(context.Background(), store, prof, cfg, stateDir)
	if err != nil {
		t.Fatalf("chooseEngine: %v", err)
	}
	if d.Engine != catalog.RuntimeOllama || d.NoEngine {
		t.Fatalf("got engine=%q no_engine=%v, want ollama (state-dir install, nothing on PATH)",
			d.Engine, d.NoEngine)
	}
}

// The other direction, and the reason the Linux rule is strict: waired
// must not adopt a system ollama that happens to be on $PATH. That
// binary is not the pinned engine the daemon would spawn, so calling the
// host viable would leave the bootstrap resolving nothing. Since #489
// there is no mode in which it counts.
func TestChooseEngine_LinuxIgnoresSystemOllamaOnPATH(t *testing.T) {
	t.Setenv("WAIRED_OLLAMA_BINARY", "")
	pathDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pathDir, "ollama"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	stateDir := t.TempDir() // no bundled engine
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, false)
	cfg := agentconfig.InferenceConfig{AllowAutoFallback: true}

	d, err := chooseEngine(context.Background(), store, prof, cfg, stateDir)
	if err != nil {
		t.Fatalf("chooseEngine: %v", err)
	}
	if !d.NoEngine {
		t.Fatalf("got engine=%q source=%q, want no-engine (a system ollama is not the waired-managed engine)",
			d.Engine, d.Source)
	}
}
