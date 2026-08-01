package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if err := runInitViaDaemon(daemonInitOpts{
		MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
		GatewayBaseURL: "http://127.0.0.1:9473",
		NoBrowser:      true, NonInteractive: true,
		SkipIntegration: true, // keep the test hermetic (no home-dir writes)
	}); err != nil {
		t.Fatalf("runInitViaDaemon: %v", err)
	}
	if atomic.LoadInt32(&polls) < 2 {
		t.Errorf("expected at least 2 status polls, got %d", polls)
	}
}

// PRODUCT CONTRACT (#175): the re-auth intent must reach the daemon, and
// only when this host is actually re-authenticating. A request that always
// carried it would turn every first-run login into a re-enrolment attempt;
// one that never carried it would leave `waired init` on an enrolled host
// printing a successful sign-in for a run that renewed nothing.
func TestRunInitViaDaemonSendsReauthOnlyWhenRenewing(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	for _, tc := range []struct{ reauth bool }{{false}, {true}} {
		name := "fresh"
		if tc.reauth {
			name = "renewing"
		}
		t.Run(name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/waired/v1/login/start":
					_ = json.NewDecoder(r.Body).Decode(&body)
					_ = json.NewEncoder(w).Encode(management.LoginStatus{
						SessionID: "s1", Phase: management.LoginPhaseActive,
						AccountEmail: "user@example.com",
					})
				case "/waired/v1/status":
					_, _ = w.Write([]byte(`{}`))
				case "/waired/v1/inference/status":
					_ = json.NewEncoder(w).Encode(management.InferenceStatus{SubsystemState: "disabled"})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			if err := runInitViaDaemon(daemonInitOpts{
				MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
				GatewayBaseURL: "http://127.0.0.1:9473",
				NoBrowser:      true, NonInteractive: true,
				SkipIntegration: true, Reauth: tc.reauth,
			}); err != nil {
				t.Fatalf("runInitViaDaemon: %v", err)
			}

			got, present := body["reauth"].(bool)
			if tc.reauth && (!present || !got) {
				t.Errorf("renewing run sent reauth=%v (present=%v), want true", got, present)
			}
			// Omitted rather than false: the body a first run sends is the
			// body it has always sent.
			if !tc.reauth && present {
				t.Errorf("fresh run sent a reauth field (%v); it must be omitted", got)
			}
		})
	}
}

// An agent predating `reauth` ignores it and answers with its idempotent
// no-op — active, no session id. Reading that as success is the silent
// "renewed nothing" this whole change exists to remove, so it must be a
// named failure instead.
func TestRunInitViaDaemonNamesAnAgentTooOldToReauth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/waired/v1/login/start" {
			// No SessionID: exactly what loginController.Start returns for
			// an already-active daemon that never saw the field.
			_ = json.NewEncoder(w).Encode(management.LoginStatus{Phase: management.LoginPhaseActive})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := runInitViaDaemon(daemonInitOpts{
		MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
		GatewayBaseURL: "http://127.0.0.1:9473",
		NoBrowser:      true, NonInteractive: true,
		SkipIntegration: true, Reauth: true,
	})
	if err == nil {
		t.Fatal("want an error when the agent cannot re-authenticate")
	}
	if !strings.Contains(err.Error(), "too old") {
		t.Errorf("error should say the service is too old, got: %v", err)
	}
	// The copy is for a person who has never heard of a daemon.
	if strings.Contains(strings.ToLower(err.Error()), "session id") {
		t.Errorf("error leaks the protocol-level symptom: %v", err)
	}
}

// TestRunInitViaDaemonResumesAnEnrolledDevice is the #313 bar. The same
// answer as the test above — active, no session id — but WITHOUT reauth
// asked for, which is what every `waired init` on an enrolled Windows
// device sent, because the CLI resolved a state dir the daemon does not
// use and so found no identity to renew. It used to fail with "daemon
// did not return a login session id"; NAVI hands operators this command
// to resume a stuck setup, so setup was unresumable there.
func TestRunInitViaDaemonResumesAnEnrolledDevice(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/waired/v1/login/start":
			_ = json.NewEncoder(w).Encode(management.LoginStatus{Phase: management.LoginPhaseActive})
		case "/waired/v1/status":
			_, _ = w.Write([]byte(`{}`))
		case "/waired/v1/inference/status":
			_ = json.NewEncoder(w).Encode(management.InferenceStatus{SubsystemState: "disabled"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		err := runInitViaDaemon(daemonInitOpts{
			MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
			GatewayBaseURL: "http://127.0.0.1:9473",
			NoBrowser:      true, NonInteractive: true,
			SkipIntegration: true, SkipClaudeRoute: true,
			AccountEmail: "you@example.com",
		})
		if err != nil {
			t.Errorf("re-init on an enrolled device failed: %v", err)
		}
	})
	if !strings.Contains(out, "Already signed in as you@example.com — resuming setup.") {
		t.Errorf("the run did not announce the resume\n---\n%s", out)
	}
	if strings.Contains(out, "Sign in\n") {
		t.Errorf("the run announced a sign-in it did not do\n---\n%s", out)
	}
}

// An auth key is not spent on a device that is already signed in (the
// `tailscale up` rule), and — unlike tailscale#7995 — it does not go
// unmentioned either.
func TestRunInitViaDaemonSaysTheAuthKeyWentUnused(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/waired/v1/login/start":
			_ = json.NewEncoder(w).Encode(management.LoginStatus{Phase: management.LoginPhaseActive})
		case "/waired/v1/status":
			_, _ = w.Write([]byte(`{}`))
		case "/waired/v1/inference/status":
			_ = json.NewEncoder(w).Encode(management.InferenceStatus{SubsystemState: "disabled"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runInitViaDaemon(daemonInitOpts{
			MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
			GatewayBaseURL: "http://127.0.0.1:9473",
			NoBrowser:      true, NonInteractive: true,
			SkipIntegration: true, SkipClaudeRoute: true,
			AuthKey: "waired_ak_test", AccountEmail: "you@example.com",
		}); err != nil {
			t.Errorf("re-init with an auth key failed: %v", err)
		}
	})
	if !strings.Contains(out, "The auth key was not used") {
		t.Errorf("the unused auth key was passed over in silence\n---\n%s", out)
	}
	if strings.Contains(out, "Signing in with an auth key") {
		t.Errorf("the run claimed to sign in with the key\n---\n%s", out)
	}
}

// A no-session answer that is NOT the idempotent no-op is still a
// failure — but one that names the phase instead of the protocol.
func TestRunInitViaDaemonNamesAPhaselessStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/waired/v1/login/start" {
			_ = json.NewEncoder(w).Encode(management.LoginStatus{Phase: management.LoginPhaseUnenrolled})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := runInitViaDaemon(daemonInitOpts{
		MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
		GatewayBaseURL: "http://127.0.0.1:9473",
		NoBrowser:      true, NonInteractive: true, SkipIntegration: true,
	})
	if err == nil {
		t.Fatal("want an error when the daemon starts no sign-in")
	}
	if !strings.Contains(err.Error(), "did not start a sign-in") {
		t.Errorf("error should say what did not happen, got: %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "session id") {
		t.Errorf("error leaks the protocol-level symptom: %v", err)
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

	err := runInitViaDaemon(daemonInitOpts{
		MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
		GatewayBaseURL: "http://127.0.0.1:9473",
		NoBrowser:      true, NonInteractive: true,
		SkipIntegration: true, // keep the test hermetic (no home-dir writes)
	})
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
