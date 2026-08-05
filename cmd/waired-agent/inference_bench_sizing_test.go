package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestPlanBenchSizing pins the sizing decision.
//
// PRODUCT CONTRACT for the two rows marked below: #203 states that only a
// failure on a demonstrably working engine should read as "could not
// measure", and a request the host cannot satisfy inside its own deadline
// is not such a failure. Everything else here is a record of today's
// arithmetic — the fractions and the slack have no ratifying source.
func TestPlanBenchSizing(t *testing.T) {
	for _, c := range []struct {
		name       string
		facts      benchSizingFacts
		wantTokens int
		// wantTimeoutAtLeast is a floor rather than an equality: the exact
		// value falls out of the slack constant, which is a judgement call,
		// but "never shorter than the old fixed cap" is the invariant.
		wantTimeoutAtLeast time.Duration
	}{
		{
			// CONTRACT: nothing measured yet must not ask for the full
			// length. This is the row that was broken — sample 0 asked for
			// 200 tokens under 30 s on a host that needed ~100 s.
			name:               "first sample takes the probe",
			facts:              benchSizingFacts{ObservedTokps: 0, Remaining: benchMeasureBudget, SamplesLeft: 3},
			wantTokens:         benchProbeTokens,
			wantTimeoutAtLeast: benchProbeTimeout,
		},
		{
			// CONTRACT: the CI host. 2 tok/s with ~104 s left over 2 samples
			// affords 52 s * 0.7 * 2 = 72 tokens — a request it can serve.
			name:               "slow host gets a request it can serve",
			facts:              benchSizingFacts{ObservedTokps: 2, Remaining: 104 * time.Second, SamplesLeft: 2},
			wantTokens:         72,
			wantTimeoutAtLeast: benchTimeout,
		},
		{
			name:               "fast host still gets the full length",
			facts:              benchSizingFacts{ObservedTokps: 120, Remaining: 110 * time.Second, SamplesLeft: 2},
			wantTokens:         benchPromptCompletionTokens,
			wantTimeoutAtLeast: benchTimeout,
		},
		{
			name:               "budget already spent falls back to the probe",
			facts:              benchSizingFacts{ObservedTokps: 2, Remaining: 0, SamplesLeft: 2},
			wantTokens:         benchProbeTokens,
			wantTimeoutAtLeast: benchTimeout,
		},
		{
			name:               "negative remaining is treated as spent",
			facts:              benchSizingFacts{ObservedTokps: 2, Remaining: -5 * time.Second, SamplesLeft: 1},
			wantTokens:         benchProbeTokens,
			wantTimeoutAtLeast: benchTimeout,
		},
		{
			name:               "a host too slow for even the probe share still gets the probe",
			facts:              benchSizingFacts{ObservedTokps: 0.5, Remaining: 10 * time.Second, SamplesLeft: 3},
			wantTokens:         benchProbeTokens,
			wantTimeoutAtLeast: benchTimeout,
		},
		{
			name:               "lost sample count falls back to the probe",
			facts:              benchSizingFacts{ObservedTokps: 50, Remaining: benchMeasureBudget, SamplesLeft: 0},
			wantTokens:         benchProbeTokens,
			wantTimeoutAtLeast: benchProbeTimeout,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := planBenchSizing(c.facts)
			if got.CompletionTokens != c.wantTokens {
				t.Errorf("CompletionTokens = %d, want %d", got.CompletionTokens, c.wantTokens)
			}
			if got.RequestTimeout < c.wantTimeoutAtLeast {
				t.Errorf("RequestTimeout = %v, want >= %v", got.RequestTimeout, c.wantTimeoutAtLeast)
			}
			// The invariant that actually matters: the deadline must cover
			// the completion we just planned, or we asked for something we
			// never intended to wait for.
			if c.facts.ObservedTokps > 0 {
				need := time.Duration(float64(got.CompletionTokens) / c.facts.ObservedTokps * float64(time.Second))
				if got.RequestTimeout < need {
					t.Errorf("RequestTimeout %v cannot cover %d tokens at %.1f tok/s (needs %v)",
						got.RequestTimeout, got.CompletionTokens, c.facts.ObservedTokps, need)
				}
			}
		})
	}
}

// TestPlanBenchSizing_NeverPlansAnUnservableRequest sweeps the rate range
// the catalog actually spans and asserts the invariant directly, so a
// future change to the fractions cannot quietly reintroduce #203.
//
// PRODUCT CONTRACT (#203): a working engine must not be reported as a
// benchmark failure because the harness asked it for more than it could
// deliver in the time allowed.
func TestPlanBenchSizing_NeverPlansAnUnservableRequest(t *testing.T) {
	for _, tokps := range []float64{0.25, 0.5, 1, 2, 5, 12, 36, 100, 400} {
		for _, remaining := range []time.Duration{
			benchMeasureBudget, 90 * time.Second, 30 * time.Second, 5 * time.Second,
		} {
			for samples := 1; samples <= benchSampleCount; samples++ {
				p := planBenchSizing(benchSizingFacts{
					ObservedTokps: tokps, Remaining: remaining, SamplesLeft: samples,
				})
				need := time.Duration(float64(p.CompletionTokens) / tokps * float64(time.Second))
				// Below ~0.27 tok/s even benchProbeTokens exceeds the whole
				// shared budget, so no plan is servable and the honest
				// outcome is a real "could not measure on this host". The
				// contract only binds where a servable plan exists.
				if float64(benchProbeTokens)/tokps > benchMeasureBudget.Seconds() {
					continue
				}
				if p.RequestTimeout < need {
					t.Fatalf("tokps=%.2f remaining=%v samples=%d: planned %d tokens under %v, needs %v",
						tokps, remaining, samples, p.CompletionTokens, p.RequestTimeout, need)
				}
				if p.CompletionTokens < benchProbeTokens || p.CompletionTokens > benchPromptCompletionTokens {
					t.Fatalf("tokps=%.2f: CompletionTokens = %d, outside [%d,%d]",
						tokps, p.CompletionTokens, benchProbeTokens, benchPromptCompletionTokens)
				}
			}
		}
	}
}

// TestMeasureOllamaNative_SlowHostStillProducesANumber is the regression
// test for the red nightly lane: run 30998191050, job 92280317430, where
// `install+inference (linux)` failed the `no benchmark THROUGHPUT figure`
// assert on a 4-vCPU CPU-only runner serving qwen3.5-9b.
//
// The engine here refuses anything longer than 64 tokens, standing in for
// a host that cannot decode more inside the request deadline. Before the
// sizing fix the first sample asked for 200, got the refusal, and
// measureOllamaNative returned an error with zero samples — which
// RunBootBenchmark reported as a benchmark failure on a working engine.
//
// PRODUCT CONTRACT (#203, first bullet).
func TestMeasureOllamaNative_SlowHostStillProducesANumber(t *testing.T) {
	engine := &fakeOllamaEngine{
		evalCount:       32,
		evalDurationsNS: []int64{16_000_000_000}, // 32 tokens in 16 s = 2 tok/s
		// The threshold IS the bug: this host can serve anything short of
		// the full benchPromptCompletionTokens, and the old code asked for
		// exactly that on its very first sample.
		maxServableNumPredict: benchPromptCompletionTokens - 1,
	}
	srv := httptest.NewServer(engine.handler())
	t.Cleanup(srv.Close)

	deps := BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  portFromBenchURL(t, srv.URL),
		EngineModel: "qwen3.5-9b",
		HTTPClient:  http.DefaultClient,
		Logger:      slog.Default(),
	}
	tokps, _, samples, err := measureOllamaNative(context.Background(), deps)
	if err != nil {
		t.Fatalf("measureOllamaNative failed on a working-but-slow engine: %v", err)
	}
	if samples == 0 {
		t.Fatal("no samples taken")
	}
	if tokps <= 0 {
		t.Fatalf("tokps = %v, want a real rate", tokps)
	}
	if len(engine.generateNumPredict) == 0 {
		t.Fatal("engine saw no /api/generate call")
	}
	// The fix, stated as the thing the engine actually received.
	if got := engine.generateNumPredict[0]; got != benchProbeTokens {
		t.Errorf("first sample asked for %d tokens, want the probe (%d)", got, benchProbeTokens)
	}
	for i, n := range engine.generateNumPredict {
		if n > engine.maxServableNumPredict {
			t.Errorf("sample %d asked for %d tokens, more than this host can serve (%d)",
				i, n, engine.maxServableNumPredict)
		}
	}
}

// TestMeasureOllamaNative_FastHostStillAsksForTheFullLength guards the
// other direction: the sizing fix must not shrink the sample on a host
// that was never in trouble, or every existing measurement changes basis.
//
// Record of today's behaviour — benchPromptCompletionTokens is a tuning
// choice (#764), not a ratified contract.
func TestMeasureOllamaNative_FastHostStillAsksForTheFullLength(t *testing.T) {
	engine := &fakeOllamaEngine{
		evalCount:       200,
		evalDurationsNS: []int64{2_000_000_000}, // 100 tok/s
	}
	srv := httptest.NewServer(engine.handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), benchMeasureBudget)
	t.Cleanup(cancel)
	_, _, samples, err := measureOllamaNative(ctx, BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  portFromBenchURL(t, srv.URL),
		EngineModel: "qwen3:8b-q4_K_M",
		HTTPClient:  http.DefaultClient,
		Logger:      slog.Default(),
	})
	if err != nil {
		t.Fatalf("measureOllamaNative: %v", err)
	}
	if samples != benchSampleCount {
		t.Fatalf("samples = %d, want %d", samples, benchSampleCount)
	}
	if got := engine.generateNumPredict[0]; got != benchProbeTokens {
		t.Errorf("first sample asked for %d, want the probe (%d)", got, benchProbeTokens)
	}
	// Once the probe has established ~100 tok/s there is budget for the
	// full length, and the remaining samples must take it.
	for i, n := range engine.generateNumPredict[1:] {
		if n != benchPromptCompletionTokens {
			t.Errorf("sample %d asked for %d tokens, want the full %d on a fast host",
				i+1, n, benchPromptCompletionTokens)
		}
	}
}
