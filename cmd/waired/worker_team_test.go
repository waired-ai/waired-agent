package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Team share spec §10.2: a teammate's computer is listed as "<device>
// (<owner>)" and its device id is never shown, so `waired worker set
// --pin` accepts that label, sends the real device id as the pin value
// (the router matches on it), and never prints a teammate's id when a
// name is ambiguous. Record of today's behaviour for the matching order;
// the no-device-id rule is the spec's.
func TestWorkerSet_PinTeammate(t *testing.T) {
	const teamA, teamB = "dev_teammate_a00001", "dev_teammate_b00001"
	teammate := func(id, owner string) inferencemesh.PeerView {
		return inferencemesh.PeerView{
			DeviceID: id, DeviceName: "studio-mac",
			Grant:          &signer.PeerGrant{Kind: signer.GrantKindTeam, Role: signer.GrantRoleProvider, DisplayName: owner},
			InferenceState: &signer.InferenceState{Reachable: true},
		}
	}
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{teammate(teamA, "Alice"), teammate(teamB, "Bob")}}
	srv, spy := workerTestServer(t, snap)
	defer srv.Close()

	var err error
	_ = captureStdout(t, func() {
		err = runWorker([]string{"set", "--mgmt", srv.URL, "--pin=studio-mac"})
	})
	if err == nil {
		t.Fatal("two teammates' same-named machines resolved instead of being refused")
	}
	for _, id := range []string{teamA, teamB} {
		if strings.Contains(err.Error(), id) {
			t.Errorf("the ambiguity error printed a teammate's device id: %v", err)
		}
	}
	if !strings.Contains(err.Error(), "studio-mac (Bob)") {
		t.Errorf("the ambiguity error does not list the labels to choose from: %v", err)
	}

	_ = captureStdout(t, func() {
		if err := runWorker([]string{"set", "--mgmt", srv.URL, "--pin=studio-mac (Bob)"}); err != nil {
			t.Fatalf("runWorker set --pin by team label: %v", err)
		}
	})
	if len(spy.posts) != 1 || spy.posts[0].PinnedPeerDeviceID != teamB {
		t.Fatalf("POSTs = %+v, want one pin to %s", spy.posts, teamB)
	}
}
