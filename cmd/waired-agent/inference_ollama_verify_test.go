package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// fakeOllamaAPI is a mutable /api/ps + /api/tags + /api/generate stub.
type fakeOllamaAPI struct {
	mu sync.Mutex

	psName    string
	psSize    int64
	psVRAM    int64
	psCtx     int
	psEmpty   bool
	tagSize   int64
	genStatus int // 0 = 200
}

func (f *fakeOllamaAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/ps":
			if f.psEmpty {
				_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{
				"name": f.psName, "size": f.psSize, "size_vram": f.psVRAM,
				"context_length": f.psCtx,
			}}})
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{
				"name": f.psName, "size": f.tagSize,
			}}})
		case "/api/generate":
			if f.genStatus != 0 {
				w.WriteHeader(f.genStatus)
				return
			}
			f.psEmpty = false
			_ = json.NewEncoder(w).Encode(map[string]any{"done": true})
		default:
			http.NotFound(w, r)
		}
	}))
}

// verifyFixture builds the shared manifest/variant/host/tuning for the
// verify tests: 24 GB discrete host, 10 GB weights, 65536 B/tok KV —
// q8_0 sizing caps at the 262144-token manifest window.
func verifyFixture() (catalog.Manifest, catalog.Variant, hardware.Profile, ollamaTuning) {
	m := catalog.Manifest{
		ModelID:       "verify-model",
		ContextLength: 262144,
		Variants: []catalog.Variant{{
			VariantID:           "q4",
			RuntimeSupport:      []string{catalog.RuntimeOllama},
			EstimatedWeightGB:   10.0,
			KVBytesPerTokenFP16: 65536,
		}},
	}
	hw := discrete24GB()
	t := computeOllamaTuning(m, m.Variants[0], hw, "q8_0")
	return m, m.Variants[0], hw, t
}

const verifyTag = "verify-model:q4"

func TestVerifyOllamaTuning(t *testing.T) {
	_, _, hw, tn := verifyFixture()
	if tn.ContextLength != 262144 {
		t.Fatalf("fixture sizing drifted: ctx = %d, want 262144", tn.ContextLength)
	}
	weight := int64(10e9)
	// Healthy q8_0 excess: ~0.5 × 65536 × 262144 ≈ 8.6 GB.
	healthySize := weight + int64(0.5*65536*262144)

	run := func(f *fakeOllamaAPI, tun ollamaTuning, hw hardware.Profile) (tuningVerdict, string) {
		srv := f.server(t)
		defer srv.Close()
		return verifyOllamaTuning(context.Background(), srv.Client(), srv.URL, tun, verifyTag, hw)
	}

	t.Run("ok", func(t *testing.T) {
		f := &fakeOllamaAPI{psName: verifyTag, psSize: healthySize, psVRAM: healthySize,
			psCtx: 262144, tagSize: weight}
		v, detail := run(f, tn, hw)
		if v != tuningOK || detail != "" {
			t.Errorf("= (%v, %q), want (tuningOK, \"\")", v, detail)
		}
	})

	t.Run("ctx-not-applied", func(t *testing.T) {
		f := &fakeOllamaAPI{psName: verifyTag, psSize: healthySize, psVRAM: healthySize,
			psCtx: 32768, tagSize: weight}
		v, detail := run(f, tn, hw)
		if v != tuningOK || !strings.Contains(detail, "did not apply") {
			t.Errorf("= (%v, %q), want ctx-mismatch detail", v, detail)
		}
	})

	t.Run("ctx-times-parallel-accepted", func(t *testing.T) {
		tp := tn
		tp.NumParallel = 2
		f := &fakeOllamaAPI{psName: verifyTag, psSize: healthySize, psVRAM: healthySize,
			psCtx: 2 * 262144, tagSize: weight}
		v, detail := run(f, tp, hw)
		if v != tuningOK || detail != "" {
			t.Errorf("= (%v, %q), want (tuningOK, \"\") for ctx×parallel", v, detail)
		}
	})

	t.Run("spill-discrete", func(t *testing.T) {
		f := &fakeOllamaAPI{psName: verifyTag, psSize: healthySize,
			psVRAM: healthySize * 8 / 10, psCtx: 262144, tagSize: weight}
		v, detail := run(f, tn, hw)
		if v != tuningSpill {
			t.Errorf("= (%v, %q), want tuningSpill", v, detail)
		}
	})

	t.Run("spill-uma-ignored", func(t *testing.T) {
		uma := hardware.Profile{RAMTotalGB: 128, UnifiedMemory: true, UsableVRAMMB: 98304}
		f := &fakeOllamaAPI{psName: verifyTag, psSize: healthySize,
			psVRAM: healthySize * 8 / 10, psCtx: 262144, tagSize: weight}
		v, _ := run(f, tn, uma)
		if v == tuningSpill {
			t.Error("UMA hosts must not classify size_vram<size as a spill")
		}
	})

	t.Run("f16-fallback", func(t *testing.T) {
		// Excess ≈ full-f16 KV: 65536 × 262144 ≈ 17.2 GB.
		f := &fakeOllamaAPI{psName: verifyTag,
			psSize: weight + int64(65536)*262144, psVRAM: weight + int64(65536)*262144,
			psCtx: 262144, tagSize: weight}
		v, detail := run(f, tn, hw)
		if v != tuningF16Fallback {
			t.Errorf("= (%v, %q), want tuningF16Fallback", v, detail)
		}
	})

	t.Run("other-model-loaded-abstains-from-sizes", func(t *testing.T) {
		// A different model is resident: ctx check still applies, but the
		// f16 heuristic must abstain even with an f16-sized excess.
		f := &fakeOllamaAPI{psName: "someone-else:latest",
			psSize: weight + int64(65536)*262144, psVRAM: weight + int64(65536)*262144,
			psCtx: 262144, tagSize: weight}
		v, detail := run(f, tn, hw)
		if v != tuningOK || detail != "" {
			t.Errorf("= (%v, %q), want (tuningOK, \"\")", v, detail)
		}
	})

	t.Run("inconclusive-when-load-fails", func(t *testing.T) {
		f := &fakeOllamaAPI{psEmpty: true, genStatus: 500}
		v, _ := run(f, tn, hw)
		if v != tuningInconclusive {
			t.Errorf("= %v, want tuningInconclusive", v)
		}
	})

	t.Run("loads-idle-model-then-verifies", func(t *testing.T) {
		f := &fakeOllamaAPI{psEmpty: true, psName: verifyTag,
			psSize: healthySize, psVRAM: healthySize, psCtx: 262144, tagSize: weight}
		v, detail := run(f, tn, hw)
		if v != tuningOK || detail != "" {
			t.Errorf("= (%v, %q), want OK after forced load", v, detail)
		}
	})
}

type fakeModelEnvSwitcher struct {
	envs     [][]string
	tunings  []infruntime.ModelTuning
	stops    int
	ensures  int
	onEnsure func()
	stopErr  error
}

func (f *fakeModelEnvSwitcher) SetModelEnv(env []string) {
	f.envs = append(f.envs, append([]string(nil), env...))
}
func (f *fakeModelEnvSwitcher) SetAppliedTuning(t infruntime.ModelTuning) {
	f.tunings = append(f.tunings, t)
}
func (f *fakeModelEnvSwitcher) Stop(context.Context) error { f.stops++; return f.stopErr }
func (f *fakeModelEnvSwitcher) EnsureRunning(context.Context) error {
	f.ensures++
	if f.onEnsure != nil {
		f.onEnsure()
	}
	return nil
}

func (f *fakeModelEnvSwitcher) lastTuning(t *testing.T) infruntime.ModelTuning {
	t.Helper()
	if len(f.tunings) == 0 {
		t.Fatal("SetAppliedTuning never called")
	}
	return f.tunings[len(f.tunings)-1]
}

func TestApplyOllamaTuningVerification(t *testing.T) {
	m, variant, hw, tn := verifyFixture()
	weight := int64(10e9)
	healthy := func(ctx int) (int64, int64) {
		s := weight + int64(0.5*65536*float64(ctx))
		return s, s
	}

	t.Run("ok-records-verified", func(t *testing.T) {
		size, vram := healthy(262144)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: vram,
			psCtx: 262144, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		sw := &fakeModelEnvSwitcher{}
		applyOllamaTuningVerification(context.Background(), sw, tn, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), testLogger())
		got := sw.lastTuning(t)
		if !got.Verified || got.Warning != "" || got.ContextLength != 262144 {
			t.Errorf("recorded %+v, want verified clean 262144", got)
		}
		if sw.stops != 0 || sw.ensures != 0 {
			t.Errorf("engine touched on OK verdict (stops=%d ensures=%d)", sw.stops, sw.ensures)
		}
	})

	t.Run("f16-fallback-restarts-once-with-f16-sizing", func(t *testing.T) {
		f16Size := weight + int64(65536)*262144
		api := &fakeOllamaAPI{psName: verifyTag, psSize: f16Size, psVRAM: f16Size,
			psCtx: 262144, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()

		wantNext := computeOllamaTuning(m, variant, hw, "f16")
		sw := &fakeModelEnvSwitcher{}
		sw.onEnsure = func() {
			// The restarted engine serves the recomputed window at a
			// healthy f16 size.
			api.mu.Lock()
			api.psCtx = wantNext.ContextLength
			s := weight + int64(65536)*int64(wantNext.ContextLength)
			api.psSize, api.psVRAM = s, s
			api.mu.Unlock()
		}
		applyOllamaTuningVerification(context.Background(), sw, tn, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), testLogger())

		if sw.stops != 1 || sw.ensures != 1 {
			t.Fatalf("stops=%d ensures=%d, want exactly one restart", sw.stops, sw.ensures)
		}
		if len(sw.envs) != 1 {
			t.Fatalf("SetModelEnv called %d times, want 1", len(sw.envs))
		}
		wantEnv := fmt.Sprintf("OLLAMA_CONTEXT_LENGTH=%d", wantNext.ContextLength)
		found := false
		for _, kv := range sw.envs[0] {
			if kv == wantEnv {
				found = true
			}
		}
		if !found {
			t.Errorf("recomputed env missing %q: %v", wantEnv, sw.envs[0])
		}
		got := sw.lastTuning(t)
		if !got.Verified || got.KVCacheType != "f16" || got.ContextLength != wantNext.ContextLength {
			t.Errorf("recorded %+v, want verified f16 @ %d", got, wantNext.ContextLength)
		}
		if !strings.Contains(got.Warning, "f16") {
			t.Errorf("warning should explain the f16 fallback: %q", got.Warning)
		}
	})

	t.Run("still-degraded-never-restarts-twice", func(t *testing.T) {
		size, _ := healthy(262144)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: size * 7 / 10,
			psCtx: 262144, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		sw := &fakeModelEnvSwitcher{} // onEnsure absent: the spill persists
		applyOllamaTuningVerification(context.Background(), sw, tn, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), testLogger())
		if sw.stops != 1 || sw.ensures != 1 {
			t.Fatalf("stops=%d ensures=%d, want exactly one restart even when still degraded", sw.stops, sw.ensures)
		}
		got := sw.lastTuning(t)
		if !strings.Contains(got.Warning, "still degraded") {
			t.Errorf("warning should record the persisting spill: %q", got.Warning)
		}
	})

	t.Run("spill-at-floor-warns-without-restart", func(t *testing.T) {
		floored := tn
		floored.ContextLength = ollamaContextFloor
		size, _ := healthy(ollamaContextFloor)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: size / 2,
			psCtx: ollamaContextFloor, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		sw := &fakeModelEnvSwitcher{}
		applyOllamaTuningVerification(context.Background(), sw, floored, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), testLogger())
		if sw.stops != 0 || sw.ensures != 0 {
			t.Errorf("no restart should happen at the floor (stops=%d ensures=%d)", sw.stops, sw.ensures)
		}
		got := sw.lastTuning(t)
		if !strings.Contains(got.Warning, "minimum context window") {
			t.Errorf("warning should say the floor still spills: %q", got.Warning)
		}
	})

	t.Run("stop-error-keeps-engine-and-warns", func(t *testing.T) {
		f16Size := weight + int64(65536)*262144
		api := &fakeOllamaAPI{psName: verifyTag, psSize: f16Size, psVRAM: f16Size,
			psCtx: 262144, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		sw := &fakeModelEnvSwitcher{stopErr: errors.New("stop refused")}
		applyOllamaTuningVerification(context.Background(), sw, tn, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), testLogger())
		if sw.ensures != 0 {
			t.Errorf("EnsureRunning must not run after a failed Stop (ensures=%d)", sw.ensures)
		}
		got := sw.lastTuning(t)
		if !got.Verified || got.Warning == "" {
			t.Errorf("failed restart should still record a verified warning: %+v", got)
		}
	})

	t.Run("inconclusive-records-unverified", func(t *testing.T) {
		api := &fakeOllamaAPI{psEmpty: true, genStatus: 500}
		srv := api.server(t)
		defer srv.Close()
		sw := &fakeModelEnvSwitcher{}
		applyOllamaTuningVerification(context.Background(), sw, tn, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), testLogger())
		got := sw.lastTuning(t)
		if got.Verified {
			t.Errorf("inconclusive must record Verified=false: %+v", got)
		}
		if sw.stops != 0 {
			t.Errorf("inconclusive must never restart (stops=%d)", sw.stops)
		}
	})
}

// anchorSpillFixture mirrors the #625 anchor: mtp-class weights on the
// 24467 MiB card, where computeOllamaTuning takes the intentional-spill
// branch. Under the #670 speed cap the served window is the largest one
// holding OllamaIntentionalSpillCapExpected (~163k, expected ≈ 7.4%),
// not the full 200704 floor (whose ≈11.5% expected spill would drop
// decode below the selection floor per the #664 measurement).
func anchorSpillFixture() (catalog.Manifest, catalog.Variant, hardware.Profile, ollamaTuning) {
	m := catalog.Manifest{
		ModelID:       "anchor-moe",
		ContextLength: 262144,
		Variants: []catalog.Variant{{
			VariantID:           "mtp-q4",
			RuntimeSupport:      []string{catalog.RuntimeOllama},
			EstimatedWeightGB:   22.62,
			KVBytesPerTokenFP16: 20480,
		}},
	}
	hw := hardware.Profile{
		RAMTotalGB: 120,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	tn := computeOllamaTuning(m, m.Variants[0], hw, "q8_0")
	return m, m.Variants[0], hw, tn
}

// #624: a measured spill inside the planned bound verifies as a working
// configuration — informational verdict, no degrade.
func TestVerifyOllamaTuning_PlannedSpillWithinBound(t *testing.T) {
	m, _, hw, tn := anchorSpillFixture()
	_ = m
	if tn.ExpectedSpillFraction <= 0 || tn.ContextLength >= 200704 || tn.ContextLength <= ollamaContextFloor {
		t.Fatalf("fixture should take the speed-capped intentional-spill branch: %+v", tn.ModelTuning)
	}
	// Measured 13.5% in system RAM (the #625 shape) — under the
	// tolerance 2×expected ≈ 14.8% at the capped window.
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: 23_100_000_000,
		psVRAM: 19_981_500_000, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	verdict, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL, tn, "anchor:tag", hw)
	if verdict != tuningOKPlannedSpill {
		t.Fatalf("verdict = %v (%s), want tuningOKPlannedSpill", verdict, detail)
	}
	if !strings.Contains(detail, "within the planned bound") {
		t.Errorf("detail should read informationally: %q", detail)
	}
	for _, bad := range []string{"fail", "error", "degraded"} {
		if strings.Contains(strings.ToLower(detail), bad) {
			t.Errorf("planned-spill detail must not read as an error (%q): %q", bad, detail)
		}
	}
}

// #642: the forced generation ubatch (num_batch=2048) adds a compute
// buffer that pushes weights to RAM, so the verify pass widens its spill
// tolerance by that buffer. A spill that would be an over-bound failure
// without the batch is a planned spill with it.
func TestVerifyOllamaTuning_LargeBatchWidensSpillTolerance(t *testing.T) {
	m, _, hw, tn := anchorSpillFixture()
	_ = m
	if tn.ExpectedSpillFraction <= 0 {
		t.Fatalf("fixture should be an intentional-spill config: %+v", tn.ModelTuning)
	}
	// 18.7% measured spill (the #642 reference-host measurement): over the
	// base tolerance 2×expected (~14.8%) but within it plus the 2 GiB
	// generation-buffer allowance (~9.3% of a 23.1 GB model).
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: 23_100_000_000,
		psVRAM: 18_780_000_000, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	// Baseline (automatic batch): the same spill is an over-bound failure.
	base := tn
	base.NumBatch = 0
	if v, _ := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL, base, "anchor:tag", hw); v != tuningSpill {
		t.Fatalf("baseline verdict = %v, want tuningSpill (proves the batch allowance is what saves it)", v)
	}
	// With num_batch=2048 the buffer is expected, so it passes.
	big := tn
	big.NumBatch = ollamaLargeBatch
	if v, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL, big, "anchor:tag", hw); v != tuningOKPlannedSpill {
		t.Fatalf("with forced batch verdict = %v (%s), want tuningOKPlannedSpill", v, detail)
	}
}

// #624: a spill grossly over the planned bound (>2× expected) falls
// back to the no-spill sizing with exactly one restart.
func TestApplyOllamaTuningVerification_PlannedSpillOverBound(t *testing.T) {
	m, v, hw, tn := anchorSpillFixture()
	// 30% measured spill > tolerance 2×expected ≈ 14.8%.
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: 23_100_000_000,
		psVRAM: 16_170_000_000, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	sw := &fakeModelEnvSwitcher{}
	// After the restart the fake reports a fully-resident model at the
	// shrunken window so the re-verify passes.
	sw.onEnsure = func() {
		f.mu.Lock()
		f.psVRAM = f.psSize
		f.psCtx = 0
		f.mu.Unlock()
	}
	applyOllamaTuningVerification(context.Background(), sw, tn, m, v, hw, "anchor:tag", srv.URL, srv.Client(), testLogger())

	if sw.stops != 1 || sw.ensures != 1 {
		t.Fatalf("restarts: stops=%d ensures=%d, want exactly 1 each", sw.stops, sw.ensures)
	}
	applied := sw.lastTuning(t)
	if applied.ContextLength >= 200704 {
		t.Errorf("degraded ContextLength = %d, want the no-spill window (< floor)", applied.ContextLength)
	}
	if !strings.Contains(applied.Warning, "exceeded the planned bound") {
		t.Errorf("warning should record the over-bound spill: %q", applied.Warning)
	}
}
