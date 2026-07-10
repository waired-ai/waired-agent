package controlclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CandidateAdvertise is one entry in AdvertiseEndpoints.
type CandidateAdvertise struct {
	Addr     string `json:"addr"`
	Kind     string `json:"kind"`
	Priority int    `json:"priority,omitempty"`
}

// AdvertiseEndpointsRequest mirrors api.AdvertiseEndpointsRequest. We
// re-declare the body shape here instead of importing the api package
// to keep agent ↔ CP packages independent.
type advertiseEndpointsRequest struct {
	DeviceID   string               `json:"device_id"`
	IssuedAt   string               `json:"issued_at"`
	Nonce      string               `json:"nonce"`
	Candidates []CandidateAdvertise `json:"candidates"`
}

// AdvertiseEndpoints pushes a fresh candidate set to CP via
// POST /v1/devices/self/endpoints. The body is signed with the device's
// Ed25519 MachineKey; CP verifies by looking up the device's
// MachinePublicKey from store.
//
// The agent calls this after disco has observed a fresh public
// endpoint or NAT-type change. It's rate-limited server-side to 1
// update / 5s with burst 3, so the agent need not throttle further.
func (c *Client) AdvertiseEndpoints(ctx context.Context, deviceID string, candidates []CandidateAdvertise, machineKey ed25519.PrivateKey) error {
	if c.HTTP == nil {
		return errors.New("controlclient: HTTP client is nil")
	}
	if len(machineKey) != ed25519.PrivateKeySize {
		return errors.New("controlclient: machine key must be 64 bytes")
	}
	body := advertiseEndpointsRequest{
		DeviceID:   deviceID,
		IssuedAt:   time.Now().UTC().Format(time.RFC3339),
		Nonce:      freshNonceB64(),
		Candidates: candidates,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(machineKey, bodyBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/devices/self/endpoints", bytes.NewReader(bodyBytes))
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
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("controlclient: advertise endpoints status %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

func freshNonceB64() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}
