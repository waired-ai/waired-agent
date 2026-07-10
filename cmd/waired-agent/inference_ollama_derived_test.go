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
		base  string
		batch int
		want  string
	}{
		{"qwen3.6:35b-a3b-mtp-q4_K_M", 2048, "qwen3.6:35b-a3b-mtp-q4_K_M-wb2048"},
		{"qwen3:8b-q4_K_M", 1024, "qwen3:8b-q4_K_M-wb1024"},
		{"", 2048, ""},
		{"base", 0, ""},
		{"base", -1, ""},
	}
	for _, c := range cases {
		if got := ollamaDerivedTag(c.base, c.batch); got != c.want {
			t.Errorf("ollamaDerivedTag(%q, %d) = %q, want %q", c.base, c.batch, got, c.want)
		}
	}
}

func TestEnsureOllamaDerivedModel(t *testing.T) {
	t.Run("creates-with-parameters", func(t *testing.T) {
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

		got, err := ensureOllamaDerivedModel(context.Background(), srv.Client(), srv.URL, "base:tag", 2048)
		if err != nil {
			t.Fatalf("ensureOllamaDerivedModel: %v", err)
		}
		if got != "base:tag-wb2048" {
			t.Errorf("derived tag = %q, want base:tag-wb2048", got)
		}
		if gotBody["from"] != "base:tag" {
			t.Errorf("create from = %v, want base:tag", gotBody["from"])
		}
		params, _ := gotBody["parameters"].(map[string]any)
		if params == nil || params["num_batch"] != float64(2048) {
			t.Errorf("parameters = %v, want num_batch=2048", gotBody["parameters"])
		}
	})

	t.Run("propagates-create-failure", func(t *testing.T) {
		// base absent → /api/create 4xx → error surfaced so the caller
		// falls back to the base tag with automatic batch.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"model 'base:tag' not found"}`))
		}))
		defer srv.Close()

		if _, err := ensureOllamaDerivedModel(context.Background(), srv.Client(), srv.URL, "base:tag", 2048); err == nil {
			t.Error("expected an error when /api/create fails")
		}
	})

	t.Run("rejects-invalid-input", func(t *testing.T) {
		if _, err := ensureOllamaDerivedModel(context.Background(), http.DefaultClient, "http://unused", "", 2048); err == nil {
			t.Error("expected an error for an empty base tag")
		}
	})
}
