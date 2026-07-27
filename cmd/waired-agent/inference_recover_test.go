package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// recoverProvider builds the minimum provider onEngineUnhealthy needs: a real
// adapter (so Borrowed/Mode/IsParked/LatchFailed behave) and an injectable
// clock for the stability window.
func recoverProvider(t *testing.T, a *infruntime.OllamaAdapter, now func() time.Time) *agentInferenceProvider {
	t.Helper()
	// A real store: the recovery reconcile reads it, and a fake that returns
	// nothing would make the failing case unwritable.
	return &agentInferenceProvider{
		ollama:   a,
		store:    catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		logger:   slog.New(slog.DiscardHandler),
		agentCtx: context.Background(),
		now:      now,
	}
}

// TestOnEngineUnhealthy_SchedulesRecovery pins that the first death schedules
// a reconcile immediately. PRODUCT CONTRACT: the first attempt has no backoff
// because a human is typically waiting at a coding-agent prompt.
func TestOnEngineUnhealthy_SchedulesRecovery(t *testing.T) {
	a := newTestAdapter(t, false)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p := recoverProvider(t, a, nil)

	// Claim the in-flight latch first, so requestEngineReconcile coalesces
	// into "the running reconcile will observe the new intent" and returns
	// without draining engineRecoverPending. That makes the intent
	// observable instead of racing the reconcile goroutine — and it also
	// pins the coalescing itself.
	p.engineReconcileInFlight.Store(true)

	p.onEngineUnhealthy("engine returned HTTP 500: llama-server process has terminated")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.engineRecoverPending.Load() {
			return // recovery was requested
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Error("onEngineUnhealthy did not request a recovery reconcile")
}

// TestOnEngineUnhealthy_GivesUpAfterBudget pins the honest give-up: past the
// budget the engine is latched rather than respawned forever, which is what
// keeps a deterministically-crashing model from turning every request into a
// fresh 150-second spawn attempt.
//
// PRODUCT CONTRACT.
func TestOnEngineUnhealthy_GivesUpAfterBudget(t *testing.T) {
	a := newTestAdapter(t, false)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	// A frozen clock keeps every crash inside the stability window.
	frozen := time.Now()
	p := recoverProvider(t, a, func() time.Time { return frozen })

	for i := 0; i < engineRecoveryMaxAttempts; i++ {
		p.onEngineUnhealthy("crash")
		if a.FailureLatched() {
			t.Fatalf("latched after %d crashes, want only after more than %d", i+1, engineRecoveryMaxAttempts)
		}
	}
	// One more than the budget allows.
	p.onEngineUnhealthy("crash")
	if !a.FailureLatched() {
		t.Errorf("engine not latched after %d crashes within %s",
			engineRecoveryMaxAttempts+1, engineRecoveryStableFor)
	}
	if h := a.Health(context.Background()); h.LastErr == "" {
		t.Error("a give-up must record why, so `waired status` can show it")
	}
}

// TestOnEngineUnhealthy_ForgivesAfterStableWindow pins that a single crash a
// day never accumulates into a give-up: a run that stays up for
// engineRecoveryStableFor resets the strike count.
func TestOnEngineUnhealthy_ForgivesAfterStableWindow(t *testing.T) {
	a := newTestAdapter(t, false)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	clock := time.Now()
	p := recoverProvider(t, a, func() time.Time { return clock })

	for i := 0; i < engineRecoveryMaxAttempts*3; i++ {
		p.onEngineUnhealthy("occasional crash")
		if a.FailureLatched() {
			t.Fatalf("latched on crash %d despite each being a stable window apart", i+1)
		}
		clock = clock.Add(engineRecoveryStableFor + time.Second)
	}
}

// TestOnEngineUnhealthy_SkipsEnginesWaiedDoesNotOwn pins that recovery only
// touches an engine waired spawned.
//
// PRODUCT CONTRACT: a borrowed (reuse-mode) engine belongs to the operator,
// and a parked one was stopped on purpose. Restarting either would undo a
// deliberate decision, so the StateFailed the adapter already recorded is the
// whole answer for them.
func TestOnEngineUnhealthy_SkipsEnginesWairedDoesNotOwn(t *testing.T) {
	t.Run("borrowed", func(t *testing.T) {
		a := newTestAdapter(t, true) // borrowed
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning: %v", err)
		}
		p := recoverProvider(t, a, nil)
		p.onEngineUnhealthy("crash")
		time.Sleep(30 * time.Millisecond)
		if p.engineRecoverPending.Load() {
			t.Error("a borrowed engine must not be scheduled for recovery")
		}
	})

	t.Run("parked", func(t *testing.T) {
		a := newTestAdapter(t, false)
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning: %v", err)
		}
		a.Park(context.Background())
		p := recoverProvider(t, a, nil)
		p.onEngineUnhealthy("crash")
		time.Sleep(30 * time.Millisecond)
		if p.engineRecoverPending.Load() {
			t.Error("a parked engine must not be scheduled for recovery")
		}
	})
}

// TestEngineRecoveryBackoff pins the schedule: no wait, then 15s, then 60s.
func TestEngineRecoveryBackoff(t *testing.T) {
	cases := map[int]time.Duration{1: 0, 2: 15 * time.Second, 3: 60 * time.Second, 4: 60 * time.Second}
	for attempt, want := range cases {
		if got := engineRecoveryBackoff(attempt); got != want {
			t.Errorf("engineRecoveryBackoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

// TestStartEngine_ClearsFailureLatch pins the documented way back from a
// give-up: `waired inference engine start`. No new endpoint or CLI verb.
func TestStartEngine_ClearsFailureLatch(t *testing.T) {
	a := newTestAdapter(t, false)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	a.LatchFailed("crashed repeatedly")
	if !a.FailureLatched() {
		t.Fatal("precondition: not latched")
	}

	ec := newEngineController(context.Background(), a, nil)
	if err := ec.StartEngine(context.Background()); err != nil {
		t.Fatalf("StartEngine: %v", err)
	}
	if a.FailureLatched() {
		t.Error("StartEngine must clear the crash-recovery give-up latch")
	}
}
