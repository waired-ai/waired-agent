package router

import (
	"sort"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	protocatalog "github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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
	// comparison. The size floor and the class toggles still apply.
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
	// MinModelSize is the floor on the size of the model a public node
	// advertises — hostfit.ModelSizeSmall / Medium / Large. Empty = no
	// floor.
	//
	// A size and not a tier because the two orderings cross: glm-4.5-air
	// is tier 75 and large, qwen3.6-35b-a3b is tier 90 and medium, so no
	// numeric boundary draws the size line (#537). The tier is still the
	// ranking everything sorts by — it just is not a thing a person
	// types.
	MinModelSize string
	// LegacyMinQualityTier is a floor stored before #537, kept readable
	// so an operator who set one does not silently lose it. Resolved to a
	// size in publicGateFor, which is where the catalog is in hand; the
	// stored file keeps it until the next settings write clears it.
	LegacyMinQualityTier int
	// Main and Sub gate the Claude traffic classes independently.
	Main, Sub bool
}

// PublicNudge is the payload of the one-shot pre-consent hint (spec
// §4.2): enabling Public Share MIGHT give access to better nodes. It
// deliberately carries no tier and names no node — a pre-consent agent
// holds no grants, so no public node is in its map and none is
// observable. See waired/docs/decisions/ (20260720).
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

// publicDenial names WHICH consumer-side switch refused, for the one
// caller that has to say so (waired-agent#1201). It annotates: admit
// still decides, and the zero value claims nothing.
//
// Recorded on the gate rather than re-read from the policy where the
// message is built, because the policy is republished under an atomic
// pointer while a selection runs (publicUseController). A
// `waired public use --off` landing between the attempt and its error
// would otherwise explain the refusal with a posture that did not cause
// it — the same hazard effectivePref avoids by reading the routing
// preference once and handing it down. peerOnlyMissNote's successor also
// has no request class in hand, and the class is what decides which
// toggle to name.
type publicDenial int

const (
	// publicDenialUnrecorded is the zero value: no cause was recorded, so
	// the refusal names no switch. A gate that did not say why must not
	// invent somewhere to send the operator.
	publicDenialUnrecorded publicDenial = iota
	// publicDenialNone means the gate admits. Nothing refused.
	publicDenialNone
	// publicDenialNotConsented: the first-use security and privacy
	// warning has never been accepted on this computer.
	publicDenialNotConsented
	// publicDenialModeOff: consented once, and then switched off.
	publicDenialModeOff
	// publicDenialMainOff, publicDenialSubOff and publicDenialBothOff are
	// the per-class toggles (PublicPolicy.Main / .Sub). Both-off is the
	// answer for an empty class, which classAllowsPublic admits on either.
	publicDenialMainOff
	publicDenialSubOff
	publicDenialBothOff
)

// publicAdmit is admits' verdict.
//
// A verdict rather than a bool because the size floor is the one
// rejection the refusal has to be able to name: "somebody is lending a
// machine and it is below the smallest model you accept" is a different
// fact from "nobody is lending" (waired-agent#1201).
type publicAdmit int

const (
	// publicAdmitNo is the zero value and fails closed.
	publicAdmitNo publicAdmit = iota
	// publicAdmitBelowMinSize: the peer's model is smaller than
	// PublicPolicy.MinModelSize.
	publicAdmitBelowMinSize
	// publicAdmitNotBetter: auto mode, and the peer does not beat this
	// host's own best tier.
	publicAdmitNotBetter
	// publicAdmitYes admits the peer.
	publicAdmitYes
)

// publicGate is the resolved public-candidate admission decision for
// one Select. The zero value admits nothing.
type publicGate struct {
	// admit is the policy-level verdict: mode is not off and this
	// request's class is enabled. False short-circuits everything.
	admit bool
	// denial says which switch made admit false, so a refused turn can
	// name it (waired-agent#1201). Annotation only — nothing on the
	// admission path reads it.
	denial publicDenial
	// auto requires a candidate to strictly beat beat.
	auto bool
	// minSize is the resolved size floor — PublicPolicy.MinModelSize, or
	// the migrated legacy tier floor. Empty = no floor.
	minSize string
	// beat is the best tier among the consumer's own online nodes.
	// Only meaningful when auto.
	beat int
	// beatComputed guards the lazy fill of beat.
	beatComputed bool
}

// admits reports whether a public peer may enter the candidate set,
// given the tier and the size class it advertises.
//
// The two are asked different questions, and that split is the shape of
// #537. The SIZE carries the operator's floor, because a size class is
// the only thing that means the same on a machine you cannot see. The
// TIER carries the auto-mode comparison, which is an ordering nobody
// types or reads — exactly the internal use #537 kept the number for.
//
// Both zero values are "no information" and both fail closed. Tier 0
// means the peer serves nothing this catalog knows, so it can never beat
// an own tier; an empty size ranks below every real class, so any floor
// excludes it. Either way the peer only survives explicit mode with no
// floor — matching proto/catalog.BestTier's documented contract.
func (g *publicGate) admits(tier int, size string) publicAdmit {
	if !g.admit {
		return publicAdmitNo
	}
	if g.minSize != "" && hostfit.SizeRank(size) < hostfit.SizeRank(g.minSize) {
		return publicAdmitBelowMinSize
	}
	if g.auto && tier <= g.beat {
		return publicAdmitNotBetter
	}
	return publicAdmitYes
}

// publicOnly reports whether this selection may use ONLY public machines.
//
// Set for one request by the Claude surface when the operator picked the
// "Waired public share" /model entry (waired-agent#901). It narrows the
// candidate set; it does not widen it. The standing Public Share posture still
// decides what is admissible — mode off still admits nothing, and mode auto
// still requires a public candidate to beat the consumer's own best tier
// (owner ruling 2026-08-20: the entry respects the posture rather than
// overriding it). So this can only ever remove own-network candidates from a
// set the policy had already allowed public ones into.
func (s *Selector) publicOnly() bool { return s.in.PublicOnly }

// publicShareDeclineReason is the sentence a "Waired public share" turn is
// refused with: which of the operator's own Public Share settings declined,
// or which fact about the world did.
//
// Empty when the attempt learned nothing that would be true to say. The
// caller then leaves the reason off rather than guessing one.
//
// Order is the most specific thing the operator can act on first, and the
// SETTINGS arms come before the reachability arm. That order is the defect
// this function was rewritten for (waired-agent#1201): with the posture off
// the grant acquirer releases every held grant, so no provider is in the map,
// and the reachability arm reported "nobody is lending" about a refusal the
// operator's own switch had caused. Reachability still comes before the auto
// comparison — with nobody lending, no comparison ran, so "none was better"
// would be equally untrue.
//
// short carries the snapshot, the gate and the floor count from the attempt
// that found nothing, which is what makes any of this available here.
func publicShareDeclineReason(short publicShortfall) string {
	if !short.hit {
		return ""
	}
	switch short.gate.denial {
	case publicDenialNotConsented:
		return "Public Share has not been turned on here — its security and privacy warning has not been accepted; accept it with `waired public use --auto`"
	case publicDenialModeOff:
		return "this computer is set not to use other people's public machines; turn it on with `waired public use --auto` or `--explicit`"
	case publicDenialMainOff:
		return "Public Share is turned off for main-agent turns; turn it on with `waired public use --main on`"
	case publicDenialSubOff:
		return "Public Share is turned off for sub-agent turns; turn it on with `waired public use --sub on`"
	case publicDenialBothOff:
		return "Public Share is turned off for both main-agent and sub-agent turns; turn them on with `waired public use --main on --sub on`"
	case publicDenialUnrecorded:
		// Nothing wired the policy in. That is not a fact about the
		// operator's settings, so name no switch.
		return ""
	}
	// A peer counted here was in the map, so the reachability arm below is
	// false by construction; and admits tests the size floor before the tier
	// comparison, so a peer dropped for size never reached that one.
	if short.belowPublicFloor > 0 {
		return "no public machine runs " + ModelSizePhrase(short.gate.minSize) +
			", which is the smallest you accept; change it with `waired public use --min-model-size`"
	}
	if !snapshotHasPublicProvider(short.snap) {
		return "no public machine is reachable right now"
	}
	if short.gate.auto {
		return "Public Share is set to use another machine only when it beats this one, and none does; use one anyway with `waired public use --explicit`"
	}
	return "no public machine can serve this request"
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
	// Consent BEFORE mode. agentconfig.PublicUse.EffectiveMode already
	// collapses "never consented" into "off" before the policy reaches the
	// router, so testing mode first would leave the unconsented case
	// permanently unnameable.
	//
	// Known imprecision, recorded rather than fixed: a public_use.json the
	// daemon could not read also publishes the zero policy, so this arm can
	// say "not consented" about an unreadable file. The daemon logs the real
	// cause, and the switch to look at is the same one either way.
	if !p.Consented {
		return publicGate{denial: publicDenialNotConsented}
	}
	if p.Mode == PublicModeOff {
		return publicGate{denial: publicDenialModeOff}
	}
	if d := classDenial(class, p); d != publicDenialNone {
		return publicGate{denial: d}
	}
	return publicGate{
		admit:   true,
		denial:  publicDenialNone,
		auto:    p.Mode == PublicModeAuto,
		minSize: s.resolveMinModelSize(p),
	}
}

// classDenial names which per-class toggle refused, or publicDenialNone when
// the class is allowed. classAllowsPublic keeps the single definition of the
// rule; this only says which switch a refusal should send the operator to.
func classDenial(class string, p PublicPolicy) publicDenial {
	if classAllowsPublic(class, p) {
		return publicDenialNone
	}
	switch class {
	case state.ClaudeClassMain:
		return publicDenialMainOff
	case state.ClaudeClassSub:
		return publicDenialSubOff
	default:
		// An empty class is admitted on EITHER toggle, so reaching here
		// means both are off.
		return publicDenialBothOff
	}
}

// resolveMinModelSize is the size floor to enforce, migrating a floor
// stored as a tier before #537.
//
// The migration takes the LEAST restrictive class among the models that
// tier floor admitted. A tier floor and a size floor do not draw the
// same line — that is why the field changed — so any mapping is
// approximate, and this one is approximate in the direction that does
// not silently take machines away from somebody who is not watching.
//
// A floor above everything in the catalog admitted nothing, so it maps
// to the most restrictive class rather than to "no floor": reading an
// exclude-everything setting as an allow-everything one is the one
// outcome the operator certainly did not ask for.
func (s *Selector) resolveMinModelSize(p PublicPolicy) string {
	if p.MinModelSize != "" {
		return p.MinModelSize
	}
	if p.LegacyMinQualityTier <= 0 {
		return ""
	}
	best := ""
	for _, m := range s.in.Manifests {
		for _, v := range m.Variants {
			if v.QualityTier < p.LegacyMinQualityTier {
				continue
			}
			if sz := hostfit.VariantSize(v); sz != "" &&
				(best == "" || hostfit.SizeRank(sz) < hostfit.SizeRank(best)) {
				best = sz
			}
		}
	}
	if best == "" {
		return hostfit.ModelSizeLarge
	}
	return best
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
//
// A device whose local inference is off serves none of them, so the bar
// a public candidate has to beat is 0 — reading weights on disk as
// serving capacity would let the toggle keep a public peer out of a
// request this device cannot answer itself (waired-agent#829).
func (s *Selector) localBestTier() int {
	best := 0
	if s.in.LocalServingOff {
		return best
	}
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

// peerSize is peerTier's sibling: the largest size class an inference
// endpoint advertises, resolved through the same catalog and the same
// engine-kind default (an empty Type means ollama, or every legacy peer
// would resolve to "unknown" and be excluded by any floor).
//
// The VARIANT decides, not the family — this is what the peer is
// running, and a model shipping both a light and a heavy build would
// otherwise be reported at the light one.
func (s *Selector) peerSize(engineType string, models []string) string {
	if engineType == "" {
		engineType = catalog.RuntimeOllama
	}
	return hostfit.BestSizeIn(s.in.Manifests, engineType, models)
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
//
// publicShortfall remembers that the mesh could not supply a candidate,
// so SelectK can decide — once, at its exit — whether the request truly
// went unserved. Recording is not the same as emitting: several routing
// modes consult the mesh first and still fall through to a healthy local
// engine.
type publicShortfall struct {
	hit    bool
	snap   inferencemesh.Snapshot
	gate   publicGate
	reason string

	// belowFloor counts peers dropped by the operator's minimum model
	// class (waired-agent#1128). It rides here because it is the same
	// kind of fact — something the mesh attempt learned that the terminal
	// error has to be able to say — and because the alternative is
	// mutating the Selector, which several requests share.
	belowFloor int
	// belowPublicFloor counts peers dropped by the CONSUMER's Public Share
	// floor (PublicPolicy.MinModelSize) — a different setting, changed with
	// a different command, and deliberately kept out of belowFloor so it
	// never triggers the SizeFloorError wrapper, which names the operator's
	// `waired worker set --min-model-size` (waired-agent#1201).
	belowPublicFloor int
}

// record keeps the FIRST shortfall seen. There is at most one mesh
// attempt per SelectK today; first-wins is the conservative choice if
// that ever changes, since the earliest reason is the most specific.
func (p *publicShortfall) record(snap inferencemesh.Snapshot, gate publicGate, reason string) {
	if p == nil || p.hit {
		return
	}
	p.hit, p.snap, p.gate, p.reason = true, snap, gate, reason
}

// emitPublicShortfall fires the two Public Share side signals, and is
// called only from SelectK's failure exit — the request reached no
// engine at all.
//
// The two are mutually exclusive by construction:
//
//   - the acquirer demand wake, when policy WOULD admit a public
//     candidate but the map carries no provider grant to route to. A
//     grant costs an acquire round trip plus map propagation, so without
//     this the first request after consent waits out the acquirer's
//     periodic tick (spec §4.3 cold start).
//   - the pre-consent nudge, when no consent has been recorded. Consent
//     is a precondition for holding a grant, so an unconsented agent can
//     never reach the demand branch.
func (s *Selector) emitPublicShortfall(short publicShortfall, modelID string) {
	if !short.hit {
		return
	}
	if short.gate.admit && !snapshotHasPublicProvider(short.snap) {
		s.notifyPublicGrantDemand()
	}
	s.notifyPublicNudge(s.publicPolicy(), modelID, short.reason)
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

// notifyPublicGrantUsed reports that a request was just committed to the
// provider behind grantID, so the background acquirer treats the grant as
// carrying traffic (waired#898). Fire and forget, nil-safe: the
// production callback records a timestamp under a small lock and never
// blocks the routing hot path.
func (s *Selector) notifyPublicGrantUsed(grantID string) {
	if s.in.OnPublicGrantUsed != nil {
		s.in.OnPublicGrantUsed(grantID)
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
