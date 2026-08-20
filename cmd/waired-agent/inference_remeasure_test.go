package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// waired-agent#821: the re-measurement of a model a pull just activated
// started at the ONE moment the engine could not be measured, was declined,
// and was never tried again — so the host went on serving a model nothing
// had ever measured.
//
// The trigger fires from runPullJob's tail, where endPull has not run yet
// (it is one of that function's deferred calls), so the pull is still in
// pullsInFlight and the benchmark's EngineQuiet gate refuses. Waiting at the
// endPull boundary instead is not enough either: runPullJob always sets
// retuneDeferred, so endPull always fires a serve reconcile, and a pending
// reconcile is busy for the same reason a running one is. Both conditions
// get a test below, because a fix that only handled the first would look
// right and still leave the reported host unmeasured.
//
// Deliberately built on hostCutoffProvider: it is the fixture in this
// package with a REAL ollama adapter behind a live http test server, which
// is what makes engineIsQuiet answer anything at all. The narrow providers
// (inference_activate_test.go) have p.ollama nil, where engineQuietForBench
// short-circuits to "quiet" and none of this is reachable.

const remeasureFixtureModel = "the-active-model"

// seedRemeasureFixture puts the fixture provider in the state the trigger
// fires from — the model is the committed selection and is ready on disk —
// and hands back the channel its (fake) benchmark writes to.
//
// benchRun is faked here on purpose: the subject of these tests is the WAIT
// in front of the run, not the measurement. The gate itself is covered
// unfaked by TestBenchmarkJob_DeclinesWhileTheEngineIsBusy below, which is
// the coverage this defect slipped through — every existing test on this
// path injected benchRun, and runBenchmarkJob skips RunBootBenchmark
// entirely when it is set.
func seedRemeasureFixture(t *testing.T, p *agentInferenceProvider) <-chan struct{} {
	t.Helper()
	if err := p.store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{
			remeasureFixtureModel: {State: catalog.ModelStateReady, VariantID: "q4"},
		}
		s.Active = &catalog.ActiveSelection{ModelID: remeasureFixtureModel, VariantID: "q4"}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	runs := make(chan struct{}, 8)
	p.benchRun = func(context.Context) BenchResult {
		select {
		case runs <- struct{}{}:
		default:
		}
		return BenchResult{
			TokensPerSec: 80, Capacity: 2,
			ModelID: remeasureFixtureModel, Outcome: benchOutcomeMeasured,
		}
	}
	// Nothing on file describes this model — the state the trigger exists
	// to end. A measurement of the PREVIOUS model is what the reported host
	// actually had.
	p.SetLastBench(BenchResult{
		TokensPerSec: 12, Capacity: 1,
		ModelID: "the-previous-model", Outcome: benchOutcomeMeasured,
	})
	shrinkRemeasureTimers(t)
	return runs
}

// shrinkRemeasureTimers paces the wait for a test rather than for a host.
// The bound itself is left alone; the test that needs a short one shrinks it
// explicitly, because "how long before giving up" is that test's subject.
func shrinkRemeasureTimers(t *testing.T) {
	t.Helper()
	poll, pause := remeasureSettlePoll, remeasureRetryPause
	remeasureSettlePoll = 5 * time.Millisecond
	remeasureRetryPause = 5 * time.Millisecond
	t.Cleanup(func() {
		remeasureSettlePoll, remeasureRetryPause = poll, pause
	})
}

// The reported condition, exactly: the trigger fires while the pull that
// caused it is still registered. Nothing may be measured then — and the
// measurement must still happen once it is not.
func TestRemeasureForActiveModel_WaitsForThePullToFinish(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	runs := seedRemeasureFixture(t, p)

	// What runPullJob's tail looks like from here: endPull is deferred, so
	// this pull is still in the registry when the trigger runs.
	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{remeasureFixtureModel: {modelID: remeasureFixtureModel}}
	p.pullMu.Unlock()

	done := p.remeasureForActiveModel(remeasureFixtureModel)
	if done == nil {
		t.Fatal("no attempt started for a model nothing on file describes")
	}

	select {
	case <-runs:
		t.Fatal("measured while the pull was still in flight — the engine gates decline " +
			"there, which is how #821's single attempt was spent on nothing")
	case <-time.After(150 * time.Millisecond):
	}

	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{}
	p.pullMu.Unlock()

	select {
	case <-runs:
	case <-time.After(waitBackstop):
		t.Fatal("the pull left the registry and the model was never measured — this is " +
			"#821: the one attempt was made at the one moment it could not succeed")
	}
	waitRemeasureDone(t, done)
}

// The second half of the same defect, and the reason retrying at the
// endPull boundary would not have been a fix: runPullJob stores
// retuneDeferred unconditionally, so the last endPull always fires a serve
// reconcile — a stop-and-restart of the engine — and engineIsQuiet counts a
// PENDING one as busy. A retry aimed at the boundary lands inside this
// window.
func TestRemeasureForActiveModel_WaitsForAPendingEngineReconcile(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	runs := seedRemeasureFixture(t, p)

	p.engineReconcileInFlight.Store(true)

	done := p.remeasureForActiveModel(remeasureFixtureModel)
	if done == nil {
		t.Fatal("no attempt started for a model nothing on file describes")
	}
	select {
	case <-runs:
		t.Fatal("measured with a reconcile in flight — the restart it is about to " +
			"perform is what kills the measurement")
	case <-time.After(150 * time.Millisecond):
	}

	p.engineReconcileInFlight.Store(false)
	select {
	case <-runs:
	case <-time.After(waitBackstop):
		t.Fatal("the reconcile finished and the model was never measured")
	}
	waitRemeasureDone(t, done)
}

// The wait and the job's own gate ask the same question at two different
// moments, so a run can still be declined after the wait said yes — a
// request, a sibling pull or an engine bounce arriving in between. That
// second reading is the one that decides whether anything is measured, so
// it gets a retry rather than ending the attempt.
func TestRemeasureForActiveModel_RetriesARunTheEngineGatesDeclined(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	seedRemeasureFixture(t, p)

	var calls atomic.Int32
	p.benchRun = func(context.Context) BenchResult {
		if calls.Add(1) == 1 {
			// What RunBootBenchmark returns from its EngineQuiet gate:
			// notReadyBenchResult. Failed, so nothing on file describes
			// the model afterwards either.
			return BenchResult{
				Capacity: unmeasuredCapacity, Failed: true,
				Err:     "engine busy: a download or an engine restart is in flight",
				Outcome: benchOutcomeEngineNotReady,
			}
		}
		return BenchResult{
			TokensPerSec: 80, Capacity: 2,
			ModelID: remeasureFixtureModel, Outcome: benchOutcomeMeasured,
		}
	}

	done := p.remeasureForActiveModel(remeasureFixtureModel)
	if done == nil {
		t.Fatal("no attempt started")
	}
	waitRemeasureDone(t, done)

	if got := calls.Load(); got != 2 {
		t.Fatalf("%d benchmark run(s), want 2 — a declined run must be retried, which is "+
			"the whole of #821", got)
	}
	if p.activeModelNeedsMeasurement(remeasureFixtureModel) {
		t.Fatal("the attempt finished with the model still unmeasured")
	}
}

// A host that never goes quiet is left unmeasured rather than measured
// badly, and it stops rather than spinning. Same bias as
// startHostSpeedMeasurement's give-up: the next activation, or the next
// boot, tries again.
func TestRemeasureForActiveModel_GivesUpWhenTheEngineNeverSettles(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	runs := seedRemeasureFixture(t, p)

	prev := remeasureSettleWait
	remeasureSettleWait = 50 * time.Millisecond
	t.Cleanup(func() { remeasureSettleWait = prev })

	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{"never-finishes": {modelID: "never-finishes"}}
	p.pullMu.Unlock()

	done := p.remeasureForActiveModel(remeasureFixtureModel)
	if done == nil {
		t.Fatal("no attempt started")
	}
	waitRemeasureDone(t, done)

	select {
	case <-runs:
		t.Fatal("measured anyway, on an engine that never went quiet")
	default:
	}
	if !p.activeModelNeedsMeasurement(remeasureFixtureModel) {
		t.Fatal("something recorded a measurement without taking one")
	}
}

// Someone else getting there first ends the attempt. The single-flight
// startBenchmarkJob would JOIN a run already going, so without this the
// wait would end in a redundant second measurement of a model that has just
// been measured.
func TestRemeasureForActiveModel_StandsDownWhenSomethingElseMeasuresIt(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	runs := seedRemeasureFixture(t, p)

	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{"still-going": {modelID: "still-going"}}
	p.pullMu.Unlock()

	done := p.remeasureForActiveModel(remeasureFixtureModel)
	if done == nil {
		t.Fatal("no attempt started")
	}

	// The boot benchmark lands while this one is still waiting.
	p.SetLastBench(BenchResult{
		TokensPerSec: 44, Capacity: 2,
		ModelID: remeasureFixtureModel, Outcome: benchOutcomeMeasured,
	})
	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{}
	p.pullMu.Unlock()

	waitRemeasureDone(t, done)
	select {
	case <-runs:
		t.Fatal("re-measured a model that had just been measured while the wait ran")
	default:
	}
}

// A model that stopped being the selection while the wait ran is not this
// attempt's to measure: whatever activation replaced it fires its own
// trigger, and measuring the old id here would put a number on file under a
// model this host no longer serves — the exact confusion #783 reported.
func TestRemeasureForActiveModel_StandsDownWhenTheSelectionMovedOn(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	runs := seedRemeasureFixture(t, p)

	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{"still-going": {modelID: "still-going"}}
	p.pullMu.Unlock()

	done := p.remeasureForActiveModel(remeasureFixtureModel)
	if done == nil {
		t.Fatal("no attempt started")
	}

	if err := p.store.Update(func(s *catalog.State) {
		s.Models["a-later-model"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4"}
		s.Active = &catalog.ActiveSelection{ModelID: "a-later-model", VariantID: "q4"}
	}); err != nil {
		t.Fatalf("move the selection on: %v", err)
	}
	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{}
	p.pullMu.Unlock()

	waitRemeasureDone(t, done)
	select {
	case <-runs:
		t.Fatal("measured a model that is no longer the active selection")
	default:
	}
}

// The gate itself, UNFAKED — the coverage this defect slipped through.
//
// Every test that reached remeasureForActiveModel set p.benchRun, and
// runBenchmarkJob skips RunBootBenchmark entirely when it is set, so no test
// in this package had ever run the EngineQuiet gate on this path. The
// #810 test written to prove the trigger is reached says as much beside
// itself: a draft that DEADLOCKED the daemon here passed the whole suite.
//
// This pins the mechanism rather than the fix. If the gate ever stops
// declining a busy engine the wait in front of it becomes pointless, and
// nothing else in the package would notice.
func TestBenchmarkJob_DeclinesWhileTheEngineIsBusy(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	if err := p.store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{
			remeasureFixtureModel: {State: catalog.ModelStateReady, VariantID: "q4"},
		}
		s.Active = &catalog.ActiveSelection{ModelID: remeasureFixtureModel, VariantID: "q4"}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	// Anti-vacuity: if the readiness gate in front of the quiet gate
	// refuses, the run never reaches the condition this test is about and
	// the assertion below would pass for the wrong reason.
	if ready, _ := p.EngineReady(); !ready {
		t.Fatal("the fixture engine is not ready, so the run would stop at the readiness " +
			"gate and never reach the quiet one this test is about")
	}

	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{remeasureFixtureModel: {modelID: remeasureFixtureModel}}
	p.pullMu.Unlock()

	waitDone(t, p.startBenchmarkJob(0))

	p.benchJobMu.Lock()
	kind := p.benchJobOutcomeKind
	p.benchJobMu.Unlock()
	if kind != benchOutcomeEngineNotReady {
		t.Fatalf("outcome %q with a pull in flight, want %q — the gate #821 is built "+
			"around no longer declines a busy engine", kind, benchOutcomeEngineNotReady)
	}
}

func waitRemeasureDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(waitBackstop):
		t.Fatal("the re-measurement attempt never finished")
	}
}
