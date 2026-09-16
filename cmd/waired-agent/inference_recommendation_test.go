package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// recTestManifests returns two ollama families on a footprint ladder:
// heavy (12 GB RAM) and light (4 GB RAM).
func recTestManifests() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID: "heavy", ContextLength: 262144, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4", Format: "ollama-tag", Quantization: "Q4_K_M",
				RuntimeSupport: []string{"ollama"}, EstimatedWeightGB: 5.0,
				MinRAMGB: 12, QualityTier: 50, ParamCount: 8_000_000_000, KVBytesPerTokenFP16: 4096,
				Source: catalog.VariantSource{Type: "ollama", Tag: "heavy:8b"},
			}},
		},
		{
			ModelID: "light", ContextLength: 262144, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4", Format: "ollama-tag", Quantization: "Q4_K_M",
				RuntimeSupport: []string{"ollama"}, EstimatedWeightGB: 1.5,
				MinRAMGB: 4, QualityTier: 20, ParamCount: 2_000_000_000, KVBytesPerTokenFP16: 4096,
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

func TestRecommendationFromBench_OverTheLineSuggestsLighter(t *testing.T) {
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 15.8, DecodeTokps: 15.8, TurnSeconds: 228, Capacity: 1},
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec == nil {
		t.Fatalf("expected a recommendation, got nil")
	}
	if rec.FromModelID != "heavy" || rec.ToModelID != "light" {
		t.Errorf("from/to = %s→%s, want heavy→light", rec.FromModelID, rec.ToModelID)
	}
	if rec.TurnSeconds != 228 || rec.BudgetSeconds != hostfit.ModelTurnBudgetSeconds || rec.MeasuredTokps != 15.8 {
		t.Errorf("rec = %+v, want 228 s against %v", rec, hostfit.ModelTurnBudgetSeconds)
	}
	if rec.Dismissed {
		t.Errorf("Dismissed should be false on a fresh recommendation")
	}
}

func TestRecommendationFromBench_AtTheLineNil(t *testing.T) {
	// Exactly at the line is inside it — pins the boundary.
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 5, TurnSeconds: hostfit.ModelTurnBudgetSeconds, Capacity: 2},
		storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("at the line → want nil, got %+v", rec)
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
		BenchResult{TurnSeconds: 400, Capacity: 1},
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
		BenchResult{TurnSeconds: 400, Capacity: 1},
		store, cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
	if rec != nil {
		t.Errorf("already lightest → want nil, got %+v", rec)
	}
}

// PRODUCT CONTRACT (decision 3 of docs/decisions/20260913/2245): the
// retired interactive_floor_tokps judges nothing — a slow decode rate inside
// the line is not a reason to step down, whatever the setting says.
func TestRecommendationFromBench_TheRetiredFloorSettingJudgesNothing(t *testing.T) {
	rec := recommendationFromBench(
		BenchResult{TokensPerSec: 10, DecodeTokps: 10, TurnSeconds: 150, Capacity: 1},
		storeWithActive(t), cpuHost(), recTestManifests(),
		agentconfig.InferenceConfig{InteractiveFloorTokps: 60}, "")
	if rec != nil {
		t.Errorf("10 tok/s inside the line with the floor set to 60 → want nil, got %+v", rec)
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
		BenchResult{TurnSeconds: 228, Capacity: 1},
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
		ModelID: "tiny", ContextLength: 262144, Capabilities: []string{"chat"},
		Variants: []catalog.Variant{{
			VariantID: "q4", Format: "ollama-tag", Quantization: "Q4_K_M",
			RuntimeSupport: []string{"ollama"}, EstimatedWeightGB: 0.6,
			MinRAMGB: 2, QualityTier: 10, ParamCount: 600_000_000, KVBytesPerTokenFP16: 4096,
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
		for modelID, seconds := range measured {
			sha := activeVariantSHA(recTestLadder(), modelID, "q4")
			if sha == "" {
				t.Fatalf("no fixture variant for %q", modelID)
			}
			s.MeasuredVariants[sha] = catalog.VariantMeasurement{
				ModelID: modelID, VariantID: "q4", TurnSeconds: seconds,
			}
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return store
}

// PRODUCT CONTRACT (waired-agent#784): the step-down does not offer a
// rung this host has already tried and measured over the line.
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
		BenchResult{TurnSeconds: 400, Capacity: 1, ModelID: "heavy"},
		storeWithMeasured(t, "heavy", nil), cpuHost(), recTestLadder(),
		agentconfig.InferenceConfig{}, "")
	if first == nil {
		t.Fatal("no step-down offered for a slow heavy model")
	}
	if first.ToModelID != "light" {
		t.Fatalf("first step = %q, want light", first.ToModelID)
	}

	// Same host, same active model, but "light" has since been run here
	// and measured over the line. The next rung down is the only
	// honest offer left.
	again := recommendationFromBench(
		BenchResult{TurnSeconds: 400, Capacity: 1, ModelID: "heavy"},
		storeWithMeasured(t, "heavy", map[string]float64{"light": 260}),
		cpuHost(), recTestLadder(), agentconfig.InferenceConfig{}, "")
	if again == nil {
		t.Fatal("no step-down offered once light was known to be slow here")
	}
	if again.ToModelID != "tiny" {
		t.Errorf("step = %q, want tiny — light was already measured at 260 s per request here",
			again.ToModelID)
	}
}

// PRODUCT CONTRACT (waired-agent#784): a completed benchmark files its
// figure under the variant it MEASURED, so the ranking can stop
// recommending a model this host has already timed as too slow.
func TestBenchMeasurement_RecordsWhatWasMeasured(t *testing.T) {
	sha, got := benchMeasurement(
		BenchResult{TokensPerSec: 15.8, DecodeTokps: 15.8, PrefillTokps: 252.9, DepthTokens: 32780,
			TurnSeconds: 228, Samples: 1, ModelID: "heavy", VariantID: "q4", Method: "ollama_eval"},
		recTestManifests(), BenchDeps{EngineKind: "ollama", EngineVersion: "0.32.13",
			GPUModel: "RTX", VRAMTotalMB: 24000, DriverVersion: "595", AppliedWindow: 200704, KVCacheType: "q4_0", NumParallel: 1},
	)
	if want := activeVariantSHA(recTestManifests(), "heavy", "q4"); sha == "" || sha != want {
		t.Fatalf("key = %q, want the measured variant's SHA %q", sha, want)
	}
	if got.ModelID != "heavy" || got.VariantID != "q4" {
		t.Errorf("subject = %q/%q, want heavy/q4", got.ModelID, got.VariantID)
	}
	if got.MeasuredTokps != 15.8 || got.TurnSeconds != 228 || got.PrefillTokps != 252.9 || got.DepthTokens != 32780 {
		t.Errorf("measurement = %+v, want the seconds and the rates behind them", got)
	}
	if got.GPUModel != "RTX" || got.VRAMTotalMB != 24000 || got.DriverVersion != "595" ||
		got.AppliedWindow != 200704 || got.KVCacheType != "q4_0" || got.NumParallel != 1 {
		t.Errorf("configuration = %+v, want what the stored figure may answer for", got)
	}
	if got.Method != "ollama_eval" {
		t.Errorf("Method = %q, want ollama_eval", got.Method)
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
			BenchResult{TurnSeconds: 228, ModelID: "heavy", VariantID: "q4", Failed: true}},
		{"no seconds is not a measurement",
			BenchResult{TokensPerSec: 26, ModelID: "heavy", VariantID: "q4"}},
		{"a lower bound is not a stored speed",
			BenchResult{TurnFloorSeconds: 240, ModelID: "heavy", VariantID: "q4"}},
		{"an unlabelled model cannot be keyed",
			BenchResult{TurnSeconds: 228, VariantID: "q4"}},
		{"an unlabelled variant cannot be keyed",
			BenchResult{TurnSeconds: 228, ModelID: "heavy"}},
		{"a variant the catalog does not have cannot be keyed",
			BenchResult{TurnSeconds: 228, ModelID: "heavy", VariantID: "q8"}},
		{"a model the catalog does not have cannot be keyed",
			BenchResult{TurnSeconds: 228, ModelID: "nosuch", VariantID: "q4"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sha, got := benchMeasurement(tt.bench, recTestManifests(), BenchDeps{EngineKind: "ollama", EngineVersion: "0.32.13"})
			if sha != "" {
				t.Errorf("key = %q, want empty (record nothing)", sha)
			}
			if got != (catalog.VariantMeasurement{}) {
				t.Errorf("measurement = %+v, want the zero value", got)
			}
		})
	}
}

// TestSpeedVerdict_JudgesSecondsPerRequest replaces
// TestInteractiveFloorVerdict_RestsOnTheShallowRateAlone, which decision
// 20260904/0000 pinned and decision 3 of docs/decisions/20260913/2245 (its
// Consequences name this test) supersedes: the verdict is one request's
// seconds against the line, on the finished figure or on a lower bound
// already past it. A rate judges nothing.
func TestSpeedVerdict_JudgesSecondsPerRequest(t *testing.T) {
	line := hostfit.ModelTurnBudgetSeconds
	for _, c := range []struct {
		name  string
		bench BenchResult
		over  bool
	}{
		{"228 s is over the line", BenchResult{TurnSeconds: 228, TokensPerSec: 15.8, Capacity: 1}, true},
		{"70 s is inside it", BenchResult{TurnSeconds: 70, TokensPerSec: 45.7, Capacity: 4}, false},
		{"a 195 s bound is already over", BenchResult{TurnFloorSeconds: 195, Capacity: 1}, true},
		{"a 120 s bound says nothing yet", BenchResult{TurnFloorSeconds: 120, Capacity: 1}, false},
		{"a slow rate with no seconds is no claim", BenchResult{TokensPerSec: 5, Capacity: 1}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := speedVerdictOf(c.bench)
			if v.Over != c.over || v.Budget != line {
				t.Errorf("verdict = %+v, want Over=%v against %v", v, c.over, line)
			}
			rec := recommendationFromBench(c.bench, storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, "")
			if (rec != nil) != c.over {
				t.Errorf("recommendation = %+v, want one only when over the line", rec)
			}
			if rec != nil && rec.TurnSeconds != c.bench.TurnSeconds && rec.TurnFloorSeconds != c.bench.TurnFloorSeconds {
				t.Errorf("recommendation carries %+v, want the figure it rests on", rec)
			}
		})
	}
	for _, c := range []struct {
		name  string
		bench BenchResult
	}{
		{"failed", BenchResult{TurnSeconds: 400, Capacity: 1, Failed: true}},
		{"skipped", BenchResult{TurnSeconds: 400, Capacity: 0}},
	} {
		if rec := recommendationFromBench(c.bench, storeWithActive(t), cpuHost(), recTestManifests(), agentconfig.InferenceConfig{}, ""); rec != nil {
			t.Errorf("%s run → want nil, got %+v", c.name, rec)
		}
	}
}
