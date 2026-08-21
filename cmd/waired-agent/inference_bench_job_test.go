package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/management"
)

// benchJobProvider builds a provider whose measurement is the injected
// fn — the real path talks HTTP to an engine with multi-minute budgets.
// The engine is a REAL adapter brought to Ready, not an absent one.
//
// It used to be omitted, and EngineReady's health check was written as
// `p.ollama != nil && servingEngine() == ollama`, so a provider with no
// adapter read as ready. That branch is unreachable in production —
// agentInferenceProvider always builds an OllamaAdapter — and the same
// spelling let a vLLM host advertise capacity with a dead engine (#944).
// Now that "no adapter" correctly means "not ready", the fixture has to
// model the host it claims to be: seedActiveReady asserts EngineReady as a
// precondition precisely so these tests cannot pass through the wrong door.
func benchJobProvider(t *testing.T, run func(ctx context.Context) BenchResult) *agentInferenceProvider {
	t.Helper()
	a := newTestAdapter(t)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("fixture engine did not come up: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return &agentInferenceProvider{
		ollama: a,
		store:  catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		profiler: hardware.NewProfiler(t.TempDir(),
			hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
				return nil, hardware.Accelerators{}, nil
			})),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		benchRun: run,
	}
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(waitBackstop):
		t.Fatal("benchmark job did not complete")
	}
}

// TestBenchmarkJob_SingleFlightAndPersistence pins the waired#835 §12
// job semantics: concurrent starts join one run, completion is
// persisted to catalog.State.LastBenchmark, and BenchmarkStatus
// reflects running → done.
func TestBenchmarkJob_SingleFlightAndPersistence(t *testing.T) {
	block := make(chan struct{})
	runs := 0
	p := benchJobProvider(t, func(context.Context) BenchResult {
		runs++
		<-block
		return BenchResult{TokensPerSec: 42, Capacity: 1}
	})

	done1 := p.startBenchmarkJob(3)
	done2 := p.startBenchmarkJob(7) // joins; gen 7 is NOT a second run
	if done1 != done2 {
		t.Fatal("concurrent startBenchmarkJob calls must join the same run")
	}
	if got := p.BenchmarkStatus(); got.State != management.BenchmarkStateRunning {
		t.Fatalf("state while in flight = %q, want running", got.State)
	}

	close(block)
	waitDone(t, done1)
	if runs != 1 {
		t.Fatalf("measurement ran %d times, want 1", runs)
	}

	got := p.BenchmarkStatus()
	if got.State != management.BenchmarkStateDone || got.Gen != 3 || got.MeasuredTokps != 42 {
		t.Fatalf("status after completion = %+v, want done/gen=3/42", got)
	}

	// Persisted: a fresh provider over the same store (simulated
	// restart) reads the same record back.
	fresh := benchJobProvider(t, nil)
	fresh.store = p.store
	if got := fresh.BenchmarkStatus(); got.State != management.BenchmarkStateDone || got.Gen != 3 {
		t.Fatalf("status after restart = %+v, want done/gen=3", got)
	}

	// The job slot is free again — a new run starts (not a join).
	done3 := p.startBenchmarkJob(0)
	waitDone(t, done3)
	if runs != 2 {
		t.Fatalf("second explicit run: measurement ran %d times, want 2", runs)
	}
}

// TestBenchmarkJob_GenZeroKeepsStoredGen: a boot/CLI run (gen 0) must
// not regress a counter-driven generation the CP already saw.
func TestBenchmarkJob_GenZeroKeepsStoredGen(t *testing.T) {
	p := benchJobProvider(t, func(context.Context) BenchResult {
		return BenchResult{TokensPerSec: 10, Capacity: 1}
	})
	if err := p.store.Update(func(s *catalog.State) {
		s.LastBenchmark = &catalog.BenchmarkRecord{Gen: 5, MeasuredTokps: 99, MeasuredAt: time.Now().UTC()}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	waitDone(t, p.startBenchmarkJob(0))
	got := p.BenchmarkStatus()
	if got.Gen != 5 {
		t.Fatalf("gen after gen-0 run = %d, want 5 (kept)", got.Gen)
	}
	if got.MeasuredTokps != 10 {
		t.Fatalf("measured after gen-0 run = %v, want the fresh 10", got.MeasuredTokps)
	}
}

// TestBenchmarkJob_FailedRun surfaces state=failed with the error
// detail, both in memory and across a restart.
func TestBenchmarkJob_FailedRun(t *testing.T) {
	p := benchJobProvider(t, func(context.Context) BenchResult {
		return BenchResult{Failed: true, Err: "engine exploded", Capacity: 1}
	})
	waitDone(t, p.startBenchmarkJob(2))
	got := p.BenchmarkStatus()
	if got.State != management.BenchmarkStateFailed || got.Error != "engine exploded" || got.Gen != 2 {
		t.Fatalf("status = %+v, want failed/gen=2 with error detail", got)
	}
}

// TestBenchmarkJob_IdleBeforeAnyRun: no in-memory record, nothing
// persisted → idle.
func TestBenchmarkJob_IdleBeforeAnyRun(t *testing.T) {
	p := benchJobProvider(t, nil)
	if got := p.BenchmarkStatus(); got.State != management.BenchmarkStateIdle {
		t.Fatalf("state = %q, want idle", got.State)
	}
}
