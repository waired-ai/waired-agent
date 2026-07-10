package controlclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/devicekeys"
)

// RotateNodeKeyParams is the input to Client.RotateNodeKey, mirroring the
// wire of POST /v1/devices/{device_id}/node-key/rotate (auth spec §13.2).
type RotateNodeKeyParams struct {
	DeviceID  string
	NetworkID string
	// OldNodePublicKey / NewNodePublicKey are std-base64 X25519 public
	// keys. Old must match the device's current key; new is the freshly
	// generated key the agent is rotating to.
	OldNodePublicKey string
	NewNodePublicKey string
	// MachineKey signs the rotation transcript, proving the request comes
	// from the device's long-lived install identity.
	MachineKey *devicekeys.MachineKey
}

// RotateNodeKeyResult is the decoded server response.
type RotateNodeKeyResult struct {
	DeviceCertificateJSON []byte
	OldNodeKeyValidUntil  time.Time
	NodeKeyExpiresAt      time.Time
	NetworkMapEpoch       int64
}

// ErrNodeKeyMismatch — the CP rejected the rotation because
// old_node_public_key no longer matches the device's current key (stale
// view or a concurrent rotation). The agent should re-poll the network
// map and retry against the current key.
var ErrNodeKeyMismatch = errors.New("controlclient: node key mismatch — re-poll map and retry")

// RotateNodeKey posts a Node Key rotation and returns the new certificate
// and lifecycle stamps. The request is authenticated by the device access
// token (BearerFn) and signed with the Machine Key. The caller is
// responsible for persisting the new key + certificate on success.
func (c *Client) RotateNodeKey(ctx context.Context, p RotateNodeKeyParams) (*RotateNodeKeyResult, error) {
	if c.HTTP == nil {
		return nil, errors.New("controlclient: HTTP client is nil")
	}
	if p.DeviceID == "" || p.NetworkID == "" || p.OldNodePublicKey == "" || p.NewNodePublicKey == "" {
		return nil, errors.New("controlclient: RotateNodeKey: missing required fields")
	}
	if p.MachineKey == nil {
		return nil, errors.New("controlclient: RotateNodeKey: MachineKey is required")
	}

	nonceBytes := make([]byte, 16)
	if _, err := readRandom(nonceBytes); err != nil {
		return nil, err
	}
	nonce := base64.StdEncoding.EncodeToString(nonceBytes)

	transcript := rotateNodeKeyTranscript(p.DeviceID, p.NetworkID, p.OldNodePublicKey, p.NewNodePublicKey, nonce)
	sig := p.MachineKey.Sign(transcript)

	body, _ := json.Marshal(map[string]string{
		"old_node_public_key": p.OldNodePublicKey,
		"new_node_public_key": p.NewNodePublicKey,
		"machine_signature":   base64.StdEncoding.EncodeToString(sig),
		"client_nonce":        nonce,
	})

	url := c.BaseURL + "/v1/devices/" + p.DeviceID + "/node-key/rotate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	bearer := c.BearerFn()
	if c.UseCustomAuthHeader {
		req.Header.Set("X-Waired-Agent-Bearer", bearer)
	} else {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rotate node key: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, classifyRotateError(resp.StatusCode, respBody)
	}

	var rr struct {
		DeviceCertificate    json.RawMessage `json:"device_certificate"`
		OldNodeKeyValidUntil string          `json:"old_node_key_valid_until"`
		NodeKeyExpiresAt     string          `json:"node_key_expires_at"`
		NetworkMapEpoch      int64           `json:"network_map_epoch"`
	}
	if err := json.Unmarshal(respBody, &rr); err != nil {
		return nil, fmt.Errorf("decode rotate response: %w", err)
	}
	validUntil, _ := time.Parse(time.RFC3339, rr.OldNodeKeyValidUntil)
	expiresAt, _ := time.Parse(time.RFC3339, rr.NodeKeyExpiresAt)
	return &RotateNodeKeyResult{
		DeviceCertificateJSON: []byte(rr.DeviceCertificate),
		OldNodeKeyValidUntil:  validUntil,
		NodeKeyExpiresAt:      expiresAt,
		NetworkMapEpoch:       rr.NetworkMapEpoch,
	}, nil
}

// classifyRotateError maps the CP's structured error envelope to a
// sentinel where the agent needs to branch (node_key_mismatch → re-poll
// and retry); everything else is surfaced verbatim.
func classifyRotateError(status int, body []byte) error {
	var env struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	if status == http.StatusConflict && env.Error.Type == "node_key_mismatch" {
		return ErrNodeKeyMismatch
	}
	return fmt.Errorf("rotate node key: status %d (%s): %s", status, env.Error.Type, body)
}

// rotateNodeKeyTranscript MUST match
// internal/controlplane/api.RotateNodeKeyTranscript byte-for-byte.
// Duplicated to avoid an agent -> CP package import (same convention as
// refreshTranscript).
func rotateNodeKeyTranscript(deviceID, networkID, oldNodePubB64, newNodePubB64, clientNonce string) []byte {
	var b bytes.Buffer
	b.WriteString("WAIRED-MACHINE-SIGNATURE-V1\n")
	b.WriteString("purpose=node-key-rotation\n")
	b.WriteString("device_id=")
	b.WriteString(deviceID)
	b.WriteByte('\n')
	b.WriteString("network_id=")
	b.WriteString(networkID)
	b.WriteByte('\n')
	b.WriteString("old_node_public_key=")
	b.WriteString(oldNodePubB64)
	b.WriteByte('\n')
	b.WriteString("new_node_public_key=")
	b.WriteString(newNodePubB64)
	b.WriteByte('\n')
	b.WriteString("client_nonce=")
	b.WriteString(clientNonce)
	b.WriteByte('\n')
	return b.Bytes()
}
