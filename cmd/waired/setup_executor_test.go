package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
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
	setupStatePollInterval = 5 * time.Millisecond
	setupExecutorHeartbeatInterval = 5 * time.Millisecond
	setupResidencyBudget = 42 * time.Minute // distinguishable from benchPollDeadline
	t.Cleanup(func() {
		setupStatePollInterval, setupExecutorHeartbeatInterval, setupResidencyBudget = prevPoll, prevBeat, prevResidency
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
	s.Failed("ollama", "boom")
	s.Release()
	if got := len(d.noted()); got != 0 {
		t.Fatalf("posted %d lease updates to an older daemon, want 0", got)
	}

	budget, active, enter := awaitBrowserSetup(s, bufio.NewScanner(strings.NewReader("")), io.Discard, false, false)
	if budget != benchPollDeadline || active {
		t.Fatalf("budget=%v active=%v, want the legacy deadline and no setup", budget, active)
	}
	enter.Drain(io.Discard) // must not block
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

// TestAwaitSetupBudgetBackgroundedByEnter: pressing Enter is how the
// operator takes the terminal back, and it must not wait out the grace.
func TestAwaitSetupBudgetBackgroundedByEnter(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	enter := listenForEnter(bufio.NewScanner(strings.NewReader("\n")))
	waitForCond(t, enter.Backgrounded, "the Enter line to be read")

	budget, active := awaitSetupBudget(s, time.Minute, io.Discard, enter)
	if active || budget != benchPollDeadline {
		t.Fatalf("budget=%v active=%v, want the legacy deadline after Enter", budget, active)
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
		budget, active, enter := awaitBrowserSetup(s, bufio.NewScanner(strings.NewReader("")), io.Discard, tc.nonInteractive, tc.noBrowser)
		if active || budget != benchPollDeadline {
			t.Fatalf("nonInteractive=%v noBrowser=%v: budget=%v active=%v, want the legacy path",
				tc.nonInteractive, tc.noBrowser, budget, active)
		}
		enter.Drain(io.Discard) // must not block
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
