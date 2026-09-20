package main

import (
	"context"
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
