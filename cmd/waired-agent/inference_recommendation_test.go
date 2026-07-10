package main

import (
	"path/filepath"
	"strings"
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
		BenchResult{TokensPerSec: 10, Capacity: 1}, nil,
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
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 120, Capacity: 4}, nil,
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("above floor → want nil, got %+v", rec)
	}
}

func TestRecommendationFromBench_FailedNil(t *testing.T) {
	rec := recommendationFromBench(
		BenchResult{Failed: true, Capacity: 1, Err: "timeout"}, nil,
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("failed benchmark → want nil, got %+v", rec)
	}
}

func TestRecommendationFromBench_SkippedNil(t *testing.T) {
	// Capacity==0 with Failed==false is the "skipped" encoding.
	rec := recommendationFromBench(
		BenchResult{Capacity: 0}, nil,
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("skipped benchmark → want nil, got %+v", rec)
	}
}

func TestRecommendationFromBench_NoActiveNil(t *testing.T) {
	emptyStore := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 5, Capacity: 1}, nil,
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
		BenchResult{TokensPerSec: 5, Capacity: 1}, nil,
		store, cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("already lightest → want nil, got %+v", rec)
	}
}

func TestRecommendationFromBench_ConfigurableFloor(t *testing.T) {
	// floor=8 → a 10 tok/s result is now ABOVE the floor → no suggestion.
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 10, Capacity: 1}, nil,
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
		BenchResult{TokensPerSec: 10, Capacity: 1}, nil,
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
	// tok/s ≥ 100 × 1.25 bar → upgrade light→heavy.
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
	// Above the floor (120 ≥ 100) but heavy predicts only 36 tok/s
	// (120 × 1.5/5) < the 125 bar → no upgrade.
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

// #624/#670: the depth sweep's worst completed decode binds the lighter
// recommendation when it falls under floor × CodingAgentDepthFloorFraction
// — a host can decode fine at zero depth and crawl at 131k. The depth leg
// is held to the scaled floor (80 at the 100 default), not the full one:
// the shallow floor already prices in the expected depth degradation.
func TestRecommendationFromBench_DepthDecodeBinds(t *testing.T) {
	depth := &DepthBenchResult{Stages: []DepthStageResult{
		{TargetTokens: 65536, DecodeTokps: 110},
		{TargetTokens: 131072, DecodeTokps: 22},
	}}
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 120, Capacity: 4}, depth,
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec == nil {
		t.Fatal("expected a recommendation: worst depth decode 22 < depth floor 80")
	}
	if rec.MeasuredTokps != 22 {
		t.Errorf("MeasuredTokps = %v, want the binding depth decode 22", rec.MeasuredTokps)
	}
	if !strings.Contains(rec.Reason, "~128k context") {
		t.Errorf("Reason should name the binding depth: %q", rec.Reason)
	}

	// Depth between the scaled floor (80) and the full floor (100) does
	// NOT bind — demanding the full floor at depth would double-count
	// the degradation the shallow floor already prices in.
	fast := &DepthBenchResult{Stages: []DepthStageResult{{TargetTokens: 131072, DecodeTokps: 90}}}
	if rec := recommendationFromBench(
		BenchResult{TokensPerSec: 120, Capacity: 4}, fast,
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, ""); rec != nil {
		t.Errorf("120 tok/s boot + 90 tok/s depth → want nil, got %+v", rec)
	}

	// Failed stages are ignored.
	failed := &DepthBenchResult{Stages: []DepthStageResult{{TargetTokens: 131072, Failed: true}}}
	if rec := recommendationFromBench(
		BenchResult{TokensPerSec: 120, Capacity: 4}, failed,
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, ""); rec != nil {
		t.Errorf("failed depth stages must not bind: %+v", rec)
	}
}
