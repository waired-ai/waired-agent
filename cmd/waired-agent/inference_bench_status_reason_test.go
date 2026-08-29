package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// PRODUCT CONTRACT (waired-agent#1150): a host still waiting for its
// engine does not answer what a host nobody has asked answers.
//
// With no persisted record and no job in flight, /inference/benchmark/
// status returned a bare "idle" for a host whose boot benchmark had
// stopped at the readiness gate — the same bytes as "nobody has asked for
// one yet". That is the reading gap #1150 had to close by hand, from
// journal lines, on live hardware.
func TestBenchmarkStatus_SaysTheEngineWasNotReady(t *testing.T) {
	p := benchJobProvider(t, nil)

	// Nothing has happened at all: idle, and silent about why.
	before := p.BenchmarkStatus()
	if before.State != management.BenchmarkStateIdle || before.Outcome != "" {
		t.Fatalf("a host nobody has asked = %+v, want a bare idle", before)
	}

	p.SetLastBench(notReadyBench())
	got := p.BenchmarkStatus()
	if got.State != management.BenchmarkStateIdle {
		t.Errorf("state = %q, want %q — the engine was never reached, so this is "+
			"neither a failure nor a run in flight", got.State, management.BenchmarkStateIdle)
	}
	if got.Outcome != benchOutcomeEngineNotReady {
		t.Errorf("outcome = %q, want %q; without it this answer is byte-identical "+
			"to the one above", got.Outcome, benchOutcomeEngineNotReady)
	}
	if got.Error == "" {
		t.Error("no reason was carried")
	}
	if got.MeasuredTokps != 0 {
		t.Errorf("measured_tokps = %v, want 0 — nothing was measured", got.MeasuredTokps)
	}
}

// A run that reached a working engine and could not measure it keeps
// answering "failed" — the #203 ending, and the one arm this change must
// not swallow.
func TestBenchmarkStatus_AFailedRunStillReadsAsFailed(t *testing.T) {
	p := benchJobProvider(t, nil)
	p.SetLastBench(failBench(BenchDeps{Logger: testLogger()}, "warmup", errFakeWarmup{}))

	got := p.BenchmarkStatus()
	if got.State != management.BenchmarkStateFailed {
		t.Fatalf("state = %q, want %q", got.State, management.BenchmarkStateFailed)
	}
	if got.Outcome != benchOutcomeFailed {
		t.Errorf("outcome = %q, want %q", got.Outcome, benchOutcomeFailed)
	}
}

type errFakeWarmup struct{}

func (errFakeWarmup) Error() string { return "engine refused the warm-up" }
