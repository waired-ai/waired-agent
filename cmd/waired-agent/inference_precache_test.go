package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
)

// precacheProvider is a CPU host with room for both fixture models, so
// which model gets pre-cached is decided by the picker rather than by
// whatever RAM the test machine happens to have.
func precacheProvider(t *testing.T, r download.CommandRunner) *agentInferenceProvider {
	t.Helper()
	return &agentInferenceProvider{
		store:     catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		cfg:       agentconfig.InferenceConfig{AllowPull: true},
		manifests: recTestManifests(), // heavy (tier 50) / light (tier 20)
		puller:    download.NewPuller("ollama-fake", r),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		agentCtx:  context.Background(),
		profiler: hardware.NewProfiler(t.TempDir(),
			hardware.WithRAM(func(context.Context) (int, int, error) { return 16, 16, nil }),
			hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
				return nil, hardware.Accelerators{}, nil
			}),
			hardware.WithEngineVersion(func(_ context.Context, name string) (bool, string) {
				return name == "ollama", "0.31.0"
			}),
		),
	}
}

// PRODUCT CONTRACT: pre-caching an UPDATE presupposes something to
// update.
//
// computeAvailableUpdate has a "No active yet -> the picker output is
// itself the update" branch, which is right for the /inference/status
// field it also feeds but wrong as a trigger to download: on a fresh
// install state.Active is nil, so the 30-second pre-cache goroutine
// dispatched a THIRD multi-GB pull alongside the operator's model and the
// bundled fallback (#306).
func TestMaybePreCache_SkipsWhenNothingIsActive(t *testing.T) {
	r := &scriptedRunner{results: []error{nil}}
	p := precacheProvider(t, r)

	p.maybePreCache(context.Background())
	p.waitForPulls()

	if got := r.calls(); got != 0 {
		t.Fatalf("pulls dispatched with no active model = %d, want 0", got)
	}
}

// The other half of the contract: this is a suppression, not a removal.
// A host that IS serving a model must still pre-fetch the better
// candidate, so `waired runtimes refresh` stays a near-instant swap.
func TestMaybePreCache_StillRunsWhenAModelIsActive(t *testing.T) {
	r := &scriptedRunner{results: []error{nil}}
	p := precacheProvider(t, r)
	if err := p.store.Update(func(s *catalog.State) {
		s.Models["light"] = catalog.ModelState{
			State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "light:2b",
		}
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: "light", VariantID: "q4",
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	p.maybePreCache(context.Background())
	p.waitForPulls()

	if got := r.calls(); got != 1 {
		t.Fatalf("pulls dispatched while serving the lighter model = %d, want 1 "+
			"(the better candidate is still worth pre-fetching)", got)
	}
	if got := modelStateOf(t, p, "heavy").State; got != catalog.ModelStateReady {
		t.Fatalf("pre-cached model state = %q, want %q", got, catalog.ModelStateReady)
	}
}

// --- an update that is only a better VARIANT is not already cached (#361) ---

// precacheVariantManifests is recTestManifests with a second, better
// variant on the heavy family — floored the way the shipping qwen3.6
// entries are, so the picker takes it only when the engine version
// clears the floor.
func precacheVariantManifests() []catalog.Manifest {
	ms := recTestManifests()
	for i := range ms {
		if ms[i].ModelID != "heavy" {
			continue
		}
		ms[i].Variants = append([]catalog.Variant{{
			VariantID: "mtp-q4", Format: "ollama-tag", Quantization: "Q4_K_M",
			RuntimeSupport: []string{"ollama"}, EstimatedWeightGB: 5.0,
			MinRAMGB: 12, QualityTier: 51, ParamCount: 8_000_000_000,
			MinEngineVersion: "0.30.0",
			Source:           catalog.VariantSource{Type: "ollama", Tag: "heavy:8b-mtp"},
		}}, ms[i].Variants...)
	}
	return ms
}

func heavyPick(t *testing.T, variantID string) router.Pick {
	t.Helper()
	for _, m := range precacheVariantManifests() {
		if m.ModelID != "heavy" {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == variantID {
				return router.Pick{Manifest: m, Variant: v}
			}
		}
	}
	t.Fatalf("fixture has no heavy/%s variant", variantID)
	return router.Pick{}
}

// PRODUCT CONTRACT (#361): PreCached answers "are THESE weights on this
// disk", and weights are identified by variant.
//
// computeAvailableUpdate reports an update when Active differs from the
// pick in the model id OR the variant. Reading only the model id made
// every variant-only update claim to be already cached, which is what
// left a host that resolved its variant blind with nothing able to move
// it: maybePreCache returns early on PreCached.
func TestAvailableUpdateFromPick_AVariantOnlyUpdateIsNotPreCached(t *testing.T) {
	state := catalog.State{Models: map[string]catalog.ModelState{
		"heavy": {State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "heavy:8b"},
	}}

	upd := availableUpdateFromPick(catalog.RuntimeOllama, heavyPick(t, "mtp-q4"), state)

	if upd == nil {
		t.Fatal("availableUpdateFromPick returned nil")
	}
	if upd.PreCached {
		t.Error("PreCached = true for a variant that is not on disk — only heavy/q4 is")
	}
	if upd.ExpectedSwapSeconds != 60 {
		t.Errorf("ExpectedSwapSeconds = %d, want 60 — the weights still have to be fetched",
			upd.ExpectedSwapSeconds)
	}
}

// The positive control, so the fix cannot be "always false": the variant
// that IS on disk still reports the near-instant swap.
func TestAvailableUpdateFromPick_TheOnDiskVariantIsPreCached(t *testing.T) {
	state := catalog.State{Models: map[string]catalog.ModelState{
		"heavy": {State: catalog.ModelStateReady, VariantID: "mtp-q4", OllamaTag: "heavy:8b-mtp"},
	}}

	upd := availableUpdateFromPick(catalog.RuntimeOllama, heavyPick(t, "mtp-q4"), state)

	if upd == nil {
		t.Fatal("availableUpdateFromPick returned nil")
	}
	if !upd.PreCached {
		t.Error("PreCached = false for the variant recorded Ready on disk")
	}
	if upd.ExpectedSwapSeconds != 5 {
		t.Errorf("ExpectedSwapSeconds = %d, want 5", upd.ExpectedSwapSeconds)
	}
}

// The behaviour that early return was suppressing: a host serving the
// lower variant of the right model fetches the better one. This is the
// recovery path for every host that pulled blind before #361 — nothing
// else re-pulls a model that is already Ready.
func TestMaybePreCache_FetchesABetterVariantOfTheModelAlreadyServed(t *testing.T) {
	r := &scriptedRunner{results: []error{nil}}
	p := precacheProvider(t, r)
	p.manifests = precacheVariantManifests()
	if err := p.store.Update(func(s *catalog.State) {
		s.Models["heavy"] = catalog.ModelState{
			State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "heavy:8b",
		}
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: "heavy", VariantID: "q4",
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	p.maybePreCache(context.Background())
	p.waitForPulls()

	if got := r.calls(); got != 1 {
		t.Fatalf("pulls dispatched for the better variant = %d, want 1", got)
	}
	if got := modelStateOf(t, p, "heavy").VariantID; got != "mtp-q4" {
		t.Errorf("recorded variant after the pre-cache = %q, want mtp-q4", got)
	}
}
