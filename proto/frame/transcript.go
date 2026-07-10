package frame

import "strings"

// ProofTranscript returns the canonical byte string both client and relay
// recompute and the client signs with its Machine Key during the
// waired-relay-v1 handshake. It implements the
// "signature_over_client_hello_and_relay_challenge" requirement from
// docs/specs/waired_control_plane_auth_spec.md §10.2.
//
//	WAIRED-RELAY-PROOF-V1
//	purpose=relay-session-handshake
//	relay_id=<relay_id>
//	network_id=<network_id>
//	device_id=<device_id>
//	machine_public_key=<std-base64>
//	node_public_key=<std-base64>
//	client_nonce=<std-base64>
//	relay_nonce=<std-base64>
//	server_time=<rfc3339>
//	(trailing newline)
//
// A text transcript is preferred over canonical JSON of the two frames
// because it sidesteps any number-formatting ambiguity in
// signer.CanonicalJSON, mirrors the audited MachineSignatureTranscript
// pattern in internal/controlplane/api/enrollment.go, and embeds field
// names in the signed bytes so two distinct (hello, challenge) pairs
// cannot collide.
func ProofTranscript(hello ClientHello, ch RelayChallenge) []byte {
	var b strings.Builder
	b.WriteString("WAIRED-RELAY-PROOF-V1\n")
	b.WriteString("purpose=relay-session-handshake\n")
	b.WriteString("relay_id=")
	b.WriteString(ch.RelayID)
	b.WriteByte('\n')
	b.WriteString("network_id=")
	b.WriteString(hello.NetworkID)
	b.WriteByte('\n')
	b.WriteString("device_id=")
	b.WriteString(hello.DeviceID)
	b.WriteByte('\n')
	b.WriteString("machine_public_key=")
	b.WriteString(hello.MachinePublicKey)
	b.WriteByte('\n')
	b.WriteString("node_public_key=")
	b.WriteString(hello.NodePublicKey)
	b.WriteByte('\n')
	b.WriteString("client_nonce=")
	b.WriteString(hello.ClientNonce)
	b.WriteByte('\n')
	b.WriteString("relay_nonce=")
	b.WriteString(ch.RelayNonce)
	b.WriteByte('\n')
	b.WriteString("server_time=")
	b.WriteString(ch.ServerTime)
	b.WriteByte('\n')
	return []byte(b.String())
}

// TicketRequestTranscript is what the agent signs with its Machine Key
// when it asks the Control Plane to mint a relay_ticket via
// POST /v1/relays/tickets. Mirrors the MachineSignatureTranscript pattern
// from enrollment but scoped to the ticket request.
//
//	WAIRED-RELAY-TICKET-V1
//	purpose=request-relay-ticket
//	device_id=<device_id>
//	requested_at=<rfc3339>
//	(trailing newline)
func TicketRequestTranscript(deviceID, requestedAt string) []byte {
	var b strings.Builder
	b.WriteString("WAIRED-RELAY-TICKET-V1\n")
	b.WriteString("purpose=request-relay-ticket\n")
	b.WriteString("device_id=")
	b.WriteString(deviceID)
	b.WriteByte('\n')
	b.WriteString("requested_at=")
	b.WriteString(requestedAt)
	b.WriteByte('\n')
	return []byte(b.String())
}
