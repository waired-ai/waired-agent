package main

import (
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
}

func (d *fakeSetupDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/setup/state", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.notFound {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(d.state)
	})
	mux.HandleFunc("/waired/v1/setup/executor", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.notFound {
			http.NotFound(w, r)
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

func (d *fakeSetupDaemon) noted() []management.SetupExecutorRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]management.SetupExecutorRequest(nil), d.requests...)
}

// shrinkSetupTimers keeps these tests fast without changing what they
// assert; the production values live in setup_executor.go.
func shrinkSetupTimers(t *testing.T) {
	t.Helper()
	prevPoll, prevBeat, prevResidency := setupStatePollInterval, setupExecutorHeartbeatInterval, setupResidencyBudget
	prevGrace := setupAwaitGrace
	setupStatePollInterval = 5 * time.Millisecond
	setupExecutorHeartbeatInterval = 5 * time.Millisecond
	setupResidencyBudget = 42 * time.Minute // distinguishable from benchPollDeadline
	// Long enough for a scripted keystroke to arrive, short enough that a
	// test which never sends one fails in seconds instead of minutes.
	setupAwaitGrace = 2 * time.Second
	t.Cleanup(func() {
		setupStatePollInterval, setupExecutorHeartbeatInterval, setupResidencyBudget = prevPoll, prevBeat, prevResidency
		setupAwaitGrace = prevGrace
	})
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

	budget, active, enter, watch := awaitBrowserSetup(s, nil, io.Discard, false, false)
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
}

func TestExecutorSessionAttachHeartbeatRelease(t *testing.T) {
	shrinkSetupTimers(t)
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
	budget, active, enter, _ := awaitBrowserSetup(s, owner, &out, false, false)
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

	var out strings.Builder
	_, active, enter, _ := awaitBrowserSetup(s, newStdinReader(strings.NewReader("\ny\n")), &out, false, false)
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

	for _, tc := range []struct{ nonInteractive, noBrowser bool }{
		{true, false},
		{false, true},
	} {
		budget, active, enter, watch := awaitBrowserSetup(s, nil, io.Discard, tc.nonInteractive, tc.noBrowser)
		if active || budget != benchPollDeadline {
			t.Fatalf("nonInteractive=%v noBrowser=%v: budget=%v active=%v, want the legacy path",
				tc.nonInteractive, tc.noBrowser, budget, active)
		}
		if took, note := enter.Poll(); took || note != "" {
			t.Fatalf("inert watch produced (%v, %q)", took, note)
		}
		// #308: these paths never hand the terminal to a browser, so the
		// setup watch stays inert too — this daemon reports Active, and a
		// watch that polled it would flip the run into browser-driven mode.
		if started, _, _ := watch.Poll(); started || watch.Started() {
			t.Fatalf("nonInteractive=%v noBrowser=%v: setup watch reported a browser setup",
				tc.nonInteractive, tc.noBrowser)
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
