package main

import (
	"bufio"
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
	if !waitForBundledModel(srv.URL, &out, false /*tty*/, benchPollDeadline, false, nil) {
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
	if waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil) {
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
		ready = waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil)
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
	if !waitForBundledModel(srv.URL, &out, false /*tty*/, benchPollDeadline, true /*setupActive*/, nil) {
		t.Fatalf("setup-active wait gave up on no_engine; out=%q", out.String())
	}
	if strings.Contains(out.String(), "AI engine still isn't up") {
		t.Errorf("setup-active wait printed the give-up notice: %q", out.String())
	}
}

// Enter backgrounds the wait so the operator can take the terminal back.
func TestWaitForBundledModel_BackgroundedByEnter(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{{SubsystemState: "no_engine"}}}
	srv := stub.server()
	defer srv.Close()

	enter := listenForEnter(bufio.NewScanner(strings.NewReader("\n")))
	waitForCond(t, enter.Backgrounded, "the Enter line to be read")

	var out strings.Builder
	if waitForBundledModel(srv.URL, &out, false, benchPollDeadline, true, enter) {
		t.Fatal("a backgrounded wait must return false")
	}
	if !strings.Contains(out.String(), "Continuing in the background") {
		t.Errorf("expected the background note, got: %q", out.String())
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
	if !waitForBundledModel(srv.URL, &out, false /*tty*/, benchPollDeadline, false, nil) {
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
