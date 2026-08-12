package main

import (
	"context"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
)

// seedActiveReady makes EngineReady() answer true on a provider built by
// benchJobProvider: an active selection whose model is in the catalog as
// ready. The other two EngineReady gates (isInferenceDisabled, ollama
// health) are nil on such a provider and skip themselves.
func seedActiveReady(t *testing.T, p *agentInferenceProvider, modelID string) {
	t.Helper()
	if err := p.store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{
			Runtime:   catalog.RuntimeOllama,
			ModelID:   modelID,
			VariantID: "q4-gguf",
		}
		if s.Models == nil {
			s.Models = map[string]catalog.ModelState{}
		}
		s.Models[modelID] = catalog.ModelState{State: catalog.ModelStateReady}
	}); err != nil {
		t.Fatalf("seed active model: %v", err)
	}
	if ready, _ := p.EngineReady(); !ready {
		t.Fatalf("EngineReady is false after seeding %q; the front gate would answer 425 and the test would pass for the wrong reason", modelID)
	}
}

// notReadyBench is what RunBootBenchmark's readiness gates return
// (inference_bench.go): Failed, because every #203 consumer gates on that
// flag to skip an unusable measurement, plus the Outcome that says the
// engine was never reached.
//
// Built by the production constructor rather than restated here, so the
// two cannot drift — and so the busy-engine gate added for #582/#601,
// which returns the same shape with a different reason, is covered by
// every test below without a second fixture.
func notReadyBench() BenchResult {
	return notReadyBenchResult(BenchDeps{VariantID: "q4-gguf"}, "engine not ready")
}

// TestRunBenchmark_NotReadyLeavesThroughThe425Door is #576.
//
// PRODUCT CONTRACT — the Inference interface's own wording
// (internal/management/inference_handlers.go): "ok is false when the
// engine/model is not ready yet (the handler maps this to 425/409 so a
// caller can poll)". The provider used to answer ok=true carrying
// BenchmarkOutcome.Failed for a run that stopped at the readiness gate,
// which leaves through the 503 `benchmark_did_not_complete` door — and
// `waired init` reads 503 as a fault and exits 3, which install.sh
// (WAIRED_INIT_LOCAL_AI_DOWN) and install.ps1 branch on. Observed on a
// host whose engine measured 20 tok/s 52 seconds later.
func TestRunBenchmark_NotReadyLeavesThroughThe425Door(t *testing.T) {
	p := benchJobProvider(t, func(context.Context) BenchResult { return notReadyBench() })
	seedActiveReady(t, p, "granite4-350m")

	out, ok, err := p.RunBenchmark(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil — not-ready is not an error", err)
	}
	if ok {
		t.Fatalf("ok = true (out = %+v), want false: the 425 door", out)
	}
	if out.Failed || out.Error != "" {
		t.Errorf("outcome = %+v, want the zero value — nothing ran, so there is nothing to report as failed", out)
	}
}

// TestRunBenchmark_AFailedRunStillLeavesThroughThe503Door is the negative
// control for the test above, and the load-bearing half: only the
// readiness ending moves doors.
//
// PRODUCT CONTRACT (waired-agent#29). A warm-up that got an engine 5xx,
// or a measurement that timed out, RAN — the engine is the thing to look
// at, and the handler must keep answering 503 so the CLI says so instead
// of polling for ten minutes and then printing a success box.
func TestRunBenchmark_AFailedRunStillLeavesThroughThe503Door(t *testing.T) {
	p := benchJobProvider(t, func(context.Context) BenchResult {
		return BenchResult{
			Capacity:  1,
			VariantID: "q4-gguf",
			Failed:    true,
			Err:       "warm-up failed: HTTP 500: llama-server process has terminated",
			Outcome:   benchOutcomeFailed,
		}
	})
	seedActiveReady(t, p, "granite4-350m")

	out, ok, err := p.RunBenchmark(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Fatal("ok = false, want true: a run that reached the engine and failed goes out of the 503 door, not the 425 one")
	}
	if !out.Failed {
		t.Error("outcome.Failed = false; the handler keys the 503 door on it")
	}
	if out.Error != "warm-up failed: HTTP 500: llama-server process has terminated" {
		t.Errorf("outcome.Error = %q, want the engine's own reason carried through", out.Error)
	}
}

// TestRunBenchmark_JoiningANotReadyJobAlsoAnswers425 is the shape the CI
// log took: a benchmark job was already in flight (the setup reconciler
// kicks one on the served generation counter), RunBenchmark's front
// EngineReady gate answered ready, and startBenchmarkJob returned the
// running job's channel rather than starting a second run. The joined
// job's verdict is the one the caller gets, so it has to leave by the
// same door.
func TestRunBenchmark_JoiningANotReadyJobAlsoAnswers425(t *testing.T) {
	release := make(chan struct{})
	runs := 0
	p := benchJobProvider(t, func(context.Context) BenchResult {
		runs++
		<-release
		return notReadyBench()
	})
	seedActiveReady(t, p, "granite4-350m")
	// The join leaves no state a poll could observe, so the provider's
	// test hook is the only honest way to hold the first run open until
	// the POST has actually joined it. Waiting on benchJobDone instead
	// was a no-op — startSetupBenchmark registers it synchronously — and
	// on a loaded runner the run could finish before the goroutine below
	// ever called startBenchmarkJob, which then started a legitimate
	// second run and failed the single-run assertion.
	joined := make(chan struct{})
	p.benchJobJoined = func() { close(joined) }

	p.startSetupBenchmark(7) // in flight, blocked in the measurement

	type result struct {
		out management.BenchmarkOutcome
		ok  bool
		err error
	}
	got := make(chan result, 1)
	go func() {
		out, ok, err := p.RunBenchmark(context.Background())
		got <- result{out, ok, err}
	}()

	// Let the POST reach startBenchmarkJob and join before the job ends.
	select {
	case <-joined:
	case <-time.After(waitBackstop):
		t.Fatal("RunBenchmark never joined the in-flight job")
	}
	close(release)

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("err = %v, want nil", r.err)
		}
		if r.ok {
			t.Fatalf("ok = true (out = %+v), want false — the joined run stopped at the readiness gate", r.out)
		}
	case <-time.After(waitBackstop):
		t.Fatal("RunBenchmark did not return")
	}
	if runs != 1 {
		t.Errorf("measurement ran %d times, want 1 — the POST must join, not start a second run", runs)
	}
}

// TestBenchmarkJob_NotReadyIsNotACompletionRecord: a run that stopped at
// the readiness gate never reached the engine, so it is not a benchmark
// result and must not become one.
//
// Two things follow from recording it, both observed (#576). It replaces
// this host's last real measurement with a failure — /benchmark/status
// answers `failed` where it used to answer the number. And because the
// record carries the REQUESTED generation, the setup reconciler's retry
// guard (`bs.Gen < d.benchmarkGen`, setup_desired.go) is already
// satisfied, so the wizard's benchmark step stays failed for a host whose
// engine came up seconds later and nothing ever re-runs it.
func TestBenchmarkJob_NotReadyIsNotACompletionRecord(t *testing.T) {
	p := benchJobProvider(t, func(context.Context) BenchResult { return notReadyBench() })
	measuredAt := time.Now().UTC().Add(-time.Hour)
	if err := p.store.Update(func(s *catalog.State) {
		s.LastBenchmark = &catalog.BenchmarkRecord{
			Gen: 5, MeasuredTokps: 99, Method: benchMethodOllamaEval, MeasuredAt: measuredAt,
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	waitDone(t, p.startBenchmarkJob(7))

	got := p.BenchmarkStatus()
	if got.State != management.BenchmarkStateDone {
		t.Errorf("state = %q, want %q — the gate ending is not a completed run",
			got.State, management.BenchmarkStateDone)
	}
	if got.Gen != 5 || got.MeasuredTokps != 99 {
		t.Errorf("status = gen %d / %v tok/s, want the untouched gen 5 / 99", got.Gen, got.MeasuredTokps)
	}
	if got.Error != "" {
		t.Errorf("error = %q, want empty", got.Error)
	}

	// And on disk, which is what survives a restart and what the
	// reconciler's generation comparison reads.
	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.LastBenchmark == nil || st.LastBenchmark.Gen != 5 || st.LastBenchmark.Failed {
		t.Errorf("persisted record = %+v, want the seeded gen-5 measurement untouched", st.LastBenchmark)
	}
}

// TestBenchmarkJob_NotReadyStillDeRatesTheNode pins the half that must
// NOT change with the above.
//
// PRODUCT CONTRACT (#203). SetLastBench feeds AdvertisedCapacity, and the
// gate returns Capacity 1 as the fail-safe — 0 means UNLIMITED on the
// wire. Skipping the *completion record* must not also skip telling the
// mesh this host has no working engine.
func TestBenchmarkJob_NotReadyStillDeRatesTheNode(t *testing.T) {
	p := benchJobProvider(t, func(context.Context) BenchResult { return notReadyBench() })
	waitDone(t, p.startBenchmarkJob(0))
	if got := p.AdvertisedCapacity(); got != 1 {
		t.Errorf("AdvertisedCapacity = %d, want 1 (0 would advertise UNLIMITED)", got)
	}
}

// TestRunBenchmark_MeasuredRunIsUnaffected is the plain path, so the two
// door tests above cannot both pass by returning ok=false for everything.
func TestRunBenchmark_MeasuredRunIsUnaffected(t *testing.T) {
	p := benchJobProvider(t, func(context.Context) BenchResult {
		return BenchResult{TokensPerSec: 42, Capacity: 1, Outcome: benchOutcomeMeasured}
	})
	seedActiveReady(t, p, "granite4-350m")

	out, ok, err := p.RunBenchmark(context.Background())
	if err != nil || !ok {
		t.Fatalf("RunBenchmark = (%+v, %v, %v), want a measured result", out, ok, err)
	}
	if out.MeasuredTokps != 42 || out.Failed {
		t.Errorf("outcome = %+v, want 42 tok/s and not failed", out)
	}
	if got := p.BenchmarkStatus(); got.State != management.BenchmarkStateDone || got.MeasuredTokps != 42 {
		t.Errorf("status = %+v, want done/42 — a real run is still recorded", got)
	}
}
