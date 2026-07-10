package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/inference"
)

// stubTransport is a minimal http.RoundTripper for ProbeHealth tests.
// Lets each test inject a status code, body, or error without
// spinning up an httptest.Server (probe coordinator tests in
// internal/gateway exercise the netted path).
type stubTransport struct {
	status int
	body   string
	err    error
	delay  time.Duration
	seen   *http.Request
}

func (t *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.seen = req
	if t.delay > 0 {
		select {
		case <-time.After(t.delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	if t.err != nil {
		return nil, t.err
	}
	return &http.Response{
		StatusCode: t.status,
		Status:     http.StatusText(t.status),
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// TestProbeHealth_OKDecodesBody confirms the happy path: 200 + valid
// JSON body → ProbeOK with the Status struct fully populated.
func TestProbeHealth_OKDecodesBody(t *testing.T) {
	tr := &stubTransport{
		status: 200,
		body:   `{"engine_ready":true,"model_id":"qwen3:8b-q4_K_M","capacity_total":10,"capacity_used":3,"paused":false,"share_enabled":true}`,
	}
	got := ProbeHealth(context.Background(), tr, "http://peer-A.wgnet:55000")
	if got.Outcome != ProbeOK {
		t.Fatalf("Outcome = %v, want ProbeOK", got.Outcome)
	}
	if !got.Status.EngineReady {
		t.Errorf("EngineReady = false, want true")
	}
	if got.Status.ModelID != "qwen3:8b-q4_K_M" {
		t.Errorf("ModelID = %q, want %q", got.Status.ModelID, "qwen3:8b-q4_K_M")
	}
	if got.Status.CapacityUsed != 3 {
		t.Errorf("CapacityUsed = %d, want 3", got.Status.CapacityUsed)
	}
	if !got.IsReady() {
		t.Errorf("IsReady() = false on a healthy probe")
	}
}

// TestProbeHealth_404IsLegacyPeer confirms Phase 7 mesh members are
// treated as ready. The probe client must not exclude old peers from
// the mixed-version mesh just because they don't speak /healthz.
func TestProbeHealth_404IsLegacyPeer(t *testing.T) {
	tr := &stubTransport{status: 404, body: "404 page not found"}
	got := ProbeHealth(context.Background(), tr, "http://peer-A.wgnet:55000")
	if got.Outcome != ProbeLegacyPeer {
		t.Errorf("Outcome = %v, want ProbeLegacyPeer", got.Outcome)
	}
	if !got.IsReady() {
		t.Errorf("legacy peer must report IsReady() = true so mixed-version mesh works")
	}
	if got.FailureReason() != "" {
		t.Errorf("legacy peer must have empty FailureReason; got %q", got.FailureReason())
	}
}

// TestProbeHealth_401IsAuthError captures the case where WG IP
// resolves but the Ed25519 signature is rejected — clock skew, nonce
// replay, missing identity. The probe client surfaces this distinctly
// so operators investigating "peer keeps getting fallback'd" can
// triage WG vs application failures.
func TestProbeHealth_401IsAuthError(t *testing.T) {
	tr := &stubTransport{status: 401, body: "unauthorized"}
	got := ProbeHealth(context.Background(), tr, "http://peer-A.wgnet:55000")
	if got.Outcome != ProbeAuthError {
		t.Errorf("Outcome = %v, want ProbeAuthError", got.Outcome)
	}
	if got.IsReady() {
		t.Errorf("auth-error peer must NOT be ready")
	}
}

// TestProbeHealth_5xxIsTransportError confirms upstream server errors
// (5xx from any layer) are routed to ProbeTransportError so the
// coordinator excludes the peer for this round.
func TestProbeHealth_5xxIsTransportError(t *testing.T) {
	tr := &stubTransport{status: 500, body: "internal error"}
	got := ProbeHealth(context.Background(), tr, "http://peer-A.wgnet:55000")
	if got.Outcome != ProbeTransportError {
		t.Errorf("Outcome = %v, want ProbeTransportError", got.Outcome)
	}
	if got.Err == nil {
		t.Errorf("ProbeTransportError must carry Err")
	}
	if got.IsReady() {
		t.Errorf("5xx peer must NOT be ready")
	}
}

// TestProbeHealth_DialFailureIsTransportError exercises the
// connect-error path — RoundTrip returns an error before any status
// code is available. Common for NAT-asymmetry / WG-keepalive failure
// scenarios.
func TestProbeHealth_DialFailureIsTransportError(t *testing.T) {
	tr := &stubTransport{err: errors.New("dial tcp: connection refused")}
	got := ProbeHealth(context.Background(), tr, "http://peer-A.wgnet:55000")
	if got.Outcome != ProbeTransportError {
		t.Errorf("Outcome = %v, want ProbeTransportError", got.Outcome)
	}
}

// TestProbeHealth_BodyDecodeFailure simulates a peer that 200s with
// non-JSON garbage (would indicate a serious bug, but defense in
// depth: don't blow up the coordinator).
func TestProbeHealth_BodyDecodeFailure(t *testing.T) {
	tr := &stubTransport{status: 200, body: "this is not JSON"}
	got := ProbeHealth(context.Background(), tr, "http://peer-A.wgnet:55000")
	if got.Outcome != ProbeTransportError {
		t.Errorf("Outcome = %v, want ProbeTransportError", got.Outcome)
	}
}

// TestProbeHealth_RespectsContextCancellation confirms the per-probe
// context deadline is honored. The parallel coordinator uses this to
// cap each probe at ~50ms; without it a stuck transport would block
// the whole fanout.
func TestProbeHealth_RespectsContextCancellation(t *testing.T) {
	tr := &stubTransport{status: 200, body: `{}`, delay: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := ProbeHealth(ctx, tr, "http://peer-A.wgnet:55000")
	elapsed := time.Since(start)
	if got.Outcome != ProbeTransportError {
		t.Errorf("Outcome = %v, want ProbeTransportError (deadline exceeded)", got.Outcome)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("ProbeHealth waited %v past the 50ms deadline; should return promptly", elapsed)
	}
}

// TestProbeHealth_NilTransport guards against a logic bug in the
// gateway: a nil transport must not panic. Returns a clean error
// instead.
func TestProbeHealth_NilTransport(t *testing.T) {
	got := ProbeHealth(context.Background(), nil, "http://peer-A.wgnet:55000")
	if got.Outcome != ProbeTransportError {
		t.Errorf("Outcome = %v, want ProbeTransportError", got.Outcome)
	}
	if got.Err == nil {
		t.Errorf("nil transport must surface an error")
	}
}

// TestProbeHealth_URLConstruction confirms the base URL is correctly
// suffixed regardless of trailing slash. Both forms are valid inputs
// from the peer adapter (BaseURL() may or may not include a trailing
// slash depending on Phase 4 / 5 evolution).
func TestProbeHealth_URLConstruction(t *testing.T) {
	for _, base := range []string{
		"http://peer-A.wgnet:55000",
		"http://peer-A.wgnet:55000/",
	} {
		tr := &stubTransport{status: 200, body: `{"engine_ready":true}`}
		_ = ProbeHealth(context.Background(), tr, base)
		if tr.seen == nil {
			t.Fatalf("base=%q: stubTransport never invoked", base)
		}
		want := "http://peer-A.wgnet:55000/waired/v1/inference/healthz"
		if got := tr.seen.URL.String(); got != want {
			t.Errorf("base=%q → URL = %q, want %q", base, got, want)
		}
	}
}

// TestIsReady_RejectsEachUnreadyCondition is an exhaustive table check
// that the IsReady predicate fires when any of the four gate signals
// indicate not-ready: engine not ready, paused, share off, capacity
// full. Each row should evaluate to false.
func TestIsReady_RejectsEachUnreadyCondition(t *testing.T) {
	cases := []struct {
		name string
		s    HealthStatus
		want bool
	}{
		{"all-ok", HealthStatus{EngineReady: true, ShareEnabled: true, CapacityTotal: 4, CapacityUsed: 2}, true},
		{"engine-down", HealthStatus{EngineReady: false, ShareEnabled: true, CapacityTotal: 4}, false},
		{"paused", HealthStatus{EngineReady: true, ShareEnabled: true, Paused: true, CapacityTotal: 4}, false},
		{"share-off", HealthStatus{EngineReady: true, ShareEnabled: false, CapacityTotal: 4}, false},
		{"capacity-full", HealthStatus{EngineReady: true, ShareEnabled: true, CapacityTotal: 4, CapacityUsed: 4}, false},
		{"unlimited-cap", HealthStatus{EngineReady: true, ShareEnabled: true, CapacityTotal: 0, CapacityUsed: 999}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ProbeResult{Outcome: ProbeOK, Status: tc.s}
			if got := r.IsReady(); got != tc.want {
				t.Errorf("IsReady() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFailureReason_TagsMatchHeaderContract pins the
// X-Waired-Fallback-Reason values the gateway will surface. Reordered
// or renamed without intent → header drifts and dashboards break.
func TestFailureReason_TagsMatchHeaderContract(t *testing.T) {
	cases := []struct {
		name string
		r    ProbeResult
		want string
	}{
		{"engine-down", ProbeResult{Outcome: ProbeOK, Status: HealthStatus{ShareEnabled: true, CapacityTotal: 4}}, "engine_not_ready"},
		{"paused", ProbeResult{Outcome: ProbeOK, Status: HealthStatus{EngineReady: true, Paused: true, ShareEnabled: true, CapacityTotal: 4}}, "paused"},
		{"share-off", ProbeResult{Outcome: ProbeOK, Status: HealthStatus{EngineReady: true, ShareEnabled: false, CapacityTotal: 4}}, "share_off"},
		{"capacity-full", ProbeResult{Outcome: ProbeOK, Status: HealthStatus{EngineReady: true, ShareEnabled: true, CapacityTotal: 1, CapacityUsed: 1}}, "capacity_full"},
		{"transport-err", ProbeResult{Outcome: ProbeTransportError, Err: errors.New("dial")}, "transport_error"},
		{"auth-err", ProbeResult{Outcome: ProbeAuthError, Err: errors.New("401")}, "auth_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.FailureReason(); got != tc.want {
				t.Errorf("FailureReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHealthStatus_WireCompatWithInferenceHealthSnapshot is the cross-
// package contract test. The inference server serialises a
// HealthSnapshot; this package decodes into a HealthStatus. Field
// names must agree exactly, or the probe coordinator silently treats
// every peer as "engine_ready=false".
func TestHealthStatus_WireCompatWithInferenceHealthSnapshot(t *testing.T) {
	server := inference.HealthSnapshot{
		EngineReady:   true,
		ModelID:       "qwen3:8b-q4_K_M",
		CapacityTotal: 10,
		CapacityUsed:  3,
		Paused:        false,
		ShareEnabled:  true,
	}
	wire, err := json.Marshal(server)
	if err != nil {
		t.Fatalf("encode inference.HealthSnapshot: %v", err)
	}
	var client HealthStatus
	if err := json.Unmarshal(wire, &client); err != nil {
		t.Fatalf("decode into router.HealthStatus: %v", err)
	}
	if client.EngineReady != server.EngineReady ||
		client.ModelID != server.ModelID ||
		client.CapacityTotal != server.CapacityTotal ||
		client.CapacityUsed != server.CapacityUsed ||
		client.Paused != server.Paused ||
		client.ShareEnabled != server.ShareEnabled {
		t.Errorf("round-trip mismatch:\n  server=%+v\n  client=%+v", server, client)
	}
}
