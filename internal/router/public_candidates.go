package router

import (
	"sort"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	protocatalog "github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Public Share consumer-side candidate partitioning (waired#827,
// public share spec §4.2).
//
// A public candidate is a foreign peer the control plane injected into
// the signed network map under a Public Share grant — PeerView.Grant is
// non-nil with Role "provider". The CP has already folded the
// provider's public_max_clients into InferenceState.Capacity (§7.1), so
// admission, sticky affinity, probe-then-commit and the 503 chain need
// no changes here: a public peer is an ordinary mesh candidate that
// carries an extra admission gate and an extra display rule.

// PublicMode is the consumer's Public Share routing posture.
//
// This is deliberately a router-local enum rather than the stored
// strings from internal/agentconfig: the router must not import the
// settings package (nor internal/management, which owns the warning
// version), and an enum turns a drift between the two vocabularies
// into one obviously-wrong switch arm instead of a silent string
// mismatch. The zero value is Off, so every "forgot to wire it" path
// fails closed.
type PublicMode int

const (
	// PublicModeOff admits no public candidates. Zero value.
	PublicModeOff PublicMode = iota
	// PublicModeAuto admits a public candidate only when its advertised
	// quality tier strictly beats the best tier among the consumer's own
	// online nodes.
	PublicModeAuto
	// PublicModeExplicit admits public candidates without the tier
	// comparison. The min-tier floor and the class toggles still apply.
	PublicModeExplicit
)

func (m PublicMode) String() string {
	switch m {
	case PublicModeAuto:
		return "auto"
	case PublicModeExplicit:
		return "explicit"
	default:
		return "off"
	}
}

// PublicPolicy is the already-resolved consumer-side Public Share
// posture for one Select. The caller (cmd/waired-agent) collapses the
// stored settings plus the consent record's warning version into Mode
// before handing it here — see agentconfig.PublicUse.EffectiveMode —
// so the router carries no dependency on how any of that is persisted.
//
// The zero value admits nothing.
type PublicPolicy struct {
	Mode PublicMode
	// Consented reports whether a consent record for the CURRENT
	// warning text exists. Tracked separately from Mode because the
	// nudge must distinguish "never consented" (nudgeable) from
	// "consented and deliberately switched off" (never nudge).
	Consented bool
	// MinQualityTier is the floor on a public node's advertised tier.
	// 0 = no floor.
	MinQualityTier int
	// Main and Sub gate the Claude traffic classes independently.
	Main, Sub bool
}

// PublicNudge is the payload of the one-shot pre-consent hint (spec
// §4.2): enabling Public Share MIGHT give access to better nodes. It
// deliberately carries no tier and names no node — a pre-consent agent
// holds no grants, so no public node is in its map and none is
// observable. See waired/docs/decisions.md (20260720).
//
// Primitive fields only: the router does not import
// internal/observability, matching the narrow-interface convention the
// other recorder seams follow.
type PublicNudge struct {
	// ModelID is the model whose request found nothing to run on.
	ModelID string
	// Reason is a stable tag for why own capacity came up short:
	// "no_candidate" or "all_overloaded".
	Reason string
}

const (
	// NudgeReasonNoCandidate means no own node advertises the model.
	NudgeReasonNoCandidate = "no_candidate"
	// NudgeReasonAllOverloaded means own nodes have the model but every
	// one of them is at capacity.
	NudgeReasonAllOverloaded = "all_overloaded"
)

// publicGate is the resolved public-candidate admission decision for
// one Select. The zero value admits nothing.
type publicGate struct {
	// admit is the policy-level verdict: mode is not off and this
	// request's class is enabled. False short-circuits everything.
	admit bool
	// auto requires a candidate to strictly beat beat.
	auto bool
	// minTier is PublicPolicy.MinQualityTier.
	minTier int
	// beat is the best tier among the consumer's own online nodes.
	// Only meaningful when auto.
	beat int
	// beatComputed guards the lazy fill of beat.
	beatComputed bool
}

// admits reports whether a public peer advertising tier may enter the
// candidate set. Tier 0 means "no tier information" (the peer serves
// nothing this catalog knows) and is excluded by any floor, and can
// never beat an own tier, so it only survives explicit mode with no
// floor — matching proto/catalog.BestTier's documented contract.
func (g *publicGate) admits(tier int) bool {
	if !g.admit {
		return false
	}
	if g.minTier > 0 && tier < g.minTier {
		return false
	}
	if g.auto && tier <= g.beat {
		return false
	}
	return true
}

// publicGateFor resolves the policy and the request class into a gate.
// The own-best-tier comparison input is filled lazily by the caller
// (see ensureBeat) so the overwhelmingly common "no public peers in the
// snapshot" path never pays for the scan.
func (s *Selector) publicGateFor(class string) publicGate {
	if s.in.PublicPolicyFn == nil {
		return publicGate{}
	}
	p := s.in.PublicPolicyFn()
	if p.Mode == PublicModeOff {
		return publicGate{}
	}
	if !classAllowsPublic(class, p) {
		return publicGate{}
	}
	return publicGate{
		admit:   true,
		auto:    p.Mode == PublicModeAuto,
		minTier: p.MinQualityTier,
	}
}

// classAllowsPublic applies the per-class toggles. An empty class —
// general non-Claude inference via `waired infer` or the LocalAPI — is
// admitted when EITHER toggle is on.
//
// The existing ExcludeMain/ExcludeSub filter in buildMeshCandidates
// leaves an empty class unfiltered, but that is the wrong default here:
// these toggles express "am I willing to send prompts to a stranger's
// machine", so both-off must mean no traffic of any kind. Admitting on
// either-on rather than requiring both keeps a user who disabled only
// sub-agent traffic from silently losing general inference.
func classAllowsPublic(class string, p PublicPolicy) bool {
	switch class {
	case state.ClaudeClassMain:
		return p.Main
	case state.ClaudeClassSub:
		return p.Sub
	default:
		return p.Main || p.Sub
	}
}

// ensureBeat fills the auto-mode comparison input on first use: the
// best quality tier across everything the consumer already owns.
//
// Sources, in the order they are unioned:
//   - the local engine, via the ready entries in Inputs.LocalState. This
//     is authoritative even when the aggregator's view of self is nil or
//     lagging behind a just-finished pull.
//   - Snapshot.Self, when it carries a reachable InferenceState.
//   - every own-network peer (Grant == nil) that is reachable and fresh.
//
// Snapshot.Reachable is NOT consulted: it is a documented peers-only OR
// aggregate (see inferencemesh.Snapshot), so it says nothing about
// which nodes to include here.
func (s *Selector) ensureBeat(g *publicGate, snap inferencemesh.Snapshot) {
	if g.beatComputed {
		return
	}
	g.beatComputed = true
	g.beat = s.ownBestTier(snap)
}

// ownBestTier is ensureBeat's body, exposed separately so the nudge path
// can ask the same question without a gate.
func (s *Selector) ownBestTier(snap inferencemesh.Snapshot) int {
	best := s.localBestTier()
	if st := snap.Self.InferenceState; st != nil && st.Reachable {
		if t := s.peerTier(st.Type, st.Models); t > best {
			best = t
		}
	}
	for i := range snap.Peers {
		p := &snap.Peers[i]
		if p.Grant != nil || p.InferenceState == nil || !p.InferenceState.Reachable || p.Stale {
			continue
		}
		if t := s.peerTier(p.InferenceState.Type, p.InferenceState.Models); t > best {
			best = t
		}
	}
	return best
}

// localBestTier is the highest quality tier among the variants this
// device has pulled and marked ready.
func (s *Selector) localBestTier() int {
	best := 0
	for modelID, ms := range s.in.LocalState.Models {
		if ms.State != catalog.ModelStateReady {
			continue
		}
		m, ok := catalog.LookupByAlias(modelID, s.in.Manifests)
		if !ok {
			continue
		}
		v, ok := findVariant(m, ms.VariantID)
		if !ok {
			continue
		}
		if v.QualityTier > best {
			best = v.QualityTier
		}
	}
	return best
}

// peerTier resolves the best quality tier an inference endpoint
// advertises, using the same catalog SSoT the control plane's Public
// Share matchmaking uses (proto/catalog.BestTier). BestTierIn is
// preferred over the argument-less BestTier so tier resolution runs
// over the Selector's own manifest set rather than the embedded bundled
// catalog.
//
// The engine-kind default mirrors buildMeshCandidates: an empty Type
// means ollama. Without it every legacy peer would resolve to tier 0.
func (s *Selector) peerTier(engineType string, models []string) int {
	if engineType == "" {
		engineType = catalog.RuntimeOllama
	}
	return protocatalog.BestTierIn(s.in.Manifests, engineType, models)
}

// publicDisplayID is the only identifier that may be shown for a public
// peer: the grant's pseudonym for the peer's owner account. Real
// foreign device identifiers never cross into a header, an event, a log
// line or a CLI surface (spec §8.5).
//
// Returns ok=false when the pseudonym is missing, which fails the peer
// closed: the control plane skips injecting a peer whose pseudonym row
// is absent, so seeing one here means something is wrong, and routing to
// a peer we cannot name safely is worse than not routing to it.
func publicDisplayID(g *signer.PeerGrant) (string, bool) {
	if g == nil || g.Pseudonym == "" {
		return "", false
	}
	return g.Pseudonym, true
}

// snapshotHasPublicProvider reports whether the map currently carries
// any peer injected under a Public Share provider grant — i.e. whether
// this agent holds a usable grant at all. Drives the acquirer demand
// signal: policy wants public, but there is nothing to route to.
func snapshotHasPublicProvider(snap inferencemesh.Snapshot) bool {
	for i := range snap.Peers {
		if isPublicProvider(&snap.Peers[i]) {
			return true
		}
	}
	return false
}

// isPublicProvider reports whether a peer entry is a Public Share
// provider injected for this device. Grant.Role is authoritative: the
// same foreign device can appear as a consumer (a guest using OUR
// engine), which must never become a routing candidate.
func isPublicProvider(p *inferencemesh.PeerView) bool {
	return p.Grant != nil && p.Grant.Role == peerGrantRoleProvider
}

// peerGrantRoleProvider mirrors the control plane's PeerGrant.Role value
// for a peer that serves inference to this device (spec §7.1).
const peerGrantRoleProvider = "provider"

// partitionOwnFirst re-asserts the own > public ordering after the
// sticky and pinned-peer hoists, both of which move a candidate to
// index 0 keyed on deviceID alone. Stable, so the relative order each
// hoist produced survives inside its own partition.
func partitionOwnFirst(cands []meshCandidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		return !cands[i].public && cands[j].public
	})
}

// publicCameUpShort runs on the two paths where the consumer's own
// nodes could not serve a request: no candidate at all, and every
// candidate at capacity.
//
// It drives the two Public Share side signals, which are deliberately
// mutually exclusive by construction:
//
//   - the acquirer demand signal, when policy WOULD admit a public
//     candidate but the map carries no provider grant to route to. A
//     grant takes an acquire round trip plus map propagation, so
//     without this the first request after consent waits out the
//     acquirer's periodic tick (spec §4.3 cold start).
//   - the pre-consent nudge, when no consent has been recorded. Consent
//     is a precondition for holding a grant, so an unconsented agent can
//     never reach the demand branch.
func (s *Selector) publicCameUpShort(snap inferencemesh.Snapshot, gate publicGate, modelID, reason string) {
	policy := s.publicPolicy()
	if gate.admit && !snapshotHasPublicProvider(snap) {
		s.notifyPublicGrantDemand()
	}
	s.notifyPublicNudge(policy, modelID, reason)
}

// notifyPublicGrantDemand tells the background grant acquirer that a
// request wanted a public candidate and found no grant to use. Fire and
// forget: the callback is a non-blocking send onto a coalescing
// buffered channel, so the routing hot path never waits on the acquirer.
func (s *Selector) notifyPublicGrantDemand() {
	if s.in.OnPublicGrantDemand != nil {
		s.in.OnPublicGrantDemand()
	}
}

// notifyPublicNudge emits the one-shot pre-consent hint. The receiver
// owns once-ness; the Selector emits on every qualifying request and
// deliberately keeps no state of its own.
func (s *Selector) notifyPublicNudge(policy PublicPolicy, modelID, reason string) {
	if s.in.OnPublicNudge == nil || policy.Consented {
		return
	}
	s.in.OnPublicNudge(PublicNudge{ModelID: modelID, Reason: reason})
}

// publicPolicy reads the resolved policy, or the zero (off, unconsented)
// value when nothing is wired.
func (s *Selector) publicPolicy() PublicPolicy {
	if s.in.PublicPolicyFn == nil {
		return PublicPolicy{}
	}
	return s.in.PublicPolicyFn()
}
