package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeSetupProvider implements setupProvider with scriptable state. Its
// benchmark start flips the status to running, mirroring the real
// single-flight job so idempotency tests reflect the production contract.
//
// setupEngineState RECORDS its arguments and can answer differently per
// engine (CLAUDE.md §Test discipline). It used to accept
// `(context.Context, string)` with both parameters unnamed and discarded,
// which made the whole of #179 inexpressible: the real predicate answers
// "installed" per engine kind by stat-ing the state dir, and a fake that
// cannot see which engine it was asked about cannot distinguish "the
// engine waired installed" from "some engine on PATH" — the exact
// distinction that failed.
type fakeSetupProvider struct {
	mu sync.Mutex
	// engineStateCalls records every setupEngineState call in order.
	engineStateCalls []engineStateCall
	// engineHealthCalls records every setupEngineHealth call, for the same
	// reason: the real implementation answers per engine kind and returns ""
	// for one it is not serving, so a fake that dropped the engine could not
	// express "the latch belongs to the other engine".
	engineHealthCalls []engineStateCall
	// perEngine overrides the default answer for one engine kind.
	perEngine       map[string]engineState
	engineInstalled bool
	engineReady     bool
	// engineLatched / engineLastErr script setupEngineHealth: the daemon has
	// given up restarting this engine, and the reason it recorded.
	engineLatched  bool
	engineLastErr  string
	modelState     string
	modelCompleted int64
	modelTotal     int64
	modelErr       string
	bench          management.BenchmarkStatusResponse
	benchStarts    []int
	// engineStarts records the reason of every startSetupEngine call, in
	// order. The reason is kept rather than a bare count so a test can
	// tell the executor-done trigger from the engine-appeared one (#304).
	engineStarts []string
	pulls        []string
	pullErr      error
	stateDir     string

	// applies records every model the reconciler asked to APPLY, in
	// order. Separate from pulls because the two are different
	// operations: applying makes the device serve the model, pulling only
	// fetches its weights, and #230 was the gap between them.
	applies []string
	// applyErr, when set, is returned by setupApplyModel. errSwapNeedsRestart
	// exercises the cross-engine fallback to a bare pull.
	applyErr error
	// preferred is what the device is currently set to serve, i.e. what
	// the real provider reads back from effectivePreferredModelID. A
	// successful apply sets it, which is what makes the reconciler's
	// convergence observable rather than flag-based.
	preferred string
}

// engineStateCall is one observed setupEngineState call. The context is
// recorded as "was one passed" rather than kept: the real implementation
// no longer profiles and ignores it (setup_desired.go), but a fake that
// silently accepted a nil context would hide a caller that stopped
// threading one.
type engineStateCall struct {
	Engine string
	HadCtx bool
}

type engineState struct{ installed, ready bool }

func (f *fakeSetupProvider) setupEngineState(ctx context.Context, engine string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.engineStateCalls = append(f.engineStateCalls, engineStateCall{Engine: engine, HadCtx: ctx != nil})
	if st, ok := f.perEngine[engine]; ok {
		return st.installed, st.ready
	}
	return f.engineInstalled, f.engineReady
}

func (f *fakeSetupProvider) setupEngineHealth(ctx context.Context, engine string) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.engineHealthCalls = append(f.engineHealthCalls, engineStateCall{Engine: engine, HadCtx: ctx != nil})
	return f.engineLatched, f.engineLastErr
}

// healthAsked is the ordered list of engine kinds whose health was probed.
func (f *fakeSetupProvider) healthAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.engineHealthCalls {
		out = append(out, c.Engine)
	}
	return out
}

// enginesAsked is the ordered list of engine kinds the reconciler asked
// about, deduplicated to first appearance.
func (f *fakeSetupProvider) enginesAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	for _, c := range f.engineStateCalls {
		if !seen[c.Engine] {
			seen[c.Engine] = true
			out = append(out, c.Engine)
		}
	}
	return out
}

// setEngineFor scripts one engine kind independently of the default,
// so a test can say "the bundled engine is installed, the other one is
// not" — which is what #179 is about.
func (f *fakeSetupProvider) setEngineFor(engine string, installed, ready bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.perEngine == nil {
		f.perEngine = map[string]engineState{}
	}
	f.perEngine[engine] = engineState{installed: installed, ready: ready}
}

func (f *fakeSetupProvider) setupStateDir() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateDir
}

func (f *fakeSetupProvider) setupModelState(string) (string, int64, int64, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelState, f.modelCompleted, f.modelTotal, f.modelErr
}

func (f *fakeSetupProvider) BenchmarkStatus() management.BenchmarkStatusResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bench
}

func (f *fakeSetupProvider) startSetupBenchmark(gen int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.benchStarts = append(f.benchStarts, gen)
	f.bench.State = management.BenchmarkStateRunning
}

func (f *fakeSetupProvider) PullModel(_ context.Context, model string) (management.PullJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, model)
	if f.pullErr != nil {
		return management.PullJob{}, f.pullErr
	}
	f.modelState = catalog.ModelStateQueued
	return management.PullJob{JobID: "job-1", ModelID: model, Status: "queued"}, nil
}

// setupPreferredModelID reports what the device is set to serve.
func (f *fakeSetupProvider) setupPreferredModelID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.preferred
}

// setupApplyModel mirrors the real adapter's observable contract: it
// records the model it was given, marks it as the served one, and starts
// a download ONLY when the weights are not already local — the same
// split SwapPreferredModel makes. A fake that always pulled would hide
// the "already on disk, so nothing happened" half of #230; one that
// never pulled would hide the wizard's progress bar.
func (f *fakeSetupProvider) setupApplyModel(_ context.Context, model string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies = append(f.applies, model)
	if f.applyErr != nil {
		if !errors.Is(f.applyErr, errSwapNeedsRestart) {
			return false, f.applyErr
		}
		// Cross-engine: the real adapter falls back to a bare pull.
		f.pulls = append(f.pulls, model)
		f.modelState = catalog.ModelStateQueued
		return true, nil
	}
	f.preferred = model
	if f.modelState == catalog.ModelStateReady {
		return false, nil
	}
	f.pulls = append(f.pulls, model)
	f.modelState = catalog.ModelStateQueued
	return true, nil
}

// setEngine scripts the (installed, ready) pair the reconciler observes,
// which is how the executor's install becomes visible to it.
func (f *fakeSetupProvider) setEngine(installed, ready bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.engineInstalled = installed
	f.engineReady = ready
}

func (f *fakeSetupProvider) setModelState(state, errText string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelState = state
	f.modelErr = errText
}

func (f *fakeSetupProvider) pullCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pulls)
}

func (f *fakeSetupProvider) startSetupEngine(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.engineStarts = append(f.engineStarts, reason)
}

func (f *fakeSetupProvider) engineStartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.engineStarts)
}

// fakeClock drives the reconciler's `now` seam so lease expiry is tested
// without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// stepByID returns the named step from a snapshot, failing the test when
// it is absent — a missing step is always a bug in these cases.
func stepByID(t *testing.T, p *signer.SetupProgress, id string) signer.SetupStep {
	t.Helper()
	if p == nil {
		t.Fatalf("snapshot is nil, want a step %q", id)
	}
	for _, s := range p.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("step %q missing from %+v", id, p.Steps)
	return signer.SetupStep{}
}

func desiredFrame(engine, model string, gen int) *signer.InferenceState {
	return &signer.InferenceState{
		DesiredEngine:       engine,
		DesiredModelID:      model,
		DesiredBenchmarkGen: gen,
	}
}

// retryFrame is desiredFrame plus the model-download retry generation
// (#136) — the CP's request that the pull be admitted again.
func retryFrame(engine, model string, modelGen int) *signer.InferenceState {
	st := desiredFrame(engine, model, 0)
	st.DesiredModelGen = modelGen
	return st
}

// TestSetupEngineStateIsAskedPerDesiredEngine pins that the reconciler
// asks about the DESIRED engine kind, and that the answer for one engine
// does not leak into another.
//
// It exists because the fake used to discard both arguments, which made
// this unwritable — and "the predicate answers per engine, resolved
// against the state dir" is the whole content of #179. With the
// arguments discarded, a reconciler that asked about the wrong engine,
// or stopped passing the desired one at all, would have gone unnoticed
// by every test in this file.
func TestSetupEngineStateIsAskedPerDesiredEngine(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	// The desired engine is installed and serving; a different engine
	// kind on the same host is not installed at all.
	f.setEngineFor("ollama", true, true)
	f.setEngineFor("vllm", false, false)

	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()
	r.Apply(ctx, desiredFrame("ollama", "m1", 0))
	snap := r.snapshot(ctx)

	if got := f.enginesAsked(); len(got) != 1 || got[0] != "ollama" {
		t.Fatalf("engines asked = %v, want exactly [ollama] — the desired engine", got)
	}
	for _, c := range f.engineStateCalls {
		if !c.HadCtx {
			t.Errorf("setupEngineState called with a nil context: %+v", c)
		}
	}
	if st := stepByID(t, snap, setupStepEngineInstall).Status; st != signer.SetupStatusDone {
		t.Errorf("engine step = %q, want done — ollama is installed and serving", st)
	}

	// Flip it. The DEFAULT here is "installed and ready" — which is what
	// an argument-blind fake would return no matter what it was asked —
	// while the desired engine is scripted as absent. Both halves of this
	// test therefore fail against a fake that discards its arguments,
	// which is the point: before this change neither could be written.
	g := &fakeSetupProvider{
		modelState:      catalog.ModelStateNotPresent,
		engineInstalled: true,
		engineReady:     true,
	}
	g.setEngineFor("ollama", false, false)
	r2 := newSetupReconciler(g, nil, "dev-1", nil, quietLogger())
	r2.Apply(ctx, desiredFrame("ollama", "m1", 0))
	if st := stepByID(t, r2.snapshot(ctx), setupStepEngineInstall).Status; st == signer.SetupStatusDone {
		t.Error("engine step is done, but the DESIRED engine (ollama) is not installed — " +
			"an answer meant for some other engine leaked through")
	}
}

// TestSetupApplyIdempotent pins the §6 contract: streaming has no frame
// dedup, so replaying the identical desired state must trigger each
// action at most once — one benchmark job, one pull admission.
func TestSetupApplyIdempotent(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	frame := desiredFrame("", "qwen3-8b-instruct", 2)
	for i := 0; i < 3; i++ {
		r.Apply(ctx, frame)
	}
	if len(f.benchStarts) != 1 || f.benchStarts[0] != 2 {
		t.Fatalf("benchStarts = %v, want exactly one start at gen 2", f.benchStarts)
	}
	if len(f.pulls) != 1 || f.pulls[0] != "qwen3-8b-instruct" {
		t.Fatalf("pulls = %v, want exactly one pull", f.pulls)
	}
}

// TestSetupDesiredStaleOnLeftoverState is the #308 contract, and the rc7
// symptom exactly: the control plane never clears desired_engine /
// desired_model_id, so a device that ran a wizard once carries that
// instruction on its map entry forever. A daemon whose FIRST frame
// already carries it is looking at a snapshot of an old run, not at a
// wizard that is driving now, and `waired init` must not report
// "Setup has started in your browser" for it.
func TestSetupDesiredStaleOnLeftoverState(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateReady}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	r.Apply(ctx, desiredFrame("ollama", "qwen3-8b-instruct", 1))

	st := r.SetupState(ctx)
	if !st.Active {
		t.Fatal("a desired instruction was not recorded at all")
	}
	if !st.DesiredStale {
		t.Error("leftover desired state reported as a live wizard (#308)")
	}
}

// TestSetupDesiredFreshWhenTheWizardWrites is the other half, and the
// case that matters most: on a device that has never been set up the
// first frames carry nothing, and the instruction arrives while this
// daemon is watching. That IS a wizard driving, and must not be filed
// as leftovers — which is why the baseline is anchored on the first
// frame folded, not on the first frame that carries desired state.
func TestSetupDesiredFreshWhenTheWizardWrites(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	r.Apply(ctx, &signer.InferenceState{}) // the fleet-at-rest frames
	r.Apply(ctx, &signer.InferenceState{})
	r.Apply(ctx, desiredFrame("ollama", "qwen3-8b-instruct", 1)) // the wizard writes

	if st := r.SetupState(ctx); !st.Active || st.DesiredStale {
		t.Errorf("a wizard writing while we watched read as leftovers: active=%v stale=%v",
			st.Active, st.DesiredStale)
	}
}

// A wizard that wrote and then went away stops counting as live: the
// window matches the control plane's own setup-ticket TTL, so an
// abandoned page cannot hold a later `waired init` in browser-driven
// mode forever.
func TestSetupDesiredGoesStaleAfterTheWindow(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ctx := context.Background()

	r.Apply(ctx, &signer.InferenceState{})
	r.Apply(ctx, desiredFrame("ollama", "qwen3-8b-instruct", 1))
	if st := r.SetupState(ctx); st.DesiredStale {
		t.Fatal("fresh write already reported stale")
	}

	now = now.Add(setupDesiredFreshWindow - time.Minute)
	if st := r.SetupState(ctx); st.DesiredStale {
		t.Error("a write inside the window reported stale")
	}
	now = now.Add(2 * time.Minute) // past the window
	if st := r.SetupState(ctx); !st.DesiredStale {
		t.Error("an abandoned wizard still counts as driving")
	}
}

// Re-sent frames are not writes. The map is re-delivered on every poll,
// so treating repetition as a fresh instruction would keep any device
// that ever ran a wizard permanently "live".
func TestSetupDesiredRepeatIsNotAWrite(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ctx := context.Background()

	r.Apply(ctx, &signer.InferenceState{})
	frame := desiredFrame("ollama", "qwen3-8b-instruct", 1)
	r.Apply(ctx, frame)

	now = now.Add(setupDesiredFreshWindow + time.Minute)
	r.Apply(ctx, frame) // the same instruction, re-delivered
	if st := r.SetupState(ctx); !st.DesiredStale {
		t.Error("a re-delivered frame refreshed the instruction")
	}
}

// TestSetupApplyZeroDesiredIsFree pins the fleet-at-rest guarantee: a
// host that never ran a NAVI setup does no work and reports nothing.
func TestSetupApplyZeroDesiredIsFree(t *testing.T) {
	f := &fakeSetupProvider{}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	r.Apply(ctx, nil)
	r.Apply(ctx, &signer.InferenceState{})
	if len(f.benchStarts) != 0 || len(f.pulls) != 0 {
		t.Fatalf("zero desired must not act: starts=%v pulls=%v", f.benchStarts, f.pulls)
	}
	if snap := r.snapshot(ctx); snap != nil {
		t.Fatalf("zero desired must snapshot nil, got %+v", snap)
	}
}

// TestSetupApplySkipsPresentModelAndAnsweredBenchmark: a model that is
// already local (any in-flight/ready state) is never re-pulled, and a
// benchmark that already ran at the requested generation — even a
// FAILED one — is never rerun (failure is an answer; NAVI re-bumps).
//
// Product contract, with one clause inverted by #230: the choice is
// still APPLIED when the weights are already on disk. Skipping the whole
// model step for a locally-present model is precisely how a wizard run
// could finish having changed nothing at all.
func TestSetupApplySkipsPresentModelAndAnsweredBenchmark(t *testing.T) {
	f := &fakeSetupProvider{
		modelState: catalog.ModelStateReady,
		bench:      management.BenchmarkStatusResponse{State: management.BenchmarkStateFailed, Gen: 3, Error: "boom"},
	}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(context.Background(), desiredFrame("", "qwen3-8b-instruct", 3))
	if len(f.pulls) != 0 {
		t.Fatalf("ready model re-pulled: %v", f.pulls)
	}
	if len(f.applies) != 1 || f.applies[0] != "qwen3-8b-instruct" {
		t.Fatalf("applies = %v, want the ready model applied exactly once", f.applies)
	}
	if len(f.benchStarts) != 0 {
		t.Fatalf("answered (failed) gen rerun: %v", f.benchStarts)
	}
}

// TestSetupDesiredModelBecomesTheServedModel is the #230 regression
// test, and a product contract: the model the wizard writes is the model
// the device ends up SERVING, not merely one it downloads.
//
// Before this, Apply handed the desired model to PullModel and to
// nothing else. PullModel writes weights and catalog state; it does not
// touch the preference or the active selection, and neither activation
// path could pick the choice up — activateBundledIfUnset fires only for
// the install-time bundled model, activatePreferredIfNeeded needs a
// preference the setup path never wrote. A user who chose anything other
// than what the daemon had already auto-selected for itself got a
// multi-gigabyte download, a green wizard, and the old model still
// answering every request.
func TestSetupDesiredModelBecomesTheServedModel(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())

	r.Apply(context.Background(), desiredFrame("", "qwen3-8b-instruct", 0))

	if len(f.applies) != 1 || f.applies[0] != "qwen3-8b-instruct" {
		t.Fatalf("applies = %v, want the desired model applied exactly once", f.applies)
	}
	if got := f.setupPreferredModelID(); got != "qwen3-8b-instruct" {
		t.Fatalf("served model after setup = %q, want the desired one", got)
	}
	// The weights still have to arrive; applying is what makes them the
	// model that answers once they do.
	if len(f.pulls) != 1 || f.pulls[0] != "qwen3-8b-instruct" {
		t.Fatalf("pulls = %v, want the desired model fetched", f.pulls)
	}
}

// TestSetupAlreadyServedModelIsNotReapplied pins the other half of the
// convergence rule: admission is keyed on OBSERVABLE state, so a daemon
// that restarts and re-reads the same instruction does not re-apply it.
// Re-applying would bounce `ollama serve` on every boot of a host that
// is already exactly where the wizard left it.
func TestSetupAlreadyServedModelIsNotReapplied(t *testing.T) {
	f := &fakeSetupProvider{
		modelState: catalog.ModelStateReady,
		preferred:  "qwen3-8b-instruct",
	}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	for i := 0; i < 3; i++ {
		r.Apply(context.Background(), desiredFrame("", "qwen3-8b-instruct", 0))
	}
	if len(f.applies) != 0 {
		t.Fatalf("applies on a converged host = %v, want none", f.applies)
	}
}

// TestSetupDesiredModelChangeIsReapplied: the owner picking a different
// model in the wizard has to reach the device. The one-shot admission is
// per desired VALUE, not per session.
func TestSetupDesiredModelChangeIsReapplied(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	r.Apply(ctx, desiredFrame("", "model-a", 0))
	f.setModelState(catalog.ModelStateNotPresent, "")
	r.Apply(ctx, desiredFrame("", "model-b", 0))
	r.Apply(ctx, desiredFrame("", "model-b", 0))

	if len(f.applies) != 2 || f.applies[0] != "model-a" || f.applies[1] != "model-b" {
		t.Fatalf("applies = %v, want [model-a model-b]", f.applies)
	}
	if got := f.setupPreferredModelID(); got != "model-b" {
		t.Fatalf("served model = %q, want model-b", got)
	}
}

// TestSetupApplyModel_RealAdapterPinsAndActivates exercises the REAL
// provider adapter, not the fake: the reconciler's fake sits above this
// seam, so without this the production path — persist the preference,
// then flip the active selection — is never executed by any test.
//
// Product contract, and the end-to-end statement of #230: after the
// wizard's model is applied, preferred-model.json names it (so it
// survives the restart an engine install causes) AND the active
// selection points at it (so it is what actually answers requests).
func TestSetupApplyModel_RealAdapterPinsAndActivates(t *testing.T) {
	manifests := recTestManifests()
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{
			"heavy": {State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "heavy:8b"},
			"light": {State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "light:2b"},
		}
		// The daemon auto-selected "heavy" for itself; the wizard asks
		// for "light". That mismatch is the whole of #230.
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: "heavy", VariantID: "q4", DecidedBy: "auto",
		}
	}); err != nil {
		t.Fatal(err)
	}
	adapter, _ := newSwapTestAdapter(t)
	if err := adapter.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	prefPath := filepath.Join(t.TempDir(), "preferred-model.json")

	p := &agentInferenceProvider{
		cfg:            agentconfig.InferenceConfig{BundledModelID: "heavy"},
		manifests:      manifests,
		store:          store,
		ollama:         adapter,
		profiler:       cpuSwapProfiler(t),
		preferencePath: prefPath,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if _, err := p.setupApplyModel(context.Background(), "light"); err != nil {
		t.Fatalf("setupApplyModel: %v", err)
	}

	pref, ok, err := agentconfig.LoadPreference(prefPath)
	if err != nil || !ok || pref.ModelID != "light" {
		t.Fatalf("preference = %+v ok=%v err=%v, want light persisted", pref, ok, err)
	}
	// The activation runs on the reconcile goroutine.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := store.Load(); st.Active != nil && st.Active.ModelID == "light" {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := store.Load()
	t.Fatalf("the chosen model never became active: Active=%+v", st.Active)
}

// TestSetupApplyModelCrossEngineFallsBackToPull: a target the in-process
// switch cannot apply (a cross-engine change) still has to get its
// weights now — the preference the adapter persisted activates them
// after the restart the engine change causes. Reporting the refusal as a
// model failure instead would show the operator a red step for a setup
// that is in fact on its way.
func TestSetupApplyModelCrossEngineFallsBackToPull(t *testing.T) {
	f := &fakeSetupProvider{
		modelState: catalog.ModelStateNotPresent,
		applyErr:   errSwapNeedsRestart,
	}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()
	r.Apply(ctx, desiredFrame("", "m-1", 0))

	if len(f.pulls) != 1 || f.pulls[0] != "m-1" {
		t.Fatalf("pulls = %v, want the weights fetched anyway", f.pulls)
	}
	if got := stepByID(t, r.snapshot(ctx), setupStepModelPull); got.Status != signer.SetupStatusRunning {
		t.Fatalf("model step = %+v, want running", got)
	}
}

// TestSetupSnapshotStatuses walks the §7 step derivations.
func TestSetupSnapshotStatuses(t *testing.T) {
	ctx := context.Background()

	// Engine missing → failed + permission_denied (§11: install is the
	// executor's job).
	f := &fakeSetupProvider{modelState: catalog.ModelStateDownloading, modelCompleted: 512, modelTotal: 4096}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(ctx, desiredFrame("ollama", "m1", 1))

	snap := r.snapshot(ctx)
	if snap == nil || len(snap.Steps) != 3 {
		t.Fatalf("snapshot = %+v, want 3 steps", snap)
	}
	eng, mod, bench := snap.Steps[0], snap.Steps[1], snap.Steps[2]
	if eng.ID != setupStepEngineInstall || eng.Status != signer.SetupStatusFailed || eng.ErrorCode != signer.SetupErrorPermissionDenied {
		t.Fatalf("engine step = %+v, want failed/permission_denied", eng)
	}
	if mod.ID != setupStepModelPull || mod.Status != signer.SetupStatusRunning || mod.CompletedBytes != 512 || mod.TotalBytes != 4096 {
		t.Fatalf("model step = %+v, want running with bytes", mod)
	}
	// startSetupBenchmark flipped the fake to running.
	if bench.ID != setupStepBenchmark || bench.Status != signer.SetupStatusRunning {
		t.Fatalf("benchmark step = %+v, want running", bench)
	}

	// Engine installed → done: this step installs the engine, and the
	// engine is installed (#187 — it used to wait for the MODEL to be
	// ready). Benchmark done at gen carries the measurement.
	f.mu.Lock()
	f.engineInstalled = true
	f.modelState = catalog.ModelStateReady
	f.bench = management.BenchmarkStatusResponse{State: management.BenchmarkStateDone, Gen: 1, MeasuredTokps: 42.5}
	f.mu.Unlock()
	snap = r.snapshot(ctx)
	if snap.Steps[0].Status != signer.SetupStatusDone {
		t.Fatalf("installed engine step = %+v, want done", snap.Steps[0])
	}
	if snap.Steps[1].Status != signer.SetupStatusDone {
		t.Fatalf("ready model step = %+v, want done", snap.Steps[1])
	}
	if snap.Steps[2].Status != signer.SetupStatusDone || snap.Benchmark == nil ||
		snap.Benchmark.Gen != 1 || snap.Benchmark.MeasuredTokps != 42.5 {
		t.Fatalf("benchmark step = %+v benchmark=%+v, want done + measurement", snap.Steps[2], snap.Benchmark)
	}

	f.mu.Lock()
	f.engineReady = true
	f.mu.Unlock()
	if snap = r.snapshot(ctx); snap.Steps[0].Status != signer.SetupStatusDone {
		t.Fatalf("ready engine step = %+v, want done", snap.Steps[0])
	}

	// Failed pull carries the stored error as network_error.
	f.mu.Lock()
	f.modelState = catalog.ModelStateFailed
	f.modelErr = "connection reset"
	f.mu.Unlock()
	snap = r.snapshot(ctx)
	if snap.Steps[1].Status != signer.SetupStatusFailed || snap.Steps[1].ErrorCode != signer.SetupErrorNetworkError ||
		snap.Steps[1].ErrorDetail != "connection reset" {
		t.Fatalf("failed model step = %+v, want failed/network_error", snap.Steps[1])
	}
}

// TestSetupModelRejectedReportsModelNotFound: the provider refusing the
// ID (not in the catalog, or no variant the engine can serve) surfaces
// as failed/model_not_found and is not retried on later frames.
//
// The refusal now arrives from setupApplyModel rather than PullModel
// (#230) — same failure, one layer up, because applying the choice is
// what resolves it against the catalog. A refused model is never
// downloaded.
func TestSetupModelRejectedReportsModelNotFound(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent, applyErr: errors.New("unknown model")}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	frame := desiredFrame("", "no-such-model", 0)
	r.Apply(ctx, frame)
	r.Apply(ctx, frame)
	if len(f.applies) != 1 {
		t.Fatalf("rejected model retried: %v", f.applies)
	}
	if len(f.pulls) != 0 {
		t.Fatalf("a refused model must not be downloaded: %v", f.pulls)
	}
	snap := r.snapshot(ctx)
	if len(snap.Steps) != 1 || snap.Steps[0].Status != signer.SetupStatusFailed ||
		snap.Steps[0].ErrorCode != signer.SetupErrorModelNotFound || snap.Steps[0].ErrorDetail != "unknown model" {
		t.Fatalf("rejected pull step = %+v, want failed/model_not_found", snap.Steps[0])
	}
}

// TestSetupPushDedupes drives runPush against a fake CP: identical
// snapshots push once, a content change pushes again, and the payload
// is a validly signed UpsertSetupProgressRequest shape.
func TestSetupPushDedupes(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/devices/self/setup-progress" {
			t.Errorf("unexpected path %s", req.URL.Path)
		}
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cli := controlclient.NewWithBearer(srv.URL, func() string { return "tok" })

	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	r := newSetupReconciler(f, cli, "dev-1", priv, quietLogger())
	r.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.runPush(ctx) }()

	r.Apply(ctx, desiredFrame("", "m1", 0)) // → pending pull → queued
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(bodies) >= 1 }, "first push")
	time.Sleep(50 * time.Millisecond) // several ticks with unchanged content
	mu.Lock()
	afterFirst := len(bodies)
	mu.Unlock()
	if afterFirst != 1 {
		t.Fatalf("unchanged content pushed %d times, want 1", afterFirst)
	}

	f.mu.Lock()
	f.modelState = catalog.ModelStateReady
	f.mu.Unlock()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(bodies) >= 2 }, "second push after change")

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	var req struct {
		DeviceID string               `json:"device_id"`
		IssuedAt string               `json:"issued_at"`
		Nonce    string               `json:"nonce"`
		Progress signer.SetupProgress `json:"progress"`
	}
	if err := json.Unmarshal(bodies[1], &req); err != nil {
		t.Fatalf("unmarshal push body: %v", err)
	}
	if req.DeviceID != "dev-1" || req.Nonce == "" || req.IssuedAt == "" {
		t.Fatalf("push envelope = %+v", req)
	}
	if len(req.Progress.Steps) != 1 || req.Progress.Steps[0].Status != signer.SetupStatusDone {
		t.Fatalf("second push progress = %+v, want model done", req.Progress)
	}
}

// --- executor lease (waired#835 §9/§11) ---

// leasedReconciler wires a reconciler to a controllable clock with one
// desired frame already applied.
func leasedReconciler(t *testing.T, f *fakeSetupProvider, engine, model string) (*setupReconciler, *fakeClock) {
	t.Helper()
	c := newFakeClock()
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.now = c.now
	r.Apply(context.Background(), desiredFrame(engine, model, 0))
	return r, c
}

// TestSetupSnapshotNoExecutorStillPermissionDenied pins the UNCHANGED
// pre-lease wire for the case that has not moved: nobody ever attached,
// so this is a privileges problem, not a liveness one. Sending the
// operator to re-run a command would be wrong here — there is no reason
// to think a second run would attach either.
func TestSetupSnapshotNoExecutorStillPermissionDenied(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	step := stepByID(t, r.snapshot(context.Background()), setupStepEngineInstall)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorPermissionDenied {
		t.Fatalf("engine step = %+v, want failed/permission_denied", step)
	}
}

// TestSetupSnapshotUnelevatedExecutorIsPermissionDenied: an executor is
// present but cannot install. Reporting executor_gone here would send the
// operator to re-run a command that fails the same way.
func TestSetupSnapshotUnelevatedExecutorIsPermissionDenied(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{Attached: true, Elevated: false})
	step := stepByID(t, r.snapshot(context.Background()), setupStepEngineInstall)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorPermissionDenied {
		t.Fatalf("engine step = %+v, want failed/permission_denied", step)
	}
}

// TestSetupSnapshotElevatedExecutorIsRunning: with an elevated executor
// attached the install step must read as in-progress. Before the lease
// existed this reported a hard failure from the very first push, which
// is the regression this wave exists to fix.
func TestSetupSnapshotElevatedExecutorIsRunning(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	for _, phase := range []string{management.SetupExecutorPhaseIdle, management.SetupExecutorPhaseInstalling} {
		r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
			Attached: true, Elevated: true, Phase: phase, Engine: "ollama",
		})
		step := stepByID(t, r.snapshot(context.Background()), setupStepEngineInstall)
		if step.Status != signer.SetupStatusRunning {
			t.Fatalf("phase %q: engine step = %+v, want running", phase, step)
		}
	}
}

// TestSetupSnapshotExecutorGoneAfterTTL covers §9-4: the executor was
// here and is gone, which is the RECOVERABLE case — NAVI offers the
// command to re-run.
func TestSetupSnapshotExecutorGoneAfterTTL(t *testing.T) {
	f := &fakeSetupProvider{}
	r, clock := leasedReconciler(t, f, "ollama", "")
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	clock.advance(setupExecutorTTL + time.Second)
	step := stepByID(t, r.snapshot(context.Background()), setupStepEngineInstall)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorExecutorGone {
		t.Fatalf("engine step = %+v, want failed/executor_gone", step)
	}
}

// TestSetupExecutorReleaseIsImmediate: Ctrl-C releases explicitly, so the
// wizard must not wait out the whole TTL to stop spinning.
func TestSetupExecutorReleaseIsImmediate(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	ctx := context.Background()
	r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: true, Elevated: true})
	r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: false})
	step := stepByID(t, r.snapshot(ctx), setupStepEngineInstall)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorExecutorGone {
		t.Fatalf("engine step = %+v, want failed/executor_gone", step)
	}
}

// TestSetupExecutorGoneDoesNotStallOtherSteps pins §9-5: steps that do
// not need the executor keep going after it dies.
func TestSetupExecutorGoneDoesNotStallOtherSteps(t *testing.T) {
	f := &fakeSetupProvider{
		modelState:     catalog.ModelStateDownloading,
		modelCompleted: 512,
		modelTotal:     4096,
		bench:          management.BenchmarkStatusResponse{State: management.BenchmarkStateRunning},
	}
	c := newFakeClock()
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.now = c.now
	ctx := context.Background()
	r.Apply(ctx, desiredFrame("ollama", "m-1", 3))
	r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: true, Elevated: true})
	r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: false})

	snap := r.snapshot(ctx)
	if got := stepByID(t, snap, setupStepEngineInstall); got.ErrorCode != signer.SetupErrorExecutorGone {
		t.Fatalf("engine step = %+v, want executor_gone", got)
	}
	if got := stepByID(t, snap, setupStepModelPull); got.Status != signer.SetupStatusRunning || got.CompletedBytes != 512 {
		t.Fatalf("model step = %+v, want running with bytes", got)
	}
	if got := stepByID(t, snap, setupStepBenchmark); got.Status != signer.SetupStatusRunning {
		t.Fatalf("benchmark step = %+v, want running", got)
	}
}

// TestSetupInstallClaimIsLeaseBound is the blocker regression test.
// A claim bound to desired_engine instead of to the lease would make the
// executor_gone recovery copy ("re-run sudo waired init") a no-op: the
// re-run would see the stale claim and skip the install, leaving the step
// red forever. It would also let one local POST block installation
// permanently.
func TestSetupInstallClaimIsLeaseBound(t *testing.T) {
	f := &fakeSetupProvider{}
	r, clock := leasedReconciler(t, f, "ollama", "")
	ctx := context.Background()
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	if got := r.SetupState(ctx).InstallClaimed; got != "ollama" {
		t.Fatalf("install_claimed = %q while the lease is live, want ollama", got)
	}
	clock.advance(setupExecutorTTL + time.Second)
	if got := r.SetupState(ctx).InstallClaimed; got != "" {
		t.Fatalf("install_claimed = %q after the claiming lease expired, want empty", got)
	}
	// A fresh executor (the operator re-ran the command) can claim it.
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	if got := r.SetupState(ctx).InstallClaimed; got != "ollama" {
		t.Fatalf("install_claimed = %q after re-attach, want ollama", got)
	}
}

// TestSetupExecutorFailedPhaseCarriesItsOwnError: when the executor tried
// and failed, its text beats anything the daemon could infer.
func TestSetupExecutorFailedPhaseCarriesItsOwnError(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	ctx := context.Background()
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseFailed,
		Engine: "ollama", Error: "download failed: no space left on device",
	})
	step := stepByID(t, r.snapshot(ctx), setupStepEngineInstall)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorDiskFull {
		t.Fatalf("engine step = %+v, want failed/disk_full", step)
	}
	if step.ErrorDetail == "" {
		t.Fatal("engine step lost the executor's error detail")
	}
	if got := r.SetupState(ctx).InstallClaimed; got != "" {
		t.Fatalf("install_claimed = %q after a failed attempt, want empty so a retry can claim it", got)
	}
}

// TestSetupEngineStepMatrix walks the whole (installed, ready, phase)
// space of the engine_install arm, because the arm ORDER is the contract
// (#187) and order bugs only show up as a specific pair disagreeing.
//
// Every row has a live elevated lease, so the `leaseLive` arm is the
// thing each earlier arm has to beat — which is exactly the shape the
// bug had: a real state falling through to "working on it".
func TestSetupEngineStepMatrix(t *testing.T) {
	for _, tc := range []struct {
		name            string
		installed       bool
		ready           bool
		latched         bool
		phase           string
		wantStatus      string
		wantErrCode     string
		wantErrorDetail bool
	}{
		{
			// THE regression bar #187 names. A half-configured engine
			// leaves a binary behind, so `installed` is true while the
			// install genuinely failed. It used to shadow the failed arm
			// and render "working on it" forever.
			name: "installed but the executor reported failure", installed: true, phase: management.SetupExecutorPhaseFailed,
			wantStatus: signer.SetupStatusFailed, wantErrCode: signer.SetupErrorDiskFull, wantErrorDetail: true,
		},
		{
			// The common case during a multi-GB model download. This is
			// the row that used to read `running` for the whole pull,
			// putting two rows in flight at once in the wizard.
			name: "installed, model not ready yet", installed: true,
			wantStatus: signer.SetupStatusDone,
		},
		{
			// Serving beats a stale failed phase from an earlier attempt.
			name: "ready despite an older failed phase", installed: true, ready: true, phase: management.SetupExecutorPhaseFailed,
			wantStatus: signer.SetupStatusDone,
		},
		{
			// The executor's explicit completion, which exists to advance
			// the wizard and used to be discarded outright.
			name: "executor done before we can see the engine", phase: management.SetupExecutorPhaseDone,
			wantStatus: signer.SetupStatusDone,
		},
		{
			name: "installing, engine not visible yet", phase: management.SetupExecutorPhaseInstalling,
			wantStatus: signer.SetupStatusRunning,
		},
		{
			name: "attached and idle", phase: management.SetupExecutorPhaseIdle,
			wantStatus: signer.SetupStatusRunning,
		},
		{
			name: "failed with nothing installed", phase: management.SetupExecutorPhaseFailed,
			wantStatus: signer.SetupStatusFailed, wantErrCode: signer.SetupErrorDiskFull, wantErrorDetail: true,
		},
		// #330. THE regression bar for this issue: a macOS bundle with a
		// broken signature is "installed" by every file-presence measure while
		// every exec of it is killed, so this row used to report Done -- on
		// every rerun, forever, while /inference/status carried
		// subsystem_state=engine_failed on the same tick.
		{
			name: "installed but the daemon gave up starting it", installed: true, latched: true,
			wantStatus: signer.SetupStatusFailed, wantErrCode: signer.SetupErrorEngineNotReady, wantErrorDetail: true,
		},
		{
			// Loses to a serving engine: a latch that outlived a successful
			// start is stale, and Done is the truth.
			name: "ready despite a latched failure", installed: true, ready: true, latched: true,
			wantStatus: signer.SetupStatusDone,
		},
		{
			// Loses to the executor's own report: it just tried, so its
			// evidence is fresher and more specific than an older latch.
			name: "latched, but the executor reported its own failure", installed: true, latched: true,
			phase:      management.SetupExecutorPhaseFailed,
			wantStatus: signer.SetupStatusFailed, wantErrCode: signer.SetupErrorDiskFull, wantErrorDetail: true,
		},
		{
			// The guard against the obvious over-correction: an engine that is
			// merely not ready yet (every model download, every restart) must
			// stay Done. Only a LATCHED give-up paints the row red.
			name: "installed and unhealthy but not latched", installed: true, latched: false,
			wantStatus: signer.SetupStatusDone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSetupProvider{
				engineInstalled: tc.installed, engineReady: tc.ready,
				engineLatched: tc.latched,
				engineLastErr: "ollama: process exited during startup: signal: killed",
			}
			r, _ := leasedReconciler(t, f, "ollama", "")
			ctx := context.Background()
			r.NoteExecutor(ctx, management.SetupExecutorRequest{
				Attached: true, Elevated: true, Phase: tc.phase, Engine: "ollama",
				Error: "download failed: no space left on device",
			})

			step := stepByID(t, r.snapshot(ctx), setupStepEngineInstall)
			if step.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (step: %+v)", step.Status, tc.wantStatus, step)
			}
			if step.ErrorCode != tc.wantErrCode {
				t.Errorf("error_code = %q, want %q", step.ErrorCode, tc.wantErrCode)
			}
			if got := step.ErrorDetail != ""; got != tc.wantErrorDetail {
				t.Errorf("has error_detail = %v, want %v (%q)", got, tc.wantErrorDetail, step.ErrorDetail)
			}
			// The health question must be asked about the engine this step is
			// for. A latch belonging to a different engine kind says nothing
			// about this row, and a probe that dropped the engine argument
			// would make that confusion unwritable (CLAUDE.md §Test
			// discipline).
			//
			// Asked at most when the engine is actually installed: "has the
			// daemon given up restarting it" is meaningless for an engine that
			// is not there, and asking would put a probe on the first-install
			// path. Both the step arm and SetupStateResponse.EngineNeedsRepair
			// consult it, so the count is not pinned — only the subject.
			asked := f.healthAsked()
			if got := len(asked) > 0; got != tc.installed {
				t.Errorf("engine health probed = %v (%v), want %v", got, asked, tc.installed)
			}
			for _, e := range asked {
				if e != "ollama" {
					t.Errorf("health probed for engine %q, want ollama", e)
				}
			}
		})
	}
}

// TestSetupEngineStepDoneDoesNotOutrunTheModelStep: completing the
// engine row on `installed` must not make the model row look complete
// too. The two answers come from different providers on purpose — the
// wizard showing "engine installed, model downloading" is the whole
// point of splitting them.
func TestSetupEngineStepDoneDoesNotOutrunTheModelStep(t *testing.T) {
	f := &fakeSetupProvider{
		engineInstalled: true,
		modelState:      catalog.ModelStateDownloading,
		modelCompleted:  512, modelTotal: 4096,
	}
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	snap := r.snapshot(context.Background())

	if got := stepByID(t, snap, setupStepEngineInstall); got.Status != signer.SetupStatusDone {
		t.Errorf("engine step = %+v, want done", got)
	}
	mod := stepByID(t, snap, setupStepModelPull)
	if mod.Status != signer.SetupStatusRunning || mod.CompletedBytes != 512 || mod.TotalBytes != 4096 {
		t.Errorf("model step = %+v, want running with byte progress", mod)
	}
}

// --- pull ordering and re-admission ---

// TestSetupPullWaitsForEngineInstall pins the ordering guarantee that
// lets the wizard write the engine and the model in ONE gesture
// (waired#904). Firing PullModel against an engine that is not installed
// yet cannot succeed — the inference subsystem starts inert on an
// engine-less host — and the resulting failure is what the operator sees
// for the whole multi-minute install: a red "Download the AI model" that
// later turns green on its own. Skipping the doomed attempt leaves the
// step at its honest `pending` instead.
//
// This is the belt that used to live in the browser as a sessionStorage
// write-order effect. Moving it here is what makes the choice survive a
// reload, a second sign-in, or a different device.
func TestSetupPullWaitsForEngineInstall(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")

	// No engine yet: repeated frames must not touch PullModel at all,
	// and the step must read pending — never failed.
	for i := 0; i < 3; i++ {
		r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	}
	if got := f.pullCount(); got != 0 {
		t.Fatalf("pulls before the engine was installed = %d, want 0", got)
	}
	if got := stepByID(t, r.snapshot(ctx), setupStepModelPull); got.Status != signer.SetupStatusPending {
		t.Fatalf("model step while the engine installs = %+v, want pending", got)
	}

	// The executor finishes: exactly one pull, on the first frame after.
	f.setEngine(true, false)
	r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	if got := f.pullCount(); got != 1 {
		t.Fatalf("pulls after the engine appeared = %d, want 1", got)
	}
	for i := 0; i < 3; i++ {
		r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	}
	if got := f.pullCount(); got != 1 {
		t.Fatalf("pulls after repeated frames = %d, want 1", got)
	}
}

// TestSetupPullProceedsWithoutDesiredEngine: a desired state that names
// only a model (an owner changing the model on a host that is already
// set up) must not be held back — there is no install to wait for.
func TestSetupPullProceedsWithoutDesiredEngine(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(ctx, desiredFrame("", "m-1", 0))
	if got := f.pullCount(); got != 1 {
		t.Fatalf("pulls with no desired engine = %d, want 1", got)
	}
}

// TestSetupPullReadmittedWhenEngineReappears keeps the re-admission path
// covered now that the ordering gate above prevents the common
// engine-less failure. It still matters: an engine that goes away and
// comes back (a reinstall, or a profiler cache that briefly reports it
// missing) invalidates the one-shot admission, and without the
// transition hook the download would stay red for the rest of the
// process's life.
func TestSetupPullReadmittedWhenEngineReappears(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	if got := f.pullCount(); got != 1 {
		t.Fatalf("pulls on an installed engine = %d, want 1", got)
	}
	f.setModelState(catalog.ModelStateFailed, "connection reset")

	// The engine disappears and returns: one — and only one — new pull.
	f.setEngine(false, false)
	r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	f.setEngine(true, false)
	f.setModelState(catalog.ModelStateNotPresent, "")
	r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	if got := f.pullCount(); got != 2 {
		t.Fatalf("pulls after the engine reappeared = %d, want 2", got)
	}
	for i := 0; i < 3; i++ {
		r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	}
	if got := f.pullCount(); got != 2 {
		t.Fatalf("pulls after repeated frames = %d, want 2", got)
	}
}

// TestSetupPullNotReadmittedWithoutEngineTransition: a download that
// fails for a real reason on a host that already has the engine must not
// be re-queued on every frame.
func TestSetupPullNotReadmittedWithoutEngineTransition(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	f.setModelState(catalog.ModelStateFailed, "connection reset")
	for i := 0; i < 5; i++ {
		r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	}
	if got := f.pullCount(); got != 1 {
		t.Fatalf("pulls with a stable installed engine = %d, want 1", got)
	}
}

// --- the model-download retry generation (#136) ---

// TestSetupRetryGenReadmitsThePull is the whole point of the lever, and
// its setup is the exact dead end from the rc7 review: an engine that was
// ALREADY installed when the daemon started, and a download that failed
// for a reason the operator then fixed.
//
// Product contract. Note what makes it a dead end without this: the only
// other re-admission is the engine going absent→present, and
// `engineObserved` is false on the first frame, so a host that already had
// its engine never sees that transition for the rest of the process's
// life. Only a daemon restart cleared it.
func TestSetupRetryGenReadmitsThePull(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	f.setModelState(catalog.ModelStateFailed, "no space left on device")

	// Frames keep arriving with the same instruction; nothing re-queues.
	for range 5 {
		r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	}
	if got := f.pullCount(); got != 1 {
		t.Fatalf("pulls before the retry = %d, want 1", got)
	}

	// The operator frees space and presses the button.
	r.Apply(ctx, retryFrame("ollama", "m-1", 1))
	if got := f.pullCount(); got != 2 {
		t.Fatalf("pulls after one retry = %d, want 2", got)
	}
}

// TestSetupRetryGenIsIdempotent: the CP re-sends its instruction on every
// map frame, so a condition on the generation's VALUE rather than on it
// advancing would re-queue a multi-GB download forever. Product contract.
func TestSetupRetryGenIsIdempotent(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	f.setModelState(catalog.ModelStateFailed, "no space left on device")

	for range 6 {
		r.Apply(ctx, retryFrame("ollama", "m-1", 3))
	}
	if got := f.pullCount(); got != 2 {
		t.Fatalf("pulls for one generation repeated 6 times = %d, want 2 (initial + one retry)", got)
	}
	// A further bump is a new request and must be honoured.
	r.Apply(ctx, retryFrame("ollama", "m-1", 4))
	if got := f.pullCount(); got != 3 {
		t.Fatalf("pulls after a second generation = %d, want 3", got)
	}
}

// TestSetupRetryGenWorksWithoutADesiredEngine: the clearing must not live
// beside the engine-appeared block, which is skipped entirely when there
// is no desired engine. A host set up earlier and now retrying a model
// change has no engine instruction at all.
func TestSetupRetryGenWorksWithoutADesiredEngine(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "", "m-1")
	f.setModelState(catalog.ModelStateFailed, "connection reset")

	before := f.pullCount()
	r.Apply(ctx, retryFrame("", "m-1", 1))
	if got := f.pullCount(); got != before+1 {
		t.Fatalf("pulls after the retry = %d, want %d", got, before+1)
	}
}

// TestSetupRetryGenClearsAStoredRejection: a refusal (unknown model, an
// engine too old for it) is latched in modelRejected and reported as the
// model step's failure. The retry has to clear it too, or the step stays
// red with a reason that describes an attempt nobody is making any more.
func TestSetupRetryGenClearsAStoredRejection(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateNotPresent}
	f.applyErr = errors.New(`unknown model "m-1"`)
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")

	step := stepByID(t, r.snapshot(ctx), setupStepModelPull)
	if step.Status != signer.SetupStatusFailed {
		t.Fatalf("model step before the retry = %+v, want failed", step)
	}
	// The model is in the catalog now; the retry must let it through.
	f.mu.Lock()
	f.applyErr = nil
	f.mu.Unlock()
	r.Apply(ctx, retryFrame("ollama", "m-1", 1))
	if step := stepByID(t, r.snapshot(ctx), setupStepModelPull); step.Status == signer.SetupStatusFailed {
		t.Fatalf("model step after the retry = %+v, want the stored rejection cleared", step)
	}
}

// TestSetupSnapshotEchoesTheActedGeneration is the other half of the
// lever, and the reason SetupProgress.ModelGen exists at all: the wizard
// cannot tell "not picked up yet" from "picked up and failed again"
// without it, and those two want opposite things on screen (a spinner,
// and the button back). Product contract.
func TestSetupSnapshotEchoesTheActedGeneration(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	if got := r.snapshot(ctx).ModelGen; got != 0 {
		t.Fatalf("model_gen before any request = %d, want 0", got)
	}
	r.Apply(ctx, retryFrame("ollama", "m-1", 2))
	if got := r.snapshot(ctx).ModelGen; got != 2 {
		t.Fatalf("model_gen after acting on gen 2 = %d, want 2", got)
	}
}

// TestSetupUntouchedWithoutARetryGeneration records that a host whose CP
// never bumps is bit-for-bit unaffected: no echo on the wire, and the
// one-shot admission still holds.
func TestSetupUntouchedWithoutARetryGeneration(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	f.setModelState(catalog.ModelStateFailed, "no space left on device")
	for range 4 {
		r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	}
	if got := f.pullCount(); got != 1 {
		t.Fatalf("pulls with no generation = %d, want 1", got)
	}
	if got := r.snapshot(ctx).ModelGen; got != 0 {
		t.Fatalf("model_gen = %d, want 0 so it stays off the wire", got)
	}
}

// TestSetupPullFailureClassification: an out-of-disk failure is the most
// likely way a multi-GB download dies, and telling the operator to check
// their internet connection sends them nowhere.
func TestSetupPullFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		errText string
		want    string
	}{
		{"write /var/lib/waired/blob: no space left on device", signer.SetupErrorDiskFull},
		{"ERROR: There is not enough space on the disk.", signer.SetupErrorDiskFull},
		{"insufficient disk space for model", signer.SetupErrorDiskFull},
		{"dial tcp: connection reset by peer", signer.SetupErrorNetworkError},
		{"", signer.SetupErrorNetworkError},
	} {
		if got := classifySetupFailure(tc.errText); got != tc.want {
			t.Errorf("classifySetupFailure(%q) = %q, want %q", tc.errText, got, tc.want)
		}
	}
}

// TestClassifyModelPullFailure covers the model row's own classifier
// (#307). classifySetupFailure above stays as it is on purpose: it is
// shared with both engine rows through executorErrorCode, so it must
// keep reading a refused connection as a network problem — for an engine
// DOWNLOAD that is exactly what it is.
//
// PRODUCT CONTRACT for the codes; the marker strings are a record of what
// this agent writes and what the ollama CLI prints today.
func TestClassifyModelPullFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		errText string
		want    string
	}{
		{
			"our own marker, with the engine's reason",
			engineNotRunningMarker + ": ollama: engine parked (stopped by operator); exit status 1",
			signer.SetupErrorEngineNotReady,
		},
		{
			"the ollama CLI's own wording",
			"exit status 1; Error: could not connect to ollama app, is it running?",
			signer.SetupErrorEngineNotReady,
		},
		{
			"a refused connect to the local engine port",
			"dial tcp 127.0.0.1:9475: connect: connection refused",
			signer.SetupErrorEngineNotReady,
		},
		{
			// The Windows phrasing for the same thing; matching only the
			// POSIX wording would have made this fix Linux/macOS-only.
			"the Windows phrasing for a refused connect",
			"No connection could be made because the target machine actively refused it.",
			signer.SetupErrorEngineNotReady,
		},
		{
			// Both markers genuinely co-occur: a pull that could not reach
			// the engine could not have written the bytes either. Only one
			// of the two is something the operator must act on.
			"a full disk outranks an unreachable engine",
			"exit status 1; Error: write /blobs: no space left on device, could not connect to ollama",
			signer.SetupErrorDiskFull,
		},
		{
			// LOAD-BEARING, not left over: a reset is a transfer that
			// started and died, which is the network's problem. It is what
			// separates this classifier from a sloppy Contains(l, "connection").
			"a reset mid-transfer is still the network",
			"dial tcp: connection reset by peer",
			signer.SetupErrorNetworkError,
		},
		{"nothing recorded", "", signer.SetupErrorNetworkError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyModelPullFailure(tc.errText); got != tc.want {
				t.Errorf("classifyModelPullFailure(%q) = %q, want %q", tc.errText, got, tc.want)
			}
		})
	}
}

// TestSetupModelRowDefersToABusyEngineRow covers the step-ordering half
// of #307.
//
// A `failed` model record while the engine is still being installed is
// not an artificial fixture: setupModelState reads the shared catalog,
// which the boot-time bundled pre-pull, `waired models pull` and the
// tray's model switch all write too. On the rc7 hosts one of those left
// a failed row behind, and projecting it turned the wizard's "Download
// the AI model" step red — "check its internet connection" — while the
// engine's own download bar was still moving (waired#986 F11).
//
// PRODUCT CONTRACT for every row.
func TestSetupModelRowDefersToABusyEngineRow(t *testing.T) {
	const (
		execNone       = "none"       // no executor has spoken
		execInstalling = "installing" // engine_install is running
		execDownload   = "download"   // engine_download is running
		execDownFailed = "downfailed" // engine_download failed -> install pinned pending
	)
	for _, tc := range []struct {
		name        string
		engine      string // desired engine ("" = no engine rows at all)
		executor    string
		modelState  string
		modelErr    string
		wantStatus  string
		wantErrCode string
	}{
		{
			// THE #307 BAR.
			name: "a failed model while the install is running", engine: "ollama", executor: execInstalling,
			modelState: catalog.ModelStateFailed, modelErr: "exit status 1",
			wantStatus: signer.SetupStatusPending,
		},
		{
			name: "a failed model while the download is running", engine: "ollama", executor: execDownload,
			modelState: catalog.ModelStateFailed, modelErr: "exit status 1",
			wantStatus: signer.SetupStatusPending,
		},
		{
			// The exclusion that keeps the gate from becoming a permanent
			// grey row: a failed engine download pins engine_install at
			// `pending` for good, so "pending means busy" alone would never
			// let this model row report anything again.
			name: "a failed engine download does not pin the model row", engine: "ollama", executor: execDownFailed,
			modelState: catalog.ModelStateFailed, modelErr: "exit status 1",
			wantStatus: signer.SetupStatusFailed, wantErrCode: signer.SetupErrorNetworkError,
		},
		{
			// A full disk is not fixed by the engine arriving, and this
			// window — a multi-GB model beside a 1.4 GB engine — is when a
			// disk is most likely to fill.
			name: "a full disk is reported even while the engine installs", engine: "ollama", executor: execInstalling,
			modelState: catalog.ModelStateFailed, modelErr: "write /blobs: no space left on device",
			wantStatus: signer.SetupStatusFailed, wantErrCode: signer.SetupErrorDiskFull,
		},
		{
			name: "weights already on disk still read done", engine: "ollama", executor: execInstalling,
			modelState: catalog.ModelStateReady,
			wantStatus: signer.SetupStatusDone,
		},
		{
			name: "a download genuinely moving still reads running", engine: "ollama", executor: execInstalling,
			modelState: catalog.ModelStateDownloading,
			wantStatus: signer.SetupStatusRunning,
		},
		{
			// NEGATIVE CONTROL — passes identically with the gate removed.
			// It pins that the gate does not over-reach into a failed
			// engine row, nothing more; do not read it as evidence the
			// gate exists.
			name: "no executor at all: the engine row is failed, not busy", engine: "ollama", executor: execNone,
			modelState: catalog.ModelStateFailed, modelErr: "exit status 1",
			wantStatus: signer.SetupStatusFailed, wantErrCode: signer.SetupErrorNetworkError,
		},
		{
			// NEGATIVE CONTROL — likewise vacuous against a removed gate.
			// It pins that an empty step list is not "busy".
			name: "no desired engine: there are no engine rows to wait on", engine: "", executor: execNone,
			modelState: catalog.ModelStateFailed, modelErr: "exit status 1",
			wantStatus: signer.SetupStatusFailed, wantErrCode: signer.SetupErrorNetworkError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
			if tc.modelState == catalog.ModelStateDownloading {
				f.modelCompleted, f.modelTotal = 512, 4096
			}
			r, _ := leasedReconciler(t, f, tc.engine, "m-1")
			switch tc.executor {
			case execInstalling:
				r.NoteExecutor(ctx, management.SetupExecutorRequest{
					Attached: true, Elevated: true, Engine: "ollama",
					Phase: management.SetupExecutorPhaseInstalling,
				})
			case execDownload:
				attachDownload(t, r, 120<<20, 1400<<20, 8<<20)
			case execDownFailed:
				attachDownload(t, r, 120<<20, 1400<<20, 8<<20)
				r.NoteExecutor(ctx, management.SetupExecutorRequest{
					Attached: true, Elevated: true, Engine: "ollama",
					Step: setupStepEngineDownload, Phase: management.SetupExecutorPhaseFailed,
					Error: "transfer aborted",
				})
			}
			// Set the model's outcome AFTER admission: the fake's apply
			// moves the row to queued, as a real dispatch does.
			f.setModelState(tc.modelState, tc.modelErr)

			step := stepByID(t, r.snapshot(ctx), setupStepModelPull)
			if step.Status != tc.wantStatus {
				t.Fatalf("model step = %+v, want status %q", step, tc.wantStatus)
			}
			if step.ErrorCode != tc.wantErrCode {
				t.Errorf("model step error_code = %q, want %q", step.ErrorCode, tc.wantErrCode)
			}
			if tc.wantStatus == signer.SetupStatusPending && step.ErrorDetail != "" {
				t.Errorf("model step detail = %q, want none — a deferred row must carry no failure",
					step.ErrorDetail)
			}
			if tc.wantStatus == signer.SetupStatusRunning && step.CompletedBytes != 512 {
				t.Errorf("model step = %+v, want the live byte progress kept", step)
			}
		})
	}
}

// TestSetupRejectedModelIsNotDeferredToTheEngineRow: a refusal recorded
// at admission outlives the engine that was installed when it happened —
// a reinstall, or a profiler cache that briefly reports the binary
// missing, puts a latched rejection next to a busy engine row. It must
// still be reported: "that model id does not exist" is not something
// finishing the engine install will fix.
//
// PRODUCT CONTRACT. Note there is deliberately no Apply after the engine
// is taken away — one would fire the engine-appeared path and clear the
// rejection.
func TestSetupRejectedModelIsNotDeferredToTheEngineRow(t *testing.T) {
	ctx := context.Background()
	f := &fakeSetupProvider{
		engineInstalled: true,
		modelState:      catalog.ModelStateNotPresent,
		applyErr:        errors.New("unknown model \"m-1\""),
	}
	r, _ := leasedReconciler(t, f, "ollama", "m-1")

	f.setEngine(false, false)
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Engine: "ollama",
		Phase: management.SetupExecutorPhaseInstalling,
	})

	step := stepByID(t, r.snapshot(ctx), setupStepModelPull)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorModelNotFound {
		t.Fatalf("model step = %+v, want failed/model_not_found", step)
	}
}

// TestSetupEngineRowsKeepReadingARefusedConnectAsNetwork is the guard on
// the OTHER side of #307, and the reason classifyModelPullFailure is a
// separate function instead of new arms inside classifySetupFailure.
//
// classifySetupFailure is reached from BOTH engine rows through
// executorErrorCode. Those rows describe an executor fetching the engine
// installer over the internet, so for them a refused connection is
// exactly what the catch-all says it is. Teaching the shared classifier
// to read it as engine_not_ready would tell an operator whose proxy
// blocked the download to go fix an engine that was never the problem —
// inverting this fix on the row next door.
//
// PRODUCT CONTRACT. The executor sends no error_code here on purpose:
// its own classification wins when it has one (executorErrorCode), so
// the fall-through is the only path this can be observed on.
func TestSetupEngineRowsKeepReadingARefusedConnectAsNetwork(t *testing.T) {
	for _, errText := range []string{
		`Get "https://ollama.com/download/ollama-linux-amd64": dial tcp 10.0.0.1:443: connect: connection refused`,
		"No connection could be made because the target machine actively refused it.",
	} {
		t.Run(errText[:min(len(errText), 32)], func(t *testing.T) {
			f := &fakeSetupProvider{}
			r, _ := leasedReconciler(t, f, "ollama", "")
			ctx := context.Background()
			// Both engine rows reach the shared classifier, by different
			// routes, so both are asserted: engine_download through
			// engineDownloadStep, engine_install through snapshot's own arm.
			r.NoteExecutor(ctx, management.SetupExecutorRequest{
				Attached: true, Elevated: true, Engine: "ollama",
				Step: setupStepEngineDownload, Phase: management.SetupExecutorPhaseFailed, Error: errText,
			})
			r.NoteExecutor(ctx, management.SetupExecutorRequest{
				Attached: true, Elevated: true, Engine: "ollama",
				Phase: management.SetupExecutorPhaseFailed, Error: errText,
			})

			snap := r.snapshot(ctx)
			for _, id := range []string{setupStepEngineDownload, setupStepEngineInstall} {
				step := stepByID(t, snap, id)
				if step.Status == signer.SetupStatusFailed && step.ErrorCode != signer.SetupErrorNetworkError {
					t.Fatalf("%s = %+v, want %q — an engine download failing to connect is a network problem",
						id, step, signer.SetupErrorNetworkError)
				}
			}
		})
	}
}

// TestSetupInstalledButUnstartableEngineFailsTheModelRowAsEngineNotReady
// is the payoff shape for #307, and the reason the classifier had to
// reach the model row rather than only the engine row.
//
// The binary IS installed, so engine_install is green and has nothing
// more to say. An engine that then refuses to start shows up in exactly
// one place the operator can see: the model that could not be
// downloaded. Reporting that as "check its internet connection" is the
// rc7 screenshot (waired#986 F11).
func TestSetupInstalledButUnstartableEngineFailsTheModelRowAsEngineNotReady(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateNotPresent}
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	// The pull is dispatched first and fails afterwards — the fake's apply
	// moves the row to queued, exactly as a real dispatch does.
	f.setModelState(catalog.ModelStateFailed, engineNotRunningMarker+
		": ollama: engine repeatedly crashed; not retrying (see last_error); exit status 1")
	snap := r.snapshot(context.Background())

	if got := stepByID(t, snap, setupStepEngineInstall); got.Status != signer.SetupStatusDone {
		t.Fatalf("engine step = %+v, want done — the binary is installed, so the gate must NOT arm here", got)
	}
	step := stepByID(t, snap, setupStepModelPull)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorEngineNotReady {
		t.Fatalf("model step = %+v, want failed/engine_not_ready", step)
	}
	if !strings.Contains(step.ErrorDetail, "repeatedly crashed") {
		t.Fatalf("model step detail = %q, want the engine's own reason carried through", step.ErrorDetail)
	}
}

// TestSetupDetailIsClamped keeps a long installer log from costing a
// whole push (the CP clamps at the same size).
func TestSetupDetailIsClamped(t *testing.T) {
	long := make([]byte, setupDetailMax*3)
	for i := range long {
		long[i] = 'x'
	}
	if got := len(clampSetupDetail(string(long))); got != setupDetailMax {
		t.Fatalf("clamped length = %d, want %d", got, setupDetailMax)
	}
}

// TestSetupStateProjection covers what the executor actually reads.
func TestSetupStateProjection(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, engineReady: true}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	st := r.SetupState(ctx)
	if !st.Active || st.DesiredEngine != "ollama" || st.DesiredModelID != "m-1" {
		t.Fatalf("state = %+v, want the applied desired triple", st)
	}
	if !st.EngineInstalled || !st.EngineReady {
		t.Fatalf("state = %+v, want the engine reported installed and ready", st)
	}
	if st.ExecutorAttached {
		t.Fatalf("state = %+v, want no executor attached", st)
	}
}

// TestSetupStatePublishesStateDir: the executor has no state dir of its
// own on the daemon path (runInitViaDaemon never receives one), and a
// CLI-side guess diverges silently from a daemon started with
// --state-dir. So the daemon declares it (waired#835 §11.1).
func TestSetupStatePublishesStateDir(t *testing.T) {
	f := &fakeSetupProvider{stateDir: "/var/lib/waired"}
	r, _ := leasedReconciler(t, f, "ollama", "")
	if st := r.SetupState(context.Background()); st.StateDir != "/var/lib/waired" {
		t.Fatalf("state = %+v, want the provider's state dir published", st)
	}
}

// TestSetupStatePublishesStateDirWithoutDesiredEngine: #115 served this
// only alongside a desired engine, on the reasoning that there is
// nothing to install otherwise. That was wrong — `waired init` on the
// daemon path installs the engine whenever the host wants inference,
// with or without a browser wizard, and no desired engine is set in that
// case. Withholding the path is what would leave a terminal-only install
// with no engine at all.
func TestSetupStatePublishesStateDirWithoutDesiredEngine(t *testing.T) {
	f := &fakeSetupProvider{stateDir: "/var/lib/waired"}
	r, _ := leasedReconciler(t, f, "", "m-1")
	if st := r.SetupState(context.Background()); st.StateDir != "/var/lib/waired" {
		t.Fatalf("state = %+v, want the state dir served without a desired engine", st)
	}
}

// TestSetupStateBeforeAnyDesiredFrame: an executor that polls before the
// operator has clicked anything must see active=false rather than an
// error, so it can keep waiting out its grace.
func TestSetupStateBeforeAnyDesiredFrame(t *testing.T) {
	r := newSetupReconciler(&fakeSetupProvider{}, nil, "dev-1", nil, quietLogger())
	if st := r.SetupState(context.Background()); st.Active {
		t.Fatalf("state = %+v, want active=false before any desired frame", st)
	}
}

// TestSetupNilReconcilerIsInert guards the switchboard delegate, which
// hands us a nil receiver before enrollment.
func TestSetupNilReconcilerIsInert(t *testing.T) {
	var r *setupReconciler
	ctx := context.Background()
	if st := r.SetupState(ctx); st.Active {
		t.Fatalf("nil reconciler state = %+v, want zero", st)
	}
	if st := r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: true}); st.Active {
		t.Fatalf("nil reconciler NoteExecutor = %+v, want zero", st)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- error classification (waired-agent#131 / #134 / #135 / #137) ---

// TestSetupFailureDetailSurvivesHeartbeatAndRelease is the #131
// regression bar, and the bar is DELIBERATELY the heartbeat rather than
// the release the issue named: the heartbeat repeats the phase every 10 s
// with an empty Error, so before this the executor's failure detail
// survived at most ten seconds even on a lease nobody had released. Once
// it was gone, classifySetupFailure("") reported network_error — every
// failed engine install, whatever had actually happened.
//
// Product contract: a report that says nothing about the failure does not
// erase the failure.
func TestSetupFailureDetailSurvivesHeartbeatAndRelease(t *testing.T) {
	const detail = "the setup command on this device is not running with administrator privileges"
	// Both follow-ups carry phase=failed and no text, because that is
	// exactly what the CLI sends: heartbeat() and Release() both repeat
	// s.currentPhase(), which Failed() has just set to failed, and neither
	// has any error text to repeat.
	for _, tc := range []struct {
		name string
		next management.SetupExecutorRequest
	}{
		{"heartbeat", management.SetupExecutorRequest{
			Attached: true, Elevated: true, Engine: "ollama",
			Phase: management.SetupExecutorPhaseFailed,
		}},
		{"release", management.SetupExecutorRequest{
			Attached: false, Engine: "ollama",
			Phase: management.SetupExecutorPhaseFailed,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSetupProvider{}
			r, _ := leasedReconciler(t, f, "ollama", "")
			ctx := context.Background()
			r.NoteExecutor(ctx, management.SetupExecutorRequest{
				Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseFailed,
				Engine: "ollama", Error: detail, ErrorCode: signer.SetupErrorPermissionDenied,
			})
			r.NoteExecutor(ctx, tc.next)
			step := stepByID(t, r.snapshot(ctx), setupStepEngineInstall)
			if step.ErrorCode != signer.SetupErrorPermissionDenied {
				t.Errorf("error_code = %q, want permission_denied", step.ErrorCode)
			}
			if step.ErrorDetail != detail {
				t.Errorf("error_detail = %q, want the executor's own text", step.ErrorDetail)
			}
		})
	}
}

// TestSetupIntegrationFailureDetailSurvivesHeartbeat: the same erasure hit
// the integration row, because FailedStep leaves the lease reporting
// against it and the next heartbeat repeats that step with no text.
func TestSetupIntegrationFailureDetailSurvivesHeartbeat(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "", "")
	ctx := context.Background()
	targets := []string{signer.IntegrationClaudeCode}
	r.Apply(ctx, integrationFrame(&targets))
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Step: setupStepIntegration,
		Phase: management.SetupExecutorPhaseFailed, Error: "open settings.json: permission denied",
	})
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Step: setupStepIntegration,
		Phase: management.SetupExecutorPhaseFailed,
	})
	step := stepByID(t, r.snapshot(ctx), setupStepIntegration)
	if step.ErrorDetail == "" || step.ErrorCode != signer.SetupErrorPermissionDenied {
		t.Fatalf("integration step = %+v, want the detail kept and permission_denied", step)
	}
}

// TestSetupMovingOffFailedClearsTheReason: the preservation rule above
// must not latch. A step that starts working again carries no stale
// failure with it.
func TestSetupMovingOffFailedClearsTheReason(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	ctx := context.Background()
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseFailed,
		Engine: "ollama", Error: "boom", ErrorCode: signer.SetupErrorInternal,
	})
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	step := stepByID(t, r.snapshot(ctx), setupStepEngineInstall)
	if step.Status != signer.SetupStatusRunning || step.ErrorCode != "" || step.ErrorDetail != "" {
		t.Fatalf("engine step = %+v, want running with no error left over", step)
	}
}

// TestSetupExecutorDeclaredCodeBeatsTextClassification is #135: the
// executor knows why it stopped, and the text it sends is exactly the
// kind classifySetupFailure would have called a network error.
func TestSetupExecutorDeclaredCodeBeatsTextClassification(t *testing.T) {
	const detail = "engine installs are turned off on this device (WAIRED_NO_OLLAMA)"
	// The pin that makes this test meaningful: this text really does
	// classify as network_error without a declared code.
	if got := classifySetupFailure(detail); got != signer.SetupErrorNetworkError {
		t.Fatalf("precondition: classifySetupFailure(opt-out text) = %q, want network_error", got)
	}
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	ctx := context.Background()
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseFailed,
		Engine: "ollama", Error: detail, ErrorCode: signer.SetupErrorPermissionDenied,
	})
	step := stepByID(t, r.snapshot(ctx), setupStepEngineInstall)
	if step.ErrorCode != signer.SetupErrorPermissionDenied {
		t.Fatalf("engine step = %+v, want the executor's declared permission_denied", step)
	}
}

// TestSetupExecutorWithoutCodeStillClassifiesText: an executor that
// declares nothing — an older CLI, or a real installer error whose text
// is the only evidence — keeps the disk-full reading it always had.
func TestSetupExecutorWithoutCodeStillClassifiesText(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	ctx := context.Background()
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseFailed,
		Engine: "ollama", Error: "extract: no space left on device",
	})
	step := stepByID(t, r.snapshot(ctx), setupStepEngineInstall)
	if step.ErrorCode != signer.SetupErrorDiskFull {
		t.Fatalf("engine step = %+v, want disk_full from the text", step)
	}
}

// TestSetupUnelevatedExecutorTerminatesAsPermissionDenied is #137. The
// pair is the point: the SAME departure means different things depending
// on whether the executor could have installed anything, and reporting
// executor_gone for the unprivileged one loops the operator through
// re-running a command that fails identically every time.
func TestSetupUnelevatedExecutorTerminatesAsPermissionDenied(t *testing.T) {
	for _, tc := range []struct {
		name     string
		elevated bool
		want     string
	}{
		{"unelevated executor exits", false, signer.SetupErrorPermissionDenied},
		{"elevated executor exits", true, signer.SetupErrorExecutorGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSetupProvider{}
			r, clock := leasedReconciler(t, f, "ollama", "")
			ctx := context.Background()
			r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: true, Elevated: tc.elevated})
			clock.advance(setupExecutorTTL + time.Second)
			step := stepByID(t, r.snapshot(ctx), setupStepEngineInstall)
			if step.Status != signer.SetupStatusFailed || step.ErrorCode != tc.want {
				t.Fatalf("engine step = %+v, want failed/%s", step, tc.want)
			}
		})
	}
}

// TestSetupDownloadTerminatesWhenTheLeaseDies is #256: the download row
// had no arm for a lease that went away, so a transfer interrupted by
// Ctrl-C drew a bar at whatever percentage it reached, forever.
//
// The install row stays PENDING, which is the rule already settled for a
// failed download: one event, one red row.
func TestSetupDownloadTerminatesWhenTheLeaseDies(t *testing.T) {
	f := &fakeSetupProvider{}
	r, clock := leasedReconciler(t, f, "ollama", "")
	ctx := context.Background()
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Engine: "ollama",
		Step: setupStepEngineDownload, Phase: management.SetupExecutorPhaseInstalling,
		CompletedBytes: 600 << 20, TotalBytes: 1500 << 20,
	})
	if step := stepByID(t, r.snapshot(ctx), setupStepEngineDownload); step.Status != signer.SetupStatusRunning {
		t.Fatalf("download step while the lease is live = %+v, want running", step)
	}
	clock.advance(setupExecutorTTL + time.Second)
	snap := r.snapshot(ctx)
	dl := stepByID(t, snap, setupStepEngineDownload)
	if dl.Status != signer.SetupStatusFailed || dl.ErrorCode != signer.SetupErrorExecutorGone {
		t.Fatalf("download step after the lease died = %+v, want failed/executor_gone", dl)
	}
	if ins := stepByID(t, snap, setupStepEngineInstall); ins.Status != signer.SetupStatusPending {
		t.Fatalf("install step = %+v, want pending — the failure belongs to the download row alone", ins)
	}
}

// TestClassifyModelRejection is #134: every refusal used to be reported
// as model_not_found, whose recovery ("choose another model") is wrong
// for two of these and impossible to satisfy for a third.
func TestClassifyModelRejection(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"engine too old", fmt.Errorf("model m requires ollama >= 0.9: %w", errEngineTooOld), signer.SetupErrorEngineNotReady},
		{"pulls disabled", fmt.Errorf("allow_pull=false: %w", errPullsDisabled), signer.SetupErrorInternal},
		{"unsupported source", fmt.Errorf("vllm is linux-only: %w", errUnsupportedSource), signer.SetupErrorInternal},
		{"unknown model", errors.New(`unknown model "nope"`), signer.SetupErrorModelNotFound},
		{"no variants", errors.New("manifest m has no variants"), signer.SetupErrorModelNotFound},
		// #257 wraps the seam sentinel AROUND the cause, so the specific
		// arms above have to keep winning through the extra layer.
		{
			"pulls disabled, through the swap",
			fmt.Errorf("start the download for m: %w: %w",
				fmt.Errorf("allow_pull=false: %w", errPullsDisabled),
				management.ErrModelSwitchUnavailable),
			signer.SetupErrorInternal,
		},
		{
			"dispatch failed with nothing more specific",
			fmt.Errorf("start the download for m: %w: %w",
				errors.New("write state.json: boom"), management.ErrModelSwitchUnavailable),
			signer.SetupErrorNetworkError,
		},
		{
			"dispatch failed on a full disk",
			fmt.Errorf("start the download for m: %w: %w",
				errors.New("write state.json: no space left on device"),
				management.ErrModelSwitchUnavailable),
			signer.SetupErrorDiskFull,
		},
	} {
		if got := classifyModelRejection(tc.err); got != tc.want {
			t.Errorf("%s: classifyModelRejection = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSetupEngineTooOldReportsEngineNotReady drives the classification
// through the reconciler, because the code has to reach the step and not
// merely exist. engine_not_ready has had no producer anywhere in this
// repo until now; this is it.
func TestSetupEngineTooOldReportsEngineNotReady(t *testing.T) {
	f := &fakeSetupProvider{
		engineInstalled: true,
		modelState:      catalog.ModelStateNotPresent,
		applyErr: fmt.Errorf(
			"model m-1 requires ollama >= 0.12.0 (engine reports 0.9.0); upgrade the engine or choose another model: %w",
			errEngineTooOld),
	}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()
	r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	step := stepByID(t, r.snapshot(ctx), setupStepModelPull)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorEngineNotReady {
		t.Fatalf("model step = %+v, want failed/engine_not_ready", step)
	}
	if !strings.Contains(step.ErrorDetail, "upgrade the engine") {
		t.Fatalf("model step detail = %q, want the engine-version text kept", step.ErrorDetail)
	}
}

// TestSetupSwallowedPullDispatchFailsTheModelRow is waired-agent#257 at
// the level the operator sees it: the swap layer used to return
// (false, nil) when it could not start the download, so nothing was
// recorded, the row fell through to the not_present default, and it sat
// at `pending` for the rest of the process's life — admission is once
// per desired model value, so no later frame retried it either.
func TestSetupSwallowedPullDispatchFailsTheModelRow(t *testing.T) {
	f := &fakeSetupProvider{
		engineInstalled: true,
		modelState:      catalog.ModelStateNotPresent,
		applyErr: fmt.Errorf("start the download for m-1: %w: %w",
			fmt.Errorf("pulls are disabled by config (allow_pull=false): %w", errPullsDisabled),
			management.ErrModelSwitchUnavailable),
	}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()
	r.Apply(ctx, desiredFrame("ollama", "m-1", 0))

	step := stepByID(t, r.snapshot(ctx), setupStepModelPull)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorInternal {
		t.Fatalf("model step = %+v, want failed/internal — pending is the #257 defect", step)
	}
	if !strings.Contains(step.ErrorDetail, "allow_pull=false") {
		t.Fatalf("model step detail = %q, want the refusal's own text", step.ErrorDetail)
	}
}

// --- the coding-tool row terminates (waired-agent#258) ---

// integrationsFrame is desiredFrame plus a coding-tool instruction, for
// the cases that need a model row as well.
func integrationsFrame(engine, model string, targets ...string) *signer.InferenceState {
	st := desiredFrame(engine, model, 0)
	st.DesiredIntegrations = &signer.DesiredIntegrations{Enabled: targets}
	return st
}

// TestSetupIntegrationTerminatesWhenNobodyCanWriteIt is #258. Only the
// elevated executor can write these files — the daemon deliberately will
// not touch a user's home — so with no executor the row had no author and
// no arm that could ever end it: a grey "coding tools" row forever, and
// setup_complete false with it.
//
// The two terminal codes are not interchangeable. Both send the operator
// back to the machine (NAVI renders a command for either), but one says
// the command was closed and the other says it never ran, and a wizard
// that tells someone to resume a terminal they never opened sends them
// nowhere.
func TestSetupIntegrationTerminatesWhenNobodyCanWriteIt(t *testing.T) {
	ctx := context.Background()

	t.Run("a live lease has simply not got here yet", func(t *testing.T) {
		f := &fakeSetupProvider{engineInstalled: true, engineReady: true, modelState: catalog.ModelStateReady}
		r, _ := leasedReconciler(t, f, "ollama", "")
		r.Apply(ctx, integrationsFrame("ollama", "m-1", signer.IntegrationOpenClaw))
		r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: true, Elevated: true})
		// The coding tools are the LAST thing the executor does, after the
		// engine and the model: pending here is a wait, not a stall.
		if step := stepByID(t, r.snapshot(ctx), setupStepIntegration); step.Status != signer.SetupStatusPending {
			t.Fatalf("step = %+v, want pending while an executor is attached", step)
		}
	})

	t.Run("the executor left before it got here", func(t *testing.T) {
		f := &fakeSetupProvider{engineInstalled: true, engineReady: true, modelState: catalog.ModelStateReady}
		r, clock := leasedReconciler(t, f, "ollama", "")
		r.Apply(ctx, integrationsFrame("ollama", "m-1", signer.IntegrationOpenClaw))
		r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: true, Elevated: true})
		clock.advance(setupExecutorTTL + time.Second)

		snap := r.snapshot(ctx)
		step := stepByID(t, snap, setupStepIntegration)
		if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorExecutorGone {
			t.Fatalf("step = %+v, want failed/executor_gone", step)
		}
		// Nothing else is red: this is the one thing that did not happen.
		if eng := stepByID(t, snap, setupStepEngineInstall); eng.Status != signer.SetupStatusDone {
			t.Fatalf("engine step = %+v, want done", eng)
		}
	})

	t.Run("no executor ever attached", func(t *testing.T) {
		// The browser-only host waired#935 left undecided: the engine is
		// already installed, so nothing sends the operator to a terminal —
		// and the toggles they just confirmed have no author at all.
		f := &fakeSetupProvider{engineInstalled: true, engineReady: true, modelState: catalog.ModelStateReady}
		r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
		r.now = newFakeClock().now
		r.Apply(ctx, integrationsFrame("ollama", "m-1", signer.IntegrationClaudeCode))

		snap := r.snapshot(ctx)
		step := stepByID(t, snap, setupStepIntegration)
		if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorPermissionDenied {
			t.Fatalf("step = %+v, want failed/permission_denied", step)
		}
		if eng := stepByID(t, snap, setupStepEngineInstall); eng.Status != signer.SetupStatusDone {
			t.Fatalf("engine step = %+v, want done — this host needed no executor for the engine", eng)
		}
	})

	t.Run("an all-off answer still reports skipped", func(t *testing.T) {
		// The terminal arms must not reach an instruction that asks for
		// nothing: there is nothing for an executor to write, so `skipped`
		// stays the answer whether one ever attached or not.
		f := &fakeSetupProvider{engineInstalled: true, engineReady: true}
		r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
		r.Apply(ctx, integrationsFrame("ollama", ""))
		if step := stepByID(t, r.snapshot(ctx), setupStepIntegration); step.Status != signer.SetupStatusSkipped {
			t.Fatalf("step = %+v, want skipped", step)
		}
	})
}

// TestSetupIntegrationStaysPendingWhileAnEngineRowIsRed is the
// no-double-red bar for #258: one event must produce one red row, or the
// operator is invited to fix the same thing twice. An executor killed
// mid-install ends the engine row and leaves this one waiting, exactly as
// a failed download leaves engine_install pending.
func TestSetupIntegrationStaysPendingWhileAnEngineRowIsRed(t *testing.T) {
	ctx := context.Background()
	f := &fakeSetupProvider{}
	r, clock := leasedReconciler(t, f, "ollama", "")
	r.Apply(ctx, integrationsFrame("ollama", "", signer.IntegrationOpenClaw))
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Engine: "ollama",
		Phase: management.SetupExecutorPhaseInstalling,
	})
	clock.advance(setupExecutorTTL + time.Second)

	snap := r.snapshot(ctx)
	eng := stepByID(t, snap, setupStepEngineInstall)
	if eng.Status != signer.SetupStatusFailed || eng.ErrorCode != signer.SetupErrorExecutorGone {
		t.Fatalf("engine step = %+v, want failed/executor_gone", eng)
	}
	if step := stepByID(t, snap, setupStepIntegration); step.Status != signer.SetupStatusPending {
		t.Fatalf("integration step = %+v, want pending — the engine row already carries this failure", step)
	}
}

// --- #304: adopting an engine installed after the daemon booted -------
//
// The daemon resolves the engine binary once, at boot. On a fresh
// install there is none, so its engine-startup goroutine returns having
// done nothing and the process stays inert for its whole life: the
// executor installs ollama, the reconciler sees the binary appear and
// dispatches `ollama pull` at a server nobody ever started, the pull
// fails, and admission is once-per-desired-value so nothing retries.
// These pin the two triggers that now perform the promised "next engine
// start". All are product contracts.

// The executor finishing the engine install starts the engine. Done(engine)
// posts phase=done with an EMPTY step, which the reconciler normalises to
// engine_install — so the empty step is what this asserts on.
func TestSetupExecutorInstallDoneStartsTheEngine(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: false, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")

	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	if got := f.engineStartCount(); got != 0 {
		t.Fatalf("engine starts while still installing = %d, want 0", got)
	}

	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Phase: management.SetupExecutorPhaseDone, Engine: "ollama",
	})
	if got := f.engineStartCount(); got != 1 {
		t.Fatalf("engine starts after the install reported done = %d, want 1", got)
	}
}

// The trigger is an EDGE. The executor re-posts phase=done every 10 s for
// as long as it stays attached — which is the whole model download, up to
// the 8 h residency budget — so a level trigger would fire thousands of
// times a session and, with no in-flight pull dedup yet (#305), dispatch a
// duplicate multi-GB pull each time.
func TestSetupExecutorHeartbeatDoesNotRestartTheEngine(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: false, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")

	for range 5 {
		r.NoteExecutor(ctx, management.SetupExecutorRequest{
			Attached: true, Elevated: true,
			Phase: management.SetupExecutorPhaseDone, Engine: "ollama",
		})
	}
	if got := f.engineStartCount(); got != 1 {
		t.Fatalf("engine starts across 5 identical done posts = %d, want 1", got)
	}
}

// `engine_download: done` means the bytes are here and the install proper
// is next, in the same lease — there is nothing to start yet. This is the
// same boundary the install claim is deliberately kept across.
func TestSetupExecutorDownloadDoneDoesNotStartTheEngine(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: false, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")

	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Step: setupStepEngineDownload,
		Phase: management.SetupExecutorPhaseDone, Engine: "ollama",
		CompletedBytes: 1400, TotalBytes: 1400,
	})
	if got := f.engineStartCount(); got != 0 {
		t.Fatalf("engine starts on engine_download done = %d, want 0", got)
	}
}

// An integration report (waired#935) rides the same lease and says nothing
// about the engine.
func TestSetupExecutorIntegrationDoneDoesNotStartTheEngine(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateReady}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")

	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Step: setupStepIntegration,
		Phase: management.SetupExecutorPhaseDone,
	})
	if got := f.engineStartCount(); got != 0 {
		t.Fatalf("engine starts on integration done = %d, want 0", got)
	}
}

// The observable-state backstop, for an executor that died mid-install or
// a daemon that restarted while the wizard was running: the same false→true
// edge that re-admits the model also starts the engine, exactly once.
func TestSetupEngineAppearedStartsTheEngine(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: false, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	if got := f.engineStartCount(); got != 0 {
		t.Fatalf("engine starts before it was ever installed = %d, want 0", got)
	}

	f.setEngine(true, false)
	r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	if got := f.engineStartCount(); got != 1 {
		t.Fatalf("engine starts on the appearance edge = %d, want 1", got)
	}
	for range 3 {
		r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	}
	if got := f.engineStartCount(); got != 1 {
		t.Fatalf("engine starts after repeated frames = %d, want 1", got)
	}
}
