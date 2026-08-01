package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// pullStub serves /waired/v1/status (reachability) and a scripted sequence of
// /waired/v1/inference/status snapshots; the last snapshot repeats once the
// sequence is exhausted.
type pullStub struct {
	mu   sync.Mutex
	seq  []management.InferenceStatus
	call int
}

func (p *pullStub) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/waired/v1/inference/status", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		i := p.call
		if i >= len(p.seq) {
			i = len(p.seq) - 1
		}
		p.call++
		snap := p.seq[i]
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(snap)
	})
	return httptest.NewServer(mux)
}

func activeSel(model string) *management.ActiveSelection {
	return &management.ActiveSelection{ModelID: model}
}

func downloadingSnap(model string, completed, total int64) management.InferenceStatus {
	return management.InferenceStatus{
		SubsystemState: "loading",
		Active:         activeSel(model),
		Models: management.ModelsSnapshot{
			Downloading: []string{model},
			Downloads:   []management.ModelDownload{{Model: model, CompletedBytes: completed, TotalBytes: total}},
		},
	}
}

// The happy path: engine still coming up (no_engine), then the model downloads
// (progress rendered), then it's ready — waitForBundledModel must render a
// progress line and return true.
func TestWaitForBundledModel_NoEngineThenDownloadThenReady(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	const mb = 1 << 20
	stub := &pullStub{seq: []management.InferenceStatus{
		{SubsystemState: "no_engine"},
		downloadingSnap("qwen", 1*mb, 4*mb),
		downloadingSnap("qwen", 3*mb, 4*mb),
		{SubsystemState: "ready", Active: activeSel("qwen"), Models: management.ModelsSnapshot{Ready: []string{"qwen"}}},
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false /*tty*/, benchPollDeadline, false, nil, nil) {
		t.Fatalf("expected ready=true; out=%q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "Downloading qwen") {
		t.Errorf("expected a download progress line, got: %q", s)
	}
	if !strings.Contains(s, "qwen ready") {
		t.Errorf("expected the ready confirmation, got: %q", s)
	}
}

// A terminal pull failure returns false with a retry hint, without hanging.
func TestWaitForBundledModel_PullFailed(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{
		{SubsystemState: "pull_failed", Active: activeSel("qwen"), Models: management.ModelsSnapshot{Failed: []string{"qwen"}}},
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil) {
		t.Fatalf("pull_failed must return false")
	}
	if !strings.Contains(out.String(), "Model download failed") {
		t.Errorf("expected a failure notice, got: %q", out.String())
	}
}

// A no_engine that never resolves gives up after the grace (not the full
// deadline) and must not hang.
func TestWaitForBundledModel_NoEnginePersists(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 20*time.Millisecond, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{{SubsystemState: "no_engine"}}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	var ready bool
	done := make(chan struct{})
	go func() {
		ready = waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForBundledModel hung on a persistent no_engine state")
	}
	if ready {
		t.Errorf("persistent no_engine must return false")
	}
	if !strings.Contains(out.String(), "AI engine still isn't up") {
		t.Errorf("expected the no_engine grace skip, got: %q", out.String())
	}
	if !strings.Contains(out.String(), "waired doctor") {
		t.Errorf("expected an actionable diagnostics hint on the grace skip, got: %q", out.String())
	}
}

// During a browser setup the no_engine grace must NOT end the wait: the
// executor is about to install the very engine that grace was written to
// give up on. Before waired#835 this cut the terminal's residency to 3
// minutes on exactly the hosts the onboarding flow targets, so the
// executor was gone before the wizard's first instruction arrived.
func TestWaitForBundledModel_NoEngineGraceIgnoredDuringSetup(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 20*time.Millisecond, time.Minute)
	const mb = 1 << 20
	stub := &pullStub{seq: []management.InferenceStatus{
		// Long enough that the 20 ms grace would have fired several times.
		{SubsystemState: "no_engine"}, {SubsystemState: "no_engine"},
		{SubsystemState: "no_engine"}, {SubsystemState: "no_engine"},
		{SubsystemState: "no_engine"}, {SubsystemState: "no_engine"},
		// The executor finishes installing and the pull starts.
		downloadingSnap("qwen", 1*mb, 4*mb),
		{SubsystemState: "ready", Active: activeSel("qwen"), Models: management.ModelsSnapshot{Ready: []string{"qwen"}}},
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false /*tty*/, benchPollDeadline, true /*engineComing*/, nil, nil) {
		t.Fatalf("engine-coming wait gave up on no_engine; out=%q", out.String())
	}
	if strings.Contains(out.String(), "AI engine still isn't up") {
		t.Errorf("engine-coming wait printed the give-up notice: %q", out.String())
	}
}

// #308: awaitSetupBudget's 3-minute grace is one window, and an operator
// still reading the model picker when it closes drops the whole run into
// terminal-driven mode on the short legacy budget. When the browser setup
// finally starts, this wait must notice: withdraw the takeover offer, say
// what the window is doing now, and switch to the residency budget.
func TestWaitForBundledModel_BrowserStartExtendsTheBudget(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	const mb = 1 << 20
	stub := &pullStub{seq: []management.InferenceStatus{
		downloadingSnap("qwen", 1*mb, 4*mb),
		downloadingSnap("qwen", 2*mb, 4*mb),
		downloadingSnap("qwen", 3*mb, 4*mb),
		{SubsystemState: "ready", Active: activeSel("qwen"), Models: management.ModelsSnapshot{Ready: []string{"qwen"}}},
	}}
	srv := stub.server()
	defer srv.Close()

	// The browser setup is observed from inside the wait — the wait itself
	// started in terminal-driven mode, on the legacy budget. (That the
	// watch stays quiet until the setup really starts is pinned in
	// setup_watch_test.go; here every look reports it.)
	state := &scriptedState{states: []management.SetupStateResponse{activeState()}}
	watch := newScriptedWatch(t, state)
	enter := newTakeoverWatch(newStdinReader(strings.NewReader("")))

	var out strings.Builder
	// A budget the old code gave up on before the download could finish;
	// the edge is read before the first deadline check, so this is a
	// deterministic bar rather than a race with the tick.
	if !waitForBundledModel(srv.URL, &out, false, time.Nanosecond, false, enter, watch) {
		t.Fatalf("the wait gave up on a budget the browser setup should have replaced; out=%q", out.String())
	}
	s := out.String()
	if strings.Contains(s, "Model still downloading") {
		t.Errorf("the wait expired on the legacy budget: %q", s)
	}
	if got := strings.Count(s, takeoverClosedLine); got != 1 {
		t.Errorf("takeover offer withdrawn %d times, want exactly 1: %q", got, s)
	}
	if !strings.Contains(s, setupKeepTerminalOpenLine) {
		t.Errorf("the handoff did not say to keep this terminal open: %q", s)
	}
	if !enter.Closed() {
		t.Error("the takeover offer is still standing after the browser committed")
	}
	if !watch.Started() {
		t.Error("the watch did not latch the browser setup for the caller")
	}
}

// The same edge must disarm the no_engine grace. The wait entered
// terminal-driven, so the grace was already counting down on an engine
// nobody was installing — and the wizard that just started is about to
// install exactly that engine (the #188 grace, seen from the other side).
func TestWaitForBundledModel_BrowserStartDisarmsTheNoEngineGrace(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 20*time.Millisecond, time.Minute)
	const mb = 1 << 20
	seq := []management.InferenceStatus{}
	for i := 0; i < 60; i++ { // well past the 20 ms grace
		seq = append(seq, management.InferenceStatus{SubsystemState: "no_engine"})
	}
	seq = append(seq,
		downloadingSnap("qwen", 1*mb, 4*mb),
		management.InferenceStatus{SubsystemState: "ready", Active: activeSel("qwen"),
			Models: management.ModelsSnapshot{Ready: []string{"qwen"}}})
	stub := &pullStub{seq: seq}
	srv := stub.server()
	defer srv.Close()

	// Inactive for the first two looks, so the grace is armed before the
	// browser setup arrives. The desired engine is not installed yet, so
	// the edge reports engineComing.
	state := &scriptedState{states: []management.SetupStateResponse{
		{}, {}, {Active: true, DesiredEngine: "ollama"},
	}}
	watch := newScriptedWatch(t, state)

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false /*engineComing*/, nil, watch) {
		t.Fatalf("the wait gave up on no_engine after the browser setup started; out=%q", out.String())
	}
	if strings.Contains(out.String(), "AI engine still isn't up") {
		t.Errorf("the wait printed the give-up notice for an engine the wizard is installing: %q", out.String())
	}
}

// A confirmed takeover ends the wait so the operator can drive setup
// from the terminal. Enter alone only opens the question (#184), so the
// scripted stdin has to answer it.
func TestWaitForBundledModel_EndedByConfirmedTakeover(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{{SubsystemState: "no_engine"}}}
	srv := stub.server()
	defer srv.Close()

	enter := newTakeoverWatch(newStdinReader(strings.NewReader("\ny\n")))

	var out strings.Builder
	if waitForBundledModel(srv.URL, &out, false, benchPollDeadline, true, enter, nil) {
		t.Fatal("a taken-over wait must return false")
	}
	s := out.String()
	for _, want := range []string{"Take over setup in this terminal?", "Continuing in the background"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in the takeover output: %q", want, s)
		}
	}
	if !enter.Fired() {
		t.Error("the watch did not record the takeover")
	}
}

// The same keystroke WITHOUT the confirmation must leave the wait
// running: a stray Enter — the one the sign-in step teaches — cannot
// silently switch the run into terminal mode (#184).
func TestWaitForBundledModel_BareEnterDoesNotTakeOver(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	const mb = 1 << 20
	stub := &pullStub{seq: []management.InferenceStatus{
		{SubsystemState: "no_engine"},
		downloadingSnap("qwen", 1*mb, 4*mb),
		{SubsystemState: "ready", Active: activeSel("qwen"), Models: management.ModelsSnapshot{Ready: []string{"qwen"}}},
	}}
	srv := stub.server()
	defer srv.Close()

	// One Enter opens the question; the second answers it with the
	// default, No. The wait must run to completion either way.
	enter := newTakeoverWatch(newStdinReader(strings.NewReader("\n\n")))

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, true, enter, nil) {
		t.Fatalf("a declined takeover ended the wait; out=%q", out.String())
	}
	if enter.Fired() {
		t.Fatal("a bare Enter took the terminal over without confirmation")
	}
	if !strings.Contains(out.String(), "Continuing in your browser") {
		t.Errorf("the declined takeover was not acknowledged: %q", out.String())
	}
}

// As the engine moves through its phases, waitForBundledModel must print one
// concise step line per distinct subsystem_state (not one per poll), then the
// download bar, then the ready confirmation — so the user sees progress instead
// of a silent wait. Repeated states must not repeat their line.
func TestWaitForBundledModel_StepsThroughPhases(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	const mb = 1 << 20
	stub := &pullStub{seq: []management.InferenceStatus{
		{SubsystemState: "initializing"},
		{SubsystemState: "initializing"}, // repeat: must not reprint
		{SubsystemState: "no_engine"},
		{SubsystemState: "awaiting_model", Active: activeSel("qwen")},
		{SubsystemState: "awaiting_model", Active: activeSel("qwen")}, // repeat
		downloadingSnap("qwen", 1*mb, 4*mb),
		downloadingSnap("qwen", 3*mb, 4*mb),
		{SubsystemState: "ready", Active: activeSel("qwen"), Models: management.ModelsSnapshot{Ready: []string{"qwen"}}},
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false /*tty*/, benchPollDeadline, false, nil, nil) {
		t.Fatalf("expected ready=true; out=%q", out.String())
	}
	s := out.String()
	for _, want := range []string{
		"Starting the AI engine…",
		"Waiting for the AI engine to start…",
		"Preparing to download qwen…",
		"Downloading qwen",
		"qwen ready",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected step %q in output; got: %q", want, s)
		}
	}
	// Dedup: a repeated state prints its line exactly once.
	if n := strings.Count(s, "Starting the AI engine…"); n != 1 {
		t.Errorf("initializing step should print once, printed %d times: %q", n, s)
	}
	if n := strings.Count(s, "Preparing to download qwen…"); n != 1 {
		t.Errorf("awaiting_model step should print once, printed %d times: %q", n, s)
	}
}
