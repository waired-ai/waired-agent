package ollamaregistry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTagSize pins the sum a download bar is seeded from.
//
// PRODUCT CONTRACT (waired-agent#1299, owner ruling 2026-09-12: the bar
// is one bar): the total a pull shows is every layer the manifest names,
// from the first byte. The sizes below are the shape the real registries
// serve, read 2026-09-12: a library tag is model + license + params, and
// a tag can put a ~0.9 GB projector layer FIRST, which is what made the
// bar restart part-way through — ollama announces layers as it reaches
// them, so a total summed from what has been seen starts at the
// projector's size and later jumps past the weights'.
func TestTagSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/library/qwen3.5/manifests/27b-q4_K_M":
			_, _ = io.WriteString(w, `{"layers":[`+
				`{"mediaType":"application/vnd.ollama.image.model","size":17419946496},`+
				`{"mediaType":"application/vnd.ollama.image.license","size":11338},`+
				`{"mediaType":"application/vnd.ollama.image.params","size":120}]}`)
		case "/v2/frob/flash-next/manifests/q2":
			_, _ = io.WriteString(w, `{"layers":[`+
				`{"mediaType":"application/vnd.ollama.image.projector","size":908000000},`+
				`{"mediaType":"application/vnd.ollama.image.model","size":78869000000},`+
				`{"mediaType":"application/vnd.ollama.image.params","size":120}]}`)
		// A manifest that names no sized layer is a real answer, and
		// zero is not a size to seed a bar with.
		case "/v2/library/empty/manifests/latest":
			_, _ = io.WriteString(w, `{"layers":[]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()

	for _, tc := range []struct {
		ref  string
		want int64
	}{
		{"qwen3.5:27b-q4_K_M", 17419957954},
		{"frob/flash-next:q2", 79777000120},
	} {
		got, err := c.TagSize(ctx, tc.ref)
		if err != nil {
			t.Errorf("TagSize(%q): %v", tc.ref, err)
			continue
		}
		if got != tc.want {
			t.Errorf("TagSize(%q) = %d, want %d", tc.ref, got, tc.want)
		}
	}

	if _, err := c.TagSize(ctx, "empty:latest"); err == nil {
		t.Error("a manifest that names no sized layer must be an error, not a 0-byte total")
	}
	// The same distinction TagExists and TagRendering draw: a registry
	// that cannot answer is a failure to answer, never a total of zero.
	// A zero seeded into the bar would read as "nothing to download".
	if _, err := c.TagSize(ctx, "confused:1b"); err == nil {
		t.Error("a 500 from the registry must be an error, not a size")
	}
}

// TestTagSize_HubReferenceAsksTheHub pins that one mechanism covers both
// registries: the Hub serves /v2/ manifests carrying the same
// layers[].size, so a hf.co tag seeds its bar from the same call.
func TestTagSize_HubReferenceAsksTheHub(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/unsloth/Qwen3.8-27B-GGUF/manifests/UD-Q3_K_XL" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"layers":[`+
			`{"mediaType":"application/vnd.ollama.image.model","size":13146000000},`+
			`{"mediaType":"application/vnd.ollama.image.template","size":1723},`+
			`{"mediaType":"application/vnd.ollama.image.projector","size":931000000},`+
			`{"mediaType":"application/vnd.ollama.image.params","size":120}]}`)
	}))
	defer hub.Close()
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a hf.co reference must not reach the ollama registry")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer reg.Close()

	c := &Client{BaseURL: reg.URL, HubBaseURL: hub.URL}
	got, err := c.TagSize(context.Background(), "hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL")
	if err != nil {
		t.Fatalf("TagSize: %v", err)
	}
	if want := int64(14077001843); got != want {
		t.Errorf("TagSize = %d, want %d", got, want)
	}
}
