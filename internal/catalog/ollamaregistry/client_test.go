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

func TestIsHubRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL":          true,
		"huggingface.co/unsloth/Qwen3.8-27B-GGUF:UD-Q2_K_XL": true,
		"qwen3.5:27b-q4_K_M":                                 false,
		"frob/qwen3.8-flash-next:125b-a6b-ud-q2_K_XL":        false,
		// A namespace that merely starts with the same letters is not
		// the Hub; the prefix has to end at the host separator.
		"hf.community/model:v1": false,
	} {
		if got := IsHubRef(ref); got != want {
			t.Errorf("IsHubRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

// TestSplitRef_SendsEachReferenceToItsOwnRegistry pins the thing that
// would otherwise fail silently: a Hub reference asked of
// registry.ollama.ai builds a well-formed URL for a repository that
// cannot exist there, and the 404 reads as "this tag is gone".
func TestSplitRef_SendsEachReferenceToItsOwnRegistry(t *testing.T) {
	// Both overridden, and to different values, so a mix-up cannot pass.
	c := &Client{BaseURL: "https://registry.example", HubBaseURL: "https://hub.example"}
	for _, tc := range []struct{ ref, base, ns, model, tag string }{
		{"qwen3.5:27b-q4_K_M", "https://registry.example", "library", "qwen3.5", "27b-q4_K_M"},
		{"frob/flash:q2", "https://registry.example", "frob", "flash", "q2"},
		{
			"hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL",
			"https://hub.example", "unsloth", "Qwen3.8-27B-GGUF", "UD-Q3_K_XL",
		},
		{
			"huggingface.co/unsloth/Qwen3.5-122B-A10B-GGUF:UD-Q2_K_XL",
			"https://hub.example", "unsloth", "Qwen3.5-122B-A10B-GGUF", "UD-Q2_K_XL",
		},
		// No tag still means latest on either registry.
		{"hf.co/unsloth/Qwen3.5-9B-GGUF", "https://hub.example", "unsloth", "Qwen3.5-9B-GGUF", "latest"},
	} {
		base, ns, model, tag, err := c.splitRef(tc.ref)
		if err != nil {
			t.Errorf("splitRef(%q): %v", tc.ref, err)
			continue
		}
		if base != tc.base || ns != tc.ns || model != tc.model || tag != tc.tag {
			t.Errorf("splitRef(%q) = %s %q/%q:%q, want %s %q/%q:%q",
				tc.ref, base, ns, model, tag, tc.base, tc.ns, tc.model, tc.tag)
		}
	}
}

func TestSplitRef_RefusesToGuess(t *testing.T) {
	c := &Client{}
	// A Hub reference naming no organisation.
	if _, _, _, _, err := c.splitRef("hf.co/Qwen3.8-27B-GGUF:UD-Q3_K_XL"); err == nil {
		t.Error("a Hub reference with no organisation must be an error, not a guess")
	}
	// The pre-existing case, unchanged.
	if _, _, _, _, err := c.splitRef(":q4"); err == nil {
		t.Error("a reference naming no model must be an error")
	}
}

func TestTagExists_HubReferenceAsksTheHub(t *testing.T) {
	var gotPath string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()
	// The ollama registry is pointed at a server that fails everything,
	// so a reference that went there would not answer "present".
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer reg.Close()

	c := &Client{BaseURL: reg.URL, HubBaseURL: hub.URL}
	ok, err := c.TagExists(context.Background(), "hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL")
	if err != nil || !ok {
		t.Fatalf("hub tag: ok=%v err=%v", ok, err)
	}
	if want := "/v2/unsloth/Qwen3.8-27B-GGUF/manifests/UD-Q3_K_XL"; gotPath != want {
		t.Errorf("asked %s, want %s", gotPath, want)
	}
}

// TestTagRendering_HubTagsNameNoRenderer records the shape measured on
// the Hub (waired-agent#1265): its config blob is docker-shaped and
// carries no "renderer" field at all, and a template layer, where the
// repository has one, is an ordinary file its publisher uploaded.
//
// On unsloth/Qwen3.8-27B-GGUF that file is a legacy three-field ollama
// template (.System / .Prompt / .Response) with no .Messages loop and no
// tool-call handling — so Renders() answering true off HasTemplate is
// not evidence the tag can render a coding agent's requests. The check
// that a Hub tag may only ship with a renderer named on the variant
// lives with the guard that reads this, not here: this function reports
// what the tag carries and must not pretend otherwise.
func TestTagRendering_HubTagsNameNoRenderer(t *testing.T) {
	const cfgDigest = "sha256:ccc"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/unsloth/Repo-GGUF/manifests/UD-Q3_K_XL":
			_, _ = io.WriteString(w, `{"config":{"digest":"`+cfgDigest+`"},"layers":[
				{"mediaType":"application/vnd.ollama.image.model"},
				{"mediaType":"application/vnd.ollama.image.template"},
				{"mediaType":"application/vnd.ollama.image.projector"}]}`)
		case "/v2/unsloth/Repo-GGUF/blobs/" + cfgDigest:
			// Docker-shaped, as the Hub serves it: no renderer field.
			_, _ = io.WriteString(w, `{"model_format":"gguf","model_family":"qwen35","model_type":"27.3B"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()

	c := &Client{HubBaseURL: hub.URL}
	r, err := c.TagRendering(context.Background(), "hf.co/unsloth/Repo-GGUF:UD-Q3_K_XL")
	if err != nil {
		t.Fatalf("TagRendering: %v", err)
	}
	if r.Renderer != "" {
		t.Errorf("renderer = %q, want empty: the Hub config carries no such field", r.Renderer)
	}
	if !r.HasTemplate {
		t.Error("the template layer was not seen")
	}
	if !r.Renders() {
		t.Error("Renders() must report the template layer it found")
	}
}
