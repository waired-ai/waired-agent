package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// providerLifetime gives a provider fixture the three things the removal
// of its state dir depends on, and gives them TOGETHER: the directory
// itself, an agent context that can be cancelled, and — through arm — the
// one cleanup that stops the engine, cancels, and waits for the work the
// provider detached.
//
// Together, because every recurrence of waired-agent#925 so far has been
// a fixture that took one or two of the three. The failure is always the
// same and always says nothing about its cause:
//
//	TempDir RemoveAll cleanup: unlinkat /var/folders/.../TestX: directory not empty
//
// Every assertion passes; only the cleanup fails; it is green on a rerun;
// and it appears on the windows and seeded-host legs and never on linux.
//
// WHAT WRITES. spawnPull's defers are LIFO and its own comment states the
// ordering — endPull runs BEFORE pullsWG.Done — so `waitForPulls()`
// returning implies endPull has already run, and endPull consumes the
// deferred retune/swap intent through requestEngineReconcile, which does
// `go reconcileEngineServe(p.agentCtx)` and joins nothing. That goroutine
// writes the Active selection with store.Update, and catalog's saveLocked
// calls secrets.SecureDir first (internal/catalog/local.go), so a late
// write does not merely touch the directory — it RE-CREATES it, which is
// what the kernel reports as not-empty.
//
// NOT a product defect: in the daemon that context outlives every pull,
// and no directory is being removed under it.
//
// WHAT ELSE WRITES, and why watching engineReconcileInFlight is not
// enough. Three writers sit outside that flag, and two of them were found
// only after the flake survived two rounds of fixing it:
//
//   - reconcileEngineServe's `defer p.warmServingModel()` runs BEFORE its
//     `defer p.engineReconcileInFlight.Store(false)`, and warmServingModel
//     only STARTS a goroutine (inference_warm.go) before returning. So the
//     reconcile flag can be false with a detached warm-up still reading
//     the provider's store. Hence warmInFlight.
//   - runPullJob calls remeasureForActiveModel, whose own comment says the
//     quiet part out loud: "a test that does not wait leaves a goroutine
//     writing into its temp directory after it has returned". That
//     goroutine ends at remeasureWhenQuiet -> startBenchmarkJob ->
//     runBenchmarkJob, which persists its completion record with
//     store.Update (inference_recommendation.go). NEITHER flag covers it.
//     Hence benchJobDone, which runBenchmarkJob clears after that write
//     and whose channel it closes on every path including the early one.
//   - a spawned engine nobody stopped leaves (*OllamaAdapter).superviseChild
//     parked on proc.Done(). When the child does die — after the test —
//     markUnhealthy calls back into onEngineUnhealthy, whose first attempt
//     has zero delay, and that asks for another reconcile. Stopping the
//     engine retires the process generation, so the supervisor sees
//     `stale` and returns instead. Hence the Stop.
//
// WHY arm IS SEPARATE, and why it is not a second thing to remember. The
// state dir has to be taken FIRST, so its removal is registered first and
// therefore runs LAST; the join has to be registered AFTER the fixture's
// httptest server, so it runs BEFORE that server closes — the engine is
// still being stopped and the reconcile is still talking to it. One call
// cannot sit at both points, so arm is the second half. Forgetting it is
// not a silent omission the way forgetting a helper was: arm is a value,
// and Go does not compile a function that declares one and never uses it.
//
// The bound is the same two seconds the first version used; once the
// context is cancelled the reconcile has nothing slow left to do.
func providerLifetime(t *testing.T) (stateDir string, agentCtx context.Context, arm func(*agentInferenceProvider)) {
	t.Helper()
	// FIRST, so this cleanup is registered first and runs last: everything
	// arm waits for has to be finished with the directory before it goes.
	stateDir = t.TempDir()
	agentCtx, cancel := context.WithCancel(context.Background())
	return stateDir, agentCtx, func(p *agentInferenceProvider) {
		t.Helper()
		t.Cleanup(func() {
			// Ordered inside one cleanup rather than spread over several,
			// because the order is not a preference: the engine has to be
			// stopped while the fixture's httptest server is still
			// answering, and the cancel has to come before the wait or the
			// wait is for work nothing has told to stop.
			if p != nil && p.ollama != nil {
				ctx, stopCancel := context.WithTimeout(context.Background(), waitBackstop)
				_ = p.ollama.Stop(ctx)
				stopCancel()
			}
			cancel()
			if p == nil {
				return
			}
			// Two consecutive quiet samples, because a reconcile that has
			// been asked for is not yet visible in the flag, and
			// chaseEngineRespawn's next ask can land between any two
			// samples.
			quiet := 0
			for range 400 {
				if p.detachedWorkQuiet() {
					quiet++
					if quiet == 2 {
						return
					}
				} else {
					quiet = 0
				}
				time.Sleep(5 * time.Millisecond)
			}
			// Deliberately not a t.Fatal: this runs in a cleanup, the run
			// is already over, and work still going after two seconds is
			// worth saying out loud rather than turning into a second
			// failure on top of whatever the test itself reported.
			t.Logf("provider work still in flight after 2s (reconcile=%v warm=%v start=%v bench=%v); "+
				"the state dir removal below may race it",
				p.engineReconcileInFlight.Load(), p.warmInFlight.Load(),
				p.engineStartInFlight.Load(), !p.benchJobQuiet())
		})
	}
}

// detachedWorkQuiet reports whether every piece of work this provider
// detaches — and that can still touch its state dir — has finished.
func (p *agentInferenceProvider) detachedWorkQuiet() bool {
	return !p.engineReconcileInFlight.Load() &&
		!p.warmInFlight.Load() &&
		!p.engineStartInFlight.Load() &&
		p.benchJobQuiet()
}

// benchJobQuiet reports whether the detached benchmark job is finished.
//
// Read through the channel rather than through benchJobDone being nil:
// the field is cleared at the END of the body, after the store.Update
// that persists the completion record, but the guards above that body can
// return early and leave it set. close(done) is deferred, so the channel
// answers on every path.
func (p *agentInferenceProvider) benchJobQuiet() bool {
	p.benchJobMu.Lock()
	done := p.benchJobDone
	p.benchJobMu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// The warm half of the contract, pinned separately because it is the half
// the first version did not have: a cleanup that watched only the
// reconcile flag returned while a detached warm-up was still reading the
// provider's store, and looked identical from every caller.
func TestProviderLifetime_WaitsForADetachedWarmUp(t *testing.T) {
	p := &agentInferenceProvider{}
	p.warmInFlight.Store(true) // reconcileEngineServe started one on its way out

	finished := make(chan struct{})
	t.Run("holds the cleanup open", func(t *testing.T) {
		_, agentCtx, arm := providerLifetime(t)
		go func() {
			<-agentCtx.Done()
			time.Sleep(50 * time.Millisecond)
			p.warmInFlight.Store(false)
			close(finished)
		}()
		arm(p)
	})

	select {
	case <-finished:
	default:
		t.Fatal("the cleanup returned while a warm-up was still in flight — " +
			"reconcileEngineServe clears its own flag after merely STARTING one " +
			"(waired-agent#925)")
	}
}

// The helper's own contract, since the fixtures that use it cannot assert
// it from inside: cancel, and then WAIT. A cleanup that only cancelled
// would look identical from every caller and would leave the race exactly
// where it was.
//
// The subject runs inside a subtest so its cleanup has a boundary this
// test can observe from the outside.
func TestProviderLifetime_WaitsForTheReconcileToFinish(t *testing.T) {
	p := &agentInferenceProvider{}
	p.engineReconcileInFlight.Store(true)

	// Stands in for reconcileEngineServe: it finishes some time after the
	// context is cancelled, not at the moment of cancellation — which is
	// the whole reason a cancel alone is not enough.
	finished := make(chan struct{})
	cancelled := make(chan struct{})
	t.Run("holds the cleanup open", func(t *testing.T) {
		_, agentCtx, arm := providerLifetime(t)
		go func() {
			<-agentCtx.Done()
			close(cancelled)
			time.Sleep(50 * time.Millisecond)
			p.engineReconcileInFlight.Store(false)
			close(finished)
		}()
		arm(p)
	})

	select {
	case <-cancelled:
	default:
		t.Fatal("the cleanup never cancelled the agent context")
	}
	select {
	case <-finished:
	default:
		t.Fatal("the cleanup returned while the reconcile was still in flight — a cancel " +
			"alone leaves the state-dir write racing the TempDir removal (waired-agent#925)")
	}
}

// The writer neither flag can see. runPullJob detaches
// remeasureForActiveModel on the very pull a fixture drives, and the
// benchmark it ends at persists its completion record with store.Update
// on a context of its own. Both engine flags read quiet for the whole of
// it, which is why a cleanup that watched only those returned while a
// write into the state dir was still to come.
func TestProviderLifetime_WaitsForTheBenchmarkThatWritesTheStore(t *testing.T) {
	p := &agentInferenceProvider{}
	done := make(chan struct{})
	p.benchJobDone = done

	wrote := make(chan struct{})
	t.Run("holds the cleanup open", func(t *testing.T) {
		_, agentCtx, arm := providerLifetime(t)
		go func() {
			<-agentCtx.Done()
			time.Sleep(50 * time.Millisecond)
			// The order runBenchmarkJob itself has: persist, clear the
			// field, and only then close the channel.
			close(wrote)
			p.benchJobMu.Lock()
			p.benchJobDone = nil
			p.benchJobMu.Unlock()
			close(done)
		}()
		arm(p)
	})

	select {
	case <-wrote:
	default:
		t.Fatal("the cleanup returned before the detached benchmark had written the store — " +
			"engineReconcileInFlight and warmInFlight are both quiet for the whole of it " +
			"(waired-agent#925)")
	}
}

// The ordering half, which is not a style point: the state dir has to be
// taken before arm registers anything, so that under LIFO its removal is
// the last cleanup to run. A fixture that takes it afterwards — which is
// what probeOrderProvider did — has the directory removed while the join
// is still waiting, and the late write then re-creates it.
//
// Asserted from inside the wait, because that is the only moment where
// the two orders differ.
func TestProviderLifetime_StateDirOutlivesTheWait(t *testing.T) {
	p := &agentInferenceProvider{}
	p.engineReconcileInFlight.Store(true)

	var existedDuringTheWait bool
	t.Run("holds the cleanup open", func(t *testing.T) {
		stateDir, agentCtx, arm := providerLifetime(t)
		go func() {
			<-agentCtx.Done()
			time.Sleep(20 * time.Millisecond)
			_, err := os.Stat(stateDir)
			existedDuringTheWait = err == nil
			p.engineReconcileInFlight.Store(false)
		}()
		arm(p)
	})

	if !existedDuringTheWait {
		t.Fatal("the state dir was already gone while the join was still waiting — " +
			"providerLifetime must take it before arm registers the join, or the " +
			"removal runs first and the late write re-creates it (waired-agent#925)")
	}
}

// The third obligation, and the one with no flag of its own: an engine
// the fixture started and nobody stopped leaves
// (*OllamaAdapter).superviseChild parked on proc.Done() — a live engine
// supervisor belonging to a test that has finished. When the child does
// die, markUnhealthy reads the engine log and calls back into
// onEngineUnhealthy, whose first recovery attempt has no delay at all, so
// a reconcile is asked for and the state dir is written long after the
// test returned. Stop retires the process generation, so the supervisor
// sees `stale` and returns instead.
func TestProviderLifetime_StopsTheEngineTheFixtureStarted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)
	sp := &fakeSpawner{}
	a := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: sp, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 50 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p := &agentInferenceProvider{ollama: a}

	t.Run("tears the fixture down", func(t *testing.T) {
		_, _, arm := providerLifetime(t)
		arm(p)
	})

	proc := sp.lastProc()
	if proc == nil {
		t.Fatal("the fixture never spawned an engine; there is nothing to stop")
	}
	select {
	case <-proc.Done():
	default:
		t.Fatal("the engine the fixture started is still running after its lifetime ended — " +
			"superviseChild stays parked on it and reports its eventual death as a crash, " +
			"which asks for a reconcile with no delay (waired-agent#925)")
	}
}
