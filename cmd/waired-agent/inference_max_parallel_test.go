package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The build limit (catalog Variant.MaxParallel) is a product contract:
// the owner decided on 2026-09-17 to follow ollama's one-slot model
// families and to hold capacity to them from the catalog
// (waired-ai/waired-agent#1423). These tests pin where it reaches — the
// tuning's request, its recommendation, an admin override, the advertised
// capacity and the enforced admission ceiling — and that a build without
// one keeps today's behaviour.

// limited is v with the build limit set.
func limited(v catalog.Variant, n int) catalog.Variant {
	v.MaxParallel = n
	return v
}

func TestComputeOllamaTuning_BuildLimit(t *testing.T) {
	m := tuningTestManifest()
	hw := umaTwoSlotHost()
	free := m.Variants[0]
	capped := limited(free, 1)

	control := computeOllamaTuningOpts(m, free, hw, ollamaTuningOpts{KVCacheType: "q8_0"})
	if control.NumParallel != 2 || control.RecommendedMaxParallel < 2 {
		t.Fatalf("precondition: a build without a limit gets NumParallel %d, recommended %d on this host; "+
			"want 2 and at least 2, or the cases below assert a clamp nothing needed",
			control.NumParallel, control.RecommendedMaxParallel)
	}

	t.Run("the request and the recommendation stop at the limit", func(t *testing.T) {
		got := computeOllamaTuningOpts(m, capped, hw, ollamaTuningOpts{KVCacheType: "q8_0"})
		if got.NumParallel != 1 {
			t.Errorf("NumParallel = %d, want 1", got.NumParallel)
		}
		if got.RecommendedMaxParallel != 1 {
			t.Errorf("RecommendedMaxParallel = %d, want 1: advising an admin toward a slot "+
				"the engine never serves", got.RecommendedMaxParallel)
		}
		if !slices.Contains(got.Env(), "OLLAMA_NUM_PARALLEL=1") {
			t.Errorf("Env() = %v, want OLLAMA_NUM_PARALLEL=1", got.Env())
		}
		// The limit moves nothing else: same window, same cache type.
		if got.ContextLength != control.ContextLength || got.KVCacheType != control.KVCacheType {
			t.Errorf("window %d / kv %s, want the unlimited build's %d / %s",
				got.ContextLength, got.KVCacheType, control.ContextLength, control.KVCacheType)
		}
	})

	t.Run("an admin override stops at the limit without a warning", func(t *testing.T) {
		got := computeOllamaTuningOpts(m, capped, hw, ollamaTuningOpts{KVCacheType: "q8_0", OperatorParallel: 4})
		if got.NumParallel != 1 {
			t.Errorf("NumParallel = %d, want 1", got.NumParallel)
		}
		if strings.Contains(got.Warning, "recommended max") {
			t.Errorf("Warning = %q: holding an override to the build's limit trades nothing away", got.Warning)
		}
		// Control: the same override on a build without a limit is honoured.
		if got := computeOllamaTuningOpts(m, free, hw, ollamaTuningOpts{KVCacheType: "q8_0", OperatorParallel: 4}); got.NumParallel != 4 {
			t.Errorf("unlimited build: NumParallel = %d, want the override 4", got.NumParallel)
		}
	})

	t.Run("an override at or under the limit is untouched", func(t *testing.T) {
		got := computeOllamaTuningOpts(m, limited(free, 3), hw, ollamaTuningOpts{KVCacheType: "q8_0", OperatorParallel: 2})
		if got.NumParallel != 2 {
			t.Errorf("NumParallel = %d, want the override 2 under a limit of 3", got.NumParallel)
		}
	})

	t.Run("the spill branch and a degrade recompute stay under it", func(t *testing.T) {
		spill := tuningTestManifest()
		got := computeOllamaTuningOpts(spill, limited(spill.Variants[0], 1), discrete24GB(), ollamaTuningOpts{KVCacheType: "q8_0", OperatorParallel: 4})
		if got.NumParallel != 1 {
			t.Errorf("spill branch with override 4: NumParallel = %d, want 1", got.NumParallel)
		}
		down := computeOllamaTuningOpts(m, capped, hw, ollamaTuningOpts{KVCacheType: "q8_0", CeilingCtx: control.ContextLength - 1})
		if down.NumParallel != 1 {
			t.Errorf("degrade recompute: NumParallel = %d, want 1", down.NumParallel)
		}
	})
}

// Every bundled build that carries a limit is held to it on a host roomy
// enough to grant anything, even against an override of 8. Reads the real
// catalog so a build that gains a limit is covered without editing this.
func TestComputeOllamaTuning_BundledLimitsHold(t *testing.T) {
	ms, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, m := range ms {
		for _, v := range m.Variants {
			if v.MaxParallel <= 0 {
				continue
			}
			checked++
			got := computeOllamaTuningOpts(m, v, umaTwoSlotHost(), ollamaTuningOpts{KVCacheType: ollamaKVAuto, OperatorParallel: 8})
			if got.NumParallel > v.MaxParallel || got.RecommendedMaxParallel > v.MaxParallel {
				t.Errorf("%s/%s: NumParallel %d, recommended %d, want at most the build's %d",
					m.ModelID, v.VariantID, got.NumParallel, got.RecommendedMaxParallel, v.MaxParallel)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no bundled build carries a limit; this test checked nothing")
	}
}

func TestServingMaxParallel(t *testing.T) {
	manifests := []catalog.Manifest{{ModelID: "qwen", Variants: []catalog.Variant{
		{VariantID: "q4-gguf", RuntimeSupport: []string{catalog.RuntimeOllama}, MaxParallel: 1},
		{VariantID: "fp8", RuntimeSupport: []string{catalog.RuntimeVLLM}},
	}}, {ModelID: "free", Variants: []catalog.Variant{
		{VariantID: "q4-gguf", RuntimeSupport: []string{catalog.RuntimeOllama}},
	}}}
	ollamaServing := func(t *testing.T, tuning infruntime.ModelTuning) *agentInferenceProvider {
		t.Helper()
		a := newTestAdapter(t)
		if tuning != (infruntime.ModelTuning{}) {
			a.SetAppliedTuning(tuning)
		}
		return &agentInferenceProvider{manifests: manifests, ollama: a}
	}

	if got := ollamaServing(t, infruntime.ModelTuning{ModelID: "qwen", VariantID: "q4-gguf"}).ServingMaxParallel(); got != 1 {
		t.Errorf("ollama serving a limited build: %d, want 1", got)
	}
	if got := ollamaServing(t, infruntime.ModelTuning{ModelID: "free", VariantID: "q4-gguf"}).ServingMaxParallel(); got != 0 {
		t.Errorf("ollama serving a build without one: %d, want 0", got)
	}
	if got := ollamaServing(t, infruntime.ModelTuning{}).ServingMaxParallel(); got != 0 {
		t.Errorf("no tuning applied yet: %d, want 0", got)
	}
	// The ollama adapter still holds the limited build's tuning, but vLLM is
	// what serves: its batching is not ollama's scheduler.
	onVLLM := ollamaServing(t, infruntime.ModelTuning{ModelID: "qwen", VariantID: "q4-gguf"})
	onVLLM.setServingEngine(catalog.RuntimeVLLM)
	if got := onVLLM.ServingMaxParallel(); got != 0 {
		t.Errorf("vLLM serving: %d, want 0", got)
	}
	var nilProv *agentInferenceProvider
	if got := nilProv.ServingMaxParallel(); got != 0 {
		t.Errorf("nil provider: %d, want 0", got)
	}
}

// A measurement latched on the previous model can still be the advertised
// answer while a switch onto a limited build is verified; the limit bounds
// it anyway.
func TestCapacityFn_HeldToTheBuildLimit(t *testing.T) {
	manifests := []catalog.Manifest{{ModelID: "qwen", Variants: []catalog.Variant{
		{VariantID: "q4-gguf", RuntimeSupport: []string{catalog.RuntimeOllama}, MaxParallel: 1},
	}}}
	a := newTestAdapter(t)
	a.SetAppliedTuning(infruntime.ModelTuning{ModelID: "qwen", VariantID: "q4-gguf"})
	sub := &inferenceSubsystem{provider: &agentInferenceProvider{manifests: manifests, ollama: a}}
	if got := capacityFn(3, sub)(); got != 1 {
		t.Errorf("capacityFn = %d with a boot figure of 3 on a limited build, want 1", got)
	}
	// Control: without the limit the boot figure stands.
	a.SetAppliedTuning(infruntime.ModelTuning{ModelID: "other", VariantID: "q4-gguf"})
	if got := capacityFn(3, sub)(); got != 3 {
		t.Errorf("capacityFn = %d on a build without a limit, want the boot figure 3", got)
	}
}

func TestLocalAdmissionRelay_BuildLimit(t *testing.T) {
	limit := 1
	setup := func() (*localAdmissionRelay, *capacityRecorder) {
		rec := &capacityRecorder{}
		relay := &localAdmissionRelay{}
		relay.Set(newFloorServer(rec))
		relay.SetBuildLimit(func() int { return limit })
		return relay, rec
	}

	t.Run("an admin override from the map is held to the limit", func(t *testing.T) {
		limit = 1
		relay, rec := setup()
		relay.SetCapacityFromMap(4)
		if got := rec.enforced(); got != 1 {
			t.Errorf("enforced = %d, want 1 (applied: %v)", got, rec.applied)
		}
	})

	t.Run("a switch to a build without a limit gives the override back", func(t *testing.T) {
		limit = 1
		relay, rec := setup()
		relay.SetCapacityFromMap(4)
		limit = 0
		// Re-applied without a new figure arriving: only the served 4 the
		// relay kept can come back.
		relay.SetBuildLimit(func() int { return limit })
		if got := rec.enforced(); got != 4 {
			t.Errorf("enforced = %d, want 4: the relay must keep the served figure, "+
				"not the clamped one (applied: %v)", got, rec.applied)
		}
	})

	t.Run("the benchmark's figure is held too", func(t *testing.T) {
		limit = 1
		relay, rec := setup()
		relay.SeedCapacity(2)
		if got := rec.enforced(); got != 1 {
			t.Errorf("enforced = %d, want 1 (applied: %v)", got, rec.applied)
		}
	})

	t.Run("a limit installed after a figure is applied at once", func(t *testing.T) {
		rec := &capacityRecorder{}
		relay := &localAdmissionRelay{}
		relay.Set(newFloorServer(rec))
		relay.SetCapacityFromMap(4)
		relay.SetBuildLimit(func() int { return 1 })
		if got := rec.enforced(); got != 1 {
			t.Errorf("enforced = %d, want 1 (applied: %v)", got, rec.applied)
		}
	})

	t.Run("no limit installed is today's behaviour", func(t *testing.T) {
		rec := &capacityRecorder{}
		relay := &localAdmissionRelay{}
		relay.Set(newFloorServer(rec))
		relay.SetCapacityFromMap(4)
		if got := rec.enforced(); got != 4 {
			t.Errorf("enforced = %d, want 4 (applied: %v)", got, rec.applied)
		}
	})
}
