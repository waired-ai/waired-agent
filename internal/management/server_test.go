package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

type fakeStatus struct{ s Status }

func (f fakeStatus) Status() Status { return f.s }

type fakePinger struct {
	res PingResult
	err error
}

func (f fakePinger) PingPeer(ctx context.Context, peer string) (PingResult, error) {
	if f.err != nil {
		return PingResult{}, f.err
	}
	out := f.res
	out.Peer = peer
	return out, nil
}

func newServer(s Status, p Pinger) *Server {
	return New(fakeStatus{s: s}, p)
}

func TestStatusEndpoint(t *testing.T) {
	srv := newServer(Status{
		DeviceName: "alice",
		OverlayIP:  "100.96.0.10",
		ListenPort: 41820,
		PeerCount:  1,
	}, fakePinger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DeviceName != "alice" || got.PeerCount != 1 {
		t.Fatalf("unexpected status: %+v", got)
	}
}

func TestStatusJSONShapeIncludesIdentityWhenSet(t *testing.T) {
	// Pin the wire-form keys the testnet CI fallback runner relies on:
	// network_id + device_id must appear in the JSON payload (and in
	// the Cloud Logging payload built from the same Status) when
	// populated. Both are omitempty so an empty identity emits a
	// payload without those keys.
	srv := newServer(Status{
		NetworkID:  "net_abc",
		DeviceID:   "dev_xyz",
		DeviceName: "alice",
		OverlayIP:  "100.96.0.10",
	}, fakePinger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, key := range []string{`"network_id":"net_abc"`, `"device_id":"dev_xyz"`} {
		if !strings.Contains(body, key) {
			t.Errorf("body missing %s\nbody=%s", key, body)
		}
	}
}

func TestStatusJSONShapeOmitsIdentityWhenEmpty(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, key := range []string{`"network_id"`, `"device_id"`} {
		if strings.Contains(body, key) {
			t.Errorf("body unexpectedly contains %s (omitempty broken)\nbody=%s", key, body)
		}
	}
}

// TestStatusJSONShape_IPv6TelemetryWhenSet pins the wire-form keys the
// testnet ipv6-verify.sh script consumes from Cloud Logging
// (scripts/dev/testnet-ipv6-verify.sh's poll_once extracts these via
// gcloud logging --format='json(jsonPayload.first_observed_v6_unix,
// jsonPayload.stun_attempts_v6, jsonPayload.stun_responses_v6, ...)').
// Status JSON ↔ Cloud Logging payload mirror via cmd/waired-agent/
// stats.go's buildPayload — if either drifts, the verifier breaks
// silently with a "matrix shows all v4" false negative.
func TestStatusJSONShape_IPv6TelemetryWhenSet(t *testing.T) {
	srv := newServer(Status{
		DeviceName:          "alice",
		ObservedAddrV6:      "[2001:db8::1]:51820",
		FirstObservedV6Unix: 1747400000,
		STUNAttemptsV4:      42,
		STUNAttemptsV6:      17,
		STUNResponsesV4:     42,
		STUNResponsesV6:     12,
	}, fakePinger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, key := range []string{
		`"observed_addr_v6":"[2001:db8::1]:51820"`,
		`"first_observed_v6_unix":1747400000`,
		`"stun_attempts_v4":42`,
		`"stun_attempts_v6":17`,
		`"stun_responses_v4":42`,
		`"stun_responses_v6":12`,
	} {
		if !strings.Contains(body, key) {
			t.Errorf("body missing %s\nbody=%s", key, body)
		}
	}
}

// TestStatusJSONShape_IPv6TelemetryOmitWhenZero confirms omitempty on
// the new fields so a pre-v6-observation Status (the common case for
// the first few minutes of agent startup) doesn't emit zero-valued
// keys. Zero is otherwise indistinguishable from a counter held at 0
// after many "send failed" rounds — the verifier prefers the absence.
func TestStatusJSONShape_IPv6TelemetryOmitWhenZero(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, key := range []string{
		`"observed_addr_v6"`,
		`"first_observed_v6_unix"`,
		`"stun_attempts_v4"`,
		`"stun_attempts_v6"`,
		`"stun_responses_v4"`,
		`"stun_responses_v6"`,
	} {
		if strings.Contains(body, key) {
			t.Errorf("body unexpectedly contains %s (omitempty broken)\nbody=%s", key, body)
		}
	}
}

func TestPingEndpoint(t *testing.T) {
	srv := newServer(Status{}, fakePinger{res: PingResult{
		OK:             true,
		LatencyMS:      12.3,
		DeviceFromPeer: "bob",
		TimeFromPeer:   "2026-04-30T12:00:00Z",
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/ping", strings.NewReader(`{"peer":"bob"}`))
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got PingResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Peer != "bob" || !got.OK || got.DeviceFromPeer != "bob" {
		t.Fatalf("unexpected ping result: %+v", got)
	}
}

func TestPingEndpointError(t *testing.T) {
	srv := newServer(Status{}, fakePinger{err: errors.New("boom")})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/ping", strings.NewReader(`{"peer":"bob"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

type fakePause struct {
	mu      sync.Mutex
	current state.Phase
	desired state.Phase
	err     error
}

func newFakePause(initial state.Phase) *fakePause {
	return &fakePause{current: initial, desired: initial}
}

func (f *fakePause) Pause(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.current = state.PhasePaused
	f.desired = state.PhasePaused
	return nil
}

func (f *fakePause) Resume(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.current = state.PhaseActive
	f.desired = state.PhaseActive
	return nil
}

func (f *fakePause) Phase() (state.Phase, state.Phase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, f.desired
}

func TestPauseEndpointFlipsPhase(t *testing.T) {
	pc := newFakePause(state.PhaseActive)
	srv := New(fakeStatus{}, fakePinger{}).WithPause(pc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/pause", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got PhaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Phase != "paused" || got.DesiredPhase != "paused" {
		t.Fatalf("after pause: %+v", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/waired/v1/resume", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Phase != "active" || got.DesiredPhase != "active" {
		t.Fatalf("after resume: %+v", got)
	}
}

func TestPauseEndpointRejectsGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithPause(newFakePause(state.PhaseActive))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/pause", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestPauseEndpointMissingControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithPause
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/pause", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no PauseController, got %d", rec.Code)
	}
}

func TestPauseEndpointPropagatesError(t *testing.T) {
	pc := newFakePause(state.PhaseActive)
	pc.err = errors.New("disk full")
	srv := New(fakeStatus{}, fakePinger{}).WithPause(pc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/pause", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("expected error body to mention disk full, got %s", rec.Body.String())
	}
}

func TestStatusIncludesPhaseWhenControllerAttached(t *testing.T) {
	pc := newFakePause(state.PhasePaused)
	srv := New(fakeStatus{s: Status{DeviceName: "alice"}}, fakePinger{}).WithPause(pc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)

	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Phase != "paused" || got.DesiredPhase != "paused" {
		t.Errorf("status missing phase fields: %+v", got)
	}
}

func TestStatusOmitsPhaseWhenNoController(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	// JSON tags use omitempty, so the field should not appear in the body.
	if strings.Contains(rec.Body.String(), `"phase"`) {
		t.Errorf("status should omit phase when no controller, got %s", rec.Body.String())
	}
}

// TestStatusJSONShape_PeerHardwareWhenSet pins the wire-form fields
// Phase 7's tray / CLI hardware row consume. Mirrors the
// signer.HardwareSummary shape stripped to GPU model + VRAM + compute
// cap + RAM total. Asserted as a JSON key-presence test (rather than
// deep struct equality) so future additions to PeerStatus do not
// require this case to be updated.
func TestStatusJSONShape_PeerHardwareWhenSet(t *testing.T) {
	srv := newServer(Status{
		DeviceName: "alice",
		Peers: []PeerStatus{{
			DeviceID:    "dev_bob",
			CurrentPath: "direct",
			Hardware: &PeerHardware{
				GPUModel:    "NVIDIA GeForce RTX 4090",
				VRAMTotalMB: 24576,
				ComputeCap:  "8.9",
				RAMTotalGB:  64,
			},
		}},
	}, fakePinger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, key := range []string{
		`"hardware":`,
		`"gpu_model":"NVIDIA GeForce RTX 4090"`,
		`"vram_total_mb":24576`,
		`"compute_cap":"8.9"`,
		`"ram_total_gb":64`,
	} {
		if !strings.Contains(body, key) {
			t.Errorf("body missing %s\nbody=%s", key, body)
		}
	}
}

func TestStatusJSONShape_PeerHardwareOmittedWhenNil(t *testing.T) {
	srv := newServer(Status{
		DeviceName: "alice",
		Peers: []PeerStatus{{
			DeviceID:    "dev_bob",
			CurrentPath: "direct",
		}},
	}, fakePinger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), `"hardware"`) {
		t.Errorf("body should omit hardware when nil, got %s", rec.Body.String())
	}
}

func TestRejectsNonLoopback(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "192.0.2.1:54321"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback, got %d", rec.Code)
	}
}
