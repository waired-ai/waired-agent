package main

import (
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

// bouncingEngine is a working fake ollama that can be taken away
// mid-request, which is what the defect looks like from the benchmark's
// side: `ollama serve` is stopped under an in-flight generation and the
// client gets a bare `EOF` — no status, no body, nothing to classify.
//
// dieOn decides per request, from its path and its 1-based ordinal.
// moveGen says whether the process generation moves with it: a restart
// THIS AGENT ordered moves it, and a crash does not. The two are the whole
// distinction the fix keys on, so the fake takes both rather than assuming
// one (CLAUDE.md §Test discipline — a fake that drops a parameter makes
// the failing case unwritable).
type bouncingEngine struct {
	inner   http.HandlerFunc
	dieOn   func(path string, n int) bool
	moveGen bool

	calls atomic.Int64
	gen   atomic.Uint64
}

func (b *bouncingEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := int(b.calls.Add(1))
	if b.dieOn != nil && b.dieOn(r.URL.Path, n) {
		if b.moveGen {
			// Bumped BEFORE the connection goes, so the generation the
			// benchmark re-reads after its request fails is already the new
			// one. Ordering it this way is what makes the test deterministic
			// without a sleep — the real adapter bumps its generation before
			// the stop for the same reason (internal/runtime/ollama.go).
			b.gen.Add(1)
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}
	b.inner(w, r)
}

// newBouncingEngine serves a healthy 100 tok/s host except where dieOn
// says otherwise.
func newBouncingEngine(t *testing.T, dieOn func(path string, n int) bool, moveGen bool) (*bouncingEngine, BenchDeps) {
	t.Helper()
	// eval_count 200 over 2 s = 100 tok/s, so a successful run is
	// Capacity = floor(100/30) = 3.
	fake := &fakeOllamaEngine{evalCount: 200, evalDurationsNS: []int64{2_000_000_000}}
	eng := &bouncingEngine{inner: fake.handler(), dieOn: dieOn, moveGen: moveGen}
	srv := httptest.NewServer(eng)
	t.Cleanup(srv.Close)

	deps := BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  portFromBenchURL(t, srv.URL),
		EngineModel: "granite4:350m",
		VariantID:   "bf16-gguf",
		EngineReady: func() (bool, string) { return true, "granite4-350m" },
		EngineGen:   func() uint64 { return eng.gen.Load() },
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
		// One request per connection: a closed connection then never
		// looks like a reusable idle one, so the request count below is
		// exactly what the benchmark issued.
		HTTPClient: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		Logger:     slog.Default(),
	}
	return eng, deps
}

// TestRunBootBenchmark_BusyEngineIsNotAPerformanceVerdict is the front
// half of the fix: a host with a download still in flight is not measured
// at all, because the reconcile that download will fire on its way out
// restarts the engine.
//
// PRODUCT CONTRACT — #582 ("a benchmark interrupted by a restart the agent
// itself ordered must not report the host as unable to answer") and #601.
// Reported through the 425 door rather than the 503 one: `waired init`
// polls the first and exits 3 on the second, which install.sh
// (WAIRED_INIT_LOCAL_AI_DOWN) and install.ps1 branch on.
func TestRunBootBenchmark_BusyEngineIsNotAPerformanceVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("engine was contacted at %s while a download was in flight", r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	var quietCalls atomic.Int64
	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  portFromBenchURL(t, srv.URL),
		VariantID:   "bf16-gguf",
		EngineReady: func() (bool, string) { return true, "granite4-350m" },
		EngineQuiet: func(context.Context) bool {
			quietCalls.Add(1)
			return false
		},
		HTTPClient: http.DefaultClient,
		Logger:     slog.Default(),
	})

	if quietCalls.Load() == 0 {
		t.Fatal("EngineQuiet was never consulted")
	}
	// The SAME value the readiness gate returns, deliberately: that is the
	// one RunBenchmark maps to ok=false and the management layer to 425,
	// including for a caller that JOINED the run
	// (TestRunBenchmark_JoiningANotReadyJobAlsoAnswers425). A new outcome
	// string here would leave through the 503 door and keep the exit 3.
	if got.Outcome != benchOutcomeEngineNotReady {
		t.Errorf("Outcome = %q, want %q", got.Outcome, benchOutcomeEngineNotReady)
	}
	if got.Capacity != 1 {
		t.Errorf("Capacity = %d, want 1 (0 would advertise UNLIMITED)", got.Capacity)
	}
	if !got.Failed {
		t.Error("Failed = false; consumers gate on it to skip an unusable measurement")
	}
	if got.TokensPerSec != 0 {
		t.Errorf("TokensPerSec = %v, want 0 — nothing was measured", got.TokensPerSec)
	}
}

// PRODUCT CONTRACT (waired-agent#703): a benchmark that cannot take the
// engine leaves through the SAME 425 door a busy one does, and contacts
// the engine not at all.
//
// EngineQuiet answers for an instant and this run is minutes long, so the
// install-time host-speed measurement — a background goroutine — could
// start anywhere after the gate passed. On real hardware it did, at
// 17:19:57 against a measurement that published at 17:20:41, and the
// figure described the two evicting each other.
func TestRunBootBenchmark_AClaimedEngineIsNotAPerformanceVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("engine was contacted at %s while another measurement held it", r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	var claims, releases atomic.Int64
	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  portFromBenchURL(t, srv.URL),
		VariantID:   "bf16-gguf",
		EngineReady: func() (bool, string) { return true, "granite4-350m" },
		EngineQuiet: func(context.Context) bool { return true },
		EngineClaim: func() (func(), bool) {
			claims.Add(1)
			return func() { releases.Add(1) }, false
		},
		HTTPClient: http.DefaultClient,
		Logger:     slog.Default(),
	})

	if claims.Load() == 0 {
		t.Fatal("EngineClaim was never consulted; a quiet answer is not a held engine")
	}
	if got.Outcome != benchOutcomeEngineNotReady {
		t.Errorf("Outcome = %q, want %q — a new outcome string leaves through the 503 door", got.Outcome, benchOutcomeEngineNotReady)
	}
	if !got.Failed || got.TokensPerSec != 0 {
		t.Errorf("Failed=%v TokensPerSec=%v, want a failed run with nothing measured", got.Failed, got.TokensPerSec)
	}
	if got.Capacity != 1 {
		t.Errorf("Capacity = %d, want 1 (0 would advertise UNLIMITED)", got.Capacity)
	}
	if releases.Load() != 0 {
		t.Errorf("released %d times a claim it never got", releases.Load())
	}
}

// PRODUCT CONTRACT (waired-agent#703): the claim is taken ONCE and held
// across the bounce-grace retries. Claiming per iteration would hand the
// gap between a restart and the next attempt to the other measurement —
// which is the window this exists to close.
func TestRunBootBenchmark_TheClaimIsHeldAcrossARestartRetry(t *testing.T) {
	// The engine dies under the first warm-up, and this agent is what
	// killed it — the #582/#601 sequence, retried without charge.
	_, deps := newBouncingEngine(t, func(path string, n int) bool { return n == 1 }, true)

	var claims, releases atomic.Int64
	deps.EngineQuiet = func(context.Context) bool { return true }
	deps.EngineClaim = func() (func(), bool) {
		if claims.Add(1) > 1 {
			// A second claim means the first was let go mid-run, which is
			// the window the other measurement would take.
			return func() {}, false
		}
		return func() { releases.Add(1) }, true
	}

	got := RunBootBenchmark(context.Background(), deps)

	if got.Failed {
		t.Fatalf("Failed=true (%q); the restart should have been retried without charge", got.Err)
	}
	if claims.Load() != 1 {
		t.Errorf("EngineClaim called %d times, want 1 — the claim must span the retries", claims.Load())
	}
	if releases.Load() != 1 {
		t.Errorf("released %d times, want exactly 1 on the way out", releases.Load())
	}
}

// TestRunBootBenchmark_RestartUnderTheWarmUpIsNotAFailure is the back
// half: the quiet gate can still be passed microseconds before the last
// pull lands, and the reconcile then arrives on top of the warm-up. This
// is the exact sequence in #601's journal — activation at 05:01:50.653,
// EOF at 05:01:53.021, a new `ollama serve` listening at 05:01:53.047.
//
// PRODUCT CONTRACT — #582/#601.
func TestRunBootBenchmark_RestartUnderTheWarmUpIsNotAFailure(t *testing.T) {
	eng, deps := newBouncingEngine(t,
		func(path string, n int) bool { return n == 1 }, true)

	got := RunBootBenchmark(context.Background(), deps)

	if got.Failed {
		t.Fatalf("Failed=true (%q) — the engine restart was OURS, and the host answered "+
			"fine on the retry; this is the exit 3 in #601", got.Err)
	}
	if got.Outcome != benchOutcomeMeasured {
		t.Errorf("Outcome = %q, want %q", got.Outcome, benchOutcomeMeasured)
	}
	if got.TokensPerSec < 99 || got.TokensPerSec > 101 {
		t.Errorf("TokensPerSec = %.1f, want ≈ 100", got.TokensPerSec)
	}
	// The killed warm-up, then a full clean run.
	if want := int64(2 + benchSampleCount); eng.calls.Load() != want {
		t.Errorf("engine saw %d requests, want %d (killed warm-up + warm-up + %d samples)",
			eng.calls.Load(), want, benchSampleCount)
	}
}

// TestRunBootBenchmark_RestartUnderTheMeasurementIsNotAFailure is the same
// contract one request later: the bounce can land after the warm-up
// succeeded, on the first timed sample.
//
// PRODUCT CONTRACT — #582/#601.
func TestRunBootBenchmark_RestartUnderTheMeasurementIsNotAFailure(t *testing.T) {
	// Request 1 is the warm-up; request 2 is the first /api/generate.
	eng, deps := newBouncingEngine(t,
		func(path string, n int) bool { return n == 2 }, true)

	got := RunBootBenchmark(context.Background(), deps)

	if got.Failed {
		t.Fatalf("Failed=true (%q), want the retry's measurement", got.Err)
	}
	if got.TokensPerSec < 99 || got.TokensPerSec > 101 {
		t.Errorf("TokensPerSec = %.1f, want ≈ 100", got.TokensPerSec)
	}
	if want := int64(2 + 1 + benchSampleCount); eng.calls.Load() != want {
		t.Errorf("engine saw %d requests, want %d (warm-up + killed sample + warm-up + %d samples)",
			eng.calls.Load(), want, benchSampleCount)
	}
}

// TestRunBootBenchmark_DeathWithoutARestartStillDeRates guards the other
// direction. An engine that drops the connection while the agent stopped
// nothing is a host that genuinely could not answer, and it must keep the
// failed verdict — the 503 door and `waired init`'s exit 3 are the signal
// install.sh branches on for a device whose local AI is down.
//
// PRODUCT CONTRACT — #203 ("Only a failure on a demonstrably working
// engine should read as 'could not measure'"), read the other way round.
func TestRunBootBenchmark_DeathWithoutARestartStillDeRates(t *testing.T) {
	eng, deps := newBouncingEngine(t,
		func(path string, n int) bool { return n == 1 }, false)

	got := RunBootBenchmark(context.Background(), deps)

	if got.Outcome != benchOutcomeFailed {
		t.Errorf("Outcome = %q, want %q — nothing restarted this engine", got.Outcome, benchOutcomeFailed)
	}
	if !got.Failed || got.Capacity != 1 {
		t.Errorf("Failed=%v Capacity=%d, want true/1", got.Failed, got.Capacity)
	}
	if eng.calls.Load() != 1 {
		t.Errorf("engine saw %d requests, want 1 — a failure with no restart is not retried",
			eng.calls.Load())
	}
}

// TestRunBootBenchmark_BounceGraceIsBounded pins the ceiling. An engine
// that restarts forever is not given a free pass forever: after the grace
// the run reports an honest failure, so a crash-looping host still reaches
// exit 3 instead of being reported as merely "not ready yet" until init's
// ten-minute deadline runs out.
//
// PRODUCT CONTRACT — the bound is the same argument enginePullBounceGrace
// records for downloads (#359).
func TestRunBootBenchmark_BounceGraceIsBounded(t *testing.T) {
	eng, deps := newBouncingEngine(t,
		func(path string, n int) bool { return path == "/v1/chat/completions" }, true)

	got := RunBootBenchmark(context.Background(), deps)

	if got.Outcome != benchOutcomeFailed {
		t.Errorf("Outcome = %q, want %q after the grace is spent", got.Outcome, benchOutcomeFailed)
	}
	if want := int64(benchEngineBounceGrace + 1); eng.calls.Load() != want {
		t.Errorf("engine saw %d warm-ups, want %d (the first plus %d free retries)",
			eng.calls.Load(), want, benchEngineBounceGrace)
	}
}

// TestRunBootBenchmark_NilQuietAndGenKeepTodaysPath pins the opt-in shape:
// a caller that wires neither hook behaves exactly as before, which is
// what keeps the pre-existing tests meaningful.
//
// Record of today's behaviour, not a contract.
func TestRunBootBenchmark_NilQuietAndGenKeepTodaysPath(t *testing.T) {
	fake := &fakeOllamaEngine{evalCount: 200, evalDurationsNS: []int64{2_000_000_000}}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  portFromBenchURL(t, srv.URL),
		EngineModel: "granite4:350m",
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	})
	if got.Failed {
		t.Fatalf("Failed=true (%q), want the plain measurement", got.Err)
	}
	if got.TokensPerSec < 99 || got.TokensPerSec > 101 {
		t.Errorf("TokensPerSec = %.1f, want ≈ 100", got.TokensPerSec)
	}
}

// TestEngineQuietForBench covers the predicate the two live callers wire.
//
// Record of today's behaviour, except the vLLM arm, which is a contract:
// the pull registry and the serve-env reconcile this guards are ollama's,
// so answering "busy" for a host serving something else would gate that
// host's benchmark off permanently.
func TestEngineQuietForBench(t *testing.T) {
	t.Run("no engine adapter is quiet", func(t *testing.T) {
		var p *agentInferenceProvider
		if !p.engineQuietForBench(context.Background()) {
			t.Error("a nil provider answered busy")
		}
		if !(&agentInferenceProvider{}).engineQuietForBench(context.Background()) {
			t.Error("a provider with no ollama adapter answered busy; nothing there can restart an engine")
		}
	})

	t.Run("a ready idle engine is quiet", func(t *testing.T) {
		p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
		hostCutoffEngineUp(t, p)
		if !p.engineQuietForBench(context.Background()) {
			t.Error("an idle ready engine answered busy")
		}
	})

	t.Run("a download in flight is not quiet", func(t *testing.T) {
		p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
		hostCutoffEngineUp(t, p)
		p.pullMu.Lock()
		p.pullsInFlight = map[string]*pullJob{"the-probe-model": {modelID: "the-probe-model"}}
		p.pullMu.Unlock()
		if p.engineQuietForBench(context.Background()) {
			t.Error("answered quiet with a pull in flight — its completion reconcile is the restart in #601")
		}
	})

	t.Run("a pending reconcile is not quiet", func(t *testing.T) {
		p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
		hostCutoffEngineUp(t, p)
		p.engineReconcileInFlight.Store(true)
		t.Cleanup(func() { p.engineReconcileInFlight.Store(false) })
		if p.engineQuietForBench(context.Background()) {
			t.Error("answered quiet with a reconcile in flight — it stops and respawns the engine")
		}
	})

	t.Run("a vLLM host is quiet even mid-download", func(t *testing.T) {
		p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
		hostCutoffEngineUp(t, p)
		p.setServingEngine(catalog.RuntimeVLLM)
		p.pullMu.Lock()
		p.pullsInFlight = map[string]*pullJob{"something": {modelID: "something"}}
		p.pullMu.Unlock()
		if !p.engineQuietForBench(context.Background()) {
			t.Error("a vLLM host answered busy; nothing here reconciles its serve env, " +
				"so this would gate its benchmark off forever")
		}
	})
}
