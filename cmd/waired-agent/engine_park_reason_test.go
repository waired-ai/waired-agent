package main

import (
	"context"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestSubsystemState_TellsTheTwoStopsApart is waired-agent#1464 at the
// seam that reaches NAVI and every other surface.
//
// PRODUCT CONTRACT (owner, 2026-09-21): an engine this product stopped
// because the computer ran out of memory is NOT the operator's `stopped`.
// That value's own documentation says "the operator hard-stopped the engine",
// so reporting it here would send someone looking for a setting nobody
// changed. It is `engine_failed`, which NAVI already renders as a fault —
// and reusing it is why this needed no new wire value.
func TestSubsystemState_TellsTheTwoStopsApart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts inferenceSubsystemFacts
		want  string
	}{
		{
			name:  "the operator hard-stopped a usable engine",
			facts: inferenceSubsystemFacts{Parked: true},
			want:  signer.SubsystemStateStopped,
		},
		{
			name:  "CONTRACT: this product stopped it because memory ran out",
			facts: inferenceSubsystemFacts{Parked: true, ParkedByError: true},
			want:  signer.SubsystemStateEngineFailed,
		},
		{
			// The operator's pause outranks both, unchanged: reporting a
			// fault on a machine that was told not to serve would send
			// someone looking for a problem that is a setting.
			name:  "a disabled host is disabled whatever the park says",
			facts: inferenceSubsystemFacts{Disabled: true, Parked: true, ParkedByError: true},
			want:  signer.SubsystemStateDisabled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := subsystemState(tc.facts); got != tc.want {
				t.Errorf("subsystemState() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParkedBecause_TheLatchIsTheAuthority: a cause must never outlive the
// park it describes.
//
// `waired inference engine start` calls Unpark on the adapter directly, so
// the cause can be left behind. Reading the two together is what stops an
// engine that is up from claiming it is stopped for memory — which would
// make every surface lie and would make the next model switch "resume" an
// engine that was never held off.
func TestParkedBecause_TheLatchIsTheAuthority(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")

	if got := p.parkedBecause(); got != parkCauseNone {
		t.Errorf("a running engine reports %v, want none", got)
	}

	// A cause recorded while the engine is UP is not a claim about anything.
	p.noteParked(parkCauseOutOfMemory)
	if got := p.parkedBecause(); got != parkCauseNone {
		t.Errorf("an engine that is not parked reports %v; the latch is the authority", got)
	}

	// And a park nobody attributed reads as the operator's, which is the
	// behaviour before this existed.
	p.noteParked(parkCauseNone)
	if err := p.ollama.Park(context.Background()); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if got := p.parkedBecause(); got != parkCauseOperator {
		t.Errorf("an unattributed park reports %v, want operator", got)
	}
}

// TestResumeAfterOutOfMemory_LeavesTheOperatorsStopAlone is the other half
// of the contract, and the one that would be easy to get wrong.
//
// A person who hard-stopped the engine did not ask for it back because they
// changed model. Only a memory park is released.
func TestResumeAfterOutOfMemory_LeavesTheOperatorsStopAlone(t *testing.T) {
	t.Run("a memory park is released", func(t *testing.T) {
		e := &warmEngine{}
		p := warmProvider(t, e, "model-a", "a:q4")
		p.parkForOutOfMemory(context.Background(), "ran out of memory")
		if !p.ollama.IsParked() {
			t.Fatal("precondition: the engine was not parked")
		}
		if got := p.parkedBecause(); got != parkCauseOutOfMemory {
			t.Fatalf("cause = %v, want out_of_memory", got)
		}

		if !p.resumeAfterOutOfMemory("a different model was chosen") {
			t.Error("resume reported that it did nothing")
		}
		if p.ollama.IsParked() {
			t.Error("the engine is still held off after the reason was released")
		}
	})

	t.Run("CONTRACT: the operator's stop is not", func(t *testing.T) {
		e := &warmEngine{}
		p := warmProvider(t, e, "model-a", "a:q4")
		p.noteParked(parkCauseOperator)
		if err := p.ollama.Park(context.Background()); err != nil {
			t.Fatalf("Park: %v", err)
		}

		if p.resumeAfterOutOfMemory("a different model was chosen") {
			t.Error("released a stop the operator asked for")
		}
		if !p.ollama.IsParked() {
			t.Error("the operator's hard stop was undone")
		}
		if got := p.parkedBecause(); got != parkCauseOperator {
			t.Errorf("cause = %v, want it still to be the operator's", got)
		}
	})

	t.Run("CONTRACT: a memory failure does not overwrite the operator's stop", func(t *testing.T) {
		e := &warmEngine{}
		p := warmProvider(t, e, "model-a", "a:q4")
		p.noteParked(parkCauseOperator)
		if err := p.ollama.Park(context.Background()); err != nil {
			t.Fatalf("Park: %v", err)
		}

		p.parkForOutOfMemory(context.Background(), "ran out of memory")

		if got := p.parkedBecause(); got != parkCauseOperator {
			t.Errorf("cause = %v: a memory failure relabelled the operator's stop, "+
				"which would then clear itself on the next model switch", got)
		}
	})
}

// TestParkForOutOfMemory_TakesThisHostOutOfTheMesh is the requirement that
// matters to everyone else on the account: a computer that cannot load its
// model must stop being chosen.
//
// PRODUCT CONTRACT (owner, 2026-09-21). It holds through the existing latch
// rather than anything new — EngineReady reads the park, and ServingTerms
// reads EngineReady as its first term — and this test is here so that stays
// true.
func TestParkForOutOfMemory_TakesThisHostOutOfTheMesh(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")

	ready, _ := p.EngineReady()
	if !ready {
		t.Fatal("precondition: this host was not serving-ready to begin with")
	}

	p.parkForOutOfMemory(context.Background(), "ran out of memory")

	ready, _ = p.EngineReady()
	if ready {
		t.Error("a host that cannot load its model still advertises capacity; " +
			"peers would keep choosing it and every request would 503")
	}
}

// TestEngineStoppedReason_NamesOneWayOut is the owner's ruling of
// 2026-09-21: do not put a state on the screen with no next action beside
// it.
//
// PRODUCT CONTRACT. `waired status` shows `engine_failed` for a memory stop,
// and that word alone tells a reader nothing they can act on. It must carry
// one remedy — and only one, because the other three would bury the one that
// applies to almost everybody.
func TestEngineStoppedReason_NamesOneWayOut(t *testing.T) {
	t.Run("a memory stop says what to do", func(t *testing.T) {
		e := &warmEngine{}
		p := warmProvider(t, e, "model-a", "a:q4")
		p.parkForOutOfMemory(context.Background(), "ran out of memory")

		got := p.engineStoppedReason()
		if got == "" {
			t.Fatal("a stopped engine reported no reason; the status line would be a dead end")
		}
		if !strings.Contains(got, "Choose a different model") {
			t.Errorf("the reason names no action a reader can take: %q", got)
		}
		// One remedy, not the list. Naming the others here would bury it.
		for _, tooMuch := range []string{"driver", "turn inference on", "engine start"} {
			if strings.Contains(strings.ToLower(got), tooMuch) {
				t.Errorf("the status line lists more than one way out (%q): %q", tooMuch, got)
			}
		}
	})

	t.Run("CONTRACT: an engine the operator stopped is told nothing", func(t *testing.T) {
		e := &warmEngine{}
		p := warmProvider(t, e, "model-a", "a:q4")
		p.noteParked(parkCauseOperator)
		if err := p.ollama.Park(context.Background()); err != nil {
			t.Fatalf("Park: %v", err)
		}
		if got := p.engineStoppedReason(); got != "" {
			t.Errorf("told the operator how to undo their own stop: %q", got)
		}
	})

	t.Run("a running engine has nothing to explain", func(t *testing.T) {
		e := &warmEngine{}
		p := warmProvider(t, e, "model-a", "a:q4")
		if got := p.engineStoppedReason(); got != "" {
			t.Errorf("a running engine explained itself: %q", got)
		}
	})
}

// TestPublishedEngineStoppedCause is waired-agent#1480: the control plane
// gets a code for why the engine is not running, so it can write its own
// words instead of guessing from `engine_failed`.
//
// PRODUCT CONTRACT: silence for the three situations nothing here decided.
// `engine_failed` covers four — a load that ran out of memory, a crashed
// runner, an exhausted recovery budget, an engine that never came up — and
// naming a remedy for the wrong one is worse than naming none. Only the one
// this product decided may speak.
func TestPublishedEngineStoppedCause(t *testing.T) {
	t.Run("a memory stop names itself", func(t *testing.T) {
		e := &warmEngine{}
		p := warmProvider(t, e, "model-a", "a:q4")
		p.parkForOutOfMemory(context.Background(), "ran out of memory")
		if got := p.PublishedEngineStoppedCause(); got != signer.EngineStoppedCauseOutOfMemory {
			t.Errorf("cause = %q, want %q", got, signer.EngineStoppedCauseOutOfMemory)
		}
	})

	t.Run("the operator's stop names itself", func(t *testing.T) {
		e := &warmEngine{}
		p := warmProvider(t, e, "model-a", "a:q4")
		p.noteParked(parkCauseOperator)
		if err := p.ollama.Park(context.Background()); err != nil {
			t.Fatalf("Park: %v", err)
		}
		if got := p.PublishedEngineStoppedCause(); got != signer.EngineStoppedCauseOperator {
			t.Errorf("cause = %q, want %q", got, signer.EngineStoppedCauseOperator)
		}
	})

	t.Run("a running engine says nothing", func(t *testing.T) {
		e := &warmEngine{}
		p := warmProvider(t, e, "model-a", "a:q4")
		if got := p.PublishedEngineStoppedCause(); got != "" {
			t.Errorf("cause = %q on a running engine, want empty", got)
		}
	})

	t.Run("CONTRACT: nothing is claimed about the failures this product did not cause", func(t *testing.T) {
		// A crashed runner reaches the same `engine_failed` on the wire.
		// Nothing parked it, so parkedBecause is none and the cause stays
		// empty — which is what keeps a reader from offering "choose a
		// different model" to someone whose engine crashed.
		e := &warmEngine{}
		p := warmProvider(t, e, "model-a", "a:q4")
		if p.ollama.IsParked() {
			t.Fatal("precondition: the engine was parked")
		}
		if got := p.PublishedEngineStoppedCause(); got != "" {
			t.Errorf("cause = %q for a failure nothing here decided; a reader would name "+
				"a remedy that does not apply", got)
		}
	})

	// Every value this can emit has to be one the boundary validator
	// accepts, or a newer agent would be rejected by the control plane.
	t.Run("every emitted value is valid on the wire", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			set  func(*agentInferenceProvider)
		}{
			{"memory", func(p *agentInferenceProvider) {
				p.parkForOutOfMemory(context.Background(), "ran out of memory")
			}},
			{"operator", func(p *agentInferenceProvider) {
				p.noteParked(parkCauseOperator)
				_ = p.ollama.Park(context.Background())
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := &warmEngine{}
				p := warmProvider(t, e, "model-a", "a:q4")
				tc.set(p)
				got := p.PublishedEngineStoppedCause()
				if !signer.IsValidEngineStoppedCause(got) {
					t.Errorf("emitted %q, which the wire validator rejects", got)
				}
			})
		}
	})
}
