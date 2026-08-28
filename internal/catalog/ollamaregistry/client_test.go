package ollamaregistry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSplitTag(t *testing.T) {
	for _, tc := range []struct{ ref, ns, model, tag string }{
		{"qwen3.5:0.8b-q8_0", "library", "qwen3.5", "0.8b-q8_0"},
		{"gpt-oss:20b", "library", "gpt-oss", "20b"},
		{"granite4:350m", "library", "granite4", "350m"},
		{"qwen3.6:27b-mtp-q4_K_M", "library", "qwen3.6", "27b-mtp-q4_K_M"},
		// No tag means latest, as the CLI treats it.
		{"qwen3.5", "library", "qwen3.5", "latest"},
		// An explicit namespace is kept.
		{"myorg/model:v1", "myorg", "model", "v1"},
		{"myorg/model", "myorg", "model", "latest"},
	} {
		ns, model, tag := SplitTag(tc.ref)
		if ns != tc.ns || model != tc.model || tag != tc.tag {
			t.Errorf("SplitTag(%q) = %q/%q:%q, want %q/%q:%q",
				tc.ref, ns, model, tag, tc.ns, tc.model, tc.tag)
		}
	}
}

func TestTagExists(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/v2/library/present/manifests/1b":
			w.WriteHeader(http.StatusOK)
		case "/v2/library/absent/manifests/1b":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()

	if ok, err := c.TagExists(ctx, "present:1b"); err != nil || !ok {
		t.Errorf("present: ok=%v err=%v", ok, err)
	}
	if gotPath != "/v2/library/present/manifests/1b" {
		t.Errorf("asked the wrong URL: %s", gotPath)
	}
	if ok, err := c.TagExists(ctx, "absent:1b"); err != nil || ok {
		t.Errorf("absent: ok=%v err=%v", ok, err)
	}

	// A registry that cannot answer must not be read as "gone". Taking a
	// live model out of the catalog on a bad afternoon is the failure
	// this distinction exists to prevent.
	ok, err := c.TagExists(ctx, "confused:1b")
	if err == nil {
		t.Error("a 500 must be an error, not an answer")
	}
	if ok {
		t.Error("a 500 must not read as present either")
	}
}
