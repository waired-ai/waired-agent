package main

import (
	"context"
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// THE #305b REGRESSION BAR. PRODUCT CONTRACT: at most one pull per model
// is in flight, and a second dispatch joins it.
//
// Six dispatchers funnel into PullModel with no coordination, and variant
// choice is engine-version dependent and fails closed: with the version
// still unknown a floored variant is skipped for the plain one, and once
// the engine reports its version the same model resolves to the mtp tag.
// Two dispatchers seconds apart therefore downloaded two different tags of
// one logical model — the 16.3 + 18.0 GB the rc7 host reported as 33.9 GB.
func TestPullModel_SecondDispatchJoinsTheInFlightJob(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)

	first, err := p.PullModel(context.Background(), "dense-mtp")
	if err != nil {
		t.Fatalf("first PullModel: %v", err)
	}
	r.awaitStarted(t)

	second, err := p.PullModel(context.Background(), "dense-mtp")
	if err != nil {
		t.Fatalf("second PullModel: %v", err)
	}
	if second.JobID != first.JobID {
		t.Errorf("joined job id = %q, want the in-flight job %q", second.JobID, first.JobID)
	}
	if second.ModelID != first.ModelID {
		t.Errorf("joined model id = %q, want %q", second.ModelID, first.ModelID)
	}

	r.releaseAll()
	p.waitForPulls()

	if got := r.calls(); got != 1 {
		t.Fatalf("pulls executed = %d (%v), want 1", got, r.pulledTags())
	}
}

// The 16.3+18.0 GB defect stated as state: the joiner must not rewrite the
// row the running job already stamped, which is what reset a downloading
// model back to queued and swapped its tag mid-flight.
func TestPullModel_JoinDoesNotRewriteTheRecordedVariant(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("first PullModel: %v", err)
	}
	r.awaitStarted(t)
	before := modelStateOf(t, p, "dense-mtp")

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("second PullModel: %v", err)
	}
	after := modelStateOf(t, p, "dense-mtp")

	if after.VariantID != before.VariantID || after.OllamaTag != before.OllamaTag {
		t.Errorf("join rewrote the recorded variant: %+v -> %+v", before, after)
	}
	if after.State != before.State {
		t.Errorf("join moved the state %q -> %q", before.State, after.State)
	}

	r.releaseAll()
	p.waitForPulls()
}

// PRODUCT CONTRACT: the pin is job-scoped, not permanent. A SEQUENTIAL
// retry must be free to re-resolve the variant — that is how a host whose
// engine version was unknown at boot picks up the better tag afterwards.
// Permanently pinning the recorded tag would freeze it on the degraded
// variant forever.
func TestPullModel_SequentialRetryRedereivesTheVariant(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)

	first, err := p.PullModel(context.Background(), "dense-mtp")
	if err != nil {
		t.Fatalf("first PullModel: %v", err)
	}
	r.awaitStarted(t)
	r.releaseAll()
	p.waitForPulls()

	second, err := p.PullModel(context.Background(), "dense-mtp")
	if err != nil {
		t.Fatalf("second PullModel: %v", err)
	}
	p.waitForPulls()

	if second.JobID == first.JobID {
		t.Error("a sequential retry reused the finished job's id; the registry entry was not released")
	}
	if got := r.calls(); got != 2 {
		t.Fatalf("pulls executed = %d (%v), want 2", got, r.pulledTags())
	}
}

// A failed pull must release its slot, or the model becomes un-pullable
// for the life of the process — the opposite of what #305 is fixing.
func TestPullModel_FailedPullReleasesTheInFlightEntry(t *testing.T) {
	p := pullGateProviderWithRunner(t, pullGateManifest(false), failingRunner{})

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("first PullModel: %v", err)
	}
	p.waitForPulls()
	if got := modelStateOf(t, p, "dense-mtp").State; got != catalog.ModelStateFailed {
		t.Fatalf("state after a failed pull = %q, want %q", got, catalog.ModelStateFailed)
	}

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("retry PullModel: %v", err)
	}
	p.waitForPulls()
}

// A dispatch that never starts a goroutine must release too. The
// unsupported-source branch returns before anything is spawned, so a leak
// here would be invisible until the next pull of that model silently
// joined a job that does not exist.
func TestPullModel_DispatchErrorReleasesTheInFlightEntry(t *testing.T) {
	m := catalog.Manifest{
		ModelID: "weird-source",
		Variants: []catalog.Variant{{
			VariantID: "v1", Format: catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			Source:         catalog.VariantSource{Type: "s3", Tag: "weird:v1"},
		}},
	}
	p := pullGateProviderWithRunner(t, m, noopRunner{})

	for i := range 2 {
		_, err := p.PullModel(context.Background(), "weird-source")
		if !errors.Is(err, errUnsupportedSource) {
			t.Fatalf("PullModel #%d error = %v, want errUnsupportedSource "+
				"(a released slot must let the second attempt reach the same branch)", i, err)
		}
	}
}

// The duplicate that happens on every affected boot, not a corner case.
// bootstrapAfterEngineStart runs bootstrapBundledModel and then
// bootstrapPreferredModel on the same goroutine; when the operator's
// preference names the bundled model, the first writes `queued` and
// dispatches, and the second sees "not ready" and dispatches again for the
// same model id. Both pulls then fight over one state row, and whichever
// finishes first wipes the survivor's byte progress.
func TestBootstrap_PreferredEqualsBundledDispatchesOnePull(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)
	p.cfg.BundledModelID = "dense-mtp"
	p.cfg.PreferredModelID = "dense-mtp"
	p.cfg.PullOnStartup = true

	ctx := context.Background()
	p.bootstrapBundledModel(ctx)
	r.awaitStarted(t)
	p.bootstrapPreferredModel(ctx)

	r.releaseAll()
	p.waitForPulls()

	if got := r.calls(); got != 1 {
		t.Fatalf("pulls executed across both bootstraps = %d (%v), want 1", got, r.pulledTags())
	}
}
