package main

import (
	"math/rand/v2"

	"github.com/waired-ai/waired-agent/internal/router"
)

// localNodeForRouting reads what THIS device is serving right now, in the
// shape the Selector ranks a peer in (waired-agent#1302).
//
// Every value here is one the host already publishes to the mesh or already
// answers /healthz with. That is the point: a peer is ranked on what it says
// about itself, and this device was ranked on a different question entirely
// ("is the model I resolved to on disk?"), which is how it came to refuse
// its own turn while serving the previous model to peers.
//
// One read, in one place, so the tag, the variant, the measured rate and the
// occupancy cannot describe two different models.
func (p *agentInferenceProvider) localNodeForRouting() router.LocalNode {
	if p == nil {
		return router.LocalNode{}
	}
	// EngineReady is the oracle the peer-facing /healthz uses: the engine
	// adapter is StateReady, inference is not disabled, the engine is not
	// parked, and the model it is committed to has its weights. Through a
	// switch it answers with the OLD model — which is the whole asymmetry
	// waired-agent#1302 reports, read from the side that was right.
	serving, modelID := p.EngineReady()
	f := localFacts{serving: serving, modelID: modelID}
	if p.meshSnapshotFn != nil {
		snap := p.meshSnapshotFn()
		f.deviceID = snap.SelfDeviceID
		f.displayName = snap.Self.DeviceName
	}
	if serving {
		f.runtime = p.servingEngine()
		if st, err := p.store.Load(); err == nil {
			// activeEngineTag is the same value narrowPublishedModels puts
			// on the wire for peers, so this device joins the want set by
			// exactly the key a peer does.
			f.engineTag, _ = activeEngineTag(st)
			if st.Active != nil {
				f.variantID = st.Active.VariantID
			}
		}
		if psm := p.pendingSwapModel.Load(); psm != nil {
			f.pendingModelID = *psm
		}
		f.contextWindow = p.DeclaredContextWindow()
		f.capacity = p.WarmConversationSlots()
		// The same counter /healthz reports as capacity_used: this
		// machine's own work as well as any peer's. A machine busy with
		// its owner's turn is busy.
		f.capacityUsed = p.servingInFlight()
		f.prefill = localPrefillForRouting(p)
	}
	return localNodeFrom(f)
}

// localFacts is everything localNodeForRouting gathered from the live host,
// so the decision that follows is a pure function of it.
//
// The gathering is deliberately thin and the DECIDING is what has a table
// test: the interesting cases — serving the previous model through a switch,
// no device identity yet, an engine that is up but has recorded no tag — are
// all about which of these facts are present, and none of them is reachable
// on a test host without a live engine (CLAUDE.md §Test discipline: put the
// seam below the behaviour under test).
type localFacts struct {
	serving        bool
	modelID        string
	deviceID       string
	displayName    string
	runtime        string
	engineTag      string
	variantID      string
	pendingModelID string
	contextWindow  int
	capacity       int
	capacityUsed   int
	prefill        *router.PrefillRate
}

// localNodeFrom decides whether this device is a routing candidate, and
// under what description.
//
// Three shapes report "not serving", and the difference matters: the auto
// arm reads an empty reading as "this device could not describe itself" and
// falls through to the branch that always served it, rather than refusing a
// turn (waired-agent#1302).
//
//   - the engine is not ready, which is the honest answer;
//   - no device id, before the first network map. Nothing keyed by device
//     id would resolve, so there is nothing to rank;
//   - no engine tag, before the active selection is recorded. The tag is
//     the join key against the request's want set, and a candidate that
//     cannot be matched is not a candidate.
func localNodeFrom(f localFacts) router.LocalNode {
	if !f.serving || f.modelID == "" || f.deviceID == "" || f.engineTag == "" {
		return router.LocalNode{Serving: false, ModelID: f.modelID}
	}
	return router.LocalNode{
		DeviceID:       f.deviceID,
		DisplayName:    f.displayName,
		Serving:        true,
		Runtime:        f.runtime,
		EngineTag:      f.engineTag,
		ModelID:        f.modelID,
		VariantID:      f.variantID,
		PendingModelID: f.pendingModelID,
		ContextWindow:  f.contextWindow,
		Capacity:       f.capacity,
		CapacityUsed:   f.capacityUsed,
		Prefill:        f.prefill,
	}
}

// localPrefillForRouting converts this host's published prefill measurement
// into the router's shape. nil stays nil: the ordering must read "no
// reading" as no information, never as slow
// (docs/decisions/20260822/0218-residency-breaks-a-tie-not-a-ranking.md).
func localPrefillForRouting(p *agentInferenceProvider) *router.PrefillRate {
	m := p.PrefillRateForHealth()
	if m == nil || len(m.Rungs) == 0 {
		return nil
	}
	out := &router.PrefillRate{VariantID: m.VariantID}
	for _, r := range m.Rungs {
		out.Rungs = append(out.Rungs, router.PrefillRung{
			Depth: r.Depth,
			Tokps: r.Tokps,
			Bound: r.Bound,
		})
	}
	return out
}

// routingTieBreak is the in-tier randomiser (waired-agent#1303, S4). It is a
// package-level var rather than a literal so a test can make the ordering
// deterministic without reaching into the Selector.
var routingTieBreak = func(n int) int {
	if n <= 1 {
		return 0
	}
	return rand.IntN(n)
}
