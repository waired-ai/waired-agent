package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
)

// The subject rule (waired-agent#971): the throughput a host reports is
// the one filed against the model it is SERVING.
//
// Before this, catalog.BenchmarkRecord identified a run by the generation
// it was requested under and by nothing else, so the number outlived the
// model it described — and the browser setup page renders it directly
// above the button that changes the model.

func benchAt(min int) time.Time {
	return time.Date(2026, 8, 22, 10, min, 0, 0, time.UTC)
}

func ledger(entries ...catalog.VariantMeasurement) catalog.State {
	st := catalog.State{MeasuredVariants: map[string]catalog.VariantMeasurement{}}
	for i, e := range entries {
		st.MeasuredVariants[string(rune('a'+i))] = e
	}
	return st
}

func TestServedModelFigure(t *testing.T) {
	// A run of the served model: everything it carries is coherent, so
	// the run-shaped fields travel with the number.
	ofServed := catalog.BenchmarkRecord{
		ModelID: "qwen3.5-4b", VariantID: "q4", MeasuredTokps: 19.8,
		Method: "ollama_eval", SpreadPct: 3, Trials: 5, MeasuredAt: benchAt(0),
	}
	// The same run, but of the model this host has since moved off.
	ofOther := catalog.BenchmarkRecord{
		ModelID: "qwen3.5-9b", VariantID: "q4", MeasuredTokps: 7.1,
		Method: "ollama_eval", SpreadPct: 3, Trials: 5, MeasuredAt: benchAt(0),
	}
	// A record written by a build that predates the label.
	unlabelled := catalog.BenchmarkRecord{
		MeasuredTokps: 31.2, Method: "wall_clock", SpreadPct: 9, Trials: 5, MeasuredAt: benchAt(0),
	}
	entry := catalog.VariantMeasurement{
		ModelID: "qwen3.5-4b", VariantID: "q4", MeasuredTokps: 19.8,
		Method: "ollama_eval", MeasuredAt: benchAt(30),
	}

	cases := []struct {
		name    string
		rec     catalog.BenchmarkRecord
		st      catalog.State
		active  string
		wantOK  bool
		want    benchmarkFigure
		because string
	}{
		{
			name: "the run measured the served model", rec: ofServed, st: ledger(entry),
			active: "qwen3.5-4b", wantOK: true,
			want: benchmarkFigure{
				ModelID: "qwen3.5-4b", Tokps: 19.8, MeasuredAt: benchAt(0),
				Method: "ollama_eval", SpreadPct: 3, Trials: 5,
			},
			because: "the record is about the right model, so its spread and trials belong to the number",
		},
		{
			name: "the run measured something else, the ledger has this one",
			rec:  ofOther, st: ledger(entry), active: "qwen3.5-4b", wantOK: true,
			want: benchmarkFigure{
				ModelID: "qwen3.5-4b", Tokps: 19.8, MeasuredAt: benchAt(30), Method: "ollama_eval",
			},
			because: "a real measurement of the right model; the run-shaped detail is left out rather than borrowed",
		},
		{
			name: "the run measured something else and nothing has measured this one",
			rec:  ofOther, st: ledger(), active: "qwen3.5-4b", wantOK: false,
			because: "nothing honest can be said about what this host serves",
		},
		{
			name: "unlabelled record, nothing in the ledger",
			rec:  unlabelled, st: ledger(), active: "qwen3.5-4b", wantOK: true,
			want: benchmarkFigure{
				Tokps: 31.2, MeasuredAt: benchAt(0), Method: "wall_clock", SpreadPct: 9, Trials: 5,
			},
			because: "keeping it is the behaviour hosts had before the label existed (benchDescribes)",
		},
		{
			name: "unlabelled record, but the ledger has the served model",
			rec:  unlabelled, st: ledger(entry), active: "qwen3.5-4b", wantOK: true,
			want: benchmarkFigure{
				ModelID: "qwen3.5-4b", Tokps: 19.8, MeasuredAt: benchAt(30), Method: "ollama_eval",
			},
			because: "a labelled measurement of the right model beats one that names nothing",
		},
		{
			name: "a failed run", rec: catalog.BenchmarkRecord{Failed: true, ModelID: "qwen3.5-4b"},
			st: ledger(entry), active: "qwen3.5-4b", wantOK: false,
			because: "a failure is not a measurement, and attaching the ledger's number would report a speed for a run that errored",
		},
		{
			name: "the host serves nothing", rec: ofOther, st: ledger(entry), active: "", wantOK: false,
			because: "there is no served model for a figure to be about",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := servedModelFigure(tc.rec, tc.st, tc.active)
			if ok != tc.wantOK {
				t.Fatalf("reported = %v, want %v — %s", ok, tc.wantOK, tc.because)
			}
			if !ok {
				return
			}
			if got != tc.want {
				t.Fatalf("figure = %+v, want %+v — %s", got, tc.want, tc.because)
			}
		})
	}
}

// measurementOfModel keys on the model, but the ledger keys on the
// VARIANT — one model can have several, each timed separately — so the
// newest entry is the one that describes the host now.
func TestMeasurementOfModelPrefersTheNewestVariant(t *testing.T) {
	st := ledger(
		catalog.VariantMeasurement{ModelID: "m", VariantID: "q4", MeasuredTokps: 10, MeasuredAt: benchAt(0)},
		catalog.VariantMeasurement{ModelID: "m", VariantID: "q8", MeasuredTokps: 6, MeasuredAt: benchAt(30)},
		catalog.VariantMeasurement{ModelID: "other", VariantID: "q4", MeasuredTokps: 99, MeasuredAt: benchAt(45)},
	)
	got, ok := measurementOfModel(st, "m")
	if !ok || got.VariantID != "q8" {
		t.Fatalf("measurementOfModel = %+v (%v), want the q8 entry", got, ok)
	}
	if _, ok := measurementOfModel(st, "absent"); ok {
		t.Fatal("a model nothing has measured must not resolve to another model's figure")
	}
}

// statusProvider is the narrowest provider BenchmarkStatus needs: a
// store, and no job in flight. Deliberately not benchJobProvider — this
// asks what the STATUS says about persisted state, and bringing up a real
// engine adapter would only add ways for it to be flaky.
func statusProvider(t *testing.T) *agentInferenceProvider {
	t.Helper()
	return &agentInferenceProvider{store: catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))}
}

func seedStatus(t *testing.T, p *agentInferenceProvider, rec catalog.BenchmarkRecord, active string, entries ...catalog.VariantMeasurement) {
	t.Helper()
	if err := p.store.Update(func(s *catalog.State) {
		r := rec
		s.LastBenchmark = &r
		if active != "" {
			s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: active}
		}
		s.MeasuredVariants = map[string]catalog.VariantMeasurement{}
		for i, e := range entries {
			s.MeasuredVariants[string(rune('a'+i))] = e
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
}

// PRODUCT CONTRACT (waired-agent#971): the wire carries no figure at all
// when the last run was of a model this host no longer serves and nothing
// has timed the one it does. Absent, not zero — a zero would read as the
// slowest possible machine.
func TestBenchmarkStatusWithholdsAFigureAboutAnotherModel(t *testing.T) {
	p := statusProvider(t)
	seedStatus(t, p, catalog.BenchmarkRecord{
		Gen: 4, ModelID: "qwen3.5-9b", MeasuredTokps: 7.1, Method: "ollama_eval", MeasuredAt: benchAt(0),
	}, "qwen3.5-4b")

	got := p.BenchmarkStatus()
	if got.MeasuredTokps != 0 || got.ModelID != "" {
		t.Fatalf("status = %+v, want no figure — the run measured qwen3.5-9b and this host serves qwen3.5-4b", got)
	}
	// The RUN is still reported. Gen especially: the setup reconciler's
	// re-run guard is `bs.Gen < d.benchmarkGen`, so withholding it here
	// would re-run a measurement that has already answered.
	if got.Gen != 4 || got.State != management.BenchmarkStateDone {
		t.Fatalf("status = %+v, want gen 4 / done — the run itself did happen", got)
	}
}

// The other side of the same rule: once something HAS timed the served
// model, the status reports that, and names it.
func TestBenchmarkStatusReportsTheServedModel(t *testing.T) {
	p := statusProvider(t)
	seedStatus(t, p, catalog.BenchmarkRecord{
		Gen: 4, ModelID: "qwen3.5-9b", MeasuredTokps: 7.1, Method: "ollama_eval", MeasuredAt: benchAt(0),
	}, "qwen3.5-4b", catalog.VariantMeasurement{
		ModelID: "qwen3.5-4b", VariantID: "q4", MeasuredTokps: 19.8, Method: "ollama_eval", MeasuredAt: benchAt(30),
	})

	got := p.BenchmarkStatus()
	if got.ModelID != "qwen3.5-4b" || got.MeasuredTokps != 19.8 {
		t.Fatalf("status = %+v, want 19.8 tok/s named as qwen3.5-4b", got)
	}
	if got.Gen != 4 {
		t.Fatalf("status gen = %d, want 4 — the generation answers the request, not the model", got.Gen)
	}
}

// A host that upgraded mid-flight carries a record with no label. Its
// figure is kept, because withholding it would make an agent update look
// like the measurement had been lost.
func TestBenchmarkStatusKeepsAnUnlabelledFigure(t *testing.T) {
	p := statusProvider(t)
	seedStatus(t, p, catalog.BenchmarkRecord{
		Gen: 2, MeasuredTokps: 31.2, Method: "wall_clock", MeasuredAt: benchAt(0),
	}, "qwen3.5-4b")

	got := p.BenchmarkStatus()
	if got.MeasuredTokps != 31.2 {
		t.Fatalf("status = %+v, want the unlabelled figure kept", got)
	}
	if got.ModelID != "" {
		t.Fatalf("status model = %q, want empty — the record names none and guessing one is worse than saying nothing", got.ModelID)
	}
}
