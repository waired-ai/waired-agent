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

// publicUsagePushRequest mirrors the CP's SelfPublicUsageRequest: the
// signed-request envelope fields FLAT alongside the embedded report, not
// nested. Re-declared here rather than imported, matching the other self
// endpoints.
type publicUsagePushRequest struct {
	DeviceID string `json:"device_id"`
	IssuedAt string `json:"issued_at"`
	Nonce    string `json:"nonce"`
	signer.PublicUsageReport
}

// PublicUsagePushResult mirrors the CP's response. Inserted is the
// number of ledger rows the CP wrote.
type PublicUsagePushResult struct {
	Status   string `json:"status"`
	Inserted int    `json:"inserted"`
}

// PublicUsageError carries the HTTP status of a rejected usage push so
// the caller can tell a definitively-pre-insert rejection from an
// ambiguous one. Retrying blindly would double-count: the endpoint is
// not idempotent, and the agent cannot correlate a retry with a prior
// attempt (see the private control-plane notes for why).
type PublicUsageError struct {
	Status int
	Code   string
	Body   string
}

func (e *PublicUsageError) Error() string {
	return fmt.Sprintf("controlclient: push public usage: %d: %s", e.Status, e.Body)
}

// Retryable reports whether the report provably never reached the
// ledger, so re-sending it cannot double-count.
//
// 429 qualifies: the CP's rate limiter runs before it builds any row.
// So does the clock-skew 401 ("issued_at_out_of_window"), which the CP
// raises while validating the envelope — long before any write — and
// which clears on its own once the clocks converge.
//
// Everything else is dropped, including 5xx: the CP surfaces a 500 from
// its batch insert, i.e. possibly AFTER the write committed, and this
// endpoint is not idempotent. Under-counting is the better failure mode
// for a ledger a user reads. A signature or replay 401 is permanent by
// nature and re-sending would never help.
func (e *PublicUsageError) Retryable() bool {
	if e == nil {
		return false
	}
	switch {
	case e.Status == http.StatusTooManyRequests:
		return true
	case e.Status == http.StatusUnauthorized && e.Code == "issued_at_out_of_window":
		return true
	default:
		return false
	}
}

// PushPublicUsage reports served Public Share inference to the CP via
// POST /v1/devices/self/public-usage (public share spec §12).
//
// Signed with the device's Ed25519 MachineKey — the full signed-request
// pattern, like the other self writes, and unlike the grant lifecycle
// endpoints which are bearer-only. The signature covers the exact
// marshalled bytes, so the request must be built once and sent as-is.
//
// The report carries aggregate counters only. No prompt or message
// content exists on this path at any point (spec §15-10).
func (c *Client) PushPublicUsage(ctx context.Context, deviceID string, report signer.PublicUsageReport, machineKey ed25519.PrivateKey) (PublicUsagePushResult, error) {
	if c.HTTP == nil {
		return PublicUsagePushResult{}, errors.New("controlclient: HTTP client is nil")
	}
	if len(machineKey) != ed25519.PrivateKeySize {
		return PublicUsagePushResult{}, errors.New("controlclient: machine key must be 64 bytes")
	}
	if len(report.Entries) == 0 {
		return PublicUsagePushResult{}, errors.New("controlclient: usage report has no entries")
	}
	body := publicUsagePushRequest{
		DeviceID:          deviceID,
		IssuedAt:          time.Now().UTC().Format(time.RFC3339),
		Nonce:             freshNonceB64(),
		PublicUsageReport: report,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return PublicUsagePushResult{}, err
	}
	sig := ed25519.Sign(machineKey, bodyBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/devices/self/public-usage", bytes.NewReader(bodyBytes))
	if err != nil {
		return PublicUsagePushResult{}, err
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
		return PublicUsagePushResult{}, err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return PublicUsagePushResult{}, &PublicUsageError{
			Status: resp.StatusCode,
			Code:   errorCodeOf(buf),
			Body:   string(buf),
		}
	}
	var out PublicUsagePushResult
	if len(buf) > 0 {
		// A 200 with an unparseable body still means the rows landed;
		// re-sending would double-count.
		_ = json.Unmarshal(buf, &out)
	}
	return out, nil
}

// errorCodeOf pulls the CP's machine-readable error code out of an
// error body, so retry classification does not depend on prose.
func errorCodeOf(buf []byte) string {
	var env struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(buf, &env); err != nil {
		return ""
	}
	switch {
	case env.Error.Code != "":
		return env.Error.Code
	case env.Error.Type != "":
		return env.Error.Type
	default:
		return env.Code
	}
}
