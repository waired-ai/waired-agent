package router

import (
	"math"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// upgradeFixture mirrors sv-mag's situation: a 24 GB discrete GPU +
// 120 GB RAM host serving a small dense model with lots of headroom.
// Weights/tiers are the real bundled-catalog values so the numbers in
// review match the report.
// Every fixture manifest is 262144-native: this file tests the
// throughput-prediction mechanics, so the #624 context floor is kept
// neutral (floor-gating behavior is covered in coding_floor_test.go /
// model_picker_test.go).
func upgradeFixture() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID: "qwen2.5-coder-7b", ContextLength: 262144,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: 4.7, MinRAMGB: 8, QualityTier: 45,
				ParamCount: 7_610_000_000, KVBytesPerTokenFP16: 28672,
				Source: catalog.VariantSource{Type: "ollama", Tag: "qwen2.5-coder:7b-q4"},
			}},
		},
		{
			ModelID: "qwen3-coder-30b-a3b", ContextLength: 262144,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: 18.4, MinRAMGB: 32, QualityTier: 65,
				ParamCount: 30_530_000_000, ActiveParams: 3_300_000_000,
				KVBytesPerTokenFP16: 65536,
				Source:              catalog.VariantSource{Type: "ollama", Tag: "qwen3-coder:30b-a3b-q4"},
			}},
		},
		{
			ModelID: "qwen3.6-27b", ContextLength: 262144,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: 16.3, MinRAMGB: 24, QualityTier: 70,
				ParamCount: 27_000_000_000, KVBytesPerTokenFP16: 65536,
				Source: catalog.VariantSource{Type: "ollama", Tag: "qwen3.6:27b-q4"},
			}},
		},
		{
			ModelID: "gpt-oss-120b", ContextLength: 262144,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "gguf", Format: "ollama-tag", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: 62.0, MinRAMGB: 96, QualityTier: 85,
				ParamCount: 116_800_000_000, ActiveParams: 5_100_000_000,
				KVBytesPerTokenFP16: 65536,
				Source:              catalog.VariantSource{Type: "ollama", Tag: "gpt-oss:120b"},
			}},
		},
	}
}

func upgradeHost() hardware.Profile {
	return hardware.Profile{
		RAMTotalGB: 120,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX PRO 4000 Blackwell", VRAMTotalMB: 24467}},
	}
}

// TestUpgradeCandidate_PicksHighestQualifyingTier reproduces sv-mag:
// 100 tok/s measured on the 7B (tier 45). gpt-oss-120b (tier 85) is
// excluded by the VRAM gate; qwen3.6-27b (tier 70, dense 16.3 GB)
// predicts ~29 tok/s < 37.5 bar; qwen3-coder-30b-a3b (tier 65, MoE)
// predicts ~236 tok/s and wins.
func TestUpgradeCandidate_PicksHighestQualifyingTier(t *testing.T) {
	pick, predicted, ok := UpgradeCandidate(UpgradeInput{
		Pick:          PickInput{Catalog: upgradeFixture(), Hardware: upgradeHost(), Engine: "ollama"},
		ActiveModelID: "qwen2.5-coder-7b", ActiveVariantID: "q4-gguf",
		MeasuredTokps: 100, FloorTokps: 30,
	})
	if !ok {
		t.Fatal("UpgradeCandidate found nothing; want qwen3-coder-30b-a3b")
	}
	if pick.Manifest.ModelID != "qwen3-coder-30b-a3b" {
		t.Errorf("ModelID = %q, want qwen3-coder-30b-a3b", pick.Manifest.ModelID)
	}
	// effBW = 100 × 4.7 = 470; activeWeight = 18.4 × 3.3/30.53 ≈ 1.989;
	// predicted ≈ 236.3.
	if math.Abs(predicted-236.3) > 1.0 {
		t.Errorf("predicted = %.1f tok/s, want ≈ 236.3", predicted)
	}
}

// TestUpgradeCandidate_DensePredictionBelowBarIsSkipped: with margin 1
// and a 24 tok/s floor the dense 27B (predicted ~28.8) qualifies and,
// being the higher tier, beats the much faster MoE.
func TestUpgradeCandidate_DensePredictionBelowBarIsSkipped(t *testing.T) {
	pick, _, ok := UpgradeCandidate(UpgradeInput{
		Pick:          PickInput{Catalog: upgradeFixture(), Hardware: upgradeHost(), Engine: "ollama"},
		ActiveModelID: "qwen2.5-coder-7b", ActiveVariantID: "q4-gguf",
		MeasuredTokps: 100, FloorTokps: 24, SafetyMargin: 1.0,
	})
	if !ok || pick.Manifest.ModelID != "qwen3.6-27b" {
		t.Errorf("pick = %+v ok=%v, want qwen3.6-27b (highest tier whose prediction clears 24 tok/s)", pick.Manifest.ModelID, ok)
	}
}

// TestUpgradeCandidate_NothingAboveActiveTier: a host already serving
// the highest fitting tier gets no suggestion.
func TestUpgradeCandidate_NothingAboveActiveTier(t *testing.T) {
	_, _, ok := UpgradeCandidate(UpgradeInput{
		Pick:          PickInput{Catalog: upgradeFixture(), Hardware: upgradeHost(), Engine: "ollama"},
		ActiveModelID: "qwen3.6-27b", ActiveVariantID: "q4-gguf",
		MeasuredTokps: 20, FloorTokps: 10,
	})
	if ok {
		t.Error("UpgradeCandidate suggested something above the top fitting tier")
	}
}

// TestUpgradeCandidate_NoHeadroomNoSuggestion: when every higher-tier
// prediction lands under floor×margin there is nothing to suggest.
// effBW = 10 × 4.7 = 47 GB/s → even the MoE (active ≈ 1.99 GB)
// predicts ~23.6 tok/s < 37.5.
func TestUpgradeCandidate_NoHeadroomNoSuggestion(t *testing.T) {
	_, _, ok := UpgradeCandidate(UpgradeInput{
		Pick:          PickInput{Catalog: upgradeFixture(), Hardware: upgradeHost(), Engine: "ollama"},
		ActiveModelID: "qwen2.5-coder-7b", ActiveVariantID: "q4-gguf",
		MeasuredTokps: 10, FloorTokps: 30,
	})
	if ok {
		t.Error("UpgradeCandidate suggested an upgrade on a host with no headroom")
	}
}

// TestUpgradeCandidate_UnknownActiveVariant: without a catalog entry
// for the active variant there is no baseline to scale from.
func TestUpgradeCandidate_UnknownActiveVariant(t *testing.T) {
	_, _, ok := UpgradeCandidate(UpgradeInput{
		Pick:          PickInput{Catalog: upgradeFixture(), Hardware: upgradeHost(), Engine: "ollama"},
		ActiveModelID: "externally-pinned", ActiveVariantID: "x",
		MeasuredTokps: 100, FloorTokps: 30,
	})
	if ok {
		t.Error("UpgradeCandidate produced a suggestion without a known baseline")
	}
}

// TestUpgradeCandidate_MoEBaseline: a MoE active model scales from its
// ACTIVE weight, not its total weight — 236 tok/s on the 30B-A3B
// (active ≈ 1.99 GB) implies effBW ≈ 470 GB/s, far too slow for the
// dense 27B (≈ 29 tok/s < bar), so nothing qualifies even though the
// 27B's TOTAL weight is smaller than the MoE's.
func TestUpgradeCandidate_MoEBaseline(t *testing.T) {
	_, _, ok := UpgradeCandidate(UpgradeInput{
		Pick:          PickInput{Catalog: upgradeFixture(), Hardware: upgradeHost(), Engine: "ollama"},
		ActiveModelID: "qwen3-coder-30b-a3b", ActiveVariantID: "q4-gguf",
		MeasuredTokps: 236, FloorTokps: 30,
	})
	if ok {
		t.Error("UpgradeCandidate used total instead of active weight for the MoE baseline")
	}
}

func TestActiveWeightGB(t *testing.T) {
	for _, c := range []struct {
		name string
		v    catalog.Variant
		want float64
	}{
		{"dense", catalog.Variant{EstimatedWeightGB: 16.3, ParamCount: 27e9}, 16.3},
		{"moe", catalog.Variant{EstimatedWeightGB: 18.4, ParamCount: 30.53e9, ActiveParams: 3.3e9}, 18.4 * 3.3 / 30.53},
		{"no weight", catalog.Variant{ParamCount: 27e9}, 0},
		{"degenerate active>=param treated dense", catalog.Variant{EstimatedWeightGB: 5, ParamCount: 5e9, ActiveParams: 5e9}, 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ActiveWeightGB(c.v); math.Abs(got-c.want) > 1e-9 {
				t.Errorf("ActiveWeightGB = %v, want %v", got, c.want)
			}
		})
	}
}

// TestUpgradeCandidate_SpillBoundCapsThePrediction closes a gap #229
// opened. The prediction extrapolates from a measurement taken on the
// ACTIVE model, and that silently assumes the candidate reads at the
// same effective bandwidth. It used to be safe by accident: RankModels'
// residency gate could not return a candidate the card would not hold.
// With the gate gone, a measurement taken on a resident model would
// otherwise promise graphics-card speed for a model that reads most of
// its weights over PCIe.
//
// Product contract, and it takes some setting up, because the cap is
// the LAST guard rather than the first. The candidate is a mixture of
// experts so its small active share carries it past the speed floor, and
// both models are 131072-native so the #624 context floor finds nothing
// to prefer and falls through — which is exactly the situation the
// residency gate used to backstop. A dense candidate, or a 262144-native
// one, is dropped an earlier gate and proves nothing about this one.
func TestUpgradeCandidate_SpillBoundCapsThePrediction(t *testing.T) {
	// An 8 GB card with plenty of RAM behind it: the active model is
	// resident and measured fast, the candidate is eight times larger.
	hw := hardware.Profile{
		RAMTotalGB: 128,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4060", VRAMTotalMB: 8192}},
	}
	active := catalog.Variant{
		VariantID: "q4-gguf", Format: "ollama-tag", RuntimeSupport: []string{"ollama"},
		EstimatedWeightGB: 3.0, MinRAMGB: 8, QualityTier: 40,
		ParamCount: 4_000_000_000, KVBytesPerTokenFP16: 16384,
		Source: catalog.VariantSource{Type: "ollama", Tag: "small:4b"},
	}
	candidate := catalog.Variant{
		VariantID: "q4-gguf", Format: "ollama-tag", RuntimeSupport: []string{"ollama"},
		EstimatedWeightGB: 24.0, MinRAMGB: 32, QualityTier: 80,
		ParamCount: 32_000_000_000, ActiveParams: 3_000_000_000,
		KVBytesPerTokenFP16: 16384,
		Source:              catalog.VariantSource{Type: "ollama", Tag: "big:32b-a3b"},
	}
	cat := []catalog.Manifest{
		{ModelID: "active-small", ContextLength: 131072, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{active}},
		{ModelID: "candidate-big", ContextLength: 131072, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{candidate}},
	}

	in := UpgradeInput{
		Pick:            PickInput{Catalog: cat, Hardware: hw, Engine: catalog.RuntimeOllama},
		ActiveModelID:   "active-small",
		ActiveVariantID: "q4-gguf",
		// 200 tok/s over 3 GB of active weights implies ~600 GB/s of
		// effective bandwidth. Extrapolated onto the candidate's 2.25 GB
		// active share that is ~266 tok/s — far above the bar — while the
		// card holds barely a quarter of its weights.
		MeasuredTokps: 200,
		FloorTokps:    40,
	}

	// The fixture is only meaningful if both gates it must pass are real.
	if naive := in.MeasuredTokps * ActiveWeightGB(active) / ActiveWeightGB(candidate); naive < in.FloorTokps*DefaultUpgradeSafetyMargin {
		t.Fatalf("fixture no longer exercises the cap: the naive prediction %.1f is "+
			"already below the bar", naive)
	}
	ranked, err := RankModels(in.Pick)
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	if len(ranked) == 0 || ranked[0].Manifest.ModelID != "candidate-big" {
		t.Fatalf("fixture no longer exercises the cap: the candidate did not survive "+
			"selection (ranked=%d)", len(ranked))
	}

	if _, predicted, ok := UpgradeCandidate(in); ok {
		t.Errorf("suggested an upgrade at %.1f tok/s; the candidate holds about a quarter "+
			"of its weights on an 8 GB card and cannot reach the rate a resident "+
			"measurement implies", predicted)
	}
}
