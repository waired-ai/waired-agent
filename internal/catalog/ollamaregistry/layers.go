package ollamaregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Layer media types an ollama manifest uses for the parts the sizing
// reads.
const (
	modelMediaType     = "application/vnd.ollama.image.model"
	projectorMediaType = "application/vnd.ollama.image.projector"
	paramsMediaType    = "application/vnd.ollama.image.params"
)

// TagLayers is what a tag's manifest says about the files a pull brings.
type TagLayers struct {
	// ModelDigest / ModelBytes name the GGUF weights blob.
	ModelDigest string
	ModelBytes  int64
	// ProjectorBytes is the multimodal projector blob, 0 when the tag
	// carries none. ollama offloads it to the GPU beside the model and
	// raises llama.cpp's fit target by its size (llm/llama_server.go,
	// mmprojFitTargetMiB, ollama v0.33.3).
	ProjectorBytes int64
	// Params is the tag's params layer (e.g. draft_num_predict), nil when
	// the tag carries none.
	Params map[string]any
}

// Layers reads ref's manifest and its params layer.
func (c *Client) Layers(ctx context.Context, ref string) (TagLayers, error) {
	base, namespace, model, tag, err := c.splitRef(ref)
	if err != nil {
		return TagLayers{}, err
	}
	var man struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	manURL := fmt.Sprintf("%s/v2/%s/%s/manifests/%s", base, namespace, model, tag)
	if err := c.getJSON(ctx, manURL, &man); err != nil {
		return TagLayers{}, err
	}
	var out TagLayers
	paramsDigest := ""
	for _, l := range man.Layers {
		switch l.MediaType {
		case modelMediaType:
			out.ModelDigest, out.ModelBytes = l.Digest, l.Size
		case projectorMediaType:
			out.ProjectorBytes += l.Size
		case paramsMediaType:
			paramsDigest = l.Digest
		}
	}
	if out.ModelDigest == "" {
		return TagLayers{}, fmt.Errorf("ollamaregistry: %s: the manifest names no model layer", ref)
	}
	if paramsDigest != "" {
		var p map[string]any
		if err := c.getJSON(ctx, fmt.Sprintf("%s/v2/%s/%s/blobs/%s", base, namespace, model, paramsDigest), &p); err != nil {
			return TagLayers{}, err
		}
		out.Params = p
	}
	return out, nil
}

// OpenBlob streams one blob of ref. The caller closes it; closing early
// is how a header-only reader avoids fetching the weights.
func (c *Client) OpenBlob(ctx context.Context, ref, digest string) (io.ReadCloser, error) {
	base, namespace, model, _, err := c.splitRef(ref)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v2/%s/%s/blobs/%s", base, namespace, model, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// No overall timeout: the stream is closed by the reader, and the
	// header of a large build can take longer than the manifest client's
	// 30 s to arrive over a slow link.
	resp, err := (&http.Client{Transport: c.httpClient().Transport}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollamaregistry: GET %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("ollamaregistry: GET %s: status %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

// ParamInt reads an integer from the params layer.
func (l TagLayers) ParamInt(key string) (int, bool) {
	switch v := l.Params[key].(type) {
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	}
	return 0, false
}
