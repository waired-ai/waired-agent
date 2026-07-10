package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHealthz_ReportsAllFields verifies the /healthz endpoint returns
// every field the Phase 8 probe client consumes. The handler bypasses
// the gates (paused / inference-off / share-off / capacity) so it can
// report current state — the gates would mask "share=off, capacity=2/2"
// behind a single 503, leaving the probe client unable to distinguish
// "peer is full" from "peer is configured off".
func TestHealthz_ReportsAllFields(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.Capacity = 4
		paused := false
		shareDenied := false
		inferenceOff := false
		c.IsPaused = func() bool { return paused }
		c.IsShareDenied = func() bool { return shareDenied }
		c.IsInferenceDisabled = func() bool { return inferenceOff }
		c.EngineReadyFn = func() (bool, string) {
			return true, "qwen3:8b-q4_K_M"
		}
	})

	req := newSignedGetRequest(t, "/waired/v1/inference/healthz", "peer-A", priv,
		time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got HealthSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if !got.EngineReady {
		t.Errorf("EngineReady = false, want true")
	}
	if got.ModelID != "qwen3:8b-q4_K_M" {
		t.Errorf("ModelID = %q, want %q", got.ModelID, "qwen3:8b-q4_K_M")
	}
	if got.CapacityTotal != 4 {
		t.Errorf("CapacityTotal = %d, want 4", got.CapacityTotal)
	}
	if got.CapacityUsed != 0 {
		t.Errorf("CapacityUsed = %d, want 0", got.CapacityUsed)
	}
	if got.Paused {
		t.Errorf("Paused = true, want false")
	}
	if !got.ShareEnabled {
		t.Errorf("ShareEnabled = false, want true")
	}
}

// TestHealthz_PausedReportedButNot503 confirms that paused state is
// reported in the body rather than hidden behind a 503. The Phase 8
// probe client uses this to know "peer is admin-disabled" vs "peer is
// at capacity" vs "peer's WG path failed" — three different errors
// that benefit from different operator-facing messages.
func TestHealthz_PausedReportedButNot503(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.IsPaused = func() bool { return true }
		c.EngineReadyFn = func() (bool, string) { return true, "qwen3:8b-q4_K_M" }
	})

	req := newSignedGetRequest(t, "/waired/v1/inference/healthz", "peer-A", priv,
		time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("paused peer must still 200 /healthz (state in body); got status=%d body=%s",
			rec.Code, rec.Body.String())
	}
	var got HealthSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Paused {
		t.Errorf("Paused = false, want true (gate state must reflect in body)")
	}
}

// TestHealthz_ShareDeniedReported confirms a mesh-share opted-out peer
// answers /healthz with share_enabled=false rather than dropping the
// request. Phase 6 mesh aggregator already filters these candidates
// for fresh enrollees, but a probe arriving during the 15 s aggregator
// staleness window may still target a share-off peer; the body lets
// the probe coordinator exclude it cleanly.
func TestHealthz_ShareDeniedReported(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.IsShareDenied = func() bool { return true }
		c.EngineReadyFn = func() (bool, string) { return true, "qwen3:8b-q4_K_M" }
	})

	req := newSignedGetRequest(t, "/waired/v1/inference/healthz", "peer-A", priv,
		time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("share-denied peer must still 200 /healthz; got %d", rec.Code)
	}
	var got HealthSnapshot
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.ShareEnabled {
		t.Errorf("ShareEnabled = true under IsShareDenied=true; gates not reflected")
	}
}

// TestHealthz_CapacityFullReports200 — the most surprising of the lot:
// even when CapacityUsed == CapacityTotal, /healthz returns 200 with
// the counts in the body. The probe-then-commit two-phase pattern in
// the Phase 8 router relies on the probe seeing live counters so it
// can decide "this peer is full" without burning an inference slot
// to find out.
func TestHealthz_CapacityFullReports200(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newBlockingGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.Capacity = 1
		c.EngineReadyFn = func() (bool, string) { return true, "qwen3:8b-q4_K_M" }
	})

	// Park one inference request to consume the only slot.
	infReq := signedReq(t, "/v1/chat/completions", []byte(`{}`), "peer-A", priv,
		time.Date(2026, 5, 9, 18, 0, 1, 0, time.UTC))
	go s.Handler().ServeHTTP(httptest.NewRecorder(), infReq)
	gw.waitForInFlight(t, 1)

	// /healthz must still 200 and report 1/1 used.
	req := newSignedGetRequest(t, "/waired/v1/inference/healthz", "peer-A", priv,
		time.Date(2026, 5, 9, 18, 0, 2, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("capacity-full /healthz must 200; got %d body=%s", rec.Code, rec.Body.String())
	}
	var got HealthSnapshot
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.CapacityTotal != 1 {
		t.Errorf("CapacityTotal = %d, want 1", got.CapacityTotal)
	}
	if got.CapacityUsed != 1 {
		t.Errorf("CapacityUsed = %d, want 1 (live inflight count)", got.CapacityUsed)
	}

	// Release the parked inference so the test cleans up.
	gw.release()
}

// TestHealthz_RequiresPeerAuth confirms /healthz still goes through
// wgPeerOnly + verifyPeerSignature. The body reveals capacity / paused
// / share state — leaking that to anyone able to reach the WG IP
// (which includes the relay's egress) would be a privacy regression.
func TestHealthz_RequiresPeerAuth(t *testing.T) {
	pub, _ := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.EngineReadyFn = func() (bool, string) { return true, "qwen3:8b-q4_K_M" }
	})

	// Unsigned request: no X-Waired-* headers, no peer-overlay RemoteAddr.
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/healthz", strings.NewReader(""))
	req.RemoteAddr = "1.2.3.4:55555"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("unauthenticated probe must NOT receive a 200 — got body=%s", rec.Body.String())
	}
}

// TestHealthz_NotMountedWhenGatewayNil confirms the endpoint is only
// present when peer-engine routing is wired. A ping-only Server (no
// GatewayHandler) has no engine state to report and must 404.
func TestHealthz_NotMountedWhenGatewayNil(t *testing.T) {
	s := NewServer("self")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/healthz", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ping-only server must 404 /healthz; got status=%d", rec.Code)
	}
}

// TestHealthz_EngineReadyFnNilIsOptional confirms a Config without an
// EngineReadyFn still serves /healthz (with engine_ready=false). This
// lets the agent mount the endpoint before the engine subsystem is
// up — the prober's "ready" check looks at engine_ready, so a transient
// boot window where the engine is still starting reads as not-ready.
func TestHealthz_EngineReadyFnNilIsOptional(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer)

	req := newSignedGetRequest(t, "/waired/v1/inference/healthz", "peer-A", priv,
		time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got HealthSnapshot
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.EngineReady {
		t.Errorf("EngineReady = true when EngineReadyFn is nil; want false")
	}
	if got.ShareEnabled != true {
		t.Errorf("ShareEnabled defaults to true when IsShareDenied is nil; got %v", got.ShareEnabled)
	}
}
