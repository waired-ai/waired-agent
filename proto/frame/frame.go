// Package frame defines the JSON wire format the waired-relay-v1 subprotocol
// uses on the WebSocket between agent and relay. Frames mirror
// docs/specs/waired_control_plane_auth_spec.md §10.2 and §10.4.
package frame

import (
	"encoding/json"
	"fmt"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// Subprotocol is the value the WebSocket Sec-WebSocket-Protocol header
// must carry on both client and server sides.
const Subprotocol = "waired-relay-v1"

// Frame type discriminators.
const (
	TypeClientHello      = "client_hello"
	TypeRelayChallenge   = "relay_challenge"
	TypeClientProof      = "client_proof"
	TypeRelayEstablished = "relay_established"
	TypeEncryptedPacket  = "encrypted_packet"
	TypeHeartbeat        = "heartbeat"
)

// Version is the waired-relay-v1 protocol version baked into ClientHello
// and EncryptedPacket. Bump if the wire format changes incompatibly.
const Version = 1

// Envelope is the minimum struct used to peek at a frame's type before
// re-decoding into a concrete struct.
type Envelope struct {
	Type string `json:"type"`
}

// ClientHello is the first frame the client sends after WebSocket upgrade.
type ClientHello struct {
	Type              string                   `json:"type"`
	Version           int                      `json:"version"`
	NetworkID         string                   `json:"network_id"`
	DeviceID          string                   `json:"device_id"`
	NodePublicKey     string                   `json:"node_public_key"`
	MachinePublicKey  string                   `json:"machine_public_key"`
	DeviceCertificate signer.DeviceCertificate `json:"device_certificate"`
	ClientNonce       string                   `json:"client_nonce"`
	SupportedFrames   []string                 `json:"supported_frames"`
}

// RelayChallenge is the relay's response to ClientHello.
type RelayChallenge struct {
	Type       string `json:"type"`
	RelayID    string `json:"relay_id"`
	RelayNonce string `json:"relay_nonce"`
	ServerTime string `json:"server_time"`
}

// ClientProof carries the Ed25519 signature over ProofTranscript.
type ClientProof struct {
	Type         string `json:"type"`
	SignatureAlg string `json:"signature_alg"`
	Signature    string `json:"signature"`
}

// RelayEstablished is the relay's accept frame after verifying the proof.
type RelayEstablished struct {
	Type                     string `json:"type"`
	RelaySessionID           string `json:"relay_session_id"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	MaxFrameSizeBytes        int    `json:"max_frame_size_bytes"`
}

// EncryptedPacket carries one WireGuard UDP datagram between two peers.
// The relay forwards based on (NetworkID, DstDeviceID) only - it does not
// decrypt Payload.
type EncryptedPacket struct {
	Type         string `json:"type"`
	Version      int    `json:"version"`
	NetworkID    string `json:"network_id"`
	SrcDeviceID  string `json:"src_device_id"`
	DstDeviceID  string `json:"dst_device_id"`
	SrcNodeKeyID string `json:"src_node_key_id,omitempty"`
	DstNodeKeyID string `json:"dst_node_key_id,omitempty"`
	PacketID     string `json:"packet_id,omitempty"`
	Payload      string `json:"payload"`
}

// Heartbeat is the keep-alive frame both sides may send.
type Heartbeat struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
}

// Encode marshals a frame value as JSON. The struct's Type field must be
// non-empty so misuse (e.g., zero-valued struct) is caught early.
func Encode(v any) ([]byte, error) {
	switch f := v.(type) {
	case ClientHello:
		if f.Type == "" {
			f.Type = TypeClientHello
		}
		return json.Marshal(f)
	case RelayChallenge:
		if f.Type == "" {
			f.Type = TypeRelayChallenge
		}
		return json.Marshal(f)
	case ClientProof:
		if f.Type == "" {
			f.Type = TypeClientProof
		}
		return json.Marshal(f)
	case RelayEstablished:
		if f.Type == "" {
			f.Type = TypeRelayEstablished
		}
		return json.Marshal(f)
	case EncryptedPacket:
		if f.Type == "" {
			f.Type = TypeEncryptedPacket
		}
		return json.Marshal(f)
	case Heartbeat:
		if f.Type == "" {
			f.Type = TypeHeartbeat
		}
		return json.Marshal(f)
	default:
		return nil, fmt.Errorf("frame: unknown frame type %T", v)
	}
}

// Decode peeks at the JSON envelope's type, then unmarshals into the
// matching struct. Returns the concrete value, the type string, or an
// error including unknown-type rejection.
func Decode(raw []byte) (any, string, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, "", fmt.Errorf("frame: decode envelope: %w", err)
	}
	switch env.Type {
	case TypeClientHello:
		var f ClientHello
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, env.Type, fmt.Errorf("frame: decode %s: %w", env.Type, err)
		}
		return f, env.Type, nil
	case TypeRelayChallenge:
		var f RelayChallenge
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, env.Type, fmt.Errorf("frame: decode %s: %w", env.Type, err)
		}
		return f, env.Type, nil
	case TypeClientProof:
		var f ClientProof
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, env.Type, fmt.Errorf("frame: decode %s: %w", env.Type, err)
		}
		return f, env.Type, nil
	case TypeRelayEstablished:
		var f RelayEstablished
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, env.Type, fmt.Errorf("frame: decode %s: %w", env.Type, err)
		}
		return f, env.Type, nil
	case TypeEncryptedPacket:
		var f EncryptedPacket
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, env.Type, fmt.Errorf("frame: decode %s: %w", env.Type, err)
		}
		return f, env.Type, nil
	case TypeHeartbeat:
		var f Heartbeat
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, env.Type, fmt.Errorf("frame: decode %s: %w", env.Type, err)
		}
		return f, env.Type, nil
	case "":
		return nil, "", fmt.Errorf("frame: empty type")
	default:
		return nil, env.Type, fmt.Errorf("frame: unknown type %q", env.Type)
	}
}
