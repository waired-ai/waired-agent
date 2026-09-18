package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

func turnSpeedStore(t *testing.T, hostClass string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "turnspeeds.json")
	body := `{"schema":1,"notes":"test","host_class":"` + hostClass + `","models":{}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// testEngineFlags is what snapshot records as the engine's launch flags
// unless a test writes its own with withRunner.
const testEngineFlags = "-c=200704 -np=1 -b=2048 -ub=2048 --cache-type-k=q4_0 --flash-attn=on"

// measurement is one measured_variants entry as the product writes it.
func measurement(modelID, variantID string, at time.Time, turn float64) catalog.VariantMeasurement {
	return catalog.VariantMeasurement{
		ModelID: modelID, VariantID: variantID, EngineKind: catalog.RuntimeOllama,
		EngineVersion: "0.34.0", MeasuredAt: at, MeasuredTokps: 40, PrefillTokps: 900,
		DepthTokens: 33313, TurnSeconds: turn, Samples: 1, AppliedWindow: 200704,
		KVCacheType: "q4_0", NumParallel: 1,
	}
}

// writeSnapshot writes a state.json whose last run is lb and whose
// measured_variants holds ms, with the engine's launch flags beside it.
func writeSnapshot(t *testing.T, lb catalog.BenchmarkRecord, ms ...catalog.VariantMeasurement) string {
	t.Helper()
	bundled, err := catalog.BundledManifests()
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]catalog.VariantMeasurement{}
	for _, m := range ms {
		v, ok := findShippedVariant(bundled, m.ModelID, m.VariantID)
		if !ok {
			t.Fatalf("no shipped %s/%s", m.ModelID, m.VariantID)
		}
		byKey[catalog.VariantSHA(v)] = m
	}
	data, _ := json.Marshal(map[string]any{"last_benchmark": lb, "measured_variants": byKey})
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return withRunner(t, p, testEngineFlags)
}

// ranHere is the last_benchmark a run that measured m leaves behind. The
// product stamps it with its own clock read, a fraction of a millisecond
// before the entry's (observed on the reference host: .1497 s against
// .1503 s).
func ranHere(m catalog.VariantMeasurement) catalog.BenchmarkRecord {
	return catalog.BenchmarkRecord{
		ModelID: m.ModelID, VariantID: m.VariantID, MeasuredAt: m.MeasuredAt.Add(-500 * time.Microsecond),
		TurnSeconds: m.TurnSeconds, Outcome: "measured",
	}
}

// snapshot writes a state.json taken right after the product measured
// modelID/variantID, with the engine's launch flags beside it.
func snapshot(t *testing.T, modelID, variantID string, at time.Time, turn float64, mutate func(*catalog.VariantMeasurement)) string {
	t.Helper()
	m := measurement(modelID, variantID, at, turn)
	if mutate != nil {
		mutate(&m)
	}
	return writeSnapshot(t, ranHere(m), m)
}

func TestTurnSpeedsImportTakesTheMedianOfRepeatedRuns(t *testing.T) {
	store := turnSpeedStore(t, "amd-unified-128gb")
	t0 := time.Date(2026, 9, 16, 13, 0, 0, 0, time.UTC)
	paths := []string{
		snapshot(t, "qwen3.5-4b", "q4-gguf", t0, 66.7, nil),
		snapshot(t, "qwen3.5-4b", "q4-gguf", t0.Add(time.Minute), 67.0, nil),
		snapshot(t, "qwen3.5-4b", "q4-gguf", t0.Add(2*time.Minute), 66.9, nil),
		// The same measurement seen twice counts once.
		snapshot(t, "qwen3.5-4b", "q4-gguf", t0.Add(2*time.Minute), 66.9, nil),
	}
	if err := runTurnSpeeds(append(flagsFor(paths), "--store", store, "--host", "amd-unified-128gb",
		"--backend", "vulkan", "--retrieved", "2026-09-16")); err != nil {
		t.Fatalf("import: %v", err)
	}
	set, err := loadTurnSpeeds(store)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := set.Lookup("qwen3.5-4b", "q4-gguf")
	if !ok {
		t.Fatal("no record written")
	}
	if rec.TurnSeconds != 66.9 || rec.Samples != 3 || rec.Method != catalog.TurnSpeedMeasured || rec.Backend != "vulkan" {
		t.Errorf("record = %+v, want the median 66.9 s of 3 samples", rec)
	}
	if rec.SpreadPct < 0.44 || rec.SpreadPct > 0.46 {
		t.Errorf("spread = %v%%, want (67.0-66.7)/66.9", rec.SpreadPct)
	}
}

func TestTurnSpeedsImportRefusesWhatIsNotTheProductsMeasurement(t *testing.T) {
	store := turnSpeedStore(t, "amd-unified-128gb")
	t0 := time.Date(2026, 9, 16, 13, 0, 0, 0, time.UTC)
	short := func(m *catalog.VariantMeasurement) { m.AppliedWindow = 32768 }
	paths := []string{
		// Too few runs.
		snapshot(t, "qwen3.5-2b", "q4-gguf", t0, 30, nil),
		snapshot(t, "qwen3.5-2b", "q4-gguf", t0.Add(time.Minute), 30, nil),
		// A small window is not the figure the step-down compares.
		snapshot(t, "qwen3.5-9b", "q4-gguf", t0, 90, short),
		snapshot(t, "qwen3.5-9b", "q4-gguf", t0.Add(time.Minute), 90, short),
		snapshot(t, "qwen3.5-9b", "q4-gguf", t0.Add(2*time.Minute), 90, short),
	}
	if err := runTurnSpeeds(append(flagsFor(paths), "--store", store, "--host", "amd-unified-128gb",
		"--retrieved", "2026-09-16")); err != nil {
		t.Fatalf("import: %v", err)
	}
	set, _ := loadTurnSpeeds(store)
	if len(set.Models) != 0 {
		t.Errorf("records written from refused samples: %+v", set.Models)
	}

	// Mixing host classes in one store is refused.
	other := turnSpeedStore(t, "apple-unified-48gb")
	err := runTurnSpeeds(append(flagsFor(paths[:1]), "--store", other, "--host", "amd-unified-128gb",
		"--retrieved", "2026-09-16"))
	if err == nil || !strings.Contains(err.Error(), "mix host classes") {
		t.Errorf("err = %v, want a refusal to mix host classes", err)
	}
}

func flagsFor(paths []string) []string {
	var args []string
	for _, p := range paths {
		args = append(args, "--import", p)
	}
	return args
}

// withRunner writes the engine's launch flags beside a snapshot, the way the
// measurement harness leaves them.
func withRunner(t *testing.T, snapshot, flags string) string {
	t.Helper()
	p := strings.TrimSuffix(snapshot, ".state.json") + ".runner.txt"
	if err := os.WriteFile(p, []byte(flags+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestTurnSpeedsImportRecordsTheEngineFlags(t *testing.T) {
	store := turnSpeedStore(t, "amd-unified-128gb")
	t0 := time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)
	const flags = "-c=200704 -np=1 -b=1024 -ub=1024 --cache-type-k=q4_0 --flash-attn=on"
	var paths []string
	for i := 0; i < 3; i++ {
		paths = append(paths, withRunner(t, snapshot(t, "qwen3.5-9b", "q4-gguf", t0.Add(time.Duration(i)*time.Minute), 80, nil), flags))
	}
	if err := runTurnSpeeds(append(flagsFor(paths), "--store", store, "--host", "amd-unified-128gb",
		"--retrieved", "2026-09-18")); err != nil {
		t.Fatalf("import: %v", err)
	}
	set, _ := loadTurnSpeeds(store)
	if rec, _ := set.Lookup("qwen3.5-9b", "q4-gguf"); rec.EngineFlags != flags {
		t.Errorf("engine_flags = %q, want %q", rec.EngineFlags, flags)
	}
}

// Record of a finding, not a product rule: on the reference host two loads of
// the same build picked different batches and a dense 27B's prefill moved by
// 2x, so samples taken under different engine flags are not one figure.
func TestTurnSpeedsImportRefusesSamplesUnderDifferentEngineFlags(t *testing.T) {
	store := turnSpeedStore(t, "amd-unified-128gb")
	t0 := time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)
	paths := []string{
		withRunner(t, snapshot(t, "qwen3.5-27b", "q4-gguf", t0, 318, nil), "-c=200704 -np=1 -b=2048 -ub=2048"),
		withRunner(t, snapshot(t, "qwen3.5-27b", "q4-gguf", t0.Add(time.Minute), 318, nil), "-c=200704 -np=1 -b=2048 -ub=2048"),
		withRunner(t, snapshot(t, "qwen3.5-27b", "q4-gguf", t0.Add(2*time.Minute), 500, nil), "-c=200704 -np=1 -b=512 -ub=512"),
	}
	err := runTurnSpeeds(append(flagsFor(paths), "--store", store, "--host", "amd-unified-128gb", "--retrieved", "2026-09-18"))
	if err == nil || !strings.Contains(err.Error(), "different engine flags") {
		t.Errorf("err = %v, want a refusal to fold samples taken under different engine flags", err)
	}
}

// Record of today's importer (#1400): a sample with no launch flags beside it
// cannot be compared with the others, so the whole variant is refused rather
// than written without them — whether one sample lacks them or all do.
func TestTurnSpeedsImportRefusesSamplesWithoutEngineFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		drop    []int
		wantErr string
	}{
		{"one sample", []int{1}, "2 of 3 samples carry the engine's flags"},
		{"every sample", []int{0, 1, 2}, "0 of 3 samples carry the engine's flags"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := turnSpeedStore(t, "amd-unified-128gb")
			t0 := time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)
			paths := []string{
				snapshot(t, "qwen3.5-9b", "q4-gguf", t0, 80, nil),
				snapshot(t, "qwen3.5-9b", "q4-gguf", t0.Add(time.Minute), 80, nil),
				snapshot(t, "qwen3.5-9b", "q4-gguf", t0.Add(2*time.Minute), 80, nil),
			}
			for _, i := range tc.drop {
				if err := os.Remove(paths[i] + ".runner.txt"); err != nil {
					t.Fatal(err)
				}
			}
			err := runTurnSpeeds(append(flagsFor(paths), "--store", store, "--host", "amd-unified-128gb", "--retrieved", "2026-09-18"))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want a refusal naming the sample count", err)
			}
			set, _ := loadTurnSpeeds(store)
			if len(set.Models) != 0 {
				t.Errorf("records written without flags: %+v", set.Models)
			}
		})
	}
}

// Record of today's importer (#1400): a snapshot contributes only what its
// own last run measured. On the reference host the product answered some
// runs with a figure stored two days earlier under another build (cached),
// and every snapshot also carried older entries for other variants; the
// runner file beside a snapshot describes neither. A failed run is the
// same case: the entry for its variant is an earlier run's.
func TestTurnSpeedsImportTakesOnlyWhatEachSnapshotsRunMeasured(t *testing.T) {
	store := turnSpeedStore(t, "amd-unified-128gb")
	old := time.Date(2026, 9, 16, 13, 0, 0, 0, time.UTC)
	t0 := time.Date(2026, 9, 18, 13, 0, 0, 0, time.UTC)
	stale9b := measurement("qwen3.5-9b", "q4-gguf", old, 140)
	stale4b := measurement("qwen3.5-4b", "q4-gguf", old.Add(time.Hour), 90)
	reused := ranHere(stale9b)
	reused.MeasuredAt, reused.Cached = t0, true
	// The same answer stamped with the figure's own time: still not a
	// sample, because the run measured nothing.
	reusedAsStamped := ranHere(stale9b)
	reusedAsStamped.MeasuredAt, reusedAsStamped.Cached = stale9b.MeasuredAt, true
	var paths []string
	// The automatic run answered with the stored 9b figure.
	paths = append(paths, writeSnapshot(t, reused, stale9b, stale4b))
	paths = append(paths, writeSnapshot(t, reusedAsStamped, stale9b, stale4b))
	// A run that failed leaves the entry an earlier run wrote.
	failed := catalog.BenchmarkRecord{ModelID: "qwen3.5-9b", VariantID: "q4-gguf", MeasuredAt: t0, Failed: true, Outcome: "failed"}
	paths = append(paths, writeSnapshot(t, failed, stale9b, stale4b))
	var run3 catalog.VariantMeasurement
	for i := 1; i <= 3; i++ {
		run3 = measurement("qwen3.5-9b", "q4-gguf", t0.Add(time.Duration(i)*time.Minute), 100)
		paths = append(paths, writeSnapshot(t, ranHere(run3), run3, stale4b))
	}
	// Then the harness moved to 4b, whose automatic run answered with the
	// stored figure. That snapshot's newest entry is 9b's third run, and
	// the runner file beside it is 4b's engine, not 9b's.
	reused4b := ranHere(stale4b)
	reused4b.MeasuredAt, reused4b.Cached = t0.Add(10*time.Minute), true
	paths = append(paths, withRunner(t, writeSnapshot(t, reused4b, run3, stale4b), "-c=200704 -np=1 -b=512 -ub=512"))
	if err := runTurnSpeeds(append(flagsFor(paths), "--store", store, "--host", "amd-unified-128gb",
		"--retrieved", "2026-09-18")); err != nil {
		t.Fatalf("import: %v", err)
	}
	set, _ := loadTurnSpeeds(store)
	rec, ok := set.Lookup("qwen3.5-9b", "q4-gguf")
	if !ok || rec.Samples != 3 || rec.TurnSeconds != 100 {
		t.Errorf("9b record = %+v, want 3 samples at 100 s (the reused 140 s figure is not a sample)", rec)
	}
	if rec, ok := set.Lookup("qwen3.5-4b", "q4-gguf"); ok {
		t.Errorf("4b recorded from entries no run in these snapshots measured: %+v", rec)
	}
}
