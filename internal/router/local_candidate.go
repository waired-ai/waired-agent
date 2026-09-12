package router

import (
	"fmt"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// LocalNode is one reading of THIS device as a routing candidate: every
// field is the local answer to a question buildMeshCandidates asks a peer.
//
// It exists because the auto arm asked "is local ready?" as a BRANCH and not
// as a candidate, so this device either took the turn without looking at the
// mesh or was left out of it entirely — including while it was serving the
// old model through a switch. The owner's ruling on peer selection is that
// there is no distinction to make: "ローカルと peer は区別しない" (2026-08-29,
// waired-agent#1128/#1129), which is a statement about one ordered list.
// waired-agent#1302 is that ruling reaching the arm it was made for.
//
// One value struct behind one closure rather than a field per fact: the tag,
// the variant, the measured rate and the occupancy all have to describe the
// SAME model, and reading them independently lets a model switch land between
// two of them and describe a host that does not exist.
type LocalNode struct {
	// DeviceID is this device's own id, from the mesh snapshot's SelfDeviceID.
	// "" means the device does not know who it is yet, and there is no
	// candidate to build.
	DeviceID string
	// DisplayName is what a person calls this computer. Prose only.
	DisplayName string
	// Serving is "the engine is up AND the weights of the model it is
	// committed to are on disk and ready" — the same question
	// EngineReady() answers for the peer-facing /healthz.
	Serving bool
	// Runtime is catalog.RuntimeOllama or catalog.RuntimeVLLM.
	Runtime string
	// EngineTag is what the engine answers to, and it is the join key
	// against the request's want set — exactly as a peer's advertised tag
	// is. Through a model switch this is still the OLD model: it is the
	// same value narrowPublishedModels is already advertising to peers,
	// which is why a peer could be served by this host while this host
	// refused its own turn.
	EngineTag string
	// ModelID / VariantID are the catalog entry EngineTag resolves to.
	ModelID   string
	VariantID string
	// PendingModelID is the model a switch is on its way to, when one is
	// in flight and differs from ModelID. Prose only — it is what lets the
	// reason line say why the turn is running on something else.
	PendingModelID string
	// ContextWindow is the input-token window the engine is loaded with.
	// 0 = this device declares nothing, the same reading a peer's 0 has,
	// and never "serves nothing".
	ContextWindow int
	// Capacity is how many conversations this host holds warm, and
	// CapacityUsed how many are in use right now — the same pair a peer
	// publishes as capacity_total / capacity_used, read from the same
	// counter that answers its /healthz.
	Capacity     int
	CapacityUsed int
	// Prefill is this host's own published prefill measurement, or nil when
	// it has none. nil is "unmeasured", and the ordering must read it as no
	// information rather than as slow (docs/decisions/20260822/0218).
	Prefill *PrefillRate
}

// localDrop says why this device was not put in the candidate list, for the
// reasons the caller assembles. Only the floor needs to travel: it is the
// operator's own setting and the surfaces name it rather than reporting a
// fault (waired-agent#1128).
type localDrop struct {
	belowFloor bool
}

// buildLocalCandidate turns a LocalNode into a candidate in the same shape,
// and through the same filters in the same order, as buildMeshCandidates
// applies to a peer.
//
// The filters it does NOT apply, each for a reason:
//
//   - the grant gate. This device is never a Public Share provider to
//     itself; local carries public=false, which is what keeps
//     partitionOwnFirst correct.
//   - ExcludeMain / ExcludeSub, and Priority. Both are CP-injected into a
//     PEER's map entry; Snapshot.Self comes from this agent's own push
//     (inferencemesh.Aggregator.UpdateLocal) and never carries them, so
//     they are structurally invisible here. Read as governing the mesh
//     traffic this device would serve, which is what they gate today.
//     Recorded as today's behaviour, not as a ruling.
func (s *Selector) buildLocalCandidate(ln LocalNode, minWindow int, want meshWant) (meshCandidate, bool, localDrop) {
	var drop localDrop
	// The mirror of a peer's "!Reachable || Stale": nothing to offer.
	if ln.DeviceID == "" || !ln.Serving || s.in.LocalServingOff {
		return meshCandidate{}, false, drop
	}
	// "Waired public share" asked for somebody else's computer, and this
	// host is the population that entry exists to leave (waired-agent#901).
	if s.publicOnly() {
		return meshCandidate{}, false, drop
	}
	// The declared-window filter, in the one place that now applies it to
	// both sides (waired#1031). 0 is "declares nothing" and is left alone.
	if minWindow > 0 && ln.ContextWindow > 0 && ln.ContextWindow < minWindow {
		return meshCandidate{}, false, drop
	}
	entries := want.ollama
	if ln.Runtime == catalog.RuntimeVLLM {
		entries = want.vllm
	}
	e, ok := entries[ln.EngineTag]
	if !ok {
		// This device does not serve what the request wants. Identical to
		// the peer rule: a request that NAMED a model finds no candidate
		// here, and one that named none matched against the whole catalog.
		return meshCandidate{}, false, drop
	}
	// The operator's floor, on this device's own engine as much as on a
	// peer's (owner ruling 2026-08-29, waired-agent#1128). The same
	// helper the pre-#1302 local arm used, so the two cannot drift.
	if !variantMeetsSizeFloor(e.manifest, e.variant.VariantID, s.in.MinModelSize) {
		drop.belowFloor = true
		return meshCandidate{}, false, drop
	}
	return meshCandidate{
		local:       true,
		deviceID:    ln.DeviceID,
		displayID:   ln.DeviceID,
		displayName: ln.DisplayName,
		variant:     e.variant,
		manifest:    e.manifest,
		runtime:     ln.Runtime,
		tag:         ln.EngineTag,

		contextWindow: ln.ContextWindow,
		capacity:      ln.Capacity,
		capacityUsed:  ln.CapacityUsed,
		score:         int64(e.variant.ParamCount) * int64(e.variant.QuantizationTier),
		sizeClass:     hostfit.VariantSize(e.variant),

		// public / silent / priority / errorRate / rttMS / inFlight /
		// loadFraction / mapAgeMS are deliberately the zero value. See
		// localRankingNotes for what each one means here.
		rttMS: localRTT(ln),
	}, true, drop
}

// localRTT is the distance key for this device's own entry.
//
// Zero is the honest figure — there is no network leg — and rttBucketMS is
// 25 ms, so on a LAN this device shares a bucket with its peers and the key
// decides nothing. It is also below speed, size, error rate and the grant
// tier, so it only ever breaks a tie among candidates that matched on
// everything above it.
//
// But it is NOT honest on a COLD round, and that case is a starvation loop
// rather than a mis-ordering. With no prefill reading anywhere, the nil rule
// gives every candidate the same best speed bucket
// (docs/decisions/20260822/0218), identical models tie on score, and rtt
// decides — for this device, every time. ParallelProbe then fast-paths a
// local winner at index 0 without probing anybody
// (internal/gateway/probe.go), OnPeerProbe never fires, PrefillWindow never
// learns a peer's rate, and this device wins the next round for the same
// reason. Peer speeds are only ever learned on the request path: the
// measurements are stripped from the served network map
// (proto/signer/inference_state.go).
//
// So a device that has never had anything to compare itself with reports the
// same "unknown" a peer nobody has pinged reports, and the round ties down to
// the deviceID — where the tie-break of shuffleWithinTiers spreads the first
// few rounds and the readings start arriving.
func localRTT(ln LocalNode) uint32 {
	if ln.Prefill == nil {
		return RTTUnknown
	}
	return 0
}

// roundSpeeds is the per-peer speed map for one selection round, with this
// device's own reading folded in under its own device id.
//
// A copy, never a mutation: PrefillWindow.Snapshot() is the live view of
// what this requester has learned about OTHER computers, read by more than
// this call, and self is not a peer of itself.
func roundSpeeds(peers map[string]PeerSpeed, ln LocalNode) map[string]PeerSpeed {
	if ln.DeviceID == "" {
		return peers
	}
	out := make(map[string]PeerSpeed, len(peers)+1)
	for k, v := range peers {
		out[k] = v
	}
	rungs := map[int]PrefillRung{}
	if ln.Prefill != nil {
		for _, r := range ln.Prefill.Rungs {
			if r.Depth > 0 && r.Tokps > 0 {
				rungs[r.Depth] = r
			}
		}
	}
	if len(rungs) == 0 {
		rungs = nil
	}
	out[ln.DeviceID] = PeerSpeed{
		VariantID: ln.VariantID,
		Rungs:     rungs,
		// The congestion divisor, and the same population a peer's
		// capacity_used counts: this machine's own work as well as the
		// mesh's. A machine busy with its owner's turn is busy.
		CapacityUsed: ln.CapacityUsed,
	}
	return out
}

// localCandidateReason says what this device is contributing to the ordered
// list, or why it is not in it.
//
// It replaces localBypassReason on the ranked auto arm ONLY. That function
// still answers the three modes that genuinely bypass this device, and the
// auto arm of a build with no LocalNode wired, and its wording is pinned
// (waired-agent#854 via docs/decisions/20260819/1900).
//
// Record of today's behaviour: no ruling pins these words. They are quoted
// in docs-site and move with it.
func localCandidateReason(ln LocalNode, in bool, drop localDrop, servingOff bool, localState, floor string) string {
	switch {
	case servingOff:
		// The line above this one already said the toggle is off.
		return ""
	case drop.belowFloor:
		// Verbatim from the arm this replaces, so an operator reads the
		// string they already know.
		return fmt.Sprintf("this computer's model is smaller than %q (routing floor)", floor)
	case !in && !ln.Serving:
		return fmt.Sprintf("this computer is not serving right now (local state for %q is %q), so only other computers are candidates",
			ln.ModelID, localState)
	case !in:
		return fmt.Sprintf("this computer is serving %q, which this request did not ask for, so only other computers are candidates",
			ln.ModelID)
	case ln.PendingModelID != "" && ln.PendingModelID != ln.ModelID:
		return fmt.Sprintf("this computer is serving %q (%s %q); the switch to %q has not finished, so this turn runs on the model that is loaded",
			ln.ModelID, ln.Runtime, ln.EngineTag, ln.PendingModelID)
	default:
		return fmt.Sprintf("this computer is serving %q (%s %q) and is ranked with the other computers",
			ln.ModelID, ln.Runtime, ln.EngineTag)
	}
}

// localLine is this device's own entry in the candidate trace, the sibling
// of makeMeshCandidate's peer line.
//
// Three of the peer line's figures are deliberately absent rather than
// printed as zero:
//
//   - rtt_ms, when there is no network leg and no reading to compare (see
//     localRTT); it is printed when it is a real 0.
//   - in_flight / load, which are this requester's OUTBOUND overlay
//     requests to a peer. A local turn is not one. This device's own
//     occupancy is in used=, where a peer's is too.
//   - map_age_ms, which qualifies figures read off one network-map frame.
//     These came from this process, this instant. Printing 0 would assert
//     a fresh frame rather than no frame.
func localLine(c meshCandidate) string {
	return fmt.Sprintf("this computer: %s model %q (score=%d, cap=%d, used=%d, silent=false)",
		c.runtime, c.tag, c.score, c.capacity, c.capacityUsed)
}

// localModelState is the catalog state of the model this device says it is
// serving, for the reason line. "" (this device named no model) reads as
// unknown rather than as not_present: absence of a reading is not evidence
// that the weights are gone.
func localModelState(st catalog.State, modelID string) string {
	if modelID == "" {
		return "unknown"
	}
	ms, ok := st.Models[modelID]
	if !ok {
		return string(catalog.ModelStateNotPresent)
	}
	return string(ms.State)
}
