package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/waired-ai/waired-agent/internal/observability"
)

type stubStateProvider struct {
	state ObservabilityState
}

func (s stubStateProvider) ObservabilityState() ObservabilityState { return s.state }

func newObservabilityServer(t *testing.T, ring *observability.Ring, state ObservabilityStateProvider, metrics http.Handler) *Server {
	t.Helper()
	srv := New(stubStatus{}, stubPinger{})
	srv.WithObservability(ObservabilityConfig{
		Ring:           ring,
		MetricsHandler: metrics,
		State:          state,
	})
	return srv
}

func TestObservabilityEvents_EmptyRing(t *testing.T) {
	ring := observability.NewRing(8)
	srv := newObservabilityServer(t, ring, nil, nil)
	w, r := httpGet(t, "/waired/v1/observability/events")
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Events    []observability.Event `json:"events"`
		NextSince uint64                `json:"next_since"`
		OldestSeq uint64                `json:"oldest_seq"`
		Gap       bool                  `json:"gap"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 0 || resp.OldestSeq != 0 || resp.Gap {
		t.Fatalf("empty ring: events=%d oldest=%d gap=%v", len(resp.Events), resp.OldestSeq, resp.Gap)
	}
}

func TestObservabilityEvents_RoundTripsSinceCursor(t *testing.T) {
	ring := observability.NewRing(16)
	for i := 0; i < 5; i++ {
		ring.Append(observability.Event{
			Kind:    observability.KindRequest,
			Request: &observability.RequestEvent{Kind: "openai", Model: "m"},
		})
	}
	srv := newObservabilityServer(t, ring, nil, nil)
	w, r := httpGet(t, "/waired/v1/observability/events?since=3")
	srv.Handler().ServeHTTP(w, r)

	var resp struct {
		Events    []observability.Event `json:"events"`
		NextSince uint64                `json:"next_since"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Events) != 2 {
		t.Fatalf("since=3 should return 2 events; got %d", len(resp.Events))
	}
	if resp.NextSince != 5 {
		t.Errorf("next_since: got %d want 5", resp.NextSince)
	}
}

func TestObservabilityEvents_KindFilter(t *testing.T) {
	ring := observability.NewRing(16)
	ring.Append(observability.Event{Kind: observability.KindRequest, Request: &observability.RequestEvent{}})
	ring.Append(observability.Event{Kind: observability.KindFallback, Fallback: &observability.FallbackEvent{}})
	ring.Append(observability.Event{Kind: observability.KindRequest, Request: &observability.RequestEvent{}})

	srv := newObservabilityServer(t, ring, nil, nil)
	w, r := httpGet(t, "/waired/v1/observability/events?kinds=fallback")
	srv.Handler().ServeHTTP(w, r)

	var resp struct {
		Events []observability.Event `json:"events"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Events) != 1 || resp.Events[0].Kind != observability.KindFallback {
		t.Fatalf("kinds=fallback: events=%+v", resp.Events)
	}
}

func TestObservabilityEvents_GapFlag(t *testing.T) {
	ring := observability.NewRing(4)
	for i := 0; i < 6; i++ {
		ring.Append(observability.Event{Kind: observability.KindRequest, Request: &observability.RequestEvent{}})
	}
	// Ring holds seq 3..6. since=1 → gap.
	srv := newObservabilityServer(t, ring, nil, nil)
	w, r := httpGet(t, "/waired/v1/observability/events?since=1")
	srv.Handler().ServeHTTP(w, r)

	var resp struct {
		Gap       bool   `json:"gap"`
		OldestSeq uint64 `json:"oldest_seq"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Gap {
		t.Errorf("gap flag should be true for since < oldest-1")
	}
	if resp.OldestSeq != 3 {
		t.Errorf("oldest_seq: got %d want 3", resp.OldestSeq)
	}
}

func TestObservabilityEvents_NoRing503(t *testing.T) {
	srv := newObservabilityServer(t, nil, nil, nil)
	// Route is not registered when Ring is nil; loopback mux returns 404
	w, r := httpGet(t, "/waired/v1/observability/events")
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("route should be unregistered when Ring nil; got %d", w.Code)
	}
}

func TestObservabilityState_Snapshot(t *testing.T) {
	ring := observability.NewRing(8)
	ring.Append(observability.Event{
		Kind: observability.KindRequest,
		TS:   time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
		Request: &observability.RequestEvent{
			Kind:         "openai",
			Decision:     "remote",
			PeerID:       "peer-c",
			Model:        "qwen3:8b",
			FallbackFrom: "peer-a",
			LatencyMs:    312,
		},
	})
	state := stubStateProvider{state: ObservabilityState{
		Agent: AgentState{
			DeviceID:      "dev-1",
			Version:       "0.x",
			EngineReady:   true,
			ModelID:       "qwen3:8b",
			ShareEnabled:  true,
			CapacityTotal: 10,
			Inflight:      2,
		},
		Mesh: MeshState{
			PeersEnrolled:  12,
			PeersReachable: 9,
			PeersReady:     7,
		},
	}}
	srv := newObservabilityServer(t, ring, state, nil)
	w, r := httpGet(t, "/waired/v1/observability/state")
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var got ObservabilityState
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Agent.DeviceID != "dev-1" {
		t.Errorf("device_id: %q", got.Agent.DeviceID)
	}
	if got.Mesh.PeersReady != 7 {
		t.Errorf("peers_ready: %d", got.Mesh.PeersReady)
	}
	if got.LastInference == nil {
		t.Fatalf("last_inference should be derived from ring; got nil")
	}
	if got.LastInference.PeerID != "peer-c" {
		t.Errorf("last_inference peer: %q", got.LastInference.PeerID)
	}
	if !got.LastInference.HadFallback {
		t.Errorf("had_fallback should be true when FallbackFrom set")
	}
}

func TestObservabilityState_NoProvider(t *testing.T) {
	srv := newObservabilityServer(t, nil, nil, nil)
	w, r := httpGet(t, "/waired/v1/observability/state")
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("route should be unregistered when State nil; got %d", w.Code)
	}
}

func TestObservabilityMetrics_PromExposition(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)
	m.InferenceRequestsTotal.WithLabelValues("openai", "success", "").Inc()

	srv := newObservabilityServer(t, nil, nil, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	w, r := httpGet(t, "/waired/v1/metrics")
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "waired_inference_requests_total") {
		t.Errorf("missing waired_inference_requests_total in /metrics body")
	}
}

func TestObservabilityEvents_MethodNotAllowed(t *testing.T) {
	ring := observability.NewRing(4)
	srv := newObservabilityServer(t, ring, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/waired/v1/observability/events", nil)
	r.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST should 405; got %d body=%s", w.Code, w.Body.String())
	}
}

// --- helpers ---

func httpGet(t *testing.T, path string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "127.0.0.1:1"
	return httptest.NewRecorder(), r
}
