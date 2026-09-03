package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// A computer that is not running models says so (waired-agent#1206).
//
// The defect was found on real hardware twice: a freshly enrolled host
// with no engine at all, and a host whose owner answered "don't run local
// AI", both showed `State: Unreachable · Engine: Ollama · endpoint
// http://127.0.0.1:9475` on their device page, with the daemon's own
// `dial tcp 127.0.0.1:9475: connect: connection refused` underneath. The
// picture the control plane was given described a broken Ollama on a
// machine that had never had one.

// TestProbeTarget_NoEngineInstalledIsNone: servingEngine() answers
// RuntimeOllama for an unset pointer and can never say "none", so the
// question this getter asks has to be the one that means it — is that
// engine actually on this host.
//
// PRODUCT CONTRACT (waired-agent#1206): what the probe loop dials is
// decided by whether an engine is installed, not by which engine would be
// chosen if one were.
func TestProbeTarget_NoEngineInstalledIsNone(t *testing.T) {
	cfg := agentconfig.InferenceConfig{}
	yes := func() bool { return true }
	no := func() bool { return false }

	tests := []struct {
		name         string
		engine       string
		ollamaUsable func() bool
		vllmUsable   func() bool
		wantKind     string
		wantPort     bool // a port at all
	}{
		{
			name:   "an ollama host with the binary installed",
			engine: catalog.RuntimeOllama, ollamaUsable: yes,
			wantKind: signer.InferenceTypeOllama, wantPort: true,
		},
		{
			name:   "a vLLM host with the venv installed",
			engine: catalog.RuntimeVLLM, vllmUsable: yes,
			wantKind: signer.InferenceTypeVLLM, wantPort: true,
		},
		{
			// The defect. servingEngine() defaults to ollama, so this host
			// dialled 9475 and reported a refused connection forever.
			name:   "no engine installed at all",
			engine: catalog.RuntimeOllama, ollamaUsable: no,
			wantKind: signer.InferenceTypeNone,
		},
		{
			// The wizard's host between choosing vLLM and the venv landing.
			name:   "vLLM chosen, venv not there yet",
			engine: catalog.RuntimeVLLM, vllmUsable: no,
			wantKind: signer.InferenceTypeNone,
		},
		{
			// nil reads as "not usable" everywhere else that asks these
			// seams; it must not read as "assume ollama" here.
			name:     "an unwired seam is not an installed engine",
			engine:   catalog.RuntimeOllama,
			wantKind: signer.InferenceTypeNone,
		},
		{
			// The engines do not stand in for each other: an ollama binary
			// on a host serving vLLM is not a vLLM venv.
			name:   "the other engine's binary does not count",
			engine: catalog.RuntimeVLLM, ollamaUsable: yes, vllmUsable: no,
			wantKind: signer.InferenceTypeNone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &agentInferenceProvider{ollamaUsable: tc.ollamaUsable, vllmUsable: tc.vllmUsable}
			p.setServingEngine(tc.engine)
			kind, port := p.probeTarget(cfg)
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
			if (port != 0) != tc.wantPort {
				t.Errorf("port = %d, want a port: %v", port, tc.wantPort)
			}
			// The pair has to agree, because engineKindProbable(kind) and
			// port == 0 are separate tests in the probe loop's guard.
			if (kind == signer.InferenceTypeNone) != (port == 0) {
				t.Errorf("kind %q and port %d disagree; the loop's guard reads them separately", kind, port)
			}
		})
	}
}

// TestProbeDepsEngineLess folds the three reasons there is nothing to
// probe. The flag is fixed for the process; the other two are a setting
// and an install, and reading either once at boot is what stranded the
// hosts this issue is about.
func TestProbeDepsEngineLess(t *testing.T) {
	off := func() bool { return true }
	on := func() bool { return false }
	tests := []struct {
		name string
		deps inferenceProbeDeps
		want bool
	}{
		{
			name: "an installed engine with inference on",
			deps: inferenceProbeDeps{
				EngineTarget:      staticEngineTarget(signer.InferenceTypeOllama, 9475),
				LocalInferenceOff: on,
			},
		},
		{
			name: "--disable-inference",
			deps: inferenceProbeDeps{
				EngineTarget: staticEngineTarget(signer.InferenceTypeOllama, 9475),
				Disabled:     true,
			},
			want: true,
		},
		{
			name: "the person here turned local inference off",
			deps: inferenceProbeDeps{
				EngineTarget:      staticEngineTarget(signer.InferenceTypeOllama, 9475),
				LocalInferenceOff: off,
			},
			want: true,
		},
		{
			name: "no engine installed",
			deps: inferenceProbeDeps{
				EngineTarget: staticEngineTarget(signer.InferenceTypeNone, 0),
			},
			want: true,
		},
		{
			name: "an unwired target is not an engine",
			deps: inferenceProbeDeps{},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.deps.engineLess(); got != tc.want {
				t.Errorf("engineLess() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunLocalInferenceProbe_StartsProbingWhenTheEngineArrives is the
// regression this issue's fix could easily have introduced, pinned.
//
// PRODUCT CONTRACT (waired-agent#1206, and #304/#339 for the premise): an
// engine installed after boot is adopted without a daemon restart, and
// the browser wizard's whole flow is a host that had no engine when the
// daemon started. The engine-less branch used to block for the life of
// the process on the reading that "does this device have an engine" is a
// configuration fact — so a host that took it would be described as
// engine-less however well it went on to serve.
func TestRunLocalInferenceProbe_StartsProbingWhenTheEngineArrives(t *testing.T) {
	cp, pushes, lastState := capturingCP(t)

	// A mux, not a bare handler: httptest listeners are not private to the
	// test that opened them, so a stray request from elsewhere must not be
	// answerable as this engine's /api/tags.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"m:q4","size":1}]}`))
	})
	engine := httptest.NewServer(mux)
	defer engine.Close()
	_, enginePort := hostPort(t, engine.URL)

	var installed atomic.Bool
	deps := engineLessDeps(t, cp.URL)
	deps.Interval = 20 * time.Millisecond
	deps.EngineTarget = func() (string, int) {
		if !installed.Load() {
			return signer.InferenceTypeNone, 0
		}
		return signer.InferenceTypeOllama, enginePort
	}

	// The engine lands a moment after the daemon has already decided there
	// was none.
	go func() {
		time.Sleep(120 * time.Millisecond)
		installed.Store(true)
	}()

	probeRunUntil(t, deps, "the probe to describe a serving engine", func() bool {
		return lastState().Reachable
	})

	st := lastState()
	if st.Type != signer.InferenceTypeOllama {
		t.Errorf("type = %q, want %q once the engine is installed", st.Type, signer.InferenceTypeOllama)
	}
	if !st.Reachable {
		t.Errorf("the probe never reported the engine that arrived after boot: %+v", st)
	}
	if pushes() == 0 {
		t.Error("nothing was pushed at all")
	}
}

// TestRunLocalInferenceProbe_NoEngineIsNotABrokenOne is the defect's own
// shape: what the control plane is told about a computer with no engine.
//
// PRODUCT CONTRACT (waired-agent#1206). type=none carries no endpoint and
// no error — "this computer does not run models" — where the old push
// carried ollama, 127.0.0.1:9475 and a connection-refused string, which
// the device page renders as a fault.
func TestRunLocalInferenceProbe_NoEngineIsNotABrokenOne(t *testing.T) {
	cp, _, lastState := capturingCP(t)

	deps := engineLessDeps(t, cp.URL)
	deps.Interval = 20 * time.Millisecond
	probeRunUntil(t, deps, "a hardware-only report", func() bool {
		return lastState().Hardware != nil
	})

	st := lastState()
	if st.Type != signer.InferenceTypeNone {
		t.Errorf("type = %q, want %q", st.Type, signer.InferenceTypeNone)
	}
	if st.Reachable {
		t.Error("reachable=true on a host with no engine")
	}
	if st.LastError != "" {
		t.Errorf("last_error = %q — a computer that runs no models has no engine error to report", st.LastError)
	}
	if st.Endpoint != "" {
		t.Errorf("endpoint = %q — nothing was dialled, so nothing should be named", st.Endpoint)
	}
}

// TestWaitForEngine_EndsWithTheDaemon: the wait is not a leak.
func TestWaitForEngine_EndsWithTheDaemon(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	deps := inferenceProbeDeps{
		Interval:     10 * time.Millisecond,
		EngineTarget: staticEngineTarget(signer.InferenceTypeNone, 0),
	}
	done := make(chan bool, 1)
	go func() { done <- waitForEngine(ctx, deps) }()
	cancel()
	select {
	case got := <-done:
		if got {
			t.Error("waitForEngine reported an engine after the context ended")
		}
	case <-time.After(waitBackstop):
		t.Fatal("waitForEngine did not return when the daemon went down")
	}
}
