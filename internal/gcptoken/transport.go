// Package gcptoken provides an http.RoundTripper that injects a Google
// identity token (audience pinned at construction time) into the
// Authorization header. Used when both the waired CLI and waired-agent
// need to talk to a Cloud Run / IAP IAM-gated Control Plane.
//
// The token is fetched on demand from the GCE metadata server and
// cached for 50 minutes (tokens are valid for one hour).
package gcptoken

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// IdentityTokenTransport injects a Google identity token into
// Authorization. If the request already carries an Authorization
// header, the existing value wins (i.e., this is purely additive).
type IdentityTokenTransport struct {
	Base     http.RoundTripper
	Audience string

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// New returns a transport for the given audience. Base may be nil; in
// that case http.DefaultTransport is used.
func New(audience string, base http.RoundTripper) *IdentityTokenTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &IdentityTokenTransport{Base: base, Audience: audience}
}

func (t *IdentityTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == "" {
		token, err := t.fetch(req.Context())
		if err != nil {
			return nil, err
		}
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return t.Base.RoundTrip(req)
}

func (t *IdentityTokenTransport) fetch(ctx context.Context) (string, error) {
	t.mu.Lock()
	if t.token != "" && time.Now().Before(t.expiresAt) {
		tok := t.token
		t.mu.Unlock()
		return tok, nil
	}
	t.mu.Unlock()

	tok, err := FetchIdentityToken(ctx, t.Audience)
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	t.token = tok
	t.expiresAt = time.Now().Add(50 * time.Minute)
	t.mu.Unlock()
	return tok, nil
}

// Probe is a one-shot fetch with a short timeout. Useful for deciding
// whether the GCE metadata server is reachable before installing the
// transport.
func (t *IdentityTokenTransport) Probe(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := t.fetch(probeCtx)
	return err == nil
}

// FetchIdentityToken queries the GCE metadata server for an identity
// token bound to the given audience.
func FetchIdentityToken(ctx context.Context, audience string) (string, error) {
	u := "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=" + url.QueryEscape(audience)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return "", fmt.Errorf("metadata server: status %d: %s", resp.StatusCode, buf)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	return strings.TrimSpace(string(body)), nil
}
