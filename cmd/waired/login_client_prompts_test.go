package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// These are the regression tests for the four "who owns stdin" issues
// of the daemon-driven `waired init`: #184 (the sign-in Enter silently
// became a takeover), #185 (the takeover watch ate the coding-agent
// answer), #186 (that question was asked mid-download, and never at all
// after a late takeover) and #188 (a failed engine install parked the
// terminal on a wait for an engine that was never coming). #132 — the
// spurious "Press Enter to continue…" on the browser-driven path — falls
// out of the same change and is asserted here too.

// promptsDaemon is a scripted daemon covering everything the
// daemon-mediated init touches: login, the reachability probe, the
// inference status/benchmark pair, and the executor lease.
type promptsDaemon struct {
	mu         sync.Mutex
	setupState management.SetupStateResponse

	statusSeq   []management.InferenceStatus
	statusCalls int32
	loginPolls  int32
	setupPolls  int32

	// onStatus fires on each /inference/status poll, so a test can time
	// a keystroke to a real point in the flow instead of a wall clock.
	onStatus func(poll int32)
	// onSetupState is the same seam one stage earlier, for the window in
	// which the takeover offer is still open — i.e. before the browser
	// has written any desired state (waired-agent#198).
	onSetupState func(poll int32)
}

func (d *promptsDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/waired/v1/login/start", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.LoginStatus{
			SessionID: "s1", Phase: management.LoginPhaseLoggingIn,
			LoginURL: "https://login.example/abc", UserCode: "CODE-1",
		})
	})
	mux.HandleFunc("/waired/v1/login/status", func(w http.ResponseWriter, _ *http.Request) {
		st := management.LoginStatus{SessionID: "s1", Phase: management.LoginPhaseActivating}
		// Two polls before active, so the loop's per-tick work (the #184
		// stray-Enter acknowledgement) actually runs.
		if atomic.AddInt32(&d.loginPolls, 1) >= 2 {
			st.Phase = management.LoginPhaseActive
			st.AccountEmail = "user@example.com"
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("/waired/v1/inference/status", func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&d.statusCalls, 1)
		if d.onStatus != nil {
			d.onStatus(n)
		}
		i := int(n) - 1
		if i >= len(d.statusSeq) {
			i = len(d.statusSeq) - 1
		}
		_ = json.NewEncoder(w).Encode(d.statusSeq[i])
	})
	mux.HandleFunc("/waired/v1/inference/benchmark", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.BenchmarkRunResponse{Ran: true, MeasuredTokps: 42})
	})
	mux.HandleFunc("/waired/v1/setup/state", func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&d.setupPolls, 1)
		if d.onSetupState != nil {
			d.onSetupState(n)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(d.setupState)
	})
	mux.HandleFunc("/waired/v1/setup/executor", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		var req management.SetupExecutorRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.setupState.ExecutorAttached = req.Attached
		switch {
		case !req.Attached:
			d.setupState.InstallClaimed = ""
		case req.Phase == management.SetupExecutorPhaseInstalling && req.Engine != "":
			d.setupState.InstallClaimed = req.Engine
		case req.Phase == management.SetupExecutorPhaseDone ||
			req.Phase == management.SetupExecutorPhaseFailed:
			d.setupState.InstallClaimed = ""
		}
		_ = json.NewEncoder(w).Encode(d.setupState)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// downloadingRun is n polls' worth of in-flight download followed by a
// ready model, so a wait has room to observe a keystroke before it ends.
func downloadingRun(n int) []management.InferenceStatus {
	seq := make([]management.InferenceStatus, 0, n+1)
	for i := range n {
		seq = append(seq, downloadingStatus(int64(i+1)<<28, 8<<30))
	}
	return append(seq, readyStatus())
}

func readyStatus() management.InferenceStatus {
	return management.InferenceStatus{
		SubsystemState: "ready",
		Models:         management.ModelsSnapshot{Ready: []string{bundledModel}},
		Active:         &management.ActiveSelection{ModelID: bundledModel},
	}
}

// scriptStdin builds a scripted terminal: the same single-owner reader
// production uses, over a canned keystroke sequence. runInit owns the
// real one and hands it down, so the tests hand one down too.
func scriptStdin(keys string) *stdinReader {
	return newStdinReader(strings.NewReader(keys))
}

// scriptStdinPipe is scriptStdin for a scenario that must type DURING
// the run: awaitBrowserSetup drops whatever was typed before the
// takeover offer existed (#184), which in a canned string is the whole
// script. The returned writer feeds keystrokes in as the flow reaches
// the point they would really be pressed.
func scriptStdinPipe(t *testing.T) (*stdinReader, *io.PipeWriter) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	return newStdinReader(pr), pw
}

// daemonInitScenario is what the four scenarios below vary.
type daemonInitScenario struct {
	noBrowser       bool
	nonInteractive  bool
	skipIntegration bool
}

// runDaemonInit runs the flow under a hard timeout — a regression that
// blocks (which is exactly what #188 was) must fail, not hang.
func runDaemonInit(t *testing.T, url string, owner *stdinReader, o daemonInitScenario) string {
	t.Helper()
	stubOpener(t, nil) // no browser may be launched from a test
	// Pin the browser gate: without a display, Linux resolves to the
	// print-only gate and the scenarios that are not about that gate
	// would drift by OS (macOS and Windows always report a display).
	// --no-browser still forces print-only, which is what that test wants.
	t.Setenv("DISPLAY", ":0")
	var runErr error
	timedOut := false
	out := captureStdout(t, func() {
		done := make(chan error, 1)
		go func() {
			done <- runInitViaDaemon(daemonInitOpts{
				MgmtURL: url, Control: "https://cp.example", DeviceName: "dev-1",
				GatewayBaseURL:  "http://127.0.0.1:9473",
				NoBrowser:       o.noBrowser,
				NonInteractive:  o.nonInteractive,
				SkipIntegration: o.skipIntegration,
				// The routing flip writes a machine-wide file; these
				// scenarios are about the prompts, so opt out of it.
				SkipClaudeRoute: true,
				Owner:           owner,
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
	// Sign-in succeeded in every scenario here, so init must succeed —
	// including the failed-engine one (#188).
	if runErr != nil {
		t.Fatalf("runInitViaDaemon: %v\n---\n%s", runErr, out)
	}
	return out
}

// #184: on the print-only gate nothing reads stdin at the sign-in step,
// so an Enter pressed there used to fall through to the next reader. It
// must be answered where it was pressed instead.
func TestRunInitViaDaemon_PrintOnlyGateAcksStrayEnter(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner := scriptStdin("\n")
	d := &promptsDaemon{statusSeq: []management.InferenceStatus{readyStatus()}}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{noBrowser: true, skipIntegration: true})

	for _, want := range []string{
		"Nothing to press here — sign-in continues on its own once you open the link.",
		"Nothing to press here — waiting for you to sign in with the link above.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("print-only gate missing %q\n---\n%s", want, out)
		}
	}
}

// #184 + #185 + #186 in one run: a bare Enter opens the takeover
// question instead of switching modes, `y` confirms it, and the NEXT
// line answers the coding-agent question — which is asked at all only
// because #186 moved it after the wait, and reaches the prompt only
// because #185's second reader is gone.
func TestRunInitViaDaemon_TakeoverThenIntegrationGetsItsOwnAnswer(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner, keys := scriptStdinPipe(t)
	d := &promptsDaemon{
		// A download in flight, so the wait actually runs and the watch
		// gets the polls a real terminal would have.
		statusSeq: downloadingRun(200),
		// Deliberately NOT Active: the takeover offer is only open while
		// the browser has written nothing (waired-agent#198), so the
		// keystrokes are timed to the grace loop's own polls rather than
		// to the model wait that follows it. The inverted case — Enter
		// AFTER the browser committed — is
		// TestRunInitViaDaemon_TakeoverRefusedAfterTheBrowserCommits.
		setupState: management.SetupStateResponse{EngineInstalled: true, DesiredEngine: "ollama"},
	}
	d.onSetupState = func(poll int32) {
		switch poll {
		case 2:
			_, _ = keys.Write([]byte("\n")) // the muscle-memory Enter
		case 4:
			// The confirmation, and then the coding-agent answer that the
			// old code's still-parked listener would have eaten (#185).
			_, _ = keys.Write([]byte("y\nn\n"))
		}
	}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{})

	for _, want := range []string{
		"Take over setup in this terminal?",                // Enter asked, it did not switch
		"Taking over — setup continues",                    // `y` confirmed it
		"Coding-agent integration",                         // the terminal now owns the run, so it asks
		"Skipped. Set up the per-user integration anytime", // and `n` was ITS answer
	} {
		if !strings.Contains(out, want) {
			t.Errorf("takeover run missing %q\n---\n%s", want, out)
		}
	}
}

// TestRunInitViaDaemon_TakeoverRefusedAfterTheBrowserCommits is the
// inverted half of the test above, and a deliberate inversion of what
// this flow used to do (waired-agent#198): with desired state already
// written, Enter no longer moves setup to the terminal.
//
// The offer was open until the process exited, so a keystroke minutes
// after the browser had committed still switched modes — leaving the
// wizard driving a setup the terminal had taken over, with the control
// plane's desired state pointing at neither. It is degraded rather than
// disabled: a browser that crashes must still leave a way out, and an
// operator pressing the key the docs taught them deserves an answer.
func TestRunInitViaDaemon_TakeoverRefusedAfterTheBrowserCommits(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner, keys := scriptStdinPipe(t)
	d := &promptsDaemon{
		statusSeq:  downloadingRun(200),
		setupState: management.SetupStateResponse{Active: true, EngineInstalled: true, DesiredEngine: "ollama"},
	}
	d.onStatus = func(poll int32) {
		if poll == 3 {
			_, _ = keys.Write([]byte("\n"))
		}
	}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{})

	// The offer is withdrawn where it stops being true...
	if !strings.Contains(out, takeoverClosedLine) {
		t.Errorf("the closed-offer line was never printed\n---\n%s", out)
	}
	// ...the keystroke is answered rather than ignored...
	if !strings.Contains(out, "press Ctrl-C and run the setup command again") {
		t.Errorf("Enter after the commit said nothing\n---\n%s", out)
	}
	// ...and setup did NOT move to the terminal.
	for _, unwanted := range []string{
		"Take over setup in this terminal?",
		"Taking over — setup continues",
		"Coding-agent integration",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("takeover happened after the browser committed: found %q\n---\n%s", unwanted, out)
		}
	}
}

// #186: in terminal mode the coding-agent question must come after the
// model wait, not interrupt it. #132: the browser-driven path must not
// print the old reconciling "Press Enter to continue…" on its way out.
func TestRunInitViaDaemon_BrowserDrivenAsksNothingAndNeverPromptsToContinue(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner := scriptStdin("") // the operator never touches the terminal
	d := &promptsDaemon{
		statusSeq:  []management.InferenceStatus{readyStatus()},
		setupState: management.SetupStateResponse{Active: true, EngineInstalled: true, DesiredEngine: "ollama"},
	}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{})

	if strings.Contains(out, "Press Enter to continue") {
		t.Errorf("browser-driven path still prompts to continue (#132)\n---\n%s", out)
	}
	if strings.Contains(out, "Coding-agent integration") {
		t.Errorf("the terminal asked its own question while the browser was driving (§4.2)\n---\n%s", out)
	}
	for _, want := range []string{
		"You can set up your coding tools later",
		setupTerminalDoneLine,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("browser-driven run missing %q\n---\n%s", want, out)
		}
	}
}

// waired#939: the terminal must ask to be left open, and must ask BEFORE
// it offers to take setup over — the offer used to be the only thing said
// at the handoff, which reads as permission to walk away from the one
// process that can install anything. It must also stop saying it once it
// has nothing left to do, or the two surfaces contradict each other.
func TestRunInitViaDaemon_BrowserDrivenSaysKeepThisTerminalOpen(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner := scriptStdin("") // the operator never touches the terminal
	d := &promptsDaemon{
		statusSeq:  []management.InferenceStatus{readyStatus()},
		setupState: management.SetupStateResponse{Active: true, EngineInstalled: true, DesiredEngine: "ollama"},
	}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{})

	keep := strings.Index(out, setupKeepTerminalOpenLine)
	offer := strings.Index(out, "press Enter to continue in the terminal instead")
	if keep < 0 {
		t.Fatalf("browser-driven run never said to keep the terminal open\n---\n%s", out)
	}
	if offer < 0 {
		t.Fatalf("browser-driven run stopped offering the takeover\n---\n%s", out)
	}
	if keep > offer {
		t.Errorf("the switch offer comes before the persistence line\n---\n%s", out)
	}
	// Said again before the model wait, which is the longest stretch of the
	// flow and the one where the first warning has scrolled away.
	if n := strings.Count(out, setupKeepTerminalOpenLine); n < 2 {
		t.Errorf("persistence line printed %d times, want it repeated before the model wait\n---\n%s", n, out)
	}
	// And withdrawn at the end: this process is done, so the instruction is
	// no longer true.
	done := strings.Index(out, setupTerminalDoneLine)
	if done < 0 || done < strings.LastIndex(out, setupKeepTerminalOpenLine) {
		t.Errorf("the terminal never withdrew the keep-open instruction\n---\n%s", out)
	}
}

// The regression bar for the paths §18-12 requires to stay unchanged: with
// no browser driving, nothing about keeping a terminal open is printed —
// there is no browser to keep it open for.
func TestRunInitViaDaemon_NoBrowserSaysNothingAboutKeepingOpen(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner := scriptStdin("n\n")
	d := &promptsDaemon{statusSeq: []management.InferenceStatus{readyStatus()}}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{noBrowser: true})

	if strings.Contains(out, setupKeepTerminalOpenLine) {
		t.Errorf("--no-browser run printed the browser-handoff warning\n---\n%s", out)
	}
	if strings.Contains(out, setupTerminalDoneLine) {
		t.Errorf("--no-browser run printed the browser-handoff wrap-up\n---\n%s", out)
	}
}

// #186 ordering, stated directly: the coding-agent question comes after
// the model is ready. It used to sit above the engine install, so it
// interrupted a multi-GB download to ask about coding tools.
func TestRunInitViaDaemon_IntegrationComesAfterTheModelWait(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner := scriptStdin("n\n")
	d := &promptsDaemon{statusSeq: []management.InferenceStatus{readyStatus()}}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{noBrowser: true})

	ready := strings.Index(out, bundledModel+" ready")
	integ := strings.Index(out, "Coding-agent integration")
	if ready < 0 || integ < 0 {
		t.Fatalf("missing markers (ready=%d integration=%d)\n---\n%s", ready, integ, out)
	}
	if integ < ready {
		t.Errorf("the coding-agent question was asked before the model was ready\n---\n%s", out)
	}
}

// #188: a failed engine install must end the run with an explanation and
// a retry command — not with a model wait for an engine that will never
// start, which cost an observed 2.5 hours on a real host.
func TestRunInitViaDaemon_EngineInstallFailureSkipsTheWait(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner := scriptStdin("")
	stubEngineInstallFailure(t)

	d := &promptsDaemon{
		// no_engine forever: the state the old code waited out to the
		// full setup budget.
		statusSeq: []management.InferenceStatus{{SubsystemState: "no_engine"}},
		setupState: management.SetupStateResponse{
			Active: true, DesiredEngine: "ollama", StateDir: t.TempDir(),
		},
	}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{skipIntegration: true})

	for _, want := range []string{
		"The AI engine could not be installed on this device.",
		"Retry the install with:",
		"local AI still needs installing", // the degraded summary, not the success box
	} {
		if !strings.Contains(out, want) {
			t.Errorf("engine-failure run missing %q\n---\n%s", want, out)
		}
	}
	for _, unwanted := range []string{
		"Waiting for the AI engine to start", // the wait was skipped entirely
		"Waired is ready — everything completed successfully",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("engine-failure run still printed %q\n---\n%s", unwanted, out)
		}
	}
}

// stubEngineInstallFailure makes the executor's engine install fail on
// every OS and privilege level: an unelevated Linux/Windows runner never
// reaches the installer (it refuses for want of rights, which is itself
// a reported failure), while macOS and a root runner do — and the stub
// fails there. Detection is stubbed too so a developer box that happens
// to have ollama does not take the "already present" branch.
func stubEngineInstallFailure(t *testing.T) {
	t.Helper()
	origInstall, origDetect := setupInstallEngine, setupDetectEngine
	setupInstallEngine = func(bool, string, func(infruntime.OllamaInstallProgress)) error {
		return errors.New("no space left on device")
	}
	setupDetectEngine = func(context.Context) setup.OllamaDetection { return setup.OllamaDetection{} }
	t.Cleanup(func() { setupInstallEngine, setupDetectEngine = origInstall, origDetect })
}
