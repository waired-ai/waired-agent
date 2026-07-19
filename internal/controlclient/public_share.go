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
)

// publicSharePushRequest mirrors the CP's SelfPublicShareRequest. Like
// the other self endpoints we re-declare the body shape instead of
// importing the CP package (matches inference_status.go).
type publicSharePushRequest struct {
	DeviceID string `json:"device_id"`
	IssuedAt string `json:"issued_at"`
	Nonce    string `json:"nonce"`
	Enabled  bool   `json:"enabled"`
	// MaxClients is public_max_clients; 0 = unset, letting the CP keep
	// its min(2, capacity−1) headroom default.
	MaxClients int `json:"max_clients"`
}

// PublicSharePushResult mirrors the CP's SelfPublicShareResponse.
// RevokedGrants is how many active grants an OFF transition revoked
// (surfaced to UIs as "N guests disconnected").
type PublicSharePushResult struct {
	Status        string `json:"status"`
	Enabled       bool   `json:"enabled"`
	MaxClients    int    `json:"max_clients"`
	RevokedGrants int    `json:"revoked_grants"`
}

// PushPublicShare syncs the provider Public Share toggle to the CP via
// POST /v1/devices/self/public-share (public share spec §4.1/§6). The
// body is signed with the device's Ed25519 MachineKey — the CP
// deliberately requires the full signed-request pattern here because
// enabling cross-account exposure must not be reachable with a stolen
// bearer alone.
//
// The CP rate-limits this endpoint per device (1/5s burst 5); callers
// push on operator transitions plus a bounded retry loop, which stays
// far under that budget.
func (c *Client) PushPublicShare(ctx context.Context, deviceID string, enabled bool, maxClients int, machineKey ed25519.PrivateKey) (PublicSharePushResult, error) {
	if c.HTTP == nil {
		return PublicSharePushResult{}, errors.New("controlclient: HTTP client is nil")
	}
	if len(machineKey) != ed25519.PrivateKeySize {
		return PublicSharePushResult{}, errors.New("controlclient: machine key must be 64 bytes")
	}
	body := publicSharePushRequest{
		DeviceID:   deviceID,
		IssuedAt:   time.Now().UTC().Format(time.RFC3339),
		Nonce:      freshNonceB64(),
		Enabled:    enabled,
		MaxClients: maxClients,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return PublicSharePushResult{}, err
	}
	sig := ed25519.Sign(machineKey, bodyBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/devices/self/public-share", bytes.NewReader(bodyBytes))
	if err != nil {
		return PublicSharePushResult{}, err
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
		return PublicSharePushResult{}, err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return PublicSharePushResult{}, fmt.Errorf("controlclient: push public share: %d: %s",
			resp.StatusCode, string(buf))
	}
	var out PublicSharePushResult
	if len(buf) > 0 {
		if err := json.Unmarshal(buf, &out); err != nil {
			// A 200 with an unparseable body still means the CP applied
			// the toggle; report success without the detail fields.
			return PublicSharePushResult{Enabled: enabled}, nil
		}
	}
	return out, nil
}
