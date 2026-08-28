package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// startFailProvider is recoverProvider's sibling for the start-failure path:
// same real adapter and injectable clock, plus the registry, profiler and
// resolver Status() reads — because half of what this change is about is what
// reaches the wire, and a fake in front of Status would skip the subject.
func startFailProvider(t *testing.T, a *infruntime.OllamaAdapter, now func() time.Time) *agentInferenceProvider {
	t.Helper()
	reg := infruntime.NewRegistry()
	reg.Register(a)
	return &agentInferenceProvider{
		ollama:   a,
		registry: reg,
		store:    catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		profiler: hardware.NewProfiler(t.TempDir(),
			hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
				return nil, hardware.Accelerators{}, nil
			})),
		dlProgress: newDownloadProgress(),
		// The engine binary IS installed on the host this issue is about —
		// that is the whole trap. Without this the state would be no_engine
		// and the arms under test would never be reached.
		ollamaUsable: func() bool { return true },
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		agentCtx:     context.Background(),
		now:          now,
	}
}

// seedActiveReadyModel puts an active model on disk, so the subsystem_state
// derivation reaches its last arm instead of stopping at awaiting_model.
func seedActiveReadyModel(t *testing.T, p *agentInferenceProvider) {
	t.Helper()
	const id = "qwen3-8b-instruct"
	if err := p.store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: id, VariantID: id + "-q4"}
		if s.Models == nil {
			s.Models = map[string]catalog.ModelState{}
		}
		s.Models[id] = catalog.ModelState{VariantID: id + "-q4", State: catalog.ModelStateReady}
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

// TestOnEngineStartFailed_GivesUpAfterBudget is the daemon half of #310.
//
// PRODUCT CONTRACT: an engine that never comes up spends the same recovery
// budget as one that dies while serving, and past it waired stops trying.
//
// Before this there was no path at all: markUnhealthy demotes only out of
// StateReady, which an engine killed at exec never reaches, so no strike was
// ever charged and FailureLatched() stayed false forever — leaving #330's
// "installed but the daemon gave up" wizard arm unreachable on exactly the
// macOS hosts it was written for.
func TestOnEngineStartFailed_GivesUpAfterBudget(t *testing.T) {
	a := newTestAdapter(t)
	frozen := time.Now()
	p := startFailProvider(t, a, func() time.Time { return frozen })

	for i := range engineRecoveryMaxAttempts {
		p.onEngineStartFailed("ollama: process exited during startup: signal: killed")
		if a.FailureLatched() {
			t.Fatalf("latched after %d failed starts, want only after more than %d",
				i+1, engineRecoveryMaxAttempts)
		}
	}
	p.onEngineStartFailed("ollama: process exited during startup: signal: killed")

	latched, reason := a.FailureLatchedReason()
	if !latched {
		t.Fatalf("engine not latched after %d failed starts within %s",
			engineRecoveryMaxAttempts+1, engineRecoveryStableFor)
	}
	if !strings.Contains(reason, "signal: killed") {
		t.Errorf("latch reason = %q, want the engine's own account folded in", reason)
	}
	if !strings.Contains(reason, "waired inference engine start") {
		t.Errorf("latch reason = %q, want the documented way back", reason)
	}
}

// TestOnEngineStartFailed_DoesNotScheduleARestart. PRODUCT CONTRACT: this
// handler counts and gives up; it never respawns. Its caller has already
// retried on its own budget, and adding another restart from here is exactly
// the macOS respawn storm engine_bootstrap.go refuses to build by leaving the
// latch alone (#310).
func TestOnEngineStartFailed_DoesNotScheduleARestart(t *testing.T) {
	// Both halves claim the in-flight latch exactly as
	// TestOnEngineUnhealthy_SchedulesRecovery does. Without it
	// requestEngineReconcile's goroutine DRAINS engineRecoverPending, and
	// the negative assertion would pass over a handler that DID schedule a
	// restart — which is how it read until a mutant adding one survived.
	//
	// Separate providers, because the two halves share a recovery budget and
	// the second strike would carry a 15s backoff.
	t.Run("a crash schedules one", func(t *testing.T) {
		p := startFailProvider(t, newTestAdapter(t), nil)
		p.engineReconcileInFlight.Store(true)

		p.onEngineUnhealthy("engine returned HTTP 500: llama-server process has terminated")

		waitUntil(t, "onEngineUnhealthy to request a recovery", p.engineRecoverPending.Load)
	})

	t.Run("a failed start does not", func(t *testing.T) {
		p := startFailProvider(t, newTestAdapter(t), nil)
		p.engineReconcileInFlight.Store(true)

		p.onEngineStartFailed("ollama: process exited during startup: signal: killed")

		time.Sleep(50 * time.Millisecond)
		if p.engineRecoverPending.Load() {
			t.Error("a start that never came up must not schedule a restart of its own")
		}
	})
}

// TestOnEngineStartFailed_ForgivesAfterStableWindow. PRODUCT CONTRACT: a
// machine that fails a start once in a while — a port briefly taken at boot —
// must never accumulate into a give-up. Only a run of failures inside one
// stability window is evidence about the install.
func TestOnEngineStartFailed_ForgivesAfterStableWindow(t *testing.T) {
	a := newTestAdapter(t)
	clock := time.Now()
	p := startFailProvider(t, a, func() time.Time { return clock })

	for i := range engineRecoveryMaxAttempts * 3 {
		p.onEngineStartFailed("transient")
		if a.FailureLatched() {
			t.Fatalf("latched on failure %d despite each being a stable window apart", i+1)
		}
		clock = clock.Add(engineRecoveryStableFor + time.Second)
	}
}

// TestOnEngineStartFailed_SharesTheBudgetWithCrashes. PRODUCT CONTRACT: one
// budget, not two. A host that dies during startup, is restarted, dies
// mid-serve, and so on would otherwise keep two counters that each stay under
// the limit forever while the engine is plainly not staying up.
func TestOnEngineStartFailed_SharesTheBudgetWithCrashes(t *testing.T) {
	a := newTestAdapter(t)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	frozen := time.Now()
	p := startFailProvider(t, a, func() time.Time { return frozen })

	// Alternate the two shapes. Neither reaches the budget on its own.
	p.onEngineStartFailed("never came up")
	p.onEngineUnhealthy("died while serving")
	p.onEngineStartFailed("never came up")
	if a.FailureLatched() {
		t.Fatalf("latched after 3 failures, want only after more than %d", engineRecoveryMaxAttempts)
	}
	p.onEngineUnhealthy("died while serving")
	if !a.FailureLatched() {
		t.Error("two failure shapes must charge one budget; 4 failures did not latch")
	}
}

// TestOnEngineStartFailed_SkipsEnginesWairedDoesNotOwn mirrors the same guard
// on the crash path. PRODUCT CONTRACT: an adopted orphan has no handle waired
// can restart and a parked one was stopped on purpose — latching either would
// have waired refuse to serve an engine whose failures are not its own.
func TestOnEngineStartFailed_SkipsEnginesWairedDoesNotOwn(t *testing.T) {
	t.Run("adopted", func(t *testing.T) {
		a := newAdoptedTestAdapter(t)
		p := startFailProvider(t, a, nil)
		for range engineRecoveryMaxAttempts * 2 {
			p.onEngineStartFailed("not reachable")
		}
		if a.FailureLatched() {
			t.Error("an adopted engine must never be latched by waired")
		}
	})

	t.Run("parked", func(t *testing.T) {
		a := newTestAdapter(t)
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning: %v", err)
		}
		if err := a.Park(context.Background()); err != nil {
			t.Fatalf("Park: %v", err)
		}
		p := startFailProvider(t, a, nil)
		for range engineRecoveryMaxAttempts * 2 {
			p.onEngineStartFailed("parked, so nothing started")
		}
		if a.FailureLatched() {
			t.Error("a parked engine must not be latched: it was stopped on purpose")
		}
	})
}

// TestStatus_LatchedEngineStaysEngineFailedThroughAStop is the first test over
// the subsystem_state switch, and it pins the lie it removes.
//
// PRODUCT CONTRACT: an engine waired has permanently stopped restarting reads
// as engine_failed regardless of what Health() says this instant. LatchFailed
// writes StateFailed, but Stop() then overwrites a.state with no giveUp guard
// — a model switch, a reconcile bounce — after which the derivation used to
// fall straight through to "ready" as soon as the active model was on disk.
func TestStatus_LatchedEngineStaysEngineFailedThroughAStop(t *testing.T) {
	a := newTestAdapter(t)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p := startFailProvider(t, a, nil)
	ctx := context.Background()
	// An active model that IS on disk — the condition under which the old
	// derivation answered "ready". Without it the fall-through lands on
	// awaiting_model, which is also wrong but is not the reported lie.
	seedActiveReadyModel(t, p)

	if got := p.Status(ctx).SubsystemState; got != "ready" {
		t.Fatalf("precondition: subsystem_state = %q, want ready", got)
	}

	a.LatchFailed("engine failed to start 4 times within 5m0s; automatic restart disabled")
	if got := p.Status(ctx).SubsystemState; got != "engine_failed" {
		t.Errorf("subsystem_state right after the latch = %q, want engine_failed", got)
	}

	// The Stop that used to hide it.
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if h := a.Health(ctx); h.State == infruntime.StateFailed {
		t.Fatalf("precondition: Stop no longer clobbers the failed state (%s) — "+
			"this test would pass vacuously", h.State)
	}
	if got := p.Status(ctx).SubsystemState; got != "engine_failed" {
		t.Errorf("subsystem_state after latch+Stop = %q, want engine_failed", got)
	}
}

// TestStatus_ParkedOutranksALatch. PRODUCT CONTRACT: the operator's own hard
// stop is the more useful answer — "you stopped it" beats "it broke" on a
// machine where both are true, and only "stopped" tells them the memory was
// freed on purpose (#186).
func TestStatus_ParkedOutranksALatch(t *testing.T) {
	a := newTestAdapter(t)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p := startFailProvider(t, a, nil)

	a.LatchFailed("gave up")
	if err := a.Park(context.Background()); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if got := p.Status(context.Background()).SubsystemState; got != "stopped" {
		t.Errorf("subsystem_state = %q for a parked engine that is also latched, want stopped", got)
	}
}

// TestStatus_FailureLatchedReachesTheWire pins the field a client needs to
// tell "down, recovering" from "down, and waiting will not help" — the thing
// `waired init` previously had to infer from how long a flapping state had
// held. Also pins omitempty: a healthy runtime must not start carrying it.
func TestStatus_FailureLatchedReachesTheWire(t *testing.T) {
	a := newTestAdapter(t)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p := startFailProvider(t, a, nil)
	ctx := context.Background()

	healthy, err := json.Marshal(p.Status(ctx))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(healthy), "failure_latched") {
		t.Errorf("a healthy runtime published failure_latched: %s", healthy)
	}

	const reason = "engine failed to start 4 times within 5m0s; automatic restart disabled"
	a.LatchFailed(reason)
	// The Stop is the point: it is what empties Health().LastErr, and the
	// reason must survive it on the wire too.
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st := p.Status(ctx)
	rt, ok := st.Runtimes["ollama"]
	if !ok {
		t.Fatalf("no ollama runtime in %+v", st.Runtimes)
	}
	if !rt.FailureLatched {
		t.Error("runtimes.ollama.failure_latched = false after a give-up")
	}
	if rt.LastError != reason {
		t.Errorf("runtimes.ollama.last_error = %q, want the latch reason %q", rt.LastError, reason)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"failure_latched":true`) {
		t.Errorf("failure_latched missing from the JSON body: %s", raw)
	}
}

// THE #1110 BAR. PRODUCT CONTRACT: an explicit `waired inference engine
// start` gives the recovery budget back, not just the latch.
//
// The two halves of "give up" live in different places — the latch on the
// adapter, the strike count that DECIDES the latch on the provider — and
// the engine controller cleared only the first. So an operator who fixed
// the cause and started the engine got ONE attempt: the boot path had
// already spent three, and a single further failure inside the stability
// window made four. The troubleshooting page says "up to three times".
func TestResetEngineStrikes_GivesTheWholeBudgetBack(t *testing.T) {
	a := newTestAdapter(t)
	frozen := time.Now()
	p := startFailProvider(t, a, func() time.Time { return frozen })

	// A boot that spent its attempts, which is three strikes and no latch.
	for range engineRecoveryMaxAttempts {
		p.onEngineStartFailed("ollama: process exited during startup: exit status 1")
	}
	if a.FailureLatched() {
		t.Fatalf("latched after %d strikes; the budget is %d",
			engineRecoveryMaxAttempts, engineRecoveryMaxAttempts+1)
	}

	// The operator fixes the cause and starts the engine.
	a.ClearFailure()
	p.resetEngineStrikes()

	// They get the documented budget, not one attempt.
	for i := range engineRecoveryMaxAttempts {
		p.onEngineStartFailed("ollama: process exited during startup: exit status 1")
		if a.FailureLatched() {
			t.Fatalf("latched %d strike(s) after an explicit start reset the budget;\n"+
				"the operator is promised %d", i+1, engineRecoveryMaxAttempts)
		}
	}
	p.onEngineStartFailed("ollama: process exited during startup: exit status 1")
	if !a.FailureLatched() {
		t.Errorf("never latched after a full budget past the reset; the reset must give\n" +
			"the budget back, not remove it")
	}
}

// TestSetupEngineHealth_ReasonSurvivesAStop pins the defect that produced a
// red wizard row with nothing on it. It runs against a REAL adapter on
// purpose: the fake setupProvider replaces setupEngineHealth wholesale, so
// through it the (true, "") case is structurally unwritable — the seam has to
// sit below the behaviour under test.
func TestSetupEngineHealth_ReasonSurvivesAStop(t *testing.T) {
	a := newTestAdapter(t)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p := startFailProvider(t, a, nil)
	ctx := context.Background()

	const reason = "engine failed to start 4 times within 5m0s; automatic restart disabled"
	a.LatchFailed(reason)
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if h := a.Health(ctx); h.LastErr != "" {
		t.Fatalf("precondition: Health().LastErr = %q after Stop, want it clobbered", h.LastErr)
	}

	stopped, needsRepair, got := p.setupEngineHealth(ctx, catalog.RuntimeOllama)
	if !stopped {
		t.Fatal("setupEngineHealth reported no latch after a give-up")
	}
	if !needsRepair {
		t.Error("a give-up latch must still ask for a repair; it is the one arm that claims\n" +
			"the install itself is broken, and the executor's presence gate reads it")
	}
	if got != reason {
		t.Errorf("setupEngineHealth reason = %q, want %q — a failed row with no reason is\n"+
			"what the wizard renders when this reads the wrong field", got, reason)
	}
}

// TestSetupEngineHealth_QuietWhenNothingIsLatched is the negative control:
// every ordinary restart and model download leaves the engine not-ready for a
// while, and none of them may paint the row red.
func TestSetupEngineHealth_QuietWhenNothingIsLatched(t *testing.T) {
	a := newTestAdapter(t)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p := startFailProvider(t, a, nil)

	stopped, needsRepair, reason := p.setupEngineHealth(context.Background(), catalog.RuntimeOllama)
	if stopped || needsRepair || reason != "" {
		t.Errorf("setupEngineHealth on a healthy engine = (%v, %v, %q), want (false, false, \"\")",
			stopped, needsRepair, reason)
	}
}

// TestSetupEngineHealth_SpeaksWhenTheBootPathSpentItsAttempts is the #1093
// bar. PRODUCT CONTRACT: a daemon that has stopped trying to start its
// engine says so on the wizard's engine row, whether or not it latched.
//
// The latch needs four strikes and one boot spends three, so on the ordinary
// shape of this bug — a machine restarted with the engine's port taken —
// nothing latched, this returned (false, ""), and the row fell through to
// `case installed:` and rendered DONE over a dead engine.
//
// Against a REAL adapter for the reason the two tests above are: the fake
// setupProvider replaces this function wholesale, so the table test cannot
// see the hole at all.
func TestSetupEngineHealth_SpeaksWhenTheBootPathSpentItsAttempts(t *testing.T) {
	a := newTestAdapter(t)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p := startFailProvider(t, a, nil)
	ctx := context.Background()

	const reason = "another program is already listening on 127.0.0.1:9475, " +
		"the port the inference engine was told to use\nollama: process exited during startup"
	p.noteEngineStartExhausted(reason)

	stopped, needsRepair, got := p.setupEngineHealth(ctx, catalog.RuntimeOllama)
	if !stopped {
		t.Fatal("setupEngineHealth stayed quiet after the bootstrap spent every attempt;\n" +
			"the wizard's engine row renders DONE for this, over an engine that cannot start")
	}
	if got != reason {
		t.Errorf("reason = %q, want %q", got, reason)
	}
	if needsRepair {
		t.Error("a spent bootstrap must NOT ask for a repair: it reopens the executor's\n" +
			"presence gate to reinstall the engine, which does nothing for a busy port")
	}

	// And the latch still outranks it, with its own reason.
	a.LatchFailed("engine failed to start 4 times within 5m0s; automatic restart disabled")
	if _, _, got := p.setupEngineHealth(ctx, catalog.RuntimeOllama); !strings.Contains(got, "4 times") {
		t.Errorf("after a latch the reason = %q, want the latch's own message — the latch is\n"+
			"the stronger claim and must win", got)
	}
}

// TestSetupEngineHealth_ForgetsTheSpentAttemptsOnAnExplicitStart pins that
// the record has the lifetime of the attempt that wrote it. The operator has
// changed something; last run's verdict is not the answer to this one.
func TestSetupEngineHealth_ForgetsTheSpentAttemptsOnAnExplicitStart(t *testing.T) {
	a := newTestAdapter(t)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p := startFailProvider(t, a, nil)
	p.noteEngineStartExhausted("something the last boot said")

	p.resetEngineStrikes()

	if stopped, _, reason := p.setupEngineHealth(context.Background(), catalog.RuntimeOllama); stopped {
		t.Errorf("setupEngineHealth still reports the previous boot's give-up (%q) after an\n"+
			"explicit start reset the budget", reason)
	}
}
