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

// upsertInferenceStatusRequest mirrors api.UpsertInferenceStatusRequest.
// We re-declare the body shape here instead of importing the api package
// to keep agent ↔ CP packages independent (matches endpoints.go).
type upsertInferenceStatusRequest struct {
	DeviceID string                `json:"device_id"`
	IssuedAt string                `json:"issued_at"`
	Nonce    string                `json:"nonce"`
	State    signer.InferenceState `json:"state"`
}

// inferenceStatusResponse mirrors api.UpsertInferenceStatusResponse.
type inferenceStatusResponse struct {
	Status         string `json:"status"`
	ContentChanged bool   `json:"content_changed"`
}

// PushInferenceStatus reports the device's local inference engine state
// to CP via POST /v1/devices/self/inference-status. The body is signed
// with the device's Ed25519 MachineKey.
//
// The 5 s probe cadence on the agent side aligns with the server's
// per-device rate limit (1/5s burst 5), so callers do not need to
// throttle further. Returns (contentChanged, err): contentChanged
// reports whether the push actually mutated the peer-visible
// payload — agents can use it to log "this push will wake peers"
// vs "this was a periodic heartbeat".
//
// state.LastCheck is required and must fall within ±60 s of the CP's
// clock; state.Type must be one of signer.InferenceTypeOllama / VLLM /
// None. Validation errors come back as a 400 with a typed error body.
func (c *Client) PushInferenceStatus(ctx context.Context, deviceID string, state signer.InferenceState, machineKey ed25519.PrivateKey) (bool, error) {
	if c.HTTP == nil {
		return false, errors.New("controlclient: HTTP client is nil")
	}
	if len(machineKey) != ed25519.PrivateKeySize {
		return false, errors.New("controlclient: machine key must be 64 bytes")
	}
	body := upsertInferenceStatusRequest{
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
		c.BaseURL+"/v1/devices/self/inference-status", bytes.NewReader(bodyBytes))
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
		return false, fmt.Errorf("controlclient: push inference status: %d: %s",
			resp.StatusCode, string(buf))
	}
	var out inferenceStatusResponse
	if len(buf) > 0 {
		if err := json.Unmarshal(buf, &out); err != nil {
			// 200 with an unparseable body shouldn't happen in
			// practice, but treat it as "we don't know if content
			// changed" rather than failing the push.
			return false, nil
		}
	}
	return out.ContentChanged, nil
}
