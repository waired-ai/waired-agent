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

	"github.com/waired-ai/waired-agent/proto/catalog"
)

// maxCustomModelsBody bounds the fetched set: at most 10 own models and a
// team's routing copies, a few KB each.
const maxCustomModelsBody = 4 << 20

// customModelsFetchRequest mirrors api.CustomModelsFetchRequest.
type customModelsFetchRequest struct {
	DeviceID     string `json:"device_id"`
	IssuedAt     string `json:"issued_at"`
	Nonce        string `json:"nonce"`
	HaveRevision string `json:"have_revision,omitempty"`
}

// FetchCustomModels asks the control plane for the custom models this
// device may use (waired-ai/waired#1473): the owner's imports and the
// routing copies of what the owner's teammates imported. The body is
// signed with the device's MachineKey, like PushSetupProgress. The caller
// validates every manifest before using it (catalog.CustomSource does).
func (c *Client) FetchCustomModels(ctx context.Context, deviceID, haveRevision string, machineKey ed25519.PrivateKey) (catalog.CustomModelSet, error) {
	var set catalog.CustomModelSet
	if c.HTTP == nil {
		return set, errors.New("controlclient: HTTP client is nil")
	}
	if len(machineKey) != ed25519.PrivateKeySize {
		return set, errors.New("controlclient: machine key must be 64 bytes")
	}
	bodyBytes, err := json.Marshal(customModelsFetchRequest{
		DeviceID:     deviceID,
		IssuedAt:     time.Now().UTC().Format(time.RFC3339),
		Nonce:        freshNonceB64(),
		HaveRevision: haveRevision,
	})
	if err != nil {
		return set, err
	}
	sig := ed25519.Sign(machineKey, bodyBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/devices/self/custom-models", bytes.NewReader(bodyBytes))
	if err != nil {
		return set, err
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
		return set, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return set, fmt.Errorf("controlclient: fetch custom models: %d: %s", resp.StatusCode, string(buf))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCustomModelsBody)).Decode(&set); err != nil {
		return set, fmt.Errorf("controlclient: fetch custom models: %w", err)
	}
	return set, nil
}
