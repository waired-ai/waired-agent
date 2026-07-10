package frame

import (
	"bytes"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		v    any
		typ  string
	}{
		{
			name: "client_hello",
			typ:  TypeClientHello,
			v: ClientHello{
				Version:          Version,
				NetworkID:        "wn_1",
				DeviceID:         "dev_a",
				NodePublicKey:    "AAAA",
				MachinePublicKey: "BBBB",
				DeviceCertificate: signer.DeviceCertificate{
					Version:          1,
					NetworkID:        "wn_1",
					DeviceID:         "dev_a",
					AccountID:        "acct_1",
					MachinePublicKey: "BBBB",
					NodePublicKey:    "AAAA",
					OverlayIP:        "100.96.0.10",
					AllowedServices:  []string{"inference"},
					IssuedAt:         "2026-04-30T00:00:00Z",
					ExpiresAt:        "2026-10-27T00:00:00Z",
					Signature:        "CCCC",
				},
				ClientNonce:     "Nonce==",
				SupportedFrames: []string{"encrypted_packet", "heartbeat"},
			},
		},
		{
			name: "relay_challenge",
			typ:  TypeRelayChallenge,
			v: RelayChallenge{
				RelayID:    "relay_local_1",
				RelayNonce: "Nonce==",
				ServerTime: "2026-04-30T00:00:01Z",
			},
		},
		{
			name: "client_proof",
			typ:  TypeClientProof,
			v: ClientProof{
				SignatureAlg: "ed25519",
				Signature:    "DDDD",
			},
		},
		{
			name: "relay_established",
			typ:  TypeRelayEstablished,
			v: RelayEstablished{
				RelaySessionID:           "rs_1",
				HeartbeatIntervalSeconds: 20,
				MaxFrameSizeBytes:        1048576,
			},
		},
		{
			name: "encrypted_packet",
			typ:  TypeEncryptedPacket,
			v: EncryptedPacket{
				Version:     Version,
				NetworkID:   "wn_1",
				SrcDeviceID: "dev_a",
				DstDeviceID: "dev_b",
				Payload:     "EEEE",
			},
		},
		{
			name: "heartbeat",
			typ:  TypeHeartbeat,
			v: Heartbeat{
				Timestamp: "2026-04-30T00:00:02Z",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := Encode(tc.v)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, gotType, err := Decode(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if gotType != tc.typ {
				t.Fatalf("type: got %q want %q", gotType, tc.typ)
			}
			// Encode again and compare bytes — proves the value survives the round trip.
			raw2, err := Encode(got)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if !bytes.Equal(raw, raw2) {
				t.Fatalf("round-trip mismatch:\nraw1=%s\nraw2=%s", raw, raw2)
			}
		})
	}
}

func TestDecodeUnknownTypeRejected(t *testing.T) {
	raw := []byte(`{"type":"who-knows"}`)
	if _, _, err := Decode(raw); err == nil {
		t.Fatalf("expected error for unknown type")
	}
}

func TestDecodeEmptyTypeRejected(t *testing.T) {
	raw := []byte(`{}`)
	if _, _, err := Decode(raw); err == nil {
		t.Fatalf("expected error for empty type")
	}
}

func TestDecodeMalformedJSONRejected(t *testing.T) {
	if _, _, err := Decode([]byte("not-json")); err == nil {
		t.Fatalf("expected error for malformed json")
	}
}

func TestEncodeFillsTypeWhenMissing(t *testing.T) {
	raw, err := Encode(ClientHello{NetworkID: "wn_1"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"client_hello"`) {
		t.Fatalf("encode did not fill Type: %s", raw)
	}
}

func TestEncodeRejectsUnknownType(t *testing.T) {
	if _, err := Encode(struct{}{}); err == nil {
		t.Fatalf("expected error for unknown go type")
	}
}

func TestProofTranscriptShape(t *testing.T) {
	hello := ClientHello{
		NetworkID:        "wn_1",
		DeviceID:         "dev_a",
		MachinePublicKey: "MK==",
		NodePublicKey:    "NK==",
		ClientNonce:      "CN==",
	}
	ch := RelayChallenge{
		RelayID:    "relay_local_1",
		RelayNonce: "RN==",
		ServerTime: "2026-04-30T00:00:01Z",
	}
	got := ProofTranscript(hello, ch)
	want := strings.Join([]string{
		"WAIRED-RELAY-PROOF-V1",
		"purpose=relay-session-handshake",
		"relay_id=relay_local_1",
		"network_id=wn_1",
		"device_id=dev_a",
		"machine_public_key=MK==",
		"node_public_key=NK==",
		"client_nonce=CN==",
		"relay_nonce=RN==",
		"server_time=2026-04-30T00:00:01Z",
		"",
	}, "\n")
	if string(got) != want {
		t.Fatalf("transcript mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestProofTranscriptStableAcrossSides(t *testing.T) {
	hello := ClientHello{
		NetworkID:        "wn_1",
		DeviceID:         "dev_a",
		MachinePublicKey: "MK==",
		NodePublicKey:    "NK==",
		ClientNonce:      "CN==",
	}
	ch := RelayChallenge{
		RelayID:    "relay_local_1",
		RelayNonce: "RN==",
		ServerTime: "2026-04-30T00:00:01Z",
	}
	a := ProofTranscript(hello, ch)
	b := ProofTranscript(hello, ch)
	if !bytes.Equal(a, b) {
		t.Fatalf("transcript not deterministic")
	}
}

func TestTicketRequestTranscriptShape(t *testing.T) {
	got := TicketRequestTranscript("dev_a", "2026-04-30T00:00:00Z")
	want := strings.Join([]string{
		"WAIRED-RELAY-TICKET-V1",
		"purpose=request-relay-ticket",
		"device_id=dev_a",
		"requested_at=2026-04-30T00:00:00Z",
		"",
	}, "\n")
	if string(got) != want {
		t.Fatalf("ticket transcript mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
