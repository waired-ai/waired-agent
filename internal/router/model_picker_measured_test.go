package router

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// measuredLadderCatalog is three ollama families on a clean tier ladder,
// all declaring a coding window, so the only thing that can separate
// them in RankModels is what this host measured.
func measuredLadderCatalog() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID: "big", ContextLength: 262144, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag", Quantization: "Q4_K_M",
				RuntimeSupport: []string{"ollama"}, EstimatedWeightGB: 6.0,
				MinRAMGB: 16, QualityTier: 90,
				Source: catalog.VariantSource{Type: "ollama", Tag: "big:9b"},
			}},
		},
		{
			ModelID: "mid", ContextLength: 262144, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag", Quantization: "Q4_K_M",
				RuntimeSupport: []string{"ollama"}, EstimatedWeightGB: 3.0,
				MinRAMGB: 8, QualityTier: 60,
				Source: catalog.VariantSource{Type: "ollama", Tag: "mid:4b"},
			}},
		},
		{
			ModelID: "small", ContextLength: 262144, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag", Quantization: "Q4_K_M",
				RuntimeSupport: []string{"ollama"}, EstimatedWeightGB: 1.0,
				MinRAMGB: 4, QualityTier: 30,
				Source: catalog.VariantSource{Type: "ollama", Tag: "small:2b"},
			}},
		},
	}
}

// measuredSHAFor is the ledger key for one fixture family's only variant.
func measuredSHAFor(t *testing.T, modelID string) string {
	t.Helper()
	for _, m := range measuredLadderCatalog() {
		if m.ModelID == modelID {
			return catalog.VariantSHA(m.Variants[0])
		}
	}
	t.Fatalf("no fixture family %q", modelID)
	return ""
}

// measuredLadderInput is the picker input for a host with room for every
// fixture family, so nothing but the measurements can move the answer.
func measuredLadderInput(measured map[string]MeasuredRate, line float64) PickInput {
	return PickInput{
		Catalog:           measuredLadderCatalog(),
		Hardware:          hardware.Profile{RAMTotalGB: 64},
		Engine:            catalog.RuntimeOllama,
		Measured:          measured,
		TurnBudgetSeconds: line,
	}
}

func topPick(t *testing.T, in PickInput) string {
	t.Helper()
	ranked, err := RankModels(in)
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	if len(ranked) == 0 {
		t.Fatal("RankModels returned nothing")
	}
	return ranked[0].Manifest.ModelID
}

// PRODUCT CONTRACT (waired-agent#784): a model this host has MEASURED
// over the line stops being the model this host recommends to itself,
// and the next rung down takes the badge.
//
// This is the route decision 20260804/1937 §4 reserved when it removed
// the PREDICTED speed pass — "速度は実測が入ってから推奨の入力に戻す
// (waired-ai/waired-agent#466)". The rc9 evidence on the report: a
// Windows host measured its 9B at 11-12 tok/s and went on recommending
// the 9B.
func TestRankModels_MeasuredSlowLosesTheBadge(t *testing.T) {
	if got := topPick(t, measuredLadderInput(nil, 190)); got != "big" {
		t.Fatalf("with nothing measured, top = %q, want big", got)
	}

	oneRung := map[string]MeasuredRate{measuredSHAFor(t, "big"): {Tokps: 11, TurnSeconds: 400}}
	if got := topPick(t, measuredLadderInput(oneRung, 190)); got != "mid" {
		t.Errorf("after big measured 400 s per request, top = %q, want mid", got)
	}

	// Rule (b): the switched-to model measures slow too, and the badge
	// walks one more rung. A ledger that kept only the newest figure
	// would answer "big" here, because big's exclusion would have been
	// overwritten by mid's.
	twoRungs := map[string]MeasuredRate{
		measuredSHAFor(t, "big"): {Tokps: 11, TurnSeconds: 400},
		measuredSHAFor(t, "mid"): {Tokps: 26, TurnSeconds: 260},
	}
	if got := topPick(t, measuredLadderInput(twoRungs, 190)); got != "small" {
		t.Errorf("after big and mid both measured slow, top = %q, want small", got)
	}
}

// PRODUCT CONTRACT (waired-ai/waired#1056 decision 1): the measured pass
// is a narrow() rung, so a host that has measured EVERYTHING it offers
// as slow keeps its ranking rather than losing local inference.
//
// This is the property that separates a measured pass from the predicted
// one 1937 §4 removed: exclusion that can empty the set is what would
// leave the installer with nothing to offer.
func TestRankModels_AllMeasuredSlowStandsDown(t *testing.T) {
	all := map[string]MeasuredRate{
		measuredSHAFor(t, "big"):   {Tokps: 11, TurnSeconds: 400},
		measuredSHAFor(t, "mid"):   {Tokps: 26, TurnSeconds: 260},
		measuredSHAFor(t, "small"): {Tokps: 44, TurnSeconds: 210},
	}
	ranked, err := RankModels(measuredLadderInput(all, 190))
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	if len(ranked) != 3 {
		t.Fatalf("ranked %d candidates, want all 3 kept", len(ranked))
	}
	if ranked[0].Manifest.ModelID != "big" {
		t.Errorf("top = %q, want big — the pass must fall through, not empty the set",
			ranked[0].Manifest.ModelID)
	}
}

// PRODUCT CONTRACT (waired-agent#784): a surviving candidate carries
// what this host measured for it, and says so in its Reasons when that
// figure is below the floor.
//
// narrow() REPLACES the candidate set, so an excluded model is not in
// the result at all — there is no Pick to read its figure off. That is
// why the catalog rows read the ledger directly rather than through
// RankModels: the ranking answers "which model", not "what does every
// row say". The case where a below-floor figure does reach a Pick is the
// one that matters most for it — the stand-down, where this host has
// measured everything it offers as slow and is still being told what to
// run.
func TestRankModels_ASurvivingCandidateSaysWhatItMeasured(t *testing.T) {
	all := map[string]MeasuredRate{
		measuredSHAFor(t, "big"):   {Tokps: 11, TurnSeconds: 400},
		measuredSHAFor(t, "mid"):   {Tokps: 26, TurnSeconds: 260},
		measuredSHAFor(t, "small"): {Tokps: 44, TurnSeconds: 210},
	}
	ranked, err := RankModels(measuredLadderInput(all, 190))
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	want := map[string]float64{"big": 400, "mid": 260, "small": 210}
	for _, p := range ranked {
		if got := p.MeasuredTurnSeconds; got != want[p.Manifest.ModelID] {
			t.Errorf("%s reports %v s per request, want %v",
				p.Manifest.ModelID, got, want[p.Manifest.ModelID])
		}
		var said bool
		for _, r := range p.Reasons {
			if strings.HasPrefix(r, "measured ") && strings.Contains(r, "s per request on this host (target: 190 s or less)") {
				said = true
			}
		}
		if !said {
			t.Errorf("%s's Reasons do not say what it measured: %q",
				p.Manifest.ModelID, p.Reasons)
		}
	}
}

// PRODUCT CONTRACT (waired-agent#784): a candidate this host has NOT
// measured reports no figure, rather than a zero that reads as one.
func TestRankModels_UnmeasuredCandidatesReportNothing(t *testing.T) {
	ranked, err := RankModels(measuredLadderInput(map[string]MeasuredRate{
		measuredSHAFor(t, "big"): {Tokps: 11, TurnSeconds: 400},
	}, 190))
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	for _, p := range ranked {
		if p.Manifest.ModelID != "big" && (p.MeasuredTokps != 0 || p.MeasuredTurnSeconds != 0) {
			t.Errorf("%s reports %v tok/s / %v s; nothing was measured for it",
				p.Manifest.ModelID, p.MeasuredTokps, p.MeasuredTurnSeconds)
		}
	}
}

// PRODUCT CONTRACT (waired-agent#784): the pass is inert without a
// floor, and a measurement AT or ABOVE the floor is not evidence
// against anything.
func TestRankModels_MeasuredPassNeedsAFloorAndAShortfall(t *testing.T) {
	slow := map[string]MeasuredRate{measuredSHAFor(t, "big"): {Tokps: 11, TurnSeconds: 400}}

	// TurnBudgetSeconds 0 is "no claim" — a caller that does not care about
	// speed leaves it unset and must see the ladder it saw before.
	if got := topPick(t, measuredLadderInput(slow, 0)); got != "big" {
		t.Errorf("with no line, top = %q, want big", got)
	}

	fast := map[string]MeasuredRate{measuredSHAFor(t, "big"): {Tokps: 120, TurnSeconds: 70}}
	if got := topPick(t, measuredLadderInput(fast, 190)); got != "big" {
		t.Errorf("measured inside the line, top = %q, want big", got)
	}

	atFloor := map[string]MeasuredRate{measuredSHAFor(t, "big"): {Tokps: 60, TurnSeconds: 190}}
	if got := topPick(t, measuredLadderInput(atFloor, 190)); got != "big" {
		t.Errorf("measured exactly at the line, top = %q, want big", got)
	}
}

// PRODUCT CONTRACT (waired-agent#784): the ledger is keyed by
// VariantSHA, so a figure belongs to the WEIGHTS that were run. A
// re-quantized or re-tagged variant is a different artifact and its
// predecessor's rate says nothing about it.
func TestRankModels_MeasurementBelongsToTheWeights(t *testing.T) {
	stale := map[string]MeasuredRate{
		catalog.VariantSHA(catalog.Variant{
			VariantID: "q4-gguf", Format: "ollama-tag", Quantization: "Q4_K_M",
			Source: catalog.VariantSource{Type: "ollama", Tag: "big:9b-OLD"},
		}): {Tokps: 11, TurnSeconds: 400},
	}
	if got := topPick(t, measuredLadderInput(stale, 190)); got != "big" {
		t.Errorf("a figure for other weights excluded big: top = %q, want big", got)
	}
}

// PRODUCT CONTRACT (waired-agent#784): an explicit PreferredModelID
// bypasses the measured pass, exactly as it bypasses every other rung.
// Somebody pinned that model; being slow is not the picker's licence to
// overrule them.
func TestRankModels_PreferredModelIDBypassesTheMeasuredPass(t *testing.T) {
	in := measuredLadderInput(map[string]MeasuredRate{
		measuredSHAFor(t, "big"): {Tokps: 11, TurnSeconds: 400},
	}, 190)
	in.PreferredModelID = "big"
	if got := topPick(t, in); got != "big" {
		t.Errorf("top = %q, want the pinned big", got)
	}
}
