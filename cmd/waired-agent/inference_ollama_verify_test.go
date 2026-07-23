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
	"github.com/waired-ai/waired-agent/internal/platform/proclist"
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

	t.Run("foreign-model-loaded-abstains", func(t *testing.T) {
		// waired#763: a DIFFERENT model is resident (the model-swap race,
		// where the previous model — here a 32768-window one — is still in
		// /api/ps). The probe must abstain, not compare the foreign runner's
		// context against this tuning and cry "OLLAMA_CONTEXT_LENGTH did not
		// apply". Even an f16-sized excess must not trigger a size verdict.
		f := &fakeOllamaAPI{psName: "someone-else:latest",
			psSize: weight + int64(65536)*262144, psVRAM: weight + int64(65536)*262144,
			psCtx: 32768, tagSize: weight}
		v, detail := run(f, tn, hw)
		if v != tuningInconclusive {
			t.Errorf("= (%v, %q), want tuningInconclusive for a foreign resident model", v, detail)
		}
		if strings.Contains(detail, "did not apply") {
			t.Errorf("must not emit the false OLLAMA_CONTEXT_LENGTH warning: %q", detail)
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
t.Run("gpu-not-engaged", func(t *testing.T) {
			f := &fakeOllamaAPI{psName: verifyTag, psSize: healthySize, psVRAM: 0,
				psCtx: 262144, tagSize: weight}
			v, detail := run(f, tn, hw)
			if v != tuningGpuNotEngaged {
				t.Errorf("= (%v, %q), want tuningGpuNotEngaged", v, detail)
			}
			// Also, we expect no context shrink, so the detail should indicate GPU not engaged
			if !strings.Contains(detail, "not using GPU") {
				t.Errorf("detail should indicate GPU not engaged: %q", detail)
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
			verifyTag, srv.URL, srv.Client(), nil, testLogger())
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
			verifyTag, srv.URL, srv.Client(), nil, testLogger())

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
			verifyTag, srv.URL, srv.Client(), nil, testLogger())
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
			verifyTag, srv.URL, srv.Client(), nil, testLogger())
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
			verifyTag, srv.URL, srv.Client(), nil, testLogger())
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
			verifyTag, srv.URL, srv.Client(), nil, testLogger())
		got := sw.lastTuning(t)
		if got.Verified {
			t.Errorf("inconclusive must record Verified=false: %+v", got)
		}
		if sw.stops != 0 {
			t.Errorf("inconclusive must never restart (stops=%d)", sw.stops)
		}
	})

	t.Run("records-runner-observed-parallelism", func(t *testing.T) {
		// waired#763 symptom 2: the tuning intended num_parallel=2 but Ollama
		// launched the runner with -np 1 (per-slot KV did not fit). The
		// recorded tuning must carry the runner's real 1, plus a note — not
		// the stale intent 2. A foreign runner still resident is ignored.
		tp := tn
		tp.NumParallel = 2
		size, vram := healthy(262144)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: vram,
			psCtx: 262144, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		procs := func() ([]proclist.ProcInfo, error) {
			return []proclist.ProcInfo{
				{PID: 10, Argv: []string{"llama-server", "-c", "32768", "-np", "2"}},
				{PID: 20, Argv: []string{"llama-server", "-c", "262144", "-np", "1"}},
			}, nil
		}
		sw := &fakeModelEnvSwitcher{}
		applyOllamaTuningVerification(context.Background(), sw, tp, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), procs, testLogger())
		got := sw.lastTuning(t)
		if got.ObservedNumParallel != 1 {
			t.Errorf("ObservedNumParallel = %d, want 1 (runner -np)", got.ObservedNumParallel)
		}
		if !strings.Contains(got.Warning, "reduced request parallelism from 2 to 1") {
			t.Errorf("want a reduced-parallelism note, got %q", got.Warning)
		}
	})
}

// TestObserveRunnerParallel exercises the #763 runner correlation directly:
// a unique llama-server / ollama-runner whose -c matches the tuning's
// context (or ctx × its own -np) wins; anything else abstains so status
// keeps the intent.
func TestObserveRunnerParallel(t *testing.T) {
	_, _, _, tn := verifyFixture() // tn.ContextLength == 262144
	mk := func(argvs ...[]string) runnerProcLister {
		return func() ([]proclist.ProcInfo, error) {
			out := make([]proclist.ProcInfo, len(argvs))
			for i, a := range argvs {
				out[i] = proclist.ProcInfo{PID: i + 1, Argv: a}
			}
			return out, nil
		}
	}
	t.Run("reduced-to-1-ignores-foreign", func(t *testing.T) {
		np, ok := observeRunnerParallel(tn, mk(
			[]string{"llama-server", "-c", "32768", "-np", "2"},  // foreign 32k runner
			[]string{"llama-server", "-c", "262144", "-np", "1"}, // target, reduced
		))
		if !ok || np != 1 {
			t.Errorf("= (%d, %v), want (1, true)", np, ok)
		}
	})
	t.Run("honored-ctx-times-parallel", func(t *testing.T) {
		// -c is the TOTAL context: an honored np=2 shows -c = 262144 × 2.
		np, ok := observeRunnerParallel(tn, mk(
			[]string{"ollama", "runner", "--ctx-size", "524288", "--parallel", "2"},
		))
		if !ok || np != 2 {
			t.Errorf("= (%d, %v), want (2, true)", np, ok)
		}
	})
	t.Run("no-runner-matches-abstains", func(t *testing.T) {
		np, ok := observeRunnerParallel(tn, mk(
			[]string{"llama-server", "-c", "99999", "-np", "1"},
			[]string{"/usr/bin/vim"},
		))
		if ok || np != 0 {
			t.Errorf("= (%d, %v), want (0, false)", np, ok)
		}
	})
	t.Run("ambiguous-two-matches-abstains", func(t *testing.T) {
		if np, ok := observeRunnerParallel(tn, mk(
			[]string{"llama-server", "-c", "262144", "-np", "1"},
			[]string{"llama-server", "-c", "262144", "-np", "3"},
		)); ok {
			t.Errorf("two matching runners must abstain, got np=%d", np)
		}
	})
	t.Run("nil-lister-abstains", func(t *testing.T) {
		if _, ok := observeRunnerParallel(tn, nil); ok {
			t.Error("nil lister must abstain")
		}
	})
}

// anchorSpillFixture mirrors the #625 anchor: mtp-class weights on the
// 24467 MiB card, where computeOllamaTuning takes the intentional-spill
// branch. Under the #765 speed cap (0.20, clamped to the selection
// bound) the full 200704 floor is served with expected spill ≈ 11.7% —
// per the #664 model that decodes ~85 tok/s, above the 60 tok/s floor.
// (At the pre-#765 0.075 cap the tuner instead trimmed to ~163k.)
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
	if tn.ExpectedSpillFraction <= 0 || tn.ContextLength != 200704 {
		t.Fatalf("fixture should serve the full floor as an intentional spill: %+v", tn.ModelTuning)
	}
	// Measured 13.5% in system RAM (the #625 shape) — under the
	// tolerance 2×expected ≈ 23.4% at the floor window.
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
	// 24.2% measured spill: over the base tolerance 2×expected (~23.4%)
	// but within it plus the 2 GiB generation-buffer allowance (~9.3% of
	// a 23.1 GB model, clamped at the 25% absolute tolerance max).
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: 23_100_000_000,
		psVRAM: 17_509_800_000, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
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
	// 30% measured spill > the 25% absolute tolerance clamp.
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
	applyOllamaTuningVerification(context.Background(), sw, tn, m, v, hw, "anchor:tag", srv.URL, srv.Client(), nil, testLogger())

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
