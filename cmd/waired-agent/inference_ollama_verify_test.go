package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	t := computeOllamaTuning(m, m.Variants[0], hw, "q8_0", ollamaObservedServe{})
	return m, m.Variants[0], hw, t
}

const verifyTag = "verify-model:q4"

// verifyCtx is the window this fixture is SERVED at. Derived rather than
// written out: since #552 the sizing caps at the top rung of
// hostfit.OllamaServedWindows, so a 262144-native model is served at
// ServingWindow200k and the two numbers stopped being the same. Every
// psCtx below is "the engine is doing what it was told", which is a
// statement about the tuning, not about a constant.
var verifyCtx = func() int { _, _, _, t := verifyFixture(); return t.ContextLength }()

func TestVerifyOllamaTuning(t *testing.T) {
	_, _, hw, tn := verifyFixture()
	if tn.ContextLength != verifyCtx {
		t.Fatalf("fixture sizing drifted: ctx = %d, want %d", tn.ContextLength, verifyCtx)
	}
	weight := int64(10e9)
	// Healthy q8_0 excess: 0.5 × 65536 × the served window.
	healthySize := weight + int64(0.5*65536)*int64(verifyCtx)

	run := func(f *fakeOllamaAPI, tun ollamaTuning, hw hardware.Profile) (tuningVerdict, string) {
		srv := f.server(t)
		defer srv.Close()
		return verifyOllamaTuning(context.Background(), srv.Client(), srv.URL, tun, verifyTag, hw, ollamaVerifyDeps{})
	}

	t.Run("ok", func(t *testing.T) {
		f := &fakeOllamaAPI{psName: verifyTag, psSize: healthySize, psVRAM: healthySize,
			psCtx: verifyCtx, tagSize: weight}
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
			psCtx: 2 * verifyCtx, tagSize: weight}
		v, detail := run(f, tp, hw)
		if v != tuningOK || detail != "" {
			t.Errorf("= (%v, %q), want (tuningOK, \"\") for ctx×parallel", v, detail)
		}
	})

	t.Run("spill-discrete", func(t *testing.T) {
		f := &fakeOllamaAPI{psName: verifyTag, psSize: healthySize,
			psVRAM: healthySize * 8 / 10, psCtx: verifyCtx, tagSize: weight}
		v, detail := run(f, tn, hw)
		if v != tuningSpill {
			t.Errorf("= (%v, %q), want tuningSpill", v, detail)
		}
	})

	t.Run("spill-uma-ignored", func(t *testing.T) {
		uma := hardware.Profile{RAMTotalGB: 128, UnifiedMemory: true, UsableVRAMMB: 98304}
		f := &fakeOllamaAPI{psName: verifyTag, psSize: healthySize,
			psVRAM: healthySize * 8 / 10, psCtx: verifyCtx, tagSize: weight}
		v, _ := run(f, tn, uma)
		if v == tuningSpill {
			t.Error("UMA hosts must not classify size_vram<size as a spill")
		}
	})

	t.Run("f16-fallback", func(t *testing.T) {
		// Excess ≈ full-f16 KV: 65536 × the served window.
		f := &fakeOllamaAPI{psName: verifyTag,
			psSize: weight + int64(65536)*int64(verifyCtx), psVRAM: weight + int64(65536)*int64(verifyCtx),
			psCtx: verifyCtx, tagSize: weight}
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
			psSize: weight + int64(65536)*int64(verifyCtx), psVRAM: weight + int64(65536)*int64(verifyCtx),
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
			psSize: healthySize, psVRAM: healthySize, psCtx: verifyCtx, tagSize: weight}
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

	// engineLog is what EngineLogTail hands back, and tailAsks records
	// the byte bound it was asked for — a fake that dropped the argument
	// would make "the read is bounded" untestable.
	engineLog string
	tailAsks  []int
}

func (f *fakeModelEnvSwitcher) EngineLogTail(maxBytes int) string {
	f.tailAsks = append(f.tailAsks, maxBytes)
	if len(f.engineLog) > maxBytes {
		return f.engineLog[len(f.engineLog)-maxBytes:]
	}
	return f.engineLog
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
		size, vram := healthy(verifyCtx)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: vram,
			psCtx: verifyCtx, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		sw := &fakeModelEnvSwitcher{}
		applyOllamaTuningVerification(context.Background(), sw, tn, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{}, testLogger())
		got := sw.lastTuning(t)
		if !got.Verified || got.Warning != "" || got.ContextLength != verifyCtx {
			t.Errorf("recorded %+v, want verified clean at the served window", got)
		}
		if sw.stops != 0 || sw.ensures != 0 {
			t.Errorf("engine touched on OK verdict (stops=%d ensures=%d)", sw.stops, sw.ensures)
		}
	})

	t.Run("f16-fallback-restarts-once-with-f16-sizing", func(t *testing.T) {
		f16Size := weight + int64(65536)*int64(verifyCtx)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: f16Size, psVRAM: f16Size,
			psCtx: verifyCtx, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()

		wantNext := computeOllamaTuning(m, variant, hw, "f16", ollamaObservedServe{})
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
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{}, testLogger())

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

	t.Run("spill-at-the-only-rung-latches-without-restart", func(t *testing.T) {
		// The fixture's ladder has one rung, so a spill degrade has
		// nowhere to step (waired-agent#587): the failure latches — no
		// restart, the warning records it, and WindowFits drops to record
		// WHY the host is on that rung.
		//
		// WindowFits no longer stops the window being declared: the host
		// serves it, and spill costs decode speed rather than window size
		// (waired-ai/waired-agent#657). TestDeclaredContextWindow owns
		// that half; this one owns the latch.
		size, _ := healthy(262144)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: size * 7 / 10,
			psCtx: verifyCtx, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		sw := &fakeModelEnvSwitcher{}
		applyOllamaTuningVerification(context.Background(), sw, tn, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{}, testLogger())
		if sw.stops != 0 || sw.ensures != 0 {
			t.Fatalf("stops=%d ensures=%d, want no restart at the ladder's only rung", sw.stops, sw.ensures)
		}
		got := sw.lastTuning(t)
		if !strings.Contains(got.Warning, "minimum context window") {
			t.Errorf("warning should record the latched spill: %q", got.Warning)
		}
		if got.WindowFits {
			t.Error("a latched failure must drop WindowFits so the decision reason records the forced rung")
		}
	})

	t.Run("verification warning joins the sizing warning", func(t *testing.T) {
		// The warning the tuning arrives with is a different fact from the
		// one verification produces: what the sizing decided, versus what
		// the runner actually did. record() used to replace, so a host
		// serving under the coding-agent context floor showed only the
		// spill and never the floor (waired#1216's premise check). The
		// #763 parallelism branch in the same function already joins.
		latched := tn
		latched.Warning = "configured model is below the ~200k coding-agent context floor"
		size, _ := healthy(262144)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: size * 7 / 10,
			psCtx: verifyCtx, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		sw := &fakeModelEnvSwitcher{}
		applyOllamaTuningVerification(context.Background(), sw, latched, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{}, testLogger())
		got := sw.lastTuning(t)
		if !strings.Contains(got.Warning, "coding-agent context floor") {
			t.Errorf("the sizing warning was dropped: %q", got.Warning)
		}
		if !strings.Contains(got.Warning, "minimum context window") {
			t.Errorf("the verification warning did not land: %q", got.Warning)
		}
	})

	t.Run("spill-steps-down-one-rung-and-never-restarts-twice", func(t *testing.T) {
		// A 1M-native model served at the 1M rung: a spill degrade steps
		// down exactly one rung (to 200,704), restarts once, and when the
		// restarted engine still spills the failure latches instead of a
		// second restart.
		mm := m
		mm.ContextLength = 1048576
		v := variant
		v.KVBytesPerTokenFP16 = 20480
		big := computeOllamaTuning(mm, v, hw, "q8_0", ollamaObservedServe{})
		if big.ContextLength != 1048576 || !big.WindowFits {
			t.Fatalf("fixture should serve the 1M rung outright: %+v", big.ModelTuning)
		}
		size := weight + int64(0.5*20480)*int64(big.ContextLength)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: size * 7 / 10,
			psCtx: big.ContextLength, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		sw := &fakeModelEnvSwitcher{} // onEnsure absent: the spill persists
		applyOllamaTuningVerification(context.Background(), sw, big, mm, v, hw,
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{}, testLogger())
		if sw.stops != 1 || sw.ensures != 1 {
			t.Fatalf("stops=%d ensures=%d, want exactly one restart even when still degraded", sw.stops, sw.ensures)
		}
		got := sw.lastTuning(t)
		if got.ContextLength != 200704 {
			t.Errorf("ContextLength = %d, want the next rung down 200704", got.ContextLength)
		}
		if !strings.Contains(got.Warning, "still degraded") {
			t.Errorf("warning should record the persisting spill: %q", got.Warning)
		}
		if got.WindowFits {
			t.Error("still-degraded after the restart must drop WindowFits")
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
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{}, testLogger())
		if sw.stops != 0 || sw.ensures != 0 {
			t.Errorf("no restart should happen at the floor (stops=%d ensures=%d)", sw.stops, sw.ensures)
		}
		got := sw.lastTuning(t)
		if !strings.Contains(got.Warning, "minimum context window") {
			t.Errorf("warning should say the floor still spills: %q", got.Warning)
		}
	})

	t.Run("stop-error-keeps-engine-and-warns", func(t *testing.T) {
		f16Size := weight + int64(65536)*int64(verifyCtx)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: f16Size, psVRAM: f16Size,
			psCtx: verifyCtx, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		sw := &fakeModelEnvSwitcher{stopErr: errors.New("stop refused")}
		applyOllamaTuningVerification(context.Background(), sw, tn, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{}, testLogger())
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
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{}, testLogger())
		got := sw.lastTuning(t)
		if got.Verified {
			t.Errorf("inconclusive must record Verified=false: %+v", got)
		}
		if sw.stops != 0 {
			t.Errorf("inconclusive must never restart (stops=%d)", sw.stops)
		}
	})

	t.Run("records-runner-observed-parallelism", func(t *testing.T) {
		// waired#763 symptom 2: the tuning intended num_parallel=2 but
		// Ollama launched the runner with -np 1. The recorded tuning must
		// carry the runner's real 1, plus a note — not the stale intent
		// 2. A foreign runner still resident is ignored.
		//
		// No engine log is seeded, which is the case where the agent has
		// no reason to give and must therefore give none.
		tp := tn
		tp.NumParallel = 2
		size, vram := healthy(verifyCtx)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: vram,
			psCtx: verifyCtx, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		procs := func() ([]proclist.ProcInfo, error) {
			return []proclist.ProcInfo{
				{PID: 10, Argv: []string{"llama-server", "-c", "32768", "-np", "2"}},
				{PID: 20, Argv: []string{"llama-server", "-c", strconv.Itoa(verifyCtx), "-np", "1"}},
			}, nil
		}
		sw := &fakeModelEnvSwitcher{}
		applyOllamaTuningVerification(context.Background(), sw, tp, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{ListProcs: procs}, testLogger())
		got := sw.lastTuning(t)
		if got.ObservedNumParallel != 1 {
			t.Errorf("ObservedNumParallel = %d, want 1 (runner -np)", got.ObservedNumParallel)
		}
		if !strings.Contains(got.Warning, "reduced request parallelism from 2 to 1") {
			t.Errorf("want a reduced-parallelism note, got %q", got.Warning)
		}
		// No engine log was seeded here, so the note may not name a
		// cause. The first host this fired on was reduced for an
		// architecture limit while fully GPU-resident, and the note was
		// telling its operator the KV cache had not fit
		// (waired-ai/waired-agent#877).
		for _, claim := range []string{"KV", "did not fit", "-token window", "memory"} {
			if strings.Contains(got.Warning, claim) {
				t.Errorf("warning asserts a cause the agent never observed (%q): %q",
					claim, got.Warning)
			}
		}
		if len(sw.tailAsks) == 0 {
			t.Error("the engine log was never read; the note can only be silent about the cause")
		}
		for _, n := range sw.tailAsks {
			if n <= 0 || n > 1<<20 {
				t.Errorf("EngineLogTail asked for %d bytes, want a bound that is neither "+
					"nothing nor the whole file", n)
			}
		}
	})

	t.Run("quotes-the-engines-own-reason", func(t *testing.T) {
		// The same reduction, with the engine's log available. The note
		// must repeat what the engine said rather than the KV-capacity
		// story the agent used to infer — on the host this was measured
		// on, that story was false and this sentence is why
		// (waired-ai/waired-agent#877).
		const engineReason = "model architecture does not currently support parallel requests"
		tp := tn
		tp.NumParallel = 2
		size, vram := healthy(verifyCtx)
		api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: vram,
			psCtx: verifyCtx, tagSize: weight}
		srv := api.server(t)
		defer srv.Close()
		procs := func() ([]proclist.ProcInfo, error) {
			return []proclist.ProcInfo{
				{PID: 20, Argv: []string{"llama-server", "-c", strconv.Itoa(verifyCtx), "-np", "1"}},
			}, nil
		}
		sw := &fakeModelEnvSwitcher{engineLog: `time=2026-08-20T00:57:38.881+09:00 ` +
			`level=WARN source=sched.go:509 msg="` + engineReason + `" architecture=qwen35`}
		applyOllamaTuningVerification(context.Background(), sw, tp, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{ListProcs: procs}, testLogger())
		got := sw.lastTuning(t)
		if !strings.Contains(got.Warning, engineReason) {
			t.Errorf("warning does not carry the engine's own reason: %q", got.Warning)
		}
		if strings.Contains(got.Warning, "waired logs") {
			t.Errorf("warning still points at the log it already read: %q", got.Warning)
		}
	})
}

// TestObserveRunnerParallel exercises the #763 runner correlation directly:
// a unique llama-server / ollama-runner whose -c matches the tuning's
// context (or ctx × its own -np) wins; anything else abstains so status
// keeps the intent.
func TestObserveRunnerParallel(t *testing.T) {
	// Derived from the fixture rather than written out: the served window
	// is a product decision that has moved once already (#552 capped it
	// at the top rung of hostfit.OllamaServedWindows), and this test is
	// about matching a runner to the tuning, not about the number.
	_, _, _, tn := verifyFixture()
	ctx := strconv.Itoa(tn.ContextLength)
	ctxTimes2 := strconv.Itoa(tn.ContextLength * 2)
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
		f, ok := observeRunnerFlags(tn, mk(
			[]string{"llama-server", "-c", "32768", "-np", "2"},           // foreign 32k runner
			[]string{"llama-server", "-c", ctx, "-np", "1", "-b", "2048"}, // target, reduced
		))
		if !ok || f.NumParallel != 1 {
			t.Errorf("= (%d, %v), want (1, true)", f.NumParallel, ok)
		}
		// waired-agent#1127: the prompt batch comes off the same line.
		// The agent never asked for it, so this read is the only source.
		if f.BatchTokens != 2048 {
			t.Errorf("BatchTokens = %d, want 2048", f.BatchTokens)
		}
	})
	t.Run("runner-without-a-batch-flag-leaves-it-unknown", func(t *testing.T) {
		// The engine defaults it rather than printing it. 0 is "not
		// known", which the depth planner reads as its own default —
		// never as a batch of zero.
		f, ok := observeRunnerFlags(tn, mk(
			[]string{"llama-server", "-c", ctx, "-np", "1"},
		))
		if !ok || f.BatchTokens != 0 {
			t.Errorf("= (%d, %v), want (0, true)", f.BatchTokens, ok)
		}
	})
	t.Run("honored-ctx-times-parallel", func(t *testing.T) {
		// -c is the TOTAL context: an honored np=2 shows -c = ctx × 2.
		f, ok := observeRunnerFlags(tn, mk(
			[]string{"ollama", "runner", "--ctx-size", ctxTimes2, "--parallel", "2", "--batch-size", "4096"},
		))
		if !ok || f.NumParallel != 2 {
			t.Errorf("= (%d, %v), want (2, true)", f.NumParallel, ok)
		}
		if f.BatchTokens != 4096 {
			t.Errorf("BatchTokens = %d, want 4096 (long form)", f.BatchTokens)
		}
	})
	t.Run("no-runner-matches-abstains", func(t *testing.T) {
		f, ok := observeRunnerFlags(tn, mk(
			[]string{"llama-server", "-c", "99999", "-np", "1"},
			[]string{"/usr/bin/vim"},
		))
		if ok || f.NumParallel != 0 {
			t.Errorf("= (%d, %v), want (0, false)", f.NumParallel, ok)
		}
	})
	t.Run("ambiguous-two-matches-abstains", func(t *testing.T) {
		if f, ok := observeRunnerFlags(tn, mk(
			[]string{"llama-server", "-c", "262144", "-np", "1"},
			[]string{"llama-server", "-c", "262144", "-np", "3"},
		)); ok {
			t.Errorf("two matching runners must abstain, got np=%d", f.NumParallel)
		}
	})
	t.Run("nil-lister-abstains", func(t *testing.T) {
		if _, ok := observeRunnerFlags(tn, nil); ok {
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
	tn := computeOllamaTuning(m, m.Variants[0], hw, "q8_0", ollamaObservedServe{})
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

	verdict, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL, tn, "anchor:tag", hw, ollamaVerifyDeps{})
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

// #624/#587: a spill grossly over the planned bound (>2× expected) at
// the ladder's only rung LATCHES — the engine keeps serving the rung
// (there is no smaller window this product serves), the warning records
// the measured overshoot, and WindowFits drops so the window stops being
// declared. Before waired-agent#587 this fell back to a sub-rung
// no-spill window with one restart; that window no longer exists.
func TestApplyOllamaTuningVerification_PlannedSpillOverBound(t *testing.T) {
	m, v, hw, tn := anchorSpillFixture()
	// 30% measured spill > the 25% absolute tolerance clamp.
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: 23_100_000_000,
		psVRAM: 16_170_000_000, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	sw := &fakeModelEnvSwitcher{}
	applyOllamaTuningVerification(context.Background(), sw, tn, m, v, hw, "anchor:tag", srv.URL, srv.Client(), ollamaVerifyDeps{}, testLogger())

	if sw.stops != 0 || sw.ensures != 0 {
		t.Fatalf("restarts: stops=%d ensures=%d, want none — no smaller rung exists", sw.stops, sw.ensures)
	}
	applied := sw.lastTuning(t)
	if applied.ContextLength != 200704 {
		t.Errorf("ContextLength = %d, want the rung kept", applied.ContextLength)
	}
	if !strings.Contains(applied.Warning, "beyond the planned bound") {
		t.Errorf("warning should record the over-bound spill: %q", applied.Warning)
	}
	if applied.WindowFits {
		t.Error("an over-bound spill must drop WindowFits so the rung is no longer declared")
	}
}

// TestParallelReductionReason pins what the agent will and will not
// repeat from the engine's log when it reports a parallelism reduction.
//
// A record of today's behaviour against ollama 0.32.13's wording, not a
// contract with the engine: the point of quoting is that the agent does
// not have to know the set of reasons in advance
// (waired-ai/waired-agent#877).
func TestParallelReductionReason(t *testing.T) {
	const archLine = `time=2026-08-20T00:57:38.881+09:00 level=WARN source=sched.go:509 ` +
		`msg="model architecture does not currently support parallel requests" architecture=qwen35`
	const archReason = "model architecture does not currently support parallel requests"
	// The line that carries OLLAMA_NUM_PARALLEL. It is about parallelism
	// and it is not a reason, which is why the msg field decides.
	const configLine = `time=2026-08-20T00:56:18.825+09:00 level=INFO source=routes.go:1933 ` +
		`msg="server config" env="map[OLLAMA_NUM_PARALLEL:2 OLLAMA_KV_CACHE_TYPE:q8_0]"`
	const ginLine = `[GIN] 2026/08/20 - 00:58:04 | 200 | 1.774ms | 127.0.0.1 | GET "/api/tags"`

	cases := []struct {
		name string
		tail string
		want string
		ok   bool
	}{
		{"the measured line", archLine, archReason, true},
		{
			"still found behind the access-log noise that follows a load",
			strings.Join([]string{archLine, ginLine, ginLine, ginLine}, "\n"),
			archReason, true,
		},
		{
			"the last one wins: a later load's answer is the current one",
			strings.Join([]string{
				strings.Replace(archLine, "qwen35", "older", 1),
				ginLine,
				archLine,
			}, "\n"),
			archReason, true,
		},
		{
			"the exported OLLAMA_NUM_PARALLEL is not a reason",
			strings.Join([]string{configLine, ginLine}, "\n"),
			"", false,
		},
		{"nothing about parallelism at all", strings.Join([]string{ginLine, ginLine}, "\n"), "", false},
		{"empty tail", "", "", false},
		{
			"a control character in the engine's own text is dropped",
			"msg=\"one parallel\x07 slot\"",
			"one parallel slot", true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parallelReductionReason(c.tail)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (got %q)", ok, c.ok, got)
			}
			if ok && got != c.want {
				t.Errorf("reason = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("an overlong sentence is bounded", func(t *testing.T) {
		got, ok := parallelReductionReason(`msg="` + strings.Repeat("parallel ", 100) + `"`)
		if !ok {
			t.Fatal("want a reason")
		}
		if len(got) > engineReasonMaxChars+len("\u2026") {
			t.Errorf("reason is %d bytes, want it bounded to %d", len(got), engineReasonMaxChars)
		}
	})
}

// PRODUCT CONTRACT (waired-agent#1043): a warning the sizing already
// carries is shown once, however many times the verification runs.
//
// record joins the verification's warning onto the tuning's, deliberately —
// the two are different facts and #587/#624 needs both. Three call sites
// passed the tuning's OWN warning in as the verification's, which joined it
// to itself. The verification runs from the boot spawn AND from the
// in-process model switch (#812), and nothing resets Warning between them,
// so the copies accumulate. Observed on real hardware as
// "context window set to 200704 tokens …; context window set to 200704
// tokens …; serving a 200704-token window …" in `waired status` and the
// tray.
func TestApplyOllamaTuningVerification_DoesNotRepeatTheSizingWarning(t *testing.T) {
	m, variant, hw, tn := verifyFixture()
	weight := int64(10e9)
	const sizing = "context window set to 200704 tokens for coding-agent workloads"
	tn.Warning = sizing

	size := weight + int64(0.5*65536*float64(verifyCtx))
	api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: size,
		psCtx: verifyCtx, tagSize: weight}
	srv := api.server(t)
	defer srv.Close()

	sw := &fakeModelEnvSwitcher{}
	// Twice, because once is not the failing case: the boot spawn and the
	// in-process switch both reach here on a host that changes model.
	for range 2 {
		applyOllamaTuningVerification(context.Background(), sw, tn, m, variant, hw,
			verifyTag, srv.URL, srv.Client(), ollamaVerifyDeps{}, testLogger())
	}
	got := sw.lastTuning(t)
	if n := strings.Count(got.Warning, sizing); n != 1 {
		t.Errorf("the sizing warning appears %d times, want 1:\n%s", n, got.Warning)
	}
}
