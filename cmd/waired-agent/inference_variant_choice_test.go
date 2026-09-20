package main

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
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

// PRODUCT CONTRACT (waired#1387; found on hardware 2026-09-16): a build
// that finishes downloading after the choice moved off it is a stored build
// — reported, removable, and no swap onto it — rather than a staged row
// nothing will swap onto or report. Before this an 18 GB build a cancelled
// switch had started sat on disk out of every list, and its landing bounced
// an engine already serving the chosen build.
func TestPullLandingAfterTheChoiceMovedIsAStoredBuild(t *testing.T) {
	br := newBlockingRunner(t)
	p := precacheProvider(t, br)
	p.manifests = precacheVariantManifests()
	p.cfg.PreferredModelID = "heavy"
	if err := p.store.Update(func(s *catalog.State) {
		s.Models["heavy"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "heavy:8b"}
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "heavy", VariantID: "q4"}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	adapter, _ := newSwapTestAdapter(t)
	if err := adapter.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p.ollama = adapter

	if downloading, err := p.SwapPreferredBuild(context.Background(), "heavy", "mtp-q4", ""); err != nil || !downloading {
		t.Fatalf("SwapPreferredBuild = %v, %v; want a download", downloading, err)
	}
	br.awaitStarted(t)
	// The choice goes back to the served build while the download runs, the
	// way a cancelled switch leaves it when nothing stops the job.
	id := "heavy"
	p.preferredBuild.Store(&buildChoice{ModelID: id, VariantID: "q4"})
	p.preferredOverride.Store(&id)
	// The reconcile names whether it is a switch; a switch is what the
	// landing of an unchosen build must not ask for. Spawns are no signal
	// here: the fixture's first reconcile re-tunes and bounces either way.
	logs := &lockedLog{}
	p.logger = slog.New(slog.NewTextHandler(logs, nil))
	br.releaseAll()
	p.waitForPulls()

	st, _ := p.store.Load()
	if _, staged := st.StagedVariants["heavy"]; staged {
		t.Errorf("the landed build stayed staged: %+v", st.StagedVariants)
	}
	if got := storedVariants(st); len(got) != 1 || got[0].VariantID != "mtp-q4" {
		t.Errorf("stored builds = %+v, want the landed mtp-q4", got)
	}
	if ms := st.Models["heavy"]; ms.VariantID != "q4" || st.Active == nil || st.Active.VariantID != "q4" {
		t.Errorf("served build changed: row %+v active %+v", ms, st.Active)
	}
	// endPull turns a deferred bounce into a detached reconcile, so the
	// spawn count is read once that work has settled: two quiet samples.
	for quiet, i := 0, 0; quiet < 2 && i < 400; i++ {
		if p.detachedWorkQuiet() {
			quiet++
		} else {
			quiet = 0
		}
		time.Sleep(5 * time.Millisecond)
	}
	// endPull hands a deferred bounce to a detached reconcile; read the log
	// once that work has settled (two quiet samples).
	for quiet, i := 0, 0; quiet < 2 && i < 400; i++ {
		if p.detachedWorkQuiet() {
			quiet++
		} else {
			quiet = 0
		}
		time.Sleep(5 * time.Millisecond)
	}
	if strings.Contains(logs.String(), "switch=true") {
		t.Errorf("the landing of a build nobody chose switched the engine:\n%s", logs.String())
	}
}

// lockedLog is a log sink the detached reconcile and the test can share.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(b)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
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

	// The trailing 0 is the serving window: nothing asked for the long one
	// here, and the coding window is what 0 means (waired-ai/waired#1456).
	want := []string{"qwen3-8b-instruct|q3-gguf||0", "qwen3-8b-instruct|q3-gguf|q8_0|0"}
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
