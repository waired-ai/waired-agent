package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestBenchmarkSetupErrorCode is waired-agent#203 on the surface a
// person reads.
//
// PRODUCT CONTRACT — #203 proposal 1 ("Upstream broken should be
// attributed to the engine/model step, not to the benchmark"). Every
// other place in the benchmark path already draws this line:
// RunBootBenchmark gates on the readiness check before it dials, the
// management API answers 425 rather than 503 for a not-ready engine,
// and runBenchmarkJob refuses to persist that ending as a record. The
// setup projection was the one that did not, so an operator whose engine
// had not finished installing was shown an internal error.
func TestBenchmarkSetupErrorCode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome string
		want    string
	}{
		{
			// The case the issue is about. It is not a fault in Waired
			// and not a verdict on the host — it is a state to poll.
			"an engine that was never ready is not an internal error",
			benchOutcomeEngineNotReady, signer.SetupErrorEngineNotReady,
		},
		{
			"a run that reached the engine and failed stays internal",
			benchOutcomeFailed, signer.SetupErrorInternal,
		},
		{
			// A record written before Outcome was persisted. Guessing
			// would be worse than the unspecific answer it already gave.
			"an unrecorded outcome keeps the old code",
			"", signer.SetupErrorInternal,
		},
		{
			"an outcome this build does not know keeps the old code",
			"some_future_ending", signer.SetupErrorInternal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := benchmarkSetupErrorCode(tc.outcome); got != tc.want {
				t.Errorf("benchmarkSetupErrorCode(%q) = %q, want %q", tc.outcome, got, tc.want)
			}
		})
	}
	// The code must be one the wire declares, or the wizard renders it
	// generically and the whole point is lost.
	if !signer.IsValidSetupErrorCode(benchmarkSetupErrorCode(benchOutcomeEngineNotReady)) {
		t.Error("the mapped code is not a declared SetupError* value")
	}
}

// TestBenchmarkStatus_ReportsABootFailureThatReachedNoOtherSurface is
// #203 proposal 2.
//
// A boot benchmark that fails warn-logs and returns. It does not persist
// a catalog.BenchmarkRecord, does not move BenchmarkStatus, and does not
// appear in SetupProgress — the failure the issue actually reported (an
// engine install that left nothing listening) was observable only by
// reading the daemon log. This pins that it now reaches the management
// API, which is the surface inside this repo that can carry it.
func TestBenchmarkStatus_ReportsABootFailureThatReachedNoOtherSurface(t *testing.T) {
	p := benchJobProvider(t, nil)

	if got := p.BenchmarkStatus(); got.State != management.BenchmarkStateIdle {
		t.Fatalf("state = %q before any run, want idle", got.State)
	}

	// What the boot path does with a failure today: SetLastBench, and
	// nothing else.
	p.SetLastBench(failBench(BenchDeps{
		Logger:    discardLogger(),
		VariantID: "q4-gguf",
	}, "measure", errDialRefused{}))

	got := p.BenchmarkStatus()
	if got.State != management.BenchmarkStateFailed {
		t.Errorf("state = %q after a failed boot benchmark, want failed: "+
			"the run reached no surface at all before this", got.State)
	}
	if got.Outcome != benchOutcomeFailed {
		t.Errorf("outcome = %q, want %q", got.Outcome, benchOutcomeFailed)
	}
	if got.Error == "" {
		t.Error("error detail is empty; the reason is the useful half")
	}
}

// TestBenchmarkStatus_ABootRunThatNeverReachedTheEngineIsNotAFailure is
// the guard on the test above. A fresh install benchmarks WHILE `waired
// init` is still installing the engine and pulling the first model, so
// engine-not-ready is the ordinary shape of a first boot and self-heals
// minutes later. Reporting it would turn every first install into a
// visible failure — the mistake #203 exists to stop, arriving from the
// other direction.
func TestBenchmarkStatus_ABootRunThatNeverReachedTheEngineIsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bench BenchResult
	}{
		{"engine not ready", notReadyBench()},
		{"skipped: no engine at all", BenchResult{Capacity: 0, Outcome: benchOutcomeSkipped}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := benchJobProvider(t, nil)
			p.SetLastBench(tc.bench)
			if got := p.BenchmarkStatus(); got.State != management.BenchmarkStateIdle {
				t.Errorf("state = %q, want idle: %s is not a verdict about this host",
					got.State, tc.name)
			}
		})
	}
}

// errDialRefused stands in for the transport error the issue reported —
// `dial tcp 127.0.0.1:9475: refused`, whose real cause was a failed
// engine install.
type errDialRefused struct{}

func (errDialRefused) Error() string { return "dial tcp 127.0.0.1:9475: connect: connection refused" }
