package main

import (
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
	"github.com/waired-ai/waired-agent/internal/router"
)

// routeDirectiveRows is the Waired rows GET /v1/models advertises on the Local
// Gateway: the fixed route directives this computer can honour right now, plus
// one row per computer that is serving on the mesh (waired-agent#1306).
//
// The projection lives in internal/integration/modelrows because `waired
// claude _picker` needs the same answer from the same snapshot and used to be
// able to drift apart from it. What the daemon brings is that both facts the
// projection cannot derive are already in hand here: the mesh snapshot without
// a management round trip, and the Public Share posture.
//
// A host with no mesh snapshot function wired gets the fixed table, which is
// what modelrows.FactsFromSnapshot answers for a nil snapshot and for the same
// reason: a missing READ must not turn into a missing menu item.
func (p *agentInferenceProvider) routeDirectiveRows(peerLimit int) []modelrows.Row {
	var snap *inferencemesh.Snapshot
	if p != nil && p.meshSnapshotFn != nil {
		s := p.meshSnapshotFn()
		snap = &s
	}
	return modelrows.Rows(modelrows.FactsFromSnapshot(snap, peerLimit, p.publicShareOffered()))
}

// publicShareOffered reports whether the "Waired public share" row should be
// offered at all.
//
// It is the same pair the router's own gate reads before admitting a public
// candidate (router.publicGateFor): consent for the CURRENT warning text, and
// a mode that is not off. Management publishes the same pair as one
// EffectiveMode string, which is what the CLI picker writer asks for — reading
// the policy directly here avoids a second definition of "on".
//
// Offering a row the posture will refuse loses to not offering it, because a
// picker cannot render a row as disabled (owner ruling 2026-08-20,
// waired-agent#901). The reverse is not symmetrical: a session that already
// holds the id keeps sending it, and the router still words that refusal by
// naming which switch declined.
func (p *agentInferenceProvider) publicShareOffered() bool {
	if p == nil || p.publicPolicy == nil {
		return false
	}
	pol := p.publicPolicy()
	return pol.Consented && pol.Mode != router.PublicModeOff
}
