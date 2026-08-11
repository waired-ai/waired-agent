package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// PRODUCT CONTRACT (waired-agent#633, waired-agent#641): cancelling a
// download stops it and leaves the host where it was before the pull
// started.
//
// The defect this pins: nothing could stop a pull. `models rm` looked as
// though it did, because models.downloads is derived from state.Models
// alone, so deleting the row removed the only sign the job was running —
// while the job kept fetching and wrote the model back as READY. The
// operator's removal did not stick, and a model the host had judged unfit
// went back out to the mesh.
func TestCancelPull_StopsTheJobAndLeavesNoRecord(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)
	p.agentCtx = context.Background()

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	r.awaitStarted(t)

	res, err := p.CancelPull(context.Background(), "dense-mtp")
	if err != nil {
		t.Fatalf("CancelPull: %v", err)
	}
	if res.Status != pullCancelCancelled {
		t.Fatalf("status = %q, want %q", res.Status, pullCancelCancelled)
	}
	if res.JobID == "" {
		t.Error("cancelling a running job reported no job id")
	}
	p.waitForPulls()

	// The whole point: NOT ready. Without the fix the job runs to
	// completion and this is catalog.ModelStateReady.
	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if ms, ok := st.Models["dense-mtp"]; ok {
		t.Fatalf("cancelled model still has a record: state=%q tag=%q", ms.State, ms.OllamaTag)
	}
	// And it must not be recorded as a failure either — nothing failed.
	if ms := st.Models["dense-mtp"]; ms.Error != "" {
		t.Errorf("cancelled model carries an error text %q", ms.Error)
	}
}

// PRODUCT CONTRACT (waired-agent#633): "stop this" and "there was nothing
// to stop" leave the host in the same state, which is the state the
// caller asked for — so a model with no download in flight is reported,
// not rejected. `waired models cancel` renders it as a plain sentence and
// exits 0, matching `models ls` on an empty catalog.
func TestCancelPull_NothingInFlightIsNotAnError(t *testing.T) {
	p := pullGateProviderWithRunner(t, pullGateManifest(false), &rmRunner{})

	res, err := p.CancelPull(context.Background(), "dense-mtp")
	if err != nil {
		t.Fatalf("CancelPull on an idle model: %v", err)
	}
	if res.Status != pullCancelNotDownloading {
		t.Fatalf("status = %q, want %q", res.Status, pullCancelNotDownloading)
	}
	if res.JobID != "" {
		t.Errorf("job id = %q, want empty — there is no job to name", res.JobID)
	}
}

// PRODUCT CONTRACT (waired-agent#641): `models rm` during a download stops
// it. This is the sequence the issue reported, at its reported timing —
// pull, then rm five seconds later, then the model returns as ready ~20
// minutes on.
//
// It also closes the failure waired-agent#671 introduced: `ollama rm`
// exits non-zero for a tag whose manifest was never written, so deleting
// a model mid-pull could return an error and keep the record. Stopping
// the job first means there is no half-written tag to trip over.
func TestDeleteModel_StopsTheDownloadItWasStillFetching(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)
	p.agentCtx = context.Background()

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	r.awaitStarted(t)

	if err := p.DeleteModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("DeleteModel during a pull: %v", err)
	}
	// Let anything still downloading SUCCEED. This is what makes the test
	// non-vacuous: without the cancel the job is still blocked here, and
	// releasing it writes the model back as ready — the issue's "some time
	// later, without anyone asking for it, the model is back". With the
	// cancel the job unwound before DeleteModel returned, so this is a
	// no-op.
	r.releaseAll()
	p.waitForPulls()

	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if ms, ok := st.Models["dense-mtp"]; ok {
		t.Fatalf("model came back after rm: state=%q", ms.State)
	}
}

// PRODUCT CONTRACT (waired-agent#641, waired-agent#656): deleting the
// active model clears the ActiveSelection that named it.
//
// An Active pointing at a record that no longer exists is not merely
// stale. activeEngineTag resolves Active THROUGH state.Models, so it
// answers ""; narrowPublishedModels reads an empty advertise name as
// "nothing to enforce" and passes the probe result through unmodified;
// and one 5s tick later the host advertises every tag /api/tags reports —
// the host-speed probe model included. That is #656's symptom, reached
// through a door #670 did not close.
func TestDeleteModel_ClearsAnActiveSelectionThatNamedIt(t *testing.T) {
	r := &rmRunner{}
	p, store := deleteModelProvider(t, r, map[string]catalog.ModelState{
		"qwen3.5-4b": {OllamaTag: "qwen3.5:4b-q4_K_M", VariantID: "q4", State: catalog.ModelStateReady},
	})
	st, _ := store.Load()
	st.Active = &catalog.ActiveSelection{
		Runtime: catalog.RuntimeOllama, ModelID: "qwen3.5-4b", VariantID: "q4",
	}
	if err := store.Save(st); err != nil {
		t.Fatalf("seed active: %v", err)
	}

	if err := p.DeleteModel(context.Background(), "qwen3.5-4b"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	after, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if after.Active != nil {
		t.Fatalf("Active still names %q after its model was deleted", after.Active.ModelID)
	}
	// The advertise name is the observable consequence; assert it directly
	// rather than only the field, so the test fails for the reason the
	// issue reported.
	if tag, ok := advertisedEngineTag(after); ok {
		t.Fatalf("advertised tag = %q after deleting the active model, want none", tag)
	}
}

// Records today's behaviour: deleting a model that is NOT the active one
// leaves the ActiveSelection alone. The clear above is targeted, not a
// blanket reset.
func TestDeleteModel_KeepsAnActiveSelectionForAnotherModel(t *testing.T) {
	r := &rmRunner{}
	p, store := deleteModelProvider(t, r, map[string]catalog.ModelState{
		"qwen3.5-4b": {OllamaTag: "qwen3.5:4b-q4_K_M", VariantID: "q4", State: catalog.ModelStateReady},
		"qwen3.5-2b": {OllamaTag: "qwen3.5:2b-q4_K_M", VariantID: "q4", State: catalog.ModelStateReady},
	})
	st, _ := store.Load()
	st.Active = &catalog.ActiveSelection{
		Runtime: catalog.RuntimeOllama, ModelID: "qwen3.5-4b", VariantID: "q4",
	}
	if err := store.Save(st); err != nil {
		t.Fatalf("seed active: %v", err)
	}

	if err := p.DeleteModel(context.Background(), "qwen3.5-2b"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	after, _ := store.Load()
	if after.Active == nil || after.Active.ModelID != "qwen3.5-4b" {
		t.Fatalf("Active = %+v, want the untouched qwen3.5-4b selection", after.Active)
	}
}

// PRODUCT CONTRACT (waired-agent#641): a removal survives a restart.
//
// bootstrapPreferredModel re-pulls the preferred model when it is
// missing, and a model someone just deleted is missing — which is the
// issue's "several daemon restarts in between, and it came back".
func TestDeleteModel_ClearsThePreferredModelRecord(t *testing.T) {
	r := &rmRunner{}
	p, _ := deleteModelProvider(t, r, map[string]catalog.ModelState{
		"qwen3.5-4b": {OllamaTag: "qwen3.5:4b-q4_K_M", State: catalog.ModelStateReady},
	})
	prefPath := filepath.Join(t.TempDir(), "preferred-model.json")
	if err := agentconfig.SavePreference(prefPath, agentconfig.Preference{
		ModelID: "qwen3.5-4b",
		Source:  agentconfig.PreferenceSourceOperator,
	}); err != nil {
		t.Fatalf("seed preference: %v", err)
	}
	p.preferencePath = prefPath
	id := "qwen3.5-4b"
	p.preferredOverride.Store(&id)

	if err := p.DeleteModel(context.Background(), "qwen3.5-4b"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	pref, ok, err := agentconfig.LoadPreference(prefPath)
	if err != nil {
		t.Fatalf("load preference: %v", err)
	}
	if ok && pref.ModelID == "qwen3.5-4b" {
		t.Fatal("preferred-model.json still names the deleted model; the next boot would download it again")
	}
	if got := p.effectivePreferredModelID(); got != "" {
		t.Fatalf("in-process preferred model = %q after deleting it, want empty", got)
	}
}

// Records today's behaviour: a "run without a local model" record (#586)
// is an answer to a different question and is not touched by deleting
// some other model.
func TestDeleteModel_LeavesANoneRecordAlone(t *testing.T) {
	r := &rmRunner{}
	p, _ := deleteModelProvider(t, r, map[string]catalog.ModelState{
		"qwen3.5-4b": {OllamaTag: "qwen3.5:4b-q4_K_M", State: catalog.ModelStateReady},
	})
	prefPath := filepath.Join(t.TempDir(), "preferred-model.json")
	if err := agentconfig.SavePreference(prefPath, agentconfig.Preference{
		None:   true,
		Source: agentconfig.PreferenceSourceOperator,
	}); err != nil {
		t.Fatalf("seed preference: %v", err)
	}
	p.preferencePath = prefPath

	if err := p.DeleteModel(context.Background(), "qwen3.5-4b"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	pref, ok, err := agentconfig.LoadPreference(prefPath)
	if err != nil || !ok || !pref.None {
		t.Fatalf("None record lost: pref=%+v ok=%v err=%v", pref, ok, err)
	}
}

// PRODUCT CONTRACT (waired-agent#671): a cancel that arrives after the
// weights have landed does NOT drop the record. Answering the race the
// other way would leave bytes on disk that nothing can name — the defect
// #671 fixed on the delete path, reappearing on the cancel path.
//
// Driven through settleCancelledPull directly: the window it decides is
// between "the job wrote ready" and "the job returned", which no fake can
// hold open from the outside.
func TestSettleCancelledPull_KeepsAModelThatBecameReadyFirst(t *testing.T) {
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	st, _ := store.Load()
	st.Models = map[string]catalog.ModelState{
		"dense": {OllamaTag: "dense:q4", State: catalog.ModelStateReady},
	}
	if err := store.Save(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := &agentInferenceProvider{store: store, logger: slog.Default()}

	job := &pullJob{jobID: "job_1", modelID: "dense", tag: "dense:q4", stop: newPullStop(func() {})}
	job.stop.requested.Store(true)
	p.settleCancelledPull(job)

	after, _ := store.Load()
	if ms, ok := after.Models["dense"]; !ok || ms.State != catalog.ModelStateReady {
		t.Fatalf("a model that landed before the cancel was dropped: %+v ok=%v", ms, ok)
	}
}

// Records today's behaviour: a job nobody cancelled is left entirely
// alone, so the cleanup cannot delete a healthy row on the normal path.
func TestSettleCancelledPull_IgnoresAJobThatWasNotCancelled(t *testing.T) {
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	st, _ := store.Load()
	st.Models = map[string]catalog.ModelState{
		"dense": {OllamaTag: "dense:q4", State: catalog.ModelStateFailed},
	}
	if err := store.Save(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := &agentInferenceProvider{store: store, logger: slog.Default()}

	p.settleCancelledPull(&pullJob{jobID: "job_1", modelID: "dense", stop: newPullStop(func() {})})

	after, _ := store.Load()
	if _, ok := after.Models["dense"]; !ok {
		t.Fatal("an uncancelled job's record was removed")
	}
}

// Records today's behaviour: CancelPull returns rather than hanging when
// the caller's context is already done. A wedged job must not hold a
// management request open past the CLI's own timeout.
func TestCancelPull_ReturnsWhenTheCallerGoesAway(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)
	p.agentCtx = context.Background()
	// Keep the job from unwinding: the runner ignores its own ctx only
	// after release, so hold the settle window open by shrinking it and
	// cancelling the caller instead.
	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	r.awaitStarted(t)

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := p.CancelPull(callerCtx, "dense-mtp"); err != nil {
			t.Errorf("CancelPull: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CancelPull did not return after its caller went away")
	}
	p.waitForPulls()
}
