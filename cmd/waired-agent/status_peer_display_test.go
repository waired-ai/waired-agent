package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestAgentProviderStatusPeerDisplayID is the product contract from public
// share spec §8.5, as ratified for the other pinned-peer surfaces in #739
// and extended to this one in #768: only a public machine's grant
// pseudonym may be displayed, never its real device identifier.
//
// The resolution belongs here because this is the only layer that holds
// the grant. management.PeerStatus carries none, so a client given one
// could not tell a stranger's machine from one of your own — which is how
// the tray's Peers submenu came to print a real DeviceID for any peer
// whose DeviceName was empty.
//
// DeviceID itself is deliberately unchanged on the wire: the pin the
// router matches on and the testnet-fallback scripts both read it.
func TestAgentProviderStatusPeerDisplayID(t *testing.T) {
	tests := []struct {
		name          string
		grant         *signer.PeerGrant
		wantDisplayID string
		wantPublic    bool
	}{
		{
			name:          "own peer has no grant, so its DeviceID is displayable",
			wantDisplayID: "dev_peer",
		},
		{
			name:          "grant peer is named by its pseudonym",
			grant:         &signer.PeerGrant{ID: "grant_1", Pseudonym: "pub-node-b21c"},
			wantDisplayID: "pub-node-b21c",
			wantPublic:    true,
		},
		{
			// Still never the DeviceID (#739). But no longer empty
			// either: empty left every consumer to invent its own
			// substitute — the tray row read "unknown" and `waired
			// status` had none at all — while this layer is the only one
			// holding the grant that could answer (waired-agent#809).
			name:          "grant peer without a pseudonym is named by its grant",
			grant:         &signer.PeerGrant{ID: "grant_1"},
			wantDisplayID: inferencemesh.PublicPeerLabelFor("grant_1"),
			wantPublic:    true,
		},
		{
			name:          "grant peer with nothing to say still is not its DeviceID",
			grant:         &signer.PeerGrant{},
			wantDisplayID: inferencemesh.PublicPeerLabel,
			wantPublic:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &reconciler{
				state: map[string]*peerPathState{"pub_1": {currentPath: pathDirect}},
				nm:    &signer.NetworkMap{},
			}
			rec.nm.Peers = append(rec.nm.Peers, signer.NetworkMapPeer{
				NodePublicKey: "pub_1",
				DeviceID:      "dev_peer",
				Grant:         tt.grant,
			})
			prov := &agentProvider{
				id:         &identity.Identity{DeviceID: "self", DeviceName: "self-node"},
				reconciler: rec,
				peerByID:   map[string]*signer.NetworkMapPeer{"dev_peer": &rec.nm.Peers[0]},
			}

			st := prov.Status()
			if len(st.Peers) != 1 {
				t.Fatalf("peers = %d, want 1", len(st.Peers))
			}
			if got := st.Peers[0].DisplayID; got != tt.wantDisplayID {
				t.Errorf("DisplayID = %q, want %q", got, tt.wantDisplayID)
			}
			if got := st.Peers[0].DeviceID; got != "dev_peer" {
				t.Errorf("DeviceID = %q, want the real identifier to survive", got)
			}
			// Public is what a client keys the §8.5 substitution on. It
			// cannot be re-derived from the two fields above: DisplayID
			// EQUALS DeviceID for your own machines, so a comparison finds
			// no difference to act on, and that is exactly why `waired
			// status` printed the real id (waired-agent#809).
			if got := st.Peers[0].Public; got != tt.wantPublic {
				t.Errorf("Public = %v, want %v", got, tt.wantPublic)
			}
		})
	}
}

// TestPeerDisplayIdentifier pins the rule itself, at the one function that
// states it for signer.NetworkMapPeer. Its twin for inferencemesh.PeerView
// is cmd/waired/peers.go's peerDisplayID; the two types are what each
// layer holds, and only the RULE is shared (public share spec §8.5).
func TestPeerDisplayIdentifier(t *testing.T) {
	tests := []struct {
		name string
		peer signer.NetworkMapPeer
		want string
	}{
		{name: "no grant", peer: signer.NetworkMapPeer{DeviceID: "dev_a"}, want: "dev_a"},
		{
			name: "grant with pseudonym",
			peer: signer.NetworkMapPeer{DeviceID: "dev_a", Grant: &signer.PeerGrant{Pseudonym: "pub-x"}},
			want: "pub-x",
		},
		{
			name: "grant without pseudonym",
			peer: signer.NetworkMapPeer{DeviceID: "dev_a", Grant: &signer.PeerGrant{}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := peerDisplayIdentifier(&tt.peer); got != tt.want {
				t.Errorf("peerDisplayIdentifier = %q, want %q", got, tt.want)
			}
		})
	}
}
