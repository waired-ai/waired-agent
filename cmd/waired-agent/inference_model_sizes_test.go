package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestOllamaTagSizes_OneRequestForEveryTag: the sizes for a whole engine
// come from a single /api/tags call, not one per model — which is what
// makes it affordable to ask on a listing (#661).
func TestOllamaTagSizes_OneRequestForEveryTag(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/tags" {
			t.Errorf("path = %q, want /api/tags", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"models":[
			{"name":"qwen3:8b-q4_K_M","size":5100000000},
			{"name":"gemma3:4b","size":3390000000},
			{"name":"nosize:latest"}
		]}`))
	}))
	defer srv.Close()

	got, err := ollamaTagSizes(context.Background(), srv.Client(), srv.URL, 2*time.Second)
	if err != nil {
		t.Fatalf("ollamaTagSizes: %v", err)
	}
	if calls != 1 {
		t.Errorf("made %d requests for 3 tags, want 1", calls)
	}
	if got["qwen3:8b-q4_K_M"] != 5_100_000_000 || got["gemma3:4b"] != 3_390_000_000 {
		t.Errorf("sizes = %v, want both models' figures", got)
	}
	// A tag the engine reports without a size is absent, not zero: the
	// caller has to tell "not stated" apart from "empty".
	if _, present := got["nosize:latest"]; present {
		t.Errorf("a tag with no size was recorded: %v", got)
	}
}

// TestOllamaTagSizes_EngineDownIsAnError, not an empty map that a caller
// could mistake for "the engine says every model is 0 bytes".
func TestOllamaTagSizes_EngineDownIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	client := srv.Client()
	url := srv.URL
	srv.Close() // nothing is listening now

	if _, err := ollamaTagSizes(context.Background(), client, url, 500*time.Millisecond); err == nil {
		t.Error("err = nil for an engine that is not there, want a failure")
	}
}

// TestOllamaTagSize_StillFindsOneTag guards the helper the tuning
// verification uses (#621), which now shares the fetch above.
func TestOllamaTagSize_StillFindsOneTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b-q4_K_M","size":5100000000}]}`))
	}))
	defer srv.Close()

	got, err := ollamaTagSize(context.Background(), srv.Client(), srv.URL, "qwen3:8b-q4_K_M")
	if err != nil {
		t.Fatalf("ollamaTagSize: %v", err)
	}
	if got != 5_100_000_000 {
		t.Errorf("size = %d, want 5100000000", got)
	}
	if _, err := ollamaTagSize(context.Background(), srv.Client(), srv.URL, "absent:tag"); err == nil {
		t.Error("err = nil for a tag the engine does not serve, want a failure")
	}
}
