package inferencemesh

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// This file is the one answer to "what is this peer running, and why is
// it or is it not serving" (waired#1064). Before it there were three
// implementations of the second half — the tray's peerIsServing, the
// management API's resolvePinStatus and `waired peers list`'s
// WORKER-CAPABLE column — and they disagreed: two flattened four
// distinct causes into a single "unavailable" while the third kept them
// apart. Different surfaces described one machine differently.

// Viewer-side conditions. Everything else PeerCondition can return is a
// signer.SubsystemState* value reported by the peer itself; these three
// are the answers only the viewer holds, because they are facts about
// its own view rather than about the machine.
const (
	// ConditionStale is "this peer's reporting chain is broken" — the
	// map frame or the peer's own last_check aged out. Nothing it last
	// said can be presented as current, INCLUDING which model it runs.
	ConditionStale = "stale"
	// ConditionUnreachable is "the peer's engine did not answer its
	// probe", with no more specific reason offered.
	ConditionUnreachable = "unreachable"
	// ConditionUnavailable is "not serving, and the peer gave no
	// reason" — an agent that predates subsystem_state. It is the word
	// every surface already used for all of these before there was
	// anything better to say.
	ConditionUnavailable = "unavailable"
)

// PublicPeerLabel is what a prose surface writes where a peer's display
// identifier would go, when a public machine carries no pseudonym to
// show. "public machine" is the wording `waired public` already uses for
// someone else's computer ("Public machines are other people's
// computers", cmd/waired/public.go).
//
// Not for an identifier column — a column has "-" for "nothing to show",
// and a phrase in an ID column reads as an id.
const PublicPeerLabel = "public machine"

// publicPeerLabelDigestLen is how much of the grant digest a label
// carries. Four hex characters against a host that holds one grant at a
// time (publicGrantWant = 1) is enough that two rows sharing a suffix is
// a curiosity rather than the ordinary case, and short enough that the
// suffix does not read as an identifier in its own right.
const publicPeerLabelDigestLen = 4

// PublicPeerLabelFor is PublicPeerLabel with the grant behind this peer
// named, for the surfaces that write it for more than one machine at once
// — `waired status`'s peer rows, and the two "that name is ambiguous"
// messages, which used to answer "public machine, public machine" and
// leave the operator no way to tell which was which.
//
// The suffix is a DIGEST OF THE GRANT, and both halves of that matter.
// The grant is an arrangement the control plane made with THIS host; its
// id is already in this host's own logs as grant_id, and nothing about
// the stranger's machine goes into it — which is the whole of what public
// share spec §8.5 forbids reaching a surface. And it is a digest rather
// than a prefix because a grant id is structured, so its first characters
// distinguish nothing.
//
// Stable for the life of the grant, and different after a re-issue. That
// is the honest lifetime: two rows carrying the same suffix came in under
// the same grant, which is the only sameness this host can truthfully
// claim about someone else's computer.
//
// An empty grant id yields the bare label. A label that says "grant" and
// then nothing is worse than one that never raises the question.
func PublicPeerLabelFor(grantID string) string {
	if grantID == "" {
		return PublicPeerLabel
	}
	sum := sha256.Sum256([]byte(grantID))
	return PublicPeerLabel + " (grant " + hex.EncodeToString(sum[:])[:publicPeerLabelDigestLen] + ")"
}

// PeerDisplayLabel is what a prose surface writes for this peer: its
// display identifier when it has one, the public-machine label when it
// does not.
//
// It is the shape three call sites had open-coded as "ask PeerDisplayID,
// and substitute a phrase when it answers nothing" — which is how the
// grant went unnamed on all of them at once.
func PeerDisplayLabel(p PeerView) string {
	if id, ok := PeerDisplayID(p); ok {
		return id
	}
	grantID := ""
	if p.Grant != nil {
		grantID = p.Grant.ID
	}
	return PublicPeerLabelFor(grantID)
}

// PeerDisplayID is the identifier a surface may show for this peer, and
// whether there is one at all.
//
// A peer injected under a Public Share grant is someone else's machine:
// only the grant pseudonym for its owner account may be displayed, never
// the real device identifier (public share spec §8.5, as stated on
// internal/gateway/probe.go's peerDisplayID). Own-network peers carry no
// grant and are named by DeviceID as they always were.
//
// ok=false only for a grant peer with no pseudonym. Falling back to the
// DeviceID there would be the leak itself, so this reports "nothing to
// show" and lets the surface decide how to say so — the same choice
// internal/router's publicDisplayID makes for routing. The control plane
// skips injecting a grant peer whose pseudonym row is missing, so this
// is a second lock on a door that should already be shut.
func PeerDisplayID(p PeerView) (string, bool) {
	if p.Grant == nil {
		return p.DeviceID, true
	}
	if p.Grant.Pseudonym == "" {
		return "", false
	}
	return p.Grant.Pseudonym, true
}

// PeerDisplayName is the human-readable name a prose surface may show
// for this peer, and whether there is one at all.
//
// One of your own machines is named by DeviceName, falling back to
// DeviceID for a peer that reported none — the same order aggregator's
// peerSortName uses, so a row reads under the name it sorts by.
//
// A grant peer is someone else's machine, so only the grant pseudonym
// may be shown and DeviceName is never read. The control plane
// substitutes the pseudonym into DeviceName at injection time
// (cmd/waired/peers.go), but that is a claim about another process,
// while types.go states the rule absolutely — so this reads the grant
// directly, the same second lock on the same door PeerDisplayID is.
//
// ok=false only for a grant peer with no pseudonym: naming it any other
// way would be the leak itself.
func PeerDisplayName(p PeerView) (string, bool) {
	if p.Grant != nil {
		return PeerDisplayID(p)
	}
	if p.DeviceName != "" {
		return p.DeviceName, true
	}
	return p.DeviceID, true
}

// PeerModel is the model this peer is committed to serving.
//
// ActiveModel is the catalog model_id, the one namespace every host
// agrees on; Models carries the engine's own tag, which spells the same
// weights differently per engine and therefore per OS. Preferring the
// first is what makes one model read as one model across a mixed fleet.
// Falling back to the second is what keeps a peer running an agent that
// predates the field rendering exactly as it did before.
//
// "" means the peer names no model at all.
func PeerModel(p PeerView) string {
	if p.InferenceState == nil {
		return ""
	}
	if p.InferenceState.ActiveModel != "" {
		return p.InferenceState.ActiveModel
	}
	if len(p.InferenceState.Models) > 0 {
		return p.InferenceState.Models[0]
	}
	return ""
}

// PeerServing reports whether requests routed to this peer can be served
// right now: a live report, a reachable engine, and at least one
// advertised engine tag.
//
// It stays keyed on Models rather than on the richer condition below,
// because Models is what a request is actually matched against
// (buildMeshCandidates). A peer that named a model but withdrew its tag
// is explained by PeerCondition; it is still not routable.
func PeerServing(p PeerView) bool {
	if p.Stale {
		return false
	}
	if p.InferenceState == nil || !p.InferenceState.Reachable {
		return false
	}
	return len(p.InferenceState.Models) > 0
}

// PeerCondition is why this peer is or is not serving, as one word.
//
// Returns a signer.SubsystemState* value when the peer explained itself
// and the viewer has no better answer, or one of the Condition* values
// above when it did not. Never empty.
//
// The order matters. The viewer's own facts (a broken reporting chain)
// win over anything the peer said, because a stale claim is not evidence
// about now. Below that the peer's own reason wins over the viewer's
// coarser reading — "stopped" or "pull failed" is the thing an operator
// can act on, where "unreachable" only says the probe failed.
func PeerCondition(p PeerView) string {
	if p.InferenceState == nil {
		// Never reported an engine at all. Distinct from a reported
		// no_engine only in provenance, and the answer is the same.
		return signer.SubsystemStateNoEngine
	}
	if p.Stale {
		return ConditionStale
	}
	reported := p.InferenceState.SubsystemState
	if PeerServing(p) {
		// Routable. A reported state that disagrees (a mid-switch tick
		// where the catalog row is not ready yet but the engine still
		// serves the old tag) loses to the fact that work sent here is
		// answered — which is the question this word is read to answer.
		return signer.SubsystemStateReady
	}
	if reported != "" && reported != signer.SubsystemStateReady {
		return reported
	}
	if !p.InferenceState.Reachable {
		return ConditionUnreachable
	}
	// Reachable, no advertised tag, and either no reason given or a
	// "ready" that the missing tag contradicts. This is the bare answer
	// every surface gave before waired#1064.
	return ConditionUnavailable
}

// ConditionLabel renders a PeerCondition (or a raw subsystem_state) as
// the short phrase a menu row or a table cell shows.
//
// The wire values are snake_case; these are the words the agent's own
// menu has always shown for the machine you are sitting at, so a device
// reads the same way locally and from a peer. Unmapped values pass
// through rather than being dropped: the set is validated at the control
// plane, so an unknown one means this table is behind the wire, and
// hiding it would hide the reason a node is not serving.
func ConditionLabel(c string) string {
	switch c {
	case signer.SubsystemStateNoEngine:
		return "no engine"
	case signer.SubsystemStateAwaitingModel:
		return "awaiting model"
	case signer.SubsystemStatePullFailed:
		return "pull failed"
	case signer.SubsystemStateEngineFailed:
		return "engine failed"
	// The three viewer-side conditions all mean the same thing to a
	// person — this machine cannot serve and nobody said why — and
	// "unavailable" is already the published word for that: docs-site
	// defines it for the pin row, and waired-agent#729's product
	// contract is written in terms of it. They stay one word here, and
	// keep their separate identities on the wire, where `waired peers
	// list` prints them raw in a diagnostic column.
	//
	// So waired#1064 adds specificity strictly where the peer VOLUNTEERED
	// a reason; where it did not, the row reads exactly as it did before.
	case ConditionStale, ConditionUnreachable, ConditionUnavailable:
		return ConditionUnavailable
	}
	// ready / loading / starting / disabled / degraded / initializing /
	// stopped are already the phrase.
	return c
}

// ConditionHasFreshModel reports whether PeerModel may be shown next to
// this condition. False for the two conditions that mean the viewer has
// no current report: the peer's last-known model is then a claim about
// the past, and rendering it beside a live-looking row states it as the
// present.
func ConditionHasFreshModel(c string) bool {
	return c != ConditionStale
}
