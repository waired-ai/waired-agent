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

// waired-agent#806: the probe reports the EDGE of this node's own
// reachability, not its value.
//
// The value already travels — it is written to the state file every tick
// and pushed to the control plane, where it feeds the Public Share
// eligibility check. What no consumer could see was the TRANSITION, and
// the transition is the only thing that tells a listener a condition it
// was waiting out has resolved.

// A node whose engine is up reports exactly one edge, however many ticks
// run. Anything else and the acquirer would be woken on a heartbeat.
func TestRunLocalInferenceProbe_ReportsOneReachabilityEdge(t *testing.T) {
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

	var edges, ticks atomic.Int32
	probeRunUntil(t, inferenceProbeDeps{
		StateWriter:      w,
		EngineTarget:     staticEngineTarget(signer.InferenceTypeOllama, port),
		Logger:           slog.Default(),
		OnLocalReachable: func() { edges.Add(1) },
		Interval:         5 * time.Millisecond,
		// Counted on the same path the edge is reported from, so "several
		// ticks ran" is observed rather than assumed. Without it a single
		// edge would also be the answer for a probe that ticked once.
		Hardware: func() *signer.HardwareSummary { ticks.Add(1); return nil },
	}, "report a reachability edge over several ticks", func() bool {
		return edges.Load() > 0 && ticks.Load() >= 3
	})

	if got := edges.Load(); got != 1 {
		t.Errorf("reachability edges = %d over %d ticks, want exactly 1 — this is an edge, "+
			"and a listener woken every heartbeat is a listener that learns nothing",
			got, ticks.Load())
	}
}

// A node whose engine never answers reports no edge at all. The signal
// must not fire merely because the loop is running.
func TestRunLocalInferenceProbe_UnreachableEngineReportsNoEdge(t *testing.T) {
	// A server that is closed immediately: the port answers nothing.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	port, err := portFromURL(srv.URL)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	srv.Close()

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
		OnLocalReachable: func() { edges.Add(1) },
		Interval:         5 * time.Millisecond,
		Hardware:         func() *signer.HardwareSummary { ticks.Add(1); return nil },
	}, "run several ticks against a dead engine", func() bool {
		return ticks.Load() >= 3
	})

	if got := edges.Load(); got != 0 {
		t.Errorf("reachability edges = %d against an engine that never answered, want 0", got)
	}
}

// The hook is optional. Every path except the public-grant acquirer
// leaves it nil, and a nil hook must not be a nil dereference on the
// probe tick.
func TestRunLocalInferenceProbe_NilReachabilityHookIsInert(t *testing.T) {
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
	}, "record the engine as reachable with no hook wired", func() bool {
		got, err := state.Read(dir)
		return err == nil && got.InferenceReachableLocal
	})
}
