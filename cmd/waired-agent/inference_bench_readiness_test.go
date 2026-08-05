package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestRunBootBenchmark_NotListeningIsNotAPerformanceVerdict is the #203
// contract: an engine that is not up yet must not be reported as a
// benchmark failure.
//
// PRODUCT CONTRACT — #203 first bullet ("Upstream broken should be
// attributed to the engine/model step, not to the benchmark"). The
// observed instance is the nightly install+inference leg, where the boot
// benchmark fires the instant enrollment succeeds while `waired init` is
// still installing the engine, and logs
// `reason=warmup ... dial tcp 127.0.0.1:9475: connect: connection refused`.
func TestRunBootBenchmark_NotListeningIsNotAPerformanceVerdict(t *testing.T) {
	// A closed port: if the readiness gate does not fire, the warm-up
	// dials it and we get the old failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("engine was contacted at %s despite EngineReady reporting not-ready", r.URL.Path)
	}))
	port := portFromBenchURL(t, srv.URL)
	srv.Close()

	var calls atomic.Int64
	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind: signer.InferenceTypeOllama,
		EnginePort: port,
		EngineReady: func() (bool, string) {
			calls.Add(1)
			return false, "qwen3.5-9b"
		},
		HTTPClient: http.DefaultClient,
		Logger:     slog.Default(),
	})

	if calls.Load() == 0 {
		t.Fatal("EngineReady was never consulted")
	}
	if got.Outcome != benchOutcomeEngineNotReady {
		t.Errorf("Outcome = %q, want %q", got.Outcome, benchOutcomeEngineNotReady)
	}
	// Capacity 1, NOT 0. On the wire 0 means "unlimited"
	// (proto/signer/inference_state.go) and the probe loop leaves the
	// field off the push when it is 0 — so 0 here would advertise a host
	// with no working engine as accepting unbounded concurrency.
	if got.Capacity != 1 {
		t.Errorf("Capacity = %d, want 1 (0 would advertise UNLIMITED)", got.Capacity)
	}
	// Failed stays true: every consumer gates on it, and
	// buildRecommendation would otherwise compare a zero rate against the
	// interactive floor and recommend a lighter model for a host nobody
	// measured.
	if !got.Failed {
		t.Error("Failed = false; consumers gate on it to skip an unusable measurement")
	}
	if got.TokensPerSec != 0 {
		t.Errorf("TokensPerSec = %v, want 0 — nothing was measured", got.TokensPerSec)
	}
}

// TestRunBootBenchmark_ReadyEngineThatErrorsStillDeRates guards the other
// direction: a listening engine that fails is a real measurement failure
// and must keep its Capacity=1 de-rating and its Failed flag.
//
// PRODUCT CONTRACT — #203 ("Only a failure on a demonstrably working
// engine should read as 'could not measure'").
func TestRunBootBenchmark_ReadyEngineThatErrorsStillDeRates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  portFromBenchURL(t, srv.URL),
		EngineReady: func() (bool, string) { return true, "qwen3.5-9b" },
		HTTPClient:  http.DefaultClient,
		Logger:      slog.Default(),
	})
	if got.Outcome != benchOutcomeFailed {
		t.Errorf("Outcome = %q, want %q", got.Outcome, benchOutcomeFailed)
	}
	if !got.Failed || got.Capacity != 1 {
		t.Errorf("Failed=%v Capacity=%d, want true/1", got.Failed, got.Capacity)
	}
}

// TestRunBootBenchmark_NilEngineReadyKeepsTodaysPath pins the opt-in
// shape: a caller that does not wire the gate behaves exactly as before,
// which is what keeps the pre-existing tests meaningful.
//
// Record of today's behaviour, not a contract.
func TestRunBootBenchmark_NilEngineReadyKeepsTodaysPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind: signer.InferenceTypeOllama,
		EnginePort: portFromBenchURL(t, srv.URL),
		HTTPClient: http.DefaultClient,
		Logger:     slog.Default(),
	})
	if !got.Failed || got.Capacity != 1 {
		t.Errorf("Failed=%v Capacity=%d, want true/1", got.Failed, got.Capacity)
	}
}

// TestAdvertisedCapacity_LiftedBySetLastBench is the other half of #203's
// complaint ("the de-rating is indefinite"): a later successful benchmark
// must raise what the node advertises, without a daemon restart.
//
// PRODUCT CONTRACT — #203.
func TestAdvertisedCapacity_LiftedBySetLastBench(t *testing.T) {
	p := &agentInferenceProvider{}
	if got := p.AdvertisedCapacity(); got != 0 {
		t.Errorf("with no benchmark yet: %d, want 0", got)
	}
	// The boot de-rating.
	p.SetLastBench(BenchResult{Capacity: 1, Failed: true, Outcome: benchOutcomeEngineNotReady})
	if got := p.AdvertisedCapacity(); got != 1 {
		t.Errorf("after the boot de-rating: %d, want 1", got)
	}
	// A real measurement once the engine finally came up.
	p.SetLastBench(BenchResult{Capacity: 5, TokensPerSec: 160, Outcome: benchOutcomeMeasured})
	if got := p.AdvertisedCapacity(); got != 5 {
		t.Errorf("after a real measurement: %d, want 5 — the de-rating must not be permanent", got)
	}
}

// TestCapacityFn_PrefersTheLiveProviderAnswer pins the getter the probe
// loop reads, including the no-provider fallback that --disable-inference
// and an unenrolled daemon take.
//
// Record of today's behaviour.
func TestCapacityFn_PrefersTheLiveProviderAnswer(t *testing.T) {
	t.Run("no provider falls back to the boot value", func(t *testing.T) {
		if got := capacityFn(3, nil)(); got != 3 {
			t.Errorf("= %d, want 3", got)
		}
		if got := capacityFn(3, &inferenceSubsystem{})(); got != 3 {
			t.Errorf("nil provider: = %d, want 3", got)
		}
	})
	t.Run("provider with nothing measured falls back to the boot value", func(t *testing.T) {
		sub := &inferenceSubsystem{provider: &agentInferenceProvider{}}
		if got := capacityFn(2, sub)(); got != 2 {
			t.Errorf("= %d, want 2", got)
		}
	})
	t.Run("a later measurement wins over the boot value", func(t *testing.T) {
		prov := &agentInferenceProvider{}
		sub := &inferenceSubsystem{provider: prov}
		fn := capacityFn(1, sub)
		if got := fn(); got != 1 {
			t.Fatalf("before the measurement: %d, want the boot 1", got)
		}
		prov.SetLastBench(BenchResult{Capacity: 7, Outcome: benchOutcomeMeasured})
		if got := fn(); got != 7 {
			t.Errorf("after the measurement: %d, want 7 from the same getter", got)
		}
	})
}
