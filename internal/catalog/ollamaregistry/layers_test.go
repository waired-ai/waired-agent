package ollamaregistry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLayers reads what a manifest names: the model blob, the projector
// (summed, sized from the layer), and the params layer's draft count. The
// shape is the registry manifest of qwen3.8:27b-mtp-q4_K_M.
func TestLayers(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v2/library/qwen3.8/manifests/27b-mtp-q4_K_M":
			_, _ = io.WriteString(w, `{"layers":[
				{"mediaType":"application/vnd.ollama.image.projector","digest":"sha256:proj","size":931146016},
				{"mediaType":"application/vnd.ollama.image.model","digest":"sha256:model","size":16810714464},
				{"mediaType":"application/vnd.ollama.image.license","digest":"sha256:lic","size":11345},
				{"mediaType":"application/vnd.ollama.image.params","digest":"sha256:params","size":114}]}`)
		case "/v2/library/qwen3.8/blobs/sha256:params":
			_, _ = io.WriteString(w, `{"draft_num_predict":4,"temperature":1}`)
		case "/v2/library/qwen3.8/blobs/sha256:model":
			_, _ = io.WriteString(w, "GGUF-bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}

	l, err := c.Layers(context.Background(), "qwen3.8:27b-mtp-q4_K_M")
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if l.ModelDigest != "sha256:model" || l.ModelBytes != 16810714464 || l.ProjectorBytes != 931146016 {
		t.Errorf("layers = %+v", l)
	}
	if n, ok := l.ParamInt("draft_num_predict"); !ok || n != 4 {
		t.Errorf("draft_num_predict = %d, %v; want 4", n, ok)
	}
	if _, ok := l.ParamInt("num_ctx"); ok {
		t.Error("an absent param must report ok=false")
	}

	body, err := c.OpenBlob(context.Background(), "qwen3.8:27b-mtp-q4_K_M", l.ModelDigest)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	b, _ := io.ReadAll(body)
	body.Close()
	if string(b) != "GGUF-bytes" {
		t.Errorf("blob = %q", b)
	}
	if _, err := c.OpenBlob(context.Background(), "qwen3.8:27b-mtp-q4_K_M", "sha256:gone"); err == nil {
		t.Error("a missing blob must be an error")
	}
}

// A tag with no params layer drafts nothing — which is what makes ollama
// run an hf.co MTP build without its draft head.
func TestLayers_NoParamsNoModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/library/plain/manifests/1b":
			_, _ = io.WriteString(w, `{"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":"sha256:m","size":10}]}`)
		case "/v2/library/empty/manifests/1b":
			_, _ = io.WriteString(w, `{"layers":[{"mediaType":"application/vnd.ollama.image.license","digest":"sha256:l","size":1}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	l, err := c.Layers(context.Background(), "plain:1b")
	if err != nil || l.Params != nil {
		t.Errorf("plain tag = %+v, %v; want no params and no error", l, err)
	}
	if _, err := c.Layers(context.Background(), "empty:1b"); err == nil {
		t.Error("a manifest with no model layer must be an error")
	}
}
