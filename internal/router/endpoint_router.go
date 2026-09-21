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
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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
// (gateway.ComputeStickyID: the X-Waired-Conversation-Id header when
// the client sets one, else the client's own user id and first
// message, else the request body prefix) — Phase 7's mesh fallback
// uses it for KV cache affinity. Empty means "no affinity hint"; the
// Selector falls straight through to the score-based pick.
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
	// the selection fails with ErrNoEndpointForWindow, and the turn ends
	// with that reason — nothing carries it to the real Anthropic API
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
	//
	// An endpoint that declares NOTHING (0) does not pass. It used to, for
	// agents predating the field, and that let every computer serving under
	// the smallest declarable window — a gpt-oss host, a 32k model — answer
	// the 200k row, because such a computer declares 0 rather than a
	// smaller number. There are no agents predating the field to carry
	// (owner decision 2026-09-16, waired-agent#1395).
	MinContextWindow int `json:"min_context_window,omitempty"`

	// NodeDirective is the /model directive id the client picked, when
	// that id names a NODE rather than a route — today the "Waired peer"
	// id (waired-agent#830). "" for every other request.
	//
	// The core Selector ignores it, exactly as it ignores Class. The
	// agent's Claude-intercept Selector wrapper turns it into a routing
	// preference for THIS REQUEST, and writes nothing: the operator's
	// persisted `waired worker` choice is untouched by picking an entry
	// in /model.
	//
	// It is carried here rather than read back off Model for the reason
	// MinContextWindow is: an Anthropic id is not in the catalog, so the
	// first selection returns ErrModelNotFound and ResolveUnknownModel
	// overwrites Model with the default alias before the retry. Anything
	// derived from Model at the selector is therefore correct on the
	// first attempt and gone on the second — which is every real request.
	NodeDirective string `json:"node_directive,omitempty"`
}

// localModels is the serving engine's model records.
func (in Inputs) localModels() map[string]catalog.ModelState {
	engine := in.ServingEngine
	if engine == "" {
		engine = catalog.RuntimeOllama
	}
	return in.LocalState.ModelsFor(engine)
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

	// ServingEngine is the engine this device serves with; "" reads as
	// ollama. Its records in LocalState are what this device can answer
	// with (waired-agent#1520): weights the other engine fetched are not a
	// local route.
	ServingEngine string

	// DefaultModelID, when non-empty, is the model the dynamic coding
	// alias (DynamicCodingAliases: waired/default)
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
	// local candidate is dropped when the window it reports falls short of
	// the floor, and 0 — or nil — falls short of every floor, exactly as an
	// undeclared PEER does (waired-agent#1395). The overlay-side Selector
	// leaves it nil, and a request that arrived from a peer never carries a
	// floor.
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

	// Assignments serialises SelectKAssigned and counts the requests this
	// device has assigned to its own engine (waired-agent#1354). nil keeps
	// the behaviour from before it: concurrent requests rank on the same
	// numbers, and this device's engine is ranked on its served count alone.
	Assignments *Assignments

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

	// PublicOnly narrows this selection to public machines: own-network
	// peers are dropped from the candidate set (waired-agent#901).
	//
	// Per-Selector rather than per-Request because the Claude surface
	// already builds a Selector per request, which is where the
	// operator's /model choice is known — the same seat RoutingMode
	// occupies. Every other surface leaves it false.
	//
	// It narrows and never widens. PublicPolicyFn still decides what is
	// admissible at all, so with the posture off this selects nothing
	// rather than reaching machines the operator never consented to.
	PublicOnly bool

	// OnPublicGrantDemand is called when policy would have used a public
	// candidate but this device holds no Public Share grant to one that
	// can take the request, so the background acquirer can wake early
	// instead of waiting out its periodic tick (spec §4.3 cold start).
	// minContextWindow is the request's window floor
	// (Request.MinContextWindow): 200704 or 1048576 for a Waired row, 0
	// for a request that is not one (waired-agent#1399). Must not block:
	// the production implementation is a non-blocking send onto a
	// coalescing buffered channel.
	OnPublicGrantDemand func(minContextWindow int)

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

	// PinnedPeerDisplayID is what the pinned peer may be called on a
	// surface an operator reads, recorded when the pin was set
	// (state.RoutingPreference.PinnedPeerDisplayID): the pseudonym of a
	// public machine, the "<device> (<owner>)" label of a teammate's, the
	// DeviceID of one of your own. It is what names the pin once the peer
	// has dropped out of the snapshot — a teammate who stopped sharing, a
	// guest pass that lapsed — where the snapshot can no longer say whose
	// machine it is and the raw pin would be another account's device id.
	PinnedPeerDisplayID string

	// PinnedStrict is set when the pin came from a model row that names one
	// computer (waired/peer-<name>) rather than from `waired worker`. Such a
	// row says "that computer and nothing else", so a pin running nothing
	// the catalog knows is refused, where a `waired worker` pin keeps its
	// fallthrough to the rest of the mesh (docs/decisions/20260819/
	// 1900-routing-selects-a-node-not-a-model.md). Every OTHER reason a pin
	// cannot take a turn is refused for both (waired-agent#1395).
	PinnedStrict bool

	// Prefer is what the operator asked the ordering to optimise for
	// (waired-agent#1128): state.RoutingPreferSpeed answers as fast as
	// possible, state.RoutingPreferSize uses the biggest model available.
	// Empty == RoutingPreferSpeed == the default.
	Prefer state.RoutingPrefer

	// MinModelSize is the operator's routing floor — the smallest model
	// class this device will route to (hostfit.ModelSizeSmall / Medium /
	// Large). Empty = no floor, the default.
	//
	// It EXCLUDES rather than demotes (owner ruling, 2026-08-29): a
	// request with no computer above the floor falls back and says why,
	// rather than quietly using a smaller model. It applies to this
	// device's own engine as well as to peers — "ローカルと peer は
	// 区別しない".
	MinModelSize string

	// PeerSpeeds is what this requester has learned about how fast each
	// peer prefills (PrefillWindow.Snapshot). nil disables the speed term
	// entirely, which is the pre-#1127 ordering.
	PeerSpeeds func() map[string]PeerSpeed

	// LocalServingOff reports that this host will not execute a request
	// on its own engine right now — the operator turned local inference
	// off from the tray / management API, or the host never had an
	// engine to turn on (waired-agent#829).
	//
	// It is one fact about the local candidate, not a reason to refuse
	// the request. The gate that carried this used to sit at the
	// gateway's outermost layer and 503'd every request before any
	// routing ran, so a node with no engine could not reach the mesh at
	// all — while the shipped copy for that exact install told the user
	// it still could (host_cutoff.go, first-run docs). Here it only
	// removes the local candidate; the mesh branches are untouched, and
	// a request with nowhere else to go gets ErrLocalInferenceOff.
	LocalServingOff bool

	// LocalActiveOnly limits the local candidate to the model this host is
	// serving (LocalState.Active). The overlay listener sets it for every
	// request from another computer: a model that is only on disk is not
	// offered, because serving it would swap the engine off what the owner
	// runs, and could hand a Public Share guest a model imported for the
	// owner's account only (waired-ai/waired#1473 ruling 4,
	// waired-ai/waired#1477).
	// LocalCustomModelWindow, when non-nil, returns the window this host
	// serves a custom model under 200,704 tokens with, or 0
	// (agentInferenceProvider.CustomModelWindow, waired-ai/waired#1481). The
	// local-only arm admits such a model under the coding floor, as the
	// mesh does (customWindowAdmits).
	LocalCustomModelWindow func() int

	LocalActiveOnly bool

	// LocalNode, when non-nil, reports what THIS device is serving right
	// now, so its own engine is ranked in the same ordered list as the mesh
	// instead of short-circuiting around it (waired-agent#1302; owner
	// ruling 2026-08-29 on waired-agent#1128/#1129).
	//
	// nil keeps the pre-#1302 two-way branch, which is what the
	// overlay-side Selector gets (localOnlySelector builds its Inputs from
	// baseRouterInputs, and this field is set one layer up) and what every
	// hand-built test Inputs gets.
	LocalNode func() LocalNode

	// TieBreak, when non-nil, reorders candidates INSIDE a rank tier —
	// a set this Selector has already decided it cannot tell apart. n is
	// the size of the run; the result must be in [0, n).
	//
	// It exists because the tie-break below every real key is the deviceID
	// spelling, which is deterministic and GLOBAL: three computers ranking
	// the same tied peers at the same instant all pick the same one, and
	// two of them queue behind the first. Measured on the rc6 fleet
	// (waired-agent#1303, S4): three hosts sent `Waired peer` at once, two
	// landed on the same peer, and the third machine sat idle.
	//
	// nil keeps the deterministic order, which is what the ordering tests
	// rely on.
	TieBreak func(n int) int
}

// DynamicCodingAliases are the product-fixed model names that resolve
// to the host's *current* coding default (Inputs.DefaultModelID)
// instead of a static ModelAliases entry. They used to be pinned in
// qwen2.5-coder-7b-instruct.json, which broke the out-of-the-box
// `waired infer` on every host whose selected bundled model differs
// (#632).
//
// One name, because one name is what the indirection is for: a client
// config, a script or a coding-agent setting written once keeps working
// after the model behind it changes. waired/coding and waired/small
// were retired in #521 — with the rest of the waired/* namespace, and
// for the same reason. They came from an early aim of abstracting the
// model name away entirely, for an audience that has since turned out
// to know model names perfectly well; waired/coding resolved
// identically to waired/default, so the pair only ever offered the same
// model under two names.
//
// A model that is not the default is named directly. Every model_id and
// vendor-form alias still resolves, so nothing is unreachable.
var DynamicCodingAliases = []string{DefaultModelAlias}

// DefaultModelAlias is that one name. Exported because it is also what a
// surface says when it means "the caller did not choose a model": the
// Claude intercept maps the Anthropic ids Claude Code sends to it rather
// than to a concrete model, so routing picks the node and the node's own
// model answers (waired-agent#828).
const DefaultModelAlias = "waired/default"

// openModelReason is the trace's first line: what the requested name
// resolved to.
//
// A request that named a model gets the plain resolution. A request that
// named none gets told that the resolution is a local CANDIDATE, not the
// answer: since waired-agent#828 such a request picks a node and takes
// that node's own model, so opening with "resolved dynamically to this
// host's coding default X" named a model that had not answered and was
// then contradicted two lines later by the catalog-wide want set
// (waired-agent#854).
//
// source names where the model came from, so the line stays true on
// whichever resolution arm hit.
func openModelReason(alias, modelID, source string) string {
	if modelIsUnspecified(alias) {
		return fmt.Sprintf(
			"alias %q names no model — the node that answers chooses it; %s %q applies only if that node is this host",
			alias, source, modelID)
	}
	return fmt.Sprintf("alias %q resolved to model_id %q", alias, modelID)
}

// resolveModel maps a requested model name to a manifest: dynamic
// coding aliases go to DefaultModelID when it resolves, then the static
// LookupByAlias path, then the engine-native fallback for names that
// only the mesh puts on the wire, and finally the retirement table.
// Appends the resolution reason on success.
func (s *Selector) resolveModel(name string, reasons *[]string) (catalog.Manifest, bool) {
	if s.in.DefaultModelID != "" && slices.Contains(DynamicCodingAliases, name) {
		if m, ok := catalog.LookupByAlias(s.in.DefaultModelID, s.in.Manifests); ok {
			*reasons = append(*reasons, openModelReason(name, m.ModelID, "this host's own coding default"))
			return m, true
		}
	}
	if m, ok := catalog.LookupByAlias(name, s.in.Manifests); ok {
		*reasons = append(*reasons, openModelReason(name, m.ModelID, "the catalog's entry for it"))
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

// RTTUnknown is Candidate.RTTMS when disco has never matched a pong from
// the peer — which is also what a peer only ever reached over the relay
// looks like, since disco.Service.RTTSnapshot omits peers with no
// samples. It sorts last among peers that have a measurement, and is
// never a filter.
//
// A consumer deciding how long to wait for such a peer has no distance
// estimate to scale by, and must fall back to its own ceiling rather than
// treat the sentinel as a duration.
const RTTUnknown = ^uint32(0)

// rttDisplay renders RTTMS for a human-readable surface: the reasons
// string `waired infer --explain` prints and the management API's
// /inference/select serialises (waired-agent#714).
//
// The sentinel is a value, not a measurement, so it is not shown as one.
// Printing it raw put 4294967295 in front of an operator as if the peer
// were 49 days away; printing 0 would be worse, because 0 reads as the
// closest possible peer.
//
// Rendering is the ONLY place this substitution belongs. Candidate.RTTMS
// keeps the sentinel, because it is an input to
// gateway.probeBudgetFor — which gives an unmeasured peer the probe
// ceiling precisely because there is no distance to scale by. Zeroing the
// field would collapse that budget to the floor for every relay-path peer
// and re-run waired-agent#624.
func rttDisplay(rttMS uint32) string {
	if rttMS == RTTUnknown {
		return "unmeasured"
	}
	return strconv.FormatUint(uint64(rttMS), 10)
}

// Sentinel errors. Wrap with %w so callers can use errors.Is.
var (
	ErrModelNotFound        = errors.New("router: model not found in catalog")
	ErrCapabilityNotMet     = errors.New("router: no variant satisfies the requested capabilities")
	ErrModelNotReady        = errors.New("router: model is not in ready state on disk")
	ErrHardwareInsufficient = errors.New("router: hardware does not meet variant requirements")
	ErrRuntimeNotInstalled  = errors.New("router: required runtime is not registered")
	// ErrLocalInferenceOff is returned when Inputs.LocalServingOff took
	// the local candidate out and no mesh peer could take the request
	// either (waired-agent#829). Distinct from ErrModelNotReady because
	// the remedy is different: the weights are not the problem, the
	// operator's own toggle is. The gateway renders it as the 503
	// `waired_inference_disabled` body the outermost gate used to write,
	// so a client that only ever saw that error still sees it — but now
	// only when the mesh really had nothing.
	ErrLocalInferenceOff = errors.New("router: local inference is turned off on this host")
	// ErrModelNotActive is returned under Inputs.LocalActiveOnly when the
	// request names a model this host is not serving. It is returned as a
	// *ModelNotActiveError, which also matches ErrModelNotReady so the
	// listeners answer it the way they answer a model nobody here serves.
	ErrModelNotActive = errors.New("router: model is not the one this computer is serving")
	// ErrNoEndpointForWindow is returned when Request.MinContextWindow is
	// set and no endpoint — local or mesh — declares a window that reaches
	// it (waired#1031). Distinct from ErrModelNotReady ("nobody has the
	// model") because the operator's remedy is different: the model is
	// there, the window is not, and the fix is a model or a machine that
	// can hold one. Both listeners answer it with a 400 naming that remedy
	// (waired-agent#1395); it used to become a fallback to the real
	// Anthropic API, and no longer does
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
	//
	// The Selector returns it as a *WindowFloorError.
	ErrNoEndpointForWindow = errors.New("router: no endpoint declares the required context window")
	// ErrPinnedPeerDeclined is returned when a pin — a model row naming one
	// computer, or `waired worker` pinned — is reachable but a filter
	// removed it: the window a row demands, the computer's own "Serve main
	// conversation / Serve subagents" switches, or the Public Share gate.
	// Before waired-agent#1395 the request fell through to the rest of the
	// mesh, which is the substitution a pin exists to rule out. The
	// Selector returns it as a *PinnedPeerDeclinedError.
	ErrPinnedPeerDeclined = errors.New("router: the pinned computer cannot take this turn")
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
	// ErrPeersDidNotAnswer is returned when mesh peers matched the
	// request but not one of them answered its readiness probe — every
	// probe hit a transport error or ran out of budget, so nothing was
	// learned about any of them.
	//
	// Distinct from ErrAllPeersOverloaded because the two are different
	// operator problems and only one of them is about load: "at capacity"
	// is a peer that answered and said it was full, and reporting it for
	// a mesh nobody could reach sent the #624 investigation to the
	// capacity filter, which had not run. A peer that answered "not
	// ready" for any other reason still falls under the existing
	// sentinels — this one means the mesh went unmeasured.
	ErrPeersDidNotAnswer = errors.New("router: no matching mesh peer answered its readiness probe")
)

// PinnedPeerUnreachableError is what the Selector actually returns for
// ErrPinnedPeerUnreachable. It carries the pinned peer's identity so the
// gateway can name the peer in telemetry, response headers and the
// user-facing error without re-deriving it from the routing
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

	// PeerName is the name a person would recognise the pinned computer by,
	// when this device knows one: the device name for an own-network peer,
	// and the grant pseudonym for a Public Share peer, which is the only
	// identifier that may be shown for one (spec §8.5). Empty when the pin is
	// absent from the snapshot, where the configured id is all there is.
	//
	// It exists because this error is now what the person sees. The turn ends
	// here rather than being carried to the real Anthropic API
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md),
	// and "pinned peer is unreachable: dev_4259…" named the pin with the one
	// string the reader cannot act on (waired-agent#1180).
	PeerName string
}

func (e *PinnedPeerUnreachableError) Error() string {
	who := e.PeerName
	if who == "" {
		who = e.PeerDisplayID
	}
	return fmt.Sprintf("%s: %q", ErrPinnedPeerUnreachable.Error(), who)
}

func (e *PinnedPeerUnreachableError) Unwrap() error { return ErrPinnedPeerUnreachable }

// PinnedPeerBusyError is ErrAllPeersOverloaded told truthfully on a pin.
//
// The generic sentence — "every matching mesh peer is at capacity" — names
// the whole mesh, and on a pinned request exactly one computer was ever
// considered. Measured on the 0.0.3-rc6 fleet (waired-agent#1303, S3): a
// pinned turn brief-queued for 61.6 s behind the pin's own local turn and
// came back 503 with that sentence, so the person was told the mesh was
// full while other computers sat idle.
//
// It Unwraps to ErrAllPeersOverloaded on purpose: the status mapping, the
// Retry-After sizing and every errors.Is in the gateway keep working
// unchanged, and the retry is what eventually carries the turn (S2: the
// slot freed after 32 s and the next attempt was a 200). Only the wording,
// the headers and the telemetry reason are new.
//
// PeerDisplayID follows Selection.PeerDisplayID: the grant pseudonym for a
// Public Share peer, never its real device id (spec §8.5).
type PinnedPeerBusyError struct {
	PeerDisplayID string
	PeerName      string
	ModelID       string
	// CapacityUsed / CapacityTotal are the peer's own figures, as this
	// device last read them off its /healthz. Both zero when the wait ended
	// before any probe came back with them.
	CapacityUsed  int
	CapacityTotal int
}

func (e *PinnedPeerBusyError) Error() string {
	who := e.PeerName
	if who == "" {
		who = e.PeerDisplayID
	}
	if who == "" {
		return ErrAllPeersOverloaded.Error()
	}
	slots := ""
	if e.CapacityTotal > 0 {
		slots = fmt.Sprintf(" — %d of %d conversations in use",
			e.CapacityUsed, e.CapacityTotal)
	}
	return fmt.Sprintf("%s is busy with other work%s. This turn is pinned to that computer, so no other computer can take it",
		who, slots)
}

func (e *PinnedPeerBusyError) Unwrap() error { return ErrAllPeersOverloaded }

// PinnedPeerBusy returns the typed busy error behind err, if that is what
// it is. The gateway uses it to name the peer in a header and to say how
// full it was, both of which the sentinel alone cannot carry.
func PinnedPeerBusy(err error) (*PinnedPeerBusyError, bool) {
	var e *PinnedPeerBusyError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// ModelNotReadyError is what the Selector returns for ErrModelNotReady.
// It carries the local model state behind the verdict so a caller can
// tell a model that is on its way from one that nothing is fetching —
// two conditions the sentinel alone cannot separate, with two different
// right answers for a client (waired-agent#788).
//
// Same shape as PinnedPeerUnreachableError above: Unwrap keeps every
// existing errors.Is(err, ErrModelNotReady) comparison working, and
// Error() reproduces the message the sentinel path always produced, so
// nothing that reads the wire text changes.
//
// Note is the parenthesised routing context ("routing=peer-only, no mesh
// candidate"), empty on the plain path.
type ModelNotReadyError struct {
	ModelID string
	State   string
	Note    string
	// Mesh marks a miss on a MESH branch: no reachable peer could take
	// the request. The sentinel's sentence was written for the local
	// branch, so a mesh miss read `model is not in ready state on disk:
	// "qwen3.5-35b-a3b" state="ready"` — a self-contradiction in one
	// line, about a machine the request was never going to run on
	// (waired-agent#828). The local state stays in the message because
	// it is still the next fact an operator reads; it stops being the
	// headline.
	Mesh bool
	// PublicShare marks a miss on the "Waired public share" entry, whose
	// refusal leads with its reason instead of with the mesh
	// (waired-agent#1201). The person reading it picked one row in
	// /model and wants to know which of their own settings declined;
	// "no mesh peer is available … local state=…" answered with two
	// Waired-internal facts about a machine the turn was never going to
	// run on. Note carries the reason, and is empty when the attempt
	// learned nothing true to say.
	PublicShare bool
	// LocalArrivalAnswers states that this host's own weights finishing
	// their download WOULD make this turn servable — the only condition
	// under which ModelIsArriving may read State as evidence.
	//
	// False on the branches that deliberately never consult this host's
	// engine (peer-only, "Waired public share", pinned). There State is
	// carried for the operator reading a journal (waired-agent#828) and
	// is not a fact about this turn: a host that happened to be
	// downloading a model answered every such refusal with 503 +
	// Retry-After, so the message naming the real reason never reached
	// the client (waired-agent#1252, the defect class of
	// waired-agent#788).
	//
	// Zero value false, so a branch that does not state the fact never
	// buys a retry loop on no evidence.
	LocalArrivalAnswers bool
}

func (e *ModelNotReadyError) Error() string {
	if e.PublicShare {
		if e.Note == "" {
			return "router: Waired public share declined this turn"
		}
		return "router: Waired public share declined this turn: " + e.Note
	}
	if e.Mesh {
		if e.ModelID == "" {
			// The request named no model, so nothing resolved. Name the
			// shortage rather than invent a model id.
			return fmt.Sprintf("router: no mesh peer is available (%s); local state=%q", e.Note, e.State)
		}
		if e.Note == "" {
			return fmt.Sprintf("router: no mesh peer serves %q; local state=%q", e.ModelID, e.State)
		}
		return fmt.Sprintf("router: no mesh peer serves %q (%s); local state=%q", e.ModelID, e.Note, e.State)
	}
	if e.Note == "" {
		return fmt.Sprintf("%s: %q state=%q", ErrModelNotReady.Error(), e.ModelID, e.State)
	}
	return fmt.Sprintf("%s: %q state=%q (%s)", ErrModelNotReady.Error(), e.ModelID, e.State, e.Note)
}

func (e *ModelNotReadyError) Unwrap() error { return ErrModelNotReady }

// arrivingModelStates are the local model states from which readiness
// arrives on its own: something is already fetching or checking the
// weights, so a client that waits and retries will eventually be served.
// Every other state — absent, failed, evicted — means nothing is on the
// way, and a retry loop against it never terminates.
var arrivingModelStates = map[string]bool{
	catalog.ModelStateQueued:      true,
	catalog.ModelStateDownloading: true,
	catalog.ModelStateVerifying:   true,
}

// ModelNotActiveError is ErrModelNotActive with the two ids. The message
// names only the model asked for: it is the body of the answer to another
// computer, and what this one runs is not that computer's to learn from a
// refusal.
type ModelNotActiveError struct {
	ModelID string
	Active  string // "" when nothing is active; for logs, not the message
}

func (e *ModelNotActiveError) Error() string {
	return fmt.Sprintf("%v: %q", ErrModelNotActive, e.ModelID)
}

func (e *ModelNotActiveError) Unwrap() []error { return []error{ErrModelNotActive, ErrModelNotReady} }

// ModelIsArriving reports whether a not-ready error describes a model
// that is on its way here, as opposed to one no host is serving and none
// is fetching.
//
// It answers false for anything that is not a ModelNotReadyError,
// including the bare sentinel: without a state there is no evidence the
// wait would end, and telling a client to keep retrying on no evidence
// is the defect this exists for (waired-agent#788 — `claude -p` printed
// nothing for 327 s, retrying a 503 for a model no host had).
func ModelIsArriving(err error) bool {
	var e *ModelNotReadyError
	if !errors.As(err, &e) {
		return false
	}
	// State is this host's local state, which is only evidence about this
	// turn on a branch that would have run here (waired-agent#1252).
	if !e.LocalArrivalAnswers {
		return false
	}
	return arrivingModelStates[e.State]
}

// modelNotReady builds the ModelNotReadyError for a LOCAL selection
// branch. Its sentence is a local claim ("model is not in ready state on
// disk"), so every honest use of it is one — which is why the local
// arrival is evidence here (waired-agent#1252).
func modelNotReady(modelID, state, note string) error {
	return &ModelNotReadyError{ModelID: modelID, State: state, Note: note, LocalArrivalAnswers: true}
}

// SizeFloorError wraps whatever a selection branch returned when the
// operator's minimum model class is what removed the last candidate
// (waired-agent#1128).
//
// A WRAPPER rather than a field on ModelNotReadyError, because the miss
// does not always arrive as one. On an engine-less requester — the exact
// host this feature exists for — a mesh miss is reported as
// ErrLocalInferenceOff, since on that host the toggle is normally what
// removed the local fallback. Measured on real hardware: with a `large`
// floor and no large model reachable, the surface said "local inference
// disabled", which sends the operator to the wrong switch. Wrapping
// keeps every errors.Is on the underlying sentinel working while adding
// the reason on top of it.
type SizeFloorError struct {
	Err error
	// Floor is the class the operator set, so a surface can name the
	// threshold instead of describing it.
	Floor string
	// LocalArmOnlyFloor marks the one base error the suffix cannot be
	// appended to: the local arm's "not in ready state on disk", raised
	// while its own State field says "ready", because the floor is what
	// removed the model and nothing about the disk did. The result read
	// `model is not in ready state on disk: "qwen3.6-35b-a3b"
	// state="ready" (routing floor: ...)` — waired-agent#1178.
	//
	// Recorded at the wrap site, where "the floor did this" is known,
	// rather than inferred here from the two halves disagreeing. Same
	// answer as ModelNotReadyError.Mesh gave the sibling defect
	// (waired-agent#828): a discriminant chooses the headline, and the
	// fact that contradicts it stops being the subject of the sentence.
	LocalArmOnlyFloor bool
}

func (e *SizeFloorError) Error() string {
	if e.Floor == "" {
		return e.Err.Error()
	}
	shortfall := "no computer runs " + ModelSizePhrase(e.Floor)
	if e.LocalArmOnlyFloor {
		return "router: " + shortfall + " (routing floor)"
	}
	return e.Err.Error() + " (routing floor: " + shortfall + ")"
}

// ModelSizePhrase words a routing floor the way the operator set it.
//
// "large" has no class above it, so "a large model or larger" would be a
// sentence about a ladder that ends there. Owner ruling 2026-08-29
// (waired-agent#1128), pinned as an example in docs-site/TRANSLATION.md.
//
// Exported because the phrasing was implemented twice and the router had
// neither copy: the Claude reroute notice carried the rule while this
// package wrote the form the rule forbids (waired-agent#1178).
func ModelSizePhrase(size string) string {
	if size == hostfit.ModelSizeLarge {
		return "a large model"
	}
	return "a " + size + " model or larger"
}

// localArmMiss reports whether err is the LOCAL arm's not-ready error —
// the one whose sentence the floor contradicts. A mesh miss says
// something true about the mesh and keeps the suffix.
func localArmMiss(err error) bool {
	var e *ModelNotReadyError
	return errors.As(err, &e) && !e.Mesh
}

func (e *SizeFloorError) Unwrap() error { return e.Err }

// BelowModelSizeFloor reports whether nothing could serve this request
// because the operator's minimum model class excluded everything that
// otherwise would have.
//
// It is its own question, not a shade of "no host serves this model" or
// of "local inference is off": the operator set that floor, the fallback
// is the consequence they were told about, and the surfaces name the
// reason rather than reporting an outage. Owner ruling, 2026-08-29.
func BelowModelSizeFloor(err error) bool {
	var e *SizeFloorError
	return errors.As(err, &e)
}

// ModelSizeFloor is the class the operator set, on a miss it caused.
// Empty on any other error — a surface that finds nothing here has no
// threshold to name.
func ModelSizeFloor(err error) string {
	var e *SizeFloorError
	if !errors.As(err, &e) {
		return ""
	}
	return e.Floor
}

// localMiss names why a branch that would have run locally has nothing
// to run. When the operator's toggle is what removed the candidate, the
// weights are beside the point and ErrLocalInferenceOff says so;
// otherwise it is the ordinary "this model is not ready here".
func (s *Selector) localMiss(modelID, state, note string) error {
	if s.in.LocalServingOff {
		return ErrLocalInferenceOff
	}
	return modelNotReady(modelID, state, note)
}

// meshMiss is the same verdict reached on a MESH branch: nothing on the
// network could take the request. Same sentinel — every gateway mapping
// keys on it — with the sentence written for the branch that produced it
// (waired-agent#828).
//
// LocalArrivalAnswers stays false. The branches that reach here (peer-only,
// pinned) refused to use this host's engine by construction, so its weights
// finishing changes nothing about why the turn was refused
// (waired-agent#1252).
func meshMiss(modelID, state, note string) error {
	return &ModelNotReadyError{ModelID: modelID, State: state, Note: note, Mesh: true}
}

// publicShareDeclined is the terminal error for the "Waired public share"
// entry. reason is empty when the attempt learned nothing true to say, and
// the sentence then stops after the headline (waired-agent#1201).
func publicShareDeclined(reason string) error {
	return &ModelNotReadyError{Note: reason, Mesh: true, PublicShare: true}
}

// meshMissAfterLocal is meshMiss for a branch that would have accepted a
// local candidate too (peer-preferred). The toggle wins the naming there
// for the reason it does in localMiss: it is what removed the fallback.
//
// Built here rather than delegated to meshMiss, because the one field that
// differs is exactly what separates the two: this branch WOULD have run on
// local weights, so their arrival is evidence that waiting ends
// (waired-agent#1252).
func (s *Selector) meshMissAfterLocal(modelID, state, note string) error {
	if s.in.LocalServingOff {
		return ErrLocalInferenceOff
	}
	return &ModelNotReadyError{
		ModelID: modelID, State: state, Note: note,
		Mesh: true, LocalArrivalAnswers: true,
	}
}

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
	PeerDisplayID string `json:"peer_display_id,omitempty"`

	// RTTMS is the disco round-trip estimate for this peer, or
	// RTTUnknown when disco has never matched a pong from it. Carried
	// out of the Selector because how long it is worth waiting for a
	// peer's readiness probe is a function of how far away it is, and
	// only the Selector has the measurement (waired-agent#624).
	RTTMS uint32 `json:"rtt_ms,omitempty"`

	// RankTier groups the candidates of ONE SelectK call that this Selector
	// considers interchangeable: same tier means every ranking key tied and
	// only the arbitrary deviceID suffix separated them.
	//
	// The probe layer reads it to break such a tie on residency
	// (waired-agent#880) — which peer already holds its weights is answered
	// by /healthz, at probe time, while every ranking key is a snapshot fact
	// known only to the Selector. Without the tier the probe layer cannot
	// tell "outranks" from "indistinguishable", and preferring a warm peer
	// would overturn quality, priority, distance and load rather than break
	// a tie.
	//
	// A run index over one sorted slice, so it is comparable with == and
	// with nothing else: it is not a score, not stable across calls, and
	// carries no meaning between them. Zero on a hand-built Candidate, which
	// is the same tier for every candidate and therefore the permissive
	// answer — a test fake gets the tie-break, not an accidental ranking.
	RankTier int `json:"rank_tier,omitempty"`

	Decision Decision `json:"decision"`

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

	// slot takes this candidate's count — LocalInFlight for a peer,
	// Inputs.Assignments for this device — and finish turns a taken count
	// into the Selection: the sticky count, the sticky Touch, a public
	// grant's use. commit is slot then finish. They are split so
	// SelectKAssigned can take the count while ranking, and Commit finish
	// the rest once a probe has said the candidate is ready: a candidate
	// assigned and then abandoned must not have rebound a conversation or
	// used a grant (waired-agent#1354).
	slot   func() (release func(), ok bool)
	finish func(release func()) Selection
	// held is the count SelectKAssigned took for this candidate, or nil.
	// A pointer because Candidate is passed by value.
	held *heldSlot
}

// Commit transitions this candidate from "probed-ready" to "owned by
// this request". It performs the InFlightTracker Acquire (for remote
// candidates) and touches the sticky store. Returns (Selection, true)
// on success or (zero, false) when Capacity was hit between SelectK
// and Commit — the caller's two-phase pattern is to walk the
// candidate slice on each Commit failure.
//
// A candidate SelectKAssigned already counted uses that count and cannot
// fail on capacity: nothing could have taken the slot it holds.
//
// For local and external candidates Commit always succeeds (no
// admission slot is refused) and returns the Selection SelectK
// already constructed.
//
// Calling Commit on a zero-value Candidate (or one whose commit
// closure is nil) returns (zero, false).
func (c Candidate) Commit() (Selection, bool) {
	if c.held != nil && c.finish != nil {
		if release, ok := c.held.take(); ok {
			return c.finish(release), true
		}
	}
	if c.commit == nil {
		return Selection{}, false
	}
	return c.commit()
}

// Abandon gives back the count SelectKAssigned took for this candidate,
// when the request is not going to commit it. A candidate that holds
// nothing, or whose count Commit already used, ignores it, so a caller can
// abandon every candidate it did not commit without tracking which one was
// assigned.
func (c Candidate) Abandon() {
	if c.held == nil {
		return
	}
	if release, ok := c.held.take(); ok && release != nil {
		release()
	}
}

// assign takes this candidate's count now and returns the candidate
// holding it. ok is false — and nothing is held — when the candidate has no
// count to take, or its slot is already full.
func (c Candidate) assign() (Candidate, bool) {
	if c.slot == nil || c.finish == nil || c.held != nil {
		return c, false
	}
	release, ok := c.slot()
	if !ok {
		return c, false
	}
	c.held = &heldSlot{release: release}
	return c, true
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
//
// PeerDisplayID comes across with it. The two are a pair — the real
// identifier for matching, the display one for anything a person
// reads — and dropping half left a public-share candidate whose only
// available name was the stranger's device id (spec §8.5, #739).
// buildMeshCandidates has always set both; this constructor is the
// other way a Candidate gets made.
func NewLocalCandidate(sel Selection) Candidate {
	c := Candidate{
		EndpointID:    sel.EndpointID,
		ModelID:       sel.ModelID,
		VariantID:     sel.VariantID,
		Runtime:       sel.Runtime,
		EngineModel:   sel.EngineModel,
		ExecutionMode: sel.ExecutionMode,
		PeerDisplayID: sel.PeerDisplayID,
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
	// localBelowFloor is set when the operator's minimum model class
	// disqualified THIS device's own engine. It is a local, not a field
	// on the Selector, because several requests share one Selector.
	localBelowFloor := false
	modelID := ""
	defer func() {
		if err != nil {
			s.emitPublicShortfall(short, modelID, req.MinContextWindow)
		}
	}()
	// One exit, so every branch's terminal miss carries the reason.
	// A request that found nothing BECAUSE of the floor is not an
	// outage: the operator set that floor and was told the consequence,
	// and the surfaces name it rather than reporting a fault
	// (waired-agent#1128).
	defer func() {
		if err == nil || (!localBelowFloor && short.belowFloor == 0) {
			return
		}
		// "Waired public share" never intended to run here, so a floor that
		// disqualified THIS host's engine is not why the turn was refused,
		// and `waired worker set --min-model-size` is not the switch to
		// send the operator to. Owner ruling 2026-09-06 narrowing
		// waired-agent#1128's floor-first order by this one case
		// (docs/decisions/20260906/0410-...).
		if s.publicOnly() {
			return
		}
		err = &SizeFloorError{
			Err:   err,
			Floor: s.in.MinModelSize,
			// localBelowFloor is only ever set inside `if localReady`, so
			// when it is true this device's model IS ready and the floor
			// alone removed it — exactly the case the base sentence
			// denies.
			LocalArmOnlyFloor: localBelowFloor && localArmMiss(err),
		}
	}()
	// The window floor's exit, registered after the size floor's so it runs
	// first and the size floor stays outermost: that one is the operator's
	// own setting, and naming it outranks naming the row.
	//
	// A miss where the floor removed at least one candidate that would
	// otherwise have answered is the floor's miss, and says so. Before
	// waired-agent#1395 it came back as the generic mesh miss, which names
	// neither the window nor the row. Left alone: a refusal that already
	// names something more specific (a pin, a busy mesh), and weights that
	// are still arriving — waiting for those does end.
	defer func() {
		if err == nil || short.belowWindow == 0 {
			return
		}
		if _, ok := WindowFloor(err); ok {
			return
		}
		if errors.Is(err, ErrAllPeersOverloaded) ||
			errors.Is(err, ErrPinnedPeerUnreachable) ||
			errors.Is(err, ErrPinnedPeerDeclined) ||
			errors.Is(err, ErrPeersDidNotAnswer) ||
			ModelIsArriving(err) {
			return
		}
		err = &WindowFloorError{Need: req.MinContextWindow, Public: s.publicOnly()}
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
		// A name retired with no successor is still a name we shipped, so
		// the answer says so rather than "never heard of it". It stays a
		// not-found for every caller that branches on the error: there is
		// nothing to serve, and a request naming a model is an instruction
		// given now, not a pin to fall back from
		// (docs/decisions/20260916/0340, decision 4).
		if r, retired := catalog.LookupRetirement(req.Model); retired && !catalog.HasSuccessor(r) {
			return nil, fmt.Errorf("%w: %s", ErrModelNotFound, catalog.RetirementRefusal(req.Model, r))
		}
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
	// Only when something was actually required. With both requirements
	// zero — every request in production, since nothing but the
	// /inference/select body can set them — nothing was filtered, so the
	// line asserted a step that did not run, about a manifest that on a
	// node-first selection is not the one that answers (waired-agent#854).
	if req.Requirements.MaxContextTokens > 0 || req.Requirements.NeedJSONMode {
		reasons = append(reasons, fmt.Sprintf("capability filter passed for %q (context_length=%d, json_mode=%v)",
			manifest.ModelID, manifest.ContextLength, hasCapability(manifest.Capabilities, "json_mode")))
	}

	// What the mesh branches below look for. A request that named a
	// model looks for that model; a request that named none is asking
	// the routing mode to pick a NODE, and takes whatever that node is
	// running (waired-agent#828).
	want, err := s.meshWantFor(req, manifest, &reasons)
	if err != nil {
		return nil, err
	}

	// Step 3: locality filter / mesh fallback / external fallback.
	modelState, present := s.in.localModels()[manifest.ModelID]
	// Ready weights are not enough: a host whose operator turned local
	// inference off, or that never installed an engine, has no local
	// candidate no matter what is on disk (waired-agent#829). Every
	// branch below reads localReady, so the fact lands in one place and
	// the mesh branches keep working exactly as they did.
	localReady := present && modelState.State == catalog.ModelStateReady && !s.in.LocalServingOff
	if s.in.LocalActiveOnly {
		active := ""
		if s.in.LocalState.Active != nil {
			active = s.in.LocalState.Active.ModelID
		}
		if active != manifest.ModelID {
			return nil, &ModelNotActiveError{ModelID: manifest.ModelID, Active: active}
		}
	}
	// The operator's routing floor applies to this device's own engine as
	// well as to peers — "ローカルと peer は区別しない" (owner ruling,
	// 2026-08-29, waired-agent#1128). Excluding only peers would make the
	// floor mean "use a big model, unless it is mine", which is not a
	// floor.
	if localReady && s.in.MinModelSize != "" &&
		!variantMeetsSizeFloor(manifest, modelState.VariantID, s.in.MinModelSize) {
		localReady = false
		localBelowFloor = true
		reasons = append(reasons, fmt.Sprintf(
			"this computer's model is smaller than %q (routing floor)", s.in.MinModelSize))
	}
	if s.in.LocalServingOff {
		reasons = append(reasons, "local inference is turned off on this host; only mesh candidates are eligible")
	}
	// Why this host's own engine is not the answer, stated once, from
	// the requester's model and its real state. Every mesh branch below
	// passes meshReasons instead of reasons; makeMeshCandidate used to
	// append a hard-coded "is not ready" of its own (waired-agent#854).
	meshReasons := withReason(reasons, localBypassReason(
		s.in.RoutingMode, s.in.LocalServingOff, manifest.ModelID, modelStateOf(modelState, present)))

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
			return nil, s.localMiss(manifest.ModelID,
				modelStateOf(modelState, present), "routing=local-only")
		}
		reasons = append(reasons, fmt.Sprintf("local state for %q is %q (routing=local-only)",
			manifest.ModelID, modelState.State))
		// Fall through to the local-ready candidate construction below.
	case state.RoutingModePeerPreferred:
		// Mesh first; fall back to local engine only if no mesh peer
		// can serve the request.
		if s.in.MeshSnapshotFn != nil {
			cands, err := s.tryMeshFallbackK(req, want, meshReasons, k, &short, LocalNode{})
			if err != nil {
				return nil, meshSelectionError(err, requestedName(req, manifest.ModelID))
			}
			if len(cands) > 0 {
				return cands, nil
			}
		}
		if !localReady {
			return nil, s.meshMissAfterLocal(want.modelID,
				modelStateOf(modelState, present), "routing=peer-preferred")
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
			// Not modelNotReady: peer-only never consults this host, so its
			// local state is not evidence that waiting helps
			// (waired-agent#1252).
			return nil, &ModelNotReadyError{
				ModelID: manifest.ModelID,
				State:   modelStateOf(modelState, present),
				Note:    "routing=peer-only, no mesh snapshot",
			}
		}
		cands, err := s.tryMeshFallbackK(req, want, meshReasons, k, &short, LocalNode{})
		if err != nil {
			return nil, meshSelectionError(err, requestedName(req, manifest.ModelID))
		}
		if len(cands) > 0 {
			return cands, nil
		}
		if s.publicOnly() {
			// The operator picked one /model row, so the refusal names what
			// declined it rather than the mesh (waired-agent#1201).
			return nil, publicShareDeclined(publicShareDeclineReason(short))
		}
		return nil, meshMiss(want.modelID,
			modelStateOf(modelState, present), "routing=peer-only")
	case state.RoutingModePinned:
		// Pin to a specific peer. tryMeshFallbackK handles the
		// strict / soft semantics: pin-unreachable returns
		// ErrPinnedPeerUnreachable; a reachable pin is served on, with
		// what it runs; only a pin advertising nothing the catalog
		// knows falls through to the rest of the eligible mesh.
		// MeshSnapshotFn==nil happens on the overlay-side Selector,
		// where this mode should never have been set in the first
		// place — fall back to the local-only treatment defensively
		// so an inadvertent overlay-side pin can't loop a peer's
		// gateway through itself.
		if s.in.MeshSnapshotFn == nil {
			if !localReady {
				return nil, s.localMiss(manifest.ModelID,
					modelStateOf(modelState, present), "routing=pinned, no mesh snapshot")
			}
			reasons = append(reasons, "routing=pinned: no mesh snapshot, falling back to local-ready")
			// fall through to local-ready candidate construction.
		} else {
			cands, err := s.tryMeshFallbackK(req, want, meshReasons, k, &short, LocalNode{})
			if err != nil {
				return nil, meshSelectionError(err, requestedName(req, manifest.ModelID))
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
			return nil, meshMiss(want.modelID,
				modelStateOf(modelState, present), "routing=pinned")
		}
	default:
		// RoutingModeAuto or empty.
		//
		// ONE ORDERED LIST (waired-agent#1302). This arm used to be a
		// two-way short-circuit on localReady: a ready local model took
		// the turn without the mesh ever being built, and a local model
		// that was not ready left this device out of the list even while
		// its engine was serving the previous model to peers. Neither is
		// an ordering, and the owner's ruling on peer selection is that
		// there is one — "ローカルと peer は区別しない" (2026-08-29,
		// waired-agent#1128/#1129), applied here to the arm it was made
		// for.
		//
		// Gated on LocalNode being wired AND a mesh snapshot existing:
		// tryMeshFallbackK calls MeshSnapshotFn unconditionally, and the
		// overlay-side Selector has neither. Both absent ⇒ the pre-#1302
		// arm below, byte for byte.
		//
		// And gated once more, on the reading itself: a reading this
		// device could not take is not an ordering input.
		// The local reading is empty whenever the device cannot describe
		// itself — before the first network map gives it a device id,
		// before the active selection records an engine tag, in the
		// seconds after a daemon restart while the engine comes back. In
		// those windows a device whose OWN resolved model is ready must go
		// on serving its own turn, exactly as it did before #1302.
		//
		// Measured on the M5 Pro MacBook Pro (2026-09-12), twenty seconds after a
		// daemon restart: the local reading was still empty, the mesh was
		// not, and ranking sent a turn to a 125B model on another computer
		// at 585 ms rtt while this one held a ready 35B-A3B. So the arm is
		// entered only when the reading exists, or when local was not an
		// answer anyway and there is nothing to lose.
		if s.in.LocalNode != nil && s.in.MeshSnapshotFn != nil {
			local := s.in.LocalNode()
			if local.Serving || !localReady {
				// reasons, not meshReasons: localBypassReason says "this
				// host has no candidate; trying the mesh", which describes
				// a branch this arm does not have.
				cands, err := s.tryMeshFallbackK(req, want, reasons, k, &short, local)
				if err != nil {
					return nil, meshSelectionError(err, requestedName(req, manifest.ModelID))
				}
				if len(cands) > 0 {
					return cands, nil
				}
				if !localReady {
					// Same miss, with the same arguments, as the pre-#1302
					// arm: a single-machine install whose model is still
					// arriving must keep reporting the model and its
					// state, not a mesh verdict.
					return nil, s.localMiss(manifest.ModelID,
						modelStateOf(modelState, present), "")
				}
			}
			reasons = append(reasons, fmt.Sprintf("local state for %q is %q", manifest.ModelID, modelState.State))
			break
		}
		if !localReady {
			if s.in.MeshSnapshotFn != nil {
				cands, err := s.tryMeshFallbackK(req, want, meshReasons, k, &short, LocalNode{})
				if err != nil {
					return nil, meshSelectionError(err, requestedName(req, manifest.ModelID))
				}
				if len(cands) > 0 {
					return cands, nil
				}
			}
			return nil, s.localMiss(manifest.ModelID,
				modelStateOf(modelState, present), "")
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
			deficitLabelFor(variant, engine, s.in.Hardware,
				familyPresentation(manifest, variant, engine, s.in.Hardware))))
	}
	reasons = append(reasons, fmt.Sprintf("engine %q selected", engine))
	if _, ok := s.in.Runtimes.Lookup(engine); !ok {
		return nil, fmt.Errorf("%w: %q (variant %q)",
			ErrRuntimeNotInstalled, engine, variant.VariantID)
	}

	// waired#1031: the tier filter, local half. Same rule as the mesh
	// half in buildMeshCandidates — a device whose declared window falls
	// short of the floor, or that declares none, is not an answer to a
	// tiered request (waired-agent#1395).
	if req.MinContextWindow > 0 {
		w := 0
		if s.in.LocalContextWindow != nil {
			w = s.in.LocalContextWindow()
		}
		customWin := 0
		if s.in.LocalCustomModelWindow != nil {
			customWin = s.in.LocalCustomModelWindow()
		}
		if w < req.MinContextWindow && !customWindowAdmits(req.MinContextWindow, manifest, customWin) {
			return nil, &WindowFloorError{
				Need: req.MinContextWindow,
				// Only local-only is a refusal about this computer alone;
				// the other arms that reach here consulted the mesh first.
				Local:       s.in.RoutingMode == state.RoutingModeLocalOnly,
				LocalWindow: w,
			}
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
	// displayName is what a person calls this peer, for prose only —
	// the reason lines `waired infer --explain` prints. Never an
	// identifier field: see peerLabel, and inferencemesh.PeerDisplayName
	// for why a public machine's name is its pseudonym and never its
	// DeviceName.
	displayName string
	// public marks a Public Share provider injected from a foreign
	// network. It is the dominant sort key (sortMeshCandidates) —
	// own == team > public, per the Team Share routing-order decision.
	public bool
	// team marks a Team Share provider: a teammate's node. It is NOT a
	// sort key — team candidates carry public=false and rank alongside
	// this account's own nodes by the ordinary keys (team share spec
	// §6.1) — and it exists for the surfaces that describe the choice.
	team bool
	// grantID is the Public Share grant this candidate routes under (the
	// netmap PeerView.Grant.ID), set only when public. Reported to the
	// background acquirer on Commit (OnPublicGrantUsed) so a grant that is
	// actually carrying traffic gets renewed and an idle one lapses —
	// closing the "idle consumer holds a stranger's peering forever" gap
	// (waired#898). Empty for own-network peers.
	grantID string
	variant catalog.Variant
	// manifest is the catalog model this peer turned out to be running.
	// It is per candidate, not per request: when the request named no
	// model the routing mode picks a node and the node's own model is
	// the answer, so one candidate set can span several models
	// (waired-agent#828). For a request that DID name a model, every
	// candidate carries that same manifest.
	manifest catalog.Manifest
	runtime  string // catalog.RuntimeOllama or catalog.RuntimeVLLM
	tag      string
	// contextWindow is the window this peer says its engine is loaded
	// with for tag (InferenceState.ContextWindow). 0 = the peer declares
	// nothing. The overflow guard reads that as "unknown"; a window floor
	// has already dropped such a peer (waired-agent#1395).
	contextWindow int
	// belowWindow: admitted under the request's window floor because it
	// serves a custom model under 200,704 tokens (customWindowAdmits);
	// contextWindow is that model's window, which the overflow guard then
	// enforces. Used only when nothing at the floor can take the request.
	belowWindow bool
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

	// sizeClass is this peer's model size class, as
	// hostfit.VariantSize classifies it: small is what an 8 GB card
	// holds, medium a 32 GB card, large above that. "" = the variant
	// carries no weight annotation, which SizeRank ranks below every
	// real class so an operator floor fails closed on it.
	//
	// A filter, never a sort key: the operator's floor EXCLUDES a peer
	// below it (waired-agent#1128) rather than demoting it, so a request
	// with nothing above the floor falls back and says why instead of
	// quietly using a smaller model.
	sizeClass string

	// speedBucket is what this round expects a turn on this peer to cost,
	// in 25 % bands, LOWER IS BETTER — the same direction and the same
	// bucketing idea as rttBucket. Filled by assignSpeedRanks over the
	// whole round, because where a candidate with no finished figure
	// lands depends on the figures the rest of the field published.
	//
	// 0 on a round where nothing is known, and equal to the best known
	// bucket for a candidate this requester has no reading for. A peer
	// that published only a lower bound (a measurement still running past
	// the line) sits below every measured one. See assignSpeedRanks for
	// why an unmeasured peer is ranked optimistically rather than last.
	speedBucket int

	// rankTier groups candidates this Selector considers interchangeable:
	// same tier means every key above tied and only the arbitrary deviceID
	// suffix separated them. Filled by assignRankTiers immediately after the
	// sort, so it is meaningless on an unsorted slice.
	//
	// In-process only, and deliberately so — it is not a wire field, not a
	// score, and carries no meaning outside the one sorted slice it was
	// computed over. See assignRankTiers for why the probe layer needs it.
	// local marks THIS device's own entry in the list.
	//
	// Deliberately NOT a sort key: the ordering must not be able to tell a
	// local candidate from a peer, which is the whole of the owner's ruling
	// (2026-08-29, waired-agent#1128: "ローカルと peer は区別しない"). It
	// changes only what the candidate DOES on commit and how it renders —
	// see makeLocalCandidate. Because sortMeshCandidates never reads it,
	// sameRankExceptDeviceID and its guard need no entry for it.
	local bool
	// capacityUsed is how many conversations the candidate says are in use
	// right now. For a peer it arrives with the probe and lives in
	// PeerSpeeds; it is carried here only for THIS device, whose figure is
	// read in-process and has no probe to ride on.
	capacityUsed int

	rankTier int

	// mapAgeMS is how old the network-map frame every figure above came
	// from was when the snapshot was computed
	// (inferencemesh.Snapshot.MapAgeMS). It is not a property of the
	// peer — it is the same for every candidate of one Select — and it
	// is carried per candidate anyway because the reasons string is
	// per candidate and that is where it has to be readable.
	//
	// It exists because capacity could not be re-diagnosed after the
	// fact (waired-agent#713): on the rc8 hardware run `--explain`
	// printed cap=1 for a peer whose published state said 3, and
	// nothing recorded which frame the 1 came from, so a stale frame
	// and a peer that published 1 and later raised it were
	// indistinguishable in the record.
	//
	// Unlike rttMS this needs no "unknown" rendering. A snapshot with
	// no frame, or one older than Policy.FrameStaleness, marks every
	// peer Stale (inferencemesh.Aggregator: stale = mapDead ||
	// !freshAtReceipt), the filter above drops Stale peers, and a
	// mesh candidate is the only thing that renders this — so 0 here
	// can only mean a frame that just arrived, never "no frame".
	mapAgeMS int64
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
func (s *Selector) tryMeshFallbackK(req Request, want meshWant, reasons []string, k int, short *publicShortfall, local LocalNode) ([]Candidate, error) {
	snap := s.in.MeshSnapshotFn()
	wantOllama, wantVLLM := want.ollama, want.vllm
	if len(wantOllama) == 0 && len(wantVLLM) == 0 {
		return nil, nil
	}

	// Public Share admission gate for this request (waired#827). Resolved
	// once per call; its own-best-tier input is filled lazily inside
	// buildMeshCandidates, only if a grant-tagged peer actually appears.
	gate := s.publicGateFor(req.Class)

	raw, drops := s.buildMeshCandidates(snap, req.Class, req.MinContextWindow, wantOllama, wantVLLM, &gate, modelIsUnspecified(req.Model))
	// This device's own engine, in the same list and through the same
	// filters (waired-agent#1302). The zero LocalNode yields nothing, which
	// is what every arm but the ranked auto one passes.
	localIn := false
	lc, ok, localDropped := s.buildLocalCandidate(local, req.MinContextWindow, want, modelIsUnspecified(req.Model))
	if ok {
		raw = append(raw, lc)
		localIn = true
	}
	// A custom model under the floor is used only when nothing at the floor
	// can take the request: computers that meet 200,704 are preferred
	// (owner ruling 5 on waired-ai/waired#1473).
	raw = preferAtFloor(raw)
	if r := localCandidateReason(local, localIn, localDropped, s.in.LocalServingOff,
		localModelState(s.in.localModels(), local.ModelID), s.in.MinModelSize); r != "" {
		reasons = withReason(reasons, r)
	}
	if localDropped.belowFloor && short != nil {
		short.belowFloor++
	}
	if localDropped.belowWindow && short != nil {
		short.belowWindow++
	}
	if drops.belowWindow > 0 {
		reasons = withReason(reasons, fmt.Sprintf(
			"%d peer(s) excluded: their context window is under the %d tokens this request needs", drops.belowWindow, req.MinContextWindow))
		if short != nil {
			short.belowWindow += drops.belowWindow
		}
	}
	if drops.belowOperatorFloor > 0 {
		reasons = withReason(reasons, fmt.Sprintf(
			"%d peer(s) excluded: their model is smaller than %q (routing floor)", drops.belowOperatorFloor, s.in.MinModelSize))
		if short != nil {
			short.belowFloor += drops.belowOperatorFloor
		}
	}
	if drops.belowPublicFloor > 0 {
		reasons = withReason(reasons, fmt.Sprintf(
			"%d public peer(s) excluded: their model is smaller than %q (Public Share floor)", drops.belowPublicFloor, gate.minSize))
		if short != nil {
			short.belowPublicFloor += drops.belowPublicFloor
		}
	}
	if len(raw) == 0 {
		// Manual pin needs a separate strict check: when the operator
		// has pinned a peer that is not in the snapshot at all (down,
		// stale, disco-unreachable), the request must surface 503
		// ErrPinnedPeerUnreachable rather than the generic
		// ErrModelNotReady the auto branch produces.
		if s.in.RoutingMode == state.RoutingModePinned && s.in.PinnedPeerDeviceID != "" {
			if !pinReachableInSnapshot(snap, s.in.PinnedPeerDeviceID) {
				short.record(snap, gate, NudgeReasonNoCandidate)
				return nil, s.pinUnreachable(snap, want.modelID)
			}
			// The pin is up, and nobody at all serves the requested
			// model — so there is no peer to soft-fall to either. Serve
			// on the pin with what it is running; same ruling as the
			// not-hoisted branch below, and the only branch that can
			// reach it when the mesh is otherwise empty of the model.
			pinned, pinDrops := s.pinnedNodeCandidates(snap, req, &gate)
			if len(pinned) > 0 {
				reasons = append(reasons, pinSubstitutionReason(snap, s.in.PinnedPeerDeviceID, s.in.PinnedPeerDisplayID, pinned[0].manifest.ModelID))
				raw = pinned
			} else if pinDrops.belowOperatorFloor > 0 {
				// Under the operator's routing floor: the empty result below
				// becomes the size-floor refusal, which names that setting.
				if short != nil {
					short.belowFloor += pinDrops.belowOperatorFloor
				}
			} else if err := s.pinDeclined(snap, req, want.modelID, pinDrops); err != nil {
				return nil, err
			}
		}
		if len(raw) == 0 {
			short.record(snap, gate, NudgeReasonNoCandidate)
			return nil, nil
		}
	}

	// The speed key is scored over the whole round before the sort, not
	// inside it: where an unmeasured or bounded candidate lands depends on
	// which candidates are in it (assignSpeedRanks).
	if s.in.PeerSpeeds != nil {
		// One map for the round, this device folded in under its own id:
		// the congestion multiplier has to be comparable across every
		// candidate, which is what assignSpeedRanks assumes.
		assignSpeedRanks(raw, s.withAssigned(roundSpeeds(s.in.PeerSpeeds(), local), local))
	} else if localIn && local.Speed.usable() {
		assignSpeedRanks(raw, s.withAssigned(roundSpeeds(nil, local), local))
	}
	sortMeshCandidates(raw, s.in.Prefer, s.in.TieBreak)
	raw = applyStickyFirst(req, s.in.Sticky, raw)

	// Manual pin override applied AFTER sticky so a deliberate operator
	// pin always wins over a sticky cache hit. Three behaviours:
	//
	//   1. Pin reachable + serves the requested model → hoist to the
	//      head of the candidate slice (strict pin).
	//   2. Pin reachable + running something ELSE → serve on the pin
	//      anyway, with what it is running. A pin names a node, not a
	//      (node, model) pair, and a node running a model its operator
	//      did not plan for is still the node the user asked for
	//      (owner ruling 2026-08-19, docs/decisions/20260819/
	//      1900-routing-selects-a-node-not-a-model.md; it revises the
	//      2026-05-19 soft-fallback answer to the same question). The
	//      substitution is named in the selection reasons, and the
	//      engine's own response names the model that answered.
	//      Falling through to another peer is kept for the one case
	//      left: a pin advertising nothing the catalog knows.
	//   3. Pin absent from the snapshot / stale / disco-unreachable →
	//      503 ErrPinnedPeerUnreachable. Silent fallback was rejected
	//      because it would hide an explicit operator action, and that
	//      half of the 2026-05-19 ruling stands.
	//
	// In 1 and 2 the list is cut to the pin alone. The rest of the mesh used
	// to stay behind it, and the gateway's guard against walking past a pin
	// keys on the Pinned flag of a candidate in the probed set. The admission
	// pre-filter below drops a pin this requester already fills, and a
	// public pin moved behind own peers by partitionOwnFirst can fall
	// outside k; either way the guard saw no pin, and the request ran on
	// another computer with nothing saying so (waired-agent#1365). With the
	// pin alone, a full pin is PinnedPeerBusyError, which names it.
	if s.in.RoutingMode == state.RoutingModePinned && s.in.PinnedPeerDeviceID != "" {
		var onPin []meshCandidate
		for _, c := range raw {
			if c.deviceID == s.in.PinnedPeerDeviceID {
				onPin = append(onPin, c)
			}
		}
		hoisted := len(onPin) > 0
		if hoisted {
			raw = onPin
		}
		if !hoisted {
			// Pin not in the filtered candidate set. Distinguish
			// "pin is up but lacks the model" (soft fallback) from
			// "pin is unreachable" (strict 503) using the full snapshot.
			if !pinReachableInSnapshot(snap, s.in.PinnedPeerDeviceID) {
				return nil, s.pinUnreachable(snap, want.modelID)
			}
			// The pin is up and running something the request did not
			// ask for. Build its candidate from the whole catalog and
			// put it in front.
			pinned, pinDrops := s.pinnedNodeCandidates(snap, req, &gate)
			switch {
			case len(pinned) > 0:
				reasons = append(reasons, pinSubstitutionReason(snap, s.in.PinnedPeerDeviceID, s.in.PinnedPeerDisplayID, pinned[0].manifest.ModelID))
				raw = pinned
			case pinDrops.belowOperatorFloor > 0:
				// Under the operator's routing floor. The rest of the mesh
				// is not an answer to a pin; nothing is, and the size-floor
				// wrapper names the setting that removed it.
				if short != nil {
					short.belowFloor += pinDrops.belowOperatorFloor
				}
				short.record(snap, gate, NudgeReasonNoCandidate)
				return nil, nil
			default:
				// A filter removed the pin, or it serves nothing the
				// catalog knows. Either way the rest of the mesh behind it
				// is not an answer — except for the one case decision 1900
				// kept, which pinDeclined leaves to fall through.
				if err := s.pinDeclined(snap, req, want.modelID, pinDrops); err != nil {
					return nil, err
				}
				if s.in.Recorder != nil {
					// Nothing the catalog knows: there is no model to serve
					// with, so the request does soft-fall to another peer.
					// Emit lacks_model so the tray surfaces the silent miss.
					// Named by pinDisplayID for the reason pinUnreachable is.
					s.in.Recorder.RecordPinnedPeerUnreachable(
						pinDisplayID(snap, s.in.PinnedPeerDeviceID, s.in.PinnedPeerDisplayID), want.modelID, "lacks_model")
				}
			}
		}
	}

	// Re-assert the own > public partition (waired#827). Both hoists
	// above move a candidate to index 0 by deviceID alone, which would
	// otherwise let a public peer outrank every own peer: sticky binds
	// to whatever served the previous turn, and makeMeshCandidate's
	// commit closure Touches the sticky store for public peers too, so
	// one public selection would pin the conversation there for the
	// whole sticky TTL. SliceStable keeps each hoist's effect intact
	// within its own partition.
	partitionOwnFirst(raw)

	// Admission pre-filter: drop peers we already know are at
	// capacity. The Commit closure rechecks via Acquire at commit
	// time so a concurrent request stealing the last slot fails
	// cleanly (ok=false) and the gateway falls to the next
	// candidate.
	var eligible []meshCandidate
	for _, c := range raw {
		// This device's own entry is not tracked here — see acquireSlot.
		// Its occupancy is priced in the ordering, not as an exclusion,
		// which is also what keeps it from being dropped out of a list it
		// may be the only member of.
		if c.local {
			eligible = append(eligible, c)
			continue
		}
		if s.in.LocalInFlight != nil && c.capacity > 0 &&
			int(s.in.LocalInFlight.InFlight(c.deviceID)) >= c.capacity {
			continue
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		short.record(snap, gate, NudgeReasonAllOverloaded)
		// Named at its source (docs/decisions/20260906/0210). A pin
		// considered one computer, so "every matching mesh peer" is not a
		// description of what happened (waired-agent#1303).
		if s.in.RoutingMode == state.RoutingModePinned && s.in.PinnedPeerDeviceID != "" {
			return nil, &PinnedPeerBusyError{
				PeerDisplayID: pinDisplayID(snap, s.in.PinnedPeerDeviceID, s.in.PinnedPeerDisplayID),
				PeerName:      pinDisplayName(snap, s.in.PinnedPeerDeviceID),
				ModelID:       want.modelID,
			}
		}
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
		if eligible[i].local {
			out = append(out, s.makeLocalCandidate(reasons, eligible[i], eligible))
			continue
		}
		out = append(out, s.makeMeshCandidate(req, reasons, eligible[i], eligible, spreadFrom))
	}
	return out, nil
}

// makeLocalCandidate materialises THIS device's entry.
//
// It differs from makeMeshCandidate in five places, and in nothing else:
//
//   - Runtime is the engine name, not "remote:<id>". That one field is the
//     whole dispatch switch: internal/gateway keys local dispatch and the
//     local admission hook off the absence of the remote prefix.
//   - ExecutionMode is "local" and PeerID / PeerDisplayID are empty, which
//     is what keeps this candidate off the network probe —
//     internal/gateway's ParallelProbe pre-settles any non-remote slot as
//     ready, at whatever index it sits.
//   - EndpointID keeps the spelling the pre-#1302 local path produced, so
//     nothing downstream re-keys.
//   - the commit closure never calls acquireSlot. LocalInFlight counts this
//     requester's OUTBOUND overlay requests per peer, and a turn served on
//     this device is not one of those — so there is no slot to take and
//     nothing that could refuse it. What this device's occupancy DOES
//     affect is the ordering, through the congestion divisor in
//     assignSpeedRanks: one axis, one meaning. With Inputs.Assignments
//     wired, the request is counted there instead, never refused, and
//     given back by Release — which is how a request assigned here and
//     still waiting for a slot reaches that divisor (waired-agent#1354).
//   - the sticky store is NOT touched. applyStickyFirst hoists the bound
//     device to index 0 with no ranking check at all, so binding a
//     conversation to this device would rebuild the very short-circuit
//     waired-agent#1302 removes, one layer down. A sticky-bound PEER still
//     outranks this device; that is the KV-affinity rule, unchanged.
func (s *Selector) makeLocalCandidate(reasons []string, c meshCandidate, all []meshCandidate) Candidate {
	decision := Decision{
		Reason:   append(append([]string{}, reasons...), localLine(c)),
		Fallback: fallbackTrace(all, c.deviceID),
	}
	endpointID := computeEndpointID("local", c.runtime, c.manifest.ModelID)
	sel := Selection{
		EndpointID:    endpointID,
		ModelID:       c.manifest.ModelID,
		VariantID:     c.variant.VariantID,
		Runtime:       c.runtime,
		EngineModel:   c.tag,
		ExecutionMode: "local",
		ContextWindow: c.contextWindow,
		Decision:      decision,
		Release:       noopRelease,
	}
	slot := func() (func(), bool) {
		if s.in.Assignments == nil {
			return noopRelease, true
		}
		return s.in.Assignments.addLocal(), true
	}
	finish := func(release func()) Selection {
		out := sel
		out.Release = release
		return out
	}
	return Candidate{
		EndpointID:    endpointID,
		ModelID:       c.manifest.ModelID,
		VariantID:     c.variant.VariantID,
		Runtime:       c.runtime,
		EngineModel:   c.tag,
		ExecutionMode: "local",
		RankTier:      c.rankTier,
		Decision:      decision,
		slot:          slot,
		finish:        finish,
		commit: func() (Selection, bool) {
			release, _ := slot()
			return finish(release), true
		},
	}
}

// pinSubstitutionReason is the one sentence both pin-substitution
// branches add to the selection reasons. The substitution is never
// silent: `waired infer --explain` prints these, and the engine's own
// response names the model that answered.
func pinSubstitutionReason(snap inferencemesh.Snapshot, pinnedDeviceID, savedDisplayID, modelID string) string {
	return fmt.Sprintf(
		"pinned peer %s is serving %q; a pin names a node, so this request is served there rather than routed around it",
		pinDisplayLabel(snap, pinnedDeviceID, savedDisplayID), modelID)
}

// pinDisplayLabel names the pinned peer the way makeMeshCandidate names
// a mesh candidate, so one machine reads the same on both lines of the
// same trace. Mirrors pinDisplayID's absent-from-snapshot fallback: a
// pin whose peer has dropped out is named by the identifier recorded
// when the pin was set.
func pinDisplayLabel(snap inferencemesh.Snapshot, pin, saved string) string {
	id := pinDisplayID(snap, pin, saved)
	for i := range snap.Peers {
		if snap.Peers[i].DeviceID != pin {
			continue
		}
		if name, ok := inferencemesh.PeerDisplayName(snap.Peers[i]); ok {
			return peerLabel(name, id)
		}
		break
	}
	return peerLabel("", id)
}

// pinnedNodeCandidates builds the pinned peer's candidates from the
// WHOLE catalog rather than from the request's want set — "serve on this
// node with whatever it is running" (waired-agent#828).
//
// It re-enters buildMeshCandidates with a snapshot narrowed to the pin
// so every filter that applies to any other candidate applies here too:
// the declared context window a /model tier demands, the per-class
// serving exclusions, and the Public Share admission gate. A pin that
// fails one of those is not a candidate, and the drops say which one, so
// the refusal can name it (pinDeclined, waired-agent#1395).
func (s *Selector) pinnedNodeCandidates(snap inferencemesh.Snapshot, req Request, gate *publicGate) ([]meshCandidate, meshDrops) {
	var only inferencemesh.Snapshot
	only.MapAgeMS = snap.MapAgeMS
	for i := range snap.Peers {
		if snap.Peers[i].DeviceID == s.in.PinnedPeerDeviceID {
			only.Peers = append(only.Peers, snap.Peers[i])
			break
		}
	}
	if len(only.Peers) == 0 {
		return nil, meshDrops{}
	}
	o, v := wantSetsFor(s.in.Manifests)
	return s.buildMeshCandidates(only, req.Class, req.MinContextWindow, o, v, gate, false)
}

// pinDeclined is the refusal for a reachable pin that yielded no candidate,
// or nil when the request may go on without one.
//
// nil for a `waired worker` pin serving nothing the catalog knows: decision
// 1900 let that one fall through to the rest of the mesh, and a model row
// naming the computer (PinnedStrict) is the only pin that now refuses it.
// A pin under the operator's routing floor never reaches here — the callers
// leave that to the size-floor refusal, which names the setting.
func (s *Selector) pinDeclined(snap inferencemesh.Snapshot, req Request, modelID string, d meshDrops) error {
	reason := pinDeclineReason(d)
	if reason == "" {
		if !s.in.PinnedStrict {
			return nil
		}
		reason = PinDeclinedUnknownModel
	}
	e := &PinnedPeerDeclinedError{
		PeerDisplayID: pinDisplayID(snap, s.in.PinnedPeerDeviceID, s.in.PinnedPeerDisplayID),
		PeerName:      pinDisplayName(snap, s.in.PinnedPeerDeviceID),
		ModelID:       modelID,
		Reason:        reason,
	}
	if reason == PinDeclinedWindow {
		e.Need, e.Declared = req.MinContextWindow, d.declaredWindow
	}
	return e
}

// makeMeshCandidate freezes one meshCandidate into the Candidate
// shape, capturing everything Commit needs in a closure. The closure
// performs the actual InFlightTracker Acquire and sticky Touch, so
// SelectK never modifies global state — only the gateway's call to
// Commit does, and SelectKAssigned, which takes the first candidate's
// InFlightTracker count (and nothing else) while it holds the ranking
// lock.
//
// spreadFrom names the peer demoteBusySticky moved out of the way, or
// "" when the sticky binding was left alone. It is what tells the
// commit closure that landing somewhere else is deliberate rather than
// a new preference.
func (s *Selector) makeMeshCandidate(req Request, reasons []string, c meshCandidate, all []meshCandidate, spreadFrom string) Candidate {
	manifest := c.manifest
	kindLabel := "mesh fallback"
	switch {
	case c.public:
		kindLabel = "public share fallback"
	case c.team:
		kindLabel = "team share fallback"
	}
	candReasons := append(append([]string{}, reasons...),
		// Why local was bypassed is stated by the branch that decided it
		// (localBypassReason), not here: this function knows only the
		// CANDIDATE's model, so the line it used to append named the
		// wrong model and asserted a local fact that was often false
		// (waired-agent#854).
		//
		// map_age_ms comes last, and it qualifies every figure before it:
		// they were all read off one network-map frame, and that is how
		// old the frame was. Without it cap= cannot be re-diagnosed
		// (waired-agent#713).
		fmt.Sprintf("%s: peer %s has %s model %q reachable (score=%d, err=%.2f, rtt_ms=%s, in_flight=%d, cap=%d, load=%.2f, silent=%v, map_age_ms=%d)",
			kindLabel, peerLabel(c.displayName, c.displayID), c.runtime, c.tag, c.score, c.errorRate, rttDisplay(c.rttMS), c.inFlight, c.capacity, c.loadFraction, c.silent, c.mapAgeMS),
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
	slot := func() (func(), bool) { return s.acquireSlot(c) }
	cand := Candidate{
		EndpointID:    endpointID,
		ModelID:       manifest.ModelID,
		VariantID:     c.variant.VariantID,
		Runtime:       runtimeStr,
		EngineModel:   c.tag,
		ExecutionMode: "remote",
		PeerID:        c.deviceID,
		PeerDisplayID: c.displayID,
		RTTMS:         c.rttMS,
		RankTier:      c.rankTier,
		Decision:      decision,
		Pinned: s.in.RoutingMode == state.RoutingModePinned &&
			s.in.PinnedPeerDeviceID != "" &&
			c.deviceID == s.in.PinnedPeerDeviceID,
		slot: slot,
		finish: func(release func()) Selection {
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
			}
		},
	}
	cand.commit = func() (Selection, bool) {
		release, ok := slot()
		if !ok {
			return Selection{}, false
		}
		return cand.finish(release), true
	}
	return cand
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

// meshSelectionError wraps a mesh-fallback failure with the model id the
// request named. All four routing-mode branches in SelectK go through it so
// the shape a client receives cannot drift between them. Exactly two errors
// reach it, both from tryMeshFallbackK: ErrAllPeersOverloaded and
// *PinnedPeerUnreachableError.
//
// The wrapping itself is load-bearing and stays: the gateway's probe round
// returns ErrAllPeersOverloaded BARE, and the model id after the sentinel is
// the only thing telling this admission pre-filter apart from that probe
// layer. waired-agent#624's diagnosis read one as the other, and
// internal/gateway/mesh_distance_e2e_test.go now pins both shapes.
//
// What went is a second copy of err, which used to render in parentheses
// after the model id. %w and %v render an error identically, so the copy
// could never carry anything the prefix had not already said — it repeated
// the sentence to the operator with the useful half, the model id, buried
// between the two copies, and for a pinned-peer failure it repeated the
// peer's name along with it (waired-agent#752).
func meshSelectionError(err error, modelID string) error {
	return fmt.Errorf("%w: %q", err, modelID)
}

// requestedName is what a mesh selection error names as the thing the
// client asked for: the /model row it picked when it picked one, and the
// catalog id otherwise.
//
// A row names a computer, not a model, so the router is handed the
// "caller named none" alias and resolves it to this host's own default
// (see Request.NodeDirective). Naming that resolved id told an OpenCode
// user on the peers-only row that every peer was at capacity for the
// requester's own local model, which none of those peers runs
// (waired-agent#1366).
func requestedName(req Request, modelID string) string {
	if req.NodeDirective != "" {
		return req.NodeDirective
	}
	return modelID
}

// pinUnreachable emits the strict-pin event and builds the error for the
// "operator pinned a peer that cannot serve right now" case. Both call
// sites in tryMeshFallbackK go through it so the recorded event and the
// returned error shape cannot drift apart.
func (s *Selector) pinUnreachable(snap inferencemesh.Snapshot, modelID string) error {
	// One value for both: the event lands in the ring the management API
	// serves and in a debug log line, and those are surfaces a public
	// machine's real device id may not reach any more than the error
	// string is (#739, spec §8.5). Before, the error was named correctly
	// and the event beside it was not.
	display := pinDisplayID(snap, s.in.PinnedPeerDeviceID, s.in.PinnedPeerDisplayID)
	if s.in.Recorder != nil {
		s.in.Recorder.RecordPinnedPeerUnreachable(display, modelID, "unreachable")
	}
	return &PinnedPeerUnreachableError{
		PeerDisplayID: display,
		PeerName:      pinDisplayName(snap, s.in.PinnedPeerDeviceID),
		ModelID:       modelID,
	}
}

// pinDisplayID is the identifier that may be shown for the pinned peer.
// A Public Share peer must be named by its grant pseudonym and never by
// its real device id (spec §8.5), and a teammate's by its
// "<device> (<owner>)" label. A pin that is absent from the snapshot is
// named by saved, the identifier recorded when the pin was set
// (Input.PinnedPeerDisplayID).
// pinDisplayName is the NAME for the same peer, when the snapshot holds one.
// It defers to inferencemesh.PeerDisplayName, which returns the grant
// pseudonym for a Public Share peer, so the §8.5 rule is enforced in one
// place rather than restated here.
func pinDisplayName(snap inferencemesh.Snapshot, pin string) string {
	for i := range snap.Peers {
		if snap.Peers[i].DeviceID != pin {
			continue
		}
		if name, ok := inferencemesh.PeerDisplayName(snap.Peers[i]); ok {
			return name
		}
		break
	}
	return ""
}

func pinDisplayID(snap inferencemesh.Snapshot, pin, saved string) string {
	for i := range snap.Peers {
		if snap.Peers[i].DeviceID != pin {
			continue
		}
		if snap.Peers[i].Grant == nil {
			return pin
		}
		// A grant peer — a stranger's machine or a teammate's — is named
		// through the one display rule, and never falls back to the pin,
		// which is another account's device id.
		return inferencemesh.PeerDisplayLabel(snap.Peers[i])
	}
	// Absent from the snapshot: the name recorded when the pin was set.
	// A pinned teammate who stopped sharing, or a public machine whose
	// pass lapsed, is exactly this case, and the raw pin would print
	// another account's device id. Only a pin written by an agent that
	// predates the recorded name falls through to the pin itself — and
	// those were settable only from your own machines.
	if saved != "" {
		return saved
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
	wantOllama, wantVLLM map[string]wantEntry,
	gate *publicGate,
	unnamed bool,
) (cands []meshCandidate, drops meshDrops) {
	minSize := s.in.MinModelSize
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

	const noRTT = RTTUnknown

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
		// "Waired public share" asked for someone else's computer
		// specifically, so this host's own peers are not near-misses to
		// fall back on — they are the thing that was excluded
		// (waired-agent#901).
		// A teammate's computer is not "someone else's computer" in that
		// sense either: public-only means a Public Share provider.
		if s.publicOnly() && !inferencemesh.IsPublicGrant(p.Grant) {
			continue
		}
		displayID, isPublic, isTeam := p.DeviceID, false, false
		// Resolved through the shared helper rather than from
		// p.DeviceName, so a grant peer cannot be named by its real
		// machine name here (spec §8.5). ok=false cannot reach the
		// candidate literal — the grant branch below drops a pseudonym-less
		// public peer — but it is read through the helper regardless so
		// there is one answer to "what is this peer called".
		displayName, _ := inferencemesh.PeerDisplayName(p)
		// Team Share (team share spec §6.1): a teammate's node joins the
		// same pool as this account's own nodes — same tier, same keys —
		// and none of the Public Share consumer policy applies to it:
		// joining the team was the consent, so there is no use mode, no
		// minimum tier and no auto-mode comparison. The node owner's
		// ExcludeMain / ExcludeSub still apply below, as for any peer.
		if isTeamProvider(&p) {
			label, ok := inferencemesh.PeerDisplayID(p)
			if !ok {
				// Nothing we may call it. The control plane refuses a
				// team member with no name, so this should not happen;
				// routing to a peer we cannot name is worse than not.
				continue
			}
			displayID, isTeam = label, true
		} else if p.Grant != nil {
			if !isPublicProvider(&p) {
				continue
			}
			pseudonym, ok := publicDisplayID(p.Grant)
			if !ok {
				continue
			}
			if !gate.admit {
				drops.publicDeclined++
				continue
			}
			tier := s.peerTier(p.InferenceState.Type, p.InferenceState.Models)
			if gate.auto {
				// Deferred until a public peer actually shows up, so the
				// common no-public-peers path never pays for the scan.
				s.ensureBeat(gate, snap)
			}
			switch gate.admits(tier, s.peerSize(p.InferenceState.Type, p.InferenceState.Models)) {
			case publicAdmitYes:
			case publicAdmitBelowMinSize:
				// The consumer's own Public Share floor. Counted separately
				// from the operator's routing floor so the refusal can name
				// the right command (waired-agent#1201).
				drops.belowPublicFloor++
				continue
			default:
				drops.publicDeclined++
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
				drops.excludedMain++
				continue
			}
		case state.ClaudeClassSub:
			if p.InferenceState.ExcludeSub {
				drops.excludedSub++
				continue
			}
		}
		kind := p.InferenceState.Type
		if kind == "" {
			kind = catalog.RuntimeOllama
		}
		var want map[string]wantEntry
		switch kind {
		case catalog.RuntimeOllama:
			want = wantOllama
		case catalog.RuntimeVLLM:
			want = wantVLLM
		default:
			continue
		}
		for _, m := range p.InferenceState.Models {
			e, ok := want[m]
			if !ok {
				continue
			}
			// waired#1031: a tier is a promise about the serving node. Drop
			// a peer whose declared window falls short of what this request
			// demands — and one declaring nothing (0), which is what a
			// computer serving under the smallest declarable window
			// publishes (waired-agent#1395). Tested after the model matched,
			// so the count is of peers the floor alone removed, which is
			// what lets the refusal name the window.
			window, belowWindow := p.InferenceState.ContextWindow, false
			if minWindow > 0 && p.InferenceState.ContextWindow < minWindow {
				if !customWindowAdmits(minWindow, e.manifest, p.InferenceState.CustomModelWindow) {
					drops.belowWindow++
					drops.declaredWindow = p.InferenceState.ContextWindow
					break
				}
				window, belowWindow = p.InferenceState.CustomModelWindow, true
			}
			// A custom model the request did not name, on a computer it
			// did not name: the account's switch for the rows that name
			// neither (Waired / Waired peer) decides, resolved for this
			// recipient by the control plane (waired-ai/waired#1473 ruling
			// 5). A pin to this computer names it.
			if unnamed && e.manifest.Provenance == catalog.ProvenanceCustom && p.InferenceState.ExcludeUnpinned &&
				(s.in.RoutingMode != state.RoutingModePinned || p.DeviceID != s.in.PinnedPeerDeviceID) {
				drops.excludedUnpinned++
				break
			}
			v := e.variant
			// The operator's minimum model class (waired-agent#1128).
			// EXCLUDES rather than demotes — owner ruling, 2026-08-29 —
			// and applies to every candidate, so a public peer ends up
			// held to the stricter of this and PublicPolicy.MinModelSize.
			size := hostfit.VariantSize(v)
			if minSize != "" && hostfit.SizeRank(size) < hostfit.SizeRank(minSize) {
				drops.belowOperatorFloor++
				continue
			}
			c := meshCandidate{
				deviceID:      p.DeviceID,
				displayID:     displayID,
				displayName:   displayName,
				public:        isPublic,
				team:          isTeam,
				variant:       v,
				manifest:      e.manifest,
				runtime:       kind,
				tag:           m,
				contextWindow: window,
				belowWindow:   belowWindow,
				priority:      p.InferenceState.Priority,
				silent:        p.Silent,
				capacity:      p.InferenceState.Capacity,
				score:         int64(v.ParamCount) * int64(v.QuantizationTier),
				sizeClass:     size,
				rttMS:         noRTT,
				mapAgeMS:      snap.MapAgeMS,
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
	return out, drops
}

// meshDrops is what one buildMeshCandidates pass had to throw away for a
// reason a refusal may have to name. Two floors, two settings, two
// commands: belowOperatorFloor is Inputs.MinModelSize
// (`waired worker set --min-model-size`, waired-agent#1128) and
// belowPublicFloor is PublicPolicy.MinModelSize
// (`waired public use --min-model-size`, waired-agent#1201). Keeping them
// apart is the point — folding them would send an operator to the switch
// they did not set.
type meshDrops struct {
	belowOperatorFloor int
	belowPublicFloor   int
	// belowWindow counts peers that served what the request wanted but
	// declared a window under its floor, and declaredWindow is the last such
	// peer's figure — exact when the pass was over one pinned peer
	// (waired-agent#1395).
	belowWindow    int
	declaredWindow int
	// excludedMain / excludedSub / publicDeclined are the other filters a
	// pinned peer can fail. Only pinDeclineReason reads them.
	excludedMain   int
	excludedSub    int
	publicDeclined int
	// excludedUnpinned counts peers serving a custom model that the rows
	// naming no computer may not land on (InferenceState.ExcludeUnpinned).
	excludedUnpinned int
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
func sortMeshCandidates(cands []meshCandidate, prefer state.RoutingPrefer, tieBreak func(n int) int) {
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
		// What the operator asked this ordering to optimise for
		// (waired-agent#1128). The two keys are the SAME PAIR in both
		// arms, in the other order — so `size` keeps today's ordering and
		// `speed` puts the cost of the turn above the size of the model,
		// with the other still deciding when the first ties.
		//
		// score stays under `speed` rather than being replaced: the speed
		// term only separates peers this requester has a comparable
		// reading for, and where it has none the ordering falls back to
		// exactly what it was. That is the nil rule
		// (docs/decisions/20260822/0218) — never punish an endpoint you
		// have not measured — expressed as an ordering.
		if prefer == state.RoutingPreferSize {
			if cands[i].score != cands[j].score {
				return cands[i].score > cands[j].score
			}
			if cands[i].speedBucket != cands[j].speedBucket {
				return cands[i].speedBucket < cands[j].speedBucket
			}
		} else {
			if cands[i].speedBucket != cands[j].speedBucket {
				return cands[i].speedBucket < cands[j].speedBucket
			}
			if cands[i].score != cands[j].score {
				return cands[i].score > cands[j].score
			}
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
	assignRankTiers(cands)
	shuffleWithinTiers(cands, tieBreak)
}

// shuffleWithinTiers reorders each rank tier, which by construction is a set
// this Selector cannot tell apart: assignRankTiers groups runs where every
// key above the deviceID matched, and its own comment calls that suffix
// "arbitrary as far as the request is concerned".
//
// Arbitrary and DETERMINISTIC are different things, and the difference is a
// herd. Three computers ranking the same tied peers at the same instant all
// apply the same spelling and all pick the same one; two of them then queue
// behind the first while a third machine sits idle. Measured on the rc6
// fleet (waired-agent#1303, S4): A→C, B→C, C→B, with A idle and the second
// turn on C taking 217.9 s to its first byte.
//
// This is the residual after waired-agent#1302 — the reason the herd formed
// at all was that A and B were idle machines that could not see themselves
// in the list. Once they can, each answers its own turn and the tie is
// rarely reached. What is left is the genuinely simultaneous window, before
// any capacity_used has moved, and a random pick inside a tie is the
// cheapest thing that breaks it. It overturns no ordering: everything above
// the tie already decided.
//
// tieBreak nil ⇒ the deterministic order, which is what every ordering test
// relies on and what a build that does not wire it keeps.
func shuffleWithinTiers(cands []meshCandidate, tieBreak func(n int) int) {
	if tieBreak == nil || len(cands) < 2 {
		return
	}
	for start := 0; start < len(cands); {
		end := start + 1
		for end < len(cands) && cands[end].rankTier == cands[start].rankTier {
			end++
		}
		// Fisher-Yates over the run. Tiers stay contiguous and keep their
		// index, so a caller comparing rankTier with == is unaffected.
		for i := end - 1; i > start; i-- {
			j := tieBreak(i - start + 1)
			if j < 0 || j > i-start {
				continue
			}
			cands[i], cands[start+j] = cands[start+j], cands[i]
		}
		start = end
	}
}

// assignRankTiers groups an already-sorted candidate list into runs that this
// Selector considers interchangeable: everything above tied, and only the
// deviceID suffix — the deterministic-pick tie-break, which is arbitrary as
// far as the request is concerned — separates them.
//
// It exists because residency lives one layer away (waired-agent#880). Which
// peer holds its weights in memory is answered by /healthz, i.e. at PROBE
// time, and every ranking key above is a snapshot fact known only HERE. The
// two never meet, so the probe layer cannot tell "this peer outranks that one"
// from "these two are indistinguishable and one of them happens to be warm".
// The tier is the one bit it needs, and it is deliberately the only thing this
// change hands over: residency must break a tie, never overturn a ranking. A
// peer that is genuinely better on quality, priority, error rate, distance or
// load keeps winning while it is cold.
//
// Tiers are contiguous run indices over the sorted slice, so a caller can
// compare them with == and nothing else. The common mesh has exactly one run:
// two idle LAN machines serving the same model tie on every key there is.
func assignRankTiers(cands []meshCandidate) {
	tier := 0
	for i := range cands {
		if i > 0 && !sameRankExceptDeviceID(cands[i-1], cands[i]) {
			tier++
		}
		cands[i].rankTier = tier
	}
}

// sameRankExceptDeviceID reports whether two candidates tie on every sort key
// except the deviceID suffix.
//
// Deliberately a separate function rather than a reuse of the comparator: the
// comparator answers "does i come before j", and calling it both ways round to
// infer equality would silently start answering the wrong question the moment
// a non-total key is added to it. This lists the keys, so a key added there
// and not here is a compile-time-invisible bug — which is what the test that
// walks both lists is for.
func sameRankExceptDeviceID(a, b meshCandidate) bool {
	return a.public == b.public &&
		a.silent == b.silent &&
		a.priority == b.priority &&
		a.score == b.score &&
		a.speedBucket == b.speedBucket &&
		a.errorRate == b.errorRate &&
		rttBucket(a.rttMS) == rttBucket(b.rttMS) &&
		a.loadFraction == b.loadFraction
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
		if c.local {
			// Not "remote:<id>": this device is not a peer of itself, and
			// the trace is read by `waired diagnose` and the management
			// API (waired-agent#1302).
			out = append(out, FallbackCandidate{
				EndpointID: computeEndpointID("local", c.runtime, "_"),
				Runtime:    c.runtime,
			})
			continue
		}
		out = append(out, FallbackCandidate{
			EndpointID: computeEndpointID("remote-"+c.displayID, c.runtime, "_"),
			Runtime:    "remote:" + c.displayID,
		})
	}
	return out
}

// meshWant is what a mesh branch matches peers against: the engine
// identifiers that count as a hit, plus the model id to name in errors
// and pin events.
//
// modelID is empty when the request named no model. Nothing is missing
// then — the question "which model?" has no answer until a node is
// chosen, and saying so beats naming the requester's own model, which is
// what the 404s in waired-agent#828 did.
type meshWant struct {
	ollama, vllm map[string]wantEntry
	modelID      string
}

// modelIsUnspecified reports that the caller left the model open: the
// bare `waired infer`, a client config pointing at the dynamic alias, or
// (through the Claude surface's remap) a coding agent naming an
// Anthropic id no catalog holds.
//
// `waired/default` used to resolve to the REQUESTER's model before the
// routing mode was consulted, and the mesh was then searched for that
// model alone. Since one agent advertises exactly one model, a pin or a
// peer-only fleet worked only when both ends happened to run the same
// thing — the pin's own motivating case, "use the GPU machine from the
// laptop", 404'd (waired-agent#828, waired-ai/waired#1223).
func modelIsUnspecified(name string) bool {
	return name == "" || slices.Contains(DynamicCodingAliases, name)
}

// meshWantFor builds the want sets for this request. A named model
// yields exactly the set the mesh branch has always used. An unnamed one
// yields every catalog model that satisfies the request's requirements,
// so the branch ranks NODES and each node's own model comes along.
func (s *Selector) meshWantFor(req Request, manifest catalog.Manifest, reasons *[]string) (meshWant, error) {
	if !modelIsUnspecified(req.Model) {
		o, v := variantWantSets(manifest)
		return meshWant{ollama: promoteWantSet(o, manifest), vllm: promoteWantSet(v, manifest), modelID: manifest.ModelID}, nil
	}
	eligible := make([]catalog.Manifest, 0, len(s.in.Manifests))
	for _, m := range s.in.Manifests {
		if req.Requirements.MaxContextTokens > 0 && m.ContextLength < req.Requirements.MaxContextTokens {
			continue
		}
		if req.Requirements.NeedJSONMode && !hasCapability(m.Capabilities, "json_mode") {
			continue
		}
		eligible = append(eligible, m)
	}
	if len(eligible) == 0 {
		return meshWant{}, fmt.Errorf("%w: no catalog model meets the request's requirements", ErrCapabilityNotMet)
	}
	*reasons = append(*reasons, fmt.Sprintf(
		"request named no model: any of %d catalog models a reachable node serves is eligible", len(eligible)))
	o, v := wantSetsFor(eligible)
	return meshWant{ollama: o, vllm: v}, nil
}

// promoteWantSet lifts variantWantSets' per-manifest map into the
// manifest-carrying shape buildMeshCandidates consumes.
func promoteWantSet(in map[string]catalog.Variant, m catalog.Manifest) map[string]wantEntry {
	out := make(map[string]wantEntry, len(in))
	for k, v := range in {
		out[k] = wantEntry{variant: v, manifest: m}
	}
	return out
}

// wantEntry is one engine-native identifier a peer could advertise,
// together with the catalog model it came from. buildMeshCandidates
// carries the manifest onto the candidate, so a candidate set drawn
// from more than one model still knows which model each node is
// running (waired-agent#828).
type wantEntry struct {
	variant  catalog.Variant
	manifest catalog.Manifest
}

// wantSetsFor is variantWantSets over a SET of models: the two maps a
// mesh branch matches peers against when the request did not name a
// model and the routing mode is choosing a node instead
// (waired-agent#828).
//
// Two manifests can claim one engine identifier — a retired entry and
// its successor sharing a tag, say. The stronger model wins, scored the
// way mesh candidate ordering scores (ParamCount × QuantizationTier),
// with ModelID as the deterministic tie-break, so the same fleet always
// resolves the same way.
func wantSetsFor(manifests []catalog.Manifest) (ollama, vllm map[string]wantEntry) {
	ollama = map[string]wantEntry{}
	vllm = map[string]wantEntry{}
	put := func(m map[string]wantEntry, key string, e wantEntry) {
		if prev, ok := m[key]; ok && !strongerWant(e, prev) {
			return
		}
		m[key] = e
	}
	for _, man := range manifests {
		for _, v := range man.Variants {
			if supports(v.RuntimeSupport, catalog.RuntimeOllama) && v.Source.Tag != "" {
				put(ollama, v.Source.Tag, wantEntry{variant: v, manifest: man})
			}
			if supports(v.RuntimeSupport, catalog.RuntimeVLLM) && v.Source.RepoID != "" {
				put(vllm, v.Source.RepoID, wantEntry{variant: v, manifest: man})
			}
		}
	}
	return ollama, vllm
}

func wantScore(e wantEntry) int64 {
	return int64(e.variant.ParamCount) * int64(e.variant.QuantizationTier)
}

func strongerWant(a, b wantEntry) bool {
	if sa, sb := wantScore(a), wantScore(b); sa != sb {
		return sa > sb
	}
	return a.manifest.ModelID < b.manifest.ModelID
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

// localBypassReason says why this host's own engine is not the answer,
// for the branches that hand the request to the mesh.
//
// It replaces the line makeMeshCandidate used to append unconditionally,
// `local state for %q is not ready`, which named the CANDIDATE's model
// and then asserted a local fact about it. On a pinned selection with
// the model ready on disk that line was simply false — measured on a
// host whose `waired models ls` said `ready` while `waired infer
// --explain` said it was not (waired-agent#854). Local readiness was
// never consulted there at all: a pin names a node
// (docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md).
//
// modelID is the requester's resolved model — the one this host would
// have run — and localState is its real state, so the success trace now
// reports what the mesh MISS path already reported (same decision, §5).
//
// "" when there is nothing to add: with local serving off, the line
// above already said so, and localReady is false regardless of what is
// on disk, so a state here would read as a contradiction.
//
// PIN: product contract — the decision record above, via waired-agent#854.
// RoutingModeLocalOnly never reaches a mesh branch, so it takes the same
// wording as auto; that arm is a record of today's routing, not a promise.
func localBypassReason(mode state.RoutingMode, servingOff bool, modelID, localState string) string {
	if servingOff {
		return ""
	}
	switch mode {
	case state.RoutingModePinned:
		return fmt.Sprintf(
			"routing=pinned: a pin names a node, so this host's own engine is not consulted; local state for %q is %q",
			modelID, localState)
	case state.RoutingModePeerOnly:
		return fmt.Sprintf(
			"routing=peer-only: this host is set not to serve; local state for %q is %q",
			modelID, localState)
	case state.RoutingModePeerPreferred:
		return fmt.Sprintf(
			"routing=peer-preferred: a mesh peer is tried before this host's own engine; local state for %q is %q",
			modelID, localState)
	default:
		return fmt.Sprintf(
			"local state for %q is %q, so this host has no candidate; trying the mesh",
			modelID, localState)
	}
}

// withReason returns reasons plus r, on a copy, or reasons unchanged
// when r is empty.
//
// The copy matters: the peer-preferred branch keeps appending to its own
// `reasons` after the mesh attempt returns nothing, so sharing a backing
// array would let a mesh-branch line surface in the local-ready trace.
func withReason(reasons []string, r string) []string {
	if r == "" {
		return reasons
	}
	out := make([]string, len(reasons), len(reasons)+1)
	copy(out, reasons)
	return append(out, r)
}

// peerLabel is how a reason line names a peer: the name a person would
// use, with the identifier alongside it.
//
// Both, because the two surfaces a reader goes to next accept different
// forms — `waired peers list` prints NAME and DEVICE-ID as separate
// columns, and `waired worker set --pin` takes either (waired-agent#888).
//
// Collapses to the identifier alone when the two are equal: an
// own-network peer that reported no name, and every public machine,
// whose pseudonym IS its display identifier.
func peerLabel(displayName, displayID string) string {
	if displayName == "" || displayName == displayID {
		return fmt.Sprintf("%q", displayID)
	}
	return fmt.Sprintf("%q (%s)", displayName, displayID)
}

// speedBucketRatio is the band width of the speed key: readings within
// 25 % of each other land in one bucket and do not reorder anything.
//
// A margin, not a raw comparison, and every comparable system surveyed
// while deciding this uses one — Cassandra's dynamic snitch overrides the
// topology order only past dynamic_snitch_badness_threshold (0.1),
// MongoDB picks freely inside localThresholdMS (15 ms) of the fastest
// server. Two reasons here. A continuous key would put every candidate in
// its own RankTier, and the residency tie-break of waired-agent#880 breaks
// ties WITHIN a tier — so a raw number would silently retire it. And a
// measurement that agrees with another to 10 %
// (hostfit.SpeedMeasurementSecondSampleBand) has not earned the right to
// overturn a standing order by 1 %.
const speedBucketRatio = 1.25

// assignSpeedRanks fills speedBucket over a whole selection round.
//
// Per round, not per candidate, because an unmeasured or bounded candidate is
// placed relative to the others in the round.
//
// # Seconds per request
//
// Every agent publishes what a 32,768-token request costs it
// (hostfit.TurnSecondsAt: prefill and decode together, decision 9 of
// docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md).
// The quantity is the turn's expected COST, so lower is better, like
// rttBucket:
//
//	slowness = (capacity_used + 1) × turn_seconds
//
// A peer that published only a lower bound — its measurement is still
// running past the line — is placed one bucket below the slowest measured
// candidate (or at its own bound's bucket, if that is lower still): it is
// known to be over the line, which no finished figure in the round is
// known to be worse than.
//
// # The congestion term
//
// capacity_used is the peer's own in-flight count from the last probe. It
// counts the peer owner's own work as well as mesh traffic, which is
// exactly right — a machine busy with its owner's turn is busy. The +1 is
// this request. Owner ruling, 2026-08-29: "混雑による影響係数は
// 素のprefill速度/(既存セッション数+1) でみる"; decision 9 carries it over
// to seconds as a multiplier.
//
// The multiplier is a SLOPE and prefix eviction is a CLIFF — a lost prefix
// costs a full re-prefill, 2.57 s against 35.38 s on one measured host.
// One number cannot express both, and it does not have to: once Capacity
// means warm conversation slots (waired-agent#1126) admission guarantees
// capacity_used + 1 <= slots, so this term is bounded and describes only
// compute sharing.
//
// # An unmeasured peer gets the best known bucket, not the worst
//
// Ranking it last would punish an endpoint nobody has measured, which the
// nil rule forbids (docs/decisions/20260822/0218). Leaving it out of the
// ordering is not available either: a key that ties with everything is
// not a total order, and sort.SliceStable given one answers arbitrarily.
//
// So an unmeasured candidate is scored as well as the best one known.
// That gets it PROBED — the probe is what fetches its published figure —
// and after one round it is measured like everyone else. It is the same
// move Elasticsearch's adaptive replica selection makes for the same
// reason: nudge the score of the node you did not pick, so none is starved.
//
// A round in which nobody has a reading orders nothing: every candidate
// keeps bucket 0.
func assignSpeedRanks(cands []meshCandidate, speeds map[string]PeerSpeed) {
	if len(cands) == 0 || len(speeds) == 0 {
		return
	}
	const (
		unknown = iota
		measured
		bounded
	)
	kind := make([]int, len(cands))
	buckets := make([]int, len(cands))
	haveMeasured, slowestMeasured, bestMeasured := false, 0, 0
	for i, c := range cands {
		s := speeds[c.deviceID]
		t := s.Turn
		if t == nil {
			continue
		}
		congestion := float64(s.CapacityUsed + 1)
		switch {
		case t.TurnSeconds > 0:
			kind[i], buckets[i] = measured, speedBucketOf(congestion*t.TurnSeconds)
			if !haveMeasured || buckets[i] > slowestMeasured {
				slowestMeasured = buckets[i]
			}
			if !haveMeasured || buckets[i] < bestMeasured {
				bestMeasured = buckets[i]
			}
			haveMeasured = true
		case t.TurnFloorSeconds > 0:
			kind[i], buckets[i] = bounded, speedBucketOf(congestion*t.TurnFloorSeconds)
		}
	}
	best, haveBest := bestMeasured, haveMeasured
	for i := range cands {
		if kind[i] != bounded {
			continue
		}
		if haveMeasured && buckets[i] <= slowestMeasured {
			buckets[i] = slowestMeasured + 1
		}
		if !haveMeasured && (!haveBest || buckets[i] < best) {
			best, haveBest = buckets[i], true
		}
	}
	if !haveBest {
		return
	}
	for i := range cands {
		if kind[i] == unknown {
			cands[i].speedBucket = best
			continue
		}
		cands[i].speedBucket = buckets[i]
	}
}

// speedBucketOf buckets a slowness figure into speedBucketRatio bands.
// Lower is faster. Non-positive input reads as "no information" and
// lands in the same bucket as everything else unknown.
func speedBucketOf(slowness float64) int {
	if slowness <= 0 {
		return 0
	}
	return int(math.Floor(math.Log(slowness) / math.Log(speedBucketRatio)))
}

// variantMeetsSizeFloor reports whether the named variant of m is at or
// above the operator's routing floor.
//
// A variant the manifest does not carry fails closed, the same direction
// hostfit.SizeRank fails on an unannotated one: a floor that admits what
// it cannot classify is not a floor.
func variantMeetsSizeFloor(m catalog.Manifest, variantID, floor string) bool {
	if floor == "" {
		return true
	}
	for _, v := range m.Variants {
		if v.VariantID == variantID {
			return hostfit.SizeRank(hostfit.VariantSize(v)) >= hostfit.SizeRank(floor)
		}
	}
	return false
}
