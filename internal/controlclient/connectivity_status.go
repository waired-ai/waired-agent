package controlclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// upsertConnectivityStatusRequest mirrors
// api.UpsertConnectivityStatusRequest. Re-declared here (rather than
// importing api) to keep agent ↔ CP packages independent, matching
// inference_status.go.
type upsertConnectivityStatusRequest struct {
	DeviceID string                   `json:"device_id"`
	IssuedAt string                   `json:"issued_at"`
	Nonce    string                   `json:"nonce"`
	State    signer.ConnectivityState `json:"state"`
}

// connectivityStatusResponse mirrors api.UpsertConnectivityStatusResponse.
type connectivityStatusResponse struct {
	Status         string `json:"status"`
	ContentChanged bool   `json:"content_changed"`
}

// PushConnectivityStatus reports the device's direct/relay connectivity
// summary to CP via POST /v1/devices/self/connectivity-status (#252).
// The body is signed with the device's Ed25519 MachineKey. Like
// PushInferenceStatus, the agent's push cadence is expected to align
// with the server's per-device rate limit (1/5s burst 5).
//
// state.LastCheck is required and must fall within ±60 s of the CP's
// clock. Returns (contentChanged, err).
func (c *Client) PushConnectivityStatus(ctx context.Context, deviceID string, state signer.ConnectivityState, machineKey ed25519.PrivateKey) (bool, error) {
	if c.HTTP == nil {
		return false, errors.New("controlclient: HTTP client is nil")
	}
	if len(machineKey) != ed25519.PrivateKeySize {
		return false, errors.New("controlclient: machine key must be 64 bytes")
	}
	body := upsertConnectivityStatusRequest{
		DeviceID: deviceID,
		IssuedAt: time.Now().UTC().Format(time.RFC3339),
		Nonce:    freshNonceB64(),
		State:    state,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	sig := ed25519.Sign(machineKey, bodyBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/devices/self/connectivity-status", bytes.NewReader(bodyBytes))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	bearer := c.BearerFn()
	if c.UseCustomAuthHeader {
		req.Header.Set("X-Waired-Agent-Bearer", bearer)
	} else {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("X-Waired-Body-Signature", base64.StdEncoding.EncodeToString(sig))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("controlclient: push connectivity status: %d: %s",
			resp.StatusCode, string(buf))
	}
	var out connectivityStatusResponse
	if len(buf) > 0 {
		if err := json.Unmarshal(buf, &out); err != nil {
			return false, nil
		}
	}
	return out.ContentChanged, nil
}
