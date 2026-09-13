package signer_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestInferenceState_TeamShare_CanonicalJSON is the byte-identity pin
// required of every additive proto change
// (docs/decisions/20260719/0000-concurrent-proto-development.md §3), for
// the Team Share addition (waired#1371).
//
// InferenceState rides the signed NetworkMap, so a device that is not
// shared with a team must encode byte-for-byte as it does today or every
// existing signature stops verifying on a rolling upgrade.
func TestInferenceState_TeamShare_CanonicalJSON(t *testing.T) {
	// Not shared with a team: byte-for-byte the pre-addition encoding.
	off := signer.InferenceState{
		Reachable:   true,
		Type:        signer.InferenceTypeOllama,
		Endpoint:    "http://127.0.0.1:11434",
		LastCheck:   "2026-09-13T00:00:00Z",
		PublicShare: true,
	}
	const wantOff = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-09-13T00:00:00Z","public_share":true}`
	data, err := json.Marshal(&off)
	if err != nil {
		t.Fatalf("marshal without team share: %v", err)
	}
	if got := string(data); got != wantOff {
		t.Errorf("a device not shared with a team changed the encoding:\n got %s\nwant %s", got, wantOff)
	}

	// Shared: the key sits after public_capacity, in struct-declaration
	// order, next to the Public Share state it mirrors.
	on := off
	on.PublicCapacity = 2
	on.TeamShare = true
	const wantOn = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-09-13T00:00:00Z","public_share":true,"public_capacity":2,"team_share":true}`
	data, err = json.Marshal(&on)
	if err != nil {
		t.Fatalf("marshal with team share: %v", err)
	}
	if got := string(data); got != wantOn {
		t.Errorf("team_share encoding drifted:\n got %s\nwant %s", got, wantOn)
	}

	// Round trip, then canonical re-marshal — what signature verification
	// does. A reader that knows the field must reproduce the control
	// plane's canonical bytes exactly.
	var out signer.InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.TeamShare {
		t.Error("round trip lost team_share")
	}
	before, err := signer.CanonicalJSON(&on)
	if err != nil {
		t.Fatalf("canonicalize the sent state: %v", err)
	}
	after, err := signer.CanonicalJSON(&out)
	if err != nil {
		t.Fatalf("canonicalize the read state: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("canonical form changed across a round trip:\n sent %s\n read %s", before, after)
	}

	// A pre-addition payload parses with the field false: not shared with
	// a team, which is every device today.
	var pre signer.InferenceState
	if err := json.Unmarshal([]byte(wantOff), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition: %v", err)
	}
	if pre.TeamShare {
		t.Error("TeamShare = true on a pre-addition payload, want false")
	}
}

// TestPeerGrant_DisplayName_CanonicalJSON pins the PeerGrant encoding on
// both sides of the addition: a public grant keeps its exact bytes (no
// display_name key), and a team grant carries display_name with no
// pseudonym.
func TestPeerGrant_DisplayName_CanonicalJSON(t *testing.T) {
	public := signer.PeerGrant{
		ID:        "grant_1",
		Kind:      signer.GrantKindPublic,
		Role:      "provider",
		Pseudonym: "pub-node-b21c",
	}
	const wantPublic = `{"id":"grant_1","kind":"public","role":"provider","pseudonym":"pub-node-b21c"}`
	data, err := json.Marshal(&public)
	if err != nil {
		t.Fatalf("marshal public grant: %v", err)
	}
	if got := string(data); got != wantPublic {
		t.Errorf("a public grant changed its encoding:\n got %s\nwant %s", got, wantPublic)
	}

	team := signer.PeerGrant{
		ID:          "grant_2",
		Kind:        signer.GrantKindTeam,
		Role:        "provider",
		DisplayName: "Alice Example",
	}
	const wantTeam = `{"id":"grant_2","kind":"team","role":"provider","display_name":"Alice Example"}`
	data, err = json.Marshal(&team)
	if err != nil {
		t.Fatalf("marshal team grant: %v", err)
	}
	if got := string(data); got != wantTeam {
		t.Errorf("team grant encoding drifted:\n got %s\nwant %s", got, wantTeam)
	}

	var out signer.PeerGrant
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != team {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", out, team)
	}
	before, err := signer.CanonicalJSON(&team)
	if err != nil {
		t.Fatalf("canonicalize the sent grant: %v", err)
	}
	after, err := signer.CanonicalJSON(&out)
	if err != nil {
		t.Fatalf("canonicalize the read grant: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("canonical form changed across a round trip:\n sent %s\n read %s", before, after)
	}
}

// TestNetworkMapWithTeamGrant_RoundTripVerifies covers the capable
// poller: a team peer and Self team-share state survive sign/verify,
// and tampering with the fields a member could want to rewrite (the
// owner's name, the kind, the Self switch) is rejected.
func TestNetworkMapWithTeamGrant_RoundTripVerifies(t *testing.T) {
	k, err := signer.Generate()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	nm := scenarioTestNetworkMap()
	nm.Self.InferenceState = &signer.InferenceState{
		Reachable: true,
		Type:      signer.InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		LastCheck: "2026-09-13T00:00:00Z",
		TeamShare: true,
	}
	nm.Peers[0].Grant = &signer.PeerGrant{
		ID:          "grant_1",
		Kind:        signer.GrantKindTeam,
		Role:        "provider",
		DisplayName: "Alice Example",
	}
	nm.Peers[0].NetworkID = "net_teammate"
	signed, err := k.SignNetworkMap(nm)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := signer.VerifyNetworkMap(k.Public, signed); err != nil {
		t.Fatalf("verify: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*signer.NetworkMap)
	}{
		{"DisplayName", func(m *signer.NetworkMap) { m.Peers[0].Grant.DisplayName = "Mallory" }},
		{"Kind", func(m *signer.NetworkMap) { m.Peers[0].Grant.Kind = signer.GrantKindPublic }},
		{"TeamShare", func(m *signer.NetworkMap) { m.Self.InferenceState.TeamShare = false }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tampered := signed
			tampered.Peers = append([]signer.NetworkMapPeer(nil), signed.Peers...)
			grant := *signed.Peers[0].Grant
			tampered.Peers[0].Grant = &grant
			self := *signed.Self.InferenceState
			tampered.Self.InferenceState = &self
			c.mutate(&tampered)
			if err := signer.VerifyNetworkMap(k.Public, tampered); err == nil {
				t.Fatalf("expected verification failure after tampering %s", c.name)
			}
		})
	}
}

// TestTeamShareWireValues pins the literals, because both sides of the
// wire compare the strings and only one of them is in this repo. The
// grant kinds are the values the control plane has written to
// DeviceGrant.kind since Public Share v1, and the provider / consumer
// roles the values it has sent since then; changing one is a wire break,
// not a rename.
func TestTeamShareWireValues(t *testing.T) {
	for _, c := range []struct{ name, got, want string }{
		{"CapabilityTeamShareV1", signer.CapabilityTeamShareV1, "team-share-v1"},
		{"GrantKindPublic", signer.GrantKindPublic, "public"},
		{"GrantKindTeam", signer.GrantKindTeam, "team"},
		{"GrantRoleProvider", signer.GrantRoleProvider, "provider"},
		{"GrantRoleConsumer", signer.GrantRoleConsumer, "consumer"},
		{"GrantRoleBoth", signer.GrantRoleBoth, "both"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}
