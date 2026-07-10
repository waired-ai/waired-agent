package hfclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
