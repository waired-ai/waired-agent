// Package router resolves a model alias (or model_id) to a concrete
// (engine, model variant) pair the agent can execute against.
//
// Phase A scope: only one local endpoint, single Ollama runtime, no
// remote peers, no scoring tie-breakers, no fallback chain. The full
// 7-step algorithm from waired_inference_spec.md §7.2 is implemented
// as a skeleton — the slots that Phase B/C/D fill (peer endpoints,
// download_penalty scoring, capacity-aware load shedding) are present
// as identifiable hooks but currently degenerate to "single
// candidate ⇒ pick it".
package router

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// Selection is the dry-run / actual routing decision returned to
// callers (LocalAPI, peers, gateway). The shape matches spec §7.3,
// plus EngineModel — the engine-specific identifier the gateway
// substitutes into the proxied request body (an Ollama tag such as
// "qwen3:8b-q4_K_M" today; a vLLM model path or HF repo id once
// Phase B lands).
//
// Release is the in-flight slot release callback set by Phase 7
// admission. Always non-nil — callers MUST defer-call it so a panic
// in the downstream proxy still frees the peer's tracked counter.
// For local selections and selections where admission is disabled,
// Release is a no-op closure. The field is JSON-hidden because it
// holds a closure over the Selector's internal state.
type Selection struct {
	EndpointID    string   `json:"endpoint_id"`
	ModelID       string   `json:"model_id"`
	VariantID     string   `json:"variant_id"`
	Runtime       string   `json:"runtime"`
	EngineModel   string   `json:"engine_model"`
	ExecutionMode string   `json:"execution_mode"` // "local" only in Phase A
	Decision      Decision `json:"decision"`
	Release       func()   `json:"-"`

	// PeerDisplayID is the identifier display surfaces must use for a
	// remote selection: the real DeviceID for own-network peers, the
	// grant pseudonym for Public Share peers whose real identifier must
	// never be shown (spec §8.5). Empty for local / external selections.
	PeerDisplayID string `json:"peer_display_id,omitempty"`

	// ContextWindow is the input-token window the SELECTED endpoint says
	// it serves — signer.InferenceState.ContextWindow as the chosen peer
	// advertised it (waired#1031). 0 for local and external selections,
	// and for a peer running an agent that predates the field.
	//
	// It exists because the #623 overflow guard had no way to ask. The
	// guard sized itself from the requesting device's own manifests and
	// own applied tuning even when dispatching to a peer, so a prompt
	// that overran the SERVING engine passed it and was truncated at the
	// head instead of compacted (waired-agent#436). A window is a
	// property of the endpoint that answers, not of the one that asks.
	//
	// 0 means "unknown" and callers must fall back to whatever they did
	// before rather than treating it as a zero-token window.
	ContextWindow int `json:"context_window,omitempty"`
}

// Decision is the human-readable trace of why this Selection won.
type Decision struct {
	Reason   []string            `json:"reason"`
	Fallback []FallbackCandidate `json:"fallback,omitempty"`
}

// FallbackCandidate is empty in Phase A but present so the wire shape
// stays stable as later phases populate it.
type FallbackCandidate struct {
	EndpointID string `json:"endpoint_id"`
	Runtime    string `json:"runtime"`
}

// Requirements are the hard constraints attached to one request.
type Requirements struct {
	MaxContextTokens int    `json:"max_context_tokens,omitempty"`
	NeedJSONMode     bool   `json:"need_json_mode,omitempty"`
	LatencyPriority  string `json:"latency_priority,omitempty"`
	PrivacyLevel     string `json:"privacy_level,omitempty"`
}

// Preferences are the soft policy flags attached to one request. All
// of these except AllowDownload are Phase B+ only; the field exists
// today so callers can populate it without breaking ABI.
type Preferences struct {
	AllowDownload     bool `json:"allow_download,omitempty"`
	AllowRemoteWorker bool `json:"allow_remote_worker,omitempty"`
	MaxDownloadGB     int  `json:"max_download_gb,omitempty"`
}

// Request is the input to Selector.Select.
//
// StickyID is the conversation hash the gateway computed
// (X-Waired-Conversation-Id header when present, else SHA-256 of the
// request body prefix) — Phase 7's mesh fallback uses it for KV
// cache affinity. Empty means "no affinity hint"; the Selector
// falls straight through to the score-based pick.
type Request struct {
	Model        string       `json:"model"`
	Requirements Requirements `json:"requirements"`
	Preferences  Preferences  `json:"preferences"`
	StickyID     string       `json:"sticky_id,omitempty"`

	// Class is the coding-agent traffic class ("main" / "sub",
	// state.ClaudeClass*) the gateway derived from the original client
	// model id (#645). The core Selector ignores it; per-surface
	// Selector wrappers (the agent's Claude-intercept selector) use it
	// to pick a per-class routing preference before delegating here.
	Class string `json:"class,omitempty"`

	// MinContextWindow is the input-token window the chosen endpoint must
	// declare it serves (waired#1031). Set by the gateway from the
	// /model tier the turn selected; 0 means the request makes no such
	// demand, which is every non-tier request.
	//
	// It is a hard filter and not a preference. A tier is a promise about
	// the SERVING node — Claude Code sized the session from the id before
	// this request existed — so an endpoint that cannot hold the window
	// is not a worse answer, it is a wrong one. When nothing qualifies
	// the selection fails with ErrNoEndpointForWindow, and the Claude
	// intercept's auto mode carries the turn to the real Anthropic API.
	//
	// An endpoint that declares NOTHING (0) passes: that is every agent
	// predating the field, and treating silence as failure would empty
	// the mesh the moment one node upgraded.
	MinContextWindow int `json:"min_context_window,omitempty"`
}

// Inputs bundles the world the selector reasons over: the known
// manifests, the local cache state, the local hardware profile, the
// runtime registry (which engines are wired up locally), and an
// optional mesh snapshot for Phase 4's peer-engine fallback.
type Inputs struct {
	Manifests  []catalog.Manifest
	LocalState catalog.State
	Hardware   hardware.Profile
	Runtimes   *runtime.Registry

	// DefaultModelID, when non-empty, is the model the dynamic coding
	// aliases (DynamicCodingAliases: waired/default, waired/coding)
	// resolve to — "whatever this host actually serves", computed by
	// the caller as preferred > persisted active > bundled (#632).
	// Empty falls back to static ModelAliases lookup, so callers that
	// never set it keep the historical behavior.
	DefaultModelID string

	// LocalContextWindow, when non-nil, reports the input-token window
	// THIS device declares it serves for its active model — the same
	// figure the agent publishes as InferenceState.ContextWindow, and 0
	// when it declares none (waired#1031).
	//
	// The Selector reads it only to answer Request.MinContextWindow: a
	// local candidate is dropped when the device declares a window and
	// that window falls short. nil, or 0, keeps the local candidate,
	// matching how an undeclared PEER is treated — a device that says
	// nothing is not a device that said no.
	LocalContextWindow func() int

	// MeshSnapshotFn, when non-nil, is called once per Select to
	// retrieve the current inferencemesh aggregator snapshot. The
	// Selector consults it ONLY when the locality filter would
	// otherwise return ErrModelNotReady, and picks the deterministically-
	// first peer whose InferenceState reports a matching engine model
	// AND is reachable+non-stale.
	//
	// nil disables peer-engine fallback. Wiring it nil is how the
	// agent's *overlay-side* Selector enforces loop prevention: a
	// peer-overlay request that fails the local-only locality filter
	// returns the same ErrModelNotReady the loopback-only Phase 1/2
	// Selector did, so a peer's gateway never recurses to a third
	// peer.
	MeshSnapshotFn func() inferencemesh.Snapshot

	// --- Phase 7 routing inputs --------------------------------------
	//
	// All five fields are optional. When nil/empty, the Selector
	// degrades gracefully:
	//
	//   - Sticky nil          → no affinity lookup, score-based pick wins
	//   - LocalInFlight nil   → no admission (all candidates eligible)
	//   - StickyInFlight nil  → no concurrent-sub spread, sticky-first as before
	//   - LocalRTT nil        → RTT tie-break skipped
	//   - LocalErrors nil     → error-rate tie-break skipped
	//
	// Tests inspecting only the deterministic deviceID tie-break leave
	// these nil and continue to work unchanged.

	// Sticky maps a conversation ID to its previously-routed peer.
	// Phase 7 Selector consults it first; on a hit that's still
	// reachable and under Capacity, the same peer wins (KV cache
	// affinity). Touch happens in the candidate's commit closure, so a
	// candidate the gateway probed and dropped never rebinds the
	// conversation. Expiry is lazy on Lookup; cmd/waired-agent sweeps
	// what Lookup never revisits (runStickyGC).
	Sticky *StickyStore

	// LocalInFlight tracks outstanding overlay requests this agent
	// has sent to each peer. Phase 7 admission asks
	// `InFlight(peer) < peer.Capacity` before committing a candidate
	// and Acquires the slot on the winner; the returned release is
	// embedded into Selection.Release for the gateway to defer.
	LocalInFlight *InFlightTracker

	// StickyInFlight counts those same outstanding requests per
	// (sticky key, peer) rather than per peer, which is what tells a
	// conversation's second CONCURRENT request apart from its next
	// sequential one. When the bound peer is already serving one,
	// demoteBusySticky moves it below the other candidates so the
	// sub-agent fan-out spreads instead of queueing on one machine
	// (waired-ai/waired#828); when it is idle the binding is honoured
	// unchanged and the KV prefix is reused. Acquired in the same
	// commit closure as LocalInFlight and released with it.
	//
	// nil disables the spread entirely: every request then takes the
	// plain sticky-first path, which is the pre-#828 behaviour.
	StickyInFlight *StickyInFlight

	// LocalRTT returns deviceID → recent observed overlay RTT (ms).
	// Used as the second tie-break after error rate. Closure shape so
	// the Selector pulls a fresh snapshot per call (the underlying
	// disco map can mutate between Selects).
	LocalRTT func() map[string]uint32

	// LocalErrors returns deviceID → fraction of recent overlay
	// requests this agent observed failing, in the 60 s sliding
	// window. Used as the first tie-break after the catalog score.
	LocalErrors func() map[string]float32

	// Recorder receives a RecordSelection emit each time SelectK
	// returns a non-empty candidate slice. nil disables the emit;
	// every other Selector behaviour is unchanged.
	Recorder Recorder

	// --- Public Share consumer inputs (waired#827) --------------------
	//
	// All three are optional and all three are left nil by the
	// overlay-side Selector (localOnlySelector), which must never apply
	// this device's outbound public-routing posture to a request that
	// arrived FROM a peer.

	// PublicPolicyFn returns the consumer's resolved Public Share
	// posture. Called once per SelectK that reaches the mesh, so the
	// implementation must be cheap — cmd/waired-agent serves it from an
	// atomic.Pointer refreshed on settings writes, not from disk. nil
	// (or a zero PublicPolicy) admits no public candidates.
	PublicPolicyFn func() PublicPolicy

	// OnPublicGrantDemand is called when policy would have used a public
	// candidate but this device holds no Public Share grant, so the
	// background acquirer can wake early instead of waiting out its
	// periodic tick (spec §4.3 cold start). Must not block: the
	// production implementation is a non-blocking send onto a
	// coalescing buffered channel.
	OnPublicGrantDemand func()

	// OnPublicGrantUsed is called with a Public Share grant's ID the moment
	// a request is committed to that grant's provider (Commit succeeds).
	// It is the ONLY signal that a held grant is carrying traffic, so the
	// background acquirer can renew grants in use and let idle ones lapse
	// (waired#898). Own-network and external candidates never call it.
	// Must not block: the production implementation records a timestamp
	// under a small lock. nil disables the report.
	OnPublicGrantUsed func(grantID string)

	// OnPublicNudge is called when a request could not be served by the
	// consumer's own nodes and no consent for Public Share has been
	// recorded. The receiver owns once-ness; the Selector emits on every
	// qualifying request and keeps no state.
	OnPublicNudge func(PublicNudge)

	// --- Manual routing override (Tailscale-exit-node style) -----
	//
	// RoutingMode controls how Step 3 (locality filter) of SelectK
	// picks between the local-ready engine and a mesh peer. Empty
	// value == RoutingModeAuto == current pre-feature behaviour.
	//
	// Sources: the workerController in cmd/waired-agent reads
	// state.RoutingPreference (operator's persisted choice from
	// <state-dir>/runtime/desired-worker, written by the tray /
	// `waired worker set`) and feeds it through the agent's
	// buildSelector closure on every SelectK.
	//
	// The overlay-side Selector (localOnlySelector) deliberately
	// leaves this empty so an overlay-arriving peer request never
	// applies this agent's outbound routing override — combined with
	// MeshSnapshotFn=nil it preserves the Phase 4 loop-prevention
	// guarantee.
	RoutingMode state.RoutingMode

	// PinnedPeerDeviceID is the operator-selected peer's DeviceID
	// when RoutingMode == RoutingModePinned. Ignored in other modes.
	PinnedPeerDeviceID string
}

// DynamicCodingAliases are the product-fixed model names that resolve
// to the host's *current* coding default (Inputs.DefaultModelID)
// instead of a static ModelAliases entry. They used to be pinned in
// qwen2.5-coder-7b-instruct.json, which broke the out-of-the-box
// `waired infer` on every host whose selected bundled model differs
// (#632). Size aliases like waired/small stay static — explicitly
// naming a size is a real request for that model.
var DynamicCodingAliases = []string{"waired/default", "waired/coding"}

// resolveModel maps a requested model name to a manifest: dynamic
// coding aliases go to DefaultModelID when it resolves, then the static
// LookupByAlias path, then the engine-native fallback for names that
// only the mesh puts on the wire, and finally the retirement table.
// Appends the resolution reason on success.
func (s *Selector) resolveModel(name string, reasons *[]string) (catalog.Manifest, bool) {
	if s.in.DefaultModelID != "" && slices.Contains(DynamicCodingAliases, name) {
		if m, ok := catalog.LookupByAlias(s.in.DefaultModelID, s.in.Manifests); ok {
			*reasons = append(*reasons, fmt.Sprintf(
				"alias %q resolved dynamically to this host's coding default %q", name, m.ModelID))
			return m, true
		}
	}
	if m, ok := catalog.LookupByAlias(name, s.in.Manifests); ok {
		*reasons = append(*reasons, fmt.Sprintf("alias %q resolved to model_id %q", name, m.ModelID))
		return m, true
	}
	// Engine-native fallback (#107). A request arriving from a mesh peer
	// names the model the way the ENGINE does, not the way the catalog
	// does: buildMeshCandidates matches the peer's advertised
	// InferenceState.Models against Source.Tag / Source.RepoID,
	// makeMeshCandidate carries the matched name as
	// Selection.EngineModel, and the gateway rewrites the proxied body's
	// `model` field to it. LookupByAlias only knows model_id and
	// model_aliases, and no bundled manifest lists its own engine tag as
	// an alias — so without this every peer hop 404s on the serving
	// side. Alias resolution keeps priority above, so this can only add
	// resolutions, never change one.
	if m, ok := lookupByEngineModel(name, s.in.Manifests); ok {
		*reasons = append(*reasons, fmt.Sprintf(
			"engine-native model %q resolved to model_id %q", name, m.ModelID))
		return m, true
	}
	// Retired names (#200). LAST, for the same reason the engine-native
	// fallback is: running after every live path means this can only add
	// resolutions, never redirect one. A name the catalog still answers is
	// answered by the catalog.
	//
	// Serving the successor rather than 404ing is the whole point of the
	// retirement map: the request came from a config file, a coding-agent
	// setting or a script written before the entry went away, and its
	// author is not present to fix it. The substitution is not silent —
	// the gateway rewrites the proxied body's `model` to the successor's
	// engine tag, so the response names what actually answered.
	if m, r, ok := catalog.ResolveModel(name, s.in.Manifests); ok && r.SuccessorModelID != "" {
		*reasons = append(*reasons, catalog.RetirementNotice(name, r))
		return m, true
	}
	return catalog.Manifest{}, false
}

// Sentinel errors. Wrap with %w so callers can use errors.Is.
var (
	ErrModelNotFound        = errors.New("router: model not found in catalog")
	ErrCapabilityNotMet     = errors.New("router: no variant satisfies the requested capabilities")
	ErrModelNotReady        = errors.New("router: model is not in ready state on disk")
	ErrHardwareInsufficient = errors.New("router: hardware does not meet variant requirements")
	ErrRuntimeNotInstalled  = errors.New("router: required runtime is not registered")
	// ErrNoEndpointForWindow is returned when Request.MinContextWindow is
	// set and no endpoint — local or mesh — declares a window that reaches
	// it (waired#1031). Distinct from ErrModelNotReady ("nobody has the
	// model") because the operator's remedy is different: the model is
	// there, the window is not, and the fix is a model or a machine that
	// can hold one. The Claude intercept's auto mode turns it into a
	// fallback to the real Anthropic API, which is the tier's contract.
	ErrNoEndpointForWindow = errors.New("router: no endpoint declares the required context window")
	// ErrAllPeersOverloaded is returned when at least one mesh peer
	// matched the request's model/runtime requirements but every
	// such peer's in-flight count was already at its advertised
	// Capacity. Phase 7 gateways turn this into HTTP 503
	// waired_all_peers_overloaded — distinct from ErrModelNotReady
	// ("no peer has the model at all") so operators can tell
	// "underprovisioned mesh" apart from "wrong model".
	ErrAllPeersOverloaded = errors.New("router: every matching mesh peer is at capacity")
	// ErrPinnedPeerUnreachable is returned when RoutingMode is
	// RoutingModePinned and the operator-selected peer is absent
	// from the mesh snapshot, stale, or marked unreachable by the
	// disco prober. Distinct from ErrAllPeersOverloaded and
	// ErrModelNotReady so the gateway can return a specific
	// `waired_pinned_peer_unreachable` 503 body — silent fallback
	// here would hide a user-initiated pin from the operator and was
	// rejected during the spec consultation
	// (docs/records/20260518/1530-routing-peer-pin-spec.md).
	ErrPinnedPeerUnreachable = errors.New("router: pinned peer is unreachable")
)

// PinnedPeerUnreachableError is what the Selector actually returns for
// ErrPinnedPeerUnreachable. It carries the pinned peer's identity so the
// gateway can name the peer in telemetry, response headers and the
// user-facing reroute notice without re-deriving it from the routing
// preference (which the gateway does not see).
//
// PeerDisplayID follows the same rule as Selection.PeerDisplayID: the grant
// pseudonym for a Public Share peer, the real DeviceID otherwise (spec
// §8.5). It is the only identifier that may reach an error body, a header
// or a log line.
//
// errors.Is(err, ErrPinnedPeerUnreachable) keeps working via Unwrap, so
// every existing sentinel comparison is unaffected.
type PinnedPeerUnreachableError struct {
	PeerDisplayID string
	ModelID       string
}

func (e *PinnedPeerUnreachableError) Error() string {
	return fmt.Sprintf("%s: %q", ErrPinnedPeerUnreachable.Error(), e.PeerDisplayID)
}

func (e *PinnedPeerUnreachableError) Unwrap() error { return ErrPinnedPeerUnreachable }

// Candidate is one option SelectK returns to the caller before any
// admission slot is consumed. The Phase 8 gateway probes each
// candidate's overlay /healthz endpoint in parallel (cheap, no GPU
// work) and calls Commit on the winning Candidate to atomically
// Acquire the admission slot — this two-phase pattern lets the
// gateway switch to a different peer when the snapshot turned out to
// be stale, without burning an inference attempt to find out.
//
// PeerID is the underlying mesh peer's DeviceID for "remote" mode.
// Empty for the "local" execution mode (no probing needed; those
// candidates commit directly).
//
// Decision mirrors Selection.Decision and is identical between
// candidate and the Selection that Commit produces.
type Candidate struct {
	EndpointID    string `json:"endpoint_id"`
	ModelID       string `json:"model_id"`
	VariantID     string `json:"variant_id"`
	Runtime       string `json:"runtime"`
	EngineModel   string `json:"engine_model"`
	ExecutionMode string `json:"execution_mode"`
	PeerID        string `json:"peer_id,omitempty"`
	// PeerDisplayID mirrors Selection.PeerDisplayID — the only peer
	// identifier a display surface may render.
	PeerDisplayID string   `json:"peer_display_id,omitempty"`
	Decision      Decision `json:"decision"`

	// Pinned marks this candidate as the operator's manually pinned
	// peer. The gateway uses it to tell "the pin itself failed its
	// /healthz probe" (→ ErrPinnedPeerUnreachable, naming the peer)
	// apart from "everything was busy" (→ ErrAllPeersOverloaded).
	// Before waired#729 the Selector filtered a silent pin out before
	// the probe layer ever saw it, so the distinction was made from
	// the snapshot alone; now the probe is what decides, and this flag
	// is how its verdict keeps the peer's name attached.
	Pinned bool `json:"pinned,omitempty"`

	// commit performs the InFlightTracker Acquire and sticky Touch
	// for this candidate. Captured at SelectK time so the call site
	// (gateway) can decide between candidates without coordinating
	// the admission machinery. Always non-nil for candidates returned
	// from SelectK; tests that construct Candidate by hand must
	// nil-check via Commit before invoking.
	commit func() (Selection, bool)
}

// Commit transitions this candidate from "probed-ready" to "owned by
// this request". It performs the InFlightTracker Acquire (for remote
// candidates) and touches the sticky store. Returns (Selection, true)
// on success or (zero, false) when Capacity was hit between SelectK
// and Commit — the caller's two-phase pattern is to walk the
// candidate slice on each Commit failure.
//
// For local and external candidates Commit always succeeds (no
// admission slot is consumed) and returns the Selection SelectK
// already constructed.
//
// Calling Commit on a zero-value Candidate (or one whose commit
// closure is nil) returns (zero, false).
func (c Candidate) Commit() (Selection, bool) {
	if c.commit == nil {
		return Selection{}, false
	}
	return c.commit()
}

// NewLocalCandidate wraps a pre-built Selection as a Candidate whose
// Commit always returns the same Selection. Useful for test fakes
// that want to short-circuit SelectK without dragging the full
// Selector implementation in, and for callers that need to construct
// a Candidate from a Selection produced outside the router package
// (e.g. the agent's overlay-side fast-path).
//
// PeerID is auto-derived from the Selection's Runtime when it starts
// with the "remote:" prefix, so the Phase 8 probe coordinator can
// identify the underlying peer. Despite the constructor name
// suggesting otherwise, remote Selections fed through here will
// still trigger the probe path — pre-Phase-8 test stubs depend on
// that to keep the Phase 4 transport coverage intact (the probe is
// answered with a 404 → ProbeLegacyPeer → assume ready).
func NewLocalCandidate(sel Selection) Candidate {
	c := Candidate{
		EndpointID:    sel.EndpointID,
		ModelID:       sel.ModelID,
		VariantID:     sel.VariantID,
		Runtime:       sel.Runtime,
		EngineModel:   sel.EngineModel,
		ExecutionMode: sel.ExecutionMode,
		Decision:      sel.Decision,
		commit:        func() (Selection, bool) { return sel, true },
	}
	if strings.HasPrefix(sel.Runtime, "remote:") {
		c.PeerID = strings.TrimPrefix(sel.Runtime, "remote:")
	}
	return c
}

// Selector implements the §7.2 7-step algorithm.
type Selector struct{ in Inputs }

// noopRelease is the placeholder Release closure for Selections where
// no admission slot was consumed (local routes, or a mesh route with
// LocalInFlight unset). Cheaper than returning a nil closure callers
// have to nil-check.
func noopRelease() {}

// NewSelector binds a Selector to a snapshot of inputs. Inputs are
// expected to be re-read between requests by the caller (the
// Hardware Profiler caches its own snapshot for 30s; the catalog
// State is reloaded after every download).
func NewSelector(in Inputs) *Selector { return &Selector{in: in} }

// Select is the single-Candidate convenience wrapper around SelectK.
// It picks K=1, calls Commit, and returns the resulting Selection.
// Phase 7 callers (including the loopback / overlay gateway path
// prior to the Phase 8 probe coordinator landing) continue to use
// this. Phase 8 callers prefer SelectK so the gateway can probe top-K
// peers in parallel before committing to one.
//
// When SelectK returns a candidate but Commit fails because Capacity
// was hit between SelectK and Commit, Select reports
// ErrAllPeersOverloaded — matching the Phase 7 "all saturated"
// contract.
func (s *Selector) Select(ctx context.Context, req Request) (Selection, error) {
	cands, err := s.SelectK(ctx, req, 1)
	if err != nil {
		return Selection{}, err
	}
	sel, ok := cands[0].Commit()
	if !ok {
		return Selection{}, fmt.Errorf("%w: %q (lost admission race at commit)",
			ErrAllPeersOverloaded, req.Model)
	}
	return sel, nil
}

// SelectK runs the same selection algorithm as Select but returns up
// to k ranked candidates without acquiring admission slots. The
// Phase 8 gateway probes each candidate's /healthz in parallel and
// calls Commit on the winning Candidate to atomically Acquire the
// admission slot.
//
// Returns 1 candidate (ExecutionMode = "local") when no probing is
// necessary — the local engine doesn't carry the cross-peer race the
// probe is designed to handle.
//
// For mesh fallback (ExecutionMode = "remote") returns up to k
// candidates ordered by:
//
//   - candidate[0] = sticky-bound peer when present in the eligible
//     set (KV-cache affinity, llm-d-style 87.4% hit baseline).
//   - candidate[1..] = score → error rate → RTT → deviceID.
//
// Error semantics mirror Select: ErrModelNotFound /
// ErrCapabilityNotMet / ErrModelNotReady / ErrAllPeersOverloaded.
func (s *Selector) SelectK(_ context.Context, req Request, k int) (cands []Candidate, err error) {
	if k < 1 {
		k = 1
	}
	reasons := []string{}

	// short records that the mesh could not supply a candidate, so the
	// Public Share side signals can be emitted from ONE place: the
	// deferred exit below, and only when SelectK really failed to serve
	// the request. Emitting inside tryMeshFallbackK would fire on paths
	// that still fall through to a healthy local engine
	// (RoutingModePeerPreferred with a ready local model, and the pinned
	// soft-fallback branch) — telling the user their request could not
	// run on their own machines while it did, and burning the one-shot
	// nudge on a false statement.
	var short publicShortfall
	modelID := ""
	defer func() {
		if err != nil {
			s.emitPublicShortfall(short, modelID)
		}
	}()

	// Emit one selection event per successful return with at least
	// one candidate. The first candidate's ExecutionMode is the
	// decision class (SelectK groups by class), so cands[0] is
	// representative.
	defer func() {
		if err != nil || len(cands) == 0 || s.in.Recorder == nil {
			return
		}
		c := cands[0]
		// Display identifier: the SelectionEvent lands in the
		// observability ring, which the management API serves and the
		// tray renders (spec §8.5).
		peerID := c.PeerDisplayID
		if peerID == "" {
			peerID = c.PeerID
		}
		s.in.Recorder.RecordSelection(c.ExecutionMode, peerID, c.ModelID)
	}()

	// Step 1: alias resolution.
	manifest, ok := s.resolveModel(req.Model, &reasons)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrModelNotFound, req.Model)
	}
	modelID = manifest.ModelID

	// Step 2: capability filter.
	if req.Requirements.MaxContextTokens > 0 && manifest.ContextLength < req.Requirements.MaxContextTokens {
		return nil, fmt.Errorf("%w: %q context_length=%d < requested=%d",
			ErrCapabilityNotMet, manifest.ModelID, manifest.ContextLength, req.Requirements.MaxContextTokens)
	}
	if req.Requirements.NeedJSONMode && !hasCapability(manifest.Capabilities, "json_mode") {
		return nil, fmt.Errorf("%w: %q lacks json_mode", ErrCapabilityNotMet, manifest.ModelID)
	}
	reasons = append(reasons, fmt.Sprintf("capability filter passed (context_length=%d, json_mode=%v)",
		manifest.ContextLength, hasCapability(manifest.Capabilities, "json_mode")))

	// Step 3: locality filter / mesh fallback / external fallback.
	modelState, present := s.in.LocalState.Models[manifest.ModelID]
	localReady := present && modelState.State == catalog.ModelStateReady

	// Tailscale-exit-node-style manual routing override. Empty mode
	// (= the historical pre-feature default) falls through to the
	// auto branch below.
	switch s.in.RoutingMode {
	case state.RoutingModeLocalOnly:
		// Operator chose "local engine only — never call out to the
		// mesh." 503 with ErrModelNotReady is the right shape; an
		// inference probe that uses local-only on a host with no
		// engine is the user-visible signal we want.
		if !localReady {
			return nil, fmt.Errorf("%w: %q state=%q (routing=local-only)",
				ErrModelNotReady, manifest.ModelID, modelStateOf(modelState, present))
		}
		reasons = append(reasons, fmt.Sprintf("local state for %q is %q (routing=local-only)",
			manifest.ModelID, modelState.State))
		// Fall through to the local-ready candidate construction below.
	case state.RoutingModePeerPreferred:
		// Mesh first; fall back to local engine only if no mesh peer
		// can serve the request.
		if s.in.MeshSnapshotFn != nil {
			cands, err := s.tryMeshFallbackK(req, manifest, reasons, k, &short)
			if err != nil {
				return nil, fmt.Errorf("%w: %q (%v)", err, manifest.ModelID, err)
			}
			if len(cands) > 0 {
				return cands, nil
			}
		}
		if !localReady {
			return nil, fmt.Errorf("%w: %q state=%q (routing=peer-preferred, no mesh candidate)",
				ErrModelNotReady, manifest.ModelID, modelStateOf(modelState, present))
		}
		reasons = append(reasons, fmt.Sprintf("local state for %q is %q (routing=peer-preferred, no mesh candidate)",
			manifest.ModelID, modelState.State))
		// Fall through to the local-ready candidate construction below.
	case state.RoutingModePeerOnly:
		// Mesh only, fail closed. The mirror image of local-only: the
		// operator asked for this machine NOT to serve, so a silent
		// local run would defeat the choice exactly the way the Claude
		// surface's silent fallback did (#325). A ready local model is
		// therefore deliberately not consulted, and MeshSnapshotFn==nil
		// (the overlay-side Selector, where this mode should never have
		// been set) fails rather than degrading to local.
		if s.in.MeshSnapshotFn == nil {
			return nil, fmt.Errorf("%w: %q state=%q (routing=peer-only, no mesh snapshot)",
				ErrModelNotReady, manifest.ModelID, modelStateOf(modelState, present))
		}
		cands, err := s.tryMeshFallbackK(req, manifest, reasons, k, &short)
		if err != nil {
			return nil, fmt.Errorf("%w: %q (%v)", err, manifest.ModelID, err)
		}
		if len(cands) > 0 {
			return cands, nil
		}
		return nil, fmt.Errorf("%w: %q state=%q (routing=peer-only, no mesh candidate)",
			ErrModelNotReady, manifest.ModelID, modelStateOf(modelState, present))
	case state.RoutingModePinned:
		// Pin to a specific peer. tryMeshFallbackK handles the
		// strict / soft semantics: pin-unreachable returns
		// ErrPinnedPeerUnreachable; pin-reachable-but-lacks-model
		// soft-falls through to the rest of the eligible mesh.
		// MeshSnapshotFn==nil happens on the overlay-side Selector,
		// where this mode should never have been set in the first
		// place — fall back to the local-only treatment defensively
		// so an inadvertent overlay-side pin can't loop a peer's
		// gateway through itself.
		if s.in.MeshSnapshotFn == nil {
			if !localReady {
				return nil, fmt.Errorf("%w: %q state=%q (routing=pinned, no mesh snapshot)",
					ErrModelNotReady, manifest.ModelID, modelStateOf(modelState, present))
			}
			reasons = append(reasons, "routing=pinned: no mesh snapshot, falling back to local-ready")
			// fall through to local-ready candidate construction.
		} else {
			cands, err := s.tryMeshFallbackK(req, manifest, reasons, k, &short)
			if err != nil {
				return nil, fmt.Errorf("%w: %q (%v)", err, manifest.ModelID, err)
			}
			if len(cands) > 0 {
				return cands, nil
			}
			// No mesh candidate matched the request, and pin was
			// either reachable-but-lacks-model (soft path inside
			// tryMeshFallbackK already returned [] or returned the
			// rest of the eligible peers) or the mesh had no
			// matching peer at all. In both cases the right shape
			// is ErrModelNotReady — silently bouncing to local
			// would defeat the explicit operator pin.
			return nil, fmt.Errorf("%w: %q state=%q (routing=pinned, no mesh candidate)",
				ErrModelNotReady, manifest.ModelID, modelStateOf(modelState, present))
		}
	default:
		// RoutingModeAuto or empty: historical pre-feature behaviour.
		if !localReady {
			if s.in.MeshSnapshotFn != nil {
				cands, err := s.tryMeshFallbackK(req, manifest, reasons, k, &short)
				if err != nil {
					return nil, fmt.Errorf("%w: %q (%v)", err, manifest.ModelID, err)
				}
				if len(cands) > 0 {
					return cands, nil
				}
			}
			return nil, fmt.Errorf("%w: %q state=%q",
				ErrModelNotReady, manifest.ModelID, modelStateOf(modelState, present))
		}
		reasons = append(reasons, fmt.Sprintf("local state for %q is %q", manifest.ModelID, modelState.State))
	}

	// Local-ready path. The variant + engine + hardware checks are
	// identical to the Phase 7 Select; only the return shape differs.
	variant, ok := findVariant(manifest, modelState.VariantID)
	if !ok {
		return nil, fmt.Errorf("%w: variant %q not in manifest", ErrModelNotReady, modelState.VariantID)
	}
	engine := pickEngine(variant, s.in.Hardware)
	if engine == "" {
		return nil, fmt.Errorf("%w: variant %q has no compatible engine on this host",
			ErrHardwareInsufficient, variant.VariantID)
	}
	// Memory fit is ADVISORY at serve time (#61). This model is already
	// local-ready — the user selected and pulled it, and an over-spec pick
	// is gated by a confirmation at selection time (tray / CLI) — so never
	// hard-block here: a tight fit should serve and let the engine surface a
	// genuine OOM rather than a pre-emptive 422 (the old naive
	// RAMTotalGB < MinRAMGB gate returned ErrHardwareInsufficient here).
	// hostFits prices the model against the machine's TOTAL memory at the
	// window it would be given, so the reason no longer mislabels
	// Mac / Strix Halo as RAM-short, and no longer refuses a host for a
	// hand-authored min_ram_gb it clears in practice.
	if hostFits(engine, manifest, variant, s.in.Hardware) {
		reasons = append(reasons, fmt.Sprintf("hardware ok (variant min_ram=%d, host total_ram=%d)",
			variant.MinRAMGB, s.in.Hardware.RAMTotalGB))
	} else {
		reasons = append(reasons, fmt.Sprintf("hardware below recommended: %s — serving anyway (#61)",
			deficitLabelFor(variant, engine, s.in.Hardware)))
	}
	reasons = append(reasons, fmt.Sprintf("engine %q selected", engine))
	if _, ok := s.in.Runtimes.Lookup(engine); !ok {
		return nil, fmt.Errorf("%w: %q (variant %q)",
			ErrRuntimeNotInstalled, engine, variant.VariantID)
	}

	// waired#1031: the tier filter, local half. Same rule as the mesh
	// half in buildMeshCandidates — a device that declares a window and
	// falls short is not an answer to a tiered request; one that declares
	// nothing is left alone.
	if req.MinContextWindow > 0 && s.in.LocalContextWindow != nil {
		if w := s.in.LocalContextWindow(); w > 0 && w < req.MinContextWindow {
			return nil, fmt.Errorf("%w: this device serves %d, request needs %d",
				ErrNoEndpointForWindow, w, req.MinContextWindow)
		}
	}

	localSel := Selection{
		EndpointID:    computeEndpointID("local", engine, manifest.ModelID),
		ModelID:       manifest.ModelID,
		VariantID:     variant.VariantID,
		Runtime:       engine,
		EngineModel:   engineModelFor(engine, variant, modelState),
		ExecutionMode: "local",
		Decision:      Decision{Reason: reasons, Fallback: nil},
		Release:       noopRelease,
	}
	return []Candidate{{
		EndpointID:    localSel.EndpointID,
		ModelID:       localSel.ModelID,
		VariantID:     localSel.VariantID,
		Runtime:       localSel.Runtime,
		EngineModel:   localSel.EngineModel,
		ExecutionMode: "local",
		Decision:      localSel.Decision,
		commit:        func() (Selection, bool) { return localSel, true },
	}}, nil
}

// meshCandidate is one peer eligible to serve the request, plus the
// per-peer signals the Phase 7 scoring + tie-break chain consumes.
type meshCandidate struct {
	deviceID string
	// displayID is the only identifier that may be shown for this peer.
	// Equal to deviceID for own-network peers; the grant pseudonym for
	// public peers, whose real device identifier must never reach a
	// header, an event, a log line or a CLI surface (spec §8.5).
	displayID string
	// public marks a Public Share provider injected from a foreign
	// network. It is the dominant sort key (sortMeshCandidates) —
	// own == team > public, per the Team Share routing-order decision.
	public bool
	// grantID is the Public Share grant this candidate routes under (the
	// netmap PeerView.Grant.ID), set only when public. Reported to the
	// background acquirer on Commit (OnPublicGrantUsed) so a grant that is
	// actually carrying traffic gets renewed and an idle one lapses —
	// closing the "idle consumer holds a stranger's peering forever" gap
	// (waired#898). Empty for own-network peers.
	grantID string
	variant catalog.Variant
	runtime string // catalog.RuntimeOllama or catalog.RuntimeVLLM
	tag     string
	// contextWindow is the window this peer says its engine is loaded
	// with for tag (InferenceState.ContextWindow). 0 = the peer declares
	// nothing, which is also what every agent predating the field sends,
	// so it must read as "unknown" and never as "serves nothing".
	contextWindow int
	// priority is the admin routing preference the CP folded into the peer's
	// InferenceState: High(1) / Middle(0) / Low(-1). It is the dominant sort
	// key (sortMeshCandidates), so among peers that can serve the request the
	// higher-priority ones are chosen first; overloaded high-priority peers
	// still drop out via the capacity admission filter, falling back to the
	// next tier. 0 (Middle) for agents/devices that predate or don't set it.
	priority int
	// silent is the disco prober's advisory "this peer pong'd once and
	// has gone quiet since" verdict (inferencemesh.PeerView.Silent). It
	// is a SORT key, never a filter (waired#729): the pong rides raw UDP
	// or the relay's WSS control session, not the WireGuard data plane
	// an inference request uses, so it cannot veto a peer whose data
	// plane is demonstrably carrying traffic. Ordering silent peers last
	// keeps the probeFanoutK slots pointed at the peers most likely to
	// answer while still letting a silent peer win when it is all we
	// have.
	silent    bool
	capacity  int     // 0 == unlimited
	score     int64   // ParamCount × QuantizationTier; 0 when catalog inputs unset
	errorRate float32 // 0 when LocalErrors snapshot missing this peer
	rttMS     uint32  // math.MaxUint32 when LocalRTT snapshot missing this peer

	// inFlight is this agent's count of outstanding overlay requests to
	// the peer (LocalInFlight.Snapshot()); 0 when the tracker is unwired
	// or the peer is absent from the snapshot.
	inFlight int32
	// loadFraction is inFlight / effectiveCapacity — the Phase 7
	// weighted-least-loaded balancing axis (sortMeshCandidates). It
	// distributes traffic across peers that tie on score, error rate
	// and RTT band, proportional to advertised Capacity (a Capacity=8
	// box absorbs ~4× the load of a Capacity=2 box at equal fraction).
	// 0 when LocalInFlight is nil, so a Selector with no admission
	// wiring degrades to the deterministic deviceID-asc tie-break the
	// pre-balancing tests rely on. Capacity==0 ("unlimited" admission)
	// is weighted as effectiveCapacity=1 here — balancing weight is
	// deliberately independent of the (uncapped) admission gate.
	loadFraction float64
}

// tryMeshFallbackK builds up to k mesh candidates for the request.
// The Phase 8 routing chain:
//
//  1. Filter — peers in the snapshot that (a) advertise a model
//     matching one of the manifest's variants and (b) are non-stale
//     and reachable per the snapshot.
//  2. Sort — silence → score → error rate → RTT → deviceID (Phase 7
//     ordering, with the waired#729 advisory key in front).
//  3. Sticky-first — if a StickyID resolves to a peer in the filtered
//     set, hoist it to position 0 so the gateway probes / commits it
//     first.
//  4. Admission pre-filter — drop peers whose LocalInFlight already
//     equals or exceeds Capacity. If everything is filtered out at
//     this step, ErrAllPeersOverloaded (Phase 7 contract).
//  5. Materialise — wrap the first k eligible meshCandidates as
//     []Candidate with commit closures that perform the actual
//     Acquire + sticky Touch at commit time.
//
// Returns (nil, nil) when no peer matches the model — caller turns
// that into ErrModelNotReady.
//
// Loop prevention: overlay-side Selectors receive MeshSnapshotFn=nil
// and never reach this function.
func (s *Selector) tryMeshFallbackK(req Request, manifest catalog.Manifest, reasons []string, k int, short *publicShortfall) ([]Candidate, error) {
	snap := s.in.MeshSnapshotFn()
	wantOllama, wantVLLM := variantWantSets(manifest)
	if len(wantOllama) == 0 && len(wantVLLM) == 0 {
		return nil, nil
	}

	// Public Share admission gate for this request (waired#827). Resolved
	// once per call; its own-best-tier input is filled lazily inside
	// buildMeshCandidates, only if a grant-tagged peer actually appears.
	gate := s.publicGateFor(req.Class)

	raw := s.buildMeshCandidates(snap, req.Class, req.MinContextWindow, wantOllama, wantVLLM, &gate)
	if len(raw) == 0 {
		short.record(snap, gate, NudgeReasonNoCandidate)
		// Manual pin needs a separate strict check: when the operator
		// has pinned a peer that is not in the snapshot at all (down,
		// stale, disco-unreachable), the request must surface 503
		// ErrPinnedPeerUnreachable rather than the generic
		// ErrModelNotReady the auto branch produces. The pin-reachable-
		// but-lacks-model soft case can't reach this branch because
		// raw is empty only when no peer carries the model.
		if s.in.RoutingMode == state.RoutingModePinned && s.in.PinnedPeerDeviceID != "" {
			if !pinReachableInSnapshot(snap, s.in.PinnedPeerDeviceID) {
				return nil, s.pinUnreachable(snap, manifest.ModelID)
			}
		}
		return nil, nil
	}

	sortMeshCandidates(raw)
	raw = applyStickyFirst(req, s.in.Sticky, raw)

	// Manual pin override applied AFTER sticky so a deliberate operator
	// pin always wins over a sticky cache hit. Three behaviours:
	//
	//   1. Pin reachable + serves the requested alias → hoist to the
	//      head of the candidate slice (strict pin).
	//   2. Pin reachable + does NOT serve the requested alias → leave
	//      `raw` unchanged so the request soft-falls through to another
	//      peer that does. The user has indicated "use this GPU
	//      machine" rather than "use this exact (peer, model)" — see
	//      the spec consultation outcome.
	//   3. Pin absent from the snapshot / stale / disco-unreachable →
	//      503 ErrPinnedPeerUnreachable. Silent fallback was rejected
	//      because it would hide an explicit operator action.
	if s.in.RoutingMode == state.RoutingModePinned && s.in.PinnedPeerDeviceID != "" {
		hoisted := false
		for i, c := range raw {
			if c.deviceID != s.in.PinnedPeerDeviceID {
				continue
			}
			if i != 0 {
				out := make([]meshCandidate, 0, len(raw))
				out = append(out, c)
				out = append(out, raw[:i]...)
				out = append(out, raw[i+1:]...)
				raw = out
			}
			hoisted = true
			break
		}
		if !hoisted {
			// Pin not in the filtered candidate set. Distinguish
			// "pin is up but lacks the model" (soft fallback) from
			// "pin is unreachable" (strict 503) using the full snapshot.
			if !pinReachableInSnapshot(snap, s.in.PinnedPeerDeviceID) {
				return nil, s.pinUnreachable(snap, manifest.ModelID)
			}
			// Soft fallback: leave raw alone, let scoring decide. Emit
			// a lacks_model event so the tray surfaces the silent miss.
			if s.in.Recorder != nil {
				s.in.Recorder.RecordPinnedPeerUnreachable(s.in.PinnedPeerDeviceID, manifest.ModelID, "lacks_model")
			}
		}
	}

	// Re-assert the own > public partition (waired#827). Both hoists
	// above move a candidate to index 0 by deviceID alone, which would
	// otherwise let a public peer outrank every own peer: sticky binds
	// to whatever served the previous turn, and makeMeshCandidate's
	// commit closure Touches the sticky store for public peers too, so
	// one public selection would pin the conversation there for the
	// 5-minute sticky TTL. SliceStable keeps each hoist's effect intact
	// within its own partition.
	partitionOwnFirst(raw)

	// Admission pre-filter: drop peers we already know are at
	// capacity. The Commit closure rechecks via Acquire at commit
	// time so a concurrent request stealing the last slot fails
	// cleanly (ok=false) and the gateway falls to the next
	// candidate.
	var eligible []meshCandidate
	for _, c := range raw {
		if s.in.LocalInFlight != nil && c.capacity > 0 &&
			int(s.in.LocalInFlight.InFlight(c.deviceID)) >= c.capacity {
			continue
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		short.record(snap, gate, NudgeReasonAllOverloaded)
		return nil, ErrAllPeersOverloaded
	}

	// Concurrent-sub spread (waired-ai/waired#828). Runs last, on the
	// final ordering: the tier partition and the capacity filter have
	// both already had their say, so "the slot after this one" means
	// what it says.
	eligible, spreadFrom := s.demoteBusySticky(req, eligible, k)

	if k > len(eligible) {
		k = len(eligible)
	}
	out := make([]Candidate, 0, k)
	for i := 0; i < k; i++ {
		out = append(out, s.makeMeshCandidate(req, manifest, reasons, eligible[i], eligible, spreadFrom))
	}
	return out, nil
}

// makeMeshCandidate freezes one meshCandidate into the Candidate
// shape, capturing everything Commit needs in a closure. The closure
// performs the actual InFlightTracker Acquire and sticky Touch, so
// SelectK never modifies global state — only the gateway's call to
// Commit does.
//
// spreadFrom names the peer demoteBusySticky moved out of the way, or
// "" when the sticky binding was left alone. It is what tells the
// commit closure that landing somewhere else is deliberate rather than
// a new preference.
func (s *Selector) makeMeshCandidate(req Request, manifest catalog.Manifest, reasons []string, c meshCandidate, all []meshCandidate, spreadFrom string) Candidate {
	kindLabel := "mesh fallback"
	if c.public {
		kindLabel = "public share fallback"
	}
	candReasons := append(append([]string{}, reasons...),
		fmt.Sprintf("local state for %q is not ready", manifest.ModelID),
		fmt.Sprintf("%s: peer %q has %s model %q reachable (score=%d, err=%.2f, rtt_ms=%d, in_flight=%d, cap=%d, load=%.2f, silent=%v)",
			kindLabel, c.displayID, c.runtime, c.tag, c.score, c.errorRate, c.rttMS, c.inFlight, c.capacity, c.loadFraction, c.silent),
	)
	if spreadFrom != "" {
		// Named on every candidate of the round, not just the demoted
		// one, because `waired infer --explain` prints the reasons of
		// the peer that WON and "why am I not on my usual node" is the
		// question being answered.
		candReasons = append(candReasons,
			fmt.Sprintf("conversation is already being served by its sticky-bound peer; demoted it for this request (%d concurrent)",
				s.in.StickyInFlight.InFlight(req.StickyID, spreadFrom)))
	}
	decision := Decision{
		Reason:   candReasons,
		Fallback: fallbackTrace(all, c.deviceID),
	}
	// EndpointID is built from displayID, not deviceID: computeEndpointID
	// is plain string concatenation, not a hash, and the whole Selection
	// is serialised verbatim by the management API's /inference/select
	// and printed by `waired infer --explain`. It is opaque by contract
	// and nothing parses a peer back out of it, so substituting the
	// pseudonym costs nothing and keeps a foreign device identifier off
	// a user-facing surface (spec §8.5).
	endpointID := computeEndpointID("remote-"+c.displayID, c.runtime, manifest.ModelID)
	// Runtime stays keyed on the real deviceID — it is functional, the
	// peer adapter resolves the dial target from it. Display sites
	// substitute PeerDisplayID instead of printing it.
	runtimeStr := "remote:" + c.deviceID
	return Candidate{
		EndpointID:    endpointID,
		ModelID:       manifest.ModelID,
		VariantID:     c.variant.VariantID,
		Runtime:       runtimeStr,
		EngineModel:   c.tag,
		ExecutionMode: "remote",
		PeerID:        c.deviceID,
		PeerDisplayID: c.displayID,
		Decision:      decision,
		Pinned: s.in.RoutingMode == state.RoutingModePinned &&
			s.in.PinnedPeerDeviceID != "" &&
			c.deviceID == s.in.PinnedPeerDeviceID,
		commit: func() (Selection, bool) {
			release, ok := s.acquireSlot(c)
			if !ok {
				return Selection{}, false
			}
			// Counted from the same point and given back with the same
			// closure as the admission slot, so the two can never
			// disagree about whether this request is outstanding.
			release = chainRelease(s.acquireStickySlot(req.StickyID, c.deviceID), release)
			// A public grant is "used" exactly when a request is committed
			// to its provider — after admission succeeds, so a candidate we
			// probed but dropped for capacity is not counted (waired#898).
			if c.public && c.grantID != "" {
				s.notifyPublicGrantUsed(c.grantID)
			}
			// A request that was spread off its bound peer does NOT
			// rebind the conversation (waired-ai/waired#828). The
			// binding describes where this conversation's prefix lives;
			// a sub-agent sent elsewhere because that peer was busy is a
			// deliberate one-request detour, and letting it move the
			// binding would walk the whole conversation onto whichever
			// peer happened to take the last overlapping sub. The bound
			// peer winning as the last resort still refreshes the TTL.
			spread := spreadFrom != "" && c.deviceID != spreadFrom
			if s.in.Sticky != nil && req.StickyID != "" && !spread {
				s.in.Sticky.Touch(req.StickyID, c.deviceID)
			}
			return Selection{
				EndpointID:    endpointID,
				ModelID:       manifest.ModelID,
				VariantID:     c.variant.VariantID,
				Runtime:       runtimeStr,
				EngineModel:   c.tag,
				ExecutionMode: "remote",
				PeerDisplayID: c.displayID,
				ContextWindow: c.contextWindow,
				Decision:      decision,
				Release:       release,
			}, true
		},
	}
}

// pinReachableInSnapshot reports whether the pinned peer is present
// in the mesh snapshot AND its inferencemesh aggregator flags
// (Reachable, !Stale) agree the peer is currently usable.
//
// PeerView.Silent is deliberately NOT consulted: disco silence must
// never strict-503 a pin (waired#729). An operator pin is an explicit
// "use this machine"; silence is an inference drawn from a channel the
// request does not even travel. A silent pin therefore stays reachable
// here, gets hoisted, and is probed — and if that probe fails, the
// gateway names it via ErrPinnedPeerUnreachable.
//
// The "reachable but lacks our requested model" case is intentionally
// NOT detected here — that case is the soft-fallback path callers
// want, and is determined by the model-match filter in
// buildMeshCandidates rather than by this function.
func pinReachableInSnapshot(snap inferencemesh.Snapshot, pin string) bool {
	if pin == "" {
		return false
	}
	for _, p := range snap.Peers {
		if p.DeviceID != pin {
			continue
		}
		if p.Stale {
			return false
		}
		if p.InferenceState == nil || !p.InferenceState.Reachable {
			return false
		}
		return true
	}
	return false
}

// pinUnreachable emits the strict-pin event and builds the error for the
// "operator pinned a peer that cannot serve right now" case. Both call
// sites in tryMeshFallbackK go through it so the recorded event and the
// returned error shape cannot drift apart.
func (s *Selector) pinUnreachable(snap inferencemesh.Snapshot, modelID string) error {
	if s.in.Recorder != nil {
		s.in.Recorder.RecordPinnedPeerUnreachable(s.in.PinnedPeerDeviceID, modelID, "unreachable")
	}
	return &PinnedPeerUnreachableError{
		PeerDisplayID: pinDisplayID(snap, s.in.PinnedPeerDeviceID),
		ModelID:       modelID,
	}
}

// pinDisplayID is the identifier that may be shown for the pinned peer.
// A Public Share peer must be named by its grant pseudonym and never by
// its real device id (spec §8.5). A pin that is absent from the snapshot
// can only be named by the id the operator configured — we have nothing
// else, and it is their own device id in every case a pin is settable
// from the tray's own-network peer list.
func pinDisplayID(snap inferencemesh.Snapshot, pin string) string {
	for i := range snap.Peers {
		if snap.Peers[i].DeviceID != pin {
			continue
		}
		if ps, ok := publicDisplayID(snap.Peers[i].Grant); ok {
			return ps
		}
		break
	}
	return pin
}

// applyStickyFirst hoists the sticky-bound peer to the head of the
// candidate slice if it exists in the slice. Otherwise returns cands
// unchanged. Phase 8 callers run this AFTER sortMeshCandidates so the
// sticky binding overrides the score-based tie-break.
func applyStickyFirst(req Request, store *StickyStore, cands []meshCandidate) []meshCandidate {
	if store == nil || req.StickyID == "" {
		return cands
	}
	stuckTo, ok := store.Lookup(req.StickyID)
	if !ok {
		return cands
	}
	for i, c := range cands {
		if c.deviceID != stuckTo {
			continue
		}
		if i == 0 {
			return cands
		}
		out := make([]meshCandidate, 0, len(cands))
		out = append(out, c)
		out = append(out, cands[:i]...)
		out = append(out, cands[i+1:]...)
		return out
	}
	return cands
}

// demoteBusySticky is the concurrent-sub spread (waired-ai/waired#828).
// applyStickyFirst put the conversation's bound peer at the head; if
// that peer is ALREADY serving a request for the same sticky key, this
// moves it back down so the next-ranked candidate leads instead, and
// reports which peer the request was spread away from ("" when nothing
// moved). A coding agent that fans a turn out to several sub-agents
// otherwise queues all of them on one machine while the rest of the
// mesh idles.
//
// The rule that keeps affinity intact is the in-flight count itself:
// sequential requests of one conversation never overlap, so the count
// is 0 when the next one arrives and the binding is honoured unchanged.
// Spreading them too would rebuild the KV prefix on a new peer every
// turn, which is the cost sticky routing exists to avoid.
//
// Two orderings are deliberately NOT overridden:
//
//   - An operator's manual pin. "Use this machine" outranks a
//     balancing preference, so a pinned bound peer stays put.
//   - The own > public partition (waired#827). The peer is demoted
//     within its own grant tier only: spreading an owner's traffic onto
//     a stranger's machine because two of their own sub-agents
//     overlapped would be a much bigger decision than this one.
//
// It also stays inside the probe window (the first k candidates the
// gateway actually probes). Demoting past it would silently drop a
// healthy peer from the round, and a round where every probed peer
// failed would then 503 with the bound peer sitting there able to serve
// — the point is to prefer another peer, not to refuse this one.
func (s *Selector) demoteBusySticky(req Request, cands []meshCandidate, k int) ([]meshCandidate, string) {
	if s.in.Sticky == nil || s.in.StickyInFlight == nil || req.StickyID == "" {
		return cands, ""
	}
	stuckTo, ok := s.in.Sticky.Lookup(req.StickyID)
	if !ok {
		return cands, ""
	}
	if s.in.RoutingMode == state.RoutingModePinned && s.in.PinnedPeerDeviceID == stuckTo {
		return cands, ""
	}
	if s.in.StickyInFlight.InFlight(req.StickyID, stuckTo) == 0 {
		return cands, ""
	}
	idx := -1
	for i, c := range cands {
		if c.deviceID == stuckTo {
			idx = i
			break
		}
	}
	if idx < 0 {
		return cands, ""
	}
	// Last slot inside the probe window that belongs to the same grant
	// tier. j == idx means the bound peer is the only candidate this
	// request may be spread to, so there is nothing to demote it below.
	window := min(k, len(cands))
	j := -1
	for i := 0; i < window; i++ {
		if cands[i].public == cands[idx].public {
			j = i
		}
	}
	if j <= idx {
		return cands, ""
	}
	out := make([]meshCandidate, 0, len(cands))
	out = append(out, cands[:idx]...)
	out = append(out, cands[idx+1:j+1]...)
	out = append(out, cands[idx])
	out = append(out, cands[j+1:]...)
	return out, stuckTo
}

// buildMeshCandidates filters the snapshot to peers that (a) carry
// a model matching one of manifest's variants and (b) are reachable
// and non-stale per the inferencemesh aggregator.
//
// The disco prober's recent-pong verdict (PeerView.Silent) is NOT a
// filter here. It used to be — Phase 8 hard-excluded on it — and that
// black-holed peers whose WireGuard data plane was demonstrably
// carrying traffic over relay, because a disco pong travels raw UDP or
// the relay's WSS control session and never the data plane itself
// (waired#729). It rides along on the candidate as an advisory sort
// key instead, and /healthz — which does traverse the data plane, in
// parallel, on a 50 ms budget, spinning no GPU — is what actually
// decides.
//
// class is the request's Claude traffic class ("main"/"sub", or "" for
// general non-Claude inference). A peer the admin marked ineligible for
// that class (InferenceState.ExcludeMain/ExcludeSub, CP-folded from the
// per-device serving toggles) is dropped, so the mesh stops routing that
// class there. Empty class is unfiltered.
func (s *Selector) buildMeshCandidates(
	snap inferencemesh.Snapshot,
	class string,
	minWindow int,
	wantOllama, wantVLLM map[string]catalog.Variant,
	gate *publicGate,
) []meshCandidate {
	var (
		rtts     map[string]uint32
		errors   map[string]float32
		inflight map[string]int32
	)
	if s.in.LocalRTT != nil {
		rtts = s.in.LocalRTT()
	}
	if s.in.LocalErrors != nil {
		errors = s.in.LocalErrors()
	}
	if s.in.LocalInFlight != nil {
		inflight = s.in.LocalInFlight.Snapshot()
	}

	const noRTT = ^uint32(0) // math.MaxUint32 sentinel for "no sample"

	var out []meshCandidate
	for _, p := range snap.Peers {
		if p.InferenceState == nil || !p.InferenceState.Reachable || p.Stale {
			continue
		}
		// Public Share partition (waired#827, spec §4.2). A grant-tagged
		// peer is a stranger's machine: it enters the candidate set only
		// under an explicit consumer policy, and it is displayed only by
		// its grant pseudonym. A grant whose Role is not "provider" (i.e.
		// a guest using OUR engine) is never a routing target.
		displayID, isPublic := p.DeviceID, false
		if p.Grant != nil {
			if !isPublicProvider(&p) {
				continue
			}
			pseudonym, ok := publicDisplayID(p.Grant)
			if !ok {
				continue
			}
			if !gate.admit {
				continue
			}
			tier := s.peerTier(p.InferenceState.Type, p.InferenceState.Models)
			if gate.auto {
				// Deferred until a public peer actually shows up, so the
				// common no-public-peers path never pays for the scan.
				s.ensureBeat(gate, snap)
			}
			if !gate.admits(tier) {
				continue
			}
			displayID, isPublic = pseudonym, true
		}
		// Per-class Claude serving eligibility: drop peers the admin marked
		// ineligible for this request's traffic class (CP-folded into
		// ExcludeMain/ExcludeSub). Empty class (general inference) is unfiltered.
		switch class {
		case state.ClaudeClassMain:
			if p.InferenceState.ExcludeMain {
				continue
			}
		case state.ClaudeClassSub:
			if p.InferenceState.ExcludeSub {
				continue
			}
		}
		kind := p.InferenceState.Type
		if kind == "" {
			kind = catalog.RuntimeOllama
		}
		var want map[string]catalog.Variant
		switch kind {
		case catalog.RuntimeOllama:
			want = wantOllama
		case catalog.RuntimeVLLM:
			want = wantVLLM
		default:
			continue
		}
		// waired#1031: a tier is a promise about the serving node. Drop a
		// peer whose DECLARED window falls short of what this request
		// demands. A peer declaring nothing (0) stays — that is every
		// agent predating the field, and reading silence as refusal would
		// empty the mesh the moment one node upgraded.
		if minWindow > 0 && p.InferenceState.ContextWindow > 0 &&
			p.InferenceState.ContextWindow < minWindow {
			continue
		}
		for _, m := range p.InferenceState.Models {
			v, ok := want[m]
			if !ok {
				continue
			}
			c := meshCandidate{
				deviceID:      p.DeviceID,
				displayID:     displayID,
				public:        isPublic,
				variant:       v,
				runtime:       kind,
				tag:           m,
				contextWindow: p.InferenceState.ContextWindow,
				priority:      p.InferenceState.Priority,
				silent:        p.Silent,
				capacity:      p.InferenceState.Capacity,
				score:         int64(v.ParamCount) * int64(v.QuantizationTier),
				rttMS:         noRTT,
			}
			// isPublic is only ever set inside the p.Grant != nil branch
			// above, so the grant is present here; carry its ID so Commit
			// can report the grant as used (waired#898).
			if isPublic {
				c.grantID = p.Grant.ID
			}
			if r, ok := rtts[p.DeviceID]; ok {
				c.rttMS = r
			}
			if e, ok := errors[p.DeviceID]; ok {
				c.errorRate = e
			}
			// Weighted-least-loaded balancing input. inflight is nil when
			// LocalInFlight is unwired, leaving loadFraction at 0 so the
			// sort degrades to the deterministic deviceID tie-break.
			if inflight != nil {
				c.inFlight = inflight[p.DeviceID]
				c.loadFraction = float64(c.inFlight) / float64(effectiveCapacity(c.capacity))
			}
			out = append(out, c)
			break
		}
	}
	return out
}

// acquireSlot returns (release, true) when the candidate is eligible
// under admission, or (noopRelease, false) when it's at capacity.
// Without LocalInFlight wiring, every candidate is admitted with a
// no-op release.
func (s *Selector) acquireSlot(c meshCandidate) (func(), bool) {
	if s.in.LocalInFlight == nil {
		return noopRelease, true
	}
	return s.in.LocalInFlight.Acquire(c.deviceID, c.capacity)
}

// acquireStickySlot counts this request against (sticky key, peer) so
// the NEXT request of the same conversation can see that this one is
// still running. Unlike acquireSlot it cannot refuse — it is a
// balancing signal, not admission. Returns noopRelease when the tracker
// is unwired or the request carries no affinity hint.
func (s *Selector) acquireStickySlot(stickyID, deviceID string) func() {
	if s.in.StickyInFlight == nil || stickyID == "" {
		return noopRelease
	}
	return s.in.StickyInFlight.Acquire(stickyID, deviceID)
}

// chainRelease folds two release closures into the single one that
// reaches Selection.Release, so the gateway keeps its one `defer
// sel.Release()` and neither counter can be given back without the
// other.
func chainRelease(first, second func()) func() {
	return func() {
		first()
		second()
	}
}

// rttBucketMS is the width of the coarse RTT band the sort quantises
// observed RTT into before the load-fraction axis runs. The intent
// (matching disco.Service's "RTT is a coarse signal where path
// attribution would not change a routing decision" note) is to fold
// same-LAN / same-AZ peers — whose RTTs differ by single-digit ms and
// whose differences are noise, not signal — into one band so the
// weighted-least-loaded axis can actually distribute traffic among
// them, while still separating genuinely distant peers (cross-region,
// tens-to-hundreds of ms) into worse bands. Tunable later if real
// deployments want finer/coarser granularity.
const rttBucketMS = 25

// rttBucket maps an observed RTT (ms) to its coarse band. The no-sample
// sentinel (math.MaxUint32, set when LocalRTT has never seen the peer)
// maps to the worst band so a never-probed peer sorts after peers with
// any known RTT — preserving the pre-balancing MaxUint32-last ordering.
func rttBucket(ms uint32) uint32 {
	if ms == ^uint32(0) {
		return ^uint32(0)
	}
	return ms / rttBucketMS
}

// effectiveCapacity is the balancing weight for a peer. Capacity==0
// ("unlimited" admission) is treated as weight 1 so load-fraction stays
// a well-defined monotone ratio; the admission gate's unlimited
// semantics are unaffected (acquireSlot / the pre-filter handle that
// independently).
func effectiveCapacity(capacity int) int {
	if capacity <= 0 {
		return 1
	}
	return capacity
}

// sortMeshCandidates orders candidates by:
// silence asc → priority desc → score desc → error asc → RTT-band asc →
// load-fraction asc → deviceID asc. The admin routing priority is the
// dominant key among peers we have no reason to doubt: among peers
// that can serve the request, a High device is always preferred over Middle,
// and Middle over Low (an overloaded high-priority peer drops out via the
// capacity admission filter first, so traffic falls back to the next tier).
// Within a priority tier the Phase 7 chain runs unchanged — the RTT-band +
// load-fraction pair (weighted-least-loaded) distributes traffic across peers
// that tie on score/error/RTT-band proportional to advertised Capacity, and
// the deviceID asc suffix preserves the deterministic-pick contract when every
// earlier axis ties (the case existing tests with no admission wiring rely on).
func sortMeshCandidates(cands []meshCandidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		// Grant-kind tier is the dominant key: own == team > public
		// (waired/docs/decisions/, Team Share routing order). A public
		// candidate is only ever a last resort — the auto-mode tier
		// comparison in publicGate governs whether it may be a candidate
		// at all, not where it ranks.
		if cands[i].public != cands[j].public {
			return !cands[i].public
		}
		// Disco silence, second only to the grant tier (waired#729).
		//
		// Above priority: silence predicts a failed probe, and there
		// are only probeFanoutK slots — spending one on a silent
		// High-priority peer instead of a healthy Middle one lowers the
		// expected first-round hit rate and buys nothing. A silent peer
		// is still probed and still serves the request when every peer
		// ranked above it fails its probe, which is what keeps #729's
		// rule — silence must not black-hole a peer — intact: it is a
		// last resort, not an exclusion.
		//
		// Below public: a silent OWN peer must outrank a healthy PUBLIC
		// one, or a 45-second gap in pongs would push the owner's
		// traffic onto a stranger's machine — and partitionOwnFirst
		// would only have to undo it.
		if cands[i].silent != cands[j].silent {
			return !cands[i].silent
		}
		if cands[i].priority != cands[j].priority {
			return cands[i].priority > cands[j].priority
		}
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		if cands[i].errorRate != cands[j].errorRate {
			return cands[i].errorRate < cands[j].errorRate
		}
		bi, bj := rttBucket(cands[i].rttMS), rttBucket(cands[j].rttMS)
		if bi != bj {
			return bi < bj
		}
		if cands[i].loadFraction != cands[j].loadFraction {
			return cands[i].loadFraction < cands[j].loadFraction
		}
		return cands[i].deviceID < cands[j].deviceID
	})
}

// fallbackTrace renders the runner-up peers as a Decision.Fallback
// trail. Operators inspecting `waired diagnose` get a quick "why not
// this other peer?" answer; the production routing path uses the
// chosen peer only.
func fallbackTrace(cands []meshCandidate, chosen string) []FallbackCandidate {
	if len(cands) <= 1 {
		return nil
	}
	out := make([]FallbackCandidate, 0, len(cands)-1)
	for _, c := range cands {
		if c.deviceID == chosen {
			continue
		}
		// displayID throughout: the trace is rendered by
		// `waired diagnose` and returned by the management API, so a
		// public peer appears only under its grant pseudonym (§8.5).
		out = append(out, FallbackCandidate{
			EndpointID: computeEndpointID("remote-"+c.displayID, c.runtime, "_"),
			Runtime:    "remote:" + c.displayID,
		})
	}
	return out
}

// variantWantSets builds two maps — one per engine kind — keyed by
// the engine-native model identifier the peer would advertise. The
// ollama map uses Source.Tag (e.g. "qwen3:8b-q4_K_M"); the vllm map
// uses Source.RepoID (e.g. "Qwen/Qwen3-8B-Instruct"). Variants
// missing the relevant Source field are skipped.
func variantWantSets(manifest catalog.Manifest) (ollama, vllm map[string]catalog.Variant) {
	ollama = map[string]catalog.Variant{}
	vllm = map[string]catalog.Variant{}
	for _, v := range manifest.Variants {
		if supports(v.RuntimeSupport, catalog.RuntimeOllama) && v.Source.Tag != "" {
			ollama[v.Source.Tag] = v
		}
		if supports(v.RuntimeSupport, catalog.RuntimeVLLM) && v.Source.RepoID != "" {
			vllm[v.Source.RepoID] = v
		}
	}
	return ollama, vllm
}

// engineModelFor returns the engine-specific identifier the gateway
// puts back into the proxied request body. For Ollama, the variant's
// source tag is canonical (and we cross-check with the local-state
// record so a stale manifest can't paper over a re-pulled tag).
func engineModelFor(engine string, v catalog.Variant, st catalog.ModelState) string {
	switch engine {
	case catalog.RuntimeOllama:
		if st.OllamaTag != "" {
			return st.OllamaTag
		}
		return v.Source.Tag
	case catalog.RuntimeVLLM:
		// The wire model id the gateway sends to vLLM must match the
		// name vLLM registered the model under. The agent spawns the
		// engine with --served-model-name = the HF repo id and loads the
		// weights from the on-disk LocalPath (--model), so the repo id is
		// the canonical served name (see cmd/waired-agent VLLMConfig and
		// internal/runtime/vllm.go verifyServedModelName). Manifest
		// validation guarantees RepoID is non-empty for vLLM variants.
		return v.Source.RepoID
	}
	return ""
}

// hasCapability is a small case-insensitive contains check.
func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if strings.EqualFold(c, want) {
			return true
		}
	}
	return false
}

func findVariant(m catalog.Manifest, id string) (catalog.Variant, bool) {
	for _, v := range m.Variants {
		if v.VariantID == id {
			return v, true
		}
	}
	return catalog.Variant{}, false
}

// pickEngine implements the §7.2 step 5 engine-branch rule. In Phase
// A this collapses to "use Ollama whenever the variant supports it";
// the GPU+CUDA → vLLM path is the Phase B trigger.
func pickEngine(v catalog.Variant, hw hardware.Profile) string {
	if hw.Accelerators.CUDA && supports(v.RuntimeSupport, catalog.RuntimeVLLM) {
		return catalog.RuntimeVLLM
	}
	if supports(v.RuntimeSupport, catalog.RuntimeOllama) {
		return catalog.RuntimeOllama
	}
	return ""
}

func supports(list []string, want string) bool {
	for _, x := range list {
		if x == want {
			return true
		}
	}
	return false
}

// computeEndpointID returns a deterministic "ep_<scope>_<engine>_<modelid>"
// string. Spec waired_product_spec.md §13.6 only requires the value
// to be opaque; deriving it from inputs keeps Phase A from needing a
// cross-restart endpoint registry.
func computeEndpointID(scope, engine, modelID string) string {
	return "ep_" + sanitize(scope) + "_" + sanitize(engine) + "_" + sanitize(modelID)
}

// sanitize lowercases s and replaces any non-[a-z0-9_] rune with '_'.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func modelStateOf(m catalog.ModelState, present bool) string {
	if !present {
		return catalog.ModelStateNotPresent
	}
	return m.State
}
