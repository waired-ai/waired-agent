package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// PRODUCT CONTRACT (decision 7 of docs/decisions/20260913/2245): a setup
// generation or `waired init` (mode ensure) answers from the stored
// measurement; `waired runtimes benchmark` (mode rerun) measures over it.
func TestBenchmarkJob_TheModeDecidesWhetherTheStoreMayAnswer(t *testing.T) {
	p := benchJobProvider(t, func(context.Context) BenchResult {
		return BenchResult{TokensPerSec: 45.7, TurnSeconds: 70, Capacity: 1}
	})
	var skip []bool
	p.speedDepsHook = func(d *BenchDeps) { skip = append(skip, d.SkipCacheLoad) }
	waitDone(t, p.startBenchmarkJob(0, management.BenchmarkModeEnsure))
	waitDone(t, p.startBenchmarkJob(0, management.BenchmarkModeRerun))
	if len(skip) != 2 || skip[0] || !skip[1] {
		t.Fatalf("SkipCacheLoad per run = %v, want [false true] for ensure, rerun", skip)
	}
}

// PRODUCT CONTRACT (decision 4): while a run is past the line, status says
// so — elapsed, the line, the lower bound — and offers the lighter model at
// that moment, without waiting for the end.
func TestBenchmarkStatus_ARunPastTheLineOffersTheSwitchNow(t *testing.T) {
	p := statusRecProvider(t, BenchResult{})
	p.benchJobMu.Lock()
	p.benchJobDone = make(chan struct{})
	p.benchJobVariant = p.activeSelectionKey()
	p.benchJobProgress = &BenchProgress{
		Phase: benchPhaseMeasuring, ElapsedSeconds: 200, BudgetSeconds: 190,
		OverBudget: true, TurnFloorSeconds: 201, DepthTokens: 32768,
	}
	p.benchJobMu.Unlock()

	got := p.BenchmarkStatus()
	if got.State != management.BenchmarkStateRunning {
		t.Fatalf("state = %q, want running — over the line is not an ending", got.State)
	}
	if !got.OverBudget || got.ElapsedSeconds != 200 || got.BudgetSeconds != 190 || got.TurnFloorSeconds != 201 || got.TurnSeconds != 0 {
		t.Errorf("running speed = %+v", got.SpeedMeasurement)
	}
	if got.Recommendation == nil || got.Recommendation.ToModelID != "light" || got.Recommendation.TurnFloorSeconds != 201 {
		t.Errorf("recommendation = %+v, want light on the 201 s bound", got.Recommendation)
	}
	if ms := p.modelSpeedStatus(); ms == nil || !ms.Running || !ms.OverBudget || ms.ElapsedSeconds != 200 {
		t.Errorf("model_speed = %+v, want the running measurement", ms)
	}

	// Inside the line, nothing is offered yet.
	p.benchJobMu.Lock()
	p.benchJobProgress = &BenchProgress{Phase: benchPhaseMeasuring, ElapsedSeconds: 40, BudgetSeconds: 190}
	p.benchJobMu.Unlock()
	if got := p.BenchmarkStatus(); got.OverBudget || got.Recommendation != nil {
		t.Errorf("inside the line: %+v / %+v", got.SpeedMeasurement, got.Recommendation)
	}
}

// A finished run reports its figure in seconds and, over the line, the
// recommendation beside it.
func TestBenchmarkStatus_AFinishedRunCarriesTheSeconds(t *testing.T) {
	p := statusRecProvider(t, BenchResult{TokensPerSec: 15.8, TurnSeconds: 228, Capacity: 1, ModelID: "heavy", VariantID: "q4"})
	if err := p.store.Update(func(s *catalog.State) {
		s.LastBenchmark = &catalog.BenchmarkRecord{
			Gen: 3, ModelID: "heavy", VariantID: "q4", MeasuredTokps: 15.8, DecodeTokps: 15.8,
			PrefillTokps: 252.9, DepthTokens: 32780, TurnSeconds: 228, Outcome: benchOutcomeMeasured,
			MeasuredAt: time.Now().UTC(),
		}
	}); err != nil {
		t.Fatal(err)
	}
	got := p.BenchmarkStatus()
	if got.State != management.BenchmarkStateDone || got.TurnSeconds != 228 || !got.OverBudget ||
		got.BudgetSeconds != hostfit.ModelTurnBudgetSeconds || got.PrefillTokps != 252.9 || got.DepthTokens != 32780 {
		t.Fatalf("status = %+v", got)
	}
	if got.Recommendation == nil || got.Recommendation.ToModelID != "light" {
		t.Errorf("recommendation = %+v, want light", got.Recommendation)
	}
	if ms := p.modelSpeedStatus(); ms == nil || ms.Running || ms.TurnSeconds != 228 || !ms.OverBudget {
		t.Errorf("model_speed = %+v", ms)
	}
}

// PRODUCT CONTRACT (decision 9): /healthz publishes the served model's
// seconds per request — a lower bound for a stalled measurement — and
// withholds a figure measured on another model or variant.
func TestHealthPublishers_ServeTheServedModelsMeasurement(t *testing.T) {
	p := statusRecProvider(t, BenchResult{
		VariantID: "q4", ModelID: "heavy", TurnSeconds: 228, PrefillTokps: 252.9, DecodeTokps: 15.8,
		DepthTokens: 32780, Samples: 1, Capacity: 1, Outcome: benchOutcomeMeasured,
	})
	s := p.SpeedForHealth()
	if s == nil || s.TurnSeconds != 228 || s.PrefillTokps != 252.9 || s.DecodeTokps != 15.8 || s.DepthTokens != 32780 || s.MeasuredAt == "" {
		t.Fatalf("speed = %+v", s)
	}

	// A bound: only the lower bound is published.
	p.SetLastBench(BenchResult{VariantID: "q4", TurnFloorSeconds: 400, DepthTokens: 32768, Capacity: 1, Outcome: benchOutcomeMeasured})
	if s := p.SpeedForHealth(); s == nil || s.TurnFloorSeconds != 400 || s.TurnSeconds != 0 {
		t.Errorf("bound speed = %+v", s)
	}

	// Another MODEL with the same variant id is served now: nothing is
	// published. Variant ids repeat across models ("mtp-q4-gguf" belongs to
	// more than one), and comparing the variant alone published the
	// previous model's figure against the next for the seconds a switch
	// takes — seen on hardware switching 35B-A3B to 27B.
	p.SetLastBench(BenchResult{VariantID: "q4", ModelID: "heavy", TurnSeconds: 228, DepthTokens: 32780, Capacity: 1, Outcome: benchOutcomeMeasured})
	if err := p.store.Update(func(st *catalog.State) { st.Active.ModelID = "light" }); err != nil {
		t.Fatal(err)
	}
	if p.SpeedForHealth() != nil || p.modelSpeedStatus() != nil {
		t.Error("another model's figure was published because the two share a variant id")
	}
	if err := p.store.Update(func(st *catalog.State) { st.Active.ModelID = "heavy" }); err != nil {
		t.Fatal(err)
	}

	// Another variant is served now: nothing is published.
	if err := p.store.Update(func(st *catalog.State) { st.Active.VariantID = "q8" }); err != nil {
		t.Fatal(err)
	}
	if p.SpeedForHealth() != nil {
		t.Error("a figure measured on another variant was published against the served one")
	}

	// A failed run publishes nothing.
	p.SetLastBench(BenchResult{VariantID: "q8", Failed: true, Capacity: 1})
	if p.SpeedForHealth() != nil {
		t.Error("a failed run was published as a speed")
	}
}

// The setup benchmark row carries the served-model measurement: running
// past the line stays running with the bound; done carries the seconds.
// There is no prefill_measurement row any more.
func TestSetupSnapshot_TheBenchmarkRowCarriesTheSeconds(t *testing.T) {
	ctx := context.Background()
	f := &fakeSetupProvider{engineInstalled: true, engineReady: true, modelState: catalog.ModelStateReady}
	f.bench = management.BenchmarkStatusResponse{State: management.BenchmarkStateRunning,
		SpeedMeasurement: management.SpeedMeasurement{ElapsedSeconds: 200, BudgetSeconds: 190, OverBudget: true, TurnFloorSeconds: 201, DepthTokens: 32768}}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(ctx, desiredFrame("ollama", "m1", 1))

	snap := r.snapshot(ctx)
	var bench *signer.SetupStep
	for i := range snap.Steps {
		if snap.Steps[i].ID == "prefill_measurement" {
			t.Errorf("a prefill_measurement row is still reported: %+v", snap.Steps[i])
		}
		if snap.Steps[i].ID == setupStepBenchmark {
			bench = &snap.Steps[i]
		}
	}
	if bench == nil || bench.Status != signer.SetupStatusRunning {
		t.Fatalf("benchmark row = %+v, want running", bench)
	}
	if b := snap.Benchmark; b == nil || !b.OverBudget || b.ElapsedSeconds != 200 || b.BudgetSeconds != 190 || b.TurnFloorSeconds != 201 || b.Gen != 1 {
		t.Fatalf("running benchmark = %+v", snap.Benchmark)
	}

	f.mu.Lock()
	f.bench = management.BenchmarkStatusResponse{State: management.BenchmarkStateDone, Gen: 1, MeasuredTokps: 15.8,
		SpeedMeasurement: management.SpeedMeasurement{TurnSeconds: 228, BudgetSeconds: 190, OverBudget: true, PrefillTokps: 252.9, DecodeTokps: 15.8, DepthTokens: 32780, Cached: true}}
	f.mu.Unlock()
	snap = r.snapshot(ctx)
	if b := snap.Benchmark; b == nil || b.TurnSeconds != 228 || !b.OverBudget || b.PrefillTokps != 252.9 || b.DecodeTokps != 15.8 ||
		b.DepthTokens != 32780 || !b.Cached || b.MeasuredTokps != 15.8 {
		t.Fatalf("done benchmark = %+v", snap.Benchmark)
	}
}

// PRODUCT CONTRACT (decision 7): the ledger answers only for the same
// weights on the same engine release, GPU and serving configuration, and
// only with a finished figure.
func TestStoredSpeedMeasurement_AnswersOnlyForTheSameConfiguration(t *testing.T) {
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	entry := catalog.VariantMeasurement{
		ModelID: "heavy", VariantID: "q4", MeasuredTokps: 15.8, PrefillTokps: 252.9, TurnSeconds: 228, DepthTokens: 32780,
		EngineKind: "ollama", EngineVersion: "0.33.3", GPUModel: "RTX", VRAMTotalMB: 24000, DriverVersion: "595",
		AppliedWindow: 200704, KVCacheType: "q4_0", NumParallel: 1,
	}
	if err := store.Update(func(s *catalog.State) {
		s.MeasuredVariants = map[string]catalog.VariantMeasurement{"sha": entry}
	}); err != nil {
		t.Fatal(err)
	}
	p := &agentInferenceProvider{store: store}
	base := BenchDeps{VariantSHA: "sha", EngineKind: "ollama", EngineVersion: "0.33.3", GPUModel: "RTX", VRAMTotalMB: 24000,
		DriverVersion: "595", AppliedWindow: 200704, KVCacheType: "q4_0", NumParallel: 1}
	if got, ok := p.storedSpeedMeasurement(base); !ok || got.TurnSeconds != 228 || got.PrefillTokps != 252.9 || got.TokensPerSec != 15.8 {
		t.Fatalf("matching configuration = %+v / %v", got, ok)
	}
	for name, mutate := range map[string]func(*BenchDeps){
		"variant":        func(d *BenchDeps) { d.VariantSHA = "other" },
		"engine kind":    func(d *BenchDeps) { d.EngineKind = "vllm" },
		"engine release": func(d *BenchDeps) { d.EngineVersion = "0.34.0" },
		"no release":     func(d *BenchDeps) { d.EngineVersion = "" },
		"gpu":            func(d *BenchDeps) { d.GPUModel = "Other" },
		"vram":           func(d *BenchDeps) { d.VRAMTotalMB = 16000 },
		"driver":         func(d *BenchDeps) { d.DriverVersion = "550" },
		"window":         func(d *BenchDeps) { d.AppliedWindow = 32768 },
		"kv cache type":  func(d *BenchDeps) { d.KVCacheType = "q8_0" },
		"parallel":       func(d *BenchDeps) { d.NumParallel = 2 },
	} {
		d := base
		mutate(&d)
		if _, ok := p.storedSpeedMeasurement(d); ok {
			t.Errorf("a different %s was answered from the ledger", name)
		}
	}
	if err := store.Update(func(s *catalog.State) {
		e := entry
		e.TurnSeconds = 0
		s.MeasuredVariants["sha"] = e
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.storedSpeedMeasurement(base); ok {
		t.Error("an entry with no seconds (a decode rate from before #1341) answered")
	}
}

// An unverified engine tuning holds the measurement off for its patience,
// counted from when this daemon first saw it, and no longer.
func TestSpeedTuningWatch_PatienceIsCountedFromFirstSight(t *testing.T) {
	var w speedTuningWatch
	t0 := time.Unix(1_700_000_000, 0)
	if !w.pending("k", false, t0) {
		t.Fatal("a fresh unverified tuning is not pending")
	}
	if !w.pending("k", false, t0.Add(speedTuningPatience-time.Second)) {
		t.Error("pending ended before its patience")
	}
	if w.pending("k", false, t0.Add(speedTuningPatience)) {
		t.Error("an engine that never verifies was held off past the patience")
	}
	if !w.pending("k2", false, t0.Add(speedTuningPatience)) {
		t.Error("a new selection did not restart the patience")
	}
	if w.pending("k2", true, t0.Add(speedTuningPatience)) {
		t.Error("a verified tuning is still pending")
	}
}
