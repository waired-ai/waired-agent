package router

import (
	"reflect"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Priority wire encoding (mirrors store.InferencePriority{High,Middle,Low});
// the router consumes the raw int off InferenceState.Priority.
const (
	prioHigh   = 1
	prioMiddle = 0
	prioLow    = -1
)

// mkPeerWithPriority builds an unlimited-capacity, reachable peer carrying the
// given admin routing priority, so selection turns purely on priority and the
// deviceID tie-break (no admission/load interference).
func mkPeerWithPriority(deviceID, tag string, priority int) inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID:   deviceID,
		DeviceName: deviceID,
		Stale:      false,
		InferenceState: &signer.InferenceState{
			Reachable: true,
			Type:      signer.InferenceTypeOllama,
			Models:    []string{tag},
			LastCheck: "2026-05-14T18:00:00Z",
			Priority:  priority,
		},
	}
}

// TestSortMeshCandidates_PriorityIsDominant pins the dominance contract: a
// Low-priority peer with a far higher score AND the lower deviceID still sorts
// behind a High-priority peer. Priority outranks score and every Phase 7 axis.
func TestSortMeshCandidates_PriorityIsDominant(t *testing.T) {
	cands := []meshCandidate{
		{deviceID: "peer-A", priority: prioLow, score: 9_000_000_000},
		{deviceID: "peer-Z", priority: prioHigh, score: 1},
		{deviceID: "peer-M", priority: prioMiddle, score: 5_000_000_000},
	}
	sortMeshCandidates(cands)
	got := []string{cands[0].deviceID, cands[1].deviceID, cands[2].deviceID}
	want := []string{"peer-Z", "peer-M", "peer-A"} // High → Middle → Low
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("priority ordering: got %v want %v", got, want)
	}
}

// TestSortMeshCandidates_Phase7ChainWithinTier confirms the existing chain
// still runs untouched within one priority tier: equal priority falls through
// to score desc (and onward).
func TestSortMeshCandidates_Phase7ChainWithinTier(t *testing.T) {
	cands := []meshCandidate{
		{deviceID: "peer-A", priority: prioHigh, score: 1},
		{deviceID: "peer-Z", priority: prioHigh, score: 100},
	}
	sortMeshCandidates(cands)
	if cands[0].deviceID != "peer-Z" {
		t.Fatalf("within a priority tier, higher score must win; got %q", cands[0].deviceID)
	}
}

// TestSelector_MeshFallback_HighPriorityBeatsMiddle exercises the full path
// (buildMeshCandidates → sort): peer-A is Middle and has the lower deviceID
// (so the deterministic tie-break would pick it), but peer-Z is High, so the
// admin priority must route to peer-Z. Proves priority flows off
// InferenceState into selection.
func TestSelector_MeshFallback_HighPriorityBeatsMiddle(t *testing.T) {
	s := weightedSelector(nil, nil,
		mkPeerWithPriority("peer-A", "qwen3:8b-q4_K_M", prioMiddle),
		mkPeerWithPriority("peer-Z", "qwen3:8b-q4_K_M", prioHigh),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-Z" {
		t.Errorf("High-priority peer-Z must win over Middle peer-A; got %q", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_MeshFallback_LowPriorityYieldsToMiddle is the mirror: peer-A is
// Low and has the lower deviceID (would win the tie-break), but Middle peer-Z
// must be preferred — a Low device only serves when no higher tier can.
func TestSelector_MeshFallback_LowPriorityYieldsToMiddle(t *testing.T) {
	s := weightedSelector(nil, nil,
		mkPeerWithPriority("peer-A", "qwen3:8b-q4_K_M", prioLow),
		mkPeerWithPriority("peer-Z", "qwen3:8b-q4_K_M", prioMiddle),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-Z" {
		t.Errorf("Middle peer-Z must win over Low peer-A; got %q", sel.Runtime)
	}
	sel.Release()
}
