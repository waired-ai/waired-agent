package inferencemesh

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestAggregatorPropagatesGrant pins the Public Share plumbing: a
// grant-tagged foreign peer keeps its Grant annotation through Update →
// Snapshot (D2's router partition consumes it), and own-network peers
// stay Grant-less.
func TestAggregatorPropagatesGrant(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	a := New("dev-self", 15*time.Second, func() time.Time { return now })
	nm := &signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "dev-self"},
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "dev-own", OverlayIP: "100.64.0.2"},
			{
				DeviceID:  "dev-foreign",
				OverlayIP: "100.64.0.3",
				Grant: &signer.PeerGrant{
					ID:        "grant_1",
					Kind:      "public",
					Role:      "provider",
					Pseudonym: "amber-fox-42",
				},
			},
		},
	}
	a.Update(nm)

	snap := a.Snapshot()
	byID := map[string]PeerView{}
	for _, p := range snap.Peers {
		byID[p.DeviceID] = p
	}
	own, ok := byID["dev-own"]
	if !ok || own.Grant != nil {
		t.Fatalf("own peer: ok=%v grant=%+v, want present with nil grant", ok, own.Grant)
	}
	foreign, ok := byID["dev-foreign"]
	if !ok || foreign.Grant == nil {
		t.Fatalf("foreign peer: ok=%v grant=%+v, want present with grant", ok, foreign.Grant)
	}
	if foreign.Grant.ID != "grant_1" || foreign.Grant.Role != "provider" || foreign.Grant.Pseudonym != "amber-fox-42" {
		t.Fatalf("grant fields lost in snapshot: %+v", foreign.Grant)
	}

	// A later map without the grant peer drops it (map-GC parity).
	a.Update(&signer.NetworkMap{
		Self:  signer.NetworkMapPeer{DeviceID: "dev-self"},
		Peers: []signer.NetworkMapPeer{{DeviceID: "dev-own", OverlayIP: "100.64.0.2"}},
	})
	for _, p := range a.Snapshot().Peers {
		if p.DeviceID == "dev-foreign" {
			t.Fatalf("expired grant peer must drop out of the snapshot")
		}
	}
}
