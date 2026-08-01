package main

import (
	"context"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// bounceTestManifests gives two independent models so a test can hold one
// pull open while another finishes.
func bounceTestManifests() []catalog.Manifest {
	mk := func(id, tag string) catalog.Manifest {
		return catalog.Manifest{
			ModelID: id,
			Variants: []catalog.Variant{{
				VariantID: "q4", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				Source:         catalog.VariantSource{Type: catalog.SourceOllama, Tag: tag},
			}},
		}
	}
	return []catalog.Manifest{mk("model-a", "a:q4"), mk("model-b", "b:q4")}
}

// awaitModelState blocks until modelID reaches want, or fails the test.
func awaitModelState(t *testing.T, p *agentInferenceProvider, modelID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if modelStateOf(t, p, modelID).State == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("model %s never reached %q (last %q)", modelID, want, modelStateOf(t, p, modelID).State)
}

// bounceProvider leaves p.ollama nil on purpose. reconcileEngineServe
// returns on the nil-adapter guard BEFORE it consumes swapPending, so
// swapPending is a deterministic observable for "a bounce was requested"
// with no polling and no engine fixture.
func bounceProvider(t *testing.T, r *blockingRunner) *agentInferenceProvider {
	t.Helper()
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)
	p.manifests = bounceTestManifests()
	p.agentCtx = context.Background()
	return p
}

// THE #305d REGRESSION BAR. PRODUCT CONTRACT: a completed pull's engine
// bounce waits for the other downloads.
//
// The pending-swap bounce stops `ollama serve`, and `ollama pull` is a
// client of that server — so finishing model A's download killed model
// B's mid-flight and recorded B failed, for a reason that had nothing to
// do with B.
func TestRunPullJob_DefersTheSwapBounceWhileAnotherPullIsInFlight(t *testing.T) {
	r := newBlockingRunner(t)
	p := bounceProvider(t, r)

	// B is the sibling that must survive: start it and leave it blocked.
	if _, err := p.PullModel(context.Background(), "model-b"); err != nil {
		t.Fatalf("PullModel(model-b): %v", err)
	}
	r.awaitStarted(t)

	// A is the operator's swap target. Give it its own runner so it can
	// complete while B is still blocked.
	ra := newBlockingRunner(t)
	p.puller = newTestPuller(ra)
	a := "model-a"
	p.pendingSwapModel.Store(&a)
	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel(model-a): %v", err)
	}
	ra.awaitStarted(t)
	ra.releaseAll()

	// Wait for A's job to finish by observing the state it writes last.
	// waitForPulls() cannot be used here — B is still deliberately blocked.
	awaitModelState(t, p, "model-a", catalog.ModelStateReady)

	if p.swapPending.Load() {
		t.Fatal("the engine was bounced while another model was still downloading; " +
			"stopping the engine fails the sibling pull")
	}
	if !p.swapBounceDeferred.Load() {
		t.Fatal("the swap bounce was dropped instead of deferred")
	}

	r.releaseAll()
	p.waitForPulls()

	if !p.swapPending.Load() {
		t.Fatal("the deferred bounce never fired after the last pull left")
	}
}

// The deferral must not slow the ordinary case: a swap whose pull is the
// only one in flight bounces as soon as it lands.
func TestRunPullJob_FiresTheSwapBounceImmediatelyWhenItIsTheOnlyPull(t *testing.T) {
	r := newBlockingRunner(t)
	p := bounceProvider(t, r)

	a := "model-a"
	p.pendingSwapModel.Store(&a)
	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel(model-a): %v", err)
	}
	r.awaitStarted(t)
	r.releaseAll()
	p.waitForPulls()

	if !p.swapPending.Load() {
		t.Fatal("a lone swap pull did not bounce the engine on completion")
	}
	if p.swapBounceDeferred.Load() {
		t.Error("the deferred-bounce intent was left set after firing")
	}
}

// A pull that is not the swap target must not bounce anything.
func TestRunPullJob_UnrelatedPullDoesNotBounce(t *testing.T) {
	r := newBlockingRunner(t)
	p := bounceProvider(t, r)

	a := "model-a"
	p.pendingSwapModel.Store(&a)
	if _, err := p.PullModel(context.Background(), "model-b"); err != nil {
		t.Fatalf("PullModel(model-b): %v", err)
	}
	r.awaitStarted(t)
	r.releaseAll()
	p.waitForPulls()

	if p.swapPending.Load() || p.swapBounceDeferred.Load() {
		t.Fatal("an unrelated model's pull requested the swap bounce")
	}
	if p.pendingSwapModel.Load() == nil {
		t.Error("the pending swap target was cleared by an unrelated pull")
	}
}
