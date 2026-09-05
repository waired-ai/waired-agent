package ollamaregistry

import (
	"context"
	"io"
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

// TestTagRendering covers the three shapes a tag can have, because the
// point of the check is telling the third apart from the first two.
func TestTagRendering(t *testing.T) {
	const (
		cfgRenderer = "sha256:aaa"
		cfgBare     = "sha256:bbb"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		// A tag that names a built-in renderer and has no template layer.
		case "/v2/library/rendered/manifests/1b":
			_, _ = io.WriteString(w, `{"config":{"digest":"`+cfgRenderer+`"},`+
				`"layers":[{"mediaType":"application/vnd.ollama.image.model"}]}`)
		// A tag that carries its own template and names no renderer.
		case "/v2/library/templated/manifests/1b":
			_, _ = io.WriteString(w, `{"config":{"digest":"`+cfgBare+`"},`+
				`"layers":[{"mediaType":"application/vnd.ollama.image.model"},`+
				`{"mediaType":"application/vnd.ollama.image.template"}]}`)
		// The case this check exists for: neither.
		case "/v2/community/neither/manifests/q2":
			_, _ = io.WriteString(w, `{"config":{"digest":"`+cfgBare+`"},`+
				`"layers":[{"mediaType":"application/vnd.ollama.image.model"},`+
				`{"mediaType":"application/vnd.ollama.image.params"}]}`)
		case "/v2/library/rendered/blobs/" + cfgRenderer:
			_, _ = io.WriteString(w, `{"model_format":"gguf","renderer":"qwen3.8","parser":"qwen3.5"}`)
		case "/v2/library/templated/blobs/" + cfgBare,
			"/v2/community/neither/blobs/" + cfgBare:
			_, _ = io.WriteString(w, `{"model_format":"gguf","model_family":"qwen4exp"}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()

	for _, tc := range []struct {
		ref      string
		want     Rendering
		renders  bool
		wantWord string
	}{
		{"rendered:1b", Rendering{Renderer: "qwen3.8"}, true, `renderer "qwen3.8"`},
		{"templated:1b", Rendering{HasTemplate: true}, true, "a template layer"},
		{"community/neither:q2", Rendering{}, false, "neither a renderer nor a template layer"},
	} {
		got, err := c.TagRendering(ctx, tc.ref)
		if err != nil {
			t.Errorf("TagRendering(%q): %v", tc.ref, err)
			continue
		}
		if got != tc.want {
			t.Errorf("TagRendering(%q) = %+v, want %+v", tc.ref, got, tc.want)
		}
		if got.Renders() != tc.renders {
			t.Errorf("TagRendering(%q).Renders() = %v, want %v", tc.ref, got.Renders(), tc.renders)
		}
		if got.String() != tc.wantWord {
			t.Errorf("TagRendering(%q).String() = %q, want %q", tc.ref, got.String(), tc.wantWord)
		}
	}

	// A registry that cannot answer is an error, never a tag that
	// "renders nothing" — the same distinction TagExists draws, and for
	// the same reason: the failure would read as a defect in the entry.
	if _, err := c.TagRendering(ctx, "confused:1b"); err == nil {
		t.Error("a 500 from the registry must be an error, not a verdict")
	}
}
