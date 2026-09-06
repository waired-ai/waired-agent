package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeSetupDaemon serves the two executor routes and records every lease
// update the CLI sends.
type fakeSetupDaemon struct {
	mu       sync.Mutex
	state    management.SetupStateResponse
	requests []management.SetupExecutorRequest
	notFound bool // simulate a daemon older than the executor lease
	// onState fires on each /setup/state poll, so a test can time a
	// keystroke to a real point in the flow. The grace loop's polls are
	// the only events that happen strictly AFTER awaitBrowserSetup has
	// discarded pre-offer input (#184) and armed its watch — typing
	// before that is a race the discard is designed to win.
	onState func(poll int)
	polls   int
	// stateFails and postFails break one route at a time, which is the
	// shape #746 is about: the probe is a read and reads fall back to
	// loopback TCP, the attach is a write and writes are socket-only, so
	// a host can serve one and not the other. Both are read under mu,
	// unlike notFound above, because tests flip them mid-flight.
	stateFails bool
	postFails  bool
}

func (d *fakeSetupDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/setup/state", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.polls++
		poll, hook := d.polls, d.onState
		d.mu.Unlock()
		if hook != nil {
			hook(poll)
		}
		if d.notFound {
			http.NotFound(w, r)
			return
		}
		// Read after the hook, not before: the hook is how a test
		// scripts the daemon changing its answer partway through a poll
		// loop, and reading first would serve the pre-hook state.
		d.mu.Lock()
		fails, st := d.stateFails, d.state
		d.mu.Unlock()
		if fails {
			http.Error(w, "state unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("/waired/v1/setup/executor", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.notFound {
			http.NotFound(w, r)
			return
		}
		if d.postFails {
			http.Error(w, "write path unavailable", http.StatusServiceUnavailable)
			return
		}
		var req management.SetupExecutorRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.requests = append(d.requests, req)
		d.state.ExecutorAttached = req.Attached
		d.state.ExecutorElevated = req.Attached && req.Elevated
		// Mirror the daemon's lease-bound install latch (§11.1) so the
		// executor tests see the claim they would see in production.
		switch {
		case !req.Attached:
			d.state.InstallClaimed = ""
		case req.Phase == management.SetupExecutorPhaseInstalling && req.Engine != "":
			d.state.InstallClaimed = req.Engine
		case req.Phase == management.SetupExecutorPhaseDone ||
			req.Phase == management.SetupExecutorPhaseFailed:
			d.state.InstallClaimed = ""
		}
		_ = json.NewEncoder(w).Encode(d.state)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (d *fakeSetupDaemon) setActive(active bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state.Active = active
}

// setState replaces the served state wholesale, for tests that need to
// script more than the active flag.
func (d *fakeSetupDaemon) setState(st management.SetupStateResponse) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state = st
}

// failState / failPost break one route at a time, mid-flight.
func (d *fakeSetupDaemon) failState(fail bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stateFails = fail
}

func (d *fakeSetupDaemon) failPost(fail bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.postFails = fail
}

func (d *fakeSetupDaemon) noted() []management.SetupExecutorRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]management.SetupExecutorRequest(nil), d.requests...)
}

// shrinkSetupTimers keeps these tests fast without changing what they
// assert; the production values live in setup_executor.go.
//
// It deliberately does NOT touch setupExecutorHeartbeatInterval, which is
// the one knob here that is not about speed: post() carries the CURRENT
// step and figures (currentProgress), so a fast heartbeat does not just
// make a test finish sooner — it files extra reports the daemon records,
// against the same step the test is asserting on. Anything counting rows
// is then really asking whether its work fits inside one tick, which is
// true on an idle machine and a coin flip on a loaded CI runner
// (waired-agent#914; the same bet cost TestExecutorSessionProgressIsThrottled
// a local workaround before this helper existed).
//
// Tests that genuinely wait on a beat call shrinkSetupHeartbeat too.
func shrinkSetupTimers(t *testing.T) {
	t.Helper()
	prevPoll, prevResidency := setupStatePollInterval, setupResidencyBudget
	prevGrace := setupAwaitGrace
	setupStatePollInterval = 5 * time.Millisecond
	setupResidencyBudget = 42 * time.Minute // distinguishable from benchPollDeadline
	// Long enough for a scripted keystroke to arrive, short enough that a
	// test which never sends one fails in seconds instead of minutes.
	setupAwaitGrace = 2 * time.Second
	t.Cleanup(func() {
		setupStatePollInterval, setupResidencyBudget = prevPoll, prevResidency
		setupAwaitGrace = prevGrace
	})
}

// shrinkSetupHeartbeat opts a test into a fast lease heartbeat. Only tests
// that wait for a beat need it; for everything else the production 10 s
// interval means no beat fires at all inside the test, which is what keeps
// the daemon's recorded traffic equal to what the test itself drove.
//
// Set before attachSetupExecutor: heartbeat() reads the interval once, when
// its goroutine starts.
func shrinkSetupHeartbeat(t *testing.T) {
	t.Helper()
	prev := setupExecutorHeartbeatInterval
	setupExecutorHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { setupExecutorHeartbeatInterval = prev })
}

// The default inverted above IS the fix for waired-agent#914, so it is
// pinned rather than left to the tests that benefit from it. Put the
// heartbeat back into shrinkSetupTimers and every count assertion in this
// package silently becomes a race again; only this test says so.
//
// A lower-bound wait on purpose (#384's rule): idling longer than the
// interval this helper used to install can only make the assertion truer,
// so an overshoot on a loaded runner is harmless here.
func TestShrinkSetupTimersDoesNotManufactureHeartbeats(t *testing.T) {
	shrinkSetupTimers(t)
	shrinkProgressThrottle(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	// A step has to be set first: a heartbeat repeats whatever step the
	// session is on, so before any Progress call there is nothing for it
	// to duplicate and the test would pass without the fix.
	s.Progress(management.SetupStepEngineDownload, "ollama", 5, 10, 0)
	settled := len(d.noted())
	if settled == 0 {
		t.Fatal("the session filed nothing at all, so idling below proves nothing")
	}

	time.Sleep(50 * time.Millisecond) // 10x the interval shrinkSetupTimers used to set
	if got := len(d.noted()); got != settled {
		t.Fatalf("%d reports after idling, want %d — shrinkSetupTimers is filing "+
			"heartbeats again, and every count assertion in this package is a race", got, settled)
	}
}

// TestExecutorSessionOlderDaemonIsInert is the acceptance-item-12/15
// guard: a CLI from this release run against a daemon that predates the
// executor lease must behave exactly as it does today — no lease, no
// residency extension, prompts intact.
func TestExecutorSessionOlderDaemonIsInert(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{notFound: true}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	if s.Supported() {
		t.Fatal("session reports supported against a 404 daemon")
	}
	// Every method must be a safe no-op.
	s.Installing("ollama")
	s.Done("ollama")
	s.Failed("ollama", signer.SetupErrorPermissionDenied, "boom")
	s.Release()
	if got := len(d.noted()); got != 0 {
		t.Fatalf("posted %d lease updates to an older daemon, want 0", got)
	}

	budget, active, enter, watch := awaitBrowserSetup(s, nil, io.Discard, false, false, false)
	if budget != benchPollDeadline || active {
		t.Fatalf("budget=%v active=%v, want the legacy deadline and no setup", budget, active)
	}
	if took, note := enter.Poll(); took || note != "" {
		t.Fatalf("inert watch produced (%v, %q)", took, note)
	}
	// #308: an older daemon has no setup state to watch, so the wait must
	// not start asking it for one.
	if started, _, _ := watch.Poll(); started || watch.Started() {
		t.Fatal("setup watch reported a browser setup against a 404 daemon")
	}
	if got := len(d.noted()); got != 0 {
		t.Fatalf("posted %d lease updates to an older daemon, want 0", got)
	}
	// The silence is the point, and it is only correct for THIS cause.
	// #746 gave the other causes a note; a 404 must keep saying nothing,
	// because being inert is the documented, correct outcome for it.
	if note := s.AttachNote(); note != "" {
		t.Fatalf("older daemon produced the note %q, want silence", note)
	}
}

// TestExecutorSessionUnreachableDaemonSaysSo is the other half of the
// 404 case above: the same inert session, a cause the operator can act
// on, and — before #746 — the same silence.
func TestExecutorSessionUnreachableDaemonSaysSo(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	srv.Close() // nothing is listening; the probe cannot be answered

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	if s.Supported() {
		t.Fatal("session reports supported against a daemon that never answered")
	}
	note := s.AttachNote()
	if !strings.Contains(note, "could not ask the background service about setup") {
		t.Fatalf("note = %q, want it to name the failed probe", note)
	}

	var out strings.Builder
	reportAttachNote(&out, s)
	if !strings.Contains(out.String(), "Warning: ") {
		t.Fatalf("reported %q, want a warn line", out.String())
	}
}

// TestExecutorSessionAttachPostFailureIsReported is the case the issue
// is named for. The probe is a read and reads fall back to loopback TCP;
// the attach is a write and writes are socket-only (waired#838). So a
// daemon can answer the probe and never receive the lease — and every
// gate downstream reads Supported, which the probe alone set to true.
func TestExecutorSessionAttachPostFailureIsReported(t *testing.T) {
	shrinkSetupTimers(t)
	shrinkSetupHeartbeat(t)
	d := &fakeSetupDaemon{}
	d.failPost(true)
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()

	// Deliberately still supported: the heartbeat re-sends attached=true,
	// so the lease recovers on its own. Going inert here would let one
	// failed write cancel the engine install for the whole run.
	if !s.Supported() {
		t.Fatal("a reachable daemon whose write failed must stay supported")
	}
	note := s.AttachNote()
	if !strings.Contains(note, "could not tell the background service that setup is running") {
		t.Fatalf("note = %q, want it to name the failed attach", note)
	}

	// And the recovery the note promises actually happens.
	d.failPost(false)
	waitForCond(t, func() bool {
		for _, n := range d.noted() {
			if n.Attached {
				return true
			}
		}
		return false
	}, "the heartbeat to re-attach")
}

// TestExecutorSessionCleanAttachIsSilent pins the other direction: the
// note exists for failures, and must not appear on the ordinary path.
func TestExecutorSessionCleanAttachIsSilent(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	if note := s.AttachNote(); note != "" {
		t.Fatalf("clean attach produced the note %q, want silence", note)
	}
	var out strings.Builder
	reportAttachNote(&out, s)
	if out.String() != "" {
		t.Fatalf("clean attach printed %q, want nothing", out.String())
	}
}

func TestExecutorSessionAttachHeartbeatRelease(t *testing.T) {
	shrinkSetupTimers(t)
	shrinkSetupHeartbeat(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	if !s.Supported() {
		t.Fatal("session should be supported against a current daemon")
	}
	waitForCond(t, func() bool { return len(d.noted()) >= 3 }, "heartbeats")
	s.Release()

	notes := d.noted()
	if len(notes) < 2 {
		t.Fatalf("lease updates = %d, want an attach and a release", len(notes))
	}
	if !notes[0].Attached || !notes[0].Elevated {
		t.Fatalf("first update = %+v, want an elevated attach", notes[0])
	}
	last := notes[len(notes)-1]
	if last.Attached {
		t.Fatalf("last update = %+v, want a release", last)
	}
	// Release is idempotent and must not post twice.
	before := len(d.noted())
	s.Release()
	if got := len(d.noted()); got != before {
		t.Fatalf("second Release posted again (%d -> %d)", before, got)
	}
}

// TestExecutorSessionInstallingSurvivesHeartbeat pins the claim: a
// heartbeat issued mid-install must keep reporting "installing", not
// reset the daemon's view to idle — which would drop the install claim
// and let a second elevated install start.
func TestExecutorSessionInstallingSurvivesHeartbeat(t *testing.T) {
	shrinkSetupTimers(t)
	shrinkSetupHeartbeat(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)
	s.Installing("ollama")
	before := len(d.noted())
	waitForCond(t, func() bool { return len(d.noted()) > before+1 }, "post-install heartbeats")

	for _, req := range d.noted()[before:] {
		if req.Phase != management.SetupExecutorPhaseInstalling || req.Engine != "ollama" {
			t.Fatalf("heartbeat after Installing = %+v, want phase=installing engine=ollama", req)
		}
	}
}

func TestExecutorSessionReportsUnelevated(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, false)
	t.Cleanup(s.Release)
	notes := d.noted()
	if len(notes) == 0 || notes[0].Elevated {
		t.Fatalf("first update = %+v, want elevated=false so the daemon reports permission_denied", notes)
	}
}

// TestAwaitSetupBudgetWaitsForTheClick is the core §9 regression: at
// LoginPhaseActive no desired frame has arrived, because the operator has
// not clicked anything yet. A one-shot check would read active=false and
// keep the legacy deadline — 3 minutes on an engine-less host — so the
// executor would be gone before the wizard's first write landed.
func TestAwaitSetupBudgetWaitsForTheClick(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	// The click lands a little after the wait starts.
	go func() {
		time.Sleep(30 * time.Millisecond)
		d.setActive(true)
	}()

	budget, active := awaitSetupBudget(s, 3*time.Second, io.Discard, nil)
	if !active || budget != setupResidencyBudget {
		t.Fatalf("budget=%v active=%v, want the residency budget once the setup went active", budget, active)
	}
}

func TestAwaitSetupBudgetFallsBackAfterGrace(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	budget, active := awaitSetupBudget(s, 30*time.Millisecond, io.Discard, nil)
	if active || budget != benchPollDeadline {
		t.Fatalf("budget=%v active=%v, want the legacy deadline when nobody started setup", budget, active)
	}
}

// TestAwaitSetupBudgetIgnoresLeftoverDesiredState is the #308 bar at this
// layer: a device that was set up once keeps its desired state on the map
// entry forever, so a later run reads Active on its very first probe.
// Taking the residency fast path there announced a browser setup that was
// not happening. The wait must instead sit out its grace and continue in
// the terminal.
func TestAwaitSetupBudgetIgnoresLeftoverDesiredState(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{state: management.SetupStateResponse{
		Active: true, DesiredStale: true, DesiredEngine: "ollama", EngineInstalled: true,
	}}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	var out strings.Builder
	budget, active := awaitSetupBudget(s, 30*time.Millisecond, &out, nil)
	if active || budget != benchPollDeadline {
		t.Fatalf("budget=%v active=%v, want the legacy path for leftover desired state", budget, active)
	}
	if !strings.Contains(out.String(), "No setup started in the browser") {
		t.Errorf("the wait did not fall back to the terminal: %q", out.String())
	}
	if strings.Contains(out.String(), takeoverClosedLine) {
		t.Errorf("announced a browser handoff with no browser driving: %q", out.String())
	}
}

// TestAwaitSetupBudgetTakenOver: confirming the takeover is how the
// operator takes the terminal back, and it must not wait out the grace.
func TestAwaitSetupBudgetTakenOver(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	enter := newTakeoverWatch(newStdinReader(strings.NewReader("\ny\n")))

	var out strings.Builder
	budget, active := awaitSetupBudget(s, time.Minute, &out, enter)
	if active || budget != benchPollDeadline {
		t.Fatalf("budget=%v active=%v, want the legacy deadline after a takeover", budget, active)
	}
	if !strings.Contains(out.String(), "Take over setup in this terminal?") {
		t.Errorf("the takeover was never confirmed out loud: %q", out.String())
	}
}

// A bare Enter must NOT take the terminal back: it asks, and the
// default answer keeps the browser driving (#184).
func TestAwaitSetupBudgetBareEnterKeepsBrowser(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{state: management.SetupStateResponse{Active: true}}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	enter := newTakeoverWatch(newStdinReader(strings.NewReader("\n\n")))
	budget, active := awaitSetupBudget(s, time.Minute, io.Discard, enter)
	if !active || budget != setupResidencyBudget {
		t.Fatalf("budget=%v active=%v, want the setup budget", budget, active)
	}
	if enter.Fired() {
		t.Error("a bare Enter took the terminal over")
	}
}

// TestAwaitBrowserSetupBackgroundsAfterTheGrace is the #309 contract: the
// grace expired with nothing driving, so this terminal IS the driver and
// there is nothing left to take over. Enter must mean what it means
// everywhere else a terminal owns a long download (waired#774) — put the
// wait in the background — and the terminal must say so, because until
// this line the same key meant "take setup back from the browser".
func TestAwaitBrowserSetupBackgroundsAfterTheGrace(t *testing.T) {
	shrinkSetupTimers(t)
	setupAwaitGrace = 30 * time.Millisecond
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	// An open pipe: the operator types after the grace has expired, which
	// is the whole point — before it, the offer on screen was a different
	// one.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	owner := newStdinReader(pr)

	var out strings.Builder
	budget, active, enter, _ := awaitBrowserSetup(s, owner, &out, false, false, false)
	if active || budget != benchPollDeadline {
		t.Fatalf("budget=%v active=%v, want the legacy path when nobody started setup", budget, active)
	}
	if !strings.Contains(out.String(), "press Enter anytime to continue in the background") {
		t.Errorf("the terminal never said what Enter does now: %q", out.String())
	}

	// A bare Enter now backgrounds the wait, with no question in between
	// and no note of its own — the wait narrates that.
	_, _ = pw.Write([]byte("\n"))
	took, note := pollWatch(t, enter)
	if !took {
		t.Fatal("Enter did not background the wait")
	}
	if note != "" {
		t.Errorf("Enter was answered with %q; the wait owns that line", note)
	}
}

// TestAwaitBrowserSetupKeepsAFiredTakeover: a takeover confirmed inside
// the grace already claimed the driver (awaitSetupBudget calls TakeOver),
// and runInitViaDaemon reads Fired() afterwards to keep the lease honest
// (waired-agent#198). That watch must survive the #309 swap.
func TestAwaitBrowserSetupKeepsAFiredTakeover(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	// Typed while the offer is on screen, not before it: awaitBrowserSetup
	// drops anything queued ahead of the offer (#184), and which of the
	// two wins is a race — one this test lost on Windows, where the
	// reader goroutine publishes before the discard runs.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	// Poll 1 is attachSetupExecutor's support probe, which still precedes
	// the discard; poll 2 is awaitSetupBudget's own first read, by which
	// point the offer is on screen and the watch is armed.
	d.onState = func(poll int) {
		if poll == 2 {
			_, _ = pw.Write([]byte("\ny\n"))
		}
	}

	var out strings.Builder
	_, active, enter, _ := awaitBrowserSetup(s, newStdinReader(pr), &out, false, false, false)
	if active {
		t.Fatal("a takeover left the run marked browser-driven")
	}
	if !enter.Fired() {
		t.Error("the confirmed takeover was lost when the watch was swapped")
	}
	if strings.Contains(out.String(), "continue in the background") {
		t.Errorf("the backgrounding offer was made to an operator who took the terminal: %q", out.String())
	}
}

// TestAwaitBrowserSetupSkipsNonInteractive keeps --non-interactive and
// --no-browser on the unchanged path: no lease-driven residency, no
// prompt suppression, no Enter listener.
func TestAwaitBrowserSetupSkipsNonInteractive(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{state: management.SetupStateResponse{Active: true}}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	// The auth-key row joins them in waired-agent#797: the key IS the
	// sign-in, so there is no browser session for any of this to be about.
	for _, tc := range []struct{ nonInteractive, noBrowser, authKeyRun bool }{
		{true, false, false},
		{false, true, false},
		{false, false, true},
	} {
		budget, active, enter, watch := awaitBrowserSetup(s, nil, io.Discard, tc.nonInteractive, tc.noBrowser, tc.authKeyRun)
		if active || budget != benchPollDeadline {
			t.Fatalf("nonInteractive=%v noBrowser=%v authKeyRun=%v: budget=%v active=%v, want the legacy path",
				tc.nonInteractive, tc.noBrowser, tc.authKeyRun, budget, active)
		}
		if took, note := enter.Poll(); took || note != "" {
			t.Fatalf("inert watch produced (%v, %q)", took, note)
		}
		// #308: these paths never hand the terminal to a browser, so the
		// setup watch stays inert too — this daemon reports Active, and a
		// watch that polled it would flip the run into browser-driven mode.
		if started, _, _ := watch.Poll(); started || watch.Started() {
			t.Fatalf("nonInteractive=%v noBrowser=%v authKeyRun=%v: setup watch reported a browser setup",
				tc.nonInteractive, tc.noBrowser, tc.authKeyRun)
		}
	}
}

func waitForCond(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- progress reporting (waired-agent#197) ---

// shrinkProgressThrottle removes the 500 ms throttle for tests that care
// about what is reported rather than how often.
func shrinkProgressThrottle(t *testing.T) {
	t.Helper()
	prev := executorProgressInterval
	executorProgressInterval = 0
	t.Cleanup(func() { executorProgressInterval = prev })
}

// progressReports filters a daemon's recorded traffic down to the reports
// that carry a step, which is all the progress assertions care about.
func progressReports(reqs []management.SetupExecutorRequest, step string) []management.SetupExecutorRequest {
	var out []management.SetupExecutorRequest
	for _, r := range reqs {
		if r.Step == step {
			out = append(out, r)
		}
	}
	return out
}

// The figures the terminal draws must reach the daemon, tagged with the
// row they belong to — that is the whole of #197 on this side.
func TestExecutorSessionProgressReportsBytes(t *testing.T) {
	shrinkSetupTimers(t)
	shrinkProgressThrottle(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	s.Installing("ollama")
	s.Progress(management.SetupStepEngineDownload, "ollama", 700<<20, 1400<<20, 76_281_364)

	got := progressReports(d.noted(), management.SetupStepEngineDownload)
	if len(got) == 0 {
		t.Fatal("no engine_download report reached the daemon")
	}
	last := got[len(got)-1]
	if last.CompletedBytes != 700<<20 || last.TotalBytes != 1400<<20 || last.RateBps != 76_281_364 {
		t.Errorf("report = %+v, want the installer's own figures", last)
	}
	if last.Phase != management.SetupExecutorPhaseInstalling {
		t.Errorf("phase = %q, want the install phase to survive a progress tick", last.Phase)
	}
}

// Moving to the next step closes the previous row. Without this the
// download sits at "running" in the wizard for the whole install that
// follows it, because nothing else ever says it finished.
func TestExecutorSessionProgressClosesThePreviousStep(t *testing.T) {
	shrinkSetupTimers(t)
	shrinkProgressThrottle(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	s.Installing("ollama")
	s.Progress(management.SetupStepEngineDownload, "ollama", 1400<<20, 1400<<20, 1000)
	s.Progress(management.SetupStepEngineInstall, "ollama", 0, 0, 0)

	var sawDownloadDone bool
	for _, r := range d.noted() {
		if r.Step == management.SetupStepEngineDownload && r.Phase == management.SetupExecutorPhaseDone {
			sawDownloadDone = true
		}
	}
	if !sawDownloadDone {
		t.Fatalf("engine_download was never reported done: %+v", d.noted())
	}
}

// The installer's callback fires far faster than either this IPC or the
// control plane's 1-push-per-2 s intake can use.
func TestExecutorSessionProgressIsThrottled(t *testing.T) {
	shrinkSetupTimers(t)
	prev := executorProgressInterval
	executorProgressInterval = time.Hour
	t.Cleanup(func() { executorProgressInterval = prev })
	// The heartbeat is held off by shrinkSetupTimers not shrinking it.
	// This test used to hold it off locally, and solving it here rather
	// than in the shared helper is why the same bet was still live one
	// function below (waired-agent#914).

	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()

	for i := 0; i < 50; i++ {
		s.Progress(management.SetupStepEngineDownload, "ollama", int64(i)<<20, 1400<<20, 1000)
	}
	if got := len(progressReports(d.noted(), management.SetupStepEngineDownload)); got != 1 {
		t.Fatalf("%d download reports for 50 ticks, want 1 — the throttle is not holding", got)
	}
}

// An inert session (no daemon, or one predating the executor routes) must
// stay silent rather than panic or block the install.
func TestExecutorSessionProgressInertWhenUnsupported(t *testing.T) {
	d := &fakeSetupDaemon{notFound: true}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	s.Progress(management.SetupStepEngineDownload, "ollama", 1, 2, 3)
	if len(d.noted()) != 0 {
		t.Fatalf("inert session posted %+v", d.noted())
	}
	if newExecutorProgressSink(s, "ollama") != nil {
		t.Error("sink for an inert session should be nil so the installer takes its no-callback path")
	}
}

// The sink is the seam between the installer's vocabulary and the §7
// rows: transfer stages become the download row with figures, everything
// else becomes the install row.
func TestExecutorProgressSinkMapsInstallerStages(t *testing.T) {
	shrinkSetupTimers(t)
	shrinkProgressThrottle(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()

	sink := newExecutorProgressSink(s, "ollama")
	if sink == nil {
		t.Fatal("want a live sink for a supported session")
	}
	// The stage-opening event carries the URL and no bytes; reporting it
	// would open the row with a bar of unknown size that the next event
	// immediately replaces.
	sink(infruntime.OllamaInstallProgress{Stage: "download", Message: "https://example.invalid/x.tgz"})
	if got := progressReports(d.noted(), management.SetupStepEngineDownload); len(got) != 0 {
		t.Fatalf("the byte-less stage opener was reported: %+v", got)
	}

	sink(infruntime.OllamaInstallProgress{Stage: "download", Completed: 5, Total: 10, BytesPerSec: -1})
	got := progressReports(d.noted(), management.SetupStepEngineDownload)
	if len(got) != 1 {
		t.Fatalf("%d download reports, want 1", len(got))
	}
	if got[0].RateBps != 0 {
		t.Errorf("rate_bps = %d, want the renderer's -1 flattened to 0", got[0].RateBps)
	}

	sink(infruntime.OllamaInstallProgress{Stage: "extract", Message: "/var/lib/waired"})
	if len(progressReports(d.noted(), management.SetupStepEngineInstall)) == 0 {
		t.Fatalf("a non-transfer stage did not become the install row: %+v", d.noted())
	}
}

// --- driver + the integration step (waired-agent#198, waired#935) ---

// The lease says who is driving so the wizard can tell a deliberate
// handoff from a crash. Before this, taking over released the lease and
// both looked like `executor_gone`.
func TestExecutorSessionTakeOverClaimsTheTerminalWithoutReleasing(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	s.TakeOver()

	notes := d.noted()
	last := notes[len(notes)-1]
	if last.Driver != signer.SetupDriverTerminal {
		t.Errorf("driver = %q, want terminal", last.Driver)
	}
	if !last.Attached {
		t.Error("TakeOver released the lease; the wizard would report executor_gone for a deliberate handoff")
	}

	// And it keeps saying so: the daemon's claim is lease-bound, so a
	// heartbeat that dropped the driver would let it lapse mid-run.
	s.post(true, management.SetupExecutorPhaseIdle, "", "")
	notes = d.noted()
	if got := notes[len(notes)-1].Driver; got != signer.SetupDriverTerminal {
		t.Errorf("driver = %q on a later post, want it repeated", got)
	}
}

// A terminal phase belongs to the step it is about. Reporting the
// integration through Done/Failed would set the SESSION phase, which the
// heartbeat repeats and the daemon reads as the engine install's state.
func TestExecutorSessionStepPhasesDoNotLeakAcrossSteps(t *testing.T) {
	shrinkSetupTimers(t)
	shrinkProgressThrottle(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	s.Installing("ollama")
	s.Done("ollama")

	// Now the integration starts. The engine is done; this must not be
	// reported as done before it has begun.
	s.Progress(management.SetupStepIntegration, "", 0, 0, 0)
	for _, r := range progressReports(d.noted(), management.SetupStepIntegration) {
		if r.Phase == management.SetupExecutorPhaseDone {
			t.Fatalf("the engine's done phase leaked onto the integration row: %+v", r)
		}
	}

	s.DoneStep(management.SetupStepIntegration)
	notes := d.noted()
	last := notes[len(notes)-1]
	if last.Step != management.SetupStepIntegration || last.Phase != management.SetupExecutorPhaseDone {
		t.Fatalf("final report = %+v, want integration/done", last)
	}
	if last.Engine != "" {
		t.Errorf("engine = %q on an integration report, want none", last.Engine)
	}
}

// The subset form of the per-user hop. Flags must precede the target:
// stdlib flag parsing stops at the first non-flag argument, so anything
// after it is silently ignored.
func TestLinkOneChildArgs(t *testing.T) {
	got := linkOneChildArgs("http://127.0.0.1:9473", "claude-code")
	want := []string{"link", "--force", "--no-prompt", "--gateway-base-url", "http://127.0.0.1:9473", "claude-code"}
	if len(got) != len(want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %q, want %q", got, want)
		}
	}
}

// TestRunWizardIntegrationsReportsWhetherThereWasAnInstruction pins the
// return value, which is what makes waired-agent#311's two call sites safe.
//
// The coding tools now run between the engine install and the model
// download, but two things can put the instruction out of reach at that
// moment — a browser setup that only commits during the download, and a
// wizard that writes its engine and model a beat before its coding-tool
// answer — so the old site after the wait stays as the catch-up. `false`
// means "nothing was asked for, the later site may still try"; `true` means
// "this run has handled it, do not repeat it".
//
// Product contract. Getting it backwards either writes the tools twice or
// leaves a confirmed instruction unapplied for the whole run.
func TestRunWizardIntegrationsReportsWhetherThereWasAnInstruction(t *testing.T) {
	shrinkSetupTimers(t)
	none := []string{}
	for _, tc := range []struct {
		name    string
		apply   bool
		targets *[]string
		want    bool
	}{
		// The caller said not to write: a terminal run that asks its own
		// question later, or a re-auth over an instruction this device has
		// already written (waired-agent#987).
		{"caller says do not apply", false, &[]string{"claude-code"}, false},
		// An older control plane, or a wizard that has not asked yet. The
		// later site gets another go.
		{"no instruction yet", true, nil, false},
		// "Asked, and every toggle was off" IS an answer. Nothing is
		// written, but the question is settled and must not be re-asked.
		{"asked, all toggles off", true, &none, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeSetupDaemon{}
			d.setState(management.SetupStateResponse{Active: true, Integrations: tc.targets})
			srv := d.server(t)
			s := attachSetupExecutor(srv.URL, true)
			defer s.Release()

			if got := runWizardIntegrations(s, tc.apply, setupIntegrationOpts{
				GatewayBaseURL: "http://127.0.0.1:9473",
			}); got != tc.want {
				t.Fatalf("runWizardIntegrations = %v, want %v", got, tc.want)
			}
			for _, r := range d.noted() {
				if r.Step == management.SetupStepIntegration {
					t.Fatalf("an integration outcome was reported with nothing to write: %+v", r)
				}
			}
		})
	}
}

// Nothing to write must stay nothing to write. Both "no instruction" and
// "asked, every toggle off" leave the machine untouched — the difference
// between them is reported by the daemon, not acted on here.
func TestRunSetupIntegrationsSkipsWhenThereIsNothingToWrite(t *testing.T) {
	shrinkSetupTimers(t)
	none := []string{}
	for _, tc := range []struct {
		name    string
		targets *[]string
	}{
		{"no instruction", nil},
		{"asked, all toggles off", &none},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeSetupDaemon{}
			d.setState(management.SetupStateResponse{Active: true, Integrations: tc.targets})
			srv := d.server(t)
			s := attachSetupExecutor(srv.URL, true)
			defer s.Release()

			if err := runSetupIntegrations(s, io.Discard, io.Discard, setupIntegrationOpts{
				GatewayBaseURL: "http://127.0.0.1:9473",
			}); err != nil {
				t.Fatalf("runSetupIntegrations: %v", err)
			}
			for _, r := range d.noted() {
				if r.Step == management.SetupStepIntegration {
					t.Fatalf("an integration outcome was reported with nothing to write: %+v", r)
				}
			}
		})
	}
}

// waired-agent#797. Which runs own setup from the first frame, as a table
// — the branch used to read the two flags and miss the third way a run can
// have no browser in it.
func TestTerminalDrivenFromTheStart(t *testing.T) {
	tests := []struct {
		name                                  string
		nonInteractive, noBrowser, authKeyRun bool
		want                                  bool
	}{
		{"an ordinary interactive sign-in", false, false, false, false},
		{"non-interactive", true, false, false, true},
		{"no-browser", false, true, false, true},
		{"an auth key", false, false, true, true},
		{"an auth key without a terminal", true, false, true, true},
		{"an auth key with the browser suppressed", false, true, true, true},
		{"both flags", true, true, false, true},
		{"all three", true, true, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalDrivenFromTheStart(tc.nonInteractive, tc.noBrowser, tc.authKeyRun); got != tc.want {
				t.Errorf("terminalDrivenFromTheStart(%v, %v, %v) = %v, want %v",
					tc.nonInteractive, tc.noBrowser, tc.authKeyRun, got, tc.want)
			}
		})
	}
}

// The copy that used to be addressed to a browser nobody had opened
// (waired-agent#797), and the wait that followed it. Verbatim, because
// what was wrong with them was that they were printed at all.
func TestAwaitBrowserSetupSaysNothingAboutABrowserOnAnAuthKeyRun(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	var out bytes.Buffer
	started := time.Now()
	awaitBrowserSetup(s, nil, &out, false, false, true)
	elapsed := time.Since(started)

	for _, line := range []string{
		"Setup is continuing in your browser...",
		setupKeepTerminalOpenLine,
		"No setup started in the browser; continuing here.",
	} {
		if strings.Contains(out.String(), line) {
			t.Errorf("printed %q on a run with no browser session", line)
		}
	}
	// It returns rather than sitting out the grace. shrinkSetupTimers puts
	// that grace in the low seconds, so anything near it is the old wait.
	if elapsed >= setupAwaitGrace {
		t.Errorf("waited %v for a browser that cannot exist (grace is %v)", elapsed, setupAwaitGrace)
	}
}
