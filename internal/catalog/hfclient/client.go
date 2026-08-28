// Package hfclient is a small read-only HTTP client for the public Hugging
// Face Hub API. It fetches a model's config.json (the architecture inputs the
// scoring package needs) and lists recent models for the discovery watcher.
//
// It is deliberately separate from internal/download/hf.go — that path shells
// out to `huggingface-cli` to pull multi-GB weights, whereas this is a tiny
// JSON client for catalog authoring. No authentication is required for public
// repos; an optional token raises rate limits and reaches gated metadata.
package hfclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
)

// DefaultBaseURL is the public Hugging Face Hub origin.
const DefaultBaseURL = "https://huggingface.co"

// ErrNotFound is returned when the Hub responds 404 (missing repo/revision/file).
var ErrNotFound = errors.New("hfclient: not found")

// Client talks to the Hugging Face Hub. The zero value is not usable; call New.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Token, when set, is sent as a Bearer credential (optional: higher rate
	// limits / gated metadata). Never required for the public catalog flow.
	Token string
	// RetryBackoff is the wait before a single retry on HTTP 429. Zero means
	// the 2s default; negative disables the retry (used in tests).
	RetryBackoff time.Duration
}

// New returns a Client pointed at the public Hub with a 30s timeout.
func New() *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// get issues a GET and returns the body bytes, mapping 404 to ErrNotFound.
func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("hfclient: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 16 MiB cap
	if err != nil {
		return nil, fmt.Errorf("hfclient: read %s: %w", url, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", url, ErrNotFound)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("hfclient: GET %s: status %d: %s", url, resp.StatusCode, truncate(body, 200))
	}
	return body, nil
}

// FetchConfig fetches and decodes config.json for repoID at revision (default
// "main"). It returns the decoded ArchConfig and the raw bytes (for provenance
// / cross-checking). Unknown config fields are ignored.
func (c *Client) FetchConfig(ctx context.Context, repoID, revision string) (scoring.ArchConfig, []byte, error) {
	if revision == "" {
		revision = "main"
	}
	url := fmt.Sprintf("%s/%s/resolve/%s/config.json", c.base(), repoID, revision)
	body, err := c.get(ctx, url)
	if err != nil {
		return scoring.ArchConfig{}, nil, err
	}
	var cfg scoring.ArchConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return scoring.ArchConfig{}, body, fmt.Errorf("hfclient: parse config.json for %s: %w", repoID, err)
	}
	return cfg, body, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// RepoExists reports whether repoID resolves on the Hub at revision
// (default "main").
//
// It asks for config.json with a HEAD, because that is the file every
// model repo has and the one FetchConfig already relies on. A HEAD is
// enough: the question is existence, not content, and a repo of weights
// is not worth downloading to answer it.
//
// 401 counts as absent, and that is the whole reason this is a separate
// method rather than a call to get(). The Hub answers a missing OR
// private repo with 401, not 404 — it declines to distinguish "there is
// nothing here" from "there is something here you may not see" — so
// get(), which maps only 404 to ErrNotFound, reports a nonexistent repo
// as a generic status error. That is how Qwen/Qwen3.6-27B-AWQ shipped in
// the catalog for months without existing (#824): every check anyone
// wrote was a check on the STRING.
//
// A repo that exists but is gated behind a licence click also answers
// 401 without a token. That is the right answer here too: a variant this
// project cannot fetch without an interactive acceptance is not one it
// can serve, and the catalog should not name it.
func (c *Client) RepoExists(ctx context.Context, repoID, revision string) (bool, error) {
	if revision == "" {
		revision = "main"
	}
	url := fmt.Sprintf("%s/%s/resolve/%s/config.json", c.base(), repoID, revision)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("hfclient: HEAD %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		// 2xx, and 3xx: the Hub answers a resolve/ URL with a redirect
		// to the CDN, which is what a present file looks like.
		return true, nil
	case resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		return false, nil
	default:
		// Anything else — a 429, a 5xx, a captive portal — is a failure
		// to ANSWER the question, and must never be reported as "the
		// repo does not exist". That mistake would delete a live model
		// from the catalog on a bad afternoon.
		return false, fmt.Errorf("hfclient: HEAD %s: status %d", url, resp.StatusCode)
	}
}
