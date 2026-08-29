package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// TestSpeedGate_ClearsOnEveryPath is the property that keeps the
// readiness gate from taking a host out of the mesh for the life of the
// daemon. Owner ruling, 2026-08-29 (waired-agent#1127): the gate says
// "not yet", never "not ever" — a host that CANNOT be measured still
// serves.
func TestSpeedGate_ClearsOnEveryPath(t *testing.T) {
	cases := []struct {
		name  string
		steps func(*agentInferenceProvider)
	}{
		{
			name: "a completed measurement",
			steps: func(p *agentInferenceProvider) {
				p.measureSpeedForMesh(context.Background(), PrefillDeps{
					Now: time.Now, Logger: slog.Default(),
					Sample: func(context.Context, int) (float64, int, error) {
						return 690, 51 * 80, nil
					},
				})
			},
		},
		{
			name: "an engine that cannot answer",
			steps: func(p *agentInferenceProvider) {
				p.measureSpeedForMesh(context.Background(), PrefillDeps{
					Now: time.Now, Logger: slog.Default(),
					Sample: func(context.Context, int) (float64, int, error) {
						return 0, 0, context.DeadlineExceeded
					},
				})
			},
		},
		{
			name: "an engine kind with no sampler at all",
			steps: func(p *agentInferenceProvider) {
				p.measureSpeedForMesh(context.Background(), PrefillDeps{
					EngineKind: "some-future-engine", EnginePort: 1, EngineModel: "m",
					Now: time.Now, Logger: slog.Default(),
				})
			},
		},
		{
			name: "a served window that holds no rung",
			steps: func(p *agentInferenceProvider) {
				p.measureSpeedForMesh(context.Background(), PrefillDeps{
					AppliedWindow: 4096, Now: time.Now, Logger: slog.Default(),
					Sample: func(context.Context, int) (float64, int, error) { return 690, 4000, nil },
				})
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &agentInferenceProvider{logger: slog.Default()}
			p.beginSpeedMeasurement()
			if !p.IsMeasuringSpeed() {
				t.Fatal("the gate should be armed")
			}
			c.steps(p)
			if p.IsMeasuringSpeed() {
				t.Error("the gate is still armed; this host would refuse mesh traffic forever")
			}
		})
	}
}

func TestIsMeasuringSpeed_NilProviderIsNotAGate(t *testing.T) {
	var p *agentInferenceProvider
	if p.IsMeasuringSpeed() {
		t.Error("a nil provider must not gate anything")
	}
}

// TestPrefillRateForHealth_WithholdsAMeasurementOfAnotherModel: the rate
// is keyed to the variant it was taken on. A model switch through /model,
// the tray or the desired-model channel makes the old figure a number for
// a model this host no longer runs, and publishing it would hand every
// requester a wrong answer. Unmeasured is safe; wrong is not.
func TestPrefillRateForHealth_WithholdsAMeasurementOfAnotherModel(t *testing.T) {
	measured := PrefillMeasurement{
		VariantID: "q4-gguf",
		Rungs:     []PrefillRung{{Depth: 4096, Tokps: 690, Samples: 2, SpreadPct: 3}},
	}

	t.Run("no store to check against, so the rate stands", func(t *testing.T) {
		p := &agentInferenceProvider{}
		p.SetLastPrefill(measured)
		got := p.PrefillRateForHealth()
		if got == nil || len(got.Rungs) != 1 || got.VariantID != "q4-gguf" {
			t.Fatalf("got %+v, want the measurement", got)
		}
		if got.Rungs[0].Tokps != 690 || got.Rungs[0].Depth != 4096 {
			t.Errorf("rung = %+v, want the measured one", got.Rungs[0])
		}
	})

	t.Run("nothing measured publishes nothing", func(t *testing.T) {
		p := &agentInferenceProvider{}
		if got := p.PrefillRateForHealth(); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})

	t.Run("a failed measurement publishes nothing", func(t *testing.T) {
		p := &agentInferenceProvider{}
		p.SetLastPrefill(PrefillMeasurement{VariantID: "q4-gguf", Failed: true, Err: "out of memory"})
		if got := p.PrefillRateForHealth(); got != nil {
			t.Errorf("got %+v, want nil — a failure is not a rate", got)
		}
	})

	t.Run("nil provider", func(t *testing.T) {
		var p *agentInferenceProvider
		if got := p.PrefillRateForHealth(); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})

	withActiveVariant := func(t *testing.T, variantID string) *agentInferenceProvider {
		t.Helper()
		store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
		if err := store.Update(func(s *catalog.State) {
			if s.Models == nil {
				s.Models = map[string]catalog.ModelState{}
			}
			s.Models["qwen3-8b"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: variantID}
			s.Active = &catalog.ActiveSelection{
				Runtime: catalog.RuntimeOllama, ModelID: "qwen3-8b", VariantID: variantID, DecidedBy: "user",
			}
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}
		p := &agentInferenceProvider{store: store}
		p.SetLastPrefill(measured)
		return p
	}

	t.Run("the served variant still matches", func(t *testing.T) {
		if got := withActiveVariant(t, "q4-gguf").PrefillRateForHealth(); got == nil {
			t.Fatal("got nil, want the measurement")
		}
	})

	t.Run("the host switched model, so the rate is withheld", func(t *testing.T) {
		if got := withActiveVariant(t, "q8-gguf").PrefillRateForHealth(); got != nil {
			t.Errorf("got %+v, want nil: this figure describes a model this host no longer runs", got)
		}
	})
}

// TestMaybeMeasureSpeed_WaitsForTheEngineRatherThanRacingIt is the fix
// for a wiring hole found on real hardware: a one-shot measurement hung
// off the boot benchmark inherits that benchmark's EngineReady race, and
// on a vLLM host — which takes about a minute to come up — that race was
// measured as 5 completions in 82 boots.
func TestMaybeMeasureSpeed_WaitsForTheEngineRatherThanRacingIt(t *testing.T) {
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{
			"qwen3-8b": {State: catalog.ModelStateReady, VariantID: "q4-gguf"},
		}
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: "qwen3-8b", VariantID: "q4-gguf", DecidedBy: "user",
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	attempts := 0
	depsFor := func() PrefillDeps {
		return PrefillDeps{
			Now: time.Now, Logger: slog.Default(),
			Sample: func(context.Context, int) (float64, int, error) {
				attempts++
				return 690, 51 * 80, nil
			},
		}
	}

	engineUp := false
	p := &agentInferenceProvider{
		logger: slog.Default(),
		store:  store,
		// The engine is not up yet — the state the boot race loses to.
		isInferenceDisabled: func() bool { return !engineUp },
	}
	p.beginSpeedMeasurement()

	p.maybeMeasureSpeed(context.Background(), depsFor)
	if attempts != 0 {
		t.Fatalf("measured %d times against an engine that is not up", attempts)
	}
	if p.IsMeasuringSpeed() {
		t.Error("with the engine down, EngineReady already refuses peer traffic; " +
			"holding the gate too would be a latch nothing clears")
	}

	// The engine comes up. EngineReady also needs a serving adapter, which
	// this fixture has none of, so the round still declines — what is
	// pinned here is that the loop keeps ASKING rather than having spent
	// its one chance.
	engineUp = true
	p.maybeMeasureSpeed(context.Background(), depsFor)
	p.maybeMeasureSpeed(context.Background(), depsFor)
}

// TestSpeedMeasuredFor_OneAttemptPerVariant: a failed attempt is not
// retried every tick — that would saturate the engine of a host that
// cannot answer — and a model change is what earns another.
func TestSpeedMeasuredFor_OneAttemptPerVariant(t *testing.T) {
	p := &agentInferenceProvider{}
	p.SetLastPrefill(PrefillMeasurement{VariantID: "q4-gguf", Failed: true, Err: "out of memory"})
	if !p.speedMeasuredFor("q4-gguf") {
		t.Error("a failed attempt still counts as attempted for that variant")
	}
	if p.speedMeasuredFor("q8-gguf") {
		t.Error("a different variant has not been attempted")
	}
	var nilProv *agentInferenceProvider
	if !nilProv.speedMeasuredFor("q4-gguf") {
		t.Error("a nil provider must not ask for a measurement")
	}
}

func TestActiveVariantID(t *testing.T) {
	if got := (*agentInferenceProvider)(nil).activeVariantID(); got != "" {
		t.Errorf("nil provider = %q, want empty", got)
	}
	p := &agentInferenceProvider{}
	if got := p.activeVariantID(); got != "" {
		t.Errorf("no store = %q, want empty", got)
	}
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	p = &agentInferenceProvider{store: store}
	if got := p.activeVariantID(); got != "" {
		t.Errorf("no active selection = %q, want empty", got)
	}
	if err := store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{}
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "m", VariantID: "v", DecidedBy: "user"}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := p.activeVariantID(); got != "v" {
		t.Errorf("activeVariantID = %q, want v", got)
	}
}
