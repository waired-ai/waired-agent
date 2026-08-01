package main

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// scriptedState is the seam the setup watch is built on: the states the
// daemon reports, one per read, the last repeating. It records the reads
// so a test can pin the throttle rather than the wall clock.
type scriptedState struct {
	states []management.SetupStateResponse
	reads  int
}

func (s *scriptedState) next() management.SetupStateResponse {
	st := s.states[min(s.reads, len(s.states)-1)]
	s.reads++
	return st
}

func activeState() management.SetupStateResponse {
	return management.SetupStateResponse{Active: true, DesiredEngine: "ollama", EngineInstalled: true}
}

// newScriptedWatch arms a watch over the scripted states with the throttle
// wound down, so a test drives ticks rather than seconds.
func newScriptedWatch(t *testing.T, s *scriptedState) *setupWatch {
	t.Helper()
	w := &setupWatch{state: s.next}
	w.every = time.Nanosecond
	return w
}

// TestSetupWatchReportsTheEdgeOnce is the #308 contract: a browser setup
// that starts AFTER awaitSetupBudget's grace expired must be reported —
// once, so the terminal narrates the handoff a single time.
func TestSetupWatchReportsTheEdgeOnce(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{{}, {}, activeState()}}
	w := newScriptedWatch(t, s)

	for i := 0; i < 2; i++ {
		if started, _, _ := w.Poll(); started {
			t.Fatalf("watch reported a setup start on read %d, before one existed", i)
		}
		if w.Started() {
			t.Fatalf("Started latched on read %d, before one existed", i)
		}
	}
	started, budget, engineComing := w.Poll()
	if !started {
		t.Fatal("watch missed the browser setup starting")
	}
	if budget != setupResidencyBudget {
		t.Errorf("budget = %v, want the residency budget %v", budget, setupResidencyBudget)
	}
	if engineComing {
		t.Error("engineComing is true for a state whose engine is already installed")
	}
	if !w.Started() {
		t.Error("Started did not latch after the edge")
	}
	// Latched: the caller has already closed the takeover offer and moved
	// to the residency budget, and saying it twice would print the
	// handoff line again.
	if again, _, _ := w.Poll(); again {
		t.Error("watch reported the same setup start twice")
	}
	if reads := s.reads; reads != 3 {
		t.Errorf("watch kept polling the daemon after the edge (%d reads, want 3)", reads)
	}
}

// TestSetupWatchReportsEngineArrival pins that the edge carries the same
// engineComing the caller would have computed itself, from the same read.
func TestSetupWatchReportsEngineArrival(t *testing.T) {
	cases := []struct {
		name string
		st   management.SetupStateResponse
		want bool
	}{
		{"wizard has not picked an engine yet",
			management.SetupStateResponse{Active: true}, true},
		{"an install claim is live",
			management.SetupStateResponse{Active: true, DesiredEngine: "ollama", EngineInstalled: true, InstallClaimed: "other"}, true},
		{"desired engine is not in place",
			management.SetupStateResponse{Active: true, DesiredEngine: "ollama"}, true},
		{"engine already installed",
			management.SetupStateResponse{Active: true, DesiredEngine: "ollama", EngineInstalled: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedState{states: []management.SetupStateResponse{tc.st}}
			w := newScriptedWatch(t, s)
			started, _, engineComing := w.Poll()
			if !started {
				t.Fatal("watch missed the browser setup starting")
			}
			if engineComing != tc.want {
				t.Errorf("engineComing = %v, want %v (engineArrivalPending)", engineComing, tc.want)
			}
			if engineComing != engineArrivalPending(tc.st) {
				t.Errorf("engineComing disagrees with engineArrivalPending(%+v)", tc.st)
			}
		})
	}
}

// TestSetupWatchThrottlesTheDaemon: the wait it lives in ticks once a
// second, and every read is a loopback HTTP round trip.
func TestSetupWatchThrottlesTheDaemon(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{{}}}
	w := &setupWatch{state: s.next, every: time.Hour}
	for i := 0; i < 10; i++ {
		w.Poll()
	}
	if s.reads != 1 {
		t.Errorf("watch read the daemon %d times in one throttle window, want 1", s.reads)
	}
}

// TestSetupWatchInertShapes pins that every path which is byte-identical
// today stays byte-identical, and issues no /setup/state request at all:
// a nil watch, an older daemon (unsupported session), and a setup that was
// already driving before the wait began.
func TestSetupWatchInertShapes(t *testing.T) {
	var nilWatch *setupWatch
	if started, _, _ := nilWatch.Poll(); started {
		t.Error("a nil watch reported a setup start")
	}
	if nilWatch.Started() {
		t.Error("a nil watch latched Started")
	}

	s := &scriptedState{states: []management.SetupStateResponse{activeState()}}
	// alreadyActive: awaitSetupBudget saw the setup within its grace, so
	// the caller is already in browser-driven mode and there is no edge
	// left to observe.
	w := &setupWatch{state: s.next, alreadyActive: true}
	if started, _, _ := w.Poll(); started {
		t.Error("watch reported an edge for a setup that was already active")
	}
	if s.reads != 0 {
		t.Errorf("an inert watch polled the daemon %d times", s.reads)
	}

	// An unsupported session (a daemon older than the executor routes)
	// yields the same inert watch.
	if w := newSetupWatch(nil, false); w.state != nil {
		t.Error("newSetupWatch armed a watch over an unsupported session")
	}
	if started, _, _ := newSetupWatch(nil, false).Poll(); started {
		t.Error("an unsupported session reported a setup start")
	}
}
