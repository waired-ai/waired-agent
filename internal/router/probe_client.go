package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HealthStatus mirrors internal/inference.HealthSnapshot — the body
// the peer's /waired/v1/inference/healthz endpoint emits. The two
// types are intentionally separate so this package (which lives below
// the gateway in the import hierarchy) doesn't pull in inference and
// so the wire shape can evolve independently if one side ever needs
// a private field. JSON tags are kept identical; a contract test in
// probe_client_test.go round-trips between the two definitions.
type HealthStatus struct {
	EngineReady   bool   `json:"engine_ready"`
	ModelID       string `json:"model_id"`
	CapacityTotal int    `json:"capacity_total"`
	CapacityUsed  int    `json:"capacity_used"`
	Paused        bool   `json:"paused"`
	ShareEnabled  bool   `json:"share_enabled"`
}

// ProbeOutcome is the discriminated result returned by ProbeHealth.
// Probe consumers distinguish four cases:
//
//   - OK: the peer responded with a 200 and decoded body.
//   - LegacyPeer: the peer returned 404, indicating a pre-Phase-8
//     mesh member without the /healthz endpoint. The probe client
//     surfaces this as a distinct outcome rather than an error so the
//     coordinator can fall back to "assume ready" rather than
//     excluding the peer entirely.
//   - AuthError: 401 / 403 from the peer-auth chain — usually means
//     the WG path is up but the signature envelope was rejected
//     (clock skew, nonce replay, missing identity).
//   - TransportError: any other error: dial failure, 5xx, context
//     deadline, body decode failure. The probe client treats this
//     as "exclude peer".
type ProbeOutcome int

const (
	ProbeOK ProbeOutcome = iota
	ProbeLegacyPeer
	ProbeAuthError
	ProbeTransportError
)

// String returns a stable tag suitable for the
// X-Waired-Fallback-Reason header and slog attributes.
func (p ProbeOutcome) String() string {
	switch p {
	case ProbeOK:
		return "ok"
	case ProbeLegacyPeer:
		return "legacy_peer"
	case ProbeAuthError:
		return "auth_error"
	case ProbeTransportError:
		return "transport_error"
	}
	return "unknown"
}

// ProbeResult is the full outcome of one ProbeHealth call.
//
//   - Outcome == ProbeOK: Status carries the decoded HealthSnapshot.
//   - Outcome == ProbeLegacyPeer: Status is zero; the coordinator
//     should treat the peer as ready (assume the Phase 7 contract).
//   - Outcome == ProbeAuthError or ProbeTransportError: Err is set;
//     Status is zero. The coordinator excludes this peer.
type ProbeResult struct {
	Outcome   ProbeOutcome
	Status    HealthStatus
	Err       error
	LatencyMs uint32
}

// ProbeHealth issues a GET against the peer-overlay /healthz endpoint
// and returns a ProbeResult. transport must be the peer adapter's
// signing RoundTripper — ProbeHealth does NOT sign requests itself;
// it relies on the adapter to attach the Ed25519 envelope (matching
// internal/runtime/peer/adapter.go's signingTransport).
//
// baseURL is the peer-overlay base (e.g. "http://peer-A.wgnet:55000")
// the adapter is configured with. The /waired/v1/inference/healthz
// path is appended here so the call site does not need to remember
// it.
//
// Cancellation: respects ctx. The Phase 8 probe coordinator wraps
// the per-probe context with a parallel-budget deadline; ProbeHealth
// returns a ProbeTransportError on deadline exceeded.
func ProbeHealth(ctx context.Context, transport http.RoundTripper, baseURL string) ProbeResult {
	if transport == nil {
		return ProbeResult{Outcome: ProbeTransportError, Err: errors.New("router: nil transport")}
	}
	target := strings.TrimRight(baseURL, "/") + "/waired/v1/inference/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return ProbeResult{Outcome: ProbeTransportError, Err: fmt.Errorf("build probe request: %w", err)}
	}
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := transport.RoundTrip(req)
	latency := uint32(time.Since(start).Milliseconds())
	if err != nil {
		return ProbeResult{Outcome: ProbeTransportError, Err: err, LatencyMs: latency}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var snap HealthStatus
		dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
		if err := dec.Decode(&snap); err != nil {
			return ProbeResult{Outcome: ProbeTransportError, Err: fmt.Errorf("decode /healthz body: %w", err), LatencyMs: latency}
		}
		return ProbeResult{Outcome: ProbeOK, Status: snap, LatencyMs: latency}
	case http.StatusNotFound:
		return ProbeResult{Outcome: ProbeLegacyPeer, LatencyMs: latency}
	case http.StatusUnauthorized, http.StatusForbidden:
		return ProbeResult{Outcome: ProbeAuthError, Err: fmt.Errorf("/healthz auth: %s", resp.Status), LatencyMs: latency}
	default:
		return ProbeResult{Outcome: ProbeTransportError, Err: fmt.Errorf("/healthz unexpected status: %s", resp.Status), LatencyMs: latency}
	}
}

// IsReady is the standard probe-side admission check: a peer is ready
// when it answered the probe with engine_ready=true, isn't paused,
// has share-enabled, AND has capacity headroom (used < total, or
// total == 0 meaning unlimited). LegacyPeer results are treated as
// ready so mixed Phase 7 / Phase 8 mesh deployments work — Phase 7
// peers don't advertise gates over /healthz, so trusting them here
// matches the pre-Phase-8 behaviour.
func (r ProbeResult) IsReady() bool {
	switch r.Outcome {
	case ProbeLegacyPeer:
		return true
	case ProbeOK:
		s := r.Status
		if !s.EngineReady || s.Paused || !s.ShareEnabled {
			return false
		}
		if s.CapacityTotal > 0 && s.CapacityUsed >= s.CapacityTotal {
			return false
		}
		return true
	default:
		return false
	}
}

// FailureReason returns the wire-stable tag the gateway puts into the
// X-Waired-Fallback-Reason header when this probe is the reason a
// candidate was rejected. Returns empty for ready outcomes.
func (r ProbeResult) FailureReason() string {
	if r.IsReady() {
		return ""
	}
	if r.Outcome != ProbeOK {
		return r.Outcome.String()
	}
	s := r.Status
	switch {
	case !s.EngineReady:
		return "engine_not_ready"
	case s.Paused:
		return "paused"
	case !s.ShareEnabled:
		return "share_off"
	case s.CapacityTotal > 0 && s.CapacityUsed >= s.CapacityTotal:
		return "capacity_full"
	}
	return r.Outcome.String()
}
