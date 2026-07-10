package hfclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const modelListJSON = `[
	{"id":"Qwen/Qwen4-Coder-40B","createdAt":"2026-06-10T00:00:00Z","pipeline_tag":"text-generation","gated":false,"cardData":{"license":"apache-2.0"}},
	{"modelId":"some/gated-model","createdAt":"2026-06-09T00:00:00Z","pipeline_tag":"text-generation","gated":"manual","tags":["license:mit"]}
]`

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" && r.URL.Query().Get("author") == "Qwen" {
			_, _ = w.Write([]byte(modelListJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	models, err := c.ListModels(context.Background(), "Qwen", 50)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	if models[0].RepoID() != "Qwen/Qwen4-Coder-40B" || models[0].License() != "apache-2.0" || models[0].IsGated() {
		t.Errorf("model[0] wrong: %+v", models[0])
	}
	// modelId fallback, gated string, license-via-tag.
	if models[1].RepoID() != "some/gated-model" || !models[1].IsGated() || models[1].License() != "mit" {
		t.Errorf("model[1] wrong: repo=%q gated=%v license=%q", models[1].RepoID(), models[1].IsGated(), models[1].License())
	}
}

func TestListModels_429Retry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(modelListJSON))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), RetryBackoff: time.Millisecond}
	models, err := c.ListModels(context.Background(), "Qwen", 10)
	if err != nil {
		t.Fatalf("ListModels with one 429 should retry and succeed: %v", err)
	}
	if len(models) != 2 || calls.Load() != 2 {
		t.Errorf("expected 2 models after 2 calls, got %d models / %d calls", len(models), calls.Load())
	}
}

func TestListModels_429RetryDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), RetryBackoff: -1}
	if _, err := c.ListModels(context.Background(), "Qwen", 10); err == nil {
		t.Error("expected error when retry disabled and server returns 429")
	}
}
