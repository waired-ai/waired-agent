package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Recorder is the agent-wide telemetry fan-out. Each Phase 9 emit
// point (gateway request termination, router probe completion,
// inference engine state transition, etc.) calls into a Recorder
// method; Recorder writes the same observation to three sinks:
//
//   - the in-memory Ring (Append) for the /observability/events
//     and /observability/state endpoints,
//   - the Prometheus Metrics registry for /metrics,
//   - the slog Logger for journal-based forensics (the Phase 8
//     slog.Warn calls are preserved by routing through here).
//
// Recorder satisfies, by duck typing, the narrow Recorder
// interfaces each layer (router, gateway, inference) defines for
// its own emit points. Layers should depend on their local
// interface, not on this concrete type.
//
// All methods are nil-safe: a nil *Recorder no-ops, so call sites
// can hold a *Recorder field without an extra not-nil check on
// every emit. Constructor users (cmd/waired-agent) pass a real
// Recorder; unit tests can pass nil to skip emission.
type Recorder struct {
	ring    *Ring
	metrics *Metrics
	logger  *slog.Logger

	mu sync.Mutex
	// Edge-triggered state for engine_state_change events. Each
	// flag tracks its previous value; SetX emits an event only
	// when the value transitions.
	engineReadyKnown  bool
	engineReady       bool
	shareEnabledKnown bool
	shareEnabled      bool
	pausedKnown       bool
	paused            bool
}

// NewRecorder constructs a Recorder fanning out to the supplied
// sinks. Any of ring / metrics / logger may be nil; that sink is
// skipped. A logger of nil falls back to slog.Default().
func NewRecorder(ring *Ring, metrics *Metrics, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{
		ring:    ring,
		metrics: metrics,
		logger:  logger,
	}
}

// --- Gateway-facing emits ---

// RecordRequest emits a per-request terminal observation. Called
// after gateway/openai.go or gateway/anthropic.go has decided the
// final HTTP status (success or error envelope).
func (r *Recorder) RecordRequest(ev RequestEvent) {
	if r == nil {
		return
	}
	if r.ring != nil {
		r.ring.Append(Event{Kind: KindRequest, Request: &ev})
	}
	if r.metrics != nil {
		result := "success"
		if ev.Status >= 400 || ev.ErrorReason != "" {
			result = "error"
		}
		r.metrics.InferenceRequestsTotal.WithLabelValues(ev.Kind, result, ev.ErrorReason).Inc()
		r.metrics.InferenceRequestLatency.Observe(float64(ev.LatencyMs))
	}
	if r.logger != nil && ev.ErrorReason != "" {
		r.logger.LogAttrs(context.Background(), slog.LevelWarn, "inference request error",
			slog.String("kind", ev.Kind),
			slog.String("model", ev.Model),
			slog.String("error_reason", ev.ErrorReason),
			slog.Int("status", ev.Status),
			slog.String("peer_id", ev.PeerID),
		)
	}
}

// RecordFallback emits the supplementary fallback event whenever
// the probe-then-commit winner was not the top-1 candidate. The
// gateway also calls RecordRequest for the same request; the two
// events coexist so tray-style consumers can filter on
// kind=fallback without parsing every request.
func (r *Recorder) RecordFallback(ev FallbackEvent) {
	if r == nil {
		return
	}
	if r.ring != nil {
		r.ring.Append(Event{Kind: KindFallback, Fallback: &ev})
	}
	if r.metrics != nil {
		r.metrics.InferenceFallbackTotal.WithLabelValues(ev.Reason).Inc()
	}
	if r.logger != nil {
		r.logger.LogAttrs(context.Background(), slog.LevelWarn, "inference fallback",
			slog.String("from", ev.From),
			slog.String("to", ev.To),
			slog.String("reason", ev.Reason),
			slog.String("model", ev.Model),
		)
	}
}

// RecordBriefQueueRetry is called once per request that exercised
// the 250 ms brief-queue retry path (i.e. the first ParallelProbe
// pass found no ready candidate). result is "succeeded" if the
// second pass found one, "failed" otherwise.
func (r *Recorder) RecordBriefQueueRetry(result string) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.InferenceBriefQueueRetryTotal.WithLabelValues(result).Inc()
}

// --- Router-facing emits ---

// RecordProbe is called once per /healthz probe completion. outcome
// must be one of the ProbeOutcome.String() tags ("ok",
// "legacy_peer", "auth_error", "transport_error").
func (r *Recorder) RecordProbe(outcome string, latencyMs uint32) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.InferenceProbeTotal.WithLabelValues(outcome).Inc()
	r.metrics.InferenceProbeLatency.Observe(float64(latencyMs))
}

// RecordSelection is called once per SelectK return when at least
// one candidate was produced. decision is the candidate's execution
// mode ("local" / "remote" / "external"); peerID is the underlying
// mesh peer (empty for local/external); model is the resolved model
// id. The router emits this before probe-then-commit so the counter
// reflects the selector's intent, independent of whether probe
// later forced a fallback within the same decision class.
func (r *Recorder) RecordSelection(decision, peerID, model string) {
	if r == nil {
		return
	}
	if r.ring != nil {
		r.ring.Append(Event{
			Kind:      KindSelection,
			Selection: &SelectionEvent{Decision: decision, PeerID: peerID, Model: model},
		})
	}
	if r.metrics != nil {
		r.metrics.InferenceSelectDecisionsTotal.WithLabelValues(decision).Inc()
	}
}

// RecordPinnedPeerUnreachable is called from the router's Tailscale-
// exit-node-style routing path on both the strict 503 and the
// soft-fallback "pin lacks the requested model" branches. reason
// values are "unreachable" or "lacks_model"; the Prometheus counter
// is labelled accordingly so a dashboard can distinguish the two.
func (r *Recorder) RecordPinnedPeerUnreachable(peerID, model, reason string) {
	if r == nil {
		return
	}
	if r.ring != nil {
		r.ring.Append(Event{
			Kind: KindPinnedPeerUnreachable,
			PinnedPeerUnreachable: &PinnedPeerUnreachableEvent{
				PinnedPeerDeviceID: peerID,
				Model:              model,
				Reason:             reason,
			},
		})
	}
	if r.metrics != nil && r.metrics.InferencePinnedPeerUnreachableTotal != nil {
		r.metrics.InferencePinnedPeerUnreachableTotal.WithLabelValues(reason).Inc()
	}
}

// --- Inference-facing emits ---

// RecordServed is called once per request that this agent answered
// on behalf of a mesh peer (i.e. the inbound side of the peer
// overlay). result is "success" or "error".
func (r *Recorder) RecordServed(result string, latencyMs uint32) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.InferenceServedTotal.WithLabelValues(result).Inc()
	r.metrics.InferenceServedLatency.Observe(float64(latencyMs))
}

// RecordAuthReject is called by the peerAuthChain whenever a
// request fails identity verification. reason is the failing-check
// tag ("signature", "wg_peer_only", "replay", "clock_skew",
// "other").
func (r *Recorder) RecordAuthReject(reason string) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.InferenceAuthRejectTotal.WithLabelValues(reason).Inc()
}

// SetEngineReady updates the engine-ready gauge and emits an
// engine_state_change event if the value transitioned. reason is
// the optional human tag for the event (e.g. "model_loaded",
// "ollama_died").
func (r *Recorder) SetEngineReady(ready bool, reason string) {
	if r == nil {
		return
	}
	if r.metrics != nil {
		SetBool(r.metrics.InferenceEngineReady, ready)
	}
	r.transitionEvent("engine_ready", ready, &r.engineReadyKnown, &r.engineReady, reason)
}

// SetShareEnabled updates the share-enabled gauge and emits an
// engine_state_change event on transition.
func (r *Recorder) SetShareEnabled(enabled bool, reason string) {
	if r == nil {
		return
	}
	if r.metrics != nil {
		SetBool(r.metrics.InferenceShareEnabled, enabled)
	}
	r.transitionEvent("share_enabled", enabled, &r.shareEnabledKnown, &r.shareEnabled, reason)
}

// SetPaused updates the paused gauge and emits an
// engine_state_change event on transition.
func (r *Recorder) SetPaused(paused bool, reason string) {
	if r == nil {
		return
	}
	if r.metrics != nil {
		SetBool(r.metrics.InferencePaused, paused)
	}
	r.transitionEvent("paused", paused, &r.pausedKnown, &r.paused, reason)
}

// SetCapacity updates the capacity gauge. Called whenever the
// admission ceiling changes (engine restart, config reload).
func (r *Recorder) SetCapacity(total int) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.InferenceCapacityTotal.Set(float64(total))
}

// SetInflight updates the inflight gauge. Called from the in-flight
// tracker on every Acquire/Release so the gauge stays current
// without a polling thread.
func (r *Recorder) SetInflight(n int) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.InferenceInflight.Set(float64(n))
}

// SetMeshPeers updates the mesh-peer gauges. Called once per
// mesh-snapshot refresh in the agent.
func (r *Recorder) SetMeshPeers(enrolled, reachable, ready int) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.MeshPeers.WithLabelValues("enrolled").Set(float64(enrolled))
	r.metrics.MeshPeers.WithLabelValues("reachable").Set(float64(reachable))
	r.metrics.MeshPeers.WithLabelValues("ready").Set(float64(ready))
}

// Ring returns the underlying event ring for endpoints that need
// to enumerate (the /observability/events handler, the state
// handler's last_inference lookup). Returns nil if no ring was
// configured.
func (r *Recorder) Ring() *Ring {
	if r == nil {
		return nil
	}
	return r.ring
}

// --- internal helpers ---

func (r *Recorder) transitionEvent(flag string, value bool, known *bool, prev *bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !*known {
		*known = true
		*prev = value
		return
	}
	if *prev == value {
		return
	}
	from := stateTag(flag, *prev)
	to := stateTag(flag, value)
	*prev = value
	if r.ring != nil {
		r.ring.Append(Event{
			Kind: KindEngineStateChange,
			EngineStateChange: &EngineStateChangeEvent{
				From:   from,
				To:     to,
				Reason: reason,
			},
		})
	}
	if r.logger != nil {
		r.logger.LogAttrs(context.Background(), slog.LevelInfo, "inference engine state change",
			slog.String("from", from),
			slog.String("to", to),
			slog.String("reason", reason),
			slog.Time("at", time.Now()),
		)
	}
}

func stateTag(flag string, value bool) string {
	switch flag {
	case "engine_ready":
		if value {
			return "ready"
		}
		return "not_ready"
	case "share_enabled":
		if value {
			return "share_on"
		}
		return "share_off"
	case "paused":
		if value {
			return "paused"
		}
		return "running"
	}
	return ""
}
