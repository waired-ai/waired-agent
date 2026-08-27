package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The figures in this file were measured on sv-mag (NVIDIA RTX PRO 4000
// Blackwell, 24467 MiB VRAM, 121 GB RAM) running ollama 0.32.13 with
// qwen3.8-27b mtp-q4 at the 200,704-token coding window, 2026-08-27
// (waired-agent#1038). A record of measured behaviour, not a contract
// with the engine — except where a case says otherwise.
//
//	config                 /api/ps size  size_vram  spill   free VRAM  ~26.7k prompt
//	-wb2048, ctx=200704    21.73 GB      15.54 GB   28.5 %    491 MiB  CUDA out of memory
//	plain tag, ctx=200704  20.07 GB      15.68 GB   21.9 %    945 MiB  OK, 744 tok/s prefill
//	-wb2048, ctx=65536     17.95 GB      17.95 GB    0   %   3141 MiB  OK, 999 tok/s
//	plain tag, ctx=65536   17.54 GB      17.54 GB    0   %   3969 MiB  OK, 978 tok/s
const (
	svMagForcedBatchSize    = 21_730_000_000
	svMagForcedBatchVRAM    = 15_540_000_000
	svMagForcedBatchFreeMB  = 491
	svMagAutoBatchSize      = 20_070_000_000
	svMagAutoBatchVRAM      = 15_680_000_000
	svMagAutoBatchFreeMB    = 945
	svMagSmallWindowFreeMB  = 3141
	svMagUnforcedWindowFree = 3969
)

func freeVRAM(mb int, ok bool) func(context.Context) (int, bool) {
	return func(context.Context) (int, bool) { return mb, ok }
}

// bundledManifest reads the shipped catalog rather than a hand-copied
// fixture, so an annotation change that invalidates a measurement in this
// file fails loudly.
func bundledManifest(t *testing.T, modelID string) (catalog.Manifest, bool) {
	t.Helper()
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range ms {
		if m.ModelID == modelID {
			return m, true
		}
	}
	return catalog.Manifest{}, false
}

func TestComputeOllamaTuning_ForcedBatchSuppressedAfterRefusal(t *testing.T) {
	m, v, hw, base := anchorSpillFixture()
	if base.NumBatch < ollamaLargeBatch {
		t.Fatalf("fixture must force the batch to be worth suppressing: %+v", base.ModelTuning)
	}

	refused := ollamaObservedServe{ModelID: m.ModelID, VariantID: v.VariantID, ForcedBatchRefused: true}
	got := computeOllamaTuning(m, v, hw, "q8_0", refused)

	if got.NumBatch != 0 {
		t.Errorf("NumBatch = %d, want 0 — the refusal must suppress the #642 override", got.NumBatch)
	}
	// The window is not collateral damage: dropping the batch is what
	// keeps the ~200k coding floor reachable on this host.
	if got.ContextLength != base.ContextLength {
		t.Errorf("ContextLength = %d, want the window kept at %d", got.ContextLength, base.ContextLength)
	}
	if got.ContextLength != hostfit.ServingWindow200k {
		t.Errorf("ContextLength = %d, want the coding floor %d", got.ContextLength, hostfit.ServingWindow200k)
	}
}

func TestComputeOllamaTuning_RefusalIsKeyedToModelAndVariant(t *testing.T) {
	m, v, hw, base := anchorSpillFixture()
	for _, other := range []ollamaObservedServe{
		{ModelID: "some-other-model", VariantID: v.VariantID, ForcedBatchRefused: true},
		{ModelID: m.ModelID, VariantID: "some-other-variant", ForcedBatchRefused: true},
		{ModelID: m.ModelID, VariantID: v.VariantID},
	} {
		if got := computeOllamaTuning(m, v, hw, "q8_0", other); got.NumBatch != base.NumBatch {
			t.Errorf("observed %+v suppressed the batch (%d), want %d — a refusal recorded "+
				"against other weights says nothing about these", other, got.NumBatch, base.NumBatch)
		}
	}
}

func TestOllamaObservedFromState(t *testing.T) {
	m, v, _, _ := anchorSpillFixture()
	t.Run("no record", func(t *testing.T) {
		st := catalog.State{Models: map[string]catalog.ModelState{m.ModelID: {VariantID: v.VariantID}}}
		if got := ollamaObservedFromState(st, m, v); got.ForcedBatchRefused {
			t.Error("an untouched state must not read as a refusal")
		}
	})
	t.Run("record present", func(t *testing.T) {
		st := catalog.State{Models: map[string]catalog.ModelState{
			m.ModelID: {VariantID: v.VariantID, ForcedBatchRefusedAt: time.Unix(1787836000, 0)},
		}}
		got := ollamaObservedFromState(st, m, v)
		if !got.ForcedBatchRefused || got.ModelID != m.ModelID || got.VariantID != v.VariantID {
			t.Errorf("= %+v, want a refusal keyed to (%s, %s)", got, m.ModelID, v.VariantID)
		}
	})
}

// TestVerifyOllamaTuning_HeadroomOutranksSpillFraction is the finding
// that shaped the fix: on the reproduction host the spill fraction does
// not separate the configuration that works from the one that cannot
// serve a prompt, and the free reading does.
func TestVerifyOllamaTuning_HeadroomOutranksSpillFraction(t *testing.T) {
	m, _, hw, tn := anchorSpillFixture()
	_ = m

	cases := []struct {
		name    string
		size    int64
		vram    int64
		freeMB  int
		want    tuningVerdict
		wantSub string
	}{
		{
			name: "28.5% spilled with 491 MB free is exhausted",
			size: svMagForcedBatchSize, vram: svMagForcedBatchVRAM, freeMB: svMagForcedBatchFreeMB,
			want: tuningVRAMExhausted, wantSub: "left free",
		},
		{
			name: "21.9% spilled with 945 MB free is a working configuration",
			size: svMagAutoBatchSize, vram: svMagAutoBatchVRAM, freeMB: svMagAutoBatchFreeMB,
			want: tuningOKPlannedSpill, wantSub: "planned bound",
		},
		{
			// Past the tolerance the pre-#1038 pass degraded on — and
			// still a host with room to serve. The fraction says degrade,
			// the reading says it works, and the reading wins.
			name: "30% spilled with 945 MB free is still a working configuration",
			size: 23_100_000_000, vram: 16_170_000_000, freeMB: svMagAutoBatchFreeMB,
			want: tuningOKPlannedSpill, wantSub: "still free",
		},
		{
			name: "fully resident with room to spare",
			size: svMagAutoBatchSize, vram: svMagAutoBatchSize, freeMB: svMagSmallWindowFreeMB,
			want: tuningOK,
		},
		{
			name: "fully resident, unforced window",
			size: svMagAutoBatchSize, vram: svMagAutoBatchSize, freeMB: svMagUnforcedWindowFree,
			want: tuningOK,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeOllamaAPI{psName: "anchor:tag", psSize: c.size, psVRAM: c.vram,
				psCtx: tn.ContextLength, tagSize: 22_620_000_000}
			srv := f.server(t)
			defer srv.Close()

			verdict, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL,
				tn, "anchor:tag", hw, ollamaVerifyDeps{FreeVRAMMB: freeVRAM(c.freeMB, true)})
			if verdict != c.want {
				t.Fatalf("verdict = %v (%s), want %v", verdict, detail, c.want)
			}
			if c.wantSub != "" && !strings.Contains(detail, c.wantSub) {
				t.Errorf("detail = %q, want it to mention %q", detail, c.wantSub)
			}
		})
	}
}

// TestVerifyOllamaTuning_FreeVRAMUnknownKeepsTodaysBehaviour covers every
// host the reading cannot reach — unified memory, AMD (rocm-smi reports
// no free column), a driver that rejected memory.free. "No evidence" must
// never read as "zero free".
func TestVerifyOllamaTuning_FreeVRAMUnknownKeepsTodaysBehaviour(t *testing.T) {
	m, _, hw, tn := anchorSpillFixture()
	_ = m
	// The 28.5 % configuration: without a reading this is the pre-#1038
	// tuningSpill, exactly as before.
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: svMagForcedBatchSize,
		psVRAM: svMagForcedBatchVRAM, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	for _, deps := range []ollamaVerifyDeps{
		{},                                 // nothing wired
		{FreeVRAMMB: freeVRAM(0, false)},   // the reading abstained
		{FreeVRAMMB: freeVRAM(491, false)}, // a figure with ok=false is still no evidence
	} {
		verdict, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL,
			tn, "anchor:tag", hw, deps)
		if verdict != tuningSpill {
			t.Errorf("verdict = %v (%s), want the pre-#1038 tuningSpill", verdict, detail)
		}
	}
}

func TestVerifyOllamaTuning_AllocationProbeOOMIsVRAMExhausted(t *testing.T) {
	m, _, hw, tn := anchorSpillFixture()
	_ = m
	// A load that looks fine by every static measure: fully resident, and
	// enough free VRAM to clear the floor. Only making the runner allocate
	// finds the cliff.
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: svMagAutoBatchSize,
		psVRAM: svMagAutoBatchSize, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	oom := errors.New(`engine returned 500: {"error":"an error was encountered while running ` +
		`the model: CUDA error\nCUDA error: out of memory"}`)
	var probedTag string
	var probedTokens int
	verdict, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL,
		tn, "anchor:tag", hw, ollamaVerifyDeps{
			FreeVRAMMB: freeVRAM(svMagAutoBatchFreeMB, true),
			Allocate: func(_ context.Context, tag string, tokens int) error {
				probedTag, probedTokens = tag, tokens
				return oom
			},
		})

	if verdict != tuningVRAMExhausted {
		t.Fatalf("verdict = %v (%s), want tuningVRAMExhausted", verdict, detail)
	}
	if !strings.Contains(detail, "out of memory") {
		t.Errorf("detail = %q, want the engine's own sentence", detail)
	}
	if probedTag != "anchor:tag" {
		t.Errorf("probed %q, want the loaded model", probedTag)
	}
	if probedTokens < 2*ollamaLargeBatch {
		t.Errorf("probed %d tokens, want several forced ubatches", probedTokens)
	}
}

// TestVerifyOllamaTuning_ProbeOutranksTheFreeVRAMFloor: the free reading
// is a proxy, and the proxy is engine-version-specific. On ollama
// 0.32.13 this host held 491 MB free and could not serve 2,000 tokens;
// on 0.32.15 the same model and window held 647 MB — below the floor —
// and served 26,692 tokens at 799 tok/s. A configuration the engine
// demonstrably serves must not be stepped down on a number.
func TestVerifyOllamaTuning_ProbeOutranksTheFreeVRAMFloor(t *testing.T) {
	m, _, hw, tn := anchorSpillFixture()
	_ = m
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: 25_640_000_000,
		psVRAM: 13_190_000_000, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	verdict, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL,
		tn, "anchor:tag", hw, ollamaVerifyDeps{
			FreeVRAMMB: freeVRAM(647, true), // below ollamaPostLoadFreeVRAMFloorMB
			Allocate:   func(context.Context, string, int) error { return nil },
		})
	if verdict == tuningVRAMExhausted || verdict == tuningSpill {
		t.Fatalf("verdict = %v (%s), want a working configuration — the probe served", verdict, detail)
	}
}

func TestVerifyOllamaTuning_AllocationProbeNonOOMErrorIsIgnored(t *testing.T) {
	m, _, hw, tn := anchorSpillFixture()
	_ = m
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: svMagAutoBatchSize,
		psVRAM: svMagAutoBatchSize, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	// A timeout says nothing about the fit, and turning it into a degrade
	// would step a working host down for a slow probe.
	verdict, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL,
		tn, "anchor:tag", hw, ollamaVerifyDeps{
			FreeVRAMMB: freeVRAM(svMagAutoBatchFreeMB, true),
			Allocate:   func(context.Context, string, int) error { return context.DeadlineExceeded },
		})
	if verdict == tuningVRAMExhausted {
		t.Fatalf("a probe timeout must not read as a fit failure (%s)", detail)
	}
}

func TestDegradeStep_IsStrictlyDescending(t *testing.T) {
	m, v, hw, tn := anchorSpillFixture()
	if tn.NumBatch < ollamaLargeBatch {
		t.Fatalf("fixture must start with the forced batch: %+v", tn.ModelTuning)
	}
	if got := len(hostfit.OllamaServedWindows(m)); got != 1 {
		t.Fatalf("fixture must be a one-rung model (that is the defect): %d rungs", got)
	}

	// Rung 1: the batch, and only the batch.
	next, warn, kind := degradeStep(tn, m, v, hw, tuningVRAMExhausted, "detail")
	if kind != stepTag {
		t.Fatalf("first step kind = %v, want stepTag (no engine restart)", kind)
	}
	if next.NumBatch != 0 {
		t.Errorf("NumBatch = %d, want 0", next.NumBatch)
	}
	if next.ContextLength != tn.ContextLength {
		t.Errorf("ContextLength = %d, want the window kept at %d", next.ContextLength, tn.ContextLength)
	}
	if !strings.Contains(warn, "batch") {
		t.Errorf("warning = %q, want it to name the batch", warn)
	}

	// Rung 2 on a one-rung model: the bottom.
	bottom, warn2, kind2 := degradeStep(next, m, v, hw, tuningVRAMExhausted, "detail")
	if kind2 != stepNone {
		t.Fatalf("second step kind = %v, want stepNone on a one-rung model", kind2)
	}
	if bottom.ContextLength != next.ContextLength || bottom.NumBatch != next.NumBatch {
		t.Errorf("stepNone changed the configuration: %+v → %+v", next.ModelTuning, bottom.ModelTuning)
	}
	if warn2 == "" {
		t.Error("the bottom of the ladder must still say what happened")
	}
}

// TestApplyOllamaTuningVerification_DropsForcedBatchBeforeTheWindow is
// the end-to-end shape of the fix on the reproduction host: the batch
// goes, the window stays, and the engine is never restarted.
func TestApplyOllamaTuningVerification_DropsForcedBatchBeforeTheWindow(t *testing.T) {
	m, v, hw, tn := anchorSpillFixture()

	// The engine's answer changes with the configuration: forced batch →
	// the 491 MB load; after the step → the 945 MB one that works.
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: svMagForcedBatchSize,
		psVRAM: svMagForcedBatchVRAM, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	free := svMagForcedBatchFreeMB
	applied := 0
	sw := &fakeModelEnvSwitcher{}
	applyOllamaTuningVerification(context.Background(), sw, tn, m, v, hw, "anchor:tag", srv.URL,
		srv.Client(), ollamaVerifyDeps{
			FreeVRAMMB: func(context.Context) (int, bool) { return free, true },
			ApplyStep: func(context.Context, ollamaTuning) (string, error) {
				applied++
				free = svMagAutoBatchFreeMB
				f.mu.Lock()
				f.psSize, f.psVRAM = svMagAutoBatchSize, svMagAutoBatchVRAM
				f.mu.Unlock()
				return "anchor:tag", nil
			},
		}, testLogger())

	if applied != 1 {
		t.Fatalf("ApplyStep called %d times, want exactly 1", applied)
	}
	if sw.stops != 0 || sw.ensures != 0 {
		t.Fatalf("restarts: stops=%d ensures=%d, want none — the batch rides the serving tag",
			sw.stops, sw.ensures)
	}
	got := sw.lastTuning(t)
	if got.ContextLength != hostfit.ServingWindow200k {
		t.Errorf("ContextLength = %d, want the coding floor kept", got.ContextLength)
	}
	if got.NumBatch != 0 {
		t.Errorf("NumBatch = %d, want the forced batch dropped", got.NumBatch)
	}
	if got.Degraded {
		t.Errorf("Degraded = true on a configuration that now works: %q", got.Warning)
	}
	if !got.WindowFits {
		t.Errorf("WindowFits = false, want the window still declared: %q", got.Warning)
	}
	if got.PostLoadFreeVRAMMB != svMagAutoBatchFreeMB {
		t.Errorf("PostLoadFreeVRAMMB = %d, want the post-step reading %d",
			got.PostLoadFreeVRAMMB, svMagAutoBatchFreeMB)
	}
}

// TestApplyOllamaTuningVerification_ProbesOncePerConfiguration: the
// allocation probe is a real multi-ubatch prefill — ~10 s on the
// reproduction host — so asking the same question of the same
// configuration twice is wasted boot time, not just a redundant call.
func TestApplyOllamaTuningVerification_ProbesOncePerConfiguration(t *testing.T) {
	m, v, hw, tn := anchorSpillFixture()
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: svMagAutoBatchSize,
		psVRAM: svMagAutoBatchSize, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	probes := 0
	sw := &fakeModelEnvSwitcher{}
	applyOllamaTuningVerification(context.Background(), sw, tn, m, v, hw, "anchor:tag", srv.URL,
		srv.Client(), ollamaVerifyDeps{
			FreeVRAMMB: freeVRAM(svMagSmallWindowFreeMB, true),
			Allocate: func(context.Context, string, int) error {
				probes++
				return nil
			},
			ApplyStep: func(context.Context, ollamaTuning) (string, error) {
				t.Error("a configuration that serves must not be stepped down")
				return "anchor:tag", nil
			},
		}, testLogger())

	if probes != 1 {
		t.Errorf("allocation probe ran %d times for one configuration, want 1", probes)
	}
}

// TestApplyOllamaTuningVerification_LadderTerminates: a host that never
// recovers must stop, not oscillate, and must never take the same
// configuration twice.
func TestApplyOllamaTuningVerification_LadderTerminates(t *testing.T) {
	m, v, hw, tn := anchorSpillFixture()
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: svMagForcedBatchSize,
		psVRAM: svMagForcedBatchVRAM, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	var seen []int // NumBatch of each configuration ApplyStep was asked for
	sw := &fakeModelEnvSwitcher{}
	applyOllamaTuningVerification(context.Background(), sw, tn, m, v, hw, "anchor:tag", srv.URL,
		srv.Client(), ollamaVerifyDeps{
			FreeVRAMMB: freeVRAM(svMagForcedBatchFreeMB, true), // never recovers
			ApplyStep: func(_ context.Context, next ollamaTuning) (string, error) {
				seen = append(seen, next.NumBatch)
				return "anchor:tag", nil
			},
		}, testLogger())

	if len(seen) > ollamaMaxTuningDegradeSteps {
		t.Fatalf("ApplyStep called %d times, want at most %d", len(seen), ollamaMaxTuningDegradeSteps)
	}
	for i, nb := range seen {
		for j := 0; j < i; j++ {
			if seen[j] == nb {
				t.Fatalf("configuration NumBatch=%d taken twice (steps %d and %d)", nb, j, i)
			}
		}
	}
	if sw.stops > 1 || sw.ensures > 1 {
		t.Errorf("restarts: stops=%d ensures=%d, want at most one of each (#621)", sw.stops, sw.ensures)
	}
	got := sw.lastTuning(t)
	if !got.Degraded {
		t.Errorf("Degraded = false at the bottom of the ladder: %q", got.Warning)
	}
	if got.WindowFits {
		t.Error("WindowFits should drop once the ladder is spent")
	}
	if got.Warning == "" {
		t.Error("the latch must record what happened")
	}
}

// TestSvMag_Qwen38_27b_ServesTheCodingWindow drives the whole chain the
// reproduction host walks, from the bundled manifest rather than a
// hand-copied fixture — so a catalog change that invalidates the
// measurement fails loudly rather than silently.
//
// PRODUCT CONTRACT for the window half: this host serves the ~200k
// coding floor (#624; waired-ai/waired#1056 decision 3). A record of
// measured behaviour for the memory figures (sv-mag, 2026-08-27, ollama
// 0.32.13, RTX PRO 4000 Blackwell 24467 MiB).
func TestSvMag_Qwen38_27b_ServesTheCodingWindow(t *testing.T) {
	m, ok := bundledManifest(t, "qwen3.8-27b")
	if !ok {
		t.Skip("qwen3.8-27b is no longer in the bundled catalog")
	}
	var v catalog.Variant
	for _, cand := range m.Variants {
		if cand.VariantID == "mtp-q4-gguf" {
			v = cand
			break
		}
	}
	if v.VariantID == "" {
		t.Skip("qwen3.8-27b/mtp-q4-gguf is no longer in the bundled catalog")
	}
	hw := hardware.Profile{
		RAMTotalGB: 121,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "NVIDIA RTX PRO 4000 Blackwell", VRAMTotalMB: 24467}},
	}

	// What the sizing asks for today.
	tn := computeOllamaTuning(m, v, hw, "q8_0", ollamaObservedServe{})
	if tn.ContextLength != hostfit.ServingWindow200k {
		t.Fatalf("ContextLength = %d, want the coding floor %d", tn.ContextLength, hostfit.ServingWindow200k)
	}
	if tn.NumBatch != ollamaLargeBatch {
		t.Fatalf("NumBatch = %d, want the #642 override to fire on this host", tn.NumBatch)
	}

	// What the engine did with it.
	f := &fakeOllamaAPI{psName: "qwen3.8:27b-mtp-q4_K_M-wb2048",
		psSize: svMagForcedBatchSize, psVRAM: svMagForcedBatchVRAM,
		psCtx: tn.ContextLength, tagSize: 17_741_872_171}
	srv := f.server(t)
	defer srv.Close()

	verdict, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL, tn,
		f.psName, hw, ollamaVerifyDeps{FreeVRAMMB: freeVRAM(svMagForcedBatchFreeMB, true)})
	if verdict != tuningVRAMExhausted {
		t.Fatalf("verdict = %v (%s), want tuningVRAMExhausted", verdict, detail)
	}

	// One step down the ladder, and it is the batch, not the window.
	next, _, kind := degradeStep(tn, m, v, hw, verdict, detail)
	if kind != stepTag {
		t.Fatalf("step kind = %v, want stepTag", kind)
	}
	if next.NumBatch != 0 || next.ContextLength != hostfit.ServingWindow200k {
		t.Fatalf("stepped to ctx=%d batch=%d, want ctx=%d batch=0",
			next.ContextLength, next.NumBatch, hostfit.ServingWindow200k)
	}

	// And the stepped configuration verifies as a working one.
	f.mu.Lock()
	f.psName, f.psSize, f.psVRAM = "qwen3.8:27b-mtp-q4_K_M", svMagAutoBatchSize, svMagAutoBatchVRAM
	f.mu.Unlock()
	verdict2, detail2 := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL, next,
		"qwen3.8:27b-mtp-q4_K_M", hw, ollamaVerifyDeps{FreeVRAMMB: freeVRAM(svMagAutoBatchFreeMB, true)})
	if verdict2 == tuningVRAMExhausted || verdict2 == tuningSpill {
		t.Fatalf("verdict after the step = %v (%s), want a working configuration", verdict2, detail2)
	}

	// The mesh still hears the coding window from this host.
	if !hostfit.OllamaDeclaresWindow(m, v, hw.HostFit(), hostfit.ServingWindow200k) {
		t.Error("this host must still declare the ~200k coding window (waired-ai/waired#1056 decision 3)")
	}
}
