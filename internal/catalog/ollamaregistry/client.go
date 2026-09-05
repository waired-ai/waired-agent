// Package ollamaregistry answers two questions about the public ollama
// model registry: does this tag exist, and does it bring something to
// render a request with?
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
	"encoding/json"
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

// templateMediaType is the manifest layer an ollama tag uses to carry its
// own prompt template.
const templateMediaType = "application/vnd.ollama.image.template"

// Rendering is what a tag brings for turning a request's messages into a
// prompt: the name of an ollama built-in renderer, a template layer of
// its own, or neither.
type Rendering struct {
	// Renderer is the tag config's "renderer" field — the ollama
	// built-in it asks for, e.g. "qwen3.8". Empty when the tag names
	// none.
	Renderer string
	// HasTemplate reports whether the manifest carries a template layer.
	HasTemplate bool
}

// Renders reports whether the tag brings either.
//
// Neither means ollama falls through to the chat template baked into the
// model file, and that is the case worth catching: those templates are
// written for the vendor's own API and reject shapes a coding agent
// sends. Measured on frob/qwen3.8-flash-next:125b-a6b-ud-q2_K_XL, whose
// manifest carries no template layer and whose config names no renderer:
// the engine answered 500 to a trailing system turn, to a system turn
// after a tool round-trip, and to a developer turn, raising the model
// template's own "System message must be at the beginning."
// (waired-agent#1192, and the same failure as #1035 / #1095).
//
// It is necessary, not sufficient. A tag can carry a renderer and still
// refuse a shape, which is why this never replaces the request-shape
// matrix — it only moves one specific, registry-visible cause of that
// red from the end of a GPU run to the seconds a manifest fetch takes.
func (r Rendering) Renders() bool { return r.Renderer != "" || r.HasTemplate }

// String renders the verdict for a test failure message.
func (r Rendering) String() string {
	switch {
	case r.Renderer != "" && r.HasTemplate:
		return fmt.Sprintf("renderer %q and a template layer", r.Renderer)
	case r.Renderer != "":
		return fmt.Sprintf("renderer %q", r.Renderer)
	case r.HasTemplate:
		return "a template layer"
	default:
		return "neither a renderer nor a template layer"
	}
}

// TagRendering reads what ref brings to render with.
//
// Two requests: the manifest names the layers, and the config blob it
// points at carries the renderer field. A tag that does not resolve is an
// error here rather than a zero value — the caller is expected to have
// established existence already (TagExists), so a 404 at this point means
// the two calls disagreed and silently returning "renders nothing" would
// report that as a rendering problem.
func (c *Client) TagRendering(ctx context.Context, ref string) (Rendering, error) {
	namespace, model, tag := SplitTag(ref)
	if model == "" {
		return Rendering{}, fmt.Errorf("ollamaregistry: %q names no model", ref)
	}

	var man struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			MediaType string `json:"mediaType"`
		} `json:"layers"`
	}
	manURL := fmt.Sprintf("%s/v2/%s/%s/manifests/%s", c.base(), namespace, model, tag)
	if err := c.getJSON(ctx, manURL, &man); err != nil {
		return Rendering{}, err
	}

	var out Rendering
	for _, l := range man.Layers {
		if l.MediaType == templateMediaType {
			out.HasTemplate = true
			break
		}
	}
	if man.Config.Digest == "" {
		// A manifest with no config blob names no renderer, which is a
		// complete answer rather than a failure to get one.
		return out, nil
	}

	var cfg struct {
		Renderer string `json:"renderer"`
	}
	cfgURL := fmt.Sprintf("%s/v2/%s/%s/blobs/%s", c.base(), namespace, model, man.Config.Digest)
	if err := c.getJSON(ctx, cfgURL, &cfg); err != nil {
		return Rendering{}, err
	}
	out.Renderer = cfg.Renderer
	return out, nil
}

// getJSON fetches url and decodes it, treating every non-2xx as a failure
// to answer. Bodies here are manifests and config blobs — kilobytes — so
// the read is bounded rather than streamed.
func (c *Client) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/json",
	}, ", "))

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("ollamaregistry: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("ollamaregistry: GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("ollamaregistry: GET %s: decode: %w", url, err)
	}
	return nil
}
