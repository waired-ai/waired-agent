package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func TestRunPause_FallsBackToDesiredPhaseWhenDaemonUnreachable(t *testing.T) {
	dir := t.TempDir()
	// Pick a TCP address that is guaranteed unused — bind a listener,
	// grab the port, then close so the dial reliably refuses. Avoids
	// flakes from port 1 being filtered by the OS network stack.
	ln, err := newClosedTCPAddr()
	if err != nil {
		t.Fatal(err)
	}

	if err := runPause([]string{"--mgmt", "http://" + ln, "--state-dir", dir}); err != nil {
		t.Fatalf("runPause: %v", err)
	}

	got, err := state.ReadDesiredPhase(dir)
	if err != nil {
		t.Fatalf("ReadDesiredPhase: %v", err)
	}
	if got != state.PhasePaused {
		t.Errorf("desired-phase = %q, want %q", got, state.PhasePaused)
	}
}

func TestRunResume_FallsBackToDesiredPhaseWhenDaemonUnreachable(t *testing.T) {
	dir := t.TempDir()
	if err := state.WriteDesiredPhase(dir, state.PhasePaused); err != nil {
		t.Fatal(err)
	}
	ln, err := newClosedTCPAddr()
	if err != nil {
		t.Fatal(err)
	}
	if err := runResume([]string{"--mgmt", "http://" + ln, "--state-dir", dir}); err != nil {
		t.Fatalf("runResume: %v", err)
	}
	got, _ := state.ReadDesiredPhase(dir)
	if got != state.PhaseActive {
		t.Errorf("desired-phase = %q, want %q", got, state.PhaseActive)
	}
}

func TestRunPause_HitsDaemonWhenReachable(t *testing.T) {
	dir := t.TempDir()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/pause" || r.Method != http.MethodPost {
			http.Error(w, "wrong route", http.StatusBadRequest)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"phase":"paused","desired_phase":"paused"}`))
	}))
	defer srv.Close()

	// Capture stdout so the prettyPrint output doesn't pollute test output.
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	if err := runPause([]string{"--mgmt", srv.URL, "--state-dir", dir}); err != nil {
		t.Fatalf("runPause: %v", err)
	}
	w.Close()
	io_ := readAll(t, r)
	_ = io_

	if calls != 1 {
		t.Errorf("expected 1 daemon call, got %d", calls)
	}
}
