// Package controlclient implements the agent-side HTTP client for the
// Waired Control Plane. Step3 minimum core: a long-lived Network Map
// subscriber. Step4+ will add the full `waired init` enrollment flow,
// device-token refresh, and Node Key rotation.
package controlclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// Client holds the runtime config for talking to a single Control Plane.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	// BearerFn is invoked on every authenticated request to fetch the
	// current Device Access Token. Plumbed via a closure so the agent's
	// auto-refresh loop can rotate the token without re-creating any
	// of the long-lived consumers (network-map subscription, etc.).
	// Set by New() / NewWithBearer(); never nil after construction.
	BearerFn func() string

	// UseCustomAuthHeader, when true, sends the access token in the
	// X-Waired-Agent-Bearer header instead of Authorization. Set this
	// when the CP is fronted by a Cloud Run / IAP IAM gate that consumes
	// Authorization for its Google identity check; the caller is then
	// responsible for supplying an HTTP client whose transport injects
	// that gate token into Authorization. CP-side fallback lives in
	// internal/controlplane/api.agentBearer.
	UseCustomAuthHeader bool
}

// New constructs a Client with a static access token. Use this when
// the token is not expected to rotate (tests, one-shot tools).
func New(baseURL, accessToken string) *Client {
	tok := accessToken
	return NewWithBearer(baseURL, func() string { return tok })
}

// NewWithBearer constructs a Client whose bearer is fetched fresh on
// every authenticated request. Use this in long-running processes
// (`waired-agent`) so the refresh loop can swap the live token under
// active subscribers.
func NewWithBearer(baseURL string, bearerFn func() string) *Client {
	if bearerFn == nil {
		bearerFn = func() string { return "" }
	}
	return &Client{
		BaseURL:  baseURL,
		BearerFn: bearerFn,
		// Long-lived; no Timeout because the network-map stream is a
		// connection that intentionally stays open. Per-call deadlines
		// belong on the request context.
		HTTP: &http.Client{Transport: http.DefaultTransport},
	}
}

// SubscribeNetworkMap opens a long-poll connection to
// POST /v1/network-map/poll and streams NetworkMap frames into the
// returned channel. The caller cancels by cancelling ctx.
//
// On any read error or EOF, the function returns. The returned channel
// is closed before return so callers can `range` over it. Step3 minimum:
// no automatic reconnect - that lives in the agent loop in Step 5.
func (c *Client) SubscribeNetworkMap(ctx context.Context) (<-chan *signer.NetworkMap, <-chan error) {
	frames := make(chan *signer.NetworkMap)
	errs := make(chan error, 1)

	go func() {
		defer close(frames)
		defer close(errs)

		// Declare capabilities (spec §8.4): public-share-v1 tells the
		// CP this agent parses the Public Share map fields (Grant /
		// PublicShare / PublicCapacity), so the CP may emit them and
		// count this device as matchmaking-eligible. onboarding-v1
		// declares the waired#835 desired-state applier below, so the
		// CP may fold desired_engine / desired_model_id /
		// desired_benchmark_gen into this device's own Self entry.
		// CPs predating the intake ignore the body entirely.
		body := bytes.NewBufferString(`{"capabilities":["` +
			signer.CapabilityPublicShareV1 + `","` + signer.CapabilityOnboardingV1 + `"]}`)
		req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/network-map/poll", body)
		if err != nil {
			errs <- err
			return
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
			errs <- fmt.Errorf("network-map dial: %w", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			errs <- fmt.Errorf("network-map status %d: %s", resp.StatusCode, buf)
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		// Network Map JSON can grow with peers; bump the buffer.
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			var nm signer.NetworkMap
			if err := json.Unmarshal(scanner.Bytes(), &nm); err != nil {
				errs <- fmt.Errorf("network-map decode: %w", err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case frames <- &nm:
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
			errs <- fmt.Errorf("network-map scan: %w", err)
		}
	}()

	return frames, errs
}

// VerifyMap checks that nm carries a valid signature under the supplied
// CP signing key. Convenience wrapper over signer.VerifyNetworkMap.
func VerifyMap(cpSigningPubB64 string, nm *signer.NetworkMap) error {
	pub, err := decodeBase64(cpSigningPubB64)
	if err != nil {
		return fmt.Errorf("control-plane public key: %w", err)
	}
	if len(pub) != 32 {
		return fmt.Errorf("control-plane public key: expected 32 bytes, got %d", len(pub))
	}
	return signer.VerifyNetworkMap(pub, *nm)
}

// decodeBase64 accepts standard or URL-safe base64 (with or without padding).
func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("base64 (std or url) of an Ed25519 public key")
}
