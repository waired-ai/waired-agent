package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// phase8FakePeerAdapter is the minimum runtime.Adapter + Transporter
// pair the Phase 8 integration tests need. Transport returns a
// composite RoundTripper that answers /healthz from the per-test
// stubRT and forwards every other path to the upstream inference
// fixture. BaseURL exposes the same upstream so the gateway proxy
// stitches the request URL correctly.
type phase8FakePeerAdapter struct {
	transport http.RoundTripper
	base      string
}

func (a phase8FakePeerAdapter) Name() string                          { return "remote-fake" }
func (a phase8FakePeerAdapter) EnsureRunning(_ context.Context) error { return nil }
func (a phase8FakePeerAdapter) Health(_ context.Context) runtime.Health {
	return runtime.Health{State: runtime.StateReady}
}
func (a phase8FakePeerAdapter) Stop(_ context.Context) error { return nil }
func (a phase8FakePeerAdapter) BaseURL() string              { return a.base }
func (a phase8FakePeerAdapter) Transport() http.RoundTripper { return a.transport }

// splitTransport answers /waired/v1/inference/healthz from the
// configured probeRT (which the existing probe_test.go scaffolding
// supplies) and delegates every other path to http.DefaultTransport
// so the inference proxy reaches the test's httptest upstream.
type splitTransport struct {
	probeRT http.RoundTripper
}

func (s splitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/healthz") {
		return s.probeRT.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// phase8MultiSelector returns a fixed slice of remote Candidates each
// time. Drives the selectAndProbe pipeline end-to-end at the gateway
// layer without dragging in the agent's Selector machinery.
type phase8MultiSelector struct {
	cands []router.Candidate
	err   error
}

func (m *phase8MultiSelector) Select(_ context.Context, _ router.Request) (router.Selection, error) {
	if m.err != nil {
		return router.Selection{}, m.err
	}
	if len(m.cands) == 0 {
		return router.Selection{}, router.ErrModelNotReady
	}
	sel, _ := m.cands[0].Commit()
	return sel, nil
}

func (m *phase8MultiSelector) SelectK(_ context.Context, _ router.Request, _ int) ([]router.Candidate, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.cands, nil
}

func phase8RemoteCandidate(peerID string) router.Candidate {
	return router.NewLocalCandidate(router.Selection{
		EndpointID:    "remote-" + peerID + "-ollama-qwen3-8b",
		ModelID:       "qwen3-8b-instruct",
		Runtime:       remoteRuntimePrefix + peerID,
		EngineModel:   "qwen3:8b-q4_K_M",
		ExecutionMode: "remote",
	})
}

// TestPhase8Integration_ProbeSelectsReadyPeer is the flagship integration
// case the Phase 8 plan describes: three peers, one of each failure
// class.
//
//   - peer-A returns connect-error (NAT path is down) → exclude.
//   - peer-B returns 200 with capacity_used == capacity_total → exclude.
//   - peer-C returns 200 ready → win.
//
// The gateway must commit peer-C, set the fallback headers showing
// "from peer-A reason=transport_error", and the slog.Warn (not
// asserted here, exercised in TestSetSelectionHeaders) accompanies
// the wire signal.
func TestPhase8Integration_ProbeSelectsReadyPeer(t *testing.T) {
	rtA := &stubRT{dialErr: errors.New("connect refused")}
	rtB := &stubRT{status: 200, body: readyBody(4, 4)} // capacity full
	rtC := &stubRT{status: 200, body: readyBody(0, 4)} // ready
	upstreamHits := atomic.Int32{}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	t.Cleanup(upstreamSrv.Close)

	sel := &phase8MultiSelector{
		cands: []router.Candidate{
			phase8RemoteCandidate("peer-A"),
			phase8RemoteCandidate("peer-B"),
			phase8RemoteCandidate("peer-C"),
		},
	}

	// PeerAdapterFactory routes each peerID to a fake adapter whose
	// Transport returns its stubRT (probe) and BaseURL points at the
	// shared upstream (chat completion). The probe layer never reaches
	// upstreamSrv; the inference proxy does, exactly once.
	probeRTs := map[string]http.RoundTripper{
		"peer-A": rtA, "peer-B": rtB, "peer-C": rtC,
	}
	h := buildPhase8Gateway(t, sel, probeRTs, upstreamSrv.URL)

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if peer := resp.Header.Get(HeaderInferencePeer); peer != "peer-C" {
		t.Errorf("%s = %q, want %q (winner)", HeaderInferencePeer, peer, "peer-C")
	}
	if from := resp.Header.Get(HeaderFallbackFrom); from != "peer-A" {
		t.Errorf("%s = %q, want %q (Selector's first choice)", HeaderFallbackFrom, from, "peer-A")
	}
	if reason := resp.Header.Get(HeaderFallbackReason); reason != "transport_error" {
		t.Errorf("%s = %q, want %q", HeaderFallbackReason, reason, "transport_error")
	}
	if upstreamHits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (only the winner gets the inference call)",
			upstreamHits.Load())
	}
}

// TestPhase8Integration_AllPeersFailReturns503 is the brief-queue
// exhaustion path. All three peers fail their probes; the gateway
// brief-queues 250 ms and probes once more; both rounds fail; the
// gateway returns 503 with Retry-After: 5 and the
// waired_all_peers_overloaded code dashboards key off.
//
// The test uses 1 ms briefQueueDelay-equivalent flakeproofing by
// asserting only the final wire shape, not the timing.
func TestPhase8Integration_AllPeersFailReturns503(t *testing.T) {
	rt := &stubRT{dialErr: errors.New("connect refused")}
	sel := &phase8MultiSelector{
		cands: []router.Candidate{
			phase8RemoteCandidate("peer-A"),
			phase8RemoteCandidate("peer-B"),
			phase8RemoteCandidate("peer-C"),
		},
	}
	h := buildPhase8Gateway(t, sel, map[string]http.RoundTripper{
		"peer-A": rt, "peer-B": rt, "peer-C": rt,
	}, "")

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
	}
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "5" {
		t.Errorf("Retry-After = %q, want %q", retryAfter, "5")
	}
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Error.Code != "waired_all_peers_overloaded" {
		t.Errorf("error.code = %q, want %q (body=%s)", env.Error.Code, "waired_all_peers_overloaded", raw)
	}
}

// TestPhase8Integration_LegacyPeer404TreatedReady checks the mixed
// Phase 7 / Phase 8 mesh rollout: a peer that pre-dates Phase 8 will
// 404 on /healthz. The probe coordinator must treat that as ready
// so the inference path still runs (assume Phase 7 contract).
func TestPhase8Integration_LegacyPeer404TreatedReady(t *testing.T) {
	rtLegacy := &stubRT{status: 404, body: "not found"}
	upstreamHits := atomic.Int32{}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(upstreamSrv.Close)

	sel := &phase8MultiSelector{
		cands: []router.Candidate{phase8RemoteCandidate("peer-legacy")},
	}
	h := buildPhase8Gateway(t, sel, map[string]http.RoundTripper{
		"peer-legacy": rtLegacy,
	}, upstreamSrv.URL)

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("legacy peer must serve inference (assume-ready); got %d body=%s",
			resp.StatusCode, raw)
	}
	if upstreamHits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", upstreamHits.Load())
	}
	// No fallback should have fired (cands[0] won outright).
	if from := resp.Header.Get(HeaderFallbackFrom); from != "" {
		t.Errorf("legacy peer winning at position 0 must not set fallback header; got %q", from)
	}
}

// TestPhase8Integration_PeerRoutingDisabledSurfacesRuntimeUnavailable
// pins the loop-prevention error mapping. The overlay-side gateway
// has PeerAdapterFactory=nil — every probe fails with
// ErrPeerRoutingDisabled — and the response must be
// 503 runtime_unavailable, not 503 waired_all_peers_overloaded.
func TestPhase8Integration_PeerRoutingDisabledSurfacesRuntimeUnavailable(t *testing.T) {
	sel := &phase8MultiSelector{
		cands: []router.Candidate{phase8RemoteCandidate("peer-X")},
	}
	// nil PeerAdapterFactory inside buildPhase8Gateway → ErrPeerRoutingDisabled.
	h := buildPhase8Gateway(t, sel, nil, "")

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Error.Code != "runtime_unavailable" {
		t.Errorf("error.code = %q, want %q (body=%s)", env.Error.Code, "runtime_unavailable", raw)
	}
}

// buildPhase8Gateway constructs a HandlerSet wired to the test
// Selector and a per-peer probe transport map. When probeRTs is nil
// PeerAdapterFactory is left nil so the loop-prevention path is
// exercised. When upstreamURL is non-empty, peer adapters proxy
// non-/healthz traffic to it (the inference path); otherwise the
// test only exercises the probe failure paths.
func buildPhase8Gateway(t *testing.T, sel SelectorIface, probeRTs map[string]http.RoundTripper, upstreamURL string) *HandlerSet {
	t.Helper()
	deps := Deps{
		Selector:       sel,
		ListManifests:  func() []catalog.Manifest { return nil },
		AllowOpenAI:    true,
		AllowAnthropic: true,
		HTTPClient:     &http.Client{},
	}
	if probeRTs != nil {
		deps.PeerAdapterFactory = func(deviceID string) (runtime.Adapter, error) {
			rt, ok := probeRTs[deviceID]
			if !ok {
				return nil, fmt.Errorf("unknown peer %q", deviceID)
			}
			return phase8FakePeerAdapter{
				transport: splitTransport{probeRT: rt},
				base:      upstreamURL,
			}, nil
		}
	}
	return NewHandlerSet(deps)
}
