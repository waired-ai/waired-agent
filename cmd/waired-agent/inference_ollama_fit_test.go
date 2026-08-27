package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The figures in this file were measured on sv-mag (NVIDIA RTX PRO 4000
// Blackwell, 24467 MiB VRAM, 121 GB RAM) serving qwen3.8-27b mtp-q4 at
// the 200,704-token coding window. A record of measured behaviour, not a
// contract with the engine — except where a case says otherwise.
//
// ollama 0.32.13, 2026-08-27 (waired-agent#1038):
//
//	config                 /api/ps size  size_vram  spill   free VRAM  ~26.7k prompt
//	-wb2048, ctx=200704    21.73 GB      15.54 GB   28.5 %    491 MiB  CUDA out of memory
//	plain tag, ctx=200704  20.07 GB      15.68 GB   21.9 %    945 MiB  OK, 744 tok/s prefill
//	-wb2048, ctx=65536     17.95 GB      17.95 GB    0   %   3141 MiB  OK, 999 tok/s
//	plain tag, ctx=65536   17.54 GB      17.54 GB    0   %   3969 MiB  OK, 978 tok/s
//
// ollama 0.32.15, 2026-08-28 (waired-agent#1079), after retiring the
// forced generation ubatch. Same host, same window, each depth a fresh
// load:
//
//	config                 /api/ps size  size_vram  free VRAM  ~2k    26k        171k
//	-b 512 -ub 512 (engine) 20.07 GB     15.68 GB     506 MiB  —      744 tok/s  395 tok/s
//	-b 2048 -ub 2048 (was)  21.73 GB     15.54 GB      52 MiB  OOM    OOM        OOM
//
// The second row is why waired#642 is retired: it is the configuration
// this agent used to force, and it cannot serve 2,000 tokens. The first
// is what the engine chooses for itself, and it serves 171,449.
//
// Note what the free readings do NOT do: 506 MiB serves a 171k prompt
// and 945 MiB served a 152k one, while 491 MiB served nothing. The
// number is recorded, and decides nothing.
const (
	svMagForcedBatchSize   = 21_730_000_000
	svMagForcedBatchVRAM   = 15_540_000_000
	svMagForcedBatchFreeMB = 491
	svMagAutoBatchSize     = 20_070_000_000
	svMagAutoBatchVRAM     = 15_680_000_000
	svMagAutoBatchFreeMB   = 506
	svMagSmallWindowFreeMB = 3141
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
	// plenty of free VRAM. Only making the runner allocate finds the
	// cliff — which is why the free reading decides nothing
	// (waired-agent#1079).
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
	if probedTokens < 4096 {
		t.Errorf("probed %d tokens, want a prompt spanning several ubatches", probedTokens)
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
	if got := len(hostfit.OllamaServedWindows(m)); got != 1 {
		t.Fatalf("fixture must be a one-rung model (that is the defect): %d rungs", got)
	}

	// A one-rung model IS the bottom. waired-agent#1038 was this exact
	// shape with a configuration that could not serve; the answer then
	// was a batch rung below the window, and the answer now is that the
	// configuration which needed one is no longer produced
	// (waired-agent#1079). The engine sizes its own batch and steps it
	// down itself.
	bottom, warn, kind := degradeStep(tn, m, v, hw, tuningVRAMExhausted, "detail")
	if kind != stepNone {
		t.Fatalf("step kind = %v, want stepNone on a one-rung model", kind)
	}
	if bottom.ContextLength != tn.ContextLength {
		t.Errorf("stepNone changed the window: %d → %d", tn.ContextLength, bottom.ContextLength)
	}
	if warn == "" {
		t.Error("the bottom of the ladder must still say what happened")
	}
}

// TestDegradeStep_MultiRungModelStepsTheWindow is the other half: where
// a rung exists, it is taken, and it only ever descends.
func TestDegradeStep_MultiRungModelStepsTheWindow(t *testing.T) {
	m, ok := bundledManifest(t, "qwen3.6-35b-a3b")
	if !ok {
		t.Skip("qwen3.6-35b-a3b is no longer in the bundled catalog")
	}
	rungs := hostfit.OllamaServedWindows(m)
	if len(rungs) < 2 {
		t.Skipf("fixture model has %d rung(s); this test needs a ladder", len(rungs))
	}
	var v catalog.Variant
	for _, cand := range m.Variants {
		if len(cand.RuntimeSupport) > 0 && cand.RuntimeSupport[0] == catalog.RuntimeOllama {
			v = cand
			break
		}
	}
	if v.VariantID == "" {
		t.Skip("no ollama variant in the bundled manifest")
	}
	hw := hardware.Profile{
		RAMTotalGB: 120,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	tn := computeOllamaTuning(m, v, hw, "q8_0", ollamaObservedServe{})
	if tn.ContextLength <= rungs[len(rungs)-1] {
		t.Skipf("sizing already sits at the lowest rung (%d)", tn.ContextLength)
	}

	next, warn, kind := degradeStep(tn, m, v, hw, tuningVRAMExhausted, "detail")
	if kind != stepEnv {
		t.Fatalf("step kind = %v, want stepEnv (the window lives in an OLLAMA_* var)", kind)
	}
	if next.ContextLength >= tn.ContextLength {
		t.Errorf("window %d → %d, want strictly smaller", tn.ContextLength, next.ContextLength)
	}
	if warn == "" {
		t.Error("a step must say what it did")
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
		}, testLogger())

	if probes != 1 {
		t.Errorf("allocation probe ran %d times for one configuration, want 1", probes)
	}
}

// TestApplyOllamaTuningVerification_LadderTerminates: a host that never
// recovers must stop, not oscillate — and on a one-rung model it must
// latch rather than restart the engine into the same configuration.
func TestApplyOllamaTuningVerification_LadderTerminates(t *testing.T) {
	m, v, hw, tn := anchorSpillFixture()
	f := &fakeOllamaAPI{psName: "anchor:tag", psSize: svMagForcedBatchSize,
		psVRAM: svMagForcedBatchVRAM, psCtx: tn.ContextLength, tagSize: 22_620_000_000}
	srv := f.server(t)
	defer srv.Close()

	oom := errors.New(`engine returned 500: {"error":"CUDA error: out of memory"}`)
	sw := &fakeModelEnvSwitcher{}
	applyOllamaTuningVerification(context.Background(), sw, tn, m, v, hw, "anchor:tag", srv.URL,
		srv.Client(), ollamaVerifyDeps{
			FreeVRAMMB: freeVRAM(svMagForcedBatchFreeMB, true),
			Allocate:   func(context.Context, string, int) error { return oom },
		}, testLogger())

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
// measured behaviour for the memory figures (sv-mag, ollama 0.32.15,
// 2026-08-28).
//
// This used to assert the opposite of its middle section: the sizing
// forced a generation ubatch, the load verified as unservable, and the
// ladder stepped the batch off. Retiring that override
// (waired-agent#1079) removes the configuration that failed, so the host
// now verifies clean on the first try — which is the outcome the whole
// ladder existed to reach.
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
	// VRAMFreeMB is the measured idle reading, not the total: 470 MiB of
	// this card is spoken for by the display and the driver before any
	// model loads, and the sizing budget reads the free figure. Omitting
	// it modelled a card with nothing else on it and predicted a 5.0 %
	// spill where the host predicts 10.7 % (captured with scripts/dev/
	// hwprobe on sv-mag itself, engine stopped, 2026-08-28).
	hw := hardware.Profile{
		RAMTotalGB: 121,
		GPUs: []hardware.GPU{{
			Vendor: "nvidia", Model: "NVIDIA RTX PRO 4000 Blackwell",
			VRAMTotalMB: 24467, VRAMFreeMB: 23997,
		}},
	}

	// What the sizing asks for: the coding window, and nothing about the
	// generation batch — that is the engine's to choose.
	tn := computeOllamaTuning(m, v, hw, "q8_0", ollamaObservedServe{})
	if tn.ContextLength != hostfit.ServingWindow200k {
		t.Fatalf("ContextLength = %d, want the coding floor %d", tn.ContextLength, hostfit.ServingWindow200k)
	}
	if len(tn.Env()) == 0 {
		t.Fatal("the tuning exported no engine env at all")
	}
	for _, kv := range tn.Env() {
		if strings.Contains(kv, "BATCH") {
			t.Errorf("the tuning still exports a batch var: %q", kv)
		}
	}

	// What the engine did with it: the plain tag, spilling as planned,
	// and a probe that shows it can allocate a real prompt's working set.
	f := &fakeOllamaAPI{psName: "qwen3.8:27b-mtp-q4_K_M",
		psSize: svMagAutoBatchSize, psVRAM: svMagAutoBatchVRAM,
		psCtx: tn.ContextLength, tagSize: 17_741_872_171}
	srv := f.server(t)
	defer srv.Close()

	probed := 0
	verdict, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL, tn,
		f.psName, hw, ollamaVerifyDeps{
			// 506 MiB — below the floor this file used to carry, on a
			// host that serves a 171,449-token prompt. The reading is
			// recorded; the probe is what answers.
			FreeVRAMMB: freeVRAM(svMagAutoBatchFreeMB, true),
			Allocate: func(context.Context, string, int) error {
				probed++
				return nil
			},
		})
	if probed != 1 {
		t.Errorf("allocation probe ran %d times, want 1", probed)
	}
	if verdict == tuningVRAMExhausted || verdict == tuningSpill {
		t.Fatalf("verdict = %v (%s), want a working configuration", verdict, detail)
	}

	// The mesh still hears the coding window from this host.
	if !hostfit.OllamaDeclaresWindow(m, v, hw.HostFit(), hostfit.ServingWindow200k) {
		t.Error("this host must still declare the ~200k coding window (waired-ai/waired#1056 decision 3)")
	}
}
