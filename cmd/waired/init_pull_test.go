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
	"github.com/waired-ai/waired-agent/proto/signer"
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
	if !waitForBundledModel(srv.URL, &out, false /*tty*/, benchPollDeadline, false, nil, nil, nil).ready {
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
	if waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil, nil).ready {
		t.Fatalf("pull_failed must return false")
	}
	if !strings.Contains(out.String(), "Model download failed") {
		t.Errorf("expected a failure notice, got: %q", out.String())
	}
}

// TestWaitForBundledModel_PullFailedSaysWhyAndDropsTheDeadEnd is
// waired-agent#328 on the terminal's side. Product contract, and it
// inverts what the old line said on both counts:
//
//   - "the agent will keep retrying" was false. #306's bounded retry
//     lives inside runPullJob and this line prints only once it is spent;
//     a full disk is not retried at all.
//   - "`waired runtimes benchmark` later" pointed at the one command
//     guaranteed to refuse in this exact state — its pull_failed arm
//     exists to say "Model download failed; skipping…".
func TestWaitForBundledModel_PullFailedSaysWhyAndDropsTheDeadEnd(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	const reason = "no space left on device"
	stub := &pullStub{seq: []management.InferenceStatus{{
		SubsystemState: "pull_failed", Active: activeSel("qwen"),
		Models: management.ModelsSnapshot{
			Failed:   []string{"qwen"},
			Failures: []management.ModelFailure{{Model: "qwen", Error: reason}},
		},
	}}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	res := waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil, nil)
	if res.ready {
		t.Fatalf("pull_failed must return false")
	}
	// engineFailure is #310's channel for an engine that could not come up.
	// A pull that failed on a healthy engine is a soft skip, not that fault.
	if res.engineFailure != "" {
		t.Errorf("engineFailure = %q, want none — the ENGINE did not fail here", res.engineFailure)
	}
	s := out.String()
	if !strings.Contains(s, reason) {
		t.Errorf("failure notice does not say why: %q", s)
	}
	if strings.Contains(s, "keep retrying") {
		t.Errorf("still promises a retry that is not coming: %q", s)
	}
	if strings.Contains(s, "runtimes benchmark") {
		t.Errorf("still points at the command that refuses in this state: %q", s)
	}
	if !strings.Contains(s, "waired models pull qwen") {
		t.Errorf("dropped the retry command that does work: %q", s)
	}
}

// TestWaitFailureReasonPicksTheRightModel: two concurrent pulls are
// ordinary, and quoting one model's cause under another's name would be
// worse than quoting none. Product contract.
func TestWaitFailureReasonPicksTheRightModel(t *testing.T) {
	st := management.InferenceStatus{
		Active: activeSel("bundled-model"),
		Models: management.ModelsSnapshot{
			Failed: []string{"bundled-model", "chosen-model"},
			Failures: []management.ModelFailure{
				{Model: "bundled-model", Error: "start ollama: context canceled"},
				{Model: "chosen-model", Error: "no space left on device"},
			},
		},
	}
	// want=="" is the bundled path: the active model is the subject.
	if got := waitFailureReason(st, ""); got != "start ollama: context canceled" {
		t.Errorf("waitFailureReason(active) = %q", got)
	}
	// want!="" is the wizard-chosen path.
	if got := waitFailureReason(st, "chosen-model"); got != "no space left on device" {
		t.Errorf("waitFailureReason(chosen) = %q", got)
	}
	// Nothing recorded: the caller must degrade, not invent.
	if got := waitFailureReason(st, "third-model"); got != "" {
		t.Errorf("waitFailureReason(unknown) = %q, want empty", got)
	}
	if got := waitFailureReason(management.InferenceStatus{}, ""); got != "" {
		t.Errorf("waitFailureReason(no active) = %q, want empty", got)
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
		ready = waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil, nil).ready
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
	if !waitForBundledModel(srv.URL, &out, false /*tty*/, benchPollDeadline, true /*engineComing*/, nil, nil, nil).ready {
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
	if !waitForBundledModel(srv.URL, &out, false, time.Nanosecond, false, enter, watch, nil).ready {
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

	// The edge is read on the wait's FIRST tick, before the no_engine
	// grace has anything to arm from — which is what makes this
	// deterministic rather than a race between three 1 ms ticks and a
	// 20 ms grace. It lost that race on Windows, where a 1 ms sleep is
	// ~15 ms: the grace expired before the third look ever happened.
	//
	// The bar survives the change: without the fix engineComing stays
	// false, the grace arms on this same tick and fires, and the wait
	// prints the give-up notice. The desired engine is not installed yet,
	// so the edge reports engineComing.
	state := &scriptedState{states: []management.SetupStateResponse{
		{Active: true, DesiredEngine: "ollama"},
	}}
	watch := newScriptedWatch(t, state)

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false /*engineComing*/, nil, watch, nil).ready {
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
	if waitForBundledModel(srv.URL, &out, false, benchPollDeadline, true, enter, nil, nil).ready {
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
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, true, enter, nil, nil).ready {
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
	if !waitForBundledModel(srv.URL, &out, false /*tty*/, benchPollDeadline, false, nil, nil, nil).ready {
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

// The fixtures below reproduce the rc7 Windows host of #306: the agent is
// serving the model it picked for itself while the operator's browser
// choice is still downloading. Getting this shape right is the whole
// difference between a test that pins the fix and one that passes without
// it — Active must stay on the AGENT's model for the length of the
// download, because the daemon only commits Active once weights are Ready.
// Reusing downloadingSnap for the wizard's model would set Active AND
// Downloads to the same id, and then both the label and the progress
// lookup pass unfixed.
const (
	wizardModel = "wizard-35b"  // what the operator chose in the browser
	agentModel  = "bundled-14b" // what the agent auto-selected for itself
	testGB      = 1_000_000_000 // download.HumanBytes is 1000-based
)

// racingSnap: both models are in flight, the agent's own listed first so
// the Downloads[0] fallback would pick the wrong one.
func racingSnap(sub string) management.InferenceStatus {
	return management.InferenceStatus{
		SubsystemState: sub,
		Active:         activeSel(agentModel),
		Models: management.ModelsSnapshot{
			Ready:       []string{agentModel},
			Downloading: []string{agentModel, wizardModel},
			Downloads: []management.ModelDownload{
				// 22% and 11%. The wizard's is deliberately in the same
				// 10% bucket as the pre-handoff bar in the retarget test
				// below, because that bucket is what drawDownloadLine
				// dedups on off a TTY.
				{Model: agentModel, CompletedBytes: 2 * testGB, TotalBytes: 9 * testGB},
				{Model: wizardModel, CompletedBytes: 5 * testGB, TotalBytes: 44 * testGB},
			},
		},
	}
}

// bothReady: the wizard's model has landed. Active is still the agent's
// own — activation is a separate step the terminal must not wait on.
func bothReady() management.InferenceStatus {
	return management.InferenceStatus{
		SubsystemState: "ready",
		Active:         activeSel(agentModel),
		Models:         management.ModelsSnapshot{Ready: []string{agentModel, wizardModel}},
	}
}

func fixedTarget(t *testing.T, model string) *modelTarget {
	t.Helper()
	return newScriptedTarget(t, &scriptedState{
		states: []management.SetupStateResponse{wizardState(model)},
	})
}

// The #306 bar. subsystem_state is "ready" and the agent's own model is in
// models.ready from the first tick, so the pre-fix wait announced
// "bundled-14b ready" and returned while the operator's 44 GB choice was
// still coming down.
func TestWaitForBundledModel_WaitsForTheWizardsModelNotTheActiveOne(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{
		racingSnap("ready"), racingSnap("ready"), bothReady(),
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil,
		fixedTarget(t, wizardModel)).ready {
		t.Fatalf("the wait never saw the wizard's model land; out=%q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, wizardModel+" ready") {
		t.Errorf("expected the wizard's model announced ready, got: %q", s)
	}
	if strings.Contains(s, agentModel) {
		t.Errorf("the terminal named the model the AGENT picked, which is #306: %q", s)
	}
	// The wait must have WATCHED the download, not just ended up with the
	// right label. Both racing snapshots report subsystem_state "ready" —
	// true of the agent's own model — so a wait that still consults it
	// returns on the first tick, before the operator's model exists
	// anywhere, and this bar is the proof that it did not.
	if !strings.Contains(s, "Downloading "+wizardModel) {
		t.Errorf("the wait declared ready without ever watching the download: %q", s)
	}
	// The handoff note exists to explain a bar that restarts under a
	// different model. Here the target was known before anything was
	// printed, so there is nothing to explain and it must stay quiet.
	if strings.Contains(s, "Now waiting for the model chosen in your browser") {
		t.Errorf("narrated a handoff that never happened: %q", s)
	}
}

// The other half of the same root cause: activeDownload falls back to
// Downloads[0] when nothing matches Active, so with two pulls in flight the
// bar counted the wrong model's bytes. Here Active matches exactly, so the
// pre-fix code renders 2.0/9.0 GB — a plausible-looking, wrong bar.
func TestWaitForBundledModel_RendersTheWizardsDownloadNotTheBundledOne(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{
		racingSnap("loading"), racingSnap("loading"), racingSnap("loading"), bothReady(),
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil,
		fixedTarget(t, wizardModel)).ready {
		t.Fatalf("expected ready=true; out=%q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "44.0 GB") {
		t.Errorf("expected the wizard's 44 GB transfer on the bar, got: %q", s)
	}
	if strings.Contains(s, "9.0 GB") {
		t.Errorf("the bar counted the agent's own download instead: %q", s)
	}
}

// subsystem_state "pull_failed" answers for the ACTIVE model. The agent's
// own pull failing says nothing about the operator's, and must not end a
// wait keyed to it.
func TestWaitForBundledModel_ActiveModelPullFailureDoesNotEndTheWizardsWait(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	failed := racingSnap("pull_failed")
	failed.Models.Ready = nil
	failed.Models.Failed = []string{agentModel}
	// Repeated past switchFailedStreak on purpose: a wait that still reads
	// the subsystem state would otherwise have its streak absorb the whole
	// sequence and reach the happy ending anyway.
	stub := &pullStub{seq: []management.InferenceStatus{
		failed, failed, failed, failed, bothReady(),
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil,
		fixedTarget(t, wizardModel)).ready {
		t.Fatalf("the agent's own failed pull ended a wait for a different model; out=%q", out.String())
	}
	if strings.Contains(out.String(), "Model download failed") {
		t.Errorf("reported a failure for a model this wait was not watching: %q", out.String())
	}
}

// A terminal failure of the wizard's own model does end the wait, and the
// retry hint has to name that model — the id goes inside a copy-pasteable
// `waired models pull <id>`.
func TestWaitForBundledModel_TerminalWizardFailureNamesTheWizardsModel(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, 2*time.Second)
	failed := racingSnap("ready")
	failed.Models.Downloading = []string{agentModel}
	failed.Models.Failed = []string{wizardModel}
	stub := &pullStub{seq: []management.InferenceStatus{failed}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil,
		fixedTarget(t, wizardModel)).ready {
		t.Fatalf("a terminally failed target must return false; out=%q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "waired models pull "+wizardModel) {
		t.Errorf("expected a retry command naming the wizard's model, got: %q", s)
	}
	if strings.Contains(s, "Model still downloading") {
		t.Errorf("the wait ran to its deadline instead of reading the failure: %q", s)
	}
}

// One Failed observation is not terminal for a wizard-chosen model: the
// agent records that for an in-flight pull as it shuts down, and the
// post-restart bootstrap picks the same model straight back up.
func TestWaitForBundledModel_TransientWizardFailureIsToleratedByTheStreak(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	failed := racingSnap("ready")
	failed.Models.Downloading = []string{agentModel}
	failed.Models.Failed = []string{wizardModel}
	stub := &pullStub{seq: []management.InferenceStatus{failed, failed, bothReady()}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil,
		fixedTarget(t, wizardModel)).ready {
		t.Fatalf("a transient failed record ended the wait; out=%q", out.String())
	}
	if strings.Contains(out.String(), "Model download failed") {
		t.Errorf("gave up on the first Failed observation: %q", out.String())
	}
}

// Every phase note that names a model names the ACTIVE one, so with a
// target they all name the wrong model. The phases that describe the
// ENGINE keep their wording. The dedup also has to survive several states
// collapsing onto one sentence.
func TestWaitForBundledModel_NarratesTheWizardsModelThroughThePhases(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	waiting := management.InferenceStatus{
		Active: activeSel(agentModel),
		Models: management.ModelsSnapshot{Ready: []string{agentModel}, Downloading: []string{wizardModel}},
	}
	awaiting, loading := waiting, waiting
	awaiting.SubsystemState = "awaiting_model"
	loading.SubsystemState = "loading"
	stub := &pullStub{seq: []management.InferenceStatus{
		{SubsystemState: "initializing"}, awaiting, loading, awaiting, bothReady(),
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil,
		fixedTarget(t, wizardModel)).ready {
		t.Fatalf("expected ready=true; out=%q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "Starting the AI engine…") {
		t.Errorf("the engine phase lost its own wording: %q", s)
	}
	if n := strings.Count(s, "Preparing to download "+wizardModel+"…"); n != 1 {
		t.Errorf("the prepare note named the wizard's model %d times, want exactly 1: %q", n, s)
	}
	if strings.Contains(s, agentModel) {
		t.Errorf("a phase note named the model the agent picked: %q", s)
	}
}

// #308 x #306: the browser commits partway into a wait that started
// terminal-driven, so the target arrives with a bar for the agent's own
// model already on screen. The two percentages are deliberately in the same
// 10% bucket, which is what drawDownloadLine dedups on off a TTY — without
// resetting the line state the new model's bar is swallowed whole.
func TestWaitForBundledModel_RetargetsWhenTheWizardCommitsMidWait(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{
		downloadingSnap(agentModel, 1*testGB, 9*testGB), // 11%
		downloadingSnap(agentModel, 1*testGB, 9*testGB),
		racingSnap("loading"), // the wizard's is 11% too
		bothReady(),
	}}
	srv := stub.server()
	defer srv.Close()

	target := newScriptedTarget(t, &scriptedState{states: []management.SetupStateResponse{
		{}, {}, wizardState(wizardModel),
	}})

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil, target).ready {
		t.Fatalf("expected ready=true; out=%q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "Downloading "+agentModel) {
		t.Errorf("the pre-handoff bar is missing: %q", s)
	}
	if !strings.Contains(s, "Downloading "+wizardModel) {
		t.Errorf("the wizard's bar never appeared after the handoff: %q", s)
	}
	if n := strings.Count(s, "Now waiting for the model chosen in your browser"); n != 1 {
		t.Errorf("narrated the handoff %d times, want exactly 1: %q", n, s)
	}
	if !strings.Contains(s, wizardModel+" ready") {
		t.Errorf("expected the wizard's model announced ready, got: %q", s)
	}
}

// A model the daemon never starts on must not park the terminal. The
// daemon can refuse a desired model permanently and cannot say so on any
// local endpoint, and under a browser setup this wait runs an eight-hour
// budget — so without the grace this is a hang, not a wrong answer.
func TestWaitForBundledModel_GivesUpOnAModelTheDaemonNeverPulls(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkTargetPullGrace(t, 20*time.Millisecond)
	// The agent is happily serving its own model; the wizard's is on none
	// of the daemon's books at all.
	stub := &pullStub{seq: []management.InferenceStatus{{
		SubsystemState: "ready",
		Active:         activeSel(agentModel),
		Models:         management.ModelsSnapshot{Ready: []string{agentModel}},
	}}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	var ready bool
	done := make(chan struct{})
	go func() {
		ready = waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil,
			fixedTarget(t, "never-pulled")).ready
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait parked on a model the daemon was never going to pull")
	}
	if ready {
		t.Error("a model that never appeared must not be reported ready")
	}
	if !strings.Contains(out.String(), "hasn't started downloading") {
		t.Errorf("expected the wait to say nothing had started, got: %q", out.String())
	}
	if strings.Contains(out.String(), agentModel+" ready") {
		t.Errorf("fell back to reporting the agent's own model: %q", out.String())
	}
}

// The grace must not keep counting across an engine that goes away.
// Applying a desired model is gated on an engine being present, so while
// there is none the model being on no list says nothing — and the engine
// install a wizard drives is exactly what takes it away and brings it
// back. A grace armed before the engine went would otherwise be long
// expired by the time it returns, and fire on the first frame after,
// before the reconciler has had one pass to dispatch the pull.
func TestWaitForBundledModel_TargetGraceDoesNotCountAcrossAnEngineRestart(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkTargetPullGrace(t, 50*time.Millisecond)
	// The engine is up and the wizard's model is on none of its lists, so
	// the grace arms here.
	invisible := management.InferenceStatus{
		SubsystemState: "ready",
		Active:         activeSel(agentModel),
		Models:         management.ModelsSnapshot{Ready: []string{agentModel}},
	}
	seq := []management.InferenceStatus{invisible, invisible}
	// The engine goes down for longer than the grace.
	for i := 0; i < 200; i++ {
		seq = append(seq, management.InferenceStatus{SubsystemState: "no_engine"})
	}
	// It comes back, and the reconciler has not dispatched the pull yet.
	seq = append(seq, invisible, racingSnap("loading"), bothReady())
	stub := &pullStub{seq: seq}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, true /*engineComing*/, nil, nil,
		fixedTarget(t, wizardModel)).ready {
		t.Fatalf("the grace carried across the engine restart; out=%q", out.String())
	}
	if strings.Contains(out.String(), "hasn't started downloading") {
		t.Errorf("gave up on a model the engine had only just come back to pull: %q", out.String())
	}
}

// shrinkTargetPullGrace shrinks the invisible-target grace for a test.
// setBenchTiming does not cover it: it is not one of the benchmark timings.
func shrinkTargetPullGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := targetPullGrace
	targetPullGrace = d
	t.Cleanup(func() { targetPullGrace = old })
}

// refusingTarget names a model the daemon has already refused to apply.
func refusingTarget(t *testing.T, model, code, detail string) *modelTarget {
	t.Helper()
	return newScriptedTarget(t, &scriptedState{
		states: []management.SetupStateResponse{refusedState(model, code, detail)},
	})
}

// invisibleSnap is the daemon serving its own model with the wizard's on
// none of its working lists. notPresent is what the daemon says it COULD
// still start on — empty for a daemon too old to answer (#403).
func invisibleSnap(notPresent ...string) management.InferenceStatus {
	return management.InferenceStatus{
		SubsystemState: "ready",
		Active:         activeSel(agentModel),
		Models: management.ModelsSnapshot{
			Ready:      []string{agentModel},
			NotPresent: notPresent,
		},
	}
}

// runInvisibleWait drives waitForBundledModel against a daemon that will
// never start on the target, with a hard cap so a wait that parks fails
// loudly instead of hanging the package.
func runInvisibleWait(t *testing.T, st management.InferenceStatus, target *modelTarget) (string, bool) {
	t.Helper()
	stub := &pullStub{seq: []management.InferenceStatus{st}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	var ready bool
	done := make(chan struct{})
	go func() {
		ready = waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil, target).ready
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the wait parked; out=%q", out.String())
	}
	return out.String(), ready
}

// TestWaitForBundledModel_StopsOnARefusedModel is waired-agent#404's
// terminal-side bar. Product contract (#404): a refusal the daemon has
// recorded is reported here, immediately, instead of being waited out.
//
// targetPullGrace is left at its real five minutes on purpose: the point
// is that the wait no longer reaches it, and a shrunk grace could not
// tell that from the grace expiring.
func TestWaitForBundledModel_StopsOnARefusedModel(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	out, ready := runInvisibleWait(t, invisibleSnap(agentModel, wizardModel),
		refusingTarget(t, wizardModel, signer.SetupErrorInternal, "model downloads are turned off on this device"))

	if ready {
		t.Error("a refused model was reported ready")
	}
	if !strings.Contains(out, "Waired can't download "+wizardModel) {
		t.Errorf("expected the refusal to be named, got: %q", out)
	}
	if !strings.Contains(out, "model downloads are turned off on this device") {
		t.Errorf("expected the daemon's own reason, got: %q", out)
	}
	if !strings.Contains(out, "Pick a different model in your browser") {
		t.Errorf("expected the recovery line, got: %q", out)
	}
	if strings.Contains(out, "hasn't started downloading") {
		t.Errorf("fell through to the blind grace: %q", out)
	}
}

// The engine-side code is the one refusal an update can fix, and it is
// what the wizard advises for the same code — so the terminal says the
// same thing rather than sending the operator to pick another model.
func TestWaitForBundledModel_ARefusalTheEngineCanFixSaysSo(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	out, _ := runInvisibleWait(t, invisibleSnap(agentModel, wizardModel),
		refusingTarget(t, wizardModel, signer.SetupErrorEngineNotReady,
			"the engine on this device is too old for this model"))

	if !strings.Contains(out, "waired update") {
		t.Errorf("expected the update route for engine_not_ready, got: %q", out)
	}
}

// An id the daemon's catalog does not carry is answered, not waited out:
// no download is coming, and #403's not_present list is what makes the
// difference between "nothing started yet" and "no such model" visible.
func TestWaitForBundledModel_SaysSoWhenTheDaemonHasNoSuchModel(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	out, ready := runInvisibleWait(t, invisibleSnap(agentModel, wizardModel),
		fixedTarget(t, "model-from-a-newer-catalog"))

	if ready {
		t.Error("a model the daemon does not have was reported ready")
	}
	if !strings.Contains(out, "doesn't have a model called model-from-a-newer-catalog") {
		t.Errorf("expected the unknown-id line, got: %q", out)
	}
	if strings.Contains(out, "hasn't started downloading") {
		t.Errorf("fell through to the blind grace: %q", out)
	}
}

// A daemon that does not answer the not_present question keeps the
// pre-#403 behaviour exactly: the wait must not read an absent list as
// "this model does not exist". Records that the new branch is gated on
// the daemon having spoken.
func TestWaitForBundledModel_AnOlderDaemonKeepsTheGrace(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkTargetPullGrace(t, 20*time.Millisecond)
	out, ready := runInvisibleWait(t, invisibleSnap(), fixedTarget(t, wizardModel))

	if ready {
		t.Error("a model that never appeared must not be reported ready")
	}
	if !strings.Contains(out, "hasn't started downloading") {
		t.Errorf("expected the bounded grace, got: %q", out)
	}
	if strings.Contains(out, "doesn't have a model called") {
		t.Errorf("an absent not_present list was read as a missing model: %q", out)
	}
}

// shrinkPullProgressTiming shrinks the two #382 download tunables. Not part of
// setBenchTiming: those are the benchmark's timings, and a test that wants a
// tiny budget usually wants the ordinary graces left alone.
func shrinkPullProgressTiming(t *testing.T, stall, extension time.Duration) {
	t.Helper()
	os, oe := pullStallGrace, pullProgressExtension
	pullStallGrace, pullProgressExtension = stall, extension
	t.Cleanup(func() { pullStallGrace, pullProgressExtension = os, oe })
}

// runProgressWait drives waitForBundledModel over a scripted status sequence
// with a budget that is already spent by the second poll, and reports what it
// printed plus whether it saw the model become ready.
func runProgressWait(t *testing.T, budget time.Duration, seq []management.InferenceStatus) (string, bool) {
	t.Helper()
	stub := &pullStub{seq: seq}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	var ready bool
	done := make(chan struct{})
	go func() {
		ready = waitForBundledModel(srv.URL, &out, false /*tty*/, budget, false, nil, nil, nil).ready
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("waitForBundledModel hung")
	}
	return out.String(), ready
}

// #382: a download whose byte count keeps advancing must outlive the wait's
// budget. The budget was a fixed wall-clock bet against model-registry
// throughput, which varies ~7x within a day — so the wait gave up on transfers
// that were visibly working, and `waired init` skipped the benchmark on a host
// where nothing was wrong.
//
// The budget here (1 ms) is spent before the first poll returns, so every
// remaining tick is one the old code would not have taken.
func TestWaitForBundledModel_ProgressOutlivesTheBudget(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkPullProgressTiming(t, 2*time.Second, time.Minute)
	const mb = 1 << 20
	out, ready := runProgressWait(t, time.Millisecond, []management.InferenceStatus{
		downloadingSnap("qwen", 1*mb, 8*mb),
		downloadingSnap("qwen", 2*mb, 8*mb),
		downloadingSnap("qwen", 4*mb, 8*mb),
		downloadingSnap("qwen", 7*mb, 8*mb),
		{SubsystemState: "ready", Active: activeSel("qwen"), Models: management.ModelsSnapshot{Ready: []string{"qwen"}}},
	})

	if !ready {
		t.Fatalf("a still-advancing download was abandoned at the budget; out=%q", out)
	}
	if strings.Contains(out, "Model still downloading") {
		t.Errorf("gave up on a progressing download: %q", out)
	}
}

// The other half of the same rule: bytes that stop moving end the wait after
// pullStallGrace. Without this, "progress extends the wait" would be an
// unbounded wait on any host whose first byte arrived.
//
// Note this is FASTER than the old behaviour, not slower — a pull that dies at
// 1 MB used to hold the terminal for the whole remaining budget.
func TestWaitForBundledModel_StalledDownloadEndsTheWait(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkPullProgressTiming(t, 30*time.Millisecond, time.Minute)
	const mb = 1 << 20
	// Advances once, then the same byte count forever (pullStub repeats its
	// last entry).
	out, ready := runProgressWait(t, time.Millisecond, []management.InferenceStatus{
		downloadingSnap("qwen", 1*mb, 8*mb),
		downloadingSnap("qwen", 2*mb, 8*mb),
	})

	if ready {
		t.Error("a stalled download must not be reported ready")
	}
	if !strings.Contains(out, "Model still downloading") {
		t.Errorf("expected the give-up line after the stall grace, got: %q", out)
	}
}

// pullProgressExtension is the backstop: a download that advances forever must
// still end the wait, or a slow link would hold `waired init` open with no
// upper bound — and in CI, past the job's own timeout, which is how the
// nightly Linux leg failed without reaching a single assert.
func TestWaitForBundledModel_ProgressCannotOutrunTheCap(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	// Stall grace far larger than the cap, so only the cap can end this.
	shrinkPullProgressTiming(t, time.Minute, 40*time.Millisecond)
	const mb = 1 << 20
	seq := make([]management.InferenceStatus, 0, 4096)
	for i := 1; i <= 4096; i++ { // always one more byte; never ready
		seq = append(seq, downloadingSnap("qwen", int64(i)*mb, 1<<40))
	}
	out, ready := runProgressWait(t, time.Millisecond, seq)

	if ready {
		t.Error("a never-finishing download must not be reported ready")
	}
	if !strings.Contains(out, "Model still downloading") {
		t.Errorf("expected the cap to end the wait, got: %q", out)
	}
}

// #382 x #306: when a browser wizard retargets the wait to a different model,
// the byte counter must reset with the rest of the per-model render state.
//
// The wizard's model starts its download near zero while the agent's own was
// already most of the way through, so a lastBytes carried across the handoff
// reads EVERY tick of the new download as "no progress" — the extension stops
// working exactly on the multi-GB choice that needs it most, and the stale
// stall deadline from the old model runs out underneath it.
//
// Note the direction: the existing retarget test has the wizard's model
// FURTHER along than the agent's, which cannot expose this. The counts here
// are deliberately the other way round.
func TestWaitForBundledModel_RetargetResetsTheByteCounter(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	// Stall grace short enough that the deadline armed by the agent's own
	// download expires during the wizard's, so only ticks the wait actually
	// counts as progress can keep it alive.
	shrinkPullProgressTiming(t, 30*time.Millisecond, time.Minute)

	const mb = 1 << 20
	seq := []management.InferenceStatus{
		// The agent's own model, nearly done — this is what sets lastBytes high.
		downloadingSnap(agentModel, 8*testGB, 9*testGB),
		downloadingSnap(agentModel, 8*testGB+mb, 9*testGB),
	}
	// The wizard's model, downloading from near zero. Active stays the agent's
	// (the daemon only commits Active once weights are ready), so the wait has
	// to read the wizard's row out of Downloads.
	for i := 1; i <= 400; i++ {
		seq = append(seq, management.InferenceStatus{
			SubsystemState: "loading",
			Active:         activeSel(agentModel),
			Models: management.ModelsSnapshot{
				Ready:       []string{agentModel},
				Downloading: []string{wizardModel},
				Downloads: []management.ModelDownload{
					{Model: wizardModel, CompletedBytes: int64(i) * mb, TotalBytes: 44 * testGB},
				},
			},
		})
	}
	seq = append(seq, bothReady())

	stub := &pullStub{seq: seq}
	srv := stub.server()
	defer srv.Close()
	target := newScriptedTarget(t, &scriptedState{states: []management.SetupStateResponse{
		{}, {}, wizardState(wizardModel),
	}})

	var out strings.Builder
	var ready bool
	done := make(chan struct{})
	go func() {
		// Budget spent immediately: only the progress extension can reach ready.
		ready = waitForBundledModel(srv.URL, &out, false, time.Millisecond, false, nil, nil, target).ready
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("waitForBundledModel hung")
	}

	if !ready {
		t.Fatalf("the wizard's download did not extend the wait — the byte counter appears to have survived the retarget; out=%q", out.String())
	}
}

// A wait that never sees a download is bounded exactly as it was before #382:
// the stall deadline is armed by the first advancing byte, so with no download
// at all the caller's budget is still the only clock.
func TestWaitForBundledModel_NoDownloadStillEndsAtTheBudget(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	// Both new tunables set far beyond the test's patience: if either one
	// could extend a wait that saw no bytes, this would hang instead of fail.
	shrinkPullProgressTiming(t, time.Hour, time.Hour)
	out, ready := runProgressWait(t, 20*time.Millisecond, []management.InferenceStatus{
		{SubsystemState: "loading", Active: activeSel("qwen")},
	})

	if ready {
		t.Error("no model ever became ready")
	}
	if !strings.Contains(out, "Model still downloading") {
		t.Errorf("expected the budget to end the wait unchanged, got: %q", out)
	}
}
