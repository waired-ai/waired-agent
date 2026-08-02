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
	a := New("self-id", Policy{}, func() time.Time { return now })

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

// TestAggregatorAdvertisedLivenessAtReceipt pins a PRODUCT CONTRACT:
// a peer's advertised last_check is judged once, against the instant
// its frame arrived, and the verdict is then held until a newer frame
// replaces it.
//
// This inverts the pre-waired-agent#323 test, which asserted the same
// bound was re-applied against wall-clock forever after (14 s fresh /
// 16 s stale at a 15 s threshold). That version encoded the defect:
// the CP does not re-emit a frame when only last_check advances, so
// every frame's verdict rotted 15 s after arrival.
func TestAggregatorAdvertisedLivenessAtReceipt(t *testing.T) {
	base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	now := base
	a := New("self-id", Policy{}, func() time.Time { return now })

	// A live peer: its push reached the CP a few seconds before the
	// frame was assembled, well inside DefaultAdvertisedLiveness.
	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "peer-a", InferenceState: mkState(true, now.Add(-5*time.Second))},
		},
	})
	if got := a.Snapshot(); !got.Reachable {
		t.Fatalf("Reachable=false with a peer whose last_check was 5s old at receipt")
	}

	// The bug: no new frame arrives, wall-clock advances past what used
	// to be the 15 s deadline. The peer must stay usable — the CP would
	// have re-emitted had anything actually changed.
	now = base.Add(2 * time.Minute)
	snap := a.Snapshot()
	if snap.Peers[0].Stale {
		t.Fatalf("peer went Stale without a new frame: the CP only re-emits on content change")
	}
	if !snap.Reachable {
		t.Fatalf("Reachable=false 2min after a frame that is still the CP's latest word")
	}

	// A peer that stopped pushing: the CP's stored state is frozen, so
	// the NEXT frame carries a last_check that is already too old.
	now = base.Add(5 * time.Minute)
	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "peer-a", InferenceState: mkState(true, now.Add(-4*time.Minute))},
		},
	})
	snap = a.Snapshot()
	if snap.Reachable {
		t.Fatalf("Reachable=true for a peer whose advertisement was 4min old on arrival")
	}
	if !snap.Peers[0].Stale {
		t.Fatalf("peer whose last_check was already stale at receipt must be Stale")
	}
	// But the entry remains visible — consumers must still be able to
	// render "this peer used to be reachable".
	if snap.Peers[0].InferenceState == nil {
		t.Fatalf("stale peers stay in the snapshot; only Reachable demotes")
	}
}

// TestAggregatorQuietMapWindowKeepsPeersRouteEligible is the assertion
// waired-agent#323 asks for by name: across a quiet window far longer
// than any per-peer probe cadence, a peer whose state genuinely has not
// changed stays route-eligible as long as the CP keeps honouring its
// backstop re-emit. PRODUCT CONTRACT.
func TestAggregatorQuietMapWindowKeepsPeersRouteEligible(t *testing.T) {
	base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	now := base
	a := New("self-id", Policy{}, func() time.Time { return now })

	frame := func() {
		a.Update(&signer.NetworkMap{
			Self: signer.NetworkMapPeer{DeviceID: "self-id"},
			Peers: []signer.NetworkMapPeer{
				// The peer keeps pushing every 5 s; the CP re-emits only
				// on its backstop because nothing about the peer changed.
				{DeviceID: "peer-a", InferenceState: mkState(true, now.Add(-5*time.Second))},
			},
		})
	}

	// Backstop cadence is 5 min (MapPollMaxAge). Walk ten minutes in
	// 30 s steps, delivering a frame only at the backstop boundaries.
	frame()
	for elapsed := 30 * time.Second; elapsed <= 10*time.Minute; elapsed += 30 * time.Second {
		now = base.Add(elapsed)
		if elapsed%(5*time.Minute) == 0 {
			frame()
		}
		snap := a.Snapshot()
		if snap.Peers[0].Stale {
			t.Fatalf("peer went Stale at t+%s in a quiet window", elapsed)
		}
		if !snap.Reachable {
			t.Fatalf("mesh reported unreachable at t+%s in a quiet window", elapsed)
		}
	}
}

// TestAggregatorMapStreamGoneMarksEveryPeerStale is the other half of
// the receipt-based contract: trusting an unchanged frame is only
// sound while frames still arrive. Past FrameStaleness the map stream
// itself is the thing that failed, and nothing derived from it can be
// trusted. PRODUCT CONTRACT.
func TestAggregatorMapStreamGoneMarksEveryPeerStale(t *testing.T) {
	base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	now := base
	a := New("self-id", Policy{}, func() time.Time { return now })

	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "peer-a", InferenceState: mkState(true, now)},
		},
	})

	now = base.Add(DefaultFrameStaleness - time.Second)
	if a.Snapshot().Peers[0].Stale {
		t.Fatalf("peer went Stale just inside FrameStaleness")
	}

	now = base.Add(DefaultFrameStaleness + time.Second)
	snap := a.Snapshot()
	if !snap.Peers[0].Stale {
		t.Fatalf("peer must be Stale once the newest frame is older than FrameStaleness")
	}
	if snap.Reachable {
		t.Fatalf("Reachable=true with a dead map stream")
	}
	if snap.MapAgeMS < (DefaultFrameStaleness).Milliseconds() {
		t.Errorf("MapAgeMS = %d, want > FrameStaleness so an operator can see WHY", snap.MapAgeMS)
	}
}

// TestAggregatorPeerLivenessMarksSilentAdvisory pins the third axis:
// receipt-based freshness alone would keep trusting a peer whose HOST
// dropped off the wire until the CP's next backstop frame, so the
// disco prober's recent-pong map gets a vote. Three-valued, matching
// disco.Service.ReachableSnapshot.
//
// The vote lands on Silent, NOT on Stale (waired#729). A disco pong
// travels raw UDP or the relay's WSS control session — never the WG
// data plane the inference request rides — so its absence predicts
// unusability, it does not prove it. Stale is the verdict that
// removes a peer from routing; Silent only demotes it, and the
// /healthz probe decides. PRODUCT CONTRACT.
func TestAggregatorPeerLivenessMarksSilentAdvisory(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	liveness := map[string]bool{}
	a := New("self-id", Policy{
		PeerLiveness: func() map[string]bool { return liveness },
	}, func() time.Time { return now })

	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "silent", InferenceState: mkState(true, now)},
			{DeviceID: "pinging", InferenceState: mkState(true, now)},
			{DeviceID: "unprobed", InferenceState: mkState(true, now)},
		},
	})

	liveness = map[string]bool{"silent": false, "pinging": true}

	byID := map[string]PeerView{}
	for _, p := range a.Snapshot().Peers {
		byID[p.DeviceID] = p
	}
	if !byID["silent"].Silent {
		t.Errorf("present+false (once seen, now silent) must be marked Silent")
	}
	if byID["silent"].Stale {
		t.Errorf("disco silence must not set Stale — it is advisory, not a veto (waired#729)")
	}
	if byID["pinging"].Silent || byID["pinging"].Stale {
		t.Errorf("present+true (recent pong) must be neither Silent nor Stale")
	}
	if byID["unprobed"].Silent || byID["unprobed"].Stale {
		t.Errorf("absent (never probed) must default to trust")
	}
}

// TestAggregatorSnapshotReachableCountsSilentPeers pins that a
// disco-silent peer still counts toward Snapshot.Reachable.
//
// Reachable is not just a tray label: it flows through
// SetInferenceReachableInMesh into the transparent proxy's degraded
// gate. If silence folded it down, a mesh whose only peer missed one
// pong round would flip the proxy to degraded and send every Claude
// turn straight upstream WITHOUT ever running the probe that would
// have found the peer healthy — waired#729's false veto, reproduced
// one layer up. PRODUCT CONTRACT.
func TestAggregatorSnapshotReachableCountsSilentPeers(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	a := New("self-id", Policy{
		PeerLiveness: func() map[string]bool { return map[string]bool{"only": false} },
	}, func() time.Time { return now })

	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "only", InferenceState: mkState(true, now)},
		},
	})

	snap := a.Snapshot()
	if !snap.Peers[0].Silent {
		t.Fatalf("test setup: the sole peer should be Silent")
	}
	if !snap.Reachable {
		t.Errorf("Reachable=false with a silent-but-otherwise-healthy sole peer: " +
			"this degrades the proxy without ever probing (waired#729)")
	}
}

// TestAggregatorSilentIndependentOfStale walks the two axes as a
// matrix. They answer different questions — "did the map stream or the
// peer's CP push die?" vs "has the peer's host gone quiet on disco?" —
// so neither may imply the other. PRODUCT CONTRACT.
func TestAggregatorSilentIndependentOfStale(t *testing.T) {
	base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name       string
		mapAge     time.Duration
		live       map[string]bool
		wantStale  bool
		wantSilent bool
	}{
		{"fresh frame, fresh pong", 0, map[string]bool{"p": true}, false, false},
		{"fresh frame, silent", 0, map[string]bool{"p": false}, false, true},
		{"dead frame, fresh pong", DefaultFrameStaleness + time.Minute, map[string]bool{"p": true}, true, false},
		{"dead frame, silent", DefaultFrameStaleness + time.Minute, map[string]bool{"p": false}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := base
			live := tc.live
			a := New("self-id", Policy{
				PeerLiveness: func() map[string]bool { return live },
			}, func() time.Time { return now })

			a.Update(&signer.NetworkMap{
				Self:  signer.NetworkMapPeer{DeviceID: "self-id"},
				Peers: []signer.NetworkMapPeer{{DeviceID: "p", InferenceState: mkState(true, base)}},
			})
			now = base.Add(tc.mapAge)

			got := a.Snapshot().Peers[0]
			if got.Stale != tc.wantStale {
				t.Errorf("Stale = %v, want %v", got.Stale, tc.wantStale)
			}
			if got.Silent != tc.wantSilent {
				t.Errorf("Silent = %v, want %v", got.Silent, tc.wantSilent)
			}
		})
	}
}

// TestAggregatorSelfStalenessStaysProbeCadenceBased pins that the
// receipt-based rework did NOT touch the self axis: Self is fed by the
// local probe loop, which really does tick every 5 s, so a wall-clock
// deadline on its last_check is correct there. PRODUCT CONTRACT.
func TestAggregatorSelfStalenessStaysProbeCadenceBased(t *testing.T) {
	base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	now := base
	a := New("self-id", Policy{}, func() time.Time { return now })

	a.UpdateLocal(mkState(true, base))
	if a.Snapshot().Self.Stale {
		t.Fatalf("self must be fresh immediately after a probe")
	}
	now = base.Add(DefaultSelfStaleness + time.Second)
	if !a.Snapshot().Self.Stale {
		t.Fatalf("self must go Stale once the local probe stops ticking")
	}
}

func TestAggregatorEmptyLastCheckIsStale(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	a := New("self-id", Policy{}, func() time.Time { return now })

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
	a := New("self-id", Policy{}, func() time.Time { return now })

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
	a := New("self-id", Policy{}, func() time.Time { return now })

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
	a := New("self-id", Policy{}, func() time.Time { return now })

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
	a := New("self-id", Policy{}, func() time.Time { return now })

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
	a := New("self-id", Policy{}, func() time.Time { return now })

	stale := &signer.InferenceState{
		Reachable: true,
		Type:      signer.InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		// Already older than DefaultAdvertisedLiveness when the frame
		// carrying it arrives — the CP's stored state is frozen because
		// this peer stopped pushing.
		LastCheck: now.Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano),
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

// TestAggregatorSnapshotPeersSortedStable pins a PRODUCT CONTRACT, not
// today's behaviour: Snapshot().Peers is ordered by DeviceName (DeviceID
// for unnamed peers), identically on every call.
//
// The tray fills fixed menu slots positionally from this slice, so when
// the order came straight out of the peers map — Go randomises map
// iteration per call — every 5 s poll rewrote the slot titles and node
// names visibly jumped between rows (#326). Six peers make an accidental
// pass vanishingly unlikely, and the insertion order below is
// deliberately not the expected one.
func TestAggregatorSnapshotPeersSortedStable(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	a := New("self-id", Policy{}, func() time.Time { return now })

	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "dev_04", DeviceName: "windows-desktop", InferenceState: mkState(true, now)},
			{DeviceID: "dev_02", DeviceName: "beta-node", InferenceState: mkState(true, now)},
			{DeviceID: "dev_06", DeviceName: "", InferenceState: mkState(true, now)}, // unnamed → sorts by DeviceID
			{DeviceID: "dev_01", DeviceName: "alpha-node", InferenceState: mkState(true, now)},
			{DeviceID: "dev_05", DeviceName: "linux-gpu", InferenceState: mkState(true, now)},
			{DeviceID: "dev_03", DeviceName: "mac-mini", InferenceState: mkState(true, now)},
		},
	})

	want := []string{"alpha-node", "beta-node", "dev_06", "linux-gpu", "mac-mini", "windows-desktop"}
	for i := 0; i < 20; i++ {
		snap := a.Snapshot()
		got := make([]string, 0, len(snap.Peers))
		for _, p := range snap.Peers {
			name := p.DeviceName
			if name == "" {
				name = p.DeviceID
			}
			got = append(got, name)
		}
		if len(got) != len(want) {
			t.Fatalf("call %d: got %d peers, want %d", i, len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("call %d: peer order = %v, want %v", i, got, want)
			}
		}
	}
}

// TestAggregatorSnapshotPeersSortedTieBreak covers two peers publishing
// the same DeviceName (a real possibility — names are not unique): the
// DeviceID breaks the tie so the pair does not swap between polls.
func TestAggregatorSnapshotPeersSortedTieBreak(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	a := New("self-id", Policy{}, func() time.Time { return now })

	a.Update(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self-id"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "dev_zz", DeviceName: "laptop", InferenceState: mkState(true, now)},
			{DeviceID: "dev_aa", DeviceName: "laptop", InferenceState: mkState(true, now)},
		},
	})

	for i := 0; i < 20; i++ {
		snap := a.Snapshot()
		if len(snap.Peers) != 2 {
			t.Fatalf("call %d: got %d peers, want 2", i, len(snap.Peers))
		}
		if snap.Peers[0].DeviceID != "dev_aa" || snap.Peers[1].DeviceID != "dev_zz" {
			t.Fatalf("call %d: tie-break order = %q,%q; want dev_aa,dev_zz",
				i, snap.Peers[0].DeviceID, snap.Peers[1].DeviceID)
		}
	}
}
