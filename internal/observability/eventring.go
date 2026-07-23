// Package observability provides the agent-local telemetry surface
// consumed by tray / claude plugin / Grafana scrapers via the
// management API. It is the single emit point that the gateway,
// router, and inference packages call into; the composite Recorder
// (recorder.go) fans events out to three sinks: an in-memory ring
// (this file), a Prometheus registry (metrics.go), and slog.
//
// The ring is in-memory only; durability for cross-restart history
// is the responsibility of the Phase 9.5 CP-push sink (deferred).
package observability

import (
	"log/slog"
	"sync"
	"time"
)

// Kind discriminates the polymorphic Event payload. The wire shape
// (/waired/v1/observability/events) carries one of the type-named
// fields (Request / Fallback / Selection / EngineStateChange)
// populated per Kind; the others are omitempty.
type Kind string

const (
	KindRequest           Kind = "request"
	KindFallback          Kind = "fallback"
	KindSelection         Kind = "selection"
	KindEngineStateChange Kind = "engine_state_change"
	// KindRoutingModeChange records an operator-initiated transition
	// of the Tailscale-exit-node-style routing preference. Emitted by
	// the workerController on every successful SetMode / SetPin /
	// Clear so the audit trail (and the tray's "Recent activity"
	// surface) shows what changed and when.
	KindRoutingModeChange Kind = "routing_mode_change"
	// KindPinnedPeerUnreachable is emitted on the Selector hot path
	// every time a request hits ErrPinnedPeerUnreachable (strict 503)
	// OR the soft-fallback "pin lacks the requested model" branch.
	// Reason discriminates: "unreachable" vs "lacks_model". Tray
	// surfaces both alongside the regular fallback list so the
	// operator notices a degraded pin without grepping logs.
	KindPinnedPeerUnreachable Kind = "pinned_peer_unreachable"
	// KindClaudeNodeChange records an operator-initiated transition of
	// the per-class Claude node policy (#648): which mesh node serves
	// the Claude Code main loop / subagents. Emitted by the
	// claudeNodeController on every successful SetTarget.
	KindClaudeNodeChange Kind = "claude_node_change"
	// KindClaudeNodeFallback is emitted when a Claude request whose
	// class targets a pinned mesh node could not be served there and
	// was non-destructively retried locally (#648) — the persisted
	// policy stays pinned; routing resumes when the peer returns.
	KindClaudeNodeFallback Kind = "claude_node_fallback"
	// KindPublicShareNudge is the one-shot hint that enabling Public
	// Share MIGHT give this device access to more capable nodes,
	// emitted when a request could not be served by the user's own
	// nodes and no Public Share consent has been recorded (public share
	// spec §4.2). It is a possibility hint, not evidence: a
	// pre-consent agent holds no grants, so no public node is in its
	// map and none can be observed. Never blocks or fails a request.
	KindPublicShareNudge Kind = "public_share_nudge"
)

// Event is the unit stored in the ring buffer.
type Event struct {
	Seq  uint64    `json:"seq"`
	TS   time.Time `json:"ts"`
	Kind Kind      `json:"kind"`

	Request               *RequestEvent               `json:"request,omitempty"`
	Fallback              *FallbackEvent              `json:"fallback,omitempty"`
	Selection             *SelectionEvent             `json:"selection,omitempty"`
	EngineStateChange     *EngineStateChangeEvent     `json:"engine_state_change,omitempty"`
	RoutingModeChange     *RoutingModeChangeEvent     `json:"routing_mode_change,omitempty"`
	PinnedPeerUnreachable *PinnedPeerUnreachableEvent `json:"pinned_peer_unreachable,omitempty"`
	ClaudeNodeChange      *ClaudeNodeChangeEvent      `json:"claude_node_change,omitempty"`
	ClaudeNodeFallback    *ClaudeNodeFallbackEvent    `json:"claude_node_fallback,omitempty"`
	PublicShareNudge      *PublicShareNudgeEvent      `json:"public_share_nudge,omitempty"`
}

// PublicShareNudgeEvent carries the pre-consent Public Share hint.
//
// Message ships as data, in plain English that assumes no knowledge of
// Waired internals, so the tray and the CLI render the same wording
// without each re-inventing it. Reason is a stable tag for what came up
// short ("no_candidate" / "all_overloaded") — for filtering, not for
// display.
//
// No node, account, tier or device identifier appears here, by
// construction: there is nothing to name.
type PublicShareNudgeEvent struct {
	Model   string `json:"model,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message"`
}

// PublicShareNudgeMessage is the user-facing copy for
// KindPublicShareNudge. Plain English, no internal vocabulary, and
// explicit that enabling shows a security warning first — the hint must
// never read as a silent opt-in.
const PublicShareNudgeMessage = "This request could not run on your own machines. " +
	"Turning on public sharing may let you use more capable machines shared by other people. " +
	"You will see a security and privacy warning before anything is enabled."

// RoutingModeChangeEvent records an operator transition of the
// inference routing mode. Mode strings are the string form of
// state.RoutingMode (kept untyped here so this package keeps no
// runtime/state dependency). PinnedPeerDeviceID and PinnedPeerName
// are populated when the destination is RoutingModePinned.
type RoutingModeChangeEvent struct {
	From               string `json:"from,omitempty"`
	To                 string `json:"to"`
	PinnedPeerDeviceID string `json:"pinned_peer_device_id,omitempty"`
	PinnedPeerName     string `json:"pinned_peer_name,omitempty"`
}

// PinnedPeerUnreachableEvent records a request that hit either the
// strict 503 (Reason="unreachable") or soft-fallback (Reason=
// "lacks_model") branch of the pinned routing path. Model carries
// the manifest model_id the request asked for so the operator can
// correlate "wrong model" cases against a specific request.
type PinnedPeerUnreachableEvent struct {
	PinnedPeerDeviceID string `json:"pinned_peer_device_id"`
	Model              string `json:"model,omitempty"`
	Reason             string `json:"reason"`
}

// ClaudeNodeChangeEvent records an operator transition of one Claude
// traffic class's serving node (#648). Class / Kind strings are the
// string forms of state.ClaudeClass* / state.ClaudeNodeTargetKind
// (kept untyped so this package keeps no runtime/state dependency).
type ClaudeNodeChangeEvent struct {
	Class        string `json:"class"`
	FromKind     string `json:"from_kind,omitempty"`
	FromPeer     string `json:"from_peer,omitempty"`
	ToKind       string `json:"to_kind"`
	PeerDeviceID string `json:"peer_device_id,omitempty"`
}

// ClaudeNodeFallbackEvent records one Claude request that could not be
// served on its class's pinned node and fell back to local serving
// (#648). Reason discriminates "unreachable" vs "model_not_ready".
type ClaudeNodeFallbackEvent struct {
	Class        string `json:"class"`
	PeerDeviceID string `json:"peer_device_id"`
	Reason       string `json:"reason"`
}

// RequestEvent summarizes one gateway request. Emitted on terminal
// response (success or error). FallbackFrom / FallbackReason are
// non-empty only when probe-then-commit fell back from the top-1
// candidate to a later one.
type RequestEvent struct {
	Kind           string `json:"kind"`
	Model          string `json:"model"`
	Decision       string `json:"decision"`
	PeerID         string `json:"peer_id,omitempty"`
	FallbackFrom   string `json:"fallback_from,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`
	Status         int    `json:"status"`
	LatencyMs      uint32 `json:"latency_ms"`
	ErrorReason    string `json:"error_reason,omitempty"`

	// Token counts as the upstream engine reported them, and the
	// coding-agent traffic class when the surface derived one
	// (waired#829, public share spec §12). All three are additive and
	// omitempty, so an event from a path that observed none is byte-
	// identical to before.
	//
	// Zero means "not observed", not "zero tokens": an engine that omits
	// usage, a client that disconnected mid-stream, or a compressed
	// response all land here.
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	Class        string `json:"class,omitempty"`
}

// FallbackEvent is emitted in addition to RequestEvent whenever the
// winner of probe-then-commit was not the top-1 candidate. It exists
// alongside RequestEvent so tray-style consumers can filter on
// kind=fallback without parsing every request body.
type FallbackEvent struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	Model  string `json:"model"`
}

// SelectionEvent records the selector's decision for one request,
// before probe-then-commit possibly overrides it. PeerID is empty
// for decision=local.
type SelectionEvent struct {
	Decision string `json:"decision"`
	PeerID   string `json:"peer_id,omitempty"`
	Model    string `json:"model"`
}

// EngineStateChangeEvent records transitions of the inference
// subsystem's externally observable state. From/To values are the
// Recorder-defined tags (ready, not_ready, paused, share_off).
type EngineStateChangeEvent struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

// DefaultRingCapacity is the ring size used by the agent when no
// override is configured. 16384 events at ~200 B average is ~3 MB,
// which is negligible for the agent process and gives ~hours of
// history at typical workloads.
const DefaultRingCapacity = 16384

// Ring is a fixed-size in-memory FIFO ring of Events keyed by a
// monotonic Seq counter. Concurrent appenders are serialized by mu.
// Seq starts at 1 on construction; Seq == 0 is reserved to mean
// "no event seen yet" so consumers can use it as the initial cursor.
type Ring struct {
	mu      sync.Mutex
	buf     []Event
	cap     int
	head    int
	count   int
	nextSeq uint64
}

// NewRing constructs a Ring with the given capacity. capacity <= 0
// falls back to DefaultRingCapacity.
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = DefaultRingCapacity
	}
	return &Ring{
		buf:     make([]Event, capacity),
		cap:     capacity,
		nextSeq: 1,
	}
}

// Append stores e in the ring, stamping Seq monotonically and TS
// (when zero) at append time. Returns the assigned Seq. Always
// succeeds; when the ring is full, the oldest entry is evicted.
func (r *Ring) Append(e Event) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	e.Seq = r.nextSeq
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	r.nextSeq++

	r.buf[r.head] = e
	r.head = (r.head + 1) % r.cap
	if r.count < r.cap {
		r.count++
		if r.count == r.cap {
			slog.Debug("observability ring reached capacity; oldest events evicted on further appends",
				"capacity", r.cap)
		}
	}
	return e.Seq
}

// Since returns events with Seq > since, optionally filtered by
// kinds (nil/empty = all). At most limit events are returned;
// limit <= 0 returns up to the full ring contents.
//
// oldestSeq is the Seq of the oldest event currently in the ring,
// or 0 if the ring is empty. gap is true when since > 0 and points
// to events that have been evicted (since < oldestSeq - 1); the
// consumer can surface a "history lost" indication in that case.
func (r *Ring) Since(since uint64, kinds []Kind, limit int) (events []Event, oldestSeq uint64, gap bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		return nil, 0, false
	}

	oldestSeq = r.nextSeq - uint64(r.count)
	if since > 0 && since+1 < oldestSeq {
		gap = true
		slog.Debug("observability ring history gap: consumer cursor behind oldest retained event",
			"since", since, "oldest_seq", oldestSeq)
	}

	start := r.head - r.count
	if start < 0 {
		start += r.cap
	}

	var kindSet map[Kind]struct{}
	if len(kinds) > 0 {
		kindSet = make(map[Kind]struct{}, len(kinds))
		for _, k := range kinds {
			kindSet[k] = struct{}{}
		}
	}

	if limit <= 0 {
		limit = r.count
	}
	events = make([]Event, 0, min(limit, r.count))
	for i := 0; i < r.count; i++ {
		idx := (start + i) % r.cap
		e := r.buf[idx]
		if e.Seq <= since {
			continue
		}
		if kindSet != nil {
			if _, ok := kindSet[e.Kind]; !ok {
				continue
			}
		}
		events = append(events, e)
		if len(events) >= limit {
			break
		}
	}
	return events, oldestSeq, gap
}

// LatestRequest returns the most recent RequestEvent in the ring,
// or nil if no request event has been recorded yet. Used by the
// /waired/v1/observability/state handler to populate last_inference.
func (r *Ring) LatestRequest() *Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == 0 {
		return nil
	}
	for i := 0; i < r.count; i++ {
		idx := (r.head - 1 - i + r.cap) % r.cap
		if r.buf[idx].Kind == KindRequest {
			e := r.buf[idx]
			return &e
		}
	}
	return nil
}
