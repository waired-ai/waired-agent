package tray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// --- #316: Quit semantics and the engine write budget ---

// TestOnQuit_SuspendsSharingThenStopsEngine pins a PRODUCT CONTRACT:
// quitting the tray withdraws this machine from the mesh AND frees the
// engine's memory, in that order. Suspending first means peers stop being
// routed here before the engine they would have been routed to goes away;
// the reverse order strands in-flight peer requests against a dying
// engine.
//
// It also pins WHICH endpoint does the withdrawing: /share/suspend, not
// /share/disable. The latter persists "not_shared", so closing the tray
// would silently revoke the operator's sharing preference for good.
func TestOnQuit_SuspendsSharingThenStopsEngine(t *testing.T) {
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
	tr.onQuit()

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()

	want := []string{
		"/waired/v1/inference/share/suspend",
		"/waired/v1/inference/engine/stop",
	}
	if len(got) != len(want) {
		t.Fatalf("Quit made %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A daemon that predates the share suspension answers 404. That must not
// cost the machine its engine stop — the two calls are independent
// best-effort steps on the way out.
func TestOnQuit_StopsEngineEvenWhenSuspendUnsupported(t *testing.T) {
	stopped := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/waired/v1/inference/share/suspend" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		stopped <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := &tray{cli: newTestClient(srv.URL)}
	tr.onQuit()

	select {
	case <-stopped:
	default:
		t.Fatal("engine stop skipped after the share suspension 404'd")
	}
}

// TestResumeSharingOnStart pins the other half of the suspension's
// lifetime: sharing is withheld for exactly as long as the tray is
// closed. Without this the operator would have to re-enable sharing by
// hand after every Quit.
func TestResumeSharingOnStart(t *testing.T) {
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
	tr.resumeSharingOnStart(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "/waired/v1/inference/share/unsuspend" {
		t.Fatalf("startup calls = %v, want a single /share/unsuspend", calls)
	}
}

// TestClient_EngineWritesUseTheLongBudget is the regression for the
// deterministic half of #316. http.Client.Timeout is a hard wall-clock
// cap applied on TOP of the request context, so a 3s write client could
// never wait out a stop the daemon bounds at 15s — every tray Stop failed
// with "context deadline exceeded" while the engine kept its VRAM. The
// engine verbs must therefore ride a client with its own, longer budget.
//
// The cheap client's budget is shrunk here so the test costs
// milliseconds; what is being pinned is which client each endpoint uses,
// not the specific durations.
func TestClient_EngineWritesUseTheLongBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	c.wc = &http.Client{Timeout: 20 * time.Millisecond} // stands in for the cheap budget

	// Control: an ordinary write does time out, so the handler really is
	// slower than the cheap client's ceiling.
	if err := c.DisableInference(context.Background()); err == nil {
		t.Fatal("control failed: the cheap write client did not time out")
	}
	if err := c.StopEngine(context.Background()); err != nil {
		t.Errorf("StopEngine rode the cheap budget: %v", err)
	}
	if err := c.StartEngine(context.Background()); err != nil {
		t.Errorf("StartEngine rode the cheap budget: %v", err)
	}
}

// The engine budget must outlast the daemon's own engine-stop budget
// (cmd/waired-agent: engineStopBudget, 15s) or the tray reports a timeout
// it caused itself while the daemon completes the stop.
func TestEngineWriteTimeoutOutlastsDaemonStopBudget(t *testing.T) {
	const daemonEngineStopBudget = 15 * time.Second // cmd/waired-agent/engine_control.go
	if engineWriteTimeout <= daemonEngineStopBudget {
		t.Errorf("engineWriteTimeout=%s must exceed the daemon's %s stop budget", engineWriteTimeout, daemonEngineStopBudget)
	}
	if engineWriteTimeout <= writeTimeout {
		t.Errorf("engineWriteTimeout=%s must exceed the ordinary write budget %s", engineWriteTimeout, writeTimeout)
	}
}
