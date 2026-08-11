package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestNodeKeyAgreement pins the verdict for every combination of
// (local, published, previous).
//
// A fix pin, not a record of today's behaviour: nothing compared these
// two before, which is why waired-ai/waired#1137 took a frozen log
// bundle and a cross-host count of rejected handshakes to reach the same
// conclusion this one line reports.
func TestNodeKeyAgreement(t *testing.T) {
	const (
		mine   = "bWluZQ=="
		theirs = "dGhlaXJz"
		old    = "b2xk"
	)

	cases := []struct {
		name                       string
		local, published, previous string
		want                       string
	}{
		{
			name:  "the published key is the one this device holds",
			local: mine, published: mine,
			want: management.NodeKeyAgreementOK,
		},
		{
			// The rotator tells the control plane first and promotes the
			// local file second, so the published key legitimately runs
			// ahead for the length of that window.
			name:  "a rotation the control plane has taken and this agent has not promoted",
			local: mine, published: theirs, previous: mine,
			want: management.NodeKeyAgreementRotating,
		},
		{
			name:  "a published key matching neither the current nor the previous local key",
			local: mine, published: theirs, previous: old,
			want: management.NodeKeyAgreementDiverged,
		},
		{
			name:  "no previous key published and the current one does not match",
			local: mine, published: theirs,
			want: management.NodeKeyAgreementDiverged,
		},
		{
			// A control plane that publishes no self key tells us
			// nothing; that is unknown, not a mismatch.
			name:  "the map carries no self key",
			local: mine,
			want:  "",
		},
		{
			name:      "the agent has no key of its own yet",
			published: theirs,
			want:      "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeKeyAgreement(tc.local, tc.published, tc.previous); got != tc.want {
				t.Fatalf("nodeKeyAgreement(%q, %q, %q) = %q, want %q",
					tc.local, tc.published, tc.previous, got, tc.want)
			}
		})
	}
}

// TestStatusReportsTheKeyVerdict covers the wiring: the comparison is
// only as good as the two keys reaching it, and replacePeers is the one
// place the published half is read.
func TestStatusReportsTheKeyVerdict(t *testing.T) {
	const (
		mine   = "bWluZQ=="
		theirs = "dGhlaXJz"
	)
	p := &agentProvider{
		id:          &identity.Identity{DeviceID: "dev_self", DeviceName: "workshop-mac"},
		selfNodePub: mine,
	}

	// Before any map: unknown, and no half-populated comparison.
	if st := p.Status(); st.NodeKeyAgreement != "" || st.PublishedNodePublicKey != "" {
		t.Fatalf("pre-map Status = %+v, want no verdict", st)
	}

	p.replacePeers(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{NodePublicKey: theirs},
	})
	st := p.Status()
	if st.NodeKeyAgreement != management.NodeKeyAgreementDiverged {
		t.Fatalf("NodeKeyAgreement = %q, want %q", st.NodeKeyAgreement, management.NodeKeyAgreementDiverged)
	}
	if st.NodePublicKey != mine || st.PublishedNodePublicKey != theirs {
		t.Fatalf("Status carries (%q, %q), want (%q, %q)",
			st.NodePublicKey, st.PublishedNodePublicKey, mine, theirs)
	}

	// A later map that agrees clears the verdict — the check follows the
	// live map rather than latching on the first frame.
	p.replacePeers(&signer.NetworkMap{
		Self: signer.NetworkMapPeer{NodePublicKey: mine},
	})
	if got := p.Status().NodeKeyAgreement; got != management.NodeKeyAgreementOK {
		t.Fatalf("after an agreeing map, NodeKeyAgreement = %q, want %q", got, management.NodeKeyAgreementOK)
	}
}
