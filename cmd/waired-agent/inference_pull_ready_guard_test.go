package main

import (
	"context"
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// readyThenFailRunner makes the model ready *while the pull is running*
// and then fails, which is the window the dispatch-time `refresh` flag
// structurally cannot see.
type readyThenFailRunner struct {
	p       *agentInferenceProvider
	modelID string
}

func (r readyThenFailRunner) Run(_ context.Context, _ string, _, _ []string, _ func(string)) error {
	_ = r.p.store.Update(func(s *catalog.State) {
		m := s.Models[r.modelID]
		m.State = catalog.ModelStateReady
		s.Models[r.modelID] = m
	})
	return errors.New("simulated registry throttle")
}

// THE #305c REGRESSION BAR. PRODUCT CONTRACT: nothing a pull does may take
// a ready model down.
//
// `refresh` was computed at DISPATCH time, so a job dispatched while the
// model was merely downloading carried refresh=false for its whole life
// and overwrote a sibling's completed `ready` with `failed`. That is how
// the rc7 node page ended up serving READY with three tags present while
// the wizard showed a failed download. Re-reading the state inside the
// store's own lock is the only trustworthy input.
func TestRunPullJob_FailureDoesNotDowngradeAModelMadeReadyMeanwhile(t *testing.T) {
	p := pullGateProviderWithRunner(t, pullGateManifest(false), nil)
	p.puller = newTestPuller(readyThenFailRunner{p: p, modelID: "dense-mtp"})

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	ms := modelStateOf(t, p, "dense-mtp")
	if ms.State != catalog.ModelStateReady {
		t.Fatalf("state = %q, want %q (a failing pull must not downgrade a model another job made ready)",
			ms.State, catalog.ModelStateReady)
	}
	if ms.Error == "" {
		t.Error("the failure was not recorded; the error text is the only observability left")
	}
}

// PRODUCT CONTRACT: state records the variant that was actually pulled.
//
// The success write set State/Error/PulledAt and never VariantID or
// OllamaTag, and the refresh path writes nothing up front — so a refresh
// pull that resolved a NEW variant downloaded the new blobs and left state
// pointing at the old tag. That tag is load-bearing: it is the model name
// the gateway puts on the wire, what the mesh advertises, and what the
// tuning verify targets. An engine upgrade that unlocked a better variant
// therefore spent the bandwidth and changed nothing.
func TestRunPullJob_SuccessRecordsTheVariantItActuallyPulled(t *testing.T) {
	p := pullGateProviderWithRunner(t, pullGateManifest(false), noopRunner{})

	// Seed a ready model on the OLD variant, with a derived batch tag
	// hanging off it, so the pull below takes the refresh path.
	if err := p.store.Update(func(s *catalog.State) {
		s.Models["dense-mtp"] = catalog.ModelState{
			VariantID: "stale",
			OllamaTag: "dense:stale",
			State:     catalog.ModelStateReady,
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	ms := modelStateOf(t, p, "dense-mtp")
	if ms.State != catalog.ModelStateReady {
		t.Fatalf("state = %q, want %q", ms.State, catalog.ModelStateReady)
	}
	if ms.VariantID != "q4" || ms.OllamaTag != "dense:q4" {
		t.Errorf("recorded variant = %q/%q, want q4/dense:q4 (the variant the pull actually fetched)",
			ms.VariantID, ms.OllamaTag)
	}
}
