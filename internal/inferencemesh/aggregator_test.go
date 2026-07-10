package inferencemesh

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

func mkState(reachable bool, lastCheck time.Time) *signer.InferenceState {
	return &signer.InferenceState{
		Reachable: reachable,
		Type:      signer.InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		LastCheck: lastCheck.UTC().Format(time.RFC3339Nano),
	}
}

func TestAggregatorPeersOnlyAggregate(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	a := New("self-id", 15*time.Second, func() time.Time { return now })

	// Self has a reachable engine. Snapshot.Reachable must NOT pick this
	// up — it's peers-only by design.
	a.UpdateLocal(mkState(true, now))
	if got := a.Snapshot(); got.Reachable {
		t.Fatalf("Reachable=true with self only must be false (peers-only)")
	}
	if got := a.Snapshot(); got.Self.InferenceState == nil || !got.Self.InferenceState.Reachable {
		t.Fatalf("Self.InferenceState must reflect UpdateLocal")
	}

	// Add a peer with reachable=false: still false.
	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "peer-a", InferenceState: mkState(false, now)},
		},
	})
	if got := a.Snapshot(); got.Reachable {
		t.Fatalf("Reachable=true when only unreachable peers exist")
	}

	// Add a peer with reachable=true and fresh last_check: true.
	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "peer-a", InferenceState: mkState(false, now)},
			{DeviceID: "peer-b", InferenceState: mkState(true, now)},
		},
	})
	if got := a.Snapshot(); !got.Reachable {
		t.Fatalf("Reachable=false with one reachable peer")
	}
}

func TestAggregatorStalenessThreshold(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	a := New("self-id", 15*time.Second, func() time.Time { return now })

	// Peer's last_check is 14 s ago — fresh.
	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "peer-a", InferenceState: mkState(true, now.Add(-14*time.Second))},
		},
	})
	if got := a.Snapshot(); !got.Reachable {
		t.Fatalf("Reachable=false with fresh (14s) peer")
	}
	if got := a.Snapshot(); got.Peers[0].Stale {
		t.Fatalf("peer with 14s last_check must not be Stale at 15s threshold")
	}

	// Same peer 16 s ago — stale, drop from aggregate.
	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "peer-a", InferenceState: mkState(true, now.Add(-16*time.Second))},
		},
	})
	snap := a.Snapshot()
	if snap.Reachable {
		t.Fatalf("Reachable=true with only stale peers")
	}
	if !snap.Peers[0].Stale {
		t.Fatalf("peer with 16s last_check must be Stale at 15s threshold")
	}
	// But the entry remains visible — consumers must still be able to
	// render "this peer used to be reachable".
	if snap.Peers[0].InferenceState == nil {
		t.Fatalf("stale peers stay in the snapshot; only Reachable demotes")
	}
}

func TestAggregatorEmptyLastCheckIsStale(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	a := New("self-id", 15*time.Second, func() time.Time { return now })

	bad := &signer.InferenceState{Reachable: true, Type: signer.InferenceTypeOllama, LastCheck: ""}
	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "peer-x", InferenceState: bad},
		},
	})
	snap := a.Snapshot()
	if snap.Reachable {
		t.Fatalf("Reachable=true with empty last_check (must treat as stale)")
	}
	if !snap.Peers[0].Stale {
		t.Fatalf("empty last_check must mark peer Stale")
	}
}

func TestAggregatorRemovesPeersOnNetworkMapUpdate(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	a := New("self-id", 15*time.Second, func() time.Time { return now })

	a.Update(&signer.NetworkMap{
		Self:  signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{{DeviceID: "p1"}, {DeviceID: "p2"}},
	})
	if got := len(a.Snapshot().Peers); got != 2 {
		t.Fatalf("expected 2 peers, got %d", got)
	}

	// Network map shrinks (peer revoked) — aggregator must drop it.
	a.Update(&signer.NetworkMap{
		Self:  signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{{DeviceID: "p2"}},
	})
	snap := a.Snapshot()
	if len(snap.Peers) != 1 || snap.Peers[0].DeviceID != "p2" {
		t.Fatalf("expected only p2 to remain, got %+v", snap.Peers)
	}
}

func TestAggregatorExcludesSelfFromPeers(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	a := New("self-id", 15*time.Second, func() time.Time { return now })

	// CP includes self in nm.Peers (it shouldn't, but be defensive) —
	// the aggregator must filter to avoid self counting in the peers-only
	// aggregate.
	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "self-id", InferenceState: mkState(true, now)},
			{DeviceID: "peer-a", InferenceState: mkState(false, now)},
		},
	})
	snap := a.Snapshot()
	if len(snap.Peers) != 1 || snap.Peers[0].DeviceID != "peer-a" {
		t.Fatalf("self must be filtered from Peers, got %+v", snap.Peers)
	}
	if snap.Reachable {
		t.Fatalf("self being reachable must NOT contribute to peers-only aggregate")
	}
}

func TestAggregatorSelfPlaceholderTracksNetworkMap(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	a := New("self-id", 15*time.Second, func() time.Time { return now })

	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id", DeviceName: "alice-mac", OverlayIP: "100.96.0.10"},
	})
	snap := a.Snapshot()
	if snap.Self.DeviceName != "alice-mac" || snap.Self.OverlayIP != "100.96.0.10" {
		t.Fatalf("self placeholder did not track network map: %+v", snap.Self)
	}
}

// TestAggregatorPropagatesPhase7Fields verifies the two Phase 7
// fields (Capacity, Hardware) ride through the aggregator unchanged.
// They land on InferenceState directly so no PeerView struct change
// is needed — but the Selector relies on them surviving the Update →
// Snapshot round trip, so guard with a test.
//
// PeerErrorRates and PeerRTTs were removed 20260517 (wire-only,
// consumer-less; the Selector reads agent-local snapshots instead).
func TestAggregatorPropagatesPhase7Fields(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	a := New("self-id", 15*time.Second, func() time.Time { return now })

	peerState := &signer.InferenceState{
		Reachable: true,
		Type:      signer.InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		Models:    []string{"qwen3:8b-q4_K_M"},
		LastCheck: now.UTC().Format(time.RFC3339Nano),
		Hardware: &signer.HardwareSummary{
			GPUs: []signer.HardwareGPUSummary{
				{Model: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24564, ComputeCap: "8.9"},
			},
			RAMTotalGB: 64,
		},
		Capacity: 8,
	}
	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "peer-a", InferenceState: peerState},
		},
	})

	snap := a.Snapshot()
	if len(snap.Peers) != 1 || snap.Peers[0].DeviceID != "peer-a" {
		t.Fatalf("expected single peer-a entry; got %+v", snap.Peers)
	}
	got := snap.Peers[0].InferenceState
	if got == nil {
		t.Fatal("peer-a InferenceState dropped during aggregate")
	}
	if got.Capacity != 8 {
		t.Errorf("Capacity = %d, want 8", got.Capacity)
	}
	if got.Hardware == nil || len(got.Hardware.GPUs) != 1 || got.Hardware.GPUs[0].Model != "NVIDIA GeForce RTX 4090" {
		t.Errorf("Hardware did not propagate: %+v", got.Hardware)
	}
}

// TestAggregatorStalePeerKeepsPhase7Fields documents the Selector
// contract: a stale peer still appears in Snapshot.Peers with its
// last-known Phase 7 fields intact, but with Stale=true. The
// Selector uses Stale as the gate, not InferenceState=nil.
func TestAggregatorStalePeerKeepsPhase7Fields(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	a := New("self-id", 15*time.Second, func() time.Time { return now })

	stale := &signer.InferenceState{
		Reachable: true,
		Type:      signer.InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		LastCheck: now.Add(-30 * time.Second).UTC().Format(time.RFC3339Nano), // > 15s
		Capacity:  4,
	}
	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "peer-a", InferenceState: stale},
		},
	})
	snap := a.Snapshot()
	if len(snap.Peers) != 1 {
		t.Fatalf("expected peer-a to remain in Peers even when stale; got %+v", snap.Peers)
	}
	if !snap.Peers[0].Stale {
		t.Errorf("expected Stale=true on overdue peer")
	}
	if snap.Peers[0].InferenceState == nil || snap.Peers[0].InferenceState.Capacity != 4 {
		t.Errorf("stale peer's Phase 7 fields were dropped; want Capacity=4, got %+v", snap.Peers[0].InferenceState)
	}
}
