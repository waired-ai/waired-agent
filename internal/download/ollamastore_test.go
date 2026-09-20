package download

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The two shapes the shipped catalog uses, and what they resolve to. Both
// were read off the reference host's own store before this code existed.
func TestParseOllamaTag(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want string // host/namespace/model/tag
	}{
		{"qwen3.6:35b-a3b-mtp-q4_K_M", "registry.ollama.ai/library/qwen3.6/35b-a3b-mtp-q4_K_M"},
		{"qwen3.5:0.8b-q8_0", "registry.ollama.ai/library/qwen3.5/0.8b-q8_0"},
		{"hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL", "hf.co/unsloth/Qwen3.8-27B-GGUF/UD-Q3_K_XL"},
		{"hf.co/unsloth/Qwen3.6-35B-A3B-MTP-GGUF:UD-Q2_K_XL", "hf.co/unsloth/Qwen3.6-35B-A3B-MTP-GGUF/UD-Q2_K_XL"},
		// No tag: ollama's default, and the reason the default is not "".
		{"qwen3.6", "registry.ollama.ai/library/qwen3.6/latest"},
		// A namespace without a host takes the default host.
		{"frob/qwen3.8-flash-next:125b-a6b-ud-q2_K_XL", "registry.ollama.ai/frob/qwen3.8-flash-next/125b-a6b-ud-q2_K_XL"},
		// A colon BEFORE the last slash is a port, not a tag separator.
		{"localhost:5000/library/m", "localhost:5000/library/m/latest"},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			got, err := ManifestPath("/store", tc.tag)
			if err != nil {
				t.Fatalf("ManifestPath: %v", err)
			}
			want := filepath.Join("/store", "manifests", filepath.FromSlash(tc.want))
			if got != want {
				t.Errorf("ManifestPath = %q, want %q", got, want)
			}
		})
	}
}

func TestParseOllamaTag_Refusals(t *testing.T) {
	for _, tag := range []string{"", "   ", "../../etc/passwd:latest", "a/../b:t", "m:"} {
		if _, err := ManifestPath("/store", tag); err == nil {
			t.Errorf("tag %q was accepted; a tag that does not split cleanly must not name a path", tag)
		}
	}
}

func writeManifest(t *testing.T, store, rel string, layers []map[string]any) {
	t.Helper()
	p := filepath.Join(store, "manifests", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"layers": layers})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestModelBlobPath(t *testing.T) {
	store := t.TempDir()
	const modelDigest = "sha256:d372de8e934898a59e6ccfabc3368474711384d8f1fd4d22d87a3f0a45400cdc"
	writeManifest(t, store, "registry.ollama.ai/library/qwen3.6/35b-a3b-mtp-q4_K_M", []map[string]any{
		// The weights are not the first layer, and the others must not win.
		{"mediaType": "application/vnd.ollama.image.license", "digest": "sha256:aaaa"},
		{"mediaType": "application/vnd.ollama.image.projector", "digest": "sha256:bbbb"},
		{"mediaType": mediaTypeModel, "digest": modelDigest},
		{"mediaType": "application/vnd.ollama.image.params", "digest": "sha256:cccc"},
	})

	path, digest, err := ModelBlobPath(store, "qwen3.6:35b-a3b-mtp-q4_K_M")
	if err != nil {
		t.Fatalf("ModelBlobPath: %v", err)
	}
	if digest != modelDigest {
		t.Errorf("digest = %q", digest)
	}
	want := filepath.Join(store, "blobs", "sha256-d372de8e934898a59e6ccfabc3368474711384d8f1fd4d22d87a3f0a45400cdc")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestModelBlobPath_Refusals(t *testing.T) {
	store := t.TempDir()

	t.Run("no model layer", func(t *testing.T) {
		writeManifest(t, store, "registry.ollama.ai/library/m/t", []map[string]any{
			{"mediaType": "application/vnd.ollama.image.license", "digest": "sha256:aaaa"},
		})
		if _, _, err := ModelBlobPath(store, "m:t"); !errors.Is(err, ErrNoModelLayer) {
			t.Errorf("err = %v, want ErrNoModelLayer", err)
		}
	})

	t.Run("digest that is not a plain file name", func(t *testing.T) {
		writeManifest(t, store, "registry.ollama.ai/library/n/t", []map[string]any{
			{"mediaType": mediaTypeModel, "digest": "sha256:../../../etc/passwd"},
		})
		if _, _, err := ModelBlobPath(store, "n:t"); err == nil {
			t.Error("a digest that escapes the blobs directory was accepted")
		}
	})

	t.Run("absent manifest", func(t *testing.T) {
		if _, _, err := ModelBlobPath(store, "never-pulled:t"); err == nil {
			t.Error("a tag with no manifest was accepted")
		}
	})
}

// Blobs are shared by digest. Editing one edits it for every tag that names
// it, so a caller has to be able to find out which those are.
func TestTagsSharingBlob(t *testing.T) {
	store := t.TempDir()
	const shared = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const alone = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	const license = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

	writeManifest(t, store, "registry.ollama.ai/library/qwen3.6/35b-a3b", []map[string]any{
		{"mediaType": mediaTypeModel, "digest": shared},
		{"mediaType": "application/vnd.ollama.image.license", "digest": license},
	})
	writeManifest(t, store, "registry.ollama.ai/library/qwen3.6/35b-a3b-q4_K_M", []map[string]any{
		{"mediaType": mediaTypeModel, "digest": shared},
	})
	writeManifest(t, store, "registry.ollama.ai/library/qwen3.8/27b-mtp-q4_K_M", []map[string]any{
		// Same license blob, different weights: sharing a layer is not
		// sharing the model, and only the model layer counts here.
		{"mediaType": mediaTypeModel, "digest": alone},
		{"mediaType": "application/vnd.ollama.image.license", "digest": license},
	})

	got, err := TagsSharingBlob(store, shared)
	if err != nil {
		t.Fatalf("TagsSharingBlob: %v", err)
	}
	want := map[string]bool{"qwen3.6:35b-a3b": true, "qwen3.6:35b-a3b-q4_K_M": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want the two tags sharing the weights", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected tag %q", g)
		}
	}

	solo, err := TagsSharingBlob(store, alone)
	if err != nil {
		t.Fatal(err)
	}
	if len(solo) != 1 || solo[0] != "qwen3.8:27b-mtp-q4_K_M" {
		t.Errorf("got %v, want just the one tag", solo)
	}

	// An empty store is not an error: a machine that has pulled nothing
	// shares nothing.
	none, err := TagsSharingBlob(t.TempDir(), shared)
	if err != nil {
		t.Errorf("empty store: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("empty store returned %v", none)
	}
}
