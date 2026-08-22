package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// waired-agent#943: the model-residency surfaces answered for whichever
// engine had an ollama adapter, which is every host — so on a vLLM host they
// reported, and changed, an engine that was not serving.
//
// Every fixture below gives the provider a LIVE ollama endpoint that would
// answer, because that is the shape the defect needs: a host with an
// unmanaged `ollama serve` on 11434 alongside a vLLM engine. The assertion
// that matters in each is not just the refusal — it is that the stranger's
// server was never called.

// strangerEngine is an ollama-shaped endpoint that is NOT this host's engine:
// it reports a resident model and counts every request it receives.
type strangerEngine struct {
	srv   *httptest.Server
	calls atomic.Int32
}

func newStrangerEngine(t *testing.T) *strangerEngine {
	t.Helper()
	s := &strangerEngine{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[{"name":"somebody-elses:70b","expires_at":"2318-11-30T12:52:47Z"}]}`))
		default:
			_, _ = w.Write([]byte(`{"models":[]}`))
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// vllmHostWithAStrangerOllama is a provider serving with vLLM whose ollama
// adapter points at an engine this host does not own.
func vllmHostWithAStrangerOllama(t *testing.T) (*agentInferenceProvider, *strangerEngine) {
	t.Helper()
	stranger := newStrangerEngine(t)
	host, port := hostPort(t, stranger.srv.URL)
	a := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: &fakeSpawner{}, HTTPClient: stranger.srv.Client(),
	})
	p := &agentInferenceProvider{ollama: a, logger: testLogger()}
	p.setServingEngine(catalog.RuntimeVLLM)
	return p, stranger
}

func TestUnloadServingModel_RefusesOnAVLLMHost(t *testing.T) {
	p, stranger := vllmHostWithAStrangerOllama(t)

	tag, err := p.UnloadServingModel(context.Background())
	if !errors.Is(err, management.ErrUnloadNotSupported) {
		t.Fatalf("UnloadServingModel = (%q, %v), want ErrUnloadNotSupported", tag, err)
	}
	// This is the defect, not the wording: the old code reached the wrong
	// engine and reported ITS answer as this host's.
	if n := stranger.calls.Load(); n != 0 {
		t.Errorf("the refusal still queried an engine this host does not serve with (%d calls)", n)
	}
	// waired#1067: a refusal has to say what to do instead.
	if !strings.Contains(err.Error(), "waired inference engine stop") {
		t.Errorf("refusal = %q, want it to name the only release valve this engine has", err)
	}
	// The refusal speaks of "the inference engine" generically: the fact is
	// not engine-specific, so no engine name is needed (waired-ai/waired#1272
	// names the engine only where the fact is).
	if !strings.Contains(err.Error(), "inference engine") {
		t.Errorf("refusal = %q, want it to say \"the inference engine\"", err)
	}

	// Negative control: the same fixture serving with ollama unloads for
	// real, so the assertions above are about the guard and not about a
	// provider that can never do anything.
	p.setServingEngine(catalog.RuntimeOllama)
	if _, err := p.UnloadServingModel(context.Background()); err != nil {
		t.Fatalf("UnloadServingModel on an ollama host = %v, want nil", err)
	}
	if n := stranger.calls.Load(); n == 0 {
		t.Error("the ollama path never reached the engine; the refusal test was vacuous")
	}
}

func TestResidency_UnsupportedOnAVLLMHost(t *testing.T) {
	p, stranger := vllmHostWithAStrangerOllama(t)

	if got, ok := p.CurrentResidency(); !ok || got != 0 {
		t.Errorf("CurrentResidency = (%s, %v), want (0, true): this engine holds the model for the life of the process",
			got, ok)
	}
	if p.ResidencySupported() {
		t.Error("ResidencySupported = true on an engine with no residency axis")
	}

	effect, err := p.ApplyResidency(context.Background(), 0)
	if err != nil {
		t.Fatalf("ApplyResidency = %v, want nil: it is not a fault, it is a host that cannot honour it", err)
	}
	if effect != management.ResidencyEffectUnsupported {
		t.Errorf("effect = %q, want %q", effect, management.ResidencyEffectUnsupported)
	}
	if n := stranger.calls.Load(); n != 0 {
		t.Errorf("the write reached an engine this host does not serve with (%d calls)", n)
	}
	// The live keep-alive of the non-serving adapter must be untouched: it
	// is another engine's setting, and writing it is how the old code turned
	// a residency change into an action on somebody else's process.
	if got := p.ollama.KeepAliveDuration(); got != infruntime.NewOllamaAdapter(infruntime.OllamaConfig{}).KeepAliveDuration() {
		t.Errorf("the non-serving adapter's keep-alive was changed to %s", got)
	}
}

// TestModelResident_OnAVLLMHost — a ready vLLM engine is resident by
// construction (waired-agent#965), and saying so is reporting a fact this
// host measured rather than assuming one: waitReady only reports ready after
// /health answers and /v1/models confirms the served model, and
// --gpu-memory-utilization holds the pool until the process exits.
//
// This INVERTS TestModelResident_NotObservedOnAVLLMHost, which asserted "not
// observed" here. That test was right about the defect it was written for —
// waired-agent#943 removed a reading of the ollama adapter's cache, which on
// a host running an unmanaged `ollama serve` is a stranger's — and the case
// it actually pinned survives below as "no vLLM adapter". What it also did,
// unintentionally, was make vLLM hosts permanently invisible to the warm-peer
// preference in waired-agent#880, and they are the warmest hosts there are.
//
// The stranger stays in the fixture and must stay untouched: the point of
// #943 is that this answer never comes from the engine this host does not
// serve with.
func TestModelResident_OnAVLLMHost(t *testing.T) {
	for _, tc := range []struct {
		name          string
		health        string
		wantResident  bool
		wantObserved  bool
		wantStranger  int32
		becauseItSays string
	}{
		{
			name: "no vLLM adapter at all", health: "", wantObserved: false,
			becauseItSays: "nothing has been started, so nothing has been looked at",
		},
		{
			name: "ready", health: infruntime.StateReady, wantResident: true, wantObserved: true,
			becauseItSays: "a ready vLLM holds its weights until the process exits",
		},
		{
			name: "not ready", health: infruntime.StateStopped, wantObserved: true,
			becauseItSays: "the supervisor (waired-agent#946) keeps this state live, " +
				"so not-ready is an observation and not a shrug",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, stranger := vllmHostWithAStrangerOllama(t)
			if tc.health != "" {
				p.setVLLM(&recordingAdapter{name: "vllm", health: tc.health})
			}
			s := &inferenceSubsystem{provider: p}

			resident, observed := s.ModelResident()
			if resident != tc.wantResident || observed != tc.wantObserved {
				t.Errorf("ModelResident = (%v, %v), want (%v, %v): %s",
					resident, observed, tc.wantResident, tc.wantObserved, tc.becauseItSays)
			}
			if n := stranger.calls.Load(); n != tc.wantStranger {
				t.Errorf("the answer came from an engine this host does not serve with (%d calls)", n)
			}
		})
	}
}

// TestEngineReady_VLLMParkedIsNotReady mirrors the ollama bar
// (TestEngineReady_ParkedIsNotReady): a hard-stopped engine must stop the
// peer /healthz coordinator advertising capacity that would 503.
func TestEngineReady_VLLMParkedIsNotReady(t *testing.T) {
	p, _ := vllmHostWithAStrangerOllama(t)
	p.setVLLMParked(true)
	if ready, _ := p.EngineReady(); ready {
		t.Error("EngineReady = true while the vLLM engine is stopped, want false")
	}
}

// TestEngineReady_VLLMWithNoAdapterIsNotReady: the health check used to be
// gated on the engine being ollama, so a vLLM host skipped it entirely and
// advertised capacity as long as a model was recorded Active — whatever the
// engine was doing, including not existing (#944).
func TestEngineReady_VLLMWithNoAdapterIsNotReady(t *testing.T) {
	p, _ := vllmHostWithAStrangerOllama(t)
	if ready, _ := p.EngineReady(); ready {
		t.Error("EngineReady = true on a vLLM host with no engine adapter, want false")
	}
}
