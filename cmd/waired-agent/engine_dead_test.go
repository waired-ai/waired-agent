package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The compile-time half of TestServingEngineDead below: the OLLAMA adapter
// really does implement the latch that servingEngineDead asserts for.
//
// It reaches that method through an interface assertion, which fails OPEN —
// so a production adapter that stopped implementing it would go on
// advertising a given-up host while every fake in this package still passed.
// The vLLM half lives in inference_vllm_linux_test.go, where the type exists.
var _ interface{ FailureLatched() bool } = (*infruntime.OllamaAdapter)(nil)

// bareAdapter implements infruntime.Adapter and NOTHING else — not the latch,
// not ClearFailure. recordingAdapter cannot stand in for this: it answers
// FailureLatched() false, so it exercises the assertion's hit path, not its
// miss path. internal/runtime/peer.Adapter is the production shape this
// models: it has the five Adapter methods and no latch at all.
type bareAdapter struct{ health string }

func (a *bareAdapter) Name() string                        { return "bare" }
func (a *bareAdapter) BaseURL() string                     { return "http://127.0.0.1:9479" }
func (a *bareAdapter) EnsureRunning(context.Context) error { return nil }
func (a *bareAdapter) Stop(context.Context) error          { return nil }
func (a *bareAdapter) Health(context.Context) infruntime.Health {
	return infruntime.Health{State: a.health}
}

// TestServingEngineDead is the mesh's "stop advertising this node" predicate.
//
// PRODUCT CONTRACT (waired-agent#29, #1138): the line is RECOVERABILITY, not
// severity. A boot in progress, a refused bootstrap and an exhausted start
// budget all keep the probe's own verdict, because EnsureRunning will try
// again on the next trigger (noteEngineStartExhausted's doc in
// engine_giveup.go). The give-up latch is the one state that does not: only
// an explicit start clears it (engineController.StartEngine), so waiting
// provably will not help.
func TestServingEngineDead(t *testing.T) {
	ctx := context.Background()

	// The row waired-agent#1138 is about, and the only one that needs a
	// sequence rather than a state. Stop() assigns the whole Health struct with
	// no give-up guard (its a.proc == nil branch) while the latch survives, so
	// a health-state-only predicate reads "stopped" and answers false — and the
	// node goes on advertising capacity to peers.
	//
	// It matters because in this state something is still ANSWERING the port:
	// an adopted orphan Stop() never killed, or a foreign engine that took the
	// waired-owned port (#943). The HTTP probe gets its 200 and cannot tell.
	t.Run("the latch outlives the stop", func(t *testing.T) {
		a := &latchRecorder{health: infruntime.StateStarting}
		p := vllmServingProvider(t, a)
		a.LatchFailed("another program is already listening on 127.0.0.1:9479\n" +
			"engine failed to start 4 times within 5m0s")
		if err := a.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if got := a.Health(ctx).State; got != infruntime.StateStopped {
			t.Fatalf("precondition: health = %q, want %q — the fake must model "+
				"the real Stop's whole-struct overwrite", got, infruntime.StateStopped)
		}
		if !p.servingEngineDead(ctx) {
			t.Error("a latched engine that was then stopped must stop this node " +
				"advertising: waiting cannot help, only an explicit start clears it")
		}
	})

	// A latch with no stop after it. Redundant with the health arm today
	// (LatchFailed writes StateFailed too), and kept because that overlap is
	// exactly what made the defect above invisible.
	t.Run("a latched engine is dead before anything stops it", func(t *testing.T) {
		a := &latchRecorder{health: infruntime.StateStarting}
		p := vllmServingProvider(t, a)
		a.LatchFailed("engine repeatedly crashed")
		if !p.servingEngineDead(ctx) {
			t.Error("want dead")
		}
	})

	// The states that must NOT flip it: the predicate also degrades the
	// `waired claude` wrapper and fails the transparent proxy open, so firing
	// it on a host that is merely still starting is its own defect.
	for _, tc := range []struct {
		name   string
		health string
		want   bool
	}{
		{"a failed engine is dead", infruntime.StateFailed, true},
		{"a boot in progress is not", infruntime.StateStarting, false},
		{"an engine that never started is not", infruntime.StateNotStarted, false},
		{"a healthy engine is not", infruntime.StateReady, false},
		{"a stopped engine with no latch is not", infruntime.StateStopped, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := vllmServingProvider(t, &recordingAdapter{name: "vllm", health: tc.health})
			if got := p.servingEngineDead(ctx); got != tc.want {
				t.Errorf("servingEngineDead() = %v, want %v for health %q",
					got, tc.want, tc.health)
			}
		})
	}

	// The fail-open row. internal/runtime/peer.Adapter really does not
	// implement the latch, so the assertion can genuinely miss, and an adapter
	// that cannot answer must keep the probe's verdict rather than be assumed
	// dead.
	t.Run("an adapter with no latch method is fail-open", func(t *testing.T) {
		p := vllmServingProvider(t, &bareAdapter{health: infruntime.StateStopped})
		if p.servingEngineDead(ctx) {
			t.Error("an adapter that cannot answer must not be assumed dead")
		}
	})

	// No adapter at all. The refusal is recoverable — EnsureRunning tries
	// again on the next trigger, which is what adopts an engine installed
	// after boot — so it belongs on the fail-open side even though the
	// reason is known (noteEngineStartExhausted's doc in engine_giveup.go).
	t.Run("a refused bootstrap keeps the probe's verdict", func(t *testing.T) {
		p := refusedVLLMHost(t, "another program is already listening on 127.0.0.1:9479")
		if p.servingAdapter() != nil {
			t.Fatal("precondition: this host must have no adapter")
		}
		if p.servingEngineDead(ctx) {
			t.Error("a refused bootstrap is still trying; it must not stop the node advertising")
		}
	})

	t.Run("a nil provider is fail-open", func(t *testing.T) {
		var p *agentInferenceProvider
		if p.servingEngineDead(ctx) {
			t.Error("want false")
		}
	})
}

// TestEndpointState_FollowsTheLatch: an endpoint cannot be readier than the
// engine that serves it (waired-agent#1026), and a latched engine's honest
// state is "failed" — Stop() overwrites the health snapshot with "stopped"
// while the latch survives, and endpointState is handed the whole
// RuntimeStatus with FailureLatched already on it (runtimeStatusFor fills it).
func TestEndpointState_FollowsTheLatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string
		rt       management.RuntimeStatus
		want     string
	}{
		{
			name:     "a latched engine reports failed, not the stop that erased its health",
			recorded: "ready",
			rt:       management.RuntimeStatus{State: infruntime.StateStopped, FailureLatched: true},
			want:     infruntime.StateFailed,
		},
		{
			name:     "no runtime entry leaves the record alone",
			recorded: "ready",
			rt:       management.RuntimeStatus{},
			want:     "ready",
		},
		{
			name:     "a ready engine keeps the recorded fact",
			recorded: "ready",
			rt:       management.RuntimeStatus{State: infruntime.StateReady},
			want:     "ready",
		},
		{
			name:     "a stopped engine with no latch is still stopped",
			recorded: "ready",
			rt:       management.RuntimeStatus{State: infruntime.StateStopped},
			want:     infruntime.StateStopped,
		},
		{
			name:     "a failed engine outranks the record",
			recorded: "ready",
			rt:       management.RuntimeStatus{State: infruntime.StateFailed},
			want:     infruntime.StateFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := endpointState(tc.recorded, tc.rt); got != tc.want {
				t.Errorf("endpointState(%q, %+v) = %q, want %q",
					tc.recorded, tc.rt, got, tc.want)
			}
		})
	}
}
