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
	res := BenchResult{TokensPerSec: 123.4, Capacity: 4, VariantID: "qwen3-8b-q4-gguf"}
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
		{"no GPU model", BenchDeps{VariantSHA: "abc", EngineKind: "ollama"}},
		{"no variant SHA", BenchDeps{GPUModel: "RTX 4090", EngineKind: "ollama"}},
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
