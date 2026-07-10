package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// captureRecorder records every gateway emit for assertion.
type captureRecorder struct {
	mu       sync.Mutex
	requests []observability.RequestEvent
	fallback []observability.FallbackEvent
	briefQ   []string
	probes   []struct {
		outcome   string
		latencyMs uint32
	}
}

func (c *captureRecorder) RecordRequest(ev observability.RequestEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, ev)
}

func (c *captureRecorder) RecordFallback(ev observability.FallbackEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fallback = append(c.fallback, ev)
}

func (c *captureRecorder) RecordBriefQueueRetry(result string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.briefQ = append(c.briefQ, result)
}

func (c *captureRecorder) RecordProbe(outcome string, latencyMs uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes = append(c.probes, struct {
		outcome   string
		latencyMs uint32
	}{outcome, latencyMs})
}

func (c *captureRecorder) requestsSnapshot() []observability.RequestEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]observability.RequestEvent, len(c.requests))
	copy(out, c.requests)
	return out
}

func newGatewayWithRecorder(t *testing.T, sel SelectorIface, adapterURL string, rec Recorder) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: adapterURL})
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
		Recorder:       rec,
	})
}

func TestGateway_RecordsRequestOnSuccess(t *testing.T) {
	rec := &captureRecorder{}
	upstream := fakeOllama(t, nil)
	defer upstream.Close()
	sel := &fakeSelector{sel: router.Selection{
		EndpointID:    "ep_local",
		ModelID:       "qwen3-8b-instruct",
		Runtime:       "ollama",
		EngineModel:   "qwen3:8b-q4_K_M",
		ExecutionMode: "local",
	}}
	gw := newGatewayWithRecorder(t, sel, upstream.URL, rec)

	body := bytes.NewReader([]byte(`{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	reqs := rec.requestsSnapshot()
	if len(reqs) != 1 {
		t.Fatalf("want 1 RequestEvent, got %d", len(reqs))
	}
	if reqs[0].Kind != "openai" {
		t.Errorf("kind: got %q want openai", reqs[0].Kind)
	}
	if reqs[0].Status != http.StatusOK {
		t.Errorf("status: got %d want 200", reqs[0].Status)
	}
	if reqs[0].ErrorReason != "" {
		t.Errorf("error_reason on success: got %q want empty", reqs[0].ErrorReason)
	}
	if reqs[0].Decision != "local" {
		t.Errorf("decision: got %q want local", reqs[0].Decision)
	}
	if reqs[0].Model != "qwen3-8b-instruct" {
		t.Errorf("model: got %q want qwen3-8b-instruct", reqs[0].Model)
	}
}

func TestGateway_RecordsErrorReason_ModelNotFound(t *testing.T) {
	rec := &captureRecorder{}
	sel := &fakeSelector{err: router.ErrModelNotFound}
	gw := newGatewayWithRecorder(t, sel, "", rec)

	body := bytes.NewReader([]byte(`{"model":"unknown","messages":[]}`))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	gw.Handler().ServeHTTP(httptest.NewRecorder(), r)

	reqs := rec.requestsSnapshot()
	if len(reqs) != 1 {
		t.Fatalf("want 1 RequestEvent, got %d", len(reqs))
	}
	if reqs[0].ErrorReason != "model_not_found" {
		t.Errorf("error_reason: got %q want model_not_found", reqs[0].ErrorReason)
	}
	if reqs[0].Status != http.StatusNotFound {
		t.Errorf("status: got %d want 404", reqs[0].Status)
	}
}

func TestGateway_SkipsEmitOnEarlyParseError(t *testing.T) {
	rec := &captureRecorder{}
	sel := &fakeSelector{sel: router.Selection{ModelID: "qwen3-8b-instruct"}}
	gw := newGatewayWithRecorder(t, sel, "", rec)

	// Malformed JSON: never reaches selector, no Model resolved.
	body := bytes.NewReader([]byte(`not json`))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	gw.Handler().ServeHTTP(httptest.NewRecorder(), r)

	if reqs := rec.requestsSnapshot(); len(reqs) != 0 {
		t.Fatalf("pre-selection parse error should not emit; got %+v", reqs)
	}
}

func TestSetSelectionHeaders_RoutesFallbackThroughRecorder(t *testing.T) {
	rec := &captureRecorder{}
	w := httptest.NewRecorder()
	setSelectionHeaders(w, router.Selection{
		Runtime: "remote:peer-c",
		ModelID: "qwen3-8b-instruct",
	}, "peer-a", "capacity_full", rec)

	if w.Header().Get(HeaderFallbackFrom) != "peer-a" {
		t.Errorf("header X-Waired-Fallback-From: got %q want peer-a", w.Header().Get(HeaderFallbackFrom))
	}
	if w.Header().Get(HeaderFallbackReason) != "capacity_full" {
		t.Errorf("header reason: got %q", w.Header().Get(HeaderFallbackReason))
	}
	if len(rec.fallback) != 1 {
		t.Fatalf("want 1 RecordFallback call, got %d", len(rec.fallback))
	}
	if rec.fallback[0].From != "peer-a" || rec.fallback[0].To != "peer-c" {
		t.Errorf("fallback event: got %+v", rec.fallback[0])
	}
	if rec.fallback[0].Reason != "capacity_full" {
		t.Errorf("fallback reason: %q", rec.fallback[0].Reason)
	}
}

func TestSetSelectionHeaders_NoFallbackNoEmit(t *testing.T) {
	rec := &captureRecorder{}
	w := httptest.NewRecorder()
	setSelectionHeaders(w, router.Selection{
		Runtime: "remote:peer-c",
		ModelID: "m",
	}, "", "", rec)
	if len(rec.fallback) != 0 {
		t.Fatalf("no fallback should not emit; got %+v", rec.fallback)
	}
	if w.Header().Get(HeaderInferencePeer) != "peer-c" {
		t.Errorf("inference peer header still required when no fallback")
	}
}

func TestGateway_RecordsProbe_OnRemoteCandidate(t *testing.T) {
	rec := &captureRecorder{}
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/healthz") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(router.HealthStatus{
				EngineReady: true, ShareEnabled: true,
				CapacityTotal: 10, CapacityUsed: 0,
				ModelID: "qwen3:8b-q4_K_M",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer probeSrv.Close()

	cand := remoteCandidateForTest(t, "peer-c", "qwen3-8b-instruct", "qwen3:8b-q4_K_M")
	sel := &candidatesSelector{cands: []router.Candidate{cand}}

	gw := newGatewayWithRecorder(t, sel, "", rec)
	srv := gw.set
	srv.deps.PeerAdapterFactory = func(deviceID string) (runtime.Adapter, error) {
		return &probeAdapter{baseURL: probeSrv.URL}, nil
	}

	body := bytes.NewReader([]byte(`{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	gw.Handler().ServeHTTP(httptest.NewRecorder(), r)

	probes := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.probes)
	}()
	if probes != 1 {
		t.Fatalf("want 1 probe emit, got %d", probes)
	}
	if rec.probes[0].outcome != "ok" {
		t.Errorf("probe outcome: got %q want ok", rec.probes[0].outcome)
	}
}

// remoteCandidateForTest builds a router.Candidate that commits to a
// remote Selection without exercising the inflight tracker.
func remoteCandidateForTest(t *testing.T, peerID, modelID, engineModel string) router.Candidate {
	t.Helper()
	return router.NewLocalCandidate(router.Selection{
		EndpointID:    "ep_remote_" + peerID,
		ModelID:       modelID,
		Runtime:       "remote:" + peerID,
		EngineModel:   engineModel,
		ExecutionMode: "remote",
	})
}

// candidatesSelector returns a fixed candidate slice, ignoring input.
type candidatesSelector struct {
	cands []router.Candidate
}

func (c *candidatesSelector) Select(_ context.Context, _ router.Request) (router.Selection, error) {
	return router.Selection{}, nil
}

func (c *candidatesSelector) SelectK(_ context.Context, _ router.Request, _ int) ([]router.Candidate, error) {
	return c.cands, nil
}

// probeAdapter satisfies runtime.Adapter and runtime.Transporter
// with a plain http.DefaultTransport so /healthz reaches the test
// server. EnsureRunning / Stop are no-ops; Health is always ready.
type probeAdapter struct{ baseURL string }

func (p *probeAdapter) Name() string                          { return "peer" }
func (p *probeAdapter) EnsureRunning(_ context.Context) error { return nil }
func (p *probeAdapter) Health(_ context.Context) runtime.Health {
	return runtime.Health{State: runtime.StateReady}
}
func (p *probeAdapter) Stop(_ context.Context) error { return nil }
func (p *probeAdapter) BaseURL() string              { return p.baseURL }
func (p *probeAdapter) Transport() http.RoundTripper { return http.DefaultTransport }
