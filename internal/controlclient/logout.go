package controlclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Logout deauthenticates this device server-side (#115, `tailscale logout`
// parity): POST /v1/devices/self/logout, authenticated by the device's own
// access token. The CP flips the device to reauth_required, revokes its
// tokens, and drops it from peers' maps.
//
// A 401 is treated as success: it means the access token no longer
// resolves (the device was already deauthed / its tokens revoked), which
// is the same end state logout aims for. Network/5xx errors are returned
// so the CLI can warn the user that the device may still be active.
func (c *Client) Logout(ctx context.Context) error {
	if c.HTTP == nil {
		return fmt.Errorf("controlclient: HTTP client is nil")
	}
	url := c.BaseURL + "/v1/devices/self/logout"
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
		return fmt.Errorf("logout: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		// Token no longer resolves — already deauthed. Same end state.
		return nil
	default:
		return fmt.Errorf("logout: status %d: %s", resp.StatusCode, body)
	}
}
