package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The EngineDead seam in runLocalInferenceProbe's tick had no test at all before
// waired-agent#1138 — `grep EngineDead cmd/waired-agent/*_test.go` was empty —
// which is part of why a predicate that could not see the give-up latch went
// unnoticed through the whole #1069 sweep.

// TestRunLocalInferenceProbe_EngineDeadOverridesALiveProbe: the server answers
// 200 for the whole run, and that is not a contrivance — it is the ONLY shape
// in which the defect is reachable. Where waired spawned the engine, a stop
// kills the child and the HTTP probe fails on its own. The states this
// predicate exists for are the ones where something ELSE holds the port: an
// adopted orphan Stop() never killed, or a foreign engine on the waired-owned
// port (#943). The probe gets its 200 and cannot tell the difference.
func TestRunLocalInferenceProbe_EngineDeadOverridesALiveProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3.1:8b"}]}`)
	}))
	defer srv.Close()

	port, err := portFromURL(srv.URL)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	dir := t.TempDir()
	w := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
	if err := w.Set(w.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var edges, ticks atomic.Int32
	probeRunUntil(t, inferenceProbeDeps{
		StateWriter:      w,
		EngineTarget:     staticEngineTarget(signer.InferenceTypeOllama, port),
		Logger:           slog.Default(),
		EngineDead:       func() bool { return true },
		OnLocalReachable: func() { edges.Add(1) },
		Interval:         5 * time.Millisecond,
		// Counted so "several ticks ran against a live server" is observed
		// rather than assumed: an unreachable verdict from a probe that never
		// ticked would prove nothing.
		Hardware: func() *signer.HardwareSummary { ticks.Add(1); return nil },
	}, "tick several times against a live engine it has been told is dead", func() bool {
		return ticks.Load() >= 3
	})

	got, err := state.Read(dir)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if got.InferenceReachableLocal {
		t.Error("a 200 from something on the port must not make a dead engine reachable: " +
			"this is what keeps a given-up host advertising capacity to peers")
	}
	if n := edges.Load(); n != 0 {
		t.Errorf("reachability edges = %d, want 0 — a dead engine is not an arrival", n)
	}
}

// The other half of the same seam: fail-open. A nil predicate means "not
// sure", and not-sure keeps the probe's own verdict — the mesh is not the
// place to guess a host offline.
func TestRunLocalInferenceProbe_NilEngineDeadKeepsTheProbeVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	defer srv.Close()

	port, err := portFromURL(srv.URL)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	dir := t.TempDir()
	w := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
	if err := w.Set(w.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	probeRunUntil(t, inferenceProbeDeps{
		StateWriter:  w,
		EngineTarget: staticEngineTarget(signer.InferenceTypeOllama, port),
		Logger:       slog.Default(),
		EngineDead:   nil,
	}, "keep the probe's own verdict with no predicate wired", func() bool {
		got, err := state.Read(dir)
		return err == nil && got.InferenceReachableLocal
	})
}
