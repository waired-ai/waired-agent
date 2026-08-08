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

// This test reuses captureStdout (infer_test.go): runInitViaDaemon writes the
// progress bar, guidance, and success box to os.Stdout directly.

// inferenceDaemon is a scripted daemon covering the endpoints the
// daemon-mediated init touches: login start/status, the reachability probe,
// the inference status poll, and the benchmark. statusSeq drives the
// /inference/status responses (the last entry repeats); benchOK controls
// whether /benchmark answers 200 (ran) or 425 (never ready).
type inferenceDaemon struct {
	statusSeq   []management.InferenceStatus
	statusCalls int32
	benchOK     bool
	benchTokps  float64
	loginPolls  int32
	// setupState, when set, adds the executor routes so the run reaches
	// init the way a browser-driven install does. Without it the routes
	// 404, the session is unsupported, and every watch built from it —
	// including the model target — is inert.
	setupState *management.SetupStateResponse
}

func (d *inferenceDaemon) server() *httptest.Server {
	mux := http.NewServeMux()
	if d.setupState != nil {
		mux.HandleFunc("/waired/v1/setup/state", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(*d.setupState)
		})
		mux.HandleFunc("/waired/v1/setup/executor", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(*d.setupState)
		})
	}
	mux.HandleFunc("/waired/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/waired/v1/login/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(management.LoginStatus{
			SessionID: "s1", Phase: management.LoginPhaseLoggingIn, LoginURL: "https://login.example/abc",
		})
	})
	mux.HandleFunc("/waired/v1/login/status", func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&d.loginPolls, 1)
		st := management.LoginStatus{SessionID: "s1"}
		if n == 1 {
			st.Phase = management.LoginPhaseActivating
		} else {
			st.Phase = management.LoginPhaseActive
			st.AccountEmail = "user@example.com"
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("/waired/v1/inference/status", func(w http.ResponseWriter, _ *http.Request) {
		i := int(atomic.AddInt32(&d.statusCalls, 1)) - 1
		if i >= len(d.statusSeq) {
			i = len(d.statusSeq) - 1
		}
		_ = json.NewEncoder(w).Encode(d.statusSeq[i])
	})
	mux.HandleFunc("/waired/v1/inference/benchmark", func(w http.ResponseWriter, _ *http.Request) {
		if !d.benchOK {
			w.WriteHeader(http.StatusTooEarly)
			return
		}
		_ = json.NewEncoder(w).Encode(management.BenchmarkRunResponse{Ran: true, MeasuredTokps: d.benchTokps})
	})
	return httptest.NewServer(mux)
}

// runViaDaemonGuarded runs runInitViaDaemon under a hard timeout so a
// regression that blocks (e.g. waitForBundledModel never returning on a
// disabled host) fails the test instead of hanging it. captureStdout swaps the
// global os.Stdout, so it must run on the test goroutine (not a child) to avoid
// racing the test framework's own output; runInitViaDaemon runs in the child.
func runViaDaemonGuarded(t *testing.T, url string) string {
	t.Helper()
	var runErr error
	timedOut := false
	out := captureStdout(t, func() {
		done := make(chan error, 1)
		go func() {
			done <- runInitViaDaemon(daemonInitOpts{
				MgmtURL: url, Control: "https://cp.example", DeviceName: "dev-1",
				GatewayBaseURL: "http://127.0.0.1:9473",
				NoBrowser:      true, NonInteractive: true,
				SkipIntegration: true,
			})
		}()
		select {
		case runErr = <-done:
		case <-time.After(30 * time.Second):
			timedOut = true
		}
	})
	if timedOut {
		t.Fatal("runInitViaDaemon hung")
	}
	if runErr != nil {
		t.Fatalf("runInitViaDaemon: %v", runErr)
	}
	return out
}

const bundledModel = "qwen3.5-9b"

func downloadingStatus(completed, total int64) management.InferenceStatus {
	return management.InferenceStatus{
		SubsystemState: "downloading",
		// A within-budget measurement: install-flow step 6 reads the
		// status once, finds it, and stays out of these choreographies
		// (its own scenarios build over-budget fixtures explicitly).
		HostSpeed: &management.HostSpeedStatus{TurnSeconds: 30, BudgetSeconds: 45},
		Models: management.ModelsSnapshot{
			Downloading: []string{bundledModel},
			Downloads:   []management.ModelDownload{{Model: bundledModel, CompletedBytes: completed, TotalBytes: total}},
		},
		Active: &management.ActiveSelection{ModelID: bundledModel},
	}
}

// TestRunInitViaDaemon_ShowsDownloadProgressAndGuidance is the waired#756
// happy path: the daemon-mediated init now foreground-waits the model download
// (with the progress bar), benchmarks the ready model, and prints inference-role
// guidance before the success box.
func TestRunInitViaDaemon_ShowsDownloadProgressAndGuidance(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	d := &inferenceDaemon{
		benchOK:    true,
		benchTokps: 42,
		statusSeq: []management.InferenceStatus{
			// One extra leading entry: step 6's single status read
			// consumes it before the model wait begins.
			downloadingStatus(1<<30, 4<<30),
			downloadingStatus(1<<30, 4<<30),
			downloadingStatus(3<<30, 4<<30),
			{SubsystemState: "ready", Models: management.ModelsSnapshot{Ready: []string{bundledModel}}, Active: &management.ActiveSelection{ModelID: bundledModel}},
		},
	}
	srv := d.server()
	defer srv.Close()

	out := runViaDaemonGuarded(t, srv.URL)

	for _, want := range []string{
		"Downloading",               // the model-download progress bar rendered
		bundledModel + " ready",     // waitForBundledModel saw the model become ready
		"Local inference works",     // the benchmark ran against the ready model
		"waired runtimes benchmark", // the #756 inference-role guidance printed
		"Waired is ready",           // the success box rendered
	} {
		if !strings.Contains(out, want) {
			t.Errorf("daemon-init output missing %q\n---\n%s", want, out)
		}
	}
}

// TestRunInitViaDaemon_DisabledInferenceDoesNotBlock: on a host where the
// daemon reports inference disabled (e.g. under-spec, gateway-only), the
// foreground wait must return fast (no progress bar) and init must still
// complete (waired#756).
func TestRunInitViaDaemon_DisabledInferenceDoesNotBlock(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 20*time.Millisecond, 100*time.Millisecond)
	d := &inferenceDaemon{
		benchOK: false, // benchmark never becomes ready; bounded by the shrunk deadline
		statusSeq: []management.InferenceStatus{
			{SubsystemState: "disabled"},
		},
	}
	srv := d.server()
	defer srv.Close()

	out := runViaDaemonGuarded(t, srv.URL)

	if strings.Contains(out, "Downloading") {
		t.Errorf("disabled inference must not render a download bar\n---\n%s", out)
	}
	if !strings.Contains(out, "Waired is ready") {
		t.Errorf("init must still complete on a disabled host\n---\n%s", out)
	}
}

// TestRunInitViaDaemon_ReportsTheWizardsModel carries #306 end to end, over
// the real wire and through the real call site: a browser setup is driving,
// the control plane has named a model, and the agent is meanwhile serving
// the one it picked for itself. The terminal must report the operator's
// choice.
//
// It is the only test that pins the production wiring — that
// runInitViaDaemon builds a target from its session and hands it to the
// wait at all. The unit tests below it would all still pass if that
// argument were nil.
func TestRunInitViaDaemon_ReportsTheWizardsModel(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	const chosen = "qwen3.6-27b" // what the wizard asked for

	// A live wizard whose engine is already in place, so nothing here tries
	// to install one.
	setupState := activeState()
	setupState.DesiredModelID = chosen

	// The agent is serving its own pick and reports "ready" for it the
	// whole time — the state that made the terminal announce the wrong
	// model and return.
	racing := management.InferenceStatus{
		SubsystemState: "ready",
		Active:         &management.ActiveSelection{ModelID: bundledModel},
		Models: management.ModelsSnapshot{
			Ready:       []string{bundledModel},
			Downloading: []string{chosen},
			Downloads:   []management.ModelDownload{{Model: chosen, CompletedBytes: 1 << 30, TotalBytes: 4 << 30}},
		},
	}
	landed := racing
	landed.Models.Ready = []string{bundledModel, chosen}
	landed.Models.Downloading = nil
	landed.Models.Downloads = nil

	d := &inferenceDaemon{
		setupState: &setupState,
		benchOK:    true, // let the run finish instead of polling out its deadline
		benchTokps: 42,
		statusSeq:  []management.InferenceStatus{racing, racing, landed},
	}
	srv := d.server()
	defer srv.Close()

	out := runViaDaemonGuarded(t, srv.URL)

	if !strings.Contains(out, chosen+" ready") {
		t.Errorf("the terminal did not report the model the wizard chose\n---\n%s", out)
	}
	if strings.Contains(out, bundledModel+" ready") {
		t.Errorf("the terminal reported the model the AGENT picked, which is #306\n---\n%s", out)
	}
}
