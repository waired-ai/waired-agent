package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// bootBenchLoopFixture is a provider whose EngineReady can be driven, in
// front of a fake engine that answers the measurement protocol and counts
// what it was asked.
//
// The seam is the one production has: the loop decides whether to try, the
// provider's single-flight job measures through RunBootBenchmark, and
// speedDepsHook points the live deps at the fake engine. Nothing replaces
// RunBootBenchmark itself — a fake in its place would make the engine-start
// race, which is the whole subject, unwritable.
type bootBenchLoopFixture struct {
	p        *agentInferenceProvider
	engine   *fakeOllamaEngine
	requests *atomic.Int64
	port     int
	client   *http.Client
	log      *bytes.Buffer

	mu       sync.Mutex
	verdicts []BenchResult
	// adjust, when non-nil, is applied to the deps after the fixture's own.
	adjust func(*BenchDeps)
}

func newBootBenchLoopFixture(t *testing.T) *bootBenchLoopFixture {
	t.Helper()
	var requests atomic.Int64
	engine := &fakeOllamaEngine{}
	inner := engine.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		inner(w, r)
	}))
	t.Cleanup(srv.Close)
	p := benchJobProvider(t, nil)
	// benchMeasurement files a figure under the variant's content digest,
	// which it can only compute for a variant the catalog carries.
	p.manifests = bootBenchLoopManifests()
	f := &bootBenchLoopFixture{
		p:        p,
		engine:   engine,
		requests: &requests,
		port:     portFromBenchURL(t, srv.URL),
		client:   srv.Client(),
		log:      &bytes.Buffer{},
	}
	p.speedDepsHook = func(d *BenchDeps) {
		d.EngineKind = signer.InferenceTypeOllama
		d.EngineVersion = "0.33.3"
		d.EnginePort = f.port
		d.EngineModel = "qwen3:8b"
		d.HTTPClient = f.client
		d.Logger = slog.New(slog.NewTextHandler(f.log, nil))
		d.Now = fakeNow(time.Unix(1_700_000_000, 0), time.Second)
		f.mu.Lock()
		adjust := f.adjust
		f.mu.Unlock()
		if adjust != nil {
			adjust(d)
		}
	}
	p.onSpeedVerdict = func(b BenchResult) {
		f.mu.Lock()
		f.verdicts = append(f.verdicts, b)
		f.mu.Unlock()
	}
	return f
}

func (f *bootBenchLoopFixture) verdictCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.verdicts)
}

func (f *bootBenchLoopFixture) lastVerdict() BenchResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.verdicts) == 0 {
		return BenchResult{}
	}
	return f.verdicts[len(f.verdicts)-1]
}

func (f *bootBenchLoopFixture) setAdjust(fn func(*BenchDeps)) {
	f.mu.Lock()
	f.adjust = fn
	f.mu.Unlock()
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

// PRODUCT CONTRACT (waired-agent#1150): the measurement waits for the
// engine instead of racing it, and measures once when it arrives.
func TestMaybeRunBootBenchmark_WaitsForTheEngineRatherThanRacingIt(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	ctx := context.Background()

	if ready, _ := f.p.EngineReady(); ready {
		t.Fatal("the fixture starts ready; there is no race to lose")
	}
	f.p.maybeRunBootBenchmark(ctx)
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("the engine was asked %d time(s) before it was up", n)
	}

	f.selectModel(t, "qwen3-8b", "q4-gguf")
	if ready, _ := f.p.EngineReady(); !ready {
		t.Fatal("EngineReady is still false after selecting a ready model")
	}
	f.p.maybeRunBootBenchmark(ctx)
	if f.verdictCount() != 1 {
		t.Fatalf("reached %d verdicts once the engine was up, want 1", f.verdictCount())
	}
	measured := f.requests.Load()
	if measured == 0 {
		t.Fatal("the engine was never asked anything")
	}
	if f.p.IsMeasuringSpeed() {
		t.Error("the readiness gate is still armed after the selection's verdict")
	}

	// And exactly once — not the periodic synthetic benchmark
	// waired-agent#202 argues against.
	f.p.maybeRunBootBenchmark(ctx)
	if n := f.requests.Load(); n != measured || f.verdictCount() != 1 {
		t.Errorf("a selection already measured was measured again (%d more requests)", n-measured)
	}
}

// PRODUCT CONTRACT (waired-agent#1150): a model change earns another
// attempt.
func TestMaybeRunBootBenchmark_AModelChangeEarnsAnotherAttempt(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	ctx := context.Background()

	f.selectModel(t, "qwen3-8b", "q4-gguf")
	f.p.maybeRunBootBenchmark(ctx)
	first := f.requests.Load()
	if first == 0 {
		t.Fatal("nothing measured for the first selection")
	}

	f.selectModel(t, "qwen3-27b", "q4-gguf")
	f.p.maybeRunBootBenchmark(ctx)
	if f.requests.Load() == first {
		t.Fatal("the new model was never measured")
	}
	if got := f.lastVerdict().ModelID; got != "qwen3-27b" {
		t.Errorf("verdict ModelID = %q, want the model that was just selected", got)
	}
}

// PRODUCT CONTRACT (waired-agent#703, #1150): a round stands down while
// another measurement holds the engine, WITHOUT settling and without a log
// line per tick.
func TestMaybeRunBootBenchmark_StandsDownForTheOtherMeasurement(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	ctx := context.Background()
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	release, ok := f.p.claimEngineExclusive()
	if !ok {
		t.Fatal("could not claim the fixture engine")
	}
	f.p.maybeRunBootBenchmark(ctx)
	if n := f.requests.Load(); n != 0 || f.verdictCount() != 0 {
		t.Fatalf("the engine was asked %d time(s) while another measurement held it", n)
	}
	if f.log.Len() != 0 {
		t.Errorf("a tick that could not run said something:\n%s", f.log.String())
	}

	release()
	f.p.maybeRunBootBenchmark(ctx)
	if f.requests.Load() == 0 {
		t.Error("nothing measured after the other measurement let go")
	}
}

// PRODUCT CONTRACT (waired-agent#1150): declining is not a verdict.
func TestMaybeRunBootBenchmark_ADeclinedRunDoesNotSettle(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	ctx := context.Background()
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	var engineUp atomic.Bool
	f.setAdjust(func(d *BenchDeps) {
		d.EngineReady = func() (bool, string) { return engineUp.Load(), "qwen3-8b" }
	})
	f.p.maybeRunBootBenchmark(ctx)
	if n := f.requests.Load(); n != 0 || f.verdictCount() != 0 {
		t.Fatalf("the engine was asked %d time(s) despite answering not-ready", n)
	}
	if !f.p.IsMeasuringSpeed() {
		t.Error("the readiness gate was cleared by a run that measured nothing")
	}

	engineUp.Store(true)
	f.p.maybeRunBootBenchmark(ctx)
	if f.verdictCount() != 1 {
		t.Fatalf("reached %d verdicts after the engine came up, want 1", f.verdictCount())
	}
}

// PRODUCT CONTRACT (waired-agent#203, #1150): a FAILED run is a verdict,
// and is not retried every tick.
func TestMaybeRunBootBenchmark_AFailedRunIsNotRetried(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadPort := portFromBenchURL(t, dead.URL)
	dead.Close()
	f.setAdjust(func(d *BenchDeps) {
		d.EnginePort = deadPort
		d.HTTPClient = http.DefaultClient
	})

	f.p.maybeRunBootBenchmark(context.Background())
	if f.verdictCount() != 1 || f.lastVerdict().Outcome != benchOutcomeFailed {
		t.Fatalf("verdicts = %+v, want one failed verdict", f.verdicts)
	}
	f.p.maybeRunBootBenchmark(context.Background())
	if f.verdictCount() != 1 {
		t.Error("a failed run was retried on the next tick")
	}
}

// The loop asks again after the engine comes up.
func TestRunBootBenchmarkLoop_AsksAgainAfterTheEngineComesUp(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.p.runBootBenchmarkLoop(ctx, time.Millisecond)

	time.Sleep(20 * time.Millisecond)
	if f.verdictCount() != 0 {
		t.Fatal("a verdict was reached before there was anything to measure")
	}
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	deadline := time.Now().Add(waitBackstop)
	for f.verdictCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.verdictCount() == 0 {
		t.Fatal("the loop never measured a host whose engine came up after boot")
	}
	if got := f.lastVerdict().Outcome; got != benchOutcomeMeasured {
		t.Errorf("Outcome = %q, want %q", got, benchOutcomeMeasured)
	}
}

// PRODUCT CONTRACT (waired-agent#1150): local inference turned on after boot
// gets measured.
func TestMaybeRunBootBenchmark_LocalInferenceTurnedOnAfterBootGetsMeasured(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	ctx := context.Background()
	f.selectModel(t, "qwen3-8b", "q4-gguf")

	var off atomic.Bool
	off.Store(true)
	f.p.isInferenceDisabled = func() bool { return off.Load() }

	f.p.maybeRunBootBenchmark(ctx)
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("the engine was asked %d time(s) while local inference was off", n)
	}
	off.Store(false)
	f.p.maybeRunBootBenchmark(ctx)
	if f.verdictCount() != 1 {
		t.Fatalf("reached %d verdicts after local inference was turned on, want 1", f.verdictCount())
	}
}

// PRODUCT CONTRACT (waired-agent#1150, #1341): a measurement the loop takes
// is filed where the rest of the product reads measurements — the ledger,
// in seconds per request, with the engine that measured it.
func TestMaybeRunBootBenchmark_FilesTheMeasurementInTheLedger(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	f.selectModel(t, "qwen3-8b", "q4-gguf")
	f.p.maybeRunBootBenchmark(context.Background())
	got := f.lastVerdict()
	if got.TurnSeconds <= 0 {
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
		if m.ModelID != "qwen3-8b" || m.TurnSeconds != got.TurnSeconds || m.MeasuredTokps != got.DecodeTokps {
			t.Errorf("filed %+v, want qwen3-8b at %.1f s per request", m, got.TurnSeconds)
		}
		if m.EngineKind != signer.InferenceTypeOllama || m.EngineVersion != "0.33.3" {
			t.Errorf("filed engine %q/%q, want the engine that measured it", m.EngineKind, m.EngineVersion)
		}
	}
	// A loop run answers no generation of its own: the stored one is kept.
	if st.LastBenchmark == nil || st.LastBenchmark.Gen != 0 || st.LastBenchmark.TurnSeconds != got.TurnSeconds {
		t.Errorf("LastBenchmark = %+v, want the gen-0 record of this run", st.LastBenchmark)
	}
}

// PRODUCT CONTRACT (plan §a): a measurement that gave the engine back to
// this host's own traffic is not a verdict — the selection stays owed and
// the readiness gate stays armed — and the next attempt waits for the
// traffic to be gone first.
func TestMaybeRunBootBenchmark_AYieldIsNotAVerdict(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	f.engine.hold = make(chan struct{})
	f.engine.sampleArrived = make(chan struct{})
	t.Cleanup(func() { close(f.engine.hold) })
	f.selectModel(t, "qwen3-8b", "q4-gguf")
	var serving atomic.Int64
	f.setAdjust(func(d *BenchDeps) {
		d.ServingInFlight = func() int { return int(serving.Load()) }
		d.StallCap = 5 * time.Second // a missed yield fails instead of hanging
	})
	go func() {
		<-f.engine.sampleArrived
		serving.Store(1)
	}()

	f.p.maybeRunBootBenchmark(context.Background())
	if f.verdictCount() != 0 {
		t.Fatalf("a yield reached a verdict: %+v", f.lastVerdict())
	}
	if !f.p.yieldedRecently() {
		t.Error("the yield was not remembered; the next attempt would start into the same session")
	}
	if !f.p.IsMeasuringSpeed() {
		t.Error("the readiness gate was cleared by a run that measured nothing")
	}
	st, _ := f.p.store.Load()
	if len(st.MeasuredVariants) != 0 {
		t.Errorf("a yielded measurement was filed: %v", st.MeasuredVariants)
	}
}

// The boot tail's synchronous attempt answers from a stored figure only.
func TestSeedBootBenchmark_NeverMeasures(t *testing.T) {
	f := newBootBenchLoopFixture(t)
	f.selectModel(t, "qwen3-8b", "q4-gguf")
	got := f.p.seedBootBenchmark(context.Background())
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("the boot seed sent %d request(s); it must never hold the start on a measurement", n)
	}
	if benchReachedAVerdict(got) {
		t.Errorf("seed with nothing stored = %+v, want no verdict", got)
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
		{"window", BenchDeps{ModelID: "m", VariantID: "v", EngineKind: "ollama", EngineVersion: "0.33.3", AppliedWindow: 32768}},
		{"kv cache type", BenchDeps{ModelID: "m", VariantID: "v", EngineKind: "ollama", EngineVersion: "0.33.3", KVCacheType: "q4_0"}},
		{"parallel slots", BenchDeps{ModelID: "m", VariantID: "v", EngineKind: "ollama", EngineVersion: "0.33.3", NumParallel: 2}},
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
