package signer_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestNetworkMapWithoutPublicShare_NoNewFieldsInCanonical pins the
// §8.4/§9 invariant the proto/v0.2.0 freeze rests on: when none of the
// Public Share fields are set, the canonical JSON bytes contain no
// trace of them, so maps served to non-capable pollers are
// byte-identical to pre-v0.2.0 maps and existing signatures keep
// verifying. A regression here (dropping an omitempty, marshalling the
// nil Grant as null) would break signature verification fleet-wide.
func TestNetworkMapWithoutPublicShare_NoNewFieldsInCanonical(t *testing.T) {
	nm := scenarioTestNetworkMap()
	nm.Peers[0].InferenceState = &signer.InferenceState{
		Reachable: true,
		Type:      signer.InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		LastCheck: "2026-07-19T00:00:00Z",
	}
	canonical, err := signer.CanonicalJSON(nm)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	for _, key := range []string{"grant", "public_share", "public_capacity", "pseudonym"} {
		if bytes.Contains(canonical, []byte(`"`+key+`"`)) {
			t.Fatalf("canonical JSON unexpectedly contains %q:\n%s", key, canonical)
		}
	}
	// NetworkMapPeer.NetworkID shares its JSON key with the map's own
	// top-level network_id, so a Contains check cannot see a leak —
	// count occurrences instead: exactly one (the top level) when no
	// peer carries the field (Self + ≥1 peer in the scenario map, so
	// any per-peer emission makes it ≥2).
	if n := bytes.Count(canonical, []byte(`"network_id"`)); n != 1 {
		t.Fatalf("canonical JSON contains %d network_id keys, want 1 (top-level only):\n%s", n, canonical)
	}
}

// TestNetworkMapWithGrant_RoundTripVerifies covers the capable-poller
// path: a CP-injected PeerGrant and Self public-share state round-trip
// through sign/verify, and tampering with any grant field (a guest
// rewriting its pseudonym, flipping role) is rejected.
func TestNetworkMapWithGrant_RoundTripVerifies(t *testing.T) {
	k, err := signer.Generate()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	nm := scenarioTestNetworkMap()
	nm.Self.InferenceState = &signer.InferenceState{
		Reachable:      true,
		Type:           signer.InferenceTypeOllama,
		Endpoint:       "http://127.0.0.1:11434",
		LastCheck:      "2026-07-19T00:00:00Z",
		PublicShare:    true,
		PublicCapacity: 2,
	}
	nm.Peers[0].Grant = &signer.PeerGrant{
		ID:        "grant_1",
		Kind:      "public",
		Role:      "provider",
		Pseudonym: "pub-node-b21c",
	}
	nm.Peers[0].NetworkID = "net_provider"
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
		{"GrantID", func(m *signer.NetworkMap) { m.Peers[0].Grant.ID = "grant_2" }},
		{"Role", func(m *signer.NetworkMap) { m.Peers[0].Grant.Role = "consumer" }},
		{"Pseudonym", func(m *signer.NetworkMap) { m.Peers[0].Grant.Pseudonym = "guest-ffff" }},
		{"PeerNetworkID", func(m *signer.NetworkMap) { m.Peers[0].NetworkID = "net_evil" }},
		{"PublicShare", func(m *signer.NetworkMap) { m.Self.InferenceState.PublicShare = false }},
		{"PublicCapacity", func(m *signer.NetworkMap) { m.Self.InferenceState.PublicCapacity = 9 }},
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

// TestPublicUsageReport_RoundTrip pins the §12 batch-report wire shape
// shared by the agent reporter (E1b) and the CP intake (E1a).
func TestPublicUsageReport_RoundTrip(t *testing.T) {
	report := signer.PublicUsageReport{Entries: []signer.PublicUsageEntry{{
		GrantID:      "grant_1",
		ModelID:      "qwen3:8b",
		Class:        "sub",
		Requests:     4,
		InputTokens:  1200,
		OutputTokens: 340,
		InferenceMS:  5600,
		WindowStart:  "2026-07-19T00:00:00Z",
		WindowEnd:    "2026-07-19T00:05:00Z",
	}}}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"grant_id", "model_id", "class", "requests",
		"input_tokens", "output_tokens", "inference_ms", "window_start", "window_end"} {
		if !bytes.Contains(raw, []byte(`"`+key+`"`)) {
			t.Fatalf("marshalled report missing %q:\n%s", key, raw)
		}
	}
	var got signer.PublicUsageReport
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(report, got) {
		t.Fatalf("round-trip mismatch:\nwant %+v\ngot  %+v", report, got)
	}
}

// TestCapabilityPublicShareV1_WireValue pins the capability literal:
// CP poll intake, distribution gate, and agent poller all compare this
// exact string, so a reword is a wire-protocol break, not a rename.
func TestCapabilityPublicShareV1_WireValue(t *testing.T) {
	if signer.CapabilityPublicShareV1 != "public-share-v1" {
		t.Fatalf("CapabilityPublicShareV1 = %q, want %q",
			signer.CapabilityPublicShareV1, "public-share-v1")
	}
}
