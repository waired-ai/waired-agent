package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOllamaDerivedTag(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		params ollamaDerivedParams
		want   string
	}{
		// The batch-only tags are the ones already on disk on every host
		// that has a derived model today: they must not move, or every such
		// host silently re-creates its model under a new name on upgrade.
		{"batch only", "qwen3.6:35b-a3b-mtp-q4_K_M", ollamaDerivedParams{NumBatch: 2048}, "qwen3.6:35b-a3b-mtp-q4_K_M-wb2048"},
		{"batch only, other size", "qwen3:8b-q4_K_M", ollamaDerivedParams{NumBatch: 1024}, "qwen3:8b-q4_K_M-wb1024"},
		{"no mmap only", "qwen3.5:122b-a10b-q4_K_M", ollamaDerivedParams{NoMmap: true}, "qwen3.5:122b-a10b-q4_K_M-nommap"},
		{"both, fixed order", "base:tag", ollamaDerivedParams{NumBatch: 2048, NoMmap: true}, "base:tag-wb2048-nommap"},
		{"no base tag", "", ollamaDerivedParams{NumBatch: 2048}, ""},
		{"nothing to bake", "base", ollamaDerivedParams{}, ""},
		{"negative batch is nothing to bake", "base", ollamaDerivedParams{NumBatch: -1}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ollamaDerivedTag(c.base, c.params); got != c.want {
				t.Errorf("ollamaDerivedTag(%q, %+v) = %q, want %q", c.base, c.params, got, c.want)
			}
		})
	}
}

// TestOllamaDerivedTagIsDeterministic pins the property the whole
// create-every-boot design rests on: the same inputs name the same model, so
// re-creation is idempotent rather than accumulating one manifest per boot.
func TestOllamaDerivedTagIsDeterministic(t *testing.T) {
	p := ollamaDerivedParams{NumBatch: 2048, NoMmap: true}
	first := ollamaDerivedTag("base:tag", p)
	for i := 0; i < 100; i++ {
		if got := ollamaDerivedTag("base:tag", p); got != first {
			t.Fatalf("call %d returned %q, want %q", i, got, first)
		}
	}
}

func TestEnsureOllamaDerivedModel(t *testing.T) {
	// createBody runs one ensureOllamaDerivedModel against a recording
	// /api/create and returns the derived tag plus the body the engine saw.
	createBody := func(t *testing.T, base string, params ollamaDerivedParams) (string, map[string]any) {
		t.Helper()
		var gotBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/create" {
				http.NotFound(w, r)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success"})
		}))
		defer srv.Close()

		got, err := ensureOllamaDerivedModel(context.Background(), srv.Client(), srv.URL, base, params)
		if err != nil {
			t.Fatalf("ensureOllamaDerivedModel: %v", err)
		}
		return got, gotBody
	}

	t.Run("creates-with-batch-parameter", func(t *testing.T) {
		got, body := createBody(t, "base:tag", ollamaDerivedParams{NumBatch: 2048})
		if got != "base:tag-wb2048" {
			t.Errorf("derived tag = %q, want base:tag-wb2048", got)
		}
		if body["from"] != "base:tag" {
			t.Errorf("create from = %v, want base:tag", body["from"])
		}
		params, _ := body["parameters"].(map[string]any)
		if params == nil || params["num_batch"] != float64(2048) {
			t.Fatalf("parameters = %v, want num_batch=2048", body["parameters"])
		}
		if _, ok := params["use_mmap"]; ok {
			t.Errorf("use_mmap baked in without NoMmap: %v", params)
		}
	})

	t.Run("creates-with-use-mmap-false", func(t *testing.T) {
		got, body := createBody(t, "base:tag", ollamaDerivedParams{NoMmap: true})
		if got != "base:tag-nommap" {
			t.Errorf("derived tag = %q, want base:tag-nommap", got)
		}
		params, _ := body["parameters"].(map[string]any)
		if params == nil {
			t.Fatalf("no parameters in %v", body)
		}
		// The point of the parameter is the value false, not its presence:
		// baking use_mmap=true would be the bug this exists to prevent.
		if v, ok := params["use_mmap"].(bool); !ok || v {
			t.Errorf("use_mmap = %v, want false", params["use_mmap"])
		}
		if _, ok := params["num_batch"]; ok {
			t.Errorf("num_batch baked in without NumBatch: %v", params)
		}
	})

	t.Run("propagates-create-failure", func(t *testing.T) {
		// base absent → /api/create 4xx → error surfaced so the caller
		// falls back to the base tag with the engine's own defaults.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"model 'base:tag' not found"}`))
		}))
		defer srv.Close()

		if _, err := ensureOllamaDerivedModel(context.Background(), srv.Client(), srv.URL, "base:tag", ollamaDerivedParams{NumBatch: 2048}); err == nil {
			t.Error("expected an error when /api/create fails")
		}
	})

	t.Run("rejects-invalid-input", func(t *testing.T) {
		if _, err := ensureOllamaDerivedModel(context.Background(), http.DefaultClient, "http://unused", "", ollamaDerivedParams{NumBatch: 2048}); err == nil {
			t.Error("expected an error for an empty base tag")
		}
		if _, err := ensureOllamaDerivedModel(context.Background(), http.DefaultClient, "http://unused", "base:tag", ollamaDerivedParams{}); err == nil {
			t.Error("expected an error when there is nothing to bake")
		}
	})
}
