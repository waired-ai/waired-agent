package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// bootBenchLoopFixture is a provider whose EngineReady can be driven, in
// front of an engine that answers the benchmark protocol and counts what
// it was asked.
//
// The seam is the pair the production wiring already has: the provider
// decides whether to try, RunBootBenchmark does the measuring. Nothing
// replaces RunBootBenchmark itself — a fake in its place would make the
// engine-start race, which is the whole subject, unwritable.
type bootBenchLoopFixture struct {
	p        *agentInferenceProvider
	requests *atomic.Int64
	port     int
	client   *http.Client
	log      *bytes.Buffer
}

func newBootBenchLoopFixture(t *testing.T) *bootBenchLoopFixture {
	t.Helper()
	var requests atomic.Int64
	engine := &fakeOllamaEngine{evalCount: 200, evalDurationsNS: []int64{1_000_000_000}}
	inner := engine.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		inner(w, r)
	}))
	t.Cleanup(srv.Close)
	p := benchJobProvider(t, nil)
	// benchMeasurement files a figure under the variant's content digest,
	// which it can only compute for a variant the catalog carries. Without
	// these the ledger assertions below would pass on an empty map.
	p.manifests = bootBenchLoopManifests()
	return &bootBenchLoopFixture{
		p:        p,
		requests: &requests,
		port:     portFromBenchURL(t, srv.URL),
		client:   srv.Client(),
		log:      &bytes.Buffer{},
	}
}

func bootBenchLoopManifests() []catalog.Manifest {
	variant := func(id string) catalog.Variant {
		return catalog.Variant{
			VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			Source:         catalog.VariantSource{Type: catalog.SourceOllama, Tag: id + ":q4"},
		}
	}
	return []catalog.Manifest{
		{ModelID: "qwen3-8b", Variants: []catalog.Variant{variant("qwen3-8b")}},
		{ModelID: "qwen3-27b", Variants: []catalog.Variant{variant("qwen3-27b")}},
	}
}

// depsFor mirrors main.go's closure: every field read at call time, and
// the same gates wired. EngineClaim especially — without it the run would
// not take the engine at all, and the stand-down test below would pass on
// the round's cheap pre-read alone while production relied on a claim the
// fixture never exercised.
func (f *bootBenchLoopFixture) depsFor(t *testing.T) func() BenchDeps {
	t.Helper()
	return func() BenchDeps {
		st, _ := f.p.store.Load()
		modelID, variantID := "", ""
		if st.Active != nil {
			modelID, variantID = st.Active.ModelID, st.Active.VariantID
		}
		return BenchDeps{
			EngineKind:    signer.InferenceTypeOllama,
			EngineVersion: "0.33.3",
			EnginePort:    f.port,
			EngineModel:   "qwen3:8b",
			ModelID:       modelID,
			VariantID:     variantID,
			EngineReady:   f.p.EngineReady,
			EngineQuiet:   f.p.engineQuietForBench,
			EngineClaim:   f.p.claimEngineForBench,
			HTTPClient:    f.client,
			Logger:        slog.New(slog.NewTextHandler(f.log, nil)),
			Now:           fakeNow(time.Unix(1_700_000_000, 0), time.Second),
		}
	}
}

func (f *bootBenchLoopFixture) selectModel(t *testing.T, modelID, variantID string) {
	t.Helper()
	if err := f.p.store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: modelID, VariantID: variantID,
		}
		if s.Models == nil {
			s.Models = map[string]catalog.ModelState{}
		}
		s.Models[modelID] = catalog.ModelState{State: catalog.ModelStateReady}
	}); err != nil {
		t.Fatalf("select %q: %v", modelID, err)
	}
}

// PRODUCT CONTRACT (waired-agent#1150): the boot benchmark waits for the
// engine instead of racing it, and measures once when it arrives.
//
// The one-shot it replaces asked EngineReady once, on the boot tail. On a
// host whose engine takes about a minute to come up that answer is almost
// always "not yet" — 5 completions in 82 boots, measured on one vLLM
// host, where the same boot saw the prefill measurement beside it
// complete 33 seconds after the benchmark had already stood down. Nothing
// re-ran it for the life of the daemon, so the disk cache (whose only
// writer this is) stayed empty and the host had no standing decode rate.
func TestMaybeRunBootBenchmark_WaitsForTheEngineRatherThanRacingIt(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	depsFor := f.depsFor(t)
	ctx := context.Background()

	// No committed selection yet: this is the state the boot race loses
	// to, and EngineReady says so.
	if ready, _ := f.p.EngineReady(); ready {
		t.Fatal("the fixture starts ready; there is no race to lose")
	}
	f.p.maybeRunBootBenchmark(ctx, depsFor, nil)
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("the engine was asked %d time(s) before it was up", n)
	}

	// The engine comes up. The round that follows is the one the one-shot
	// never got to make.
	f.selectModel(t, "qwen3-8b", "q4-gguf")
	if ready, _ := f.p.EngineReady(); !ready {
		t.Fatal("EngineReady is still false after selecting a ready model; " +
			"the assertion below would pass for the wrong reason")
	}
	var verdicts int
	f.p.maybeRunBootBenchmark(ctx, depsFor, func(BenchResult, BenchDeps) { verdicts++ })
	if verdicts != 1 {
		t.Fatalf("reached %d verdicts once the engine was up, want 1", verdicts)
	}
	measured := f.requests.Load()
	if measured == 0 {
		t.Fatal("the engine was never asked anything")
	}

	// And exactly once. A loop that re-measured every tick would be the
	// periodic synthetic benchmark waired-agent#202 argues against.
	f.p.maybeRunBootBenchmark(ctx, depsFor, func(BenchResult, BenchDeps) {
		t.Error("a second verdict for a selection already measured")
	})
	if n := f.requests.Load(); n != measured {
		t.Errorf("the engine was asked %d more time(s) for a selection already measured", n-measured)
	}
}

// PRODUCT CONTRACT (waired-agent#1150): a model change earns another
// attempt. The rate is a fact about (model, variant, engine, release) and
// nothing else, which is what makes at-most-once safe.
func TestMaybeRunBootBenchmark_AModelChangeEarnsAnotherAttempt(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	depsFor := f.depsFor(t)
	ctx := context.Background()

	f.selectModel(t, "qwen3-8b", "q4-gguf")
	f.p.maybeRunBootBenchmark(ctx, depsFor, nil)
	first := f.requests.Load()
	if first == 0 {
		t.Fatal("nothing measured for the first selection")
	}

	f.selectModel(t, "qwen3-27b", "q4-gguf")
	var second BenchResult
	f.p.maybeRunBootBenchmark(ctx, depsFor, func(r BenchResult, _ BenchDeps) { second = r })
	if f.requests.Load() == first {
		t.Fatal("the new model was never measured; the old figure describes a model this host no longer serves")
	}
	if second.ModelID != "qwen3-27b" {
		t.Errorf("verdict ModelID = %q, want the model that was just selected", second.ModelID)
	}
}

// PRODUCT CONTRACT (waired-agent#703, #1150): a round stands down while
// another measurement holds the engine, and stands down WITHOUT settling
// — nothing was measured, so nothing is known.
//
// Two layers do it and the test pins both. BenchDeps.EngineClaim is the
// real exclusion; the cheap read in front of it exists for the journal,
// because RunBootBenchmark logs its own decline and a fifteen-second loop
// would repeat that line for every minute the prefill measurement holds
// the engine — which is the line that gets filtered out, taking the real
// ones with it (waired-agent#633 records the same lesson for the same
// log). So the silence is asserted, not just the stand-down.
func TestMaybeRunBootBenchmark_StandsDownForTheOtherMeasurement(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	depsFor := f.depsFor(t)
	ctx := context.Background()
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	release, ok := f.p.claimEngineExclusive()
	if !ok {
		t.Fatal("could not claim the fixture engine")
	}
	f.p.maybeRunBootBenchmark(ctx, depsFor, func(BenchResult, BenchDeps) {
		t.Error("a verdict was reached while another measurement held the engine")
	})
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("the engine was asked %d time(s) while another measurement held it", n)
	}
	if f.log.Len() != 0 {
		t.Errorf("a tick that could not run said something; at one line per "+
			"fifteen seconds this fills the journal for as long as the other "+
			"measurement runs:\n%s", f.log.String())
	}

	release()
	f.p.maybeRunBootBenchmark(ctx, depsFor, nil)
	if f.requests.Load() == 0 {
		t.Error("nothing measured after the other measurement let go; " +
			"a stand-down that settles is a host that never measures")
	}
}

// PRODUCT CONTRACT (waired-agent#1150): declining is not a verdict.
//
// The two readings of readiness are taken at different moments — the
// round's, and RunBootBenchmark's own — and it is the second that decides
// whether anything is measured. A round that settled on the first would
// reinstate the one-shot under a loop.
func TestMaybeRunBootBenchmark_ADeclinedRunDoesNotSettle(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	ctx := context.Background()
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	engineUp := false
	base := f.depsFor(t)
	depsFor := func() BenchDeps {
		d := base()
		d.EngineReady = func() (bool, string) { return engineUp, "qwen3-8b" }
		return d
	}

	f.p.maybeRunBootBenchmark(ctx, depsFor, func(BenchResult, BenchDeps) {
		t.Error("a run that never reached the engine reported a verdict")
	})
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("the engine was asked %d time(s) despite answering not-ready", n)
	}

	engineUp = true
	var verdicts int
	f.p.maybeRunBootBenchmark(ctx, depsFor, func(BenchResult, BenchDeps) { verdicts++ })
	if verdicts != 1 {
		t.Fatalf("reached %d verdicts after the engine came up, want 1 — "+
			"the decline had spent this selection's one attempt", verdicts)
	}
}

// PRODUCT CONTRACT (waired-agent#203, #1150): a FAILED run is a verdict.
//
// An accelerator out of memory, a warm-up that timed out — those are
// statements about this host, and retrying them every fifteen seconds
// would saturate the engine of a machine that cannot answer while telling
// nobody anything new. speedMeasuredFor counts a failed attempt the same
// way. A model change or an engine upgrade is what earns another, which
// the selection key already expresses.
func TestMaybeRunBootBenchmark_AFailedRunIsNotRetried(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	// A port nothing is listening on: the warm-up fails, which is the
	// failure shape #203 drew the line for.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadPort := portFromBenchURL(t, dead.URL)
	dead.Close()

	base := f.depsFor(t)
	depsFor := func() BenchDeps {
		d := base()
		d.EnginePort = deadPort
		d.HTTPClient = http.DefaultClient
		return d
	}

	var results []BenchResult
	f.p.maybeRunBootBenchmark(context.Background(), depsFor,
		func(r BenchResult, _ BenchDeps) { results = append(results, r) })
	if len(results) != 1 || results[0].Outcome != benchOutcomeFailed {
		t.Fatalf("results = %+v, want one failed verdict", results)
	}
	f.p.maybeRunBootBenchmark(context.Background(), depsFor,
		func(BenchResult, BenchDeps) { t.Error("a failed run was retried on the next tick") })
}

// The loop asks again. Driven with a short poll and stopped by its
// context, so what is pinned is the shape — work happens on ticks after
// the first, which is precisely what the one-shot could not do.
func TestRunBootBenchmarkLoop_AsksAgainAfterTheEngineComesUp(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	depsFor := f.depsFor(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verdicts := make(chan BenchResult, 4)
	go f.p.runBootBenchmarkLoop(ctx, depsFor,
		func(r BenchResult, _ BenchDeps) { verdicts <- r }, time.Millisecond)

	// The engine arrives late, exactly as it does on the hardware this
	// issue was measured on.
	time.Sleep(20 * time.Millisecond)
	if len(verdicts) != 0 {
		t.Fatal("a verdict was reached before there was anything to measure")
	}
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	select {
	case r := <-verdicts:
		if r.Outcome != benchOutcomeMeasured {
			t.Errorf("Outcome = %q, want %q", r.Outcome, benchOutcomeMeasured)
		}
	case <-time.After(waitBackstop):
		t.Fatal("the loop never measured a host whose engine came up after boot")
	}
}

// PRODUCT CONTRACT (waired-agent#1150): local inference turned on after
// boot gets measured.
//
// The toggle used to be read ONCE, on the boot tail, and that single read
// enclosed the benchmark, the depth sweep AND the #1127 speed
// measurement. infCtl.onEnable starts the engine and nothing else, so a
// host that opted in later ran none of the three for the life of the
// daemon: it served peers with no published rate and had no decode figure
// of its own. Read per tick — which EngineReady already does, through
// isInferenceDisabled — it simply starts on the next one.
func TestMaybeRunBootBenchmark_LocalInferenceTurnedOnAfterBootGetsMeasured(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	depsFor := f.depsFor(t)
	ctx := context.Background()
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	off := true
	f.p.isInferenceDisabled = func() bool { return off }

	f.p.maybeRunBootBenchmark(ctx, depsFor, func(BenchResult, BenchDeps) {
		t.Error("a host told not to serve locally loaded a model to time it")
	})
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("the engine was asked %d time(s) while local inference was off", n)
	}

	off = false
	var verdicts int
	f.p.maybeRunBootBenchmark(ctx, depsFor, func(BenchResult, BenchDeps) { verdicts++ })
	if verdicts != 1 {
		t.Fatalf("reached %d verdicts after local inference was turned on, want 1", verdicts)
	}
}

// PRODUCT CONTRACT (waired-agent#1150): a measurement this path takes is
// filed where the rest of the product reads measurements.
//
// The result used to reach p.lastBench and nothing else. MeasuredVariants
// had a single writer, runBenchmarkJob, so a figure the boot path
// produced evaporated on the next restart and never reached the signed
// ModelMeasurements peers rank on, the catalog's measured_tokps,
// `waired models ls --detail`, or the tray tooltip. On a live vLLM host
// none of those had ever shown a number.
//
// LastBenchmark is deliberately untouched: it carries the generation the
// setup reconciler's re-run guard reads, and a gen-0 write inherits the
// stored one — the hazard waired-agent#980 is open on.
func TestMaybeRunBootBenchmark_FilesTheMeasurementInTheLedger(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	var got BenchResult
	f.p.maybeRunBootBenchmark(context.Background(), f.depsFor(t),
		func(r BenchResult, _ BenchDeps) { got = r })
	if got.TokensPerSec <= 0 {
		t.Fatalf("nothing was measured: %+v", got)
	}

	st, err := f.p.store.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.MeasuredVariants) != 1 {
		t.Fatalf("MeasuredVariants = %v, want the one figure just measured", st.MeasuredVariants)
	}
	for _, m := range st.MeasuredVariants {
		if m.ModelID != "qwen3-8b" || m.MeasuredTokps != got.TokensPerSec {
			t.Errorf("filed %+v, want %s at %.2f tok/s", m, "qwen3-8b", got.TokensPerSec)
		}
		if m.EngineKind != signer.InferenceTypeOllama || m.EngineVersion != "0.33.3" {
			t.Errorf("filed engine %q/%q, want the engine that measured it",
				m.EngineKind, m.EngineVersion)
		}
	}
	if st.LastBenchmark != nil {
		t.Errorf("LastBenchmark = %+v; the boot path must not write the record "+
			"that carries the wizard's generation counter (waired-agent#980)", st.LastBenchmark)
	}
}

// A cached figure is not re-filed. measuredRatesFrom keeps the most
// recent entry per variant, so re-dating a figure taken at an earlier
// boot would let it outrank a fresher measurement of the same variant.
func TestSettleBootBench_ACachedFigureIsNotReFiled(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	deps := BenchDeps{
		ModelID: "qwen3-8b", VariantID: "q4-gguf",
		EngineKind: signer.InferenceTypeOllama, EngineVersion: "0.33.3",
	}
	f.p.settleBootBench(deps, BenchResult{
		Outcome: benchOutcomeMeasured, TokensPerSec: 99, Capacity: 3,
		ModelID: "qwen3-8b", VariantID: "q4-gguf", Cached: true,
	})
	st, err := f.p.store.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.MeasuredVariants) != 0 {
		t.Errorf("a cached figure was filed with today's date: %v", st.MeasuredVariants)
	}
}

func TestBootBenchSelectionKey(t *testing.T) {
	full := BenchDeps{ModelID: "m", VariantID: "v", EngineKind: "ollama", EngineVersion: "0.33.3"}
	if bootBenchSelectionKey(full) == "" {
		t.Fatal("a complete selection produced no key")
	}
	if bootBenchSelectionKey(BenchDeps{VariantID: "v", EngineKind: "ollama"}) != "" {
		t.Error("a host with no committed model produced a key; the first real " +
			"selection would inherit its attempt")
	}
	for _, tc := range []struct {
		name string
		d    BenchDeps
	}{
		{"model", BenchDeps{ModelID: "other", VariantID: "v", EngineKind: "ollama", EngineVersion: "0.33.3"}},
		{"variant", BenchDeps{ModelID: "m", VariantID: "other", EngineKind: "ollama", EngineVersion: "0.33.3"}},
		{"engine kind", BenchDeps{ModelID: "m", VariantID: "v", EngineKind: "vllm", EngineVersion: "0.33.3"}},
		{"engine release", BenchDeps{ModelID: "m", VariantID: "v", EngineKind: "ollama", EngineVersion: "0.32.15"}},
	} {
		if bootBenchSelectionKey(tc.d) == bootBenchSelectionKey(full) {
			t.Errorf("a changed %s did not earn a new measurement", tc.name)
		}
	}
}

func TestBenchReachedAVerdict(t *testing.T) {
	for outcome, want := range map[string]bool{
		benchOutcomeMeasured:       true,
		benchOutcomeFailed:         true,
		benchOutcomeEngineNotReady: false,
		benchOutcomeSkipped:        false,
		"":                         false,
	} {
		if got := benchReachedAVerdict(BenchResult{Outcome: outcome}); got != want {
			t.Errorf("benchReachedAVerdict(%q) = %v, want %v", outcome, got, want)
		}
	}
}

func TestBootBenchSettledFor_NilProviderDoesNotAskForAMeasurement(t *testing.T) {
	var p *agentInferenceProvider
	if !p.bootBenchSettledFor("anything") {
		t.Error("a nil provider asked for a measurement it has nowhere to record")
	}
	p.markBootBenchSettled("anything") // must not panic
}
