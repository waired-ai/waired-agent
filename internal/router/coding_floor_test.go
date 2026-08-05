package router

import (
	"math"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The anchor host for every #624 calibration: RTX PRO 4000 Blackwell
// (24467 MiB) serving qwen3.6-35b mtp (22.62 GB measured weights,
// kv_bytes_per_token_fp16 20480). Measured in
// docs/reports/20260704-mtp-vs-spill-24gb.md: no-spill max window
// 114688, 200704 loads with 13.5% measured spill at usable decode.
func anchorHost() hardware.Profile {
	return hardware.Profile{
		RAMTotalGB: 120,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
}

func floorManifest(ctxLen int) catalog.Manifest {
	return catalog.Manifest{ModelID: "floor-test", ContextLength: ctxLen}
}

func TestEffectiveContextFloor(t *testing.T) {
	if got := EffectiveContextFloor(floorManifest(262144)); got != CodingAgentContextFloorTokens {
		t.Errorf("262144-manifest floor = %d, want %d", got, CodingAgentContextFloorTokens)
	}
	if got := EffectiveContextFloor(floorManifest(131072)); got != 131072 {
		t.Errorf("131072-manifest floor = %d, want capped 131072", got)
	}
	if got := EffectiveContextFloor(floorManifest(0)); got != CodingAgentContextFloorTokens {
		t.Errorf("unknown-context manifest floor = %d, want %d", got, CodingAgentContextFloorTokens)
	}
}

func TestMeetsNativeContextFloor(t *testing.T) {
	if !MeetsNativeContextFloor(floorManifest(262144)) {
		t.Error("262144 must pass the native floor")
	}
	for _, ctx := range []int{131072, 32768} {
		if MeetsNativeContextFloor(floorManifest(ctx)) {
			t.Errorf("%d must fail the native floor", ctx)
		}
	}
}

func TestOllamaServesContextFloor_AnchorBoundedSpill(t *testing.T) {
	m := floorManifest(262144)
	v := catalog.Variant{EstimatedWeightGB: 22.62, KVBytesPerTokenFP16: 20480}
	ok, spill := OllamaServesContextFloor(m, v, anchorHost())
	if !ok {
		t.Fatalf("anchor host must pass via bounded spill (expected spill %.3f)", spill)
	}
	// predicted ≈ 3.9% × calibration 3.0 ≈ 11.7% — well under the 20% bound.
	if math.Abs(spill-0.117) > 0.01 {
		t.Errorf("expected spill fraction = %.4f, want ≈ 0.117", spill)
	}
}

// TestOllamaServesContextFloor_HeavierVariantIsTheRecommendGatesToDrop
// is the #625 judgment (mtp dominant on the 24 GB anchor) after
// waired#1031 moved WHICH pass enforces it.
//
// The non-MTP tag's measured weight (23.9 GB) does not fit the anchor's
// card even before any KV, so this gate now passes it: a card that
// cannot hold the weights leaves the host no worse off than the same
// host without it, and gating there made adding VRAM remove models. The
// spill is still reported, and it is the RECOMMENDATION gate — the pass
// that owns "worth serving at all" — that keeps the tag from being
// auto-selected on this card.
func TestOllamaServesContextFloor_HeavierVariantIsTheRecommendGatesToDrop(t *testing.T) {
	m := floorManifest(262144)
	v := catalog.Variant{EstimatedWeightGB: 23.9, KVBytesPerTokenFP16: 20480}
	ok, spill := OllamaServesContextFloor(m, v, anchorHost())
	if !ok {
		t.Fatalf("the weights alone exceed the anchor's card, so this gate must pass "+
			"permissively like the card-free host does (expected spill %.3f)", spill)
	}
	if spill <= OllamaMaxExpectedSpillFraction {
		t.Errorf("expected spill fraction = %.4f, want > bound %.2f — the honest cost "+
			"must still be reported even though the gate no longer excludes on it",
			spill, OllamaMaxExpectedSpillFraction)
	}
	rec := hostfit.OllamaRecommend(v, anchorHost().HostFit())
	if rec.Fits {
		t.Error("nothing now stops the 23.9 GB tag being auto-selected on the 24 GB anchor; " +
			"the #625 judgment has no enforcing pass left")
	}
	if rec.Reason != hostfit.ReasonWeightsSpill {
		t.Errorf("recommendation Reason = %q, want %q", rec.Reason, hostfit.ReasonWeightsSpill)
	}
	// And the anchor's actual pick is unchanged: the mtp tag holds its
	// weights, so it is still the one the gate above admits AND the
	// recommendation keeps.
	mtp := catalog.Variant{EstimatedWeightGB: 22.62, KVBytesPerTokenFP16: 20480}
	if r := hostfit.OllamaRecommend(mtp, anchorHost().HostFit()); !r.Fits {
		t.Errorf("the anchor lost its flagship: mtp tag now %+v", r)
	}
}

// TestOllamaServesContextFloor_MonotoneInVRAM is the invariant whose
// absence waired#1031 exposed. The gate used to pass a CPU-only host
// unconditionally and then apply a bounded-spill test the moment a card
// appeared, so a 32 GB host with a 2 GB card kept a tier-90 model and
// the SAME host with a 4 GB card dropped to tier 27 — RankModels narrows
// on this gate and keeps any non-empty subset, so "almost everything
// excluded" is obeyed while "everything excluded" is ignored.
//
// Adding VRAM may never turn a pass into a fail. The expected-spill
// fraction is monotonically decreasing in the budget, so the only
// discontinuity was the card-free special case, and this pins that it is
// gone for a range of weights spanning laptop to workstation.
func TestOllamaServesContextFloor_MonotoneInVRAM(t *testing.T) {
	m := floorManifest(262144)
	for _, weightGB := range []float64{1.9, 3.4, 6.6, 17.0, 22.62, 23.9} {
		v := catalog.Variant{EstimatedWeightGB: weightGB, KVBytesPerTokenFP16: 20480}
		prev := true // the card-free host, which always passes
		for _, vramMB := range []int{2048, 4096, 8192, 12288, 16303, 24564, 49152} {
			hw := hardware.Profile{
				RAMTotalGB: 64,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "test", VRAMTotalMB: vramMB}},
			}
			ok, spill := OllamaServesContextFloor(m, v, hw)
			if !ok && prev {
				t.Errorf("%.2f GB weights: %d MB of VRAM fails the floor gate while less VRAM "+
					"passed it (expected spill %.3f) — adding a card removed a model",
					weightGB, vramMB, spill)
			}
			prev = ok
		}
	}
}

// TestOllamaServesContextFloor_SecondCardAdmitsIt is #264 against the
// row above it: the SAME variant on the SAME card, twice.
//
// This gate — not the #229 speed pass — is what actually drops a large
// model on a multi-GPU host, because RankModels narrows on floorOK
// first and does not fall through while smaller models still pass. So
// this is where the under-count had to be fixed for the fix to be
// visible at all.
func TestOllamaServesContextFloor_SecondCardAdmitsIt(t *testing.T) {
	m := floorManifest(262144)
	v := catalog.Variant{EstimatedWeightGB: 23.9, KVBytesPerTokenFP16: 20480}

	one := anchorHost()
	two := anchorHost()
	two.GPUs = append(two.GPUs, hardware.GPU{Vendor: "nvidia", VRAMTotalMB: 24467})

	// The boolean no longer discriminates here — one card that cannot hold
	// the weights passes permissively (see
	// TestOllamaServesContextFloor_MonotoneInVRAM) — so the #264 property
	// is asserted on the number the gate reports instead. A pooled host
	// that really does hold the weights predicts NO spill; an under-counted
	// one predicts a large one, which is what the bug looked like.
	if _, spill := OllamaServesContextFloor(m, v, one); spill <= OllamaMaxExpectedSpillFraction {
		t.Fatalf("the one-card case predicts only %.3f spill, so this test proves "+
			"nothing — re-pick the variant against the current constants", spill)
	}
	ok, spill := OllamaServesContextFloor(m, v, two)
	if !ok {
		t.Errorf("two %d MB cards still fail the floor gate (expected spill %.3f); "+
			"a host that can hold the weights is being judged as if the second "+
			"card were not there", one.GPUs[0].VRAMTotalMB, spill)
	}
	if spill != 0 {
		t.Errorf("expected spill fraction = %.4f on the pooled host, want 0 — "+
			"the weights and the floor window fit the pool outright", spill)
	}
}

// TestOllamaBudgetSitesAgreeOnTheSameHost pins that selection and
// serving size against ONE budget.
//
// They are computed in different packages (router's floor gate,
// cmd/waired-agent's serve tuning) and drifting apart is silent: a model
// admitted because its weights pool across two cards would be given a
// context window sized for one, and only #621's post-load verify probe
// would notice, at serve time, by shrinking and restarting.
func TestOllamaBudgetSitesAgreeOnTheSameHost(t *testing.T) {
	two := anchorHost()
	two.GPUs = append(two.GPUs, hardware.GPU{Vendor: "nvidia", VRAMTotalMB: 24467})

	if got, single := two.OllamaVRAMBudgetMB(), two.EffectiveVRAMMB(); got <= single {
		t.Fatalf("the pooled budget %d does not exceed the single-device figure %d, "+
			"so this fixture no longer exercises the multi-GPU path", got, single)
	}
	// OllamaExpectedSpillFraction is the shared core both the floor gate
	// and OllamaMaxContextAtSpill are built on; if it reads the pool,
	// they do too.
	v := catalog.Variant{EstimatedWeightGB: 23.9, KVBytesPerTokenFP16: 20480}
	oneCard := OllamaExpectedSpillFraction(v, anchorHost(), 1.0, 262144)
	pooled := OllamaExpectedSpillFraction(v, two, 1.0, 262144)
	if !(pooled < oneCard) {
		t.Errorf("expected spill %.4f on two cards is not below %.4f on one — "+
			"the spill model is still pricing the host at one device", pooled, oneCard)
	}
}

func TestOllamaServesContextFloor_UMANoSpillOnly(t *testing.T) {
	m := floorManifest(262144)
	v := catalog.Variant{EstimatedWeightGB: 22.62, KVBytesPerTokenFP16: 20480}

	// 24 GiB usable carve-out: no-spill window ≈ 202k ≥ floor → pass.
	roomy := hardware.Profile{UnifiedMemory: true, UsableVRAMMB: 24576, RAMTotalGB: 32}
	if ok, spill := OllamaServesContextFloor(m, v, roomy); !ok || spill != 0 {
		t.Errorf("roomy UMA: ok=%v spill=%.3f, want no-spill pass", ok, spill)
	}

	// 23 GiB usable: no-spill window < floor, and UMA gets no bounded-
	// spill allowance (one memory pool — "spill" has no meaning there).
	tight := hardware.Profile{UnifiedMemory: true, UsableVRAMMB: 23552, RAMTotalGB: 32}
	if ok, _ := OllamaServesContextFloor(m, v, tight); ok {
		t.Error("tight UMA must fail: bounded spill is discrete-only")
	}
}

func TestOllamaServesContextFloor_PermissivePaths(t *testing.T) {
	m := floorManifest(262144)

	// Unknown sizing inputs never exclude — the serve tuning and its
	// verify probe are the backstop (same philosophy as ollamaFitsVRAM).
	if ok, _ := OllamaServesContextFloor(m, catalog.Variant{}, anchorHost()); !ok {
		t.Error("unknown weight/KV must pass permissively")
	}
	// CPU-only: spilling to RAM is the design; the gate does not apply.
	cpu := hardware.Profile{RAMTotalGB: 64}
	v := catalog.Variant{EstimatedWeightGB: 22.62, KVBytesPerTokenFP16: 20480}
	if ok, _ := OllamaServesContextFloor(m, v, cpu); !ok {
		t.Error("CPU-only must pass permissively")
	}
}

// A manifest whose native window is below the floor gates on its own
// (capped) window, not on 200k — the preferred-override path serves
// such models at their full native window.
func TestOllamaServesContextFloor_SubFloorManifestCapped(t *testing.T) {
	m := floorManifest(131072)
	v := catalog.Variant{EstimatedWeightGB: 22.62, KVBytesPerTokenFP16: 20480}
	ok, spill := OllamaServesContextFloor(m, v, anchorHost())
	// 131072 q8_0 KV ≈ 1.34 GB: required ≈ 25.99 GB vs 25.66 GB budget →
	// predicted ≈ 1.3%, expected ≈ 3.8% ≤ 20% → passes at its own window.
	if !ok {
		t.Fatalf("sub-floor manifest should gate on its capped window (spill %.3f)", spill)
	}
}

// --- RankModels integration -------------------------------------------------

func floorCatalog() []catalog.Manifest {
	v := func(id string, weight float64, kv, tier, minRAM int) catalog.Variant {
		return catalog.Variant{
			VariantID: id, Format: "ollama-tag", RuntimeSupport: []string{catalog.RuntimeOllama},
			EstimatedWeightGB: weight, KVBytesPerTokenFP16: kv, QualityTier: tier, MinRAMGB: minRAM,
			Source: catalog.VariantSource{Type: "ollama", Tag: id},
		}
	}
	return []catalog.Manifest{
		{
			ModelID: "flagship-moe", ContextLength: 262144,
			Capabilities: []string{"chat", "tool_use"},
			Variants:     []catalog.Variant{v("mtp-q4", 22.62, 20480, 90, 32)},
		},
		{
			// Higher tier than the flagship but 131072-native: must lose
			// auto-selection to the floor no matter the tier.
			ModelID: "subfloor-champ", ContextLength: 131072,
			Capabilities: []string{"chat", "tool_use"},
			Variants:     []catalog.Variant{v("q4", 16.3, 65536, 95, 24)},
		},
		{
			ModelID: "small-pass", ContextLength: 262144,
			Capabilities: []string{"chat", "tool_use"},
			Variants:     []catalog.Variant{v("q4", 6.6, 32768, 52, 12)},
		},
	}
}

func TestRankModels_ContextFloorGating(t *testing.T) {
	ranked, err := RankModels(PickInput{Catalog: floorCatalog(), Hardware: anchorHost(), Engine: catalog.RuntimeOllama})
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	if ranked[0].Manifest.ModelID != "flagship-moe" {
		t.Fatalf("top pick = %s, want flagship-moe", ranked[0].Manifest.ModelID)
	}
	if !ranked[0].ContextFloorSatisfied {
		t.Error("flagship must satisfy the floor (bounded spill)")
	}
	if math.Abs(ranked[0].ExpectedSpillFraction-0.117) > 0.01 {
		t.Errorf("flagship expected spill = %.4f, want ≈ 0.117", ranked[0].ExpectedSpillFraction)
	}
	for _, p := range ranked {
		if p.Manifest.ModelID == "subfloor-champ" {
			t.Error("131072-native manifest must be absent from auto-selection when floor-passing candidates exist")
		}
	}
}

func TestRankModels_PreferredBypassesFloor(t *testing.T) {
	pick, err := PickModel(PickInput{
		Catalog: floorCatalog(), Hardware: anchorHost(),
		Engine: catalog.RuntimeOllama, PreferredModelID: "subfloor-champ",
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID != "subfloor-champ" {
		t.Fatalf("pick = %s, want the preferred subfloor-champ", pick.Manifest.ModelID)
	}
	if pick.ContextFloorSatisfied {
		t.Error("sub-floor preferred pick must report ContextFloorSatisfied=false")
	}
	found := false
	for _, r := range pick.Reasons {
		if strings.Contains(r, "overrides the ~200k coding-agent context floor") {
			found = true
		}
	}
	if !found {
		t.Errorf("reasons lack the override warning: %v", pick.Reasons)
	}
}

// TestRankModels_BestEffortFallbackWhenNothingServesFloor is the
// best-effort path after waired#1031: it fires when no model's OWN window
// reaches the floor, which is the only case left where no amount of
// hardware helps.
//
// It used to be driven by an 8 GiB card instead, on the reasoning that
// none of the fixture models "serves ~200k" there. That reasoning was
// the non-monotone gate: the same host with no card at all serves the
// flagship's 200k window out of system RAM, and a card can only add to
// that. What the 8 GiB card actually changes is how FAST it runs, which
// the recommendation gate says out loud and the case below asserts.
func TestRankModels_BestEffortFallbackWhenNothingServesFloor(t *testing.T) {
	// A catalog with nothing above the native floor: no host can serve a
	// coding-agent window from it, so every pick is best-effort.
	subFloor := []catalog.Manifest{{
		ModelID: "subfloor-champ", ContextLength: 131072,
		Capabilities: []string{"chat", "tool_use"},
		Variants: []catalog.Variant{{
			VariantID: "q4", Format: "ollama-tag", RuntimeSupport: []string{catalog.RuntimeOllama},
			EstimatedWeightGB: 16.3, KVBytesPerTokenFP16: 65536, QualityTier: 95, MinRAMGB: 24,
			Source: catalog.VariantSource{Type: "ollama", Tag: "q4"},
		}},
	}}
	hw := hardware.Profile{
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24564}},
	}
	ranked, err := RankModels(PickInput{Catalog: subFloor, Hardware: hw, Engine: catalog.RuntimeOllama})
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	for _, p := range ranked {
		if p.ContextFloorSatisfied {
			t.Errorf("%s: a 131072-native model can never satisfy the floor", p.Manifest.ModelID)
		}
	}
	pick, err := PickModel(PickInput{Catalog: subFloor, Hardware: hw, Engine: catalog.RuntimeOllama})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	found := false
	for _, r := range pick.Reasons {
		if strings.Contains(r, "best-effort selection") {
			found = true
		}
	}
	if !found {
		t.Errorf("reasons lack the best-effort line: %v", pick.Reasons)
	}
}

// TestRankModels_SmallCardCostsTierNotTheWindow is what a card too small
// for the flagship's weights may and may not cost.
//
// It may cost quality: the recommendation gate prefers a smaller model
// whose weights the card holds, and waired-ai/waired#988 accepted that
// trade explicitly ("a LOWER pick is the accepted trade; no pick is
// not"). It may NOT cost the window — under waired#1031 the pick is what
// the node will declare, so a selection that lands on a sub-floor model
// would take this host out of mesh serving entirely.
func TestRankModels_SmallCardCostsTierNotTheWindow(t *testing.T) {
	hw := hardware.Profile{
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 8192}},
	}
	pick, err := PickModel(PickInput{Catalog: floorCatalog(), Hardware: hw, Engine: catalog.RuntimeOllama})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if !pick.ContextFloorSatisfied {
		t.Errorf("%s: the pick must be one this host can declare a window for",
			pick.Manifest.ModelID)
	}
	if pick.Manifest.ContextLength < 262144 {
		t.Errorf("%s has a %d-token native window; the card cost this host the window "+
			"rather than a tier", pick.Manifest.ModelID, pick.Manifest.ContextLength)
	}
	// The flagship IS dropped on this card, and by the pass that should
	// drop it: at 22.62 GB of weights against 8 GiB it decodes in the
	// single digits, which is the #229 roofline's judgment, not the
	// window's. This gate reports its cost and excludes nothing.
	flagship := floorCatalog()[0].Variants[0]
	ok, spill := OllamaServesContextFloor(floorCatalog()[0], flagship, hw)
	if !ok {
		t.Error("the window gate must not be what drops a spilling model — it has no " +
			"speed term, and excluding on it is not monotone in VRAM")
	}
	if spill <= OllamaMaxExpectedSpillFraction {
		t.Errorf("expected spill = %.3f; the cost must still be reported honestly", spill)
	}
	if rec := hostfit.OllamaRecommend(flagship, hw.HostFit()); rec.Fits ||
		rec.Reason != hostfit.ReasonWeightsSpill {
		t.Errorf("recommendation on an 8 GiB card = %+v, want a weights_spill refusal", rec)
	}
}

// The #133 lighter-model recommendation goes through RankModels, so it
// must never step down from a floor-passing model onto a sub-floor one
// while floor-passing alternatives exist.
func TestLighterCandidate_StaysAboveContextFloor(t *testing.T) {
	pick, ok := LighterCandidate(PickInput{
		Catalog: floorCatalog(), Hardware: anchorHost(), Engine: catalog.RuntimeOllama,
	}, "flagship-moe", "mtp-q4")
	if !ok {
		t.Fatal("expected a lighter candidate")
	}
	if pick.Manifest.ModelID != "small-pass" {
		t.Errorf("lighter pick = %s, want small-pass (subfloor-champ is floor-excluded)", pick.Manifest.ModelID)
	}
}

// Pins the real-catalog interaction that forced the overhead
// recalibration and the manifest weight fix to land together: with the
// measured mtp weight (22.6 GB) the old flat 4096 MiB reservation would
// have kicked qwen3.6-35b-a3b off 24 GB hosts entirely, while the #625
// measurement shows it serving 200704 there at 13.5% spill. The
// corrected non-MTP weight (23.9 GB) must stay floor-excluded on the
// same host (mtp dominates it on both window and decode).
func TestBundledCatalog_AnchorHostKeepsFlagship(t *testing.T) {
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	ranked, err := RankModels(PickInput{
		Catalog: ms, Hardware: anchorHost(),
		Engine: catalog.RuntimeOllama, EngineVersion: "0.31.1",
	})
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	top := ranked[0]
	if top.Manifest.ModelID != "qwen3.6-35b-a3b" || top.Variant.VariantID != "mtp-q4-gguf" {
		t.Fatalf("anchor top pick = %s/%s, want qwen3.6-35b-a3b/mtp-q4-gguf",
			top.Manifest.ModelID, top.Variant.VariantID)
	}
	if !top.ContextFloorSatisfied || top.ExpectedSpillFraction <= 0 {
		t.Errorf("flagship should pass via bounded spill: floor=%v spill=%.3f",
			top.ContextFloorSatisfied, top.ExpectedSpillFraction)
	}
	for _, p := range ranked {
		if p.Manifest.ModelID == "qwen3.6-35b-a3b" && p.Variant.VariantID == "q4-gguf" {
			t.Error("the 23.9 GB non-MTP variant must be floor-excluded on 24 GB (expected spill ≈ 25%)")
		}
	}
}

// #675/#678: the vllm host gate — the floor window's fp16 KV plus
// padded weights must fit the default-utilization budget at the auto
// tensor-parallel size.
func TestVLLMServesContextFloor(t *testing.T) {
	l4 := hardware.GPU{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034}
	oneL4 := hardware.Profile{GPUs: []hardware.GPU{l4}}
	twoL4 := hardware.Profile{GPUs: []hardware.GPU{l4, l4}}
	m := floorManifest(262144)
	// 14 GB weights ×1.15 + 73728 B/tok × 200704 ≈ 30.9 GB: over one
	// L4's ~20.5 GB utilization budget, within 2×L4's ~41 GB.
	v := catalog.Variant{EstimatedWeightGB: 14.0, KVBytesPerTokenFP16: 73728}

	if VLLMServesContextFloor(m, v, oneL4) {
		t.Error("one L4 must not serve the ~200k floor for this variant")
	}
	if !VLLMServesContextFloor(m, v, twoL4) {
		t.Error("2×L4 (TP=2) must serve the ~200k floor for this variant")
	}

	t.Run("unknown sizing inputs pass permissively", func(t *testing.T) {
		if !VLLMServesContextFloor(m, catalog.Variant{}, oneL4) {
			t.Error("unknown inputs must pass (serve-time clamp is the backstop)")
		}
	})
	t.Run("no NVIDIA GPU passes permissively", func(t *testing.T) {
		if !VLLMServesContextFloor(m, v, hardware.Profile{}) {
			t.Error("hostFits owns the no-GPU rejection, not the floor gate")
		}
	})
	t.Run("weights alone overflowing fail the gate", func(t *testing.T) {
		big := catalog.Variant{EstimatedWeightGB: 40.0, KVBytesPerTokenFP16: 73728}
		if VLLMServesContextFloor(m, big, oneL4) {
			t.Error("weights past the whole budget cannot serve any window")
		}
	})
	t.Run("sub-floor native window is judged at its own cap", func(t *testing.T) {
		small := floorManifest(65536) // EffectiveContextFloor caps at 65536
		fits := catalog.Variant{EstimatedWeightGB: 4.0, KVBytesPerTokenFP16: 36864}
		if !VLLMServesContextFloor(small, fits, oneL4) {
			t.Error("4 GB weights + 65536-token KV fit one L4 easily")
		}
	})
	t.Run("fp8 KV on Ada widens a single GPU past the floor", func(t *testing.T) {
		// A variant whose 200k window overflows one L4 at f16 KV but fits
		// once fp8 halves KV — the whole point of #676. The Ada compute
		// capability (8.9) is what flips VLLMUsesFP8KV on.
		adaL4 := hardware.Profile{GPUs: []hardware.GPU{
			{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034, ComputeCap: "8.9"},
		}}
		v8 := catalog.Variant{EstimatedWeightGB: 8.0, KVBytesPerTokenFP16: 73728}
		if VLLMServesContextFloor(m, v8, oneL4) {
			t.Error("f16 KV (no compute_cap) must not serve the ~200k floor on one L4")
		}
		if !VLLMServesContextFloor(m, v8, adaL4) {
			t.Error("fp8 KV (Ada compute_cap 8.9) must serve the ~200k floor on one L4")
		}
	})
}

// #678: RankModels' vllm path applies the host floor gate — on a small
// host the floor-serving lower tier wins, on a TP=2 host the flagship
// returns.
func TestRankModels_VLLMContextFloorGating(t *testing.T) {
	l4 := hardware.GPU{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034}
	cat := []catalog.Manifest{
		{
			ModelID: "big-vllm", ContextLength: 262144,
			Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "mxfp4", Format: "safetensors",
				RuntimeSupport:    []string{"vllm"},
				EstimatedWeightGB: 14.0, KVBytesPerTokenFP16: 73728,
				MinVRAMMB: 16000, QualityTier: 90,
				Source: catalog.VariantSource{Type: "huggingface", RepoID: "big/awq"},
			}},
		},
		{
			ModelID: "small-vllm", ContextLength: 262144,
			Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "awq", Format: "safetensors",
				RuntimeSupport:    []string{"vllm"},
				EstimatedWeightGB: 4.0, KVBytesPerTokenFP16: 36864,
				MinVRAMMB: 8000, QualityTier: 60,
				Source: catalog.VariantSource{Type: "huggingface", RepoID: "small/awq"},
			}},
		},
	}

	oneL4 := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{l4}}
	pick, err := PickModel(PickInput{Catalog: cat, Hardware: oneL4, Engine: "vllm"})
	if err != nil {
		t.Fatalf("PickModel(1×L4): %v", err)
	}
	if pick.Manifest.ModelID != "small-vllm" {
		t.Errorf("1×L4 winner = %s, want small-vllm (big-vllm cannot serve the ~200k floor)", pick.Manifest.ModelID)
	}

	twoL4 := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{l4, l4}}
	pick, err = PickModel(PickInput{Catalog: cat, Hardware: twoL4, Engine: "vllm"})
	if err != nil {
		t.Fatalf("PickModel(2×L4): %v", err)
	}
	if pick.Manifest.ModelID != "big-vllm" {
		t.Errorf("2×L4 winner = %s, want big-vllm (TP=2 budget serves the floor)", pick.Manifest.ModelID)
	}

	// Best-effort fallback: when nothing serves the floor, the fitting
	// candidates all stay (floor never newly disables inference).
	onlyBig := []catalog.Manifest{cat[0]}
	pick, err = PickModel(PickInput{Catalog: onlyBig, Hardware: oneL4, Engine: "vllm"})
	if err != nil {
		t.Fatalf("PickModel(1×L4, best-effort): %v", err)
	}
	if pick.Manifest.ModelID != "big-vllm" || pick.ContextFloorSatisfied {
		t.Errorf("best-effort fallback expected (big-vllm, floor unsatisfied); got %s floorOK=%v",
			pick.Manifest.ModelID, pick.ContextFloorSatisfied)
	}
}
