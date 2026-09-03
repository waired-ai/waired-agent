//go:build linux

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// vllmStartPlan and the answer an explicit start gives (waired-agent#1170).
//
// Linux-only because the resolver is: the venv, the HF puller and the
// safetensors target exist on no other leg. The DECISION these feed — clear
// the latches, then answer with the reason — is asserted on every leg by
// TestEngineController_VLLMStartClearsTheLatchesAndAnswers.

// TestVLLMStartPlan_RefusalsAreOneStringInOnePlace pins the three reasons a
// vLLM start cannot begin. bootstrapVLLM records these into a provider field
// only the next bootstrap clears, so before this the operator's own start
// re-ran the bootstrap, hit the same reason, and answered "engine start ok."
func TestVLLMStartPlan_RefusalsAreOneStringInOnePlace(t *testing.T) {
	t.Run("no venv", func(t *testing.T) {
		p := vllmTestProvider(t)
		_, _, _, _, err := p.vllmStartPlan()
		if err == nil || !strings.Contains(err.Error(), "venv not ready") {
			t.Errorf("vllmStartPlan with no venv = %v, want a venv refusal", err)
		}
	})

	t.Run("no vLLM-capable model selected", func(t *testing.T) {
		p := vllmTestProvider(t)
		fakeVLLMVenv(t, p.stateDir)
		// The wizard's host as it actually was: the venv has landed and the
		// only model anything knows about is the bundled ollama auto-pick.
		p.manifests = vllmSwapManifests()
		p.cfg.BundledModelID = "ollama-only"
		_, _, _, _, err := p.vllmStartPlan()
		if err == nil || !strings.Contains(err.Error(), "no vLLM-capable model selected") {
			t.Errorf("vllmStartPlan with an ollama-only bundled model = %v,"+
				" want the no-vLLM-capable-model refusal", err)
		}
	})

	t.Run("the weights are still downloading", func(t *testing.T) {
		p := vllmTestProvider(t)
		fakeVLLMVenv(t, p.stateDir)
		if _, joined := p.beginPull(&pullJob{modelID: "gpt-oss-20b"}); joined {
			t.Fatal("precondition: the first claim must not join")
		}
		_, _, _, _, err := p.vllmStartPlan()
		if err == nil || !strings.Contains(err.Error(), "still downloading") {
			t.Errorf("vllmStartPlan while a pull is in flight = %v, want the still-downloading refusal\n"+
				"(the bootstrap's own fetch does not pass through beginPull, so proceeding here\n"+
				" runs a second `hf download` into the same directory)", err)
		}
	})

	t.Run("a host that can start", func(t *testing.T) {
		p := vllmTestProvider(t)
		fakeVLLMVenv(t, p.stateDir)
		puller, python, m, v, err := p.vllmStartPlan()
		if err != nil {
			t.Fatalf("vllmStartPlan on a ready host = %v, want nil", err)
		}
		if puller == nil || python == "" {
			t.Errorf("plan returned puller=%v python=%q; the spawn needs both", puller, python)
		}
		if m.ModelID != "gpt-oss-20b" || v.VariantID != "mxfp4-safetensors" {
			t.Errorf("plan target = %s/%s, want gpt-oss-20b/mxfp4-safetensors", m.ModelID, v.VariantID)
		}
	})
}

// TestEngineController_VLLMStartDispatches is the dispatch half of the
// contract #946/#1110 set: the start goes through requestEngineStart — the
// only path that can build an adapter when there is none — rather than
// EnsureRunning on the adapter it already has.
//
// Local inference reads "off" from the second read on, so the dispatched
// bootstrap declines at its first gate and records why. That is the
// observable, and it costs no engine. (The first read answers "on" because
// StartEngine refuses outright when the toggle is off, waired-agent#964, and
// this test is about what happens after it decides to proceed.)
func TestEngineController_VLLMStartDispatches(t *testing.T) {
	vllm := &recordingAdapter{name: "vllm", health: infruntime.StateStopped}
	p := vllmTestProvider(t)
	p.setVLLM(vllm)
	fakeVLLMVenv(t, p.stateDir)
	p.agentCtx = context.Background()
	p.logger = testLogger()
	var reads int
	p.isInferenceDisabled = func() bool { reads++; return reads > 1 }
	ec := newEngineController(context.Background(), p, nil)

	if err := ec.StartEngine(context.Background()); err != nil {
		t.Fatalf("StartEngine on a host whose preconditions all resolve: %v", err)
	}
	waitForStartDecline(t, p, "the start to reach startEngineAndBootstrap")
	if n := vllm.ensuring.Load(); n != 0 {
		t.Errorf("the controller called EnsureRunning on the current adapter %d times; "+
			"the vLLM start has to re-resolve the venv, weights and tuning", n)
	}
	// A start that can begin clears the stale refusal rather than leaving
	// every surface quoting the last attempt's reason while this one runs.
	if got := p.engineBootstrapRefused(); got != "" {
		t.Errorf("engineBootstrapRefused = %q after an accepted start, want empty", got)
	}
}

// TestEngineController_VLLMStartAnswersWithTheRefusal is the CLI-facing half:
// the sentence the person who asked reads.
//
// PRODUCT CONTRACT (waired-agent#1170). ErrEngineStartRefused is what the
// management handler keys on to answer 409, which cmd/waired already parses
// into the printed message — so the fix reaches the CLI without touching it.
func TestEngineController_VLLMStartAnswersWithTheRefusal(t *testing.T) {
	p := vllmTestProvider(t)
	fakeVLLMVenv(t, p.stateDir)
	p.manifests = vllmSwapManifests()
	p.cfg.BundledModelID = "ollama-only"
	p.agentCtx = context.Background()
	p.logger = testLogger()
	ec := newEngineController(context.Background(), p, nil)

	err := ec.StartEngine(context.Background())
	if !errors.Is(err, management.ErrEngineStartRefused) {
		t.Fatalf("StartEngine = %v, want a refusal wrapping %v", err, management.ErrEngineStartRefused)
	}
	if !strings.Contains(err.Error(), "no vLLM-capable model selected") {
		t.Errorf("StartEngine err = %q, want it to name the cause", err)
	}
	if p.servingEngine() != catalog.RuntimeVLLM {
		t.Fatal("precondition: this test is about the vLLM arm")
	}
}
