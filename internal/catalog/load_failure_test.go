package catalog

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// refHost and refShape are the reference host of waired-agent#1443 and the
// load that failed there: f16 KV at 131,072 tokens, projected 85,070 MiB.
var (
	refHost = LoadContext{
		EngineKind: "ollama", EngineVersion: "0.34.0",
		GPUModel: "AMD Radeon 8060S Graphics", DriverVersion: "32.0.21029.1001",
		VRAMTotalMB: 98304, RAMTotalMB: 130199,
	}
	refShape = LoadShape{ContextLength: 131072, KVCacheType: "f16", NumParallel: 1, Backend: "vulkan"}
)

// TestVariantLoadFailure_Blocks is the whole rule.
//
// PRODUCT CONTRACT (waired-agent#1453): a recorded failure must not block a
// SMALLER load. Stepping down to one is the recovery this record exists to
// cause, and a record that blocked it would latch the host into serving
// nothing at all.
func TestVariantLoadFailure_Blocks(t *testing.T) {
	rec := VariantLoadFailure{
		ModelID: "qwen3.5-122b-a10b", VariantID: "q5-k-s-gguf",
		Context: refHost, Shape: refShape,
		FailedAt: time.Date(2026, 9, 20, 4, 21, 26, 0, time.UTC),
	}
	shapeWith := func(f func(*LoadShape)) LoadShape { s := refShape; f(&s); return s }
	ctxWith := func(f func(*LoadContext)) LoadContext { c := refHost; f(&c); return c }

	for _, tc := range []struct {
		name  string
		ctx   LoadContext
		shape LoadShape
		want  bool
	}{
		{"the same build, the same computer, the same load", refHost, refShape, true},

		{"CONTRACT: a smaller window is allowed to try",
			refHost, shapeWith(func(s *LoadShape) { s.ContextLength = 98304 }), false},
		{"CONTRACT: a cheaper KV cache is allowed to try",
			refHost, shapeWith(func(s *LoadShape) { s.KVCacheType = "q4_0" }), false},
		{"CONTRACT: less parallelism is allowed to try",
			refHost, shapeWith(func(s *LoadShape) { s.NumParallel = 0 }), false},
		{"a different backend is a different load",
			refHost, shapeWith(func(s *LoadShape) { s.Backend = "rocm" }), false},

		{"a new engine build is a new computer as far as this goes",
			ctxWith(func(c *LoadContext) { c.EngineVersion = "0.35.0" }), refShape, false},
		{"a new driver likewise",
			ctxWith(func(c *LoadContext) { c.DriverVersion = "32.0.22000.1" }), refShape, false},
		{"a different GPU likewise",
			ctxWith(func(c *LoadContext) { c.GPUModel = "NVIDIA RTX PRO 4000" }), refShape, false},
		{"more RAM likewise",
			ctxWith(func(c *LoadContext) { c.RAMTotalMB = 260398 }), refShape, false},
		{"a different VRAM budget likewise",
			ctxWith(func(c *LoadContext) { c.VRAMTotalMB = 65536 }), refShape, false},
		{"a different engine likewise",
			ctxWith(func(c *LoadContext) { c.EngineKind = "vllm" }), refShape, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rec.Blocks(tc.ctx, tc.shape); got != tc.want {
				t.Errorf("Blocks() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStore_FailedLoadsRoundTrip keeps the record on the same footing as
// MeasuredVariants: it survives a restart, because a host that has already
// hurt itself learning something must not have to learn it again.
func TestStore_FailedLoadsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)
	want := VariantLoadFailure{
		ModelID: "qwen3.5-122b-a10b", VariantID: "q5-k-s-gguf",
		Reason:   "this computer ran out of memory putting the model in memory",
		Detail:   "llama-server process has terminated: exit status 0xe06d7363",
		Context:  refHost,
		Shape:    refShape,
		FailedAt: time.Date(2026, 9, 20, 4, 21, 26, 0, time.UTC).UTC(),
	}
	st := State{
		Version:     StateVersion,
		FailedLoads: map[string]VariantLoadFailure{"sha256:0fbaae8d": want},
	}
	if err := s.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	back, ok := got.FailedLoads["sha256:0fbaae8d"]
	if !ok {
		t.Fatalf("FailedLoads lost the record; got %+v", got.FailedLoads)
	}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("round trip changed the record:\n got %+v\nwant %+v", back, want)
	}
	// The whole point of persisting the context is that it is readable
	// afterwards, so a stale record can be explained rather than guessed at.
	if !back.Blocks(refHost, refShape) {
		t.Error("a record read back from disk no longer blocks the load it recorded")
	}
}
