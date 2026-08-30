package tray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
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
		// The only route a Windows desktop has: os/signal delivers
		// nothing to a -H windowsgui process, so a logout arrives as
		// WM_ENDSESSION and systray turns it into onExit
		// (waired-agent#1059).
		{causeWindowClose, "window-close", true},
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
		"/waired/v1/sharing/suspend",
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

// TestWindDown_RunsAtMostOnce is the guard for the path that now exists
// twice over. Leaving through the menu or a signal winds down and THEN
// quits the GUI loop -- and quitting the loop is itself what calls
// onSystrayExit. systray's own systrayExitCalled guard stops SYSTRAY
// calling the callback twice; it says nothing about this side reaching
// the daemon twice.
func TestWindDown_RunsAtMostOnce(t *testing.T) {
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
	// The real sequence: shutdown winds down, quits, and systray then
	// runs onExit on its way out of the loop.
	tr.shutdown(planShutdown(causeQuitMenu), tr.onSystrayExit)
	tr.onSystrayExit()

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"/waired/v1/sharing/suspend",
		"/waired/v1/inference/engine/stop",
	}
	if len(calls) != len(want) {
		t.Fatalf("the daemon saw %v, want exactly one wind-down %v", calls, want)
	}
}

// TestOnSystrayExit_WindsDownOnItsOwn: the Windows case with no earlier
// shutdown() at all -- the event loop simply ended because the desktop
// said so. This is the assert that would have failed before
// waired-agent#1059, when tray.Run passed systray an empty onExit.
func TestOnSystrayExit_WindsDownOnItsOwn(t *testing.T) {
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
	tr.onSystrayExit()

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"/waired/v1/sharing/suspend",
		"/waired/v1/inference/engine/stop",
	}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("a window-close exit did %v, want %v", calls, want)
	}
}

// TestPlanFirstLaunchAutostart pins the decision that lets an installer
// put the tray back without manufacturing consent (waired-agent#1046).
//
// The row that matters is skip:user-decided. Before the marker, "no
// login item" meant "never registered", so every tray start re-created
// one -- and switching "Start Waired on login" off did not survive a
// restart of the app, let alone an update that restarts it.
// install.sh's darwin_tray_autostart_notice names that ambiguity as the
// reason a macOS update reports the login item instead of registering it.
func TestPlanFirstLaunchAutostart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts autostartFirstLaunchFacts
		want  string
	}{
		{"a first run on an OS that registers", autostartFirstLaunchFacts{Applies: true}, "register"},
		{"linux registers nothing here", autostartFirstLaunchFacts{}, "skip:not-applicable"},
		{"already registered is left alone", autostartFirstLaunchFacts{Applies: true, Enabled: true}, "skip:already-enabled"},
		{"the user switched it off", autostartFirstLaunchFacts{Applies: true, HasRun: true}, "skip:user-decided"},
		{"they switched it on again themselves", autostartFirstLaunchFacts{Applies: true, Enabled: true, HasRun: true}, "skip:already-enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planFirstLaunchAutostart(tc.facts); got != tc.want {
				t.Errorf("planFirstLaunchAutostart(%+v) = %q, want %q", tc.facts, got, tc.want)
			}
		})
	}
}

// TestEnsureAutostart_DoesNotReRegisterAfterTheUserTurnedItOff is the
// end-to-end of the row above, through the real method: start once
// (registers), have the user turn it off, start again (must not).
func TestEnsureAutostart_DoesNotReRegisterAfterTheUserTurnedItOff(t *testing.T) {
	t.Setenv(paths.EnvOverride, t.TempDir())

	f := &fakeAutostart{}
	tr := &tray{autostartMgr: f}
	tr.ensureAutostartOnFirstLaunchFor("darwin")
	if f.enableCalls != 1 {
		t.Fatalf("first launch: Enable called %d times, want 1", f.enableCalls)
	}

	// The user opens the menu and unticks "Start Waired on login".
	f.enabled = false
	tr2 := &tray{autostartMgr: f}
	tr2.ensureAutostartOnFirstLaunchFor("darwin")
	if f.enableCalls != 1 {
		t.Errorf("a later launch re-registered the login item the user turned off (Enable calls = %d)", f.enableCalls)
	}
}
