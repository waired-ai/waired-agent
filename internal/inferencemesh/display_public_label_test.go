package inferencemesh

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// PRODUCT CONTRACT (waired-agent#809, owner ruling 2026-08-21): where a
// surface lists more than one public machine, each is named by the grant
// it came in under, so two of them do not both read "public machine".
//
// The suffix is a digest of the GRANT id — an arrangement this control
// plane made with this host, already written to this host's own logs as
// grant_id — so the assertions check twice: that the label distinguishes,
// and that nothing derived from the peer's real device identifier is in
// it.
func TestPublicPeerLabelFor(t *testing.T) {
	const (
		grantA = "grant_a7f3c1d9"
		grantB = "grant_b2e4f8a0"
	)
	a, b := PublicPeerLabelFor(grantA), PublicPeerLabelFor(grantB)
	if a == b {
		t.Fatalf("two grants share one label (%q) — the whole point is that a list of "+
			"public machines can be told apart", a)
	}
	if a != PublicPeerLabelFor(grantA) {
		t.Error("the label is not stable for one grant")
	}
	for _, label := range []string{a, b} {
		if !strings.HasPrefix(label, PublicPeerLabel+" (grant ") || !strings.HasSuffix(label, ")") {
			t.Errorf("label %q is not the owner-approved shape %q", label, PublicPeerLabel+" (grant XXXX)")
		}
		if strings.Contains(label, grantA) || strings.Contains(label, grantB) {
			t.Errorf("label %q carries the grant id verbatim; it is meant to be a digest", label)
		}
	}
	// An empty grant id must not produce a label that says "grant" and
	// then nothing.
	if got := PublicPeerLabelFor(""); got != PublicPeerLabel {
		t.Errorf("PublicPeerLabelFor(\"\") = %q, want the bare %q", got, PublicPeerLabel)
	}
}

// PeerDisplayLabel is the shape three call sites had open-coded. The real
// device id must not reach it on any branch — that is the §8.5 rule this
// consolidation exists to state once instead of three times.
func TestPeerDisplayLabel(t *testing.T) {
	const foreignDeviceID = "dev_foreign00000001"
	cases := []struct {
		name string
		peer PeerView
		want string
	}{
		{
			name: "own machine is named by its device id",
			peer: peer(nil),
			want: "dev_peer",
		},
		{
			name: "public machine with a pseudonym is named by it",
			peer: peer(func(p *PeerView) {
				p.DeviceID = foreignDeviceID
				p.Grant = &signer.PeerGrant{ID: "grant_1", Kind: "public", Role: "provider", Pseudonym: "guest-a7f3"}
			}),
			want: "guest-a7f3",
		},
		{
			name: "public machine with no pseudonym is named by its grant",
			peer: peer(func(p *PeerView) {
				p.DeviceID = foreignDeviceID
				p.Grant = &signer.PeerGrant{ID: "grant_1", Kind: "public", Role: "provider"}
			}),
			want: PublicPeerLabelFor("grant_1"),
		},
		{
			// A grant carrying neither a pseudonym nor an id still must
			// not fall through to the device id.
			name: "public machine with nothing to say is still not its device id",
			peer: peer(func(p *PeerView) {
				p.DeviceID = foreignDeviceID
				p.Grant = &signer.PeerGrant{Kind: "public", Role: "provider"}
			}),
			want: PublicPeerLabel,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PeerDisplayLabel(tc.peer)
			if got != tc.want {
				t.Errorf("PeerDisplayLabel() = %q, want %q", got, tc.want)
			}
			if tc.peer.Grant != nil && strings.Contains(got, foreignDeviceID) {
				t.Errorf("PeerDisplayLabel() = %q, which carries a public peer's real device id", got)
			}
		})
	}
}
