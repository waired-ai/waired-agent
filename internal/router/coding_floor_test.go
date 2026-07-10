package router

import (
	"math"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
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

func TestOllamaServesContextFloor_HeavierVariantExcluded(t *testing.T) {
	// The non-MTP tag's measured weight (23.9 GB): expected spill ≈ 25%
	// exceeds the bound — matches the #625 judgment (mtp dominant).
	m := floorManifest(262144)
	v := catalog.Variant{EstimatedWeightGB: 23.9, KVBytesPerTokenFP16: 20480}
	ok, spill := OllamaServesContextFloor(m, v, anchorHost())
	if ok {
		t.Fatalf("23.9 GB variant must fail the bounded-spill gate (expected spill %.3f)", spill)
	}
	if spill <= OllamaMaxExpectedSpillFraction {
		t.Errorf("expected spill fraction = %.4f, want > bound %.2f", spill, OllamaMaxExpectedSpillFraction)
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

func TestRankModels_BestEffortFallbackWhenNothingServesFloor(t *testing.T) {
	// 8 GiB discrete card: none of the fixture models serves ~200k
	// (small-pass needs ~9.9 GB incl. KV; the rest don't even fit) —
	// selection falls back to every fitting candidate, flagged.
	hw := hardware.Profile{
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 8192}},
	}
	ranked, err := RankModels(PickInput{Catalog: floorCatalog(), Hardware: hw, Engine: catalog.RuntimeOllama})
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	for _, p := range ranked {
		if p.ContextFloorSatisfied {
			t.Errorf("%s: no candidate should satisfy the floor on this host", p.Manifest.ModelID)
		}
	}
	pick, err := PickModel(PickInput{Catalog: floorCatalog(), Hardware: hw, Engine: catalog.RuntimeOllama})
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
