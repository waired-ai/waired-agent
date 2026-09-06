package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The listing's columns are the peers' claims about themselves, pushed
// over the control plane. A computer whose overlay is dead still
// receives them, so it lists every peer as capable and routes to all of
// them (#849). These tests pin what it says instead, and — just as
// importantly — when it says nothing.

// peersAndStatusServer serves both routes `waired peers list` reads: the
// mesh snapshot it has always read, and this computer's own view of the
// overlay.
func peersAndStatusServer(t *testing.T, snap inferencemesh.Snapshot, st management.Status) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/inference/mesh", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(snap)
	})
	mux.HandleFunc("/waired/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(st)
	})
	return httptest.NewServer(mux)
}

// servingPeer is a peer whose own row says it can serve — the only shape
// that makes the note's point, since a row already saying "no" is not
// the misleading case.
func servingPeer(deviceID, name string) inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID:   deviceID,
		DeviceName: name,
		InferenceState: &signer.InferenceState{
			Reachable:      true,
			Type:           signer.InferenceTypeOllama,
			Models:         []string{"qwen3:8b-q4_K_M"},
			ActiveModel:    "qwen3-8b-instruct",
			SubsystemState: signer.SubsystemStateReady,
		},
	}
}

func peersListOutput(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return captureStdout(t, func() {
		if err := runPeers([]string{"list", "--mgmt", meshAddrFromURL(srv.URL)}); err != nil {
			t.Fatalf("runPeers list: %v", err)
		}
	})
}

func TestPeersList_NamesThePeerThisComputerHasNotHeardFrom(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		servingPeer("dev_heard", "alice"),
		servingPeer("dev_quiet", "bob"),
	}}
	st := management.Status{
		DiscoEnabled: true,
		Peers: []management.PeerStatus{
			// Relay-only still counts as heard: a reply over the relay is
			// a reply. RTTUnknown's doc warns against reading "no direct
			// samples" as unreachable, and this is that warning honoured.
			{DeviceID: "dev_heard", CurrentPath: "relay", RelaySampleCount: 812},
			{DeviceID: "dev_quiet", CurrentPath: "relay"},
		},
	}
	srv := peersAndStatusServer(t, snap, st)
	defer srv.Close()

	out := peersListOutput(t, srv)
	if !strings.Contains(out, "This computer has had no reply from: bob.") {
		t.Errorf("quiet peer not named: %q", out)
	}
	if strings.Contains(out, "alice") && strings.Contains(out, "no reply from: alice") {
		t.Errorf("peer that answered named as quiet: %q", out)
	}
	if strings.Contains(out, "any computer listed above") {
		t.Errorf("partial silence reported as total: %q", out)
	}
	if !strings.Contains(out, "WORKER-CAPABLE is what each computer reports about itself") {
		t.Errorf("note does not say whose claim the column is: %q", out)
	}
}

// The case the issue calls the least useful answer available: every row
// says yes and not one of them has ever answered this computer.
func TestPeersList_TotalSilenceSaysSo(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		servingPeer("dev_a", "alice"),
		servingPeer("dev_b", "bob"),
	}}
	st := management.Status{
		DiscoEnabled: true,
		Peers: []management.PeerStatus{
			{DeviceID: "dev_a", CurrentPath: "relay"},
			{DeviceID: "dev_b", CurrentPath: "relay"},
		},
	}
	srv := peersAndStatusServer(t, snap, st)
	defer srv.Close()

	out := peersListOutput(t, srv)
	if !strings.Contains(out, "no reply from any computer listed above") {
		t.Errorf("totally isolated host not told so: %q", out)
	}
	if !strings.Contains(out, "yes") {
		t.Fatalf("test lost its point — no row claims to be capable: %q", out)
	}
}

func TestPeersList_NoNoteWhenEveryPeerHasAnswered(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		servingPeer("dev_a", "alice"),
		servingPeer("dev_b", "bob"),
	}}
	st := management.Status{
		DiscoEnabled: true,
		Peers: []management.PeerStatus{
			{DeviceID: "dev_a", CurrentPath: "direct", DirectSampleCount: 91},
			{DeviceID: "dev_b", CurrentPath: "relay", RelaySampleCount: 4},
		},
	}
	srv := peersAndStatusServer(t, snap, st)
	defer srv.Close()

	if out := peersListOutput(t, srv); strings.Contains(out, "no reply") {
		t.Errorf("healthy fleet libelled: %q", out)
	}
}

// Disco off makes every sample count structurally zero. Reading that as
// silence would put the note on every healthy --force-relay host.
func TestPeersList_NoNoteWhenDiscoIsOff(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		servingPeer("dev_a", "alice"),
	}}
	st := management.Status{
		DiscoEnabled: false,
		Peers:        []management.PeerStatus{{DeviceID: "dev_a", CurrentPath: "relay"}},
	}
	srv := peersAndStatusServer(t, snap, st)
	defer srv.Close()

	if out := peersListOutput(t, srv); strings.Contains(out, "no reply") {
		t.Errorf("note printed with no signal behind it: %q", out)
	}
}

// A daemon that does not serve the status route at all — older builds,
// and every existing test in this package — must lose the note, not the
// listing.
func TestPeersList_SurvivesADaemonWithNoStatusRoute(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		servingPeer("dev_a", "alice"),
	}}
	srv := peersTestServer(t, snap)
	defer srv.Close()

	out := peersListOutput(t, srv)
	if !strings.Contains(out, "alice") {
		t.Errorf("listing lost when the status read failed: %q", out)
	}
	if strings.Contains(out, "no reply") {
		t.Errorf("note printed without a reading: %q", out)
	}
}

// A key the network no longer has for this device is the whole story:
// nothing can reach it, so naming the peers would send the reader after
// five machines that are fine.
func TestPeersList_KeyMismatchReplacesThePeerNote(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		servingPeer("dev_a", "alice"),
	}}
	st := management.Status{
		DiscoEnabled:     true,
		NodeKeyAgreement: management.NodeKeyAgreementDiverged,
		Peers:            []management.PeerStatus{{DeviceID: "dev_a", CurrentPath: "relay"}},
	}
	srv := peersAndStatusServer(t, snap, st)
	defer srv.Close()

	out := peersListOutput(t, srv)
	if !strings.Contains(out, "This computer's key doesn't match the one your network has for it") {
		t.Errorf("key mismatch not named: %q", out)
	}
	if !strings.Contains(out, "waired init") {
		t.Errorf("note does not say how to recover: %q", out)
	}
	if strings.Contains(out, "no reply from") {
		t.Errorf("peers blamed for this computer's key: %q", out)
	}
}

// A rotation is not a fault (management.NodeKeyAgreementRotating), so it
// must not raise the key note.
func TestPeersList_RotatingKeyIsNotAFault(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		servingPeer("dev_a", "alice"),
	}}
	st := management.Status{
		DiscoEnabled:     true,
		NodeKeyAgreement: management.NodeKeyAgreementRotating,
		Peers:            []management.PeerStatus{{DeviceID: "dev_a", CurrentPath: "relay", RelaySampleCount: 3}},
	}
	srv := peersAndStatusServer(t, snap, st)
	defer srv.Close()

	if out := peersListOutput(t, srv); strings.Contains(out, "does not match") {
		t.Errorf("a finishing key change reported as a fault: %q", out)
	}
}

// Public share spec §8.5 / #739: a stranger's real device identifier
// never reaches a surface a person reads, and that includes this note.
func TestOwnViewNote_PublicPeerWithNoPseudonymIsNamedByItsLabel(t *testing.T) {
	p := servingPeer("dev_real_identifier_of_a_stranger", "")
	p.Grant = &signer.PeerGrant{Role: "provider"}
	// A second peer that did answer, so the note takes the branch that
	// names names — total silence deliberately names none.
	snap := &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{p, servingPeer("dev_heard", "alice")}}
	st := &management.Status{
		DiscoEnabled: true,
		Peers: []management.PeerStatus{
			{DeviceID: p.DeviceID, CurrentPath: "relay"},
			{DeviceID: "dev_heard", CurrentPath: "relay", RelaySampleCount: 12},
		},
	}

	note := ownViewNote(snap, st)
	if !strings.Contains(note, inferencemesh.PublicPeerLabel) {
		t.Errorf("public machine dropped from the note: %q", note)
	}
	if strings.Contains(note, p.DeviceID) {
		t.Errorf("real device identifier printed: %q", note)
	}
}

// The two sides are matched on the real DeviceID, which neither prints.
// DisplayID is empty for a public machine with no pseudonym, so matching
// on it would silently drop exactly the rows §8.5 cares about.
func TestOwnViewNote_MatchesPeersEvenWithoutADisplayID(t *testing.T) {
	snap := &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		servingPeer("dev_quiet", "bob"),
		servingPeer("dev_heard", "alice"),
	}}
	st := &management.Status{
		DiscoEnabled: true,
		Peers: []management.PeerStatus{
			{DeviceID: "dev_quiet", DisplayID: "", CurrentPath: "relay"},
			{DeviceID: "dev_heard", DisplayID: "", CurrentPath: "relay", RelaySampleCount: 7},
		},
	}

	if note := ownViewNote(snap, st); !strings.Contains(note, "bob") {
		t.Errorf("peer not matched without a DisplayID: %q", note)
	}
}

// A peer the reconciler has no entry for is unknown, not quiet.
func TestOwnViewNote_PeerWithNoPathStateIsNotClaimedQuiet(t *testing.T) {
	snap := &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		servingPeer("dev_a", "alice"),
	}}
	st := &management.Status{
		DiscoEnabled: true,
		Peers:        []management.PeerStatus{{DeviceID: "dev_other", CurrentPath: "relay"}},
	}

	if note := ownViewNote(snap, st); note != "" {
		t.Errorf("silence claimed about a peer with no path state: %q", note)
	}
}
