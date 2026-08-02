package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// startFailAdapter builds a spawned adapter over a health server that never
// answers OK, so every start attempt ends at the readiness deadline.
//
// Both callbacks are wired on purpose. The whole point of the split is that
// "it never came up" and "it was serving and died" are different events, and
// a test that watched only one could not tell them apart — which is how the
// first of the two came to have no reporting path at all (#310).
func startFailAdapter(t *testing.T, opts ...func(*OllamaConfig)) (*OllamaAdapter, *unhealthyRecorder, *unhealthyRecorder) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)
	died, neverCameUp := &unhealthyRecorder{}, &unhealthyRecorder{}
	cfg := OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: &fakeSpawner{}, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 2,
		// The spawned path waits StartupReadyTimeout, not
		// HealthMaxFails*HealthInterval — a cold start is not a crash. Shrink
		// it so the failure this file is about is a fast, deterministic one.
		StartupReadyTimeout: 150 * time.Millisecond,
		StopTimeout:         50 * time.Millisecond,
		LogDir:              t.TempDir(),
		OnUnhealthy:         died.record,
		OnStartFailed:       neverCameUp.record,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return NewOllamaAdapter(cfg), died, neverCameUp
}

// readyAdapterWithBothCallbacks is livenessAdapter plus an OnStartFailed
// recorder, for the negative control below.
func readyAdapterWithBothCallbacks(t *testing.T) (*OllamaAdapter, *fakeSpawner, *unhealthyRecorder, *unhealthyRecorder) {
	t.Helper()
	srv := okHealthServer(t)
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	died, neverCameUp := &unhealthyRecorder{}, &unhealthyRecorder{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout:   50 * time.Millisecond,
		LogDir:        t.TempDir(),
		OnUnhealthy:   died.record,
		OnStartFailed: neverCameUp.record,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got := a.Health(context.Background()).State; got != StateReady {
		t.Fatalf("precondition: state = %s, want ready", got)
	}
	return a, spawner, died, neverCameUp
}

// TestStartFailureIsEvidence is the table over the decision itself.
//
// PRODUCT CONTRACT for the parked rows: a hard stop that lands during a slow
// readiness wait tears the child down, and the error that comes back is the
// operator's own decision working. Charging it as a strike would let `waired
// inference engine stop` help spend the recovery budget on an engine that was
// never broken. The integration tests can only reach that case by racing the
// readiness wait, which is why the judgement lives here.
func TestStartFailureIsEvidence(t *testing.T) {
	boom := errors.New("ollama: process exited during startup: signal: killed")
	cases := []struct {
		name   string
		err    error
		parked bool
		want   bool
	}{
		{"a start that failed on a live engine", boom, false, true},
		{"a start that succeeded", nil, false, false},
		{"a start torn down by a park that landed mid-wait", boom, true, false},
		{"the leader's own parked re-check", ErrEngineParked, false, false},
		{"a parked re-check wrapped by a caller", fmt.Errorf("bringing up: %w", ErrEngineParked), false, false},
		{"parked, and it somehow succeeded anyway", nil, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := startFailureIsEvidence(c.err, c.parked); got != c.want {
				t.Errorf("startFailureIsEvidence(%v, parked=%v) = %v, want %v", c.err, c.parked, got, c.want)
			}
		})
	}
}

// TestOllamaAdapter_StartThatNeverCameUp_Notifies is the core of the daemon
// half of #310. PRODUCT CONTRACT: a start attempt that ends without the
// engine serving is reported, with the reason, exactly once.
//
// Before this the only report was markUnhealthy, which returns early unless
// the adapter is currently StateReady — so an engine killed at exec recorded
// nothing, and every consumer went on treating it as "not ready yet".
func TestOllamaAdapter_StartThatNeverCameUp_Notifies(t *testing.T) {
	a, died, neverCameUp := startFailAdapter(t)
	defer func() { _ = a.Stop(context.Background()) }()

	if err := a.EnsureRunning(context.Background()); err == nil {
		t.Fatal("EnsureRunning succeeded against a server that never answers OK")
	}
	waitFor(t, 2*time.Second, "OnStartFailed to fire", func() bool { return neverCameUp.count() == 1 })
	if got := neverCameUp.last(); got == "" {
		t.Error("OnStartFailed fired with an empty detail; the reason is the whole point")
	} else if !strings.Contains(got, "ollama") {
		t.Errorf("OnStartFailed detail = %q, want the engine's own account of the failure", got)
	}
	// The other half of the contract: this is NOT a death of a serving
	// engine, so the handler that schedules restarts must not hear about it.
	if n := died.count(); n != 0 {
		t.Errorf("OnUnhealthy fired %d times for a start that never came up, want 0 (detail: %q)", n, died.last())
	}
}

// TestOllamaAdapter_StartFailure_OnlyTheLeaderNotifies pins the single-flight
// property. PRODUCT CONTRACT: a burst of gateway requests joining one failing
// start is ONE attempt. Firing per caller would let request volume, rather
// than engine health, decide when the daemon gives up.
func TestOllamaAdapter_StartFailure_OnlyTheLeaderNotifies(t *testing.T) {
	a, _, neverCameUp := startFailAdapter(t)
	defer func() { _ = a.Stop(context.Background()) }()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.EnsureRunning(context.Background())
		}()
	}
	wg.Wait()
	// Give any extra callback the same chance to land as the real one had.
	time.Sleep(50 * time.Millisecond)
	if n := neverCameUp.count(); n != 1 {
		t.Errorf("OnStartFailed fired %d times for 8 concurrent starts, want 1", n)
	}
}

// TestOllamaAdapter_ParkedRefusal_IsNotAStartFailure. PRODUCT CONTRACT: a
// hard stop is the operator's decision (#186), and EnsureRunning refusing to
// revive a parked engine is that decision working. Counting it as a strike
// would let `waired inference engine stop` plus request traffic latch the
// engine unrecoverable.
func TestOllamaAdapter_ParkedRefusal_IsNotAStartFailure(t *testing.T) {
	a, _, _, neverCameUp := readyAdapterWithBothCallbacks(t)

	if err := a.Park(context.Background()); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if err := a.EnsureRunning(context.Background()); !errors.Is(err, ErrEngineParked) {
		t.Fatalf("EnsureRunning on a parked engine = %v, want ErrEngineParked", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := neverCameUp.count(); n != 0 {
		t.Errorf("OnStartFailed fired %d times on a parked engine, want 0 (detail: %q)", n, neverCameUp.last())
	}
}

// TestOllamaAdapter_LatchedRefusal_IsNotAStartFailure. PRODUCT CONTRACT: once
// the latch is set, EnsureRunning's refusal is the latch working. Reporting it
// would have the latch re-arm itself on every request for as long as traffic
// flows, so its reason could never age out and its strike count would be
// unbounded.
func TestOllamaAdapter_LatchedRefusal_IsNotAStartFailure(t *testing.T) {
	a, _, neverCameUp := startFailAdapter(t)

	a.LatchFailed("engine crashed repeatedly; automatic restart disabled")
	if err := a.EnsureRunning(context.Background()); !errors.Is(err, ErrEngineUnrecoverable) {
		t.Fatalf("EnsureRunning on a latched engine = %v, want ErrEngineUnrecoverable", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := neverCameUp.count(); n != 0 {
		t.Errorf("OnStartFailed fired %d times on a latched engine, want 0 (detail: %q)", n, neverCameUp.last())
	}
}

// TestOllamaAdapter_SuccessfulStart_DoesNotNotify is the guard against the
// obvious over-correction: firing on every EnsureRunning would latch a
// perfectly healthy engine after four ordinary starts.
func TestOllamaAdapter_SuccessfulStart_DoesNotNotify(t *testing.T) {
	a, _, _, neverCameUp := readyAdapterWithBothCallbacks(t)
	defer func() { _ = a.Stop(context.Background()) }()

	time.Sleep(50 * time.Millisecond)
	if n := neverCameUp.count(); n != 0 {
		t.Errorf("OnStartFailed fired %d times on a successful start, want 0 (detail: %q)", n, neverCameUp.last())
	}
}

// TestOllamaAdapter_CrashAfterReady_StaysOnTheUnhealthyPath is the NEGATIVE
// CONTROL for the split. RECORD OF TODAY'S BEHAVIOUR, deliberately pinned: a
// serving engine that dies must keep going through OnUnhealthy, whose handler
// restarts it. Routing it to OnStartFailed instead would silently disable
// crash recovery — the thing waired-agent#29 exists to provide.
func TestOllamaAdapter_CrashAfterReady_StaysOnTheUnhealthyPath(t *testing.T) {
	a, spawner, died, neverCameUp := readyAdapterWithBothCallbacks(t)
	defer func() { _ = a.Stop(context.Background()) }()

	spawner.process.exit(errors.New("signal: killed"))

	waitFor(t, 2*time.Second, "OnUnhealthy to fire", func() bool { return died.count() == 1 })
	if n := neverCameUp.count(); n != 0 {
		t.Errorf("OnStartFailed fired %d times for a crash after Ready, want 0 (detail: %q)", n, neverCameUp.last())
	}
}

// TestOllamaAdapter_DeadRunnerRepliesFromADownEngine_ChargeNothing is the
// other half of that control, and the reason markUnhealthy's StateReady guard
// stays exactly as strict as it is.
//
// PRODUCT CONTRACT: once the engine is already known down, the 5xx bodies its
// own failure produces are echoes, not new evidence. Accepting them would let
// request volume spend the recovery budget — and would route a startup death
// to the handler that answers by restarting, which is what this change
// deliberately did NOT do.
func TestOllamaAdapter_DeadRunnerRepliesFromADownEngine_ChargeNothing(t *testing.T) {
	a, died, neverCameUp := startFailAdapter(t)

	if err := a.EnsureRunning(context.Background()); err == nil {
		t.Fatal("EnsureRunning succeeded against a server that never answers OK")
	}
	waitFor(t, 2*time.Second, "the failed start to be reported", func() bool { return neverCameUp.count() == 1 })
	if got := a.Health(context.Background()).State; got != StateFailed {
		t.Fatalf("precondition: state = %s, want failed", got)
	}

	for range 5 {
		a.ReportUpstreamFailure(500, []byte(`{"error":"model runner has unexpectedly stopped"}`))
	}
	time.Sleep(50 * time.Millisecond)
	if n := died.count(); n != 0 {
		t.Errorf("OnUnhealthy fired %d times for an engine that was already down, want 0 (detail: %q)",
			n, died.last())
	}
}

// TestOllamaAdapter_FailureLatchedReason_SurvivesAStop pins the defect that
// made the wizard paint a red engine row with no reason on it. PRODUCT
// CONTRACT: the latch and its reason have one lifetime, ending at
// ClearFailure — a Stop in between (model switch, reconcile bounce, park)
// must not take the reason away, even though it does overwrite Health().
func TestOllamaAdapter_FailureLatchedReason_SurvivesAStop(t *testing.T) {
	a, _, _, _ := readyAdapterWithBothCallbacks(t)
	const reason = "engine crashed 4 times within 5m0s; automatic restart disabled"

	a.LatchFailed(reason)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The precondition, asserted so this test fails loudly rather than
	// vacuously if Stop ever stops clobbering a.state.
	if h := a.Health(context.Background()); h.LastErr != "" {
		t.Fatalf("precondition: Health().LastErr = %q after Stop, want it clobbered", h.LastErr)
	}
	latched, got := a.FailureLatchedReason()
	if !latched {
		t.Error("FailureLatchedReason() latched = false after LatchFailed + Stop")
	}
	if got != reason {
		t.Errorf("FailureLatchedReason() reason = %q, want %q", got, reason)
	}
	if !a.FailureLatched() {
		t.Error("FailureLatched() = false; it must agree with FailureLatchedReason()")
	}

	a.ClearFailure()
	if latched, got := a.FailureLatchedReason(); latched || got != "" {
		t.Errorf("after ClearFailure: (%v, %q), want (false, \"\")", latched, got)
	}
}
