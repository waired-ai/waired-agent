// Package ollamaregistry answers one question about the public ollama
// model registry: does this tag exist?
//
// Nothing else in this repository talks to the registry. `ollama pull`
// shells out from internal/download and discovers a bad tag on the
// user's machine, at the moment somebody is waiting for it — which is
// the failure mode waired-agent#824 is about. The catalog names 15
// ollama tags and, until now, the only check on any of them was that the
// string was non-empty.
//
// Read-only, unauthenticated, and deliberately tiny: this is catalog
// authoring support, not part of the serving path.
package ollamaregistry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the public registry origin.
const DefaultBaseURL = "https://registry.ollama.ai"

// Client talks to the registry. The zero value is usable.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// SplitTag splits an ollama reference into its namespace, model and tag.
//
// "qwen3.5:0.8b-q8_0" is the library namespace and reads as
// library/qwen3.5:0.8b-q8_0; "myorg/model:tag" names its own namespace.
// A reference with no tag means "latest", which is what the CLI does.
func SplitTag(ref string) (namespace, model, tag string) {
	namespace = "library"
	rest := ref
	if i := strings.Index(rest, "/"); i >= 0 {
		namespace, rest = rest[:i], rest[i+1:]
	}
	model, tag = rest, "latest"
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		model, tag = rest[:i], rest[i+1:]
	}
	return namespace, model, tag
}

// TagExists reports whether an ollama reference resolves in the registry.
//
// The manifest endpoint answers 200 for a tag that exists and 404 for one
// that does not. Any other status is a failure to ANSWER — never
// reported as absence, because that mistake would read a registry
// hiccup as "this model is gone" and take a live entry out of the
// catalog.
func (c *Client) TagExists(ctx context.Context, ref string) (bool, error) {
	namespace, model, tag := SplitTag(ref)
	if model == "" {
		return false, fmt.Errorf("ollamaregistry: %q names no model", ref)
	}
	url := fmt.Sprintf("%s/v2/%s/%s/manifests/%s", c.base(), namespace, model, tag)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	// The registry is OCI-shaped; ask for the manifest media types it
	// serves rather than whatever the default Accept implies.
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
	}, ", "))

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("ollamaregistry: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("ollamaregistry: GET %s: status %d", url, resp.StatusCode)
	}
}
