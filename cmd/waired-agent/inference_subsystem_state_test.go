package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// subsystemState was inline in Status() until waired#1064 gave it a second
// reader (the mesh push that tells peers why a node is or is not serving).
// The rows below are a record of today's behaviour — the answers Status()
// already produced — not new policy; they exist so the extraction cannot
// quietly change one of them, and so the ORDER of the arms stays pinned.
func TestSubsystemState(t *testing.T) {
	// serving is a machine with nothing wrong; each case perturbs one fact.
	serving := inferenceSubsystemFacts{
		UsableEngine: true,
		EngineState:  infruntime.StateReady,
		HasActive:    true,
		ModelKnown:   true,
		ModelState:   catalog.ModelStateReady,
	}
	with := func(mut func(*inferenceSubsystemFacts)) inferenceSubsystemFacts {
		f := serving
		mut(&f)
		return f
	}

	tests := []struct {
		name  string
		facts inferenceSubsystemFacts
		want  string
	}{
		{"serving", serving, signer.SubsystemStateReady},

		{"operator paused", with(func(f *inferenceSubsystemFacts) {
			f.Disabled = true
		}), signer.SubsystemStateDisabled},
		{"engine parked to free memory", with(func(f *inferenceSubsystemFacts) {
			f.Parked = true
		}), signer.SubsystemStateStopped},
		{"no engine at all", with(func(f *inferenceSubsystemFacts) {
			f.UsableEngine = false
		}), signer.SubsystemStateNoEngine},
		{"engine restarting", with(func(f *inferenceSubsystemFacts) {
			f.EngineState = infruntime.StateStarting
		}), signer.SubsystemStateStarting},
		{"engine down", with(func(f *inferenceSubsystemFacts) {
			f.EngineState = infruntime.StateFailed
		}), signer.SubsystemStateEngineFailed},

		// #310: the latch outlives the live reading, so a Stop() that
		// overwrote StateFailed must not let a permanently-dead engine read
		// as ready just because its model is on disk.
		{"failure latched but live reading recovered", with(func(f *inferenceSubsystemFacts) {
			f.FailureLatched = true
		}), signer.SubsystemStateEngineFailed},

		// waired-agent#1026: both of these fell through to ready whenever
		// the active model happened to be on disk. NotStarted is what a
		// freshly built adapter reports, and bootstrapVLLM builds one on
		// every attempt — so a host in a start-fail loop announced itself
		// ready between attempts.
		{"engine stopped and not parked", with(func(f *inferenceSubsystemFacts) {
			f.EngineState = infruntime.StateStopped
		}), signer.SubsystemStateStarting},
		{"engine never started", with(func(f *inferenceSubsystemFacts) {
			f.EngineState = infruntime.StateNotStarted
		}), signer.SubsystemStateStarting},
		// ORDER: the latch is the stronger fact. Stop() overwrites
		// StateFailed with StateStopped, so without the ordering a latched
		// engine that was bounced would read "starting" forever.
		{"stopped after a latch is still a failure", with(func(f *inferenceSubsystemFacts) {
			f.EngineState = infruntime.StateStopped
			f.FailureLatched = true
		}), signer.SubsystemStateEngineFailed},

		{"no model chosen", with(func(f *inferenceSubsystemFacts) {
			f.HasActive = false
			f.ModelKnown = false
			f.ModelState = ""
		}), signer.SubsystemStateAwaitingModel},
		{"model chosen but no catalog row", with(func(f *inferenceSubsystemFacts) {
			f.ModelKnown = false
			f.ModelState = ""
		}), signer.SubsystemStateAwaitingModel},
		{"download failed", with(func(f *inferenceSubsystemFacts) {
			f.ModelState = catalog.ModelStateFailed
		}), signer.SubsystemStatePullFailed},
		{"downloading", with(func(f *inferenceSubsystemFacts) {
			f.ModelState = catalog.ModelStateDownloading
		}), signer.SubsystemStateLoading},
		{"queued", with(func(f *inferenceSubsystemFacts) {
			f.ModelState = catalog.ModelStateQueued
		}), signer.SubsystemStateLoading},
		{"verifying", with(func(f *inferenceSubsystemFacts) {
			f.ModelState = catalog.ModelStateVerifying
		}), signer.SubsystemStateLoading},

		// A host with no ollama adapter — a vLLM host, or a provider built
		// without one — asks neither engine question. Before the extraction
		// this was the `p.ollama != nil` guard on each arm; it has to keep
		// falling through to the model axis rather than reading as a fault.
		{"no ollama adapter to ask", with(func(f *inferenceSubsystemFacts) {
			f.EngineState = ""
		}), signer.SubsystemStateReady},

		// PRODUCT CONTRACT (waired-agent#1075): a bootstrap that refused
		// before it built an adapter is not "ready". It reaches this arm
		// with the same empty EngineState as the row above — the
		// difference is that a reason was recorded, and subsystemFacts
		// only records one when there is no adapter to ask.
		{"the engine bootstrap refused", with(func(f *inferenceSubsystemFacts) {
			f.EngineState = ""
			f.EngineUnavailable = "no vLLM-capable model selected"
		}), signer.SubsystemStateEngineFailed},

		// PRODUCT CONTRACT (waired-agent#1298): the shape a real vLLM host
		// arrives in. hasUsableEngine is decided from the REGISTERED
		// adapters and only ollama is registered before a bootstrap
		// succeeds, so a host whose venv is installed and whose bootstrap
		// then refused reports UsableEngine=false — and the no_engine arm
		// used to answer first, which is how `waired init` ended on the
		// success box with exit 0 while local inference was down.
		{"the bootstrap refused and no adapter was ever registered", with(func(f *inferenceSubsystemFacts) {
			f.UsableEngine = false
			f.EngineState = ""
			f.EngineUnavailable = "venv not ready; local inference unavailable"
		}), signer.SubsystemStateEngineFailed},

		// The other half of the same guard: with no reason recorded,
		// "no engine" is still the answer. An engine install in flight
		// reports exactly this, and the terminal's grace depends on it.
		{"no engine and no reason recorded", with(func(f *inferenceSubsystemFacts) {
			f.UsableEngine = false
			f.EngineState = ""
		}), signer.SubsystemStateNoEngine},

		// PRODUCT CONTRACT (waired-agent#1298): the state the wizard now
		// parks every vLLM install in — the venv is on this host, the
		// engine is deliberately not started, and nobody has chosen a
		// model yet. It answered `no_engine`, so `waired status` told an
		// operator who was at that moment looking at the model picker
		// that there was no engine and to set one up with `waired init`.
		{"engine installed, nothing chosen, no adapter built", with(func(f *inferenceSubsystemFacts) {
			f.UsableEngine = false
			f.EngineInstalledNoAdapter = true
			f.EngineState = ""
			f.HasActive, f.ModelKnown = false, false
		}), signer.SubsystemStateAwaitingModel},

		// The same host once a model IS chosen and the bootstrap has not
		// built the adapter yet. `ready` is what this used to fall
		// through to, on a machine where nothing was serving.
		{"engine installed, model chosen, no adapter built", with(func(f *inferenceSubsystemFacts) {
			f.UsableEngine = false
			f.EngineInstalledNoAdapter = true
			f.EngineState = ""
		}), signer.SubsystemStateStarting},

		// A recorded refusal still outranks it: the engine is installed
		// AND it said why it would not start, and that is the more
		// specific answer.
		{"engine installed, no adapter, and a refusal recorded", with(func(f *inferenceSubsystemFacts) {
			f.UsableEngine = false
			f.EngineInstalledNoAdapter = true
			f.EngineState = ""
			f.EngineUnavailable = "venv not ready; local inference unavailable"
		}), signer.SubsystemStateEngineFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := subsystemState(tc.facts); got != tc.want {
				t.Errorf("subsystemState() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The arms are ordered most-decisive first, and two of those orderings are
// deliberate rather than incidental. Pinning them here because a reorder
// compiles, passes every single-fact case above, and changes what an
// operator is told about a machine.
func TestSubsystemState_ArmOrder(t *testing.T) {
	// The operator's pause wins over a crashed engine: someone who turned
	// inference off should not be sent looking for a fault.
	if got := subsystemState(inferenceSubsystemFacts{
		Disabled: true, UsableEngine: true, EngineState: infruntime.StateFailed,
	}); got != signer.SubsystemStateDisabled {
		t.Errorf("paused + crashed engine = %q, want %q", got, signer.SubsystemStateDisabled)
	}

	// The engine axis wins over the model axis: a node whose engine is down
	// is not "downloading", even when it also happens to be mid-pull.
	if got := subsystemState(inferenceSubsystemFacts{
		UsableEngine: true, EngineState: infruntime.StateFailed,
		HasActive: true, ModelKnown: true, ModelState: catalog.ModelStateDownloading,
	}); got != signer.SubsystemStateEngineFailed {
		t.Errorf("crashed engine + mid-pull = %q, want %q", got, signer.SubsystemStateEngineFailed)
	}

	// A live adapter reading is the more specific answer, so a refusal
	// recorded on some earlier boot must not outrank it. subsystemFacts
	// already guarantees the two are never both set; this pins the arm
	// order that makes it safe if that guarantee is ever relaxed
	// (waired-agent#1075).
	if got := subsystemState(inferenceSubsystemFacts{
		UsableEngine: true, EngineState: infruntime.StateStarting,
		EngineUnavailable: "no vLLM-capable model selected",
		HasActive:         true, ModelKnown: true, ModelState: catalog.ModelStateReady,
	}); got != signer.SubsystemStateStarting {
		t.Errorf("starting engine + a stale refusal = %q, want %q", got, signer.SubsystemStateStarting)
	}

	// The operator's own stop still wins over it, for the reason it wins
	// over a crash: a setting is not a fault.
	if got := subsystemState(inferenceSubsystemFacts{
		Disabled: true, EngineUnavailable: "no vLLM-capable model selected",
	}); got != signer.SubsystemStateDisabled {
		t.Errorf("disabled + refusal = %q, want %q", got, signer.SubsystemStateDisabled)
	}

	// A recorded refusal outranks "no engine here" (waired-agent#1298).
	// The two travel together on every host that refuses before building
	// an adapter, because the adapter is what hasUsableEngine counts, so
	// this precedence is not hypothetical the way the two above are — it
	// is the ordinary state of a vLLM host whose bootstrap gave up.
	if got := subsystemState(inferenceSubsystemFacts{
		UsableEngine:      false,
		EngineUnavailable: "venv not ready; local inference unavailable",
	}); got != signer.SubsystemStateEngineFailed {
		t.Errorf("no registered adapter + refusal = %q, want %q", got, signer.SubsystemStateEngineFailed)
	}
}

// Everything subsystemState can return has to survive the control plane's
// intake, which validates against signer.IsValidSubsystemState. A value
// that fails there would 400 the whole inference push, not just the field,
// and the device would go stale in every peer's mesh.
func TestSubsystemState_AllOutputsAreValidOnTheWire(t *testing.T) {
	all := []inferenceSubsystemFacts{
		{UsableEngine: true, EngineState: infruntime.StateReady, HasActive: true, ModelKnown: true, ModelState: catalog.ModelStateReady},
		{Disabled: true},
		{Parked: true, UsableEngine: true},
		{},
		{UsableEngine: true, EngineState: infruntime.StateStarting},
		{UsableEngine: true, EngineState: infruntime.StateFailed},
		{UsableEngine: true, FailureLatched: true},
		{UsableEngine: true, HasActive: true, ModelKnown: true, ModelState: catalog.ModelStateFailed},
		{UsableEngine: true, HasActive: true, ModelKnown: true, ModelState: catalog.ModelStateDownloading},
	}
	for _, f := range all {
		got := subsystemState(f)
		if !signer.IsValidSubsystemState(got) {
			t.Errorf("subsystemState(%+v) = %q, which the CP validator rejects", f, got)
		}
	}
}
