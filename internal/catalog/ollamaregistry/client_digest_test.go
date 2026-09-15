package ollamaregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTagDigest pins what a catalog digest is a digest OF: the manifest
// bytes exactly as the registry served them (waired-agent#1305), on both
// registries an ollama tag can name.
func TestTagDigest(t *testing.T) {
	const flash = `{"layers":[{"digest":"sha256:864f48ac","size":78869000000}]}`
	const hub = `{"layers":[{"digest":"sha256:ab12","size":13480000000}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/frob/flash-next/manifests/q2":
			_, _ = io.WriteString(w, flash)
		case "/v2/unsloth/Qwen3.6-35B-A3B-MTP-GGUF/manifests/UD-Q2_K_XL":
			_, _ = io.WriteString(w, hub)
		case "/v2/library/empty/manifests/latest":
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HubBaseURL: srv.URL}
	ctx := context.Background()

	want := func(body string) string {
		sum := sha256.Sum256([]byte(body))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	for _, tc := range []struct{ ref, want string }{
		{"frob/flash-next:q2", want(flash)},
		{"hf.co/unsloth/Qwen3.6-35B-A3B-MTP-GGUF:UD-Q2_K_XL", want(hub)},
	} {
		got, err := c.TagDigest(ctx, tc.ref)
		if err != nil {
			t.Errorf("TagDigest(%q): %v", tc.ref, err)
			continue
		}
		if got != tc.want {
			t.Errorf("TagDigest(%q) = %s, want %s", tc.ref, got, tc.want)
		}
	}
	// A registry that cannot answer, and one that answers nothing, are
	// errors: a digest of no bytes would read as "the build changed".
	for _, ref := range []string{"frob/missing:q2", "empty"} {
		if got, err := c.TagDigest(ctx, ref); err == nil {
			t.Errorf("TagDigest(%q) = %q, want an error", ref, got)
		}
	}
}
