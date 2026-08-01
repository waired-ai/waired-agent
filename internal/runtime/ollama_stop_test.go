package runtime

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// --- #316: a stop that starts must always end with a dead process ---
//
// These pin PRODUCT CONTRACTS, not today's behaviour:
//
//   - once stopProcess has signalled the child, no caller-side cancellation
//     may abandon it alive (the "commit to kill" rule), and
//   - a platform that cannot deliver signals must escalate to Kill at once
//     instead of burning StopTimeout waiting for a grace period the child
//     can never honour, and
//   - the park latch must never outlive a failed stop, or engine_power
//     reports "stopped" while the engine still pins VRAM.

// stopTestAdapter brings an adapter to StateReady with the supplied
// process, so each test below only has to describe how the stop fails.
func stopTestAdapter(t *testing.T, proc *fakeProcess, stopTimeout time.Duration) (*OllamaAdapter, *httptest.Server) {
	t.Helper()
	srv := okHealthServer(t)
	host, port := splitHostPort(t, srv.URL)
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: &fakeSpawner{process: proc}, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: stopTimeout,
		LogDir:      t.TempDir(),
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		srv.Close()
		t.Fatalf("EnsureRunning: %v", err)
	}
	return a, srv
}

// TestOllamaAdapter_Stop_KillsWhenCallerCancels is the core #316
// regression: the tray's 3s budget cancels the request context long
// before the 5s StopTimeout elapses. Before the fix, stopProcess returned
// ctx.Err() without ever calling Kill, leaving llama-server resident while
// Park had already latched parked=true.
func TestOllamaAdapter_Stop_KillsWhenCallerCancels(t *testing.T) {
	proc := newFakeProcess()
	// The child ignores SIGTERM, exactly like ollama on Windows.
	a, srv := stopTestAdapter(t, proc, 5*time.Second)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := a.Stop(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Stop after caller cancellation = %v, want nil (the memory WAS freed)", err)
	}
	if !proc.wasKilled() {
		t.Fatalf("caller cancellation abandoned the child alive; Kill was never called")
	}
	if elapsed >= 5*time.Second {
		t.Errorf("Stop waited out StopTimeout (%s); it should escalate as soon as the caller gives up", elapsed)
	}
	if h := a.Health(context.Background()); h.State != StateStopped {
		t.Errorf("state after cancelled stop = %s, want %s", h.State, StateStopped)
	}
	if engineLogOpen(a) {
		t.Errorf("engine log handle leaked on the cancellation path")
	}
}

// TestOllamaAdapter_Stop_ImmediateKillWhenSignalsUnsupported pins the
// Windows path without a windows build tag: the spawner reports
// ErrSignalUnsupported and the adapter must not spend StopTimeout waiting
// for a signal that was never delivered. Expressed as a value at the
// RunningProcess seam so it runs on every CI leg.
func TestOllamaAdapter_Stop_ImmediateKillWhenSignalsUnsupported(t *testing.T) {
	proc := newFakeProcess()
	proc.signalErr = ErrSignalUnsupported
	a, srv := stopTestAdapter(t, proc, 30*time.Second)
	defer srv.Close()

	start := time.Now()
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("Stop took %s; a signal-less platform must escalate to Kill immediately", elapsed)
	}
	if !proc.wasKilled() {
		t.Fatalf("Kill was never called")
	}
	// The adapter still asks — the spawner is what knows the platform
	// cannot deliver, and it must be asked to report that.
	if sigs := proc.sentSignals(); len(sigs) != 1 || !sentSIGTERM(sigs) {
		t.Errorf("signals = %v, want exactly one SIGTERM attempt", sigs)
	}
	if h := a.Health(context.Background()); h.State != StateStopped {
		t.Errorf("state after stop = %s, want %s", h.State, StateStopped)
	}
}

// TestOllamaAdapter_Park_ClearsLatchWhenStopFails: Park latches parked
// BEFORE stopping, and engine_power is derived from that latch. If the
// stop could not free the memory, the latch must come off — otherwise
// status claims "stopped" for a live engine and EnsureRunning refuses to
// revive it (both halves of the #316 symptom).
func TestOllamaAdapter_Park_ClearsLatchWhenStopFails(t *testing.T) {
	proc := newFakeProcess()
	proc.killErr = errors.New("access denied")
	a, srv := stopTestAdapter(t, proc, 20*time.Millisecond)
	defer srv.Close()

	if err := a.Park(context.Background()); err == nil {
		t.Fatalf("Park = nil, want the kill failure to propagate")
	}
	if a.IsParked() {
		t.Errorf("park latch survived a failed stop: engine_power would report 'stopped' for a live engine")
	}
	if engineLogOpen(a) {
		t.Errorf("engine log handle leaked on the kill-failure path")
	}
}

// TestOllamaAdapter_Stop_BoundedReapAfterKill: the pre-#316 code waited on
// proc.Done() forever after Kill. A child the OS refuses to reap must
// surface as an error instead of hanging the management handler (and, on
// the Quit path, the tray) indefinitely.
func TestOllamaAdapter_Stop_BoundedReapAfterKill(t *testing.T) {
	proc := newFakeProcess()
	proc.signalErr = ErrSignalUnsupported // escalate immediately
	proc.killNoExit = true                // ... but the child never dies
	const stopTimeout = 50 * time.Millisecond
	a, srv := stopTestAdapter(t, proc, stopTimeout)
	defer srv.Close()

	start := time.Now()
	err := a.Stop(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Stop = nil, want an error when the child cannot be reaped")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Stop blocked for %s; the post-Kill reap must be bounded by StopTimeout (%s)", elapsed, stopTimeout)
	}
	if engineLogOpen(a) {
		t.Errorf("engine log handle leaked on the unreapable-child path")
	}
}

// engineLogOpen reports whether the adapter still holds an engine.log
// handle, read under the adapter lock so -race stays quiet.
func engineLogOpen(a *OllamaAdapter) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.logFile != nil
}
