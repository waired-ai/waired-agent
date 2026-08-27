package tray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// --- waired-agent#1031 / #1045: what each way out of the tray does ---

// TestPlanShutdown pins a PRODUCT CONTRACT: every way the tray goes away
// winds this machine down.
//
// #316 ratified the wind-down for the Quit menu, on the grounds that
// peers must stop being routed to a machine while nobody is at the
// keyboard. The owner ruled on 2026-08-27 (waired-agent#1031/#1045) that
// a signal carries the same meaning: signing out of the desktop is that
// same event arriving by another route, and the session manager SIGTERMs
// the tray as a child of the session.
//
// waired-agent#1046 adds a restart cause, which must NOT wind down — an
// update that puts the tray back a second later has taken nobody away
// from the keyboard. That row lands here.
func TestPlanShutdown(t *testing.T) {
	for _, tc := range []struct {
		cause    shutdownCause
		name     string
		windDown bool
	}{
		{causeQuitMenu, "quit-menu", true},
		{causeSignal, "signal", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cause.String(); got != tc.name {
				t.Errorf("String() = %q, want %q", got, tc.name)
			}
			if got := planShutdown(tc.cause); got.WindDown != tc.windDown {
				t.Errorf("planShutdown(%v).WindDown = %v, want %v", tc.name, got.WindDown, tc.windDown)
			}
		})
	}
}

// TestShutdown_WindsDownThenQuits pins the ORDER, which is the half that
// matters: the mesh withdrawal has to land before the engine it would
// have routed peers to disappears, and the GUI must not be torn down
// until both have been attempted. Same contract as
// TestOnQuit_SuspendsSharingThenStopsEngine, one layer up.
func TestShutdown_WindsDownThenQuits(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := &tray{cli: newTestClient(srv.URL)}
	quits := 0
	tr.shutdown(planShutdown(causeSignal), func() {
		mu.Lock()
		calls = append(calls, "quit")
		mu.Unlock()
		quits++
	})

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	want := []string{
		"/waired/v1/inference/share/suspend",
		"/waired/v1/inference/engine/stop",
		"quit",
	}
	if len(got) != len(want) {
		t.Fatalf("shutdown did %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d = %q, want %q", i, got[i], want[i])
		}
	}
	if quits != 1 {
		t.Errorf("quit called %d times, want exactly 1", quits)
	}
}

// TestShutdown_QuitsWhenTheDaemonIsUnreachable is the contract an
// uninstaller depends on: a dead or wedged daemon must not keep the tray
// on screen. The wind-down is best-effort and bounded (quitBudget); the
// quit is not conditional on it.
func TestShutdown_QuitsWhenTheDaemonIsUnreachable(t *testing.T) {
	// A server that is closed immediately: dialling it fails fast, which
	// is the "daemon is gone" shape rather than the "daemon is wedged"
	// one. The budget itself is pinned by
	// TestClient_EngineWritesUseTheLongBudget.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	tr := &tray{cli: newTestClient(url)}
	done := make(chan struct{})
	go func() {
		tr.shutdown(planShutdown(causeSignal), func() { close(done) })
	}()
	select {
	case <-done:
	case <-time.After(ShutdownBudget):
		t.Fatalf("shutdown did not reach quit within %v with the daemon down", ShutdownBudget)
	}
}

// TestShutdown_SkipsTheWindDownWhenThePlanSaysSo is a record of today's
// behaviour and the landing pad for waired-agent#1046's restart cause:
// the plan, not the call site, decides whether the daemon is touched.
func TestShutdown_SkipsTheWindDownWhenThePlanSaysSo(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := &tray{cli: newTestClient(srv.URL)}
	quits := 0
	tr.shutdown(shutdownPlan{WindDown: false}, func() { quits++ })

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("a plan with no wind-down still called the daemon: %v", calls)
	}
	if quits != 1 {
		t.Errorf("quit called %d times, want exactly 1", quits)
	}
}

// TestWatchShutdown_IsInertUntilTheContextIsCancelled, then quits
// exactly once: the watcher runs for the whole life of a healthy tray,
// so it must touch nothing until a signal arrives — and then it must go.
//
// It drives watchShutdownWith rather than watchShutdown deliberately.
// systray.Quit is process-global and one-shot, and on Windows it
// dereferences a callback that only systray.Register sets, so a test
// that reached the real one would panic rather than assert. That is the
// same hazard watchShutdown's own placement inside onReady exists to
// avoid, arriving here as a test constraint.
func TestWatchShutdown_IsInertUntilTheContextIsCancelled(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := &tray{cli: newTestClient(srv.URL)}
	quits := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		tr.watchShutdownWith(ctx, func() { quits <- struct{}{} })
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("watchShutdown returned without a cancellation")
	case <-quits:
		t.Fatal("watchShutdown quit the tray before the context was cancelled")
	case <-time.After(50 * time.Millisecond):
	}
	mu.Lock()
	early := len(calls)
	mu.Unlock()
	if early != 0 {
		t.Errorf("watchShutdown touched the daemon before the cancellation: %v", calls)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(ShutdownBudget):
		t.Fatalf("watchShutdown did not finish within %v of the cancellation", ShutdownBudget)
	}
	if got := len(quits); got != 1 {
		t.Errorf("quit called %d times, want exactly 1", got)
	}
}

// TestElevationCtx_SurvivesTheShutdown is the regression for the
// coupling watchShutdown makes live: the four *ViaElevation helpers run
// their privileged child through exec.CommandContext, so a logout part
// way through "Update Waired" would kill the elevated installer mid-swap
// if they rode the tray's own context.
func TestElevationCtx_SurvivesTheShutdown(t *testing.T) {
	type key struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "kept"))
	child := elevationCtx(ctx)
	cancel()

	select {
	case <-child.Done():
		t.Fatal("the elevated child's context was cancelled by the tray's shutdown")
	default:
	}
	if got := child.Value(key{}); got != "kept" {
		t.Errorf("elevationCtx dropped the parent's values: %v", got)
	}
}
