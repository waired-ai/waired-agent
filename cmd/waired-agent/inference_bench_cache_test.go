package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempBenchCache(t *testing.T) *benchCache {
	t.Helper()
	dir := t.TempDir()
	return newBenchCache(filepath.Join(dir, "bench.json"), nil)
}

func TestBenchCache_LoadAbsentReturnsMiss(t *testing.T) {
	c := tempBenchCache(t)
	got, _, hit, err := c.Load("anykey")
	if err != nil {
		t.Fatalf("Load on missing file errored: %v", err)
	}
	if hit {
		t.Fatalf("expected miss for absent file, got hit %+v", got)
	}
}

func TestBenchCache_RoundTrip(t *testing.T) {
	c := tempBenchCache(t)
	now := time.Date(2026, 5, 16, 9, 0, 0, 0, time.UTC)
	res := BenchResult{
		TokensPerSec: 123.4, Capacity: 4, VariantID: "qwen3-8b-q4-gguf",
		Method: benchMethodOllamaEval, SpreadPct: 6.3,
	}
	meta := benchCacheHumanMeta{
		VariantID:     "qwen3-8b-q4-gguf",
		GPUModel:      "NVIDIA GeForce RTX 4090",
		VRAMTotalMB:   24576,
		DriverVersion: "595.58.03",
		EngineKind:    "ollama",
		EngineModel:   "qwen3:8b",
	}
	if err := c.Store("k1", res, meta, now); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, ts, hit, err := c.Load("k1")
	if err != nil || !hit {
		t.Fatalf("Load: err=%v hit=%v", err, hit)
	}
	if got.TokensPerSec != 123.4 || got.Capacity != 4 || got.VariantID != "qwen3-8b-q4-gguf" {
		t.Fatalf("Load returned unexpected result: %+v", got)
	}
	if got.Method != benchMethodOllamaEval || got.SpreadPct != 6.3 {
		t.Fatalf("Method/SpreadPct did not round-trip: %+v", got)
	}
	if !ts.Equal(now) {
		t.Fatalf("Load returned measured_at %v, want %v", ts, now)
	}
}

func TestBenchCache_MultipleEntriesCoexist(t *testing.T) {
	c := tempBenchCache(t)
	now := time.Date(2026, 5, 16, 9, 0, 0, 0, time.UTC)
	a := BenchResult{TokensPerSec: 100, Capacity: 3, VariantID: "a"}
	b := BenchResult{TokensPerSec: 200, Capacity: 6, VariantID: "b"}
	meta := benchCacheHumanMeta{GPUModel: "RTX 4090", EngineKind: "ollama"}
	if err := c.Store("key-a", a, meta, now); err != nil {
		t.Fatalf("Store a: %v", err)
	}
	if err := c.Store("key-b", b, meta, now.Add(time.Hour)); err != nil {
		t.Fatalf("Store b: %v", err)
	}
	gotA, _, hitA, _ := c.Load("key-a")
	gotB, _, hitB, _ := c.Load("key-b")
	if !hitA || !hitB {
		t.Fatalf("expected both hits, got hitA=%v hitB=%v", hitA, hitB)
	}
	if gotA.Capacity != 3 || gotB.Capacity != 6 {
		t.Fatalf("entries cross-contaminated: a=%+v b=%+v", gotA, gotB)
	}
}

func TestBenchCache_CorruptFileTreatedAsMiss(t *testing.T) {
	c := tempBenchCache(t)
	if err := os.WriteFile(c.path, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	_, _, hit, err := c.Load("k")
	if err != nil {
		t.Fatalf("Load errored on corrupt file (want warn + miss): %v", err)
	}
	if hit {
		t.Fatalf("Load reported hit on corrupt file")
	}

	// A subsequent Store should overwrite cleanly and become loadable.
	res := BenchResult{Capacity: 2, VariantID: "x"}
	if err := c.Store("k", res, benchCacheHumanMeta{GPUModel: "g"}, time.Now()); err != nil {
		t.Fatalf("Store after corrupt: %v", err)
	}
	got, _, hit, err := c.Load("k")
	if err != nil || !hit || got.Capacity != 2 {
		t.Fatalf("Load after Store-over-corrupt: err=%v hit=%v got=%+v", err, hit, got)
	}
}

func TestBenchCache_SchemaVersionMismatchTreatedAsMiss(t *testing.T) {
	c := tempBenchCache(t)
	// Write a syntactically-valid file with a version we don't accept.
	if err := os.WriteFile(c.path,
		[]byte(`{"version":999,"entries":{"k":{"capacity":7}}}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, hit, err := c.Load("k")
	if err != nil {
		t.Fatalf("Load errored on schema mismatch: %v", err)
	}
	if hit {
		t.Fatalf("Load reported hit despite schema mismatch")
	}
}

// TestBenchCache_StoreDropsStaleSchemaEntries guards the schema-bump
// invalidation: Store's read-modify-write must NOT carry entries from
// an older schema version into the new file, or measurements taken
// under the old (buggy) semantics would resurface as fresh cache hits
// for other variants after the bump.
func TestBenchCache_StoreDropsStaleSchemaEntries(t *testing.T) {
	c := tempBenchCache(t)
	if err := os.WriteFile(c.path,
		[]byte(`{"version":1,"entries":{"stale-key":{"capacity":7,"tokens_per_sec":4.6}}}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.Store("new-key", BenchResult{Capacity: 3, TokensPerSec: 93}, benchCacheHumanMeta{}, time.Now()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var file benchCacheFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse back: %v", err)
	}
	if file.Version != benchCacheSchemaVersion {
		t.Errorf("Version = %d, want %d", file.Version, benchCacheSchemaVersion)
	}
	if _, ok := file.Entries["stale-key"]; ok {
		t.Error("stale-key from schema v1 survived the version bump")
	}
	if _, ok := file.Entries["new-key"]; !ok {
		t.Error("new-key missing after Store")
	}
}

// TestBenchCache_V2WallClockEntriesInvalidated pins the #764 upgrade
// path: a v2 file (single-run wall-clock semantics, understates fast
// hosts ~35%) must read as a miss, and the next Store must rewrite the
// file as the current schema without carrying the v2 entries along.
func TestBenchCache_V2WallClockEntriesInvalidated(t *testing.T) {
	c := tempBenchCache(t)
	if err := os.WriteFile(c.path,
		[]byte(`{"version":2,"entries":{"v2-key":{"tokens_per_sec":50.0,"capacity":1,"variant_id":"x","gpu_model":"g","engine_kind":"ollama","measured_at":"2026-07-01T00:00:00Z"}}}`),
		0o644); err != nil {
		t.Fatalf("seed v2 file: %v", err)
	}
	if _, _, hit, err := c.Load("v2-key"); err != nil || hit {
		t.Fatalf("v2 entry served as hit (err=%v hit=%v); wall-clock numbers must be re-measured", err, hit)
	}
	if err := c.Store("v3-key", BenchResult{TokensPerSec: 78, Capacity: 2, Method: benchMethodOllamaEval}, benchCacheHumanMeta{}, time.Now()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var file benchCacheFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse back: %v", err)
	}
	if file.Version != benchCacheSchemaVersion {
		t.Errorf("Version = %d, want %d", file.Version, benchCacheSchemaVersion)
	}
	if _, ok := file.Entries["v2-key"]; ok {
		t.Error("v2 wall-clock entry survived the schema bump")
	}
}

func TestBenchCache_NilReceiverIsSafe(t *testing.T) {
	var c *benchCache
	if _, _, hit, err := c.Load("anything"); err != nil || hit {
		t.Fatalf("nil Load: err=%v hit=%v", err, hit)
	}
	if err := c.Store("k", BenchResult{}, benchCacheHumanMeta{}, time.Now()); err != nil {
		t.Fatalf("nil Store: %v", err)
	}
	if err := c.Invalidate(); err != nil {
		t.Fatalf("nil Invalidate: %v", err)
	}
}

func TestBenchCache_EmptyKeyIsNoOp(t *testing.T) {
	c := tempBenchCache(t)
	_, _, hit, _ := c.Load("")
	if hit {
		t.Fatalf("empty-key Load returned hit")
	}
	if err := c.Store("", BenchResult{Capacity: 1}, benchCacheHumanMeta{}, time.Now()); err != nil {
		t.Fatalf("empty-key Store: %v", err)
	}
	// Cache file should not have been created by the no-op store.
	if _, err := os.Stat(c.path); !os.IsNotExist(err) {
		t.Fatalf("expected cache file absent after empty-key Store, got err=%v", err)
	}
}

func TestBenchCache_Invalidate(t *testing.T) {
	c := tempBenchCache(t)
	if err := c.Store("k", BenchResult{Capacity: 1}, benchCacheHumanMeta{GPUModel: "g"}, time.Now()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := os.Stat(c.path); err != nil {
		t.Fatalf("file should exist after Store: %v", err)
	}
	if err := c.Invalidate(); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := os.Stat(c.path); !os.IsNotExist(err) {
		t.Fatalf("file should be gone after Invalidate, got err=%v", err)
	}
	// Idempotent: second Invalidate on missing file is fine.
	if err := c.Invalidate(); err != nil {
		t.Fatalf("Invalidate (no file): %v", err)
	}
}

func TestBenchCacheKey_Stable(t *testing.T) {
	d := BenchDeps{
		EngineKind:    "ollama",
		EngineModel:   "qwen3:8b",
		EngineVersion: "0.33.2",
		GPUModel:      "RTX 4090",
		VRAMTotalMB:   24576,
		DriverVersion: "595.58.03",
		VariantSHA:    "abc123",
	}
	a := benchCacheKey(d)
	b := benchCacheKey(d)
	if a == "" || a != b {
		t.Fatalf("expected stable non-empty key, got %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64-char hex key, got %d chars: %q", len(a), a)
	}
}

func TestBenchCacheKey_EmptyWhenMissingInputs(t *testing.T) {
	cases := []struct {
		name string
		d    BenchDeps
	}{
		{"no GPU model", BenchDeps{VariantSHA: "abc", EngineKind: "ollama", EngineVersion: "0.33.2"}},
		{"no variant SHA", BenchDeps{GPUModel: "RTX 4090", EngineKind: "ollama", EngineVersion: "0.33.2"}},
		// An engine whose version could not be read is not evidence
		// that it is current, so it disables caching rather than
		// keying an entry that would outlive the engine (#1131).
		{"no engine version", BenchDeps{GPUModel: "RTX 4090", VariantSHA: "abc", EngineKind: "ollama"}},
		{"all empty", BenchDeps{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if k := benchCacheKey(tc.d); k != "" {
				t.Fatalf("expected empty key, got %q", k)
			}
		})
	}
}

func TestBenchCacheKey_VariesWithInputs(t *testing.T) {
	base := BenchDeps{
		EngineKind:    "ollama",
		EngineModel:   "qwen3:8b",
		EngineVersion: "0.33.2",
		GPUModel:      "RTX 4090",
		VRAMTotalMB:   24576,
		DriverVersion: "595.58.03",
		VariantSHA:    "abc123",
	}
	baseKey := benchCacheKey(base)

	cases := []struct {
		name   string
		mutate func(*BenchDeps)
	}{
		{"GPUModel", func(d *BenchDeps) { d.GPUModel = "RTX 4080" }},
		{"VRAMTotalMB", func(d *BenchDeps) { d.VRAMTotalMB = 16384 }},
		{"DriverVersion", func(d *BenchDeps) { d.DriverVersion = "550.0.0" }},
		{"VariantSHA", func(d *BenchDeps) { d.VariantSHA = "different" }},
		{"EngineKind", func(d *BenchDeps) { d.EngineKind = "vllm" }},
		{"EngineModel", func(d *BenchDeps) { d.EngineModel = "llama3:8b" }},
		// The point of #1131: the same host, card, variant and model on
		// a NEWER engine must not read the old engine's measurement.
		{"EngineVersion", func(d *BenchDeps) { d.EngineVersion = "0.32.15" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			tc.mutate(&d)
			if k := benchCacheKey(d); k == baseKey {
				t.Fatalf("changing %s did not change key (still %s)", tc.name, k)
			}
		})
	}
}

// An engine upgrade must not serve the previous engine's measurement
// (waired-agent#1131). This is the end-to-end form of the key test: the
// entry is really written, and the post-upgrade read really misses, so
// a future refactor that drops EngineVersion from the hash fails here
// and not only in the key unit test.
func TestBenchCache_EngineUpgradeMissesTheOldMeasurement(t *testing.T) {
	dir := t.TempDir()
	c := newBenchCache(dir+"/bench.json", testLogger())

	before := BenchDeps{
		EngineKind:    "ollama",
		EngineModel:   "qwen3.8:27b-mtp-q4_K_M",
		EngineVersion: "0.32.15",
		GPUModel:      "RTX PRO 4000 Blackwell",
		VRAMTotalMB:   24467,
		DriverVersion: "610.43.02",
		VariantSHA:    "abc123",
	}
	res := BenchResult{TokensPerSec: 131, Capacity: 4, Method: "ollama_eval"}
	if err := c.Store(benchCacheKey(before), res, benchCacheHumanMeta{
		GPUModel: before.GPUModel, EngineKind: before.EngineKind,
		EngineModel: before.EngineModel, EngineVersion: before.EngineVersion,
	}, time.Now()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, _, hit, err := c.Load(benchCacheKey(before)); err != nil || !hit {
		t.Fatalf("same engine version should hit: hit=%v err=%v", hit, err)
	}

	after := before
	after.EngineVersion = "0.33.2"
	if _, _, hit, _ := c.Load(benchCacheKey(after)); hit {
		t.Error("a newer engine read the previous engine's measurement")
	}

	// The version is recorded in the entry too, so an operator opening
	// bench.json can see which engine produced a figure.
	raw, err := os.ReadFile(dir + "/bench.json")
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var f benchCacheFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Version != benchCacheSchemaVersion {
		t.Errorf("schema version = %d, want %d", f.Version, benchCacheSchemaVersion)
	}
	e, ok := f.Entries[benchCacheKey(before)]
	if !ok {
		t.Fatal("stored entry missing from the file")
	}
	if e.EngineVersion != "0.32.15" {
		t.Errorf("entry engine_version = %q, want %q", e.EngineVersion, "0.32.15")
	}
}

func TestBenchCacheDisabledReason_NamesEveryMissingInput(t *testing.T) {
	cases := []struct {
		name                           string
		gpu, variantSHA, engineVersion string
		want                           string
	}{
		{"usable", "RTX 4090", "abc", "0.33.2", ""},
		{"no gpu", "", "abc", "0.33.2", "no GPU was detected on this host"},
		{"no variant sha", "RTX 4090", "", "0.33.2", "the active model is not in this build's catalog"},
		{"no engine version", "RTX 4090", "abc", "", "the engine version could not be read"},
		{"nothing at all", "", "", "",
			"no GPU was detected on this host; " +
				"the active model is not in this build's catalog; " +
				"the engine version could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := benchCacheDisabledReason(tc.gpu, tc.variantSHA, tc.engineVersion)
			if got != tc.want {
				t.Errorf("benchCacheDisabledReason = %q, want %q", got, tc.want)
			}
			// The reason and the key have to agree, or one of them is
			// describing a cache the other does not have.
			key := benchCacheKey(BenchDeps{
				GPUModel: tc.gpu, VariantSHA: tc.variantSHA,
				EngineVersion: tc.engineVersion, EngineKind: "ollama",
			})
			if (key == "") != (got != "") {
				t.Errorf("key=%q but reason=%q: the two disagree about whether caching is on", key, got)
			}
		})
	}
}
