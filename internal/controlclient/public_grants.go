package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Public Share grant lifecycle calls (waired#821 second half, spec §6).
// Bearer-authenticated with plain JSON bodies — grants are CP-side
// authorization state, not self-asserted device state, so the signed
// envelope machinery buys nothing here (precedent: Revoke/Logout).

// PublicGrant mirrors the CP's PublicGrantInfo.
type PublicGrant struct {
	GrantID           string `json:"grant_id"`
	ProviderDeviceID  string `json:"provider_device_id"`
	ProviderPseudonym string `json:"provider_pseudonym"`
	ExpiresAt         string `json:"expires_at"` // RFC3339
	// Created is true when this acquire call minted the grant (false =
	// it was already part of the device's active set).
	Created bool `json:"created"`
}

// AcquirePublicGrantsRequest mirrors the CP's PublicGrantAcquireRequest.
type AcquirePublicGrantsRequest struct {
	Class          string `json:"class"`            // "" | "main" | "sub"
	MinQualityTier int    `json:"min_quality_tier"` // 0 = no floor
	Want           int    `json:"want"`             // 0 → server default (3)
	ConsentVersion int    `json:"consent_version"`  // accepted warning version, ≥1
}

// AcquirePublicGrantsResponse carries the device's FULL current active
// set (existing + newly created) — the renew scheduler's source of
// truth, self-correcting across agent restarts.
type AcquirePublicGrantsResponse struct {
	Status string        `json:"status"`
	Grants []PublicGrant `json:"grants"`
}

// RenewPublicGrantsResponse: ids absent from Renewed were not extended
// (lost ownership / expired / provider went dark) — drop them locally.
type RenewPublicGrantsResponse struct {
	Status    string   `json:"status"`
	Renewed   []string `json:"renewed"`
	ExpiresAt string   `json:"expires_at"` // shared new expiry, RFC3339
}

// ReleasePublicGrantsResponse acknowledges an idempotent release.
type ReleasePublicGrantsResponse struct {
	Status   string   `json:"status"`
	Released []string `json:"released"`
}

// Typed sentinels for the two backoff-worthy rejections; callers use
// errors.Is and back off instead of retrying on the next tick.
var (
	ErrPublicShareNotEligible = errors.New("controlclient: public share: not eligible")
	ErrPublicShareRateLimited = errors.New("controlclient: public share: rate limited")
)

// AcquirePublicGrants requests up to req.Want grants (K=3 cap CP-side).
// A 200 with an empty Grants list means "eligible but no candidates
// right now" — back off, not an error.
func (c *Client) AcquirePublicGrants(ctx context.Context, req AcquirePublicGrantsRequest) (AcquirePublicGrantsResponse, error) {
	var out AcquirePublicGrantsResponse
	err := c.postPublicGrants(ctx, "/v1/public-share/grants/acquire", req, &out)
	return out, err
}

// RenewPublicGrants extends the given grants to now+TTL. Callers cap
// the batch at 16 ids (the CP clamps harder batches).
func (c *Client) RenewPublicGrants(ctx context.Context, grantIDs []string) (RenewPublicGrantsResponse, error) {
	var out RenewPublicGrantsResponse
	err := c.postPublicGrants(ctx, "/v1/public-share/grants/renew",
		map[string][]string{"grant_ids": grantIDs}, &out)
	return out, err
}

// ReleasePublicGrants revokes the given grants (idempotent; used on
// shutdown and mode-off).
func (c *Client) ReleasePublicGrants(ctx context.Context, grantIDs []string) (ReleasePublicGrantsResponse, error) {
	var out ReleasePublicGrantsResponse
	err := c.postPublicGrants(ctx, "/v1/public-share/grants/release",
		map[string][]string{"grant_ids": grantIDs}, &out)
	return out, err
}

func (c *Client) postPublicGrants(ctx context.Context, path string, body, out any) error {
	if c.HTTP == nil {
		return errors.New("controlclient: HTTP client is nil")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
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
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrPublicShareNotEligible, string(buf))
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrPublicShareRateLimited, string(buf))
	default:
		return fmt.Errorf("controlclient: %s: %d: %s", path, resp.StatusCode, string(buf))
	}
	if len(buf) > 0 {
		if err := json.Unmarshal(buf, out); err != nil {
			return fmt.Errorf("controlclient: %s: decode: %w", path, err)
		}
	}
	return nil
}
