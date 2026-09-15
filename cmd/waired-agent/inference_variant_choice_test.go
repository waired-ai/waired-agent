package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// variantChoiceProvider is the pre-cache fixture (two builds of "heavy":
// mtp-q4 and q4) with a running fake engine, serving heavy/q4.
func variantChoiceProvider(t *testing.T) (*agentInferenceProvider, *scriptedRunner, *fakeSpawner) {
	t.Helper()
	r := &scriptedRunner{results: []error{nil}}
	p := precacheProvider(t, r)
	p.manifests = precacheVariantManifests()
	p.cfg.PreferredModelID = "heavy"
	if err := p.store.Update(func(s *catalog.State) {
		s.Models["heavy"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "heavy:8b"}
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "heavy", VariantID: "q4"}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	adapter, sp := newSwapTestAdapter(t)
	if err := adapter.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p.ollama = adapter
	return p, r, sp
}

// waitFor polls cond until it holds or the backstop passes, checking the
// row/Active invariant on every sample.
func waitForState(t *testing.T, p *agentInferenceProvider, what string, cond func(catalog.State) bool) catalog.State {
	t.Helper()
	deadline := time.Now().Add(waitBackstop)
	for {
		st, err := p.store.Load()
		if err != nil {
			t.Fatalf("store.Load: %v", err)
		}
		// THE INVARIANT (#656): Active never names a build the model's
		// row is not. The engine reads both together.
		if st.Active != nil {
			if ms, ok := st.Models[st.Active.ModelID]; ok && ms.VariantID != st.Active.VariantID {
				t.Fatalf("Active names %s/%s while the row serves %s — the engine would lose its tag (#656)",
					st.Active.ModelID, st.Active.VariantID, ms.VariantID)
			}
		}
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// PRODUCT CONTRACT (waired-agent#1348): choosing another build of the model
// this host serves downloads it BESIDE the served one — the row the engine
// reads is untouched while it arrives — and the switch moves the engine,
// the row and Active onto it together, keeping the replaced build on disk
// as a stored build the user can remove.
func TestSwapPreferredBuild_StagesAnotherBuildThenSwapsOntoIt(t *testing.T) {
	p, r, sp := variantChoiceProvider(t)
	spawnsBefore := sp.count()

	downloading, err := p.SwapPreferredBuild(context.Background(), "heavy", "mtp-q4", "")
	if err != nil {
		t.Fatalf("SwapPreferredBuild: %v", err)
	}
	if !downloading {
		t.Fatal("mtp-q4 is not on disk; downloading = false")
	}
	st, _ := p.store.Load()
	if ms := st.Models["heavy"]; ms.VariantID != "q4" || ms.OllamaTag != "heavy:8b" || ms.State != catalog.ModelStateReady {
		t.Errorf("served row changed at dispatch: %+v, want heavy/q4 ready", ms)
	}
	if sv, ok := st.StagedVariants["heavy"]; !ok || sv.VariantID != "mtp-q4" {
		t.Errorf("staged = %+v ok=%v, want the chosen build staged", sv, ok)
	}

	p.waitForPulls()
	if got := r.tagsSeen(); !slices.Contains(got, "heavy:8b-mtp") {
		t.Errorf("pulled tags = %v, want heavy:8b-mtp", got)
	}
	st = waitForState(t, p, "the swap onto mtp-q4", func(st catalog.State) bool {
		return st.Active != nil && st.Active.VariantID == "mtp-q4" && sp.count() > spawnsBefore
	})
	if ms := st.Models["heavy"]; ms.VariantID != "mtp-q4" || ms.OllamaTag != "heavy:8b-mtp" {
		t.Errorf("row after the swap = %+v, want heavy/mtp-q4 on heavy:8b-mtp", ms)
	}
	if _, staged := st.StagedVariants["heavy"]; staged {
		t.Error("the staged row survived the swap")
	}
	if got := storedVariants(st); len(got) != 1 || got[0].VariantID != "q4" {
		t.Errorf("stored builds after the swap = %+v, want the replaced q4", got)
	}
}

// PRODUCT CONTRACT: switching back to a build still on disk downloads
// nothing and moves the engine straight onto it.
func TestSwapPreferredBuild_BackToAStoredBuildNeedsNoDownload(t *testing.T) {
	p, r, sp := variantChoiceProvider(t)
	if err := p.store.Update(func(s *catalog.State) {
		s.Models["heavy"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "mtp-q4", OllamaTag: "heavy:8b-mtp"}
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "heavy", VariantID: "mtp-q4"}
		s.RetainedVariants = map[string][]catalog.ModelState{
			"heavy": {{State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "heavy:8b", SizeBytes: 5_000_000_000}},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	spawnsBefore := sp.count()

	downloading, err := p.SwapPreferredBuild(context.Background(), "heavy", "q4", "")
	if err != nil || downloading {
		t.Fatalf("SwapPreferredBuild = (%v, %v), want (false, nil) for a stored build", downloading, err)
	}
	st := waitForState(t, p, "the swap back onto q4", func(st catalog.State) bool {
		return st.Active != nil && st.Active.VariantID == "q4" && sp.count() > spawnsBefore
	})
	if got := r.calls(); got != 0 {
		t.Errorf("engine commands run = %d (%v), want 0: the build was on disk", got, r.tagsSeen())
	}
	if got := storedVariants(st); len(got) != 1 || got[0].VariantID != "mtp-q4" {
		t.Errorf("stored builds = %+v, want the replaced mtp-q4", got)
	}
}

// PRODUCT CONTRACT: a model named without a build keeps the build it
// already has on disk. Re-resolving downloaded another build of a model
// that was serving, and on a host that stepped down to a lighter build it
// pulled the heavier one straight back.
func TestPullModel_AModelOnDiskKeepsItsBuild(t *testing.T) {
	p, r, _ := variantChoiceProvider(t)
	if _, err := p.PullModel(context.Background(), "heavy"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()
	if got := r.tagsSeen(); len(got) != 1 || got[0] != "heavy:8b" {
		t.Errorf("pulled tags = %v, want the served heavy:8b refreshed", got)
	}
	st, _ := p.store.Load()
	if _, staged := st.StagedVariants["heavy"]; staged {
		t.Error("a refresh of the served build staged another one")
	}
}

// PRODUCT CONTRACT: removal takes only stored builds. A stale request
// naming the served build, or a build not on disk, removes nothing; the
// named stored build's weights go and its record with them.
func TestRemoveStoredVariants_RemovesOnlyStoredBuilds(t *testing.T) {
	p, r, _ := variantChoiceProvider(t)
	if err := p.store.Update(func(s *catalog.State) {
		s.RetainedVariants = map[string][]catalog.ModelState{
			"heavy": {{State: catalog.ModelStateReady, VariantID: "mtp-q4", OllamaTag: "heavy:8b-mtp"}},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p.RemoveStoredVariants(context.Background(), []string{"heavy/q4", "heavy/q2", "light/q4", "garbage"})
	if got := r.calls(); got != 0 {
		t.Fatalf("engine commands for requests naming no stored build = %v, want none", r.tagsSeen())
	}

	p.RemoveStoredVariants(context.Background(), []string{"heavy/mtp-q4"})
	if got := r.tagsSeen(); len(got) != 1 || got[0] != "heavy:8b-mtp" {
		t.Errorf("removed tags = %v, want heavy:8b-mtp", got)
	}
	st, _ := p.store.Load()
	if len(st.RetainedVariants) != 0 {
		t.Errorf("stored builds after removal = %+v, want none", st.RetainedVariants)
	}
	if ms := st.Models["heavy"]; ms.VariantID != "q4" || ms.State != catalog.ModelStateReady {
		t.Errorf("served row = %+v, want heavy/q4 untouched", ms)
	}
}

// PRODUCT CONTRACT (#641 for builds): deleting a model deletes its stored
// builds too, rather than leaving weights no record names.
func TestDeleteModel_TakesStoredBuildsWithIt(t *testing.T) {
	p, r, _ := variantChoiceProvider(t)
	if err := p.store.Update(func(s *catalog.State) {
		s.RetainedVariants = map[string][]catalog.ModelState{
			"heavy": {{State: catalog.ModelStateReady, VariantID: "mtp-q4", OllamaTag: "heavy:8b-mtp"}},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := p.DeleteModel(context.Background(), "heavy"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	got := r.tagsSeen()
	if !slices.Contains(got, "heavy:8b") || !slices.Contains(got, "heavy:8b-mtp") {
		t.Errorf("removed tags = %v, want both builds' weights", got)
	}
	st, _ := p.store.Load()
	if len(st.RetainedVariants) != 0 || len(st.StagedVariants) != 0 {
		t.Errorf("records left: retained %+v staged %+v", st.RetainedVariants, st.StagedVariants)
	}
}

// The setup row for a chosen build of a serving model is that build's
// download, not the served row's Ready.
func TestSetupModelState_ReportsTheChosenBuildsDownload(t *testing.T) {
	p, _, _ := variantChoiceProvider(t)
	if err := p.store.Update(func(s *catalog.State) {
		s.StagedVariants = map[string]catalog.ModelState{
			"heavy": {State: catalog.ModelStateDownloading, VariantID: "mtp-q4", OllamaTag: "heavy:8b-mtp"},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if state, _, _ := p.setupModelState("heavy"); state != catalog.ModelStateReady {
		t.Fatalf("with no build chosen the row is the served one; state = %q", state)
	}
	id := "heavy"
	p.preferredBuild.Store(&buildChoice{ModelID: "heavy", VariantID: "mtp-q4"})
	p.preferredOverride.Store(&id)
	if state, _, _ := p.setupModelState("heavy"); state != catalog.ModelStateDownloading {
		t.Errorf("with mtp-q4 chosen and downloading, state = %q, want downloading", state)
	}
	if p.setupBuildChosen("heavy", "mtp-q4", "") {
		t.Error("setupBuildChosen reports converged while the chosen build is not the row's")
	}
}

// PRODUCT CONTRACT (waired-agent#1348): another build or KV-cache type of
// the SAME model is a new instruction — the admission is per build — and a
// device already serving the chosen build is left alone.
func TestSetupDesiredBuildChangeOfTheSameModelIsApplied(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateReady, preferred: "qwen3-8b-instruct"}
	r := watchingReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()
	frame := func(variant, kv string) *signer.InferenceState {
		st := desiredFrame("", "qwen3-8b-instruct", 0)
		st.DesiredVariantID, st.DesiredKVCacheType = variant, kv
		return st
	}

	r.Apply(ctx, frame("", "")) // converged: nothing named, nothing chosen
	r.Apply(ctx, frame("q3-gguf", ""))
	r.Apply(ctx, frame("q3-gguf", "")) // converged on the chosen build
	r.Apply(ctx, frame("q3-gguf", "q8_0"))

	want := []string{"qwen3-8b-instruct|q3-gguf|", "qwen3-8b-instruct|q3-gguf|q8_0"}
	if !slices.Equal(f.buildApplies, want) {
		t.Errorf("applies = %v, want %v", f.buildApplies, want)
	}
}

// tagsSeen is every tag the runner was asked about, in order.
func (r *scriptedRunner) tagsSeen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tags...)
}
