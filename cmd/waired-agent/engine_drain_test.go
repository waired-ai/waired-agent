package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// drainProvider is the smallest provider awaitEngineDrain reads: a budget and
// a live in-flight count. The counter is the real seam — servingInflight is
// the same func field main.go wires to inference.Server.InflightCount — so
// these tests drive the subject rather than a stand-in for it.
func drainProvider(t *testing.T, budgetMs int, inflight *atomic.Int64) *agentInferenceProvider {
	t.Helper()
	return &agentInferenceProvider{
		cfg:             agentconfig.InferenceConfig{EngineDrainBudgetMs: budgetMs},
		store:           catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		servingInflight: func() int { return int(inflight.Load()) },
	}
}

// TestAwaitEngineDrain_IdleDoesNotWait: the steady-state bounce. Nothing is
// running, so the drain must cost nothing — a reconcile on an idle machine is
// as fast as it was before waired-agent#1304.
func TestAwaitEngineDrain_IdleDoesNotWait(t *testing.T) {
	var inflight atomic.Int64
	p := drainProvider(t, 600000, &inflight)

	start := time.Now()
	if got := p.awaitEngineDrain(t.Context(), p.engineDrainBudget()); got != drainIdle {
		t.Errorf("awaitEngineDrain = %v, want drainIdle", got)
	}
	if el := time.Since(start); el > engineDrainPoll {
		t.Errorf("idle drain took %v; it must not sleep at all", el)
	}
}

// TestAwaitEngineDrain_WaitsForTheTurnToFinish is the defect in
// waired-agent#1304: a switch used to stop `ollama serve` with a turn still
// on it. Here the turn ends on its own and the drain reports it, well inside
// the budget.
func TestAwaitEngineDrain_WaitsForTheTurnToFinish(t *testing.T) {
	var inflight atomic.Int64
	inflight.Store(1)
	p := drainProvider(t, 5000, &inflight)

	go func() {
		time.Sleep(3 * engineDrainPoll)
		inflight.Store(0)
	}()

	start := time.Now()
	got := p.awaitEngineDrain(t.Context(), p.engineDrainBudget())
	el := time.Since(start)
	if got != drainQuiet {
		t.Fatalf("awaitEngineDrain = %v, want drainQuiet", got)
	}
	if el < engineDrainPoll {
		t.Errorf("drain returned after %v; it cannot have observed the turn", el)
	}
	if inflight.Load() != 0 {
		t.Errorf("drain reported quiet with %d still in flight", inflight.Load())
	}
}

// TestAwaitEngineDrain_BudgetIsSpentNotInfinite pins the bound. Nothing here
// stops new turns arriving, so a machine under load would hold the operator's
// switch forever without this — which is the failure the budget exists to
// prevent, not a nicety.
func TestAwaitEngineDrain_BudgetIsSpentNotInfinite(t *testing.T) {
	var inflight atomic.Int64
	inflight.Store(2) // never drops

	p := drainProvider(t, 3*int(engineDrainPoll/time.Millisecond), &inflight)
	start := time.Now()
	got := p.awaitEngineDrain(t.Context(), p.engineDrainBudget())
	el := time.Since(start)
	if got != drainBudgetSpent {
		t.Fatalf("awaitEngineDrain = %v, want drainBudgetSpent", got)
	}
	if el < p.engineDrainBudget() {
		t.Errorf("gave up after %v, before the %v budget", el, p.engineDrainBudget())
	}
	if el > p.engineDrainBudget()+2*engineDrainPoll {
		t.Errorf("overshot the budget by more than one poll: %v vs %v", el, p.engineDrainBudget())
	}
}

// TestAwaitEngineDrain_ZeroBudgetIsTheOldBehaviour: the knob's off position
// has to be the behaviour that shipped before this existed, so an operator
// who does not want their switch held can have exactly that and nothing else.
func TestAwaitEngineDrain_ZeroBudgetIsTheOldBehaviour(t *testing.T) {
	var inflight atomic.Int64
	inflight.Store(3)
	p := drainProvider(t, 0, &inflight)

	if got := p.engineDrainBudget(); got != 0 {
		t.Fatalf("engineDrainBudget = %v, want 0", got)
	}
	start := time.Now()
	if got := p.awaitEngineDrain(t.Context(), p.engineDrainBudget()); got != drainDisabled {
		t.Errorf("awaitEngineDrain = %v, want drainDisabled", got)
	}
	if el := time.Since(start); el > engineDrainPoll {
		t.Errorf("a disabled drain slept for %v", el)
	}
	if got := p.drainBeforeBounce(t.Context(), "model switch"); got != drainDisabled {
		t.Errorf("drainBeforeBounce = %v, want drainDisabled", got)
	}
}

// TestAwaitEngineDrain_DaemonStopEndsTheWait: the process is going away, so
// the wait says so rather than reporting anything about the turns. Without
// this a shutdown would sit out the whole ten-minute budget.
func TestAwaitEngineDrain_DaemonStopEndsTheWait(t *testing.T) {
	var inflight atomic.Int64
	inflight.Store(1) // never drops
	p := drainProvider(t, 600000, &inflight)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(2 * engineDrainPoll)
		cancel()
	}()
	start := time.Now()
	if got := p.awaitEngineDrain(ctx, p.engineDrainBudget()); got != drainStopping {
		t.Errorf("awaitEngineDrain = %v, want drainStopping", got)
	}
	if el := time.Since(start); el > 10*engineDrainPoll {
		t.Errorf("cancel took %v to end the wait", el)
	}
}

// TestDrainOutcomeStrings pins the words the journal carries, because the
// journal is how a reader finds out why an engine took ten minutes to
// restart. A record of today's behaviour, not a product contract.
func TestDrainOutcomeStrings(t *testing.T) {
	for _, tc := range []struct {
		out  drainOutcome
		want string
	}{
		{drainIdle, "idle"},
		{drainQuiet, "quiet"},
		{drainBudgetSpent, "budget_spent"},
		{drainStopping, "stopping"},
		{drainDisabled, "disabled"},
	} {
		if got := tc.out.String(); got != tc.want {
			t.Errorf("drainOutcome(%d).String() = %q, want %q", tc.out, got, tc.want)
		}
	}
}

// TestEngineDrainBudget_DefaultIsTheOwnerRuling pins ten minutes (owner
// ruling 2026-09-12, waired-ai/waired#1361 lane L106). Product contract: the
// figure was chosen against the measured 34-84 s first-byte times on the
// 0.0.3-rc6 review fleet, so a smaller default would cut the turns the drain
// exists to protect.
func TestEngineDrainBudget_DefaultIsTheOwnerRuling(t *testing.T) {
	cfg := agentconfig.Defaults()
	if got := cfg.Inference.EngineDrainBudgetMs; got != 600000 {
		t.Errorf("default EngineDrainBudgetMs = %d, want 600000 (ten minutes)", got)
	}
	p := &agentInferenceProvider{
		cfg:    cfg.Inference,
		store:  catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if got := p.engineDrainBudget(); got != 10*time.Minute {
		t.Errorf("engineDrainBudget = %v, want 10m", got)
	}
}

// TestEngineRestartedSince is the real rule behind the gateway's
// engine_restarted classification: a stop stamped AFTER a leg began is one
// that happened under it, and a stop stamped before it is not.
//
// The gateway's own tests answer by this rule rather than by a fixed
// timestamp, because the instant proxyAnthropicStream hands in is its own
// dispatch start. This is where the rule itself is pinned.
func TestEngineRestartedSince(t *testing.T) {
	p := &agentInferenceProvider{
		cfg:    agentconfig.InferenceConfig{},
		store:  catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	legStart := time.Now()

	if p.engineRestartedSince(legStart) {
		t.Error("a device that never stopped its engine claimed it had")
	}
	if p.engineRestartedSince(time.Time{}) {
		t.Error("a caller with no start instant was given a claim anyway")
	}

	p.noteEngineStopped()

	if !p.engineRestartedSince(legStart) {
		t.Error("a stop after the leg began was not reported")
	}

	// And the stamp must keep the MONOTONIC reading time.Now() gave it.
	//
	// The first version of this stored unix nanoseconds, which is the wall
	// clock alone — and wall-clock resolution is a property of the OS, as
	// coarse as 15.6 ms on Windows. Two time.Now() calls microseconds
	// apart then read as the same instant and a stop stamped after a leg
	// began compared as not-after. Both the Windows and macOS CI hosts
	// failed the check above on that version; THIS machine passed it,
	// which is exactly why the local green said nothing.
	//
	// There is no way through the public API to build a Time whose wall
	// and monotonic readings disagree, so a comparison that goes through
	// the wall clock cannot be caught by a value fixture — only by a host
	// whose clock is coarse, or by this. The " m=" suffix is how the
	// standard library renders a monotonic reading (time.Time.String).
	stamp := p.engineStoppedAt.Load()
	if stamp == nil {
		t.Fatal("noteEngineStopped stored nothing")
	}
	if !strings.Contains(stamp.String(), " m=") {
		t.Errorf("the stop stamp lost its monotonic reading (%s); the comparison would fall back "+
			"to the wall clock, whose resolution is an OS property", stamp)
	}
	if p.engineRestartedSince(time.Now().Add(time.Second)) {
		t.Error("a stop was reported to a leg that began after it")
	}
	if p.engineRestartedSince(time.Time{}) {
		t.Error("a caller with no start instant was given a claim after a stop")
	}
}

// TestReconcile_CrashRecoveryDoesNotStampADeliberateStop: an engine that died
// on its own did not end the turn on anyone's instruction, so recovery must
// leave the stamp alone. Otherwise every crash would read as something this
// device chose to do, which is the misattribution waired-agent#1304 is
// removing rather than relocating.
//
// Product contract, same ruling as the drain: the two arms of that ruling are
// "do not wait for a corpse" and "do not take the blame for one".
func TestReconcile_CrashRecoveryDoesNotStampADeliberateStop(t *testing.T) {
	manifests := recTestManifests()
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{
			"heavy": {State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "heavy:8b"},
		}
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: "heavy", VariantID: "q4", DecidedBy: "auto",
		}
	}); err != nil {
		t.Fatal(err)
	}
	adapter, sp := newSwapTestAdapter(t)
	if err := adapter.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	spawnsBefore := sp.count()

	p := &agentInferenceProvider{
		cfg:       agentconfig.InferenceConfig{PreferredModelID: "heavy", BundledModelID: "heavy"},
		manifests: manifests,
		store:     store,
		ollama:    adapter,
		profiler:  cpuSwapProfiler(t),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	p.engineRecoverPending.Store(true)
	p.reconcileEngineServe(context.Background())

	if got := sp.count(); got <= spawnsBefore {
		t.Fatalf("recovery did not restart the engine (spawns %d, before %d); the test is not exercising the arm", got, spawnsBefore)
	}
	if p.engineStoppedAt.Load() != nil {
		t.Error("crash recovery stamped a deliberate stop; a crashed engine's lost turns would be filed as our doing")
	}
	if p.engineRestartedSince(time.Now().Add(-time.Minute)) {
		t.Error("engineRestartedSince claimed a restart after crash recovery")
	}
}

// TestSwapPreferredModel_HoldsTheBounceWhileATurnIsRunning is the wiring, and
// the defect end to end: with a turn on the engine, an operator switch must
// not stop `ollama serve` — and must not flip Active either, because every
// surface reads Active as "the new model is answering now" and the tray drops
// the row's "(switching…)" on it.
//
// Product contract (waired-agent#1304, owner ruling 2026-09-12): the switch
// waits for the turn. It applies as soon as the turn ends.
func TestSwapPreferredModel_HoldsTheBounceWhileATurnIsRunning(t *testing.T) {
	manifests := recTestManifests()
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{
			"heavy": {State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "heavy:8b"},
			"light": {State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "light:2b"},
		}
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: "heavy", VariantID: "q4", DecidedBy: "auto",
		}
	}); err != nil {
		t.Fatal(err)
	}

	adapter, sp := newSwapTestAdapter(t)
	if err := adapter.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	spawnsBefore := sp.count()

	var inflight atomic.Int64
	inflight.Store(1) // one turn on the old model

	p := &agentInferenceProvider{
		cfg: agentconfig.InferenceConfig{
			PreferredModelID: "heavy", BundledModelID: "heavy",
			EngineDrainBudgetMs: int(waitBackstop / time.Millisecond),
		},
		manifests:       manifests,
		store:           store,
		ollama:          adapter,
		profiler:        cpuSwapProfiler(t),
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		servingInflight: func() int { return int(inflight.Load()) },
	}

	if _, err := p.SwapPreferredModel(context.Background(), "light"); err != nil {
		t.Fatalf("SwapPreferredModel: %v", err)
	}
	// The preference publishes immediately — that half is #812 and is what
	// puts "(switching…)" on the row. Only the engine waits.
	if got := p.effectivePreferredModelID(); got != "light" {
		t.Fatalf("override not published: effectivePreferredModelID = %q, want light", got)
	}

	// Held: several polls later, neither Active nor the engine has moved.
	time.Sleep(4 * engineDrainPoll)
	st, _ := store.Load()
	if st.Active == nil || st.Active.ModelID != "heavy" {
		t.Errorf("Active = %+v while a turn is running; the old model is still the one answering", st.Active)
	}
	if got := sp.count(); got != spawnsBefore {
		t.Errorf("engine respawned %d time(s) with a turn in flight; the bounce must wait", got-spawnsBefore)
	}

	// The turn ends.
	inflight.Store(0)

	deadline := time.Now().Add(waitBackstop)
	for time.Now().Before(deadline) {
		st, _ := store.Load()
		if st.Active != nil && st.Active.ModelID == "light" && sp.count() > spawnsBefore {
			return // applied once the engine was free
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ = store.Load()
	t.Fatalf("switch never applied after the turn ended: Active=%+v spawns=%d (before=%d)",
		st.Active, sp.count(), spawnsBefore)
}
