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

// upsertSetupProgressRequest mirrors api.UpsertSetupProgressRequest.
// Re-declared here (rather than importing api) to keep agent ↔ CP
// packages independent, matching connectivity_status.go.
type upsertSetupProgressRequest struct {
	DeviceID string               `json:"device_id"`
	IssuedAt string               `json:"issued_at"`
	Nonce    string               `json:"nonce"`
	Progress signer.SetupProgress `json:"progress"`
}

// PushSetupProgress reports the device's NAVI-onboarding progress
// (waired#835 §5.2/§7) to CP via POST /v1/devices/self/setup-progress.
// The body is signed with the device's Ed25519 MachineKey, like
// PushConnectivityStatus. The caller's push cadence is expected to stay
// inside the server's per-device rate limit (1 push / 2 s, burst 10).
//
// progress.LastCheck is required and must fall within ±60 s of the
// CP's clock; the CP clamps steps to 16 entries / 512 B error detail
// and the whole body to 8 KiB.
func (c *Client) PushSetupProgress(ctx context.Context, deviceID string, progress signer.SetupProgress, machineKey ed25519.PrivateKey) error {
	if c.HTTP == nil {
		return errors.New("controlclient: HTTP client is nil")
	}
	if len(machineKey) != ed25519.PrivateKeySize {
		return errors.New("controlclient: machine key must be 64 bytes")
	}
	body := upsertSetupProgressRequest{
		DeviceID: deviceID,
		IssuedAt: time.Now().UTC().Format(time.RFC3339),
		Nonce:    freshNonceB64(),
		Progress: progress,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(machineKey, bodyBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/devices/self/setup-progress", bytes.NewReader(bodyBytes))
	if err != nil {
		return err
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
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("controlclient: push setup progress: %d: %s",
			resp.StatusCode, string(buf))
	}
	return nil
}
