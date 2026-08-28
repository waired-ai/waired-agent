package hfclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchConfig(t *testing.T) {
	const cfgJSON = `{
		"num_hidden_layers": 48,
		"hidden_size": 2048,
		"num_attention_heads": 32,
		"num_key_value_heads": 4,
		"head_dim": 128,
		"num_experts": 128,
		"num_experts_per_tok": 8,
		"max_position_embeddings": 262144,
		"some_unknown_future_field": "ignored"
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Qwen/Qwen3-Coder-30B-A3B-Instruct/resolve/main/config.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(cfgJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	cfg, raw, err := c.FetchConfig(context.Background(), "Qwen/Qwen3-Coder-30B-A3B-Instruct", "")
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if cfg.NumHiddenLayers != 48 || cfg.NumKeyValueHeads != 4 || cfg.HeadDim != 128 || cfg.NumExperts != 128 {
		t.Errorf("decoded config wrong: %+v", cfg)
	}
	if len(raw) == 0 {
		t.Error("raw config bytes empty")
	}
	full, _ := cfg.FullAttnLayers()
	if full != 48 {
		t.Errorf("FullAttnLayers = %d, want 48", full)
	}
}

func TestFetchConfig_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	_, _, err := c.FetchConfig(context.Background(), "does/not-exist", "main")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("FetchConfig error = %v, want ErrNotFound", err)
	}
}

func TestFetchConfig_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	_, _, err := c.FetchConfig(context.Background(), "x/y", "main")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("FetchConfig error = %v, want non-nil non-NotFound", err)
	}
}

// TestRepoExists pins the distinction the Hub forces on us: a missing
// repo answers 401, not 404. get() maps only 404, which is why a
// nonexistent repo used to surface as a generic status error and
// Qwen/Qwen3.6-27B-AWQ shipped for months without existing (#824).
func TestRepoExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("want HEAD, got %s", r.Method)
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/org/present/"):
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/org/redirected/"):
			// What a present file actually looks like: the Hub
			// redirects resolve/ to its CDN.
			w.WriteHeader(http.StatusTemporaryRedirect)
		case strings.HasPrefix(r.URL.Path, "/org/missing/"):
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasPrefix(r.URL.Path, "/org/gone/"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/org/gated/"):
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, RetryBackoff: -1}
	ctx := context.Background()

	for _, tc := range []struct {
		repo string
		want bool
	}{
		{"org/present", true},
		{"org/redirected", true},
		{"org/missing", false},
		{"org/gone", false},
		{"org/gated", false},
	} {
		got, err := c.RepoExists(ctx, tc.repo, "")
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.repo, err)
			continue
		}
		if got != tc.want {
			t.Errorf("RepoExists(%s) = %v, want %v", tc.repo, got, tc.want)
		}
	}

	// A Hub that cannot answer is not a Hub saying "no".
	if _, err := c.RepoExists(ctx, "org/confused", ""); err == nil {
		t.Error("a 502 must be an error, not an answer")
	}
}
