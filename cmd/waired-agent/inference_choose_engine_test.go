//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// TestEngineInstalledOnHost_SeesAVLLMVenvWithEmptyPATH is the vLLM half
// of the #179 regression bar, and it is here rather than beside its
// ollama twin in engine_resolve_test.go because only linux can express
// it: the vLLM installer is stubbed on windows and darwin
// (internal/runtime/vllm_stub_*.go), so Active() there is always false
// and the portable test can pin the ABSENT case only.
//
// PRODUCT CONTRACT — #225. The rule is that engine presence comes from
// the state dir, never from $PATH; `vllm` is never on PATH for a venv
// install, so a PATH-shaped answer here is wrong by construction. Until
// this test existed the positive direction was unasserted at this call
// site, which is how the last un-unified arm survived PR #205.
func TestEngineInstalledOnHost_SeesAVLLMVenvWithEmptyPATH(t *testing.T) {
	sealPATH(t)
	stateDir := t.TempDir()

	if engineInstalledOnHost("linux", stateDir, catalog.RuntimeVLLM) {
		t.Fatal("an empty state dir reports vllm installed")
	}
	fakeVLLMVenv(t, stateDir)
	if !engineInstalledOnHost("linux", stateDir, catalog.RuntimeVLLM) {
		t.Error("a venv under the state dir is not seen as installed; " +
			"the daemon would report no_engine on a host that can serve")
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
	fakeBundledOllama(t, runtime.GOOS, stateDir)
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
	fakeBundledOllama(t, runtime.GOOS, stateDir)
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
	fakeBundledOllama(t, runtime.GOOS, stateDir)
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
	fakeBundledOllama(t, runtime.GOOS, stateDir)
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

// The no-engine reason names the term that ACTUALLY failed, per engine.
//
// PRODUCT CONTRACT — waired-agent#778. The reason used to be one literal
// sentence, "no engine viable: vllm needs GPU, ollama needs binary",
// returned whatever the chain had actually rejected each hop for. On the
// rc9 host in #778 the GPU was an idle RTX PRO 4000 and the real reason
// was that the venv had not been installed yet; the sentence named the
// GPU, and the campaign spent its investigation on GPU detection. A
// decision log that cannot be wrong about its own reason is the point.
func TestChooseEngine_NoEngineReasonNamesTheFailedTerm(t *testing.T) {
	sealPATH(t)
	for _, tc := range []struct {
		name     string
		cuda     bool
		venv     bool
		wantHas  []string
		wantNone []string
	}{
		{
			// The #778 host: a real CUDA GPU, no venv yet.
			name:     "cuda present, venv missing",
			cuda:     true,
			venv:     false,
			wantHas:  []string{"vllm: no installed venv", "ollama: no bundled binary"},
			wantNone: []string{"vllm needs GPU"},
		},
		{
			name:     "no cuda",
			cuda:     false,
			venv:     false,
			wantHas:  []string{"vllm: no CUDA", "ollama: no bundled binary"},
			wantNone: []string{"no installed venv"}, // CUDA is the first term to fail
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			if tc.venv {
				fakeVLLMVenv(t, stateDir)
			}
			store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
			prof := chooseEngineProfiler(t, tc.cuda)
			cfg := agentconfig.InferenceConfig{AllowAutoFallback: true}

			d, err := chooseEngine(context.Background(), store, prof, cfg, stateDir)
			if err != nil {
				t.Fatalf("chooseEngine: %v", err)
			}
			if !d.NoEngine {
				t.Fatalf("got engine=%q, want no-engine", d.Engine)
			}
			joined := strings.Join(d.Reasons, " | ")
			for _, want := range tc.wantHas {
				if !strings.Contains(joined, want) {
					t.Errorf("reasons %q do not mention %q", joined, want)
				}
			}
			for _, unwanted := range tc.wantNone {
				if strings.Contains(joined, unwanted) {
					t.Errorf("reasons %q still carry %q, which is not why this host declined", joined, unwanted)
				}
			}
		})
	}
}

// A chain hop the picker never walked must not appear in the reason. With
// the vLLM auto-select gate off the chain is ollama-only, so a sentence
// naming vLLM would describe a decision that was never taken.
//
// Record of today's behaviour: the gate is a var so an operator/build can
// pin ollama-only (internal/router/engine_picker.go:38-40); nothing
// ratifies what the log should say there, beyond not inventing a hop.
func TestChooseEngine_NoEngineReasonSkipsUnwalkedHops(t *testing.T) {
	sealPATH(t)
	defer func(prev bool) { router.VLLMAutoSelectable = prev }(router.VLLMAutoSelectable)
	router.VLLMAutoSelectable = false

	stateDir := t.TempDir()
	store := catalog.NewStore(filepath.Join(stateDir, "state.json"))
	prof := chooseEngineProfiler(t, true) // a capable GPU that the chain never asks about
	cfg := agentconfig.InferenceConfig{AllowAutoFallback: true}

	d, err := chooseEngine(context.Background(), store, prof, cfg, stateDir)
	if err != nil {
		t.Fatalf("chooseEngine: %v", err)
	}
	joined := strings.Join(d.Reasons, " | ")
	if strings.Contains(joined, "vllm") {
		t.Errorf("reasons %q name vllm, but the auto-select gate kept it out of the chain", joined)
	}
	if !strings.Contains(joined, "ollama: no bundled binary") {
		t.Errorf("reasons %q do not say why ollama declined", joined)
	}
}

// The two "can this host run vLLM" predicates ask DIFFERENT questions and
// can disagree on one host at one instant. This pins the disagreement so
// the next reader finds it stated rather than deduced from a contradictory
// status payload.
//
// Record of today's behaviour, NOT a product contract — nothing ratifies
// the split, and waired-agent#778 is where it first cost someone time:
//
//	router.VLLMAutoEligible  (internal/router/engine_picker.go)
//	    vendor == nvidia && vramMB >= MinVLLMVRAMMB && goos == linux
//	    "should the picker ADVERTISE vLLM for this class of host"
//	engineViable             (this package)
//	    hw.Accelerators.CUDA && an installed venv
//	    "can THIS process serve on vLLM right now"
//
// engine_picker.go:33-36 documents the venv half of the split on purpose
// (the picker advertises, the daemon declines until the venv exists). The
// CUDA-vs-vendor half is undocumented, and it is what let the rc9 host
// report `available_update: would swap to ... on vllm / VRAM fit: host
// GPU0=24467 MB` in the same payload as `subsystem_state: no_engine`.
// Whether to unify them is a design question (#778), deliberately not
// settled here.
func TestVLLMPredicates_AdvertiseAndServeAskDifferentQuestions(t *testing.T) {
	sealPATH(t)
	stateDir := t.TempDir() // capable hardware, no venv — the #778 shape

	const vendor, vram = "nvidia", router.MinVLLMVRAMMB
	if !router.VLLMAutoEligible("linux", vendor, vram) {
		t.Fatal("the picker declines a host it is documented to advertise for")
	}
	hw := hardware.Profile{Accelerators: hardware.Accelerators{CUDA: true}}
	if engineViable(catalog.RuntimeVLLM, hw, stateDir) {
		t.Fatal("the daemon claims it can serve vLLM with no venv installed")
	}
	// And the reason says so, rather than blaming the GPU the picker just
	// judged sufficient.
	_, why := engineViability(catalog.RuntimeVLLM, hw, stateDir)
	if !strings.Contains(why, "venv") {
		t.Errorf("reason %q does not name the venv; the two surfaces would read as contradicting each other", why)
	}
}

// engineViability is the reason-bearing form of engineViable; the two must
// never disagree about the verdict, or the log would explain a decision
// that was not taken.
func TestEngineViability_AgreesWithEngineViable(t *testing.T) {
	sealPATH(t)
	stateDir := t.TempDir()
	fakeVLLMVenv(t, stateDir)

	for _, cuda := range []bool{true, false} {
		hw := hardware.Profile{Accelerators: hardware.Accelerators{CUDA: cuda}}
		for _, engine := range []string{catalog.RuntimeVLLM, catalog.RuntimeOllama, "nonesuch"} {
			ok, why := engineViability(engine, hw, stateDir)
			if ok != engineViable(engine, hw, stateDir) {
				t.Errorf("engineViability(%q, cuda=%v) = %v, engineViable says otherwise", engine, cuda, ok)
			}
			if !ok && why == "" {
				t.Errorf("engineViability(%q, cuda=%v) declined without a reason", engine, cuda)
			}
			if ok && why != "" {
				t.Errorf("engineViability(%q, cuda=%v) accepted but returned reason %q", engine, cuda, why)
			}
		}
	}
}
