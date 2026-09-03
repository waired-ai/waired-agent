package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/router"
)

// recTestManifests returns two ollama families on a footprint ladder:
// heavy (12 GB RAM) and light (4 GB RAM).
func recTestManifests() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID: "heavy", ContextLength: 32768, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4", Format: "ollama-tag", Quantization: "Q4_K_M",
				RuntimeSupport: []string{"ollama"}, EstimatedWeightGB: 5.0,
				MinRAMGB: 12, QualityTier: 50, ParamCount: 8_000_000_000,
				Source: catalog.VariantSource{Type: "ollama", Tag: "heavy:8b"},
			}},
		},
		{
			ModelID: "light", ContextLength: 32768, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4", Format: "ollama-tag", Quantization: "Q4_K_M",
				RuntimeSupport: []string{"ollama"}, EstimatedWeightGB: 1.5,
				MinRAMGB: 4, QualityTier: 20, ParamCount: 2_000_000_000,
				Source: catalog.VariantSource{Type: "ollama", Tag: "light:2b"},
			}},
		},
	}
}

// storeWithActive returns a Store whose state has heavy/q4 active.
func storeWithActive(t *testing.T) *catalog.Store {
	t.Helper()
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: "heavy", VariantID: "q4",
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return store
}

// cpuHost is an ollama host with enough RAM for both fixture families.
func cpuHost() hardware.Profile { return hardware.Profile{RAMTotalGB: 16} }

func TestRecommendationFromBench_BelowFloorSuggestsLighter(t *testing.T) {
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 10, Capacity: 1},
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec == nil {
		t.Fatalf("expected a recommendation, got nil")
	}
	if rec.FromModelID != "heavy" || rec.ToModelID != "light" {
		t.Errorf("from/to = %s→%s, want heavy→light", rec.FromModelID, rec.ToModelID)
	}
	if rec.MeasuredTokps != 10 || rec.FloorTokps != router.CodingAgentSelectionFloorTokps {
		t.Errorf("measured=%v floor=%v, want 10 / %v", rec.MeasuredTokps, rec.FloorTokps, router.CodingAgentSelectionFloorTokps)
	}
	if rec.Dismissed {
		t.Errorf("Dismissed should be false on a fresh recommendation")
	}
}

func TestRecommendationFromBench_AboveFloorNil(t *testing.T) {
	// 61 sits just above the 60 default floor — pins the boundary.
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 61, Capacity: 2},
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("above floor → want nil, got %+v", rec)
	}
}

func TestRecommendationFromBench_FailedNil(t *testing.T) {
	rec := recommendationFromBench(
		BenchResult{Failed: true, Capacity: 1, Err: "timeout"},
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("failed benchmark → want nil, got %+v", rec)
	}
}

func TestRecommendationFromBench_SkippedNil(t *testing.T) {
	// Capacity==0 with Failed==false is the "skipped" encoding.
	rec := recommendationFromBench(
		BenchResult{Capacity: 0},
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("skipped benchmark → want nil, got %+v", rec)
	}
}

func TestRecommendationFromBench_NoActiveNil(t *testing.T) {
	emptyStore := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 5, Capacity: 1},
		emptyStore, cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("no active model → want nil, got %+v", rec)
	}
}

func TestRecommendationFromBench_NoLighterNil(t *testing.T) {
	// Active is already the lightest fitting family.
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "light", VariantID: "q4"}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 5, Capacity: 1},
		store, cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("already lightest → want nil, got %+v", rec)
	}
}

func TestRecommendationFromBench_ConfigurableFloor(t *testing.T) {
	// floor=8 → a 10 tok/s result is now ABOVE the floor → no suggestion.
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 10, Capacity: 1},
		storeWithActive(t), cpuHost(), recTestManifests(),
		agentconfig.InferenceConfig{InteractiveFloorTokps: 8}, "")
	if rec != nil {
		t.Errorf("configurable floor 8 with 10 tok/s → want nil, got %+v", rec)
	}
}

func TestRecommendationFromBench_DismissedMarker(t *testing.T) {
	store := storeWithActive(t)
	// Dismiss the heavy→light pairing keyed by the active variant SHA.
	sha := activeVariantSHA(recTestManifests(), "heavy", "q4")
	if sha == "" {
		t.Fatalf("activeVariantSHA returned empty")
	}
	if err := store.Update(func(s *catalog.State) {
		s.DismissedRecommendations = map[string]time.Time{
			catalog.DismissalKey(sha, "q4"): time.Now(),
		}
	}); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 10, Capacity: 1},
		store, cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec == nil {
		t.Fatalf("expected recommendation with Dismissed=true, got nil")
	}
	if !rec.Dismissed {
		t.Errorf("Dismissed = false, want true (pairing was dismissed)")
	}
}

// storeWithActiveLight returns a Store whose state has light/q4 active
// (the baseline for upgrade-direction tests).
func storeWithActiveLight(t *testing.T) *catalog.Store {
	t.Helper()
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: "light", VariantID: "q4",
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return store
}

func TestUpgradeFromBench_HeadroomSuggestsHigherTier(t *testing.T) {
	// effBW = 450 × 1.5 = 675 GB/s; heavy (5 GB dense) predicts 135
	// tok/s ≥ the 60 × 1.25 = 75 bar → upgrade light→heavy.
	rec := upgradeFromBench(
		BenchResult{TokensPerSec: 450, Capacity: 15},
		storeWithActiveLight(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec == nil {
		t.Fatalf("expected an upgrade recommendation, got nil")
	}
	if rec.FromModelID != "light" || rec.ToModelID != "heavy" {
		t.Errorf("from/to = %s→%s, want light→heavy", rec.FromModelID, rec.ToModelID)
	}
	if rec.Direction != management.RecommendationUpgrade {
		t.Errorf("Direction = %q, want upgrade", rec.Direction)
	}
	if rec.PredictedTokps < 134 || rec.PredictedTokps > 136 {
		t.Errorf("PredictedTokps = %v, want ≈ 135", rec.PredictedTokps)
	}
}

func TestUpgradeFromBench_BelowFloorNil(t *testing.T) {
	// Below the floor the lighter flow owns the suggestion.
	rec := upgradeFromBench(
		BenchResult{TokensPerSec: 10, Capacity: 1},
		storeWithActiveLight(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("below floor → want nil, got %+v", rec)
	}
}

func TestUpgradeFromBench_InsufficientHeadroomNil(t *testing.T) {
	// Above the floor (120 ≥ 60) but heavy predicts only 36 tok/s
	// (120 × 1.5/5) < the 75 bar → no upgrade.
	rec := upgradeFromBench(
		BenchResult{TokensPerSec: 120, Capacity: 4},
		storeWithActiveLight(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("insufficient headroom → want nil, got %+v", rec)
	}
}

func TestUpgradeFromBench_FailedNil(t *testing.T) {
	rec := upgradeFromBench(
		BenchResult{Failed: true, Capacity: 1, Err: "timeout"},
		storeWithActiveLight(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("failed benchmark → want nil, got %+v", rec)
	}
}

func TestUpgradeFromBench_AlreadyTopTierNil(t *testing.T) {
	// heavy is the highest fitting tier; nothing above it.
	rec := upgradeFromBench(
		BenchResult{TokensPerSec: 500, Capacity: 16},
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("already top tier → want nil, got %+v", rec)
	}
}

func TestUpgradeFromBench_DismissedMarker(t *testing.T) {
	store := storeWithActiveLight(t)
	sha := activeVariantSHA(recTestManifests(), "light", "q4")
	if sha == "" {
		t.Fatalf("activeVariantSHA returned empty")
	}
	if err := store.Update(func(s *catalog.State) {
		s.DismissedRecommendations = map[string]time.Time{
			catalog.DismissalKey(sha, "q4"): time.Now(),
		}
	}); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	rec := upgradeFromBench(
		BenchResult{TokensPerSec: 450, Capacity: 15},
		store, cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec == nil {
		t.Fatalf("expected recommendation with Dismissed=true, got nil")
	}
	if !rec.Dismissed {
		t.Errorf("Dismissed = false, want true (pairing was dismissed)")
	}
}

// recTestLadder is recTestManifests plus a third, lightest rung, so a
// step-down has somewhere to go after the first one is used up.
func recTestLadder() []catalog.Manifest {
	return append(recTestManifests(), catalog.Manifest{
		ModelID: "tiny", ContextLength: 32768, Capabilities: []string{"chat"},
		Variants: []catalog.Variant{{
			VariantID: "q4", Format: "ollama-tag", Quantization: "Q4_K_M",
			RuntimeSupport: []string{"ollama"}, EstimatedWeightGB: 0.6,
			MinRAMGB: 2, QualityTier: 10, ParamCount: 600_000_000,
			Source: catalog.VariantSource{Type: "ollama", Tag: "tiny:0.6b"},
		}},
	})
}

// storeWithMeasured seeds an active selection plus a measurement ledger.
func storeWithMeasured(
	t *testing.T, activeModel string, measured map[string]float64,
) *catalog.Store {
	t.Helper()
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: activeModel, VariantID: "q4",
		}
		s.MeasuredVariants = map[string]catalog.VariantMeasurement{}
		for modelID, tokps := range measured {
			sha := activeVariantSHA(recTestLadder(), modelID, "q4")
			if sha == "" {
				t.Fatalf("no fixture variant for %q", modelID)
			}
			s.MeasuredVariants[sha] = catalog.VariantMeasurement{
				ModelID: modelID, VariantID: "q4", MeasuredTokps: tokps,
			}
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return store
}

// PRODUCT CONTRACT (waired-agent#784): the step-down does not offer a
// rung this host has already tried and measured below the floor.
//
// The active model was never the gap — LighterCandidate has skipped that
// one since waired-agent#754. The gap is every OTHER rung the host has
// run: the proposal knew a candidate was lighter, never that it had been
// measured here. A host that stepped down to "light", was moved back up
// to "heavy" (an operator, or a control-plane desired model), and
// re-benchmarked would be sent to "light" a second time.
func TestRecommendationFromBench_DoesNotOfferAnAlreadyMeasuredRung(t *testing.T) {
	// Nothing measured yet: the ordinary first step.
	first := recommendationFromBench(
		BenchResult{TokensPerSec: 10, Capacity: 1, ModelID: "heavy"},
		storeWithMeasured(t, "heavy", nil), cpuHost(), recTestLadder(),
		agentconfig.InferenceConfig{}, "")
	if first == nil {
		t.Fatal("no step-down offered for a slow heavy model")
	}
	if first.ToModelID != "light" {
		t.Fatalf("first step = %q, want light", first.ToModelID)
	}

	// Same host, same active model, but "light" has since been run here
	// and measured below the floor. The next rung down is the only
	// honest offer left.
	again := recommendationFromBench(
		BenchResult{TokensPerSec: 10, Capacity: 1, ModelID: "heavy"},
		storeWithMeasured(t, "heavy", map[string]float64{"light": 26}),
		cpuHost(), recTestLadder(), agentconfig.InferenceConfig{}, "")
	if again == nil {
		t.Fatal("no step-down offered once light was known to be slow here")
	}
	if again.ToModelID != "tiny" {
		t.Errorf("step = %q, want tiny — light was already measured at 26 tok/s here",
			again.ToModelID)
	}
}

// PRODUCT CONTRACT (waired-agent#784): a completed benchmark files its
// figure under the variant it MEASURED, so the ranking can stop
// recommending a model this host has already timed as too slow.
func TestBenchMeasurement_RecordsWhatWasMeasured(t *testing.T) {
	sha, got := benchMeasurement(
		BenchResult{TokensPerSec: 26, ModelID: "heavy", VariantID: "q4", Method: "ollama_native"},
		recTestManifests(), "ollama", "0.32.13",
	)
	if want := activeVariantSHA(recTestManifests(), "heavy", "q4"); sha == "" || sha != want {
		t.Fatalf("key = %q, want the measured variant's SHA %q", sha, want)
	}
	if got.ModelID != "heavy" || got.VariantID != "q4" {
		t.Errorf("subject = %q/%q, want heavy/q4", got.ModelID, got.VariantID)
	}
	if got.MeasuredTokps != 26 {
		t.Errorf("MeasuredTokps = %v, want 26", got.MeasuredTokps)
	}
	if got.Method != "ollama_native" {
		t.Errorf("Method = %q, want ollama_native", got.Method)
	}
	if got.EngineKind != "ollama" || got.EngineVersion != "0.32.13" {
		t.Errorf("engine = %q/%q, want ollama/0.32.13", got.EngineKind, got.EngineVersion)
	}
	if got.MeasuredAt.IsZero() {
		t.Error("MeasuredAt is zero; a figure with no date cannot be aged out")
	}
}

// PRODUCT CONTRACT (waired-agent#784): every condition that would make
// the key a guess records NOTHING. Filing a real measurement against a
// model that was never run would make the ranking refuse a model on
// evidence about a different one — the confusion #783 fixed on the
// display side, arriving here through the persisted ledger instead.
func TestBenchMeasurement_RefusesToGuessTheSubject(t *testing.T) {
	for _, tt := range []struct {
		name  string
		bench BenchResult
	}{
		{"a failed run is not a measurement",
			BenchResult{TokensPerSec: 26, ModelID: "heavy", VariantID: "q4", Failed: true}},
		{"a zero rate is not a measurement",
			BenchResult{TokensPerSec: 0, ModelID: "heavy", VariantID: "q4"}},
		{"an unlabelled model cannot be keyed",
			BenchResult{TokensPerSec: 26, VariantID: "q4"}},
		{"an unlabelled variant cannot be keyed",
			BenchResult{TokensPerSec: 26, ModelID: "heavy"}},
		{"a variant the catalog does not have cannot be keyed",
			BenchResult{TokensPerSec: 26, ModelID: "heavy", VariantID: "q8"}},
		{"a model the catalog does not have cannot be keyed",
			BenchResult{TokensPerSec: 26, ModelID: "nosuch", VariantID: "q4"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sha, got := benchMeasurement(tt.bench, recTestManifests(), "ollama", "0.32.13")
			if sha != "" {
				t.Errorf("key = %q, want empty (record nothing)", sha)
			}
			if got != (catalog.VariantMeasurement{}) {
				t.Errorf("measurement = %+v, want the zero value", got)
			}
		})
	}
}

// TestInteractiveFloorVerdict_RestsOnTheShallowRateAlone.
//
// PRODUCT CONTRACT — owner ruling 2026-09-04, recorded in
// docs/decisions/20260904/0000-retire-the-long-context-sweep.md
// (waired-agent#1169). The #624 long-context sweep was the only other
// input to this verdict; it is gone, and the figure a person is shown in
// setup is now the whole of the comparison.
//
// This inverts TestRecommendationFromBench_DepthDecodeBinds and
// TestInteractiveFloorVerdict_OutOfMemoryIsNotSlowness, both removed in
// the same change. A host that decodes above the floor at zero depth and
// crawls at 128k is no longer noticed — accepted, and recorded there.
func TestInteractiveFloorVerdict_RestsOnTheShallowRateAlone(t *testing.T) {
	cfg := agentconfig.InferenceConfig{}
	floor := router.CodingAgentSelectionFloorTokps

	cases := []struct {
		name  string
		tokps float64
		below bool
	}{
		// The measured sv-evox2 figure: it used to be dragged below the
		// floor by a 42 tok/s decode at 128k, and no longer is.
		{"clears the floor", 81.5, false},
		{"at the floor", floor, false},
		{"below the floor", floor - 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := interactiveFloorVerdict(BenchResult{TokensPerSec: c.tokps, Capacity: 4}, cfg)
			if v.Below != c.below {
				t.Errorf("Below = %v, want %v (measured %v, floor %v)", v.Below, c.below, c.tokps, floor)
			}
			if v.Measured != c.tokps {
				t.Errorf("Measured = %v, want the boot benchmark's own rate %v", v.Measured, c.tokps)
			}
			if v.Floor != floor {
				t.Errorf("Floor = %v, want %v", v.Floor, floor)
			}
		})
	}

	// And the proposal follows the same single input.
	if rec := recommendationFromBench(
		BenchResult{TokensPerSec: 81.5, Capacity: 4},
		storeWithActive(t), cpuHost(), recTestManifests(), cfg, ""); rec != nil {
		t.Errorf("a host above the floor must not be offered a lighter model: %+v", rec)
	}
}
