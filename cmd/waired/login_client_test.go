package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// A scripted daemon: /login/start returns logging_in with a URL, then
// /login/status walks logging_in -> activating -> active across polls. Once
// active, runInitViaDaemon foreground-waits the (already-ready) model and
// benchmarks it (waired#756), so the daemon also answers the reachability
// probe, /inference/status (ready), and /inference/benchmark.
func TestRunInitViaDaemonPollsToActive(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/waired/v1/login/start":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			_ = json.NewEncoder(w).Encode(management.LoginStatus{
				SessionID: "s1",
				Phase:     management.LoginPhaseLoggingIn,
				LoginURL:  "https://login.example/abc",
				UserCode:  "CODE-1",
			})
		case "/waired/v1/login/status":
			if got := r.URL.Query().Get("session"); got != "s1" {
				t.Errorf("status session = %q, want s1", got)
			}
			n := atomic.AddInt32(&polls, 1)
			st := management.LoginStatus{SessionID: "s1"}
			if n == 1 {
				st.Phase = management.LoginPhaseActivating
			} else {
				st.Phase = management.LoginPhaseActive
				st.AccountEmail = "user@example.com"
			}
			_ = json.NewEncoder(w).Encode(st)
		case "/waired/v1/status":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/waired/v1/inference/status":
			_ = json.NewEncoder(w).Encode(management.InferenceStatus{SubsystemState: "ready"})
		case "/waired/v1/inference/benchmark":
			_ = json.NewEncoder(w).Encode(management.BenchmarkRunResponse{Ran: true, MeasuredTokps: 40})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// noBrowser=true so the test never shells out to a browser;
	// nonInteractive=true so the post-login #133 prompt never reads stdin.
	if err := runInitViaDaemon(srv.URL, "https://cp.example", "dev-1", true, true,
		true /* skipIntegration: keep the test hermetic (no home-dir writes) */, "http://127.0.0.1:9473"); err != nil {
		t.Fatalf("runInitViaDaemon: %v", err)
	}
	if atomic.LoadInt32(&polls) < 2 {
		t.Errorf("expected at least 2 status polls, got %d", polls)
	}
}

func TestRunInitViaDaemonSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/waired/v1/login/start":
			_ = json.NewEncoder(w).Encode(management.LoginStatus{
				SessionID: "s1",
				Phase:     management.LoginPhaseLoggingIn,
				LoginURL:  "https://login.example/abc",
			})
		case "/waired/v1/login/status":
			_ = json.NewEncoder(w).Encode(management.LoginStatus{
				SessionID: "s1",
				Phase:     management.LoginPhaseError,
				Error:     "control plane denied",
			})
		}
	}))
	defer srv.Close()

	err := runInitViaDaemon(srv.URL, "https://cp.example", "dev-1", true, true,
		true /* skipIntegration: keep the test hermetic (no home-dir writes) */, "http://127.0.0.1:9473")
	if err == nil {
		t.Fatal("expected error from error phase")
	}
	if got := err.Error(); got != "login failed: control plane denied" {
		t.Errorf("unexpected error: %v", got)
	}
}

// daemonReachable selects between the daemon and standalone branches.
func TestDaemonReachableProbe(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/waired/v1/status" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()

	if !daemonReachable(up.URL) {
		t.Error("daemonReachable should be true for a live daemon")
	}
	// Closed server / nothing listening → not reachable.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := down.URL
	down.Close()
	if daemonReachable(addr) {
		t.Error("daemonReachable should be false when nothing is listening")
	}
}
