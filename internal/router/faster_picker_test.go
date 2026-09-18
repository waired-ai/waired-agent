package router

import (
	"fmt"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// stepHost is a 24 GB NVIDIA card with 64 GB of system memory: every
// stepCatalog model below holds a ~200k window resident on it except the
// ones built not to.
func stepHost() hardware.Profile {
	return hardware.Profile{
		OS: "linux", Arch: "x86_64", RAMTotalGB: 64,
		GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24 * 1024}},
	}
}

// stepModel is a one-variant ollama model with a 262k native window and a
// small KV cache, so whether it fits the step host is decided by weights.
func stepModel(id string, weightGB float64, tier int) catalog.Manifest {
	return catalog.Manifest{
		ModelID: id, ContextLength: 262144,
		Capabilities: []string{"chat", "tool_use"},
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: "ollama-tag",
			Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
			EstimatedWeightGB: weightGB, MinRAMGB: 8, QualityTier: tier,
			ParamCount: 8_000_000_000, KVBytesPerTokenFP16: 4096,
			Source: catalog.VariantSource{Type: "ollama", Tag: id + ":q4_K_M"},
		}},
	}
}

// speeds builds a TurnSpeedFor over model ids.
func speeds(byModel map[string]float64) func(catalog.Manifest, catalog.Variant) (float64, bool) {
	return func(m catalog.Manifest, _ catalog.Variant) (float64, bool) {
		s, ok := byModel[m.ModelID]
		return s, ok
	}
}

func stepInput(cat []catalog.Manifest, byModel map[string]float64) PickInput {
	return PickInput{
		Catalog: cat, Hardware: stepHost(), Engine: catalog.RuntimeOllama,
		EngineVersion: runtime.OllamaPinnedVersion,
		TurnSpeedFor:  speeds(byModel),
	}
}

func pickID(p Pick, ok bool) string {
	if !ok {
		return ""
	}
	return p.Manifest.ModelID
}

// Product contract: decision 2 of
// docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md
// (owner, 2026-09-16). The step-down offers the first ranked candidate
// that fits this host and is faster on the reference host class — even
// when it is HEAVIER than the active model, which is the case the
// weight rule got wrong: a 3B-active mixture of experts outruns a dense
// 27B of lower weight.
func TestFasterCandidate_OffersAHeavierModelThatIsFaster(t *testing.T) {
	cat := []catalog.Manifest{
		stepModel("dense-27b", 14.0, 90),  // active
		stepModel("moe-35b", 16.0, 80),    // heavier, much faster
		stepModel("small-dense", 5.0, 70), // lighter, faster
	}
	got := pickID(FasterCandidate(stepInput(cat, map[string]float64{
		"dense-27b": 228, "moe-35b": 70, "small-dense": 60,
	}), "dense-27b", "q4-gguf"))
	if got != "moe-35b" {
		t.Errorf("got %q, want moe-35b: the first ranked candidate that is faster, heavier or not", got)
	}
}

// The 5% factor (decision 2 of docs/decisions/20260916/0340: 「5%ぐらいにする」).
func TestFasterCandidate_NeedsToBeFivePercentFaster(t *testing.T) {
	cat := []catalog.Manifest{
		stepModel("active", 10.0, 90),
		stepModel("barely", 9.0, 80),
		stepModel("enough", 8.0, 70),
	}
	got := pickID(FasterCandidate(stepInput(cat, map[string]float64{
		"active": 100, "barely": 96, "enough": 95,
	}), "active", "q4-gguf"))
	if got != "enough" {
		t.Errorf("got %q, want enough: 96 s against 100 s is within noise, 95 s is not", got)
	}
	got = pickID(FasterCandidate(stepInput(cat, map[string]float64{
		"active": 100, "barely": 96, "enough": 95.1,
	}), "active", "q4-gguf"))
	if got != "" {
		t.Errorf("got %q, want no offer: nothing is 5%% faster", got)
	}
}

// Rank order first, as before (waired-agent#834): of two candidates that
// both qualify, the higher-ranked one is offered even when the other is
// faster still.
func TestFasterCandidate_FirstInRankOrder(t *testing.T) {
	cat := []catalog.Manifest{
		stepModel("active", 10.0, 90),
		stepModel("better", 9.0, 80),
		stepModel("fastest", 3.0, 40),
	}
	got := pickID(FasterCandidate(stepInput(cat, map[string]float64{
		"active": 200, "better": 150, "fastest": 20,
	}), "active", "q4-gguf"))
	if got != "better" {
		t.Errorf("got %q, want better (rank order, not the fastest)", got)
	}
}

// Fully resident here, checked even when RankModels' recommendation pass
// stood down. A candidate this host cannot hold is not an offer.
func TestFasterCandidate_SkipsWhatThisHostCannotHold(t *testing.T) {
	cat := []catalog.Manifest{
		stepModel("active", 10.0, 90),
		stepModel("too-big", 60.0, 80), // spills a 24 GB card
		stepModel("fits", 6.0, 70),
	}
	got := pickID(FasterCandidate(stepInput(cat, map[string]float64{
		"active": 200, "too-big": 50, "fits": 150,
	}), "active", "q4-gguf"))
	if got != "fits" {
		t.Errorf("got %q, want fits: too-big is faster on the reference host but not resident here", got)
	}
	// RankModels stands its residency pass down when nothing would be left,
	// so on a host where no candidate is resident the ranked list still
	// holds the spills. The check here is what keeps one from being offered.
	spills := []catalog.Manifest{
		stepModel("active", 60.0, 90),
		stepModel("too-big", 50.0, 80),
	}
	if got := pickID(FasterCandidate(stepInput(spills, map[string]float64{
		"active": 400, "too-big": 100,
	}), "active", "q4-gguf")); got != "" {
		t.Errorf("got %q, want no offer: nothing faster is resident on this host", got)
	}
}

// A variant this host measured over the line is never offered
// (waired-agent#784), whatever the reference host says.
func TestFasterCandidate_SkipsWhatThisHostMeasuredSlow(t *testing.T) {
	cat := []catalog.Manifest{
		stepModel("active", 10.0, 90),
		stepModel("slow-here", 9.0, 80),
		stepModel("next", 6.0, 70),
	}
	in := stepInput(cat, map[string]float64{"active": 300, "slow-here": 100, "next": 120})
	in.TurnBudgetSeconds = 190
	in.Measured = map[string]MeasuredRate{
		catalog.VariantSHA(cat[1].Variants[0]): {TurnSeconds: 260},
	}
	if got := pickID(FasterCandidate(in, "active", "q4-gguf")); got != "next" {
		t.Errorf("got %q, want next: slow-here measured 260 s on this host", got)
	}
	// RankModels stands its measured-slow pass down when every candidate
	// measured slow, so the ranked list then holds them again. The check
	// here is what keeps one from being offered.
	allSlow := stepInput(cat[:2], map[string]float64{"active": 300, "slow-here": 100})
	allSlow.TurnBudgetSeconds = 190
	allSlow.Measured = map[string]MeasuredRate{
		catalog.VariantSHA(cat[0].Variants[0]): {TurnSeconds: 300},
		catalog.VariantSHA(cat[1].Variants[0]): {TurnSeconds: 260},
	}
	if got := pickID(FasterCandidate(allSlow, "active", "q4-gguf")); got != "" {
		t.Errorf("got %q, want no offer: slow-here measured 260 s on this host", got)
	}
}

// Never another variant of the active model (waired-agent#754).
func TestFasterCandidate_NeverTheActiveModel(t *testing.T) {
	dual := stepModel("dual", 10.0, 90)
	fast := dual.Variants[0]
	fast.VariantID, fast.QualityTier, fast.EstimatedWeightGB = "q2-gguf", 89, 5.0
	fast.Source.Tag = "dual:q2"
	dual.Variants = append(dual.Variants, fast)
	cat := []catalog.Manifest{dual, stepModel("other", 6.0, 70)}
	in := stepInput(cat, nil)
	in.TurnSpeedFor = func(m catalog.Manifest, v catalog.Variant) (float64, bool) {
		switch m.ModelID + "/" + v.VariantID {
		case "dual/q4-gguf":
			return 200, true
		case "dual/q2-gguf":
			return 50, true
		case "other/q4-gguf":
			return 150, true
		}
		return 0, false
	}
	if got := pickID(FasterCandidate(in, "dual", "q4-gguf")); got != "other" {
		t.Errorf("got %q, want other: dual/q2-gguf is the same model", got)
	}
	// And with no other model, no offer at all.
	in.Catalog = cat[:1]
	if p, ok := FasterCandidate(in, "dual", "q4-gguf"); ok {
		t.Errorf("got %s/%s, want no offer", p.Manifest.ModelID, p.Variant.VariantID)
	}
}

// Seconds nobody recorded cannot be compared: an active variant with none
// gets no offer, and a candidate with none is never one.
func TestFasterCandidate_NoSecondsNoComparison(t *testing.T) {
	cat := []catalog.Manifest{
		stepModel("active", 10.0, 90),
		stepModel("unknown", 9.0, 80),
		stepModel("known", 6.0, 70),
	}
	if got := pickID(FasterCandidate(stepInput(cat, map[string]float64{
		"active": 200, "known": 100,
	}), "active", "q4-gguf")); got != "known" {
		t.Errorf("got %q, want known: unknown has no seconds", got)
	}
	if got := pickID(FasterCandidate(stepInput(cat, map[string]float64{
		"unknown": 50, "known": 100,
	}), "active", "q4-gguf")); got != "" {
		t.Errorf("got %q, want no offer: the active variant has no seconds", got)
	}
}

// An active variant the catalog cannot resolve falls back to the top
// ranked pick as the baseline, and the different-model skip still holds
// (waired-agent#754).
func TestFasterCandidate_ActiveNotInCatalog(t *testing.T) {
	cat := []catalog.Manifest{
		stepModel("top", 10.0, 90),
		stepModel("next", 6.0, 70),
	}
	got := pickID(FasterCandidate(stepInput(cat, map[string]float64{
		"top": 200, "next": 100,
	}), "top", "renamed-variant"))
	if got != "next" {
		t.Errorf("got %q, want next", got)
	}
}

// The doc comment's termination argument: every accepted step is at least
// 5% faster than the one before, so following the offers ends.
func TestFasterCandidate_ChainTerminates(t *testing.T) {
	cat := []catalog.Manifest{
		stepModel("a", 12.0, 90),
		stepModel("b", 11.0, 80),
		stepModel("c", 10.0, 70),
		stepModel("d", 9.0, 60),
	}
	secs := map[string]float64{"a": 300, "b": 280, "c": 200, "d": 190}
	in := stepInput(cat, secs)
	active, steps := "a", 0
	for {
		p, ok := FasterCandidate(in, active, "q4-gguf")
		if !ok {
			break
		}
		if secs[p.Manifest.ModelID] > secs[active]*FasterStepFactor {
			t.Fatalf("%s -> %s is not 5%% faster", active, p.Manifest.ModelID)
		}
		active = p.Manifest.ModelID
		if steps++; steps > len(cat) {
			t.Fatal("the chain did not terminate")
		}
	}
	if steps == 0 {
		t.Fatal("no step taken; the test proves nothing")
	}
}

// The same property on the shipped catalog and the shipped records, on the
// host class they were taken on. Record of today's behaviour: from a dense
// 27B the chain passes through flash-next (itself over the line) to the
// 35B-A3B and on down, each step at least 5% faster by the recorded seconds.
// It also fails when the shipped store answers nothing — an import that
// disarms the step-down would otherwise go unnoticed, as FasterCandidate
// reports that as "no faster model" rather than an error.
func TestFasterCandidate_ShippedChainTerminates(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	store, err := catalog.TurnSpeeds()
	if err != nil {
		t.Fatalf("TurnSpeeds: %v", err)
	}
	in := PickInput{
		Catalog:       manifests,
		Hardware:      reviewHostStrixHalo(),
		Engine:        catalog.RuntimeOllama,
		EngineVersion: runtime.OllamaPinnedVersion,
	}
	modelID, variantID := "qwen3.5-27b", "q4-gguf"
	m, v, ok := findCatalogPair(manifests, modelID, variantID)
	if !ok {
		t.Fatalf("the catalog no longer carries %s/%s", modelID, variantID)
	}
	prev, _, ok := store.For(m, v)
	if !ok {
		t.Fatalf("no recorded seconds for %s/%s", modelID, variantID)
	}
	var path []string
	for {
		p, ok := FasterCandidate(in, modelID, variantID)
		if !ok {
			break
		}
		secs, _, ok := store.For(p.Manifest, p.Variant)
		if !ok || secs > prev*FasterStepFactor {
			t.Fatalf("%s/%s (%.0f s) -> %s/%s (%.0f s, recorded=%v) is not 5%% faster",
				modelID, variantID, prev, p.Manifest.ModelID, p.Variant.VariantID, secs, ok)
		}
		modelID, variantID, prev = p.Manifest.ModelID, p.Variant.VariantID, secs
		path = append(path, fmt.Sprintf("%s/%s %.0f s", modelID, variantID, secs))
		if len(path) > len(manifests) {
			t.Fatalf("the chain did not terminate: %v", path)
		}
	}
	t.Logf("chain: %v", path)
	// Anti-vacuity: a chain of zero or one step satisfies everything above.
	if len(path) < 2 {
		t.Errorf("chain %v — the shipped records no longer give this host a multi-step chain, so nothing above was exercised", path)
	}
}

// reviewHostStrixHalo is the v0.0.3-rc2 review host (waired-ai/waired#1223):
// a Windows Ryzen AI Max 395 with a Radeon 8060S iGPU, 128 GB installed and
// a 96 GB GPU budget — the reference host class the records were taken on.
func reviewHostStrixHalo() hardware.Profile {
	return hardware.Profile{
		OS: "windows", Arch: "amd64",
		RAMTotalGB:    128,
		UnifiedMemory: true,
		UsableVRAMMB:  96 * 1024,
		GPUs:          []hardware.GPU{{Vendor: "amd", Model: "Radeon 8060S (synthetic)"}},
	}
}

// manual_only and internal_only models are never offered: FasterCandidate
// ranks through RankModels, which withholds both.
func TestFasterCandidate_NeverAWithheldModel(t *testing.T) {
	held := stepModel("held", 9.0, 80)
	held.ManualOnly = "test fixture"
	internal := stepModel("internal", 8.0, 75)
	internal.InternalOnly = "test fixture"
	cat := []catalog.Manifest{stepModel("active", 10.0, 90), held, internal, stepModel("offered", 6.0, 70)}
	got := pickID(FasterCandidate(stepInput(cat, map[string]float64{
		"active": 200, "held": 50, "internal": 50, "offered": 100,
	}), "active", "q4-gguf"))
	if got != "offered" {
		t.Errorf("got %q, want offered", got)
	}
}

// The reported case on the shipped catalog: an Apple M5 Pro with 48 GB
// measured qwen3.8-27b at 228 s per request and qwen3.6-35b-a3b at 70 s
// (docs/decisions/20260913/2245). With the 27B over the line, the offer is
// the 35B-A3B, which the weight rule could not offer because its Q4 build
// is heavier. The seconds are injected so this test does not depend on the
// reference host's records.
func TestFasterCandidate_ShippedCatalogOffersTheMoEOverTheDense27B(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	in := PickInput{
		Catalog:       manifests,
		Hardware:      syntheticAppleUMA(48, 0),
		Engine:        catalog.RuntimeOllama,
		EngineVersion: runtime.OllamaPinnedVersion,
		TurnSpeedFor: speeds(map[string]float64{
			"qwen3.8-27b": 228, "qwen3.6-35b-a3b": 70, "qwen3.5-35b-a3b": 75,
			"qwen3.5-27b": 230, "qwen3.5-9b": 60, "qwen3.5-4b": 35,
		}),
	}
	p, ok := FasterCandidate(in, "qwen3.8-27b", "mtp-q4-gguf")
	if !ok || p.Manifest.ModelID != "qwen3.6-35b-a3b" {
		t.Fatalf("got %q ok=%v, want qwen3.6-35b-a3b", p.Manifest.ModelID, ok)
	}
	if !p.Recommendation.Fits {
		t.Errorf("offered %s/%s, which is not resident on this host", p.Manifest.ModelID, p.Variant.VariantID)
	}
}
