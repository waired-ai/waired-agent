package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// refusedVLLMHost is the shape waired-agent#1075 was found in: the host
// serves with vLLM, the bootstrap refused before it built an adapter, and
// a model row left by an earlier run is still on disk and ready.
func refusedVLLMHost(t *testing.T, reason string) *agentInferenceProvider {
	t.Helper()
	a := newTestAdapter(t)
	p := startFailProvider(t, a, time.Now)
	p.setServingEngine(catalog.RuntimeVLLM)
	seedActiveReadyModel(t, p)
	p.refuseEngineBootstrap(reason)
	return p
}

// TestStatus_RefusedBootstrapIsNotReady is the defect, at the level it was
// observed on real hardware: subsystem_state said `ready` on a host with no
// local inference at all.
//
// PRODUCT CONTRACT (waired-agent#1075).
func TestStatus_RefusedBootstrapIsNotReady(t *testing.T) {
	const reason = "no vLLM-capable model selected — set a preferred model that ships a" +
		" vllm/safetensors variant (e.g. gpt-oss-20b)"
	p := refusedVLLMHost(t, reason)

	st := p.Status(context.Background())
	if st.SubsystemState != signer.SubsystemStateEngineFailed {
		t.Fatalf("subsystem_state = %q, want %q", st.SubsystemState, signer.SubsystemStateEngineFailed)
	}

	// The reason has to reach runtimes[], because that is what every
	// surface reads: `waired status`'s warning line, `waired runtimes
	// ls`/`status`, the wizard's terminal arm through engineFailureDetail,
	// and the tray through servingRuntime. The registry holds ollama alone
	// on this host — the vLLM adapter is registered in the same breath as
	// it is built — so the row is synthesised.
	row, ok := st.Runtimes[catalog.RuntimeVLLM]
	if !ok {
		t.Fatal("no runtimes[vllm] row, so no surface can say why")
	}
	if row.State != infruntime.StateFailed {
		t.Errorf("runtimes[vllm].state = %q, want %q", row.State, infruntime.StateFailed)
	}
	if !strings.Contains(row.LastError, "no vLLM-capable model selected") {
		t.Errorf("runtimes[vllm].last_error = %q, want the refusal", row.LastError)
	}
}

// TestStatus_RefusedBootstrapDoesNotClaimAReadyEndpoint is the other half
// of the same lie. An endpoint's recorded state is written when the WEIGHTS
// land, not when the engine serves, and endpointState leaves the record
// alone when the runtime has no entry — so the endpoint claimed "ready" on
// a host that had never started an engine.
func TestStatus_RefusedBootstrapDoesNotClaimAReadyEndpoint(t *testing.T) {
	p := refusedVLLMHost(t, "vllm venv not active")
	if err := p.store.Update(func(s *catalog.State) {
		if s.Endpoints == nil {
			s.Endpoints = map[string]catalog.EndpointState{}
		}
		s.Endpoints["ep_local_vllm_x"] = catalog.EndpointState{
			Runtime: catalog.RuntimeVLLM, ModelID: "x", State: "ready",
		}
	}); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}

	st := p.Status(context.Background())
	if len(st.ActiveEndpoints) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(st.ActiveEndpoints))
	}
	if got := st.ActiveEndpoints[0].State; got == "ready" {
		t.Errorf("endpoint state = %q on a host whose engine never started", got)
	}
}

// TestStatus_ARunningEngineOutranksAnOldRefusal: the refusal is read only
// where servingAdapter() is nil, so a value left over from a refused boot
// cannot describe an engine that has since come up. The serving states
// match no engine arm at all, so without this the mistake would show up
// only on healthy hosts.
func TestStatus_ARunningEngineOutranksAnOldRefusal(t *testing.T) {
	p := refusedVLLMHost(t, "no vLLM-capable model selected")
	p.setVLLM(&recordingAdapter{name: "vllm", health: infruntime.StateReady})

	st := p.Status(context.Background())
	if st.SubsystemState != signer.SubsystemStateReady {
		t.Errorf("subsystem_state = %q, want %q on a host whose engine is up",
			st.SubsystemState, signer.SubsystemStateReady)
	}
	if row, ok := st.Runtimes[catalog.RuntimeVLLM]; ok && row.LastError != "" {
		t.Errorf("a stale refusal reached runtimes[vllm].last_error: %q", row.LastError)
	}
}

// TestServingFailureReason_IsOneLineAndFollowsTheLatch pins what `waired
// doctor` is handed. LastErr is the give-up sentence, the raw error and up
// to 4 KiB of engine log; the first line is the part that names the cause.
func TestServingFailureReason_IsOneLineAndFollowsTheLatch(t *testing.T) {
	ctx := context.Background()

	t.Run("no adapter falls back to the refusal", func(t *testing.T) {
		p := refusedVLLMHost(t, "vllm venv not active under /var/lib/waired\nsecond line")
		if got := p.servingFailureReason(ctx); got != "vllm venv not active under /var/lib/waired" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a latched engine reports the first line of its reason", func(t *testing.T) {
		// The latch is the copy with the right lifetime: Stop() clears
		// Health.LastErr with no giveUp guard (#310), and this is the
		// state a host that has given up actually sits in.
		a := &latchRecorder{health: infruntime.StateStarting}
		p := vllmServingProvider(t, a)
		a.LatchFailed("another program is already listening on 127.0.0.1:9479\n" +
			"engine failed to start 4 times within 5m0s")
		if got := p.servingFailureReason(ctx); got != "another program is already listening on 127.0.0.1:9479" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a healthy engine says nothing", func(t *testing.T) {
		p := vllmServingProvider(t, &recordingAdapter{name: "vllm", health: infruntime.StateReady})
		if got := p.servingFailureReason(ctx); got != "" {
			t.Errorf("got %q, want silence", got)
		}
	})
}
