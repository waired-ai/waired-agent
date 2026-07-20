package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// fakeInferenceDaemon serves the three management routes this path
// re-applies, plus the inference status the engine decision reads.
type fakeInferenceDaemon struct {
	mu       sync.Mutex
	hits     []string
	models   []string
	subState string
}

func (f *fakeInferenceDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	record := func(p string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			f.mu.Lock()
			f.hits = append(f.hits, p)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "ok"})
		}
	}
	for _, p := range []string{
		"/waired/v1/inference/enable", "/waired/v1/inference/disable",
		"/waired/v1/inference/share/enable", "/waired/v1/inference/share/disable",
	} {
		mux.HandleFunc(p, record(p))
	}
	mux.HandleFunc("/waired/v1/inference/preferred-model", func(w http.ResponseWriter, r *http.Request) {
		var req management.PreferredModelRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.hits = append(f.hits, "/waired/v1/inference/preferred-model")
		f.models = append(f.models, req.ModelID)
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(management.PreferredModelResponse{ModelID: req.ModelID})
	})
	mux.HandleFunc("/waired/v1/inference/status", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		s := f.subState
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(management.InferenceStatus{SubsystemState: s})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeInferenceDaemon) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

// TestApplyDaemonInitInference is the waired#835 §11.2 fix: the
// installer's --inference-enabled and friends used to be accepted and
// silently dropped on this path.
func TestApplyDaemonInitInference(t *testing.T) {
	tests := []struct {
		name string
		inf  daemonInitInference
		want []string
	}{
		{"nothing passed touches nothing", daemonInitInference{}, nil},
		{
			"enabled on", daemonInitInference{Enabled: boolPtr(true)},
			[]string{"/waired/v1/inference/enable"},
		},
		{
			"enabled off", daemonInitInference{Enabled: boolPtr(false)},
			[]string{"/waired/v1/inference/disable"},
		},
		{
			"share only", daemonInitInference{Share: boolPtr(true)},
			[]string{"/waired/v1/inference/share/enable"},
		},
		{
			"all three", daemonInitInference{Enabled: boolPtr(true), Share: boolPtr(false), ModelID: "m-1"},
			[]string{
				"/waired/v1/inference/enable",
				"/waired/v1/inference/share/disable",
				"/waired/v1/inference/preferred-model",
			},
		},
		{
			// Downloading weights onto a host that just turned inference
			// off is a multi-GB no-op.
			"model skipped when inference turned off",
			daemonInitInference{Enabled: boolPtr(false), ModelID: "m-1"},
			[]string{"/waired/v1/inference/disable"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeInferenceDaemon{}
			srv := d.server(t)
			applyDaemonInitInference(srv.URL, tc.inf, io.Discard)

			got := d.got()
			if len(got) != len(tc.want) {
				t.Fatalf("routes hit = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("routes hit = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestApplyDaemonInitInferenceSurvivesFailure: login already succeeded,
// so a knob that would not apply is a warning, never a failed install.
func TestApplyDaemonInitInferenceSurvivesFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	// Must not panic and must not block; the warning goes to the writer.
	applyDaemonInitInference(srv.URL, daemonInitInference{
		Enabled: boolPtr(true), Share: boolPtr(true), ModelID: "m-1",
	}, io.Discard)
}

// TestDaemonWantsEngine covers the one signal that decides whether a
// terminal-only install downloads an engine.
func TestDaemonWantsEngine(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"no_engine", true},
		{"disabled", false}, // deliberate operator state
		{"stopped", false},  // deliberate operator state
		{"ready", false},    // an engine already exists
		{"downloading", false},
	}
	for _, tc := range tests {
		t.Run(tc.state, func(t *testing.T) {
			d := &fakeInferenceDaemon{subState: tc.state}
			srv := d.server(t)
			if got := daemonWantsEngine(srv.URL); got != tc.want {
				t.Fatalf("daemonWantsEngine(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// TestDaemonWantsEngineGivesUp: an unreachable or silent daemon must not
// hang init forever, and must not install on a guess.
func TestDaemonWantsEngineGivesUp(t *testing.T) {
	prev := engineWaitForStatus
	engineWaitForStatus = 20 * time.Millisecond
	t.Cleanup(func() { engineWaitForStatus = prev })
	shrinkSetupTimers(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	if daemonWantsEngine(srv.URL) {
		t.Fatal("wanted an engine from an unreachable daemon")
	}
}

// TestDaemonPathEngineInstallNoWizard is the core of this change: a
// terminal-only daemon-path install still gets an engine. This is the
// macOS default journey today.
func TestDaemonPathEngineInstallNoWizard(t *testing.T) {
	shrinkSetupTimers(t)
	f := (&fakeEngineInstaller{}).install(t)
	inf := &fakeInferenceDaemon{subState: "no_engine"}
	infSrv := inf.server(t)

	d := &fakeSetupDaemon{}
	// Active=false and no desired engine — exactly what a host with no
	// browser wizard reports. state_dir is served regardless.
	d.setState(management.SetupStateResponse{StateDir: "/var/lib/waired"})
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	daemonPathEngineInstall(context.Background(), s, infSrv.URL, io.Discard, "linux", true)

	if got := f.installed(); len(got) != 1 || got[0] != "/var/lib/waired" {
		t.Fatalf("installer calls = %v, want one call with the daemon's state dir", got)
	}
	if got := f.handedOff(); len(got) != 1 {
		t.Fatalf("ownership handoff = %v, want one call", got)
	}
}

func TestDaemonPathEngineInstallSkips(t *testing.T) {
	tests := []struct {
		name     string
		subState string
		setup    management.SetupStateResponse
		older    bool
	}{
		{"inference disabled", "disabled", management.SetupStateResponse{StateDir: "/s"}, false},
		{"inference stopped", "stopped", management.SetupStateResponse{StateDir: "/s"}, false},
		{"engine already up", "ready", management.SetupStateResponse{StateDir: "/s"}, false},
		{
			"no state dir from the daemon", "no_engine",
			management.SetupStateResponse{}, false,
		},
		{
			"a wizard already claimed the install", "no_engine",
			management.SetupStateResponse{StateDir: "/s", InstallClaimed: "ollama"}, false,
		},
		{"older daemon without executor routes", "no_engine", management.SetupStateResponse{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shrinkSetupTimers(t)
			prev := engineWaitForStatus
			engineWaitForStatus = 20 * time.Millisecond
			t.Cleanup(func() { engineWaitForStatus = prev })

			f := (&fakeEngineInstaller{}).install(t)
			inf := &fakeInferenceDaemon{subState: tc.subState}
			infSrv := inf.server(t)

			d := &fakeSetupDaemon{notFound: tc.older}
			d.setState(tc.setup)
			srv := d.server(t)

			s := attachSetupExecutor(srv.URL, true)
			defer s.Release()
			daemonPathEngineInstall(context.Background(), s, infSrv.URL, io.Discard, "linux", true)

			if got := f.installed(); len(got) != 0 {
				t.Fatalf("installed %v, want no install", got)
			}
		})
	}
}
