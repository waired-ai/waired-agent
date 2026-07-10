package controlclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Revoke deregisters this device server-side by moving it to the terminal
// revoked state: POST /v1/devices/self/revoke, authenticated by the
// device's own access token. The CP revokes the device (removed from the
// admin device list), revokes its tokens, and drops it from peers' maps.
//
// This is the uninstall-time counterpart to Logout: where Logout leaves
// the device in reauth_required (recoverable via `waired init`), Revoke is
// terminal — appropriate when the software is being removed for good.
//
// A 401 is treated as success: it means the access token no longer
// resolves (the device was already revoked / its tokens revoked), which is
// the same end state Revoke aims for. Network/5xx errors are returned so
// the caller can warn that the device may still be active.
func (c *Client) Revoke(ctx context.Context) error {
	if c.HTTP == nil {
		return fmt.Errorf("controlclient: HTTP client is nil")
	}
	url := c.BaseURL + "/v1/devices/self/revoke"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	bearer := c.BearerFn()
	if c.UseCustomAuthHeader {
		req.Header.Set("X-Waired-Agent-Bearer", bearer)
	} else {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		// Token no longer resolves — already revoked. Same end state.
		return nil
	default:
		return fmt.Errorf("revoke: status %d: %s", resp.StatusCode, body)
	}
}
