package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// TestDecideEnginePower is the table that carries the vLLM half of the
// engine power axis onto the darwin and windows legs, where
// infruntime.VLLMAdapter does not exist at all (CLAUDE.md §Test discipline).
func TestDecideEnginePower(t *testing.T) {
	tests := []struct {
		name        string
		in          enginePowerInputs
		wantPower   management.EnginePowerState
		wantManaged bool
	}{
		// ── ollama, unchanged by #881 ──────────────────────────────────
		{"ollama running", enginePowerInputs{
			Engine: catalog.RuntimeOllama, AdapterPresent: true, Health: infruntime.StateReady,
		}, management.EnginePowerRunning, true},
		{"ollama parked", enginePowerInputs{
			Engine: catalog.RuntimeOllama, AdapterPresent: true, Parked: true, Health: infruntime.StateReady,
		}, management.EnginePowerStopped, true},
		{"ollama starting", enginePowerInputs{
			Engine: catalog.RuntimeOllama, AdapterPresent: true, Health: infruntime.StateStarting,
		}, management.EnginePowerStarting, true},
		// An adopted orphan has no process handle, so the axis cannot free
		// its memory and the management handler 409s rather than pretending.
		{"ollama adopted is not managed", enginePowerInputs{
			Engine: catalog.RuntimeOllama, AdapterPresent: true,
			Health: infruntime.StateReady, OllamaAdopted: true,
		}, management.EnginePowerRunning, false},

		// ── vLLM: the whole point of #881 ──────────────────────────────
		{"vllm running", enginePowerInputs{
			Engine: catalog.RuntimeVLLM, AdapterPresent: true, Health: infruntime.StateReady,
		}, management.EnginePowerRunning, true},
		{"vllm parked", enginePowerInputs{
			Engine: catalog.RuntimeVLLM, AdapterPresent: true, Parked: true, Health: infruntime.StateReady,
		}, management.EnginePowerStopped, true},
		{"vllm starting", enginePowerInputs{
			Engine: catalog.RuntimeVLLM, AdapterPresent: true, Health: infruntime.StateStarting,
		}, management.EnginePowerStarting, true},
		// INVERTED by waired-agent#964: this said "stopped", which is true
		// about the memory and silent about the cause. The two engines now
		// answer the same, with a value that separates "waiting for a
		// start" from "waiting for someone to deal with a cause".
		{"vllm failed says failed", enginePowerInputs{
			Engine: catalog.RuntimeVLLM, AdapterPresent: true, Health: infruntime.StateFailed,
		}, management.EnginePowerFailed, true},

		// No adapter is the shape this whole file exists for: on a vLLM
		// host the adapter does not exist until bootstrapVLLM reaches the
		// spawn, and the venv install and the weights download happen
		// first. It is also every windows and darwin run of this table.
		{"vllm with no adapter and a start in flight", enginePowerInputs{
			Engine: catalog.RuntimeVLLM, StartInFlight: true,
		}, management.EnginePowerStarting, true},
		{"vllm with no adapter and nothing in flight", enginePowerInputs{
			Engine: catalog.RuntimeVLLM,
		}, management.EnginePowerStopped, true},
		// managed stays true with no adapter: the latch lives on the
		// provider, so the axis applies. Answering false would make the
		// management handler 409 the stop — a surface reporting a state the
		// system does not honour, which is #881's own shape.
		{"vllm parked with no adapter is still managed", enginePowerInputs{
			Engine: catalog.RuntimeVLLM, Parked: true,
		}, management.EnginePowerStopped, true},

		// ── waired-agent#964: the two arms answer the same facts alike ──
		//
		// INVERTED: the ollama arm let all three of these fall through to
		// "running", which is how a host whose engine had crashed and whose
		// recovery budget was spent reported "Engine power: running" while
		// the tray offered to Stop a process that was not there.
		{"ollama failed says failed, not running", enginePowerInputs{
			Engine: catalog.RuntimeOllama, AdapterPresent: true, Health: infruntime.StateFailed,
		}, management.EnginePowerFailed, true},
		{"ollama stopped is not running", enginePowerInputs{
			Engine: catalog.RuntimeOllama, AdapterPresent: true, Health: infruntime.StateStopped,
		}, management.EnginePowerStopped, true},
		{"ollama not started is not running", enginePowerInputs{
			Engine: catalog.RuntimeOllama, AdapterPresent: true, Health: infruntime.StateNotStarted,
		}, management.EnginePowerStopped, true},
		// A latched failure on an adopted orphan: waired owns no handle, so
		// managed stays false and the axis still cannot act — but the state
		// is reported honestly rather than as "running".
		{"an adopted orphan that died is failed and unmanaged", enginePowerInputs{
			Engine: catalog.RuntimeOllama, AdapterPresent: true,
			Health: infruntime.StateFailed, OllamaAdopted: true,
		}, management.EnginePowerFailed, false},
		// A bounce (model switch, reconcile) is a start in flight, not a
		// stop somebody asked for.
		{"ollama stopped mid-bounce is starting", enginePowerInputs{
			Engine: catalog.RuntimeOllama, AdapterPresent: true,
			Health: infruntime.StateStopped, StartInFlight: true,
		}, management.EnginePowerStarting, true},
		// Parked outranks a failure on both engines: it was stopped on
		// purpose, and reporting the fault sends someone after a setting.
		{"parked outranks a failure", enginePowerInputs{
			Engine: catalog.RuntimeOllama, AdapterPresent: true,
			Parked: true, Health: infruntime.StateFailed,
		}, management.EnginePowerStopped, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			power, managed := decideEnginePower(tc.in)
			if power != tc.wantPower || managed != tc.wantManaged {
				t.Errorf("decideEnginePower(%+v) = %s/%v, want %s/%v",
					tc.in, power, managed, tc.wantPower, tc.wantManaged)
			}
		})
	}
}

// TestEngineStopBudgetCoversTheAdapters ties the daemon's budget to the
// adapters' actual StopTimeouts rather than to a number.
//
// The single 15s constant was sized for ollama's 5s. vLLM's default is 10s,
// so its worst case — the graceful SIGTERM window plus killAndReap's own
// wait — is 20s, and the daemon abandoned the wait mid-kill (#945). The
// StopTimeouts below come from the adapter constructors, so a change there
// that outgrows a budget fails here instead of on a GPU host.
func TestEngineStopBudgetCoversTheAdapters(t *testing.T) {
	ollamaStop := infruntime.DefaultOllamaStopTimeout
	vllmStop := infruntime.DefaultVLLMStopTimeout
	if got := management.EngineStopBudgetFor(catalog.RuntimeOllama); got <= 2*ollamaStop {
		t.Errorf("ollama budget %s does not cover 2 x StopTimeout (%s)", got, 2*ollamaStop)
	}
	if got := management.EngineStopBudgetFor(catalog.RuntimeVLLM); got <= 2*vllmStop {
		t.Errorf("vllm budget %s does not cover 2 x StopTimeout (%s)", got, 2*vllmStop)
	}
	// An engine kind nobody has added an arm for yet must not silently get
	// the larger budget: the ollama default is the conservative answer.
	if management.EngineStopBudgetFor("something-new") != management.EngineStopBudgetFor(catalog.RuntimeOllama) {
		t.Error("an unknown engine should fall back to the ollama budget")
	}
	// And every client budget has to sit above the largest daemon-side one,
	// or the caller reports a timeout it caused itself while the stop is in
	// fact succeeding — waired#316's defect in reverse.
	if management.EngineStopClientBudget() <= management.EngineStopBudgetFor(catalog.RuntimeVLLM) {
		t.Errorf("client budget %s does not outlast the daemon's %s",
			management.EngineStopClientBudget(), management.EngineStopBudgetFor(catalog.RuntimeVLLM))
	}
}

// recordingAdapter is an infruntime.Adapter that records what was asked of
// it. It stands in for the Linux-only VLLMAdapter, which is what the
// provider stores anyway (atomic.Pointer[infruntime.Adapter]).
type recordingAdapter struct {
	name     string
	health   string
	stops    atomic.Int32
	stopErr  error
	cleared  atomic.Int32
	ensuring atomic.Int32
}

func (a *recordingAdapter) Name() string { return a.name }
func (a *recordingAdapter) EnsureRunning(context.Context) error {
	a.ensuring.Add(1)
	return nil
}
func (a *recordingAdapter) Health(context.Context) infruntime.Health {
	return infruntime.Health{State: a.health}
}
func (a *recordingAdapter) Stop(context.Context) error {
	a.stops.Add(1)
	return a.stopErr
}
func (a *recordingAdapter) BaseURL() string      { return "http://127.0.0.1:8000" }
func (a *recordingAdapter) ClearFailure()        { a.cleared.Add(1) }
func (a *recordingAdapter) FailureLatched() bool { return false }

// vllmServingProvider is a provider that serves with vLLM and ALSO has a
// live ollama adapter — which is the production shape, not a contrivance:
// agentInferenceProvider builds an OllamaAdapter unconditionally, whatever
// engine the host serves with. That is why every `p.ollama == nil` guard in
// the tree was dead code.
func vllmServingProvider(t *testing.T, vllm infruntime.Adapter) *agentInferenceProvider {
	t.Helper()
	a := newTestAdapter(t)
	p := &agentInferenceProvider{ollama: a}
	p.setServingEngine(catalog.RuntimeVLLM)
	if vllm != nil {
		p.setVLLM(vllm)
	}
	return p
}

// TestEngineController_VLLMStopActsOnTheVLLMEngine is waired-agent#881.
//
// The last assertion is the defect itself: before this, StopEngine held an
// *OllamaAdapter and parked THAT — so on a vLLM host the command reported
// success, the subsystem advertised "stopped" to the mesh, peers stopped
// routing to the host, and vLLM went on holding the GPU.
func TestEngineController_VLLMStopActsOnTheVLLMEngine(t *testing.T) {
	vllm := &recordingAdapter{name: "vllm", health: infruntime.StateReady}
	p := vllmServingProvider(t, vllm)
	ec := newEngineController(context.Background(), p, nil)

	if power, managed := ec.EngineState(); power != management.EnginePowerRunning || !managed {
		t.Fatalf("initial EngineState = %s managed=%v, want running/true", power, managed)
	}
	if err := ec.StopEngine(context.Background()); err != nil {
		t.Fatalf("StopEngine: %v", err)
	}
	if n := vllm.stops.Load(); n != 1 {
		t.Errorf("vLLM adapter stopped %d times, want 1", n)
	}
	if !p.vllmIsParked() {
		t.Error("the vLLM engine was stopped but not latched; the next trigger would start it again")
	}
	if p.ollama.IsParked() {
		t.Error("the ollama adapter was parked instead of the engine that is serving (waired-agent#881)")
	}
	if power, _ := ec.EngineState(); power != management.EnginePowerStopped {
		t.Errorf("after stop power = %s, want stopped", power)
	}
}

// TestEngineController_VLLMStopRollsBackTheLatchOnFailure: claiming
// "stopped" for a process that may still be alive is the worst of both
// worlds — status lies AND the latch keeps the engine from being revived
// for local and peer traffic alike (#316, the rule ollama's Park follows).
func TestEngineController_VLLMStopRollsBackTheLatchOnFailure(t *testing.T) {
	vllm := &recordingAdapter{
		name: "vllm", health: infruntime.StateReady,
		stopErr: errors.New("kill failed: the process is still alive"),
	}
	p := vllmServingProvider(t, vllm)
	ec := newEngineController(context.Background(), p, nil)

	if err := ec.StopEngine(context.Background()); err == nil {
		t.Fatal("StopEngine reported success for a stop that failed")
	}
	if p.vllmIsParked() {
		t.Error("the latch stands after a stop that could not free the memory")
	}
}

// TestEngineController_VLLMStopWithNoAdapterStillLatches is why the latch
// lives on the provider. `waired inference engine stop` has to hold on a
// host whose bootstrap has not spawned anything yet — the venv install and
// the weights download are exactly when an operator asks for their memory
// back — and an adapter-owned latch has nowhere to live then.
func TestEngineController_VLLMStopWithNoAdapterStillLatches(t *testing.T) {
	p := vllmServingProvider(t, nil)
	ec := newEngineController(context.Background(), p, nil)

	if power, managed := ec.EngineState(); power != management.EnginePowerStopped || !managed {
		t.Fatalf("EngineState with no adapter = %s managed=%v, want stopped/true", power, managed)
	}
	if err := ec.StopEngine(context.Background()); err != nil {
		t.Fatalf("StopEngine with no adapter = %v, want nil (nothing is holding memory)", err)
	}
	if !p.vllmIsParked() {
		t.Error("nothing was latched, so the download would finish and start an engine the operator stopped")
	}
	if decideVLLMBootstrap(nil, "", p.vllmIsParked(), false) != vllmBootstrapParked {
		t.Error("the bootstrap would still run on a host whose engine was stopped")
	}
}

// TestEngineController_VLLMStartClearsTheLatchAndDispatches: the start goes
// through requestEngineStart, the only path that can build an adapter when
// there is none, and it must not touch the ollama adapter's own latches.
func TestEngineController_VLLMStartClearsTheLatchAndDispatches(t *testing.T) {
	vllm := &recordingAdapter{name: "vllm", health: infruntime.StateStopped}
	p := vllmServingProvider(t, vllm)
	// Local inference is off, so startEngineAndBootstrap declines early and
	// records why — an observable that proves the dispatch reached the real
	// path without this test needing a venv.
	//
	// The FIRST read answers "on", because StartEngine now refuses outright
	// when the toggle is off (waired-agent#964) and this test is about what
	// happens after it decides to proceed. Reads after that answer "off", so
	// the dispatched bootstrap still declines where it always did. The
	// toggle is a live reading of a persisted setting, so a host really can
	// answer differently across two reads.
	var reads atomic.Int32
	p.isInferenceDisabled = func() bool { return reads.Add(1) > 1 }
	p.agentCtx = context.Background()
	p.logger = testLogger()
	ec := newEngineController(context.Background(), p, nil)

	if err := ec.StopEngine(context.Background()); err != nil {
		t.Fatalf("StopEngine: %v", err)
	}
	if err := ec.StartEngine(context.Background()); err != nil {
		t.Fatalf("StartEngine: %v", err)
	}
	if p.vllmIsParked() {
		t.Error("the latch survived an explicit start")
	}
	if n := vllm.cleared.Load(); n != 1 {
		t.Errorf("give-up latch cleared %d times, want 1", n)
	}
	waitForCond(t, 2*time.Second, "the start to reach startEngineAndBootstrap", func() bool {
		d := p.lastStartDecline.Load()
		return d != nil && *d == errInferenceOff.Error()
	})
	// The ollama adapter is not this host's engine and must not be touched.
	if n := vllm.ensuring.Load(); n != 0 {
		t.Errorf("the controller called EnsureRunning on the current adapter %d times; "+
			"the vLLM start has to re-resolve the venv, weights and tuning", n)
	}
}

func waitForCond(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", budget, what)
}

// PRODUCT CONTRACT (waired-agent#964): the hard power axis does not
// overrule the persisted soft toggle, and says so.
//
// The two arms used to disagree and both lied about it. `waired inference
// engine start` on an ollama host called EnsureRunning directly, so the
// engine came up on a device somebody had turned local inference off on;
// on a vLLM host the refusal happened inside a dispatched goroutine where
// nobody saw it. The CLI printed "engine start ok." for both.
func TestEngineController_StartRefusedWhileLocalInferenceIsOff(t *testing.T) {
	for _, engine := range []string{catalog.RuntimeOllama, catalog.RuntimeVLLM} {
		t.Run(engine, func(t *testing.T) {
			vllm := &recordingAdapter{name: "vllm", health: infruntime.StateStopped}
			p := vllmServingProvider(t, vllm)
			p.setServingEngine(engine)
			p.isInferenceDisabled = func() bool { return true }
			p.agentCtx = context.Background()
			p.logger = testLogger()
			ec := newEngineController(context.Background(), p, nil)

			if err := ec.StopEngine(context.Background()); err != nil {
				t.Fatalf("StopEngine: %v", err)
			}
			err := ec.StartEngine(context.Background())
			if !errors.Is(err, management.ErrEngineStartRefused) {
				t.Fatalf("StartEngine err = %v, want it to wrap ErrEngineStartRefused", err)
			}
			// A refusal does nothing, which is the difference between
			// refusing and failing: the latches an operator set are still
			// exactly as they were.
			if engine == catalog.RuntimeVLLM && !p.vllmIsParked() {
				t.Error("a refused start cleared the park latch")
			}
			if n := vllm.cleared.Load(); n != 0 {
				t.Errorf("a refused start cleared the give-up latch %d times, want 0", n)
			}
			if n := vllm.ensuring.Load(); n != 0 {
				t.Errorf("a refused start called EnsureRunning %d times, want 0", n)
			}
		})
	}
}
