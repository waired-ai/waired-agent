package tray

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func TestClient_ClaudeRouting_GET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/waired/v1/integration/claude/route" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"policy":{"main":"anthropic","sub":"waired"}}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	got, err := c.ClaudeRouting(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy.Main != state.ClaudeRouteAnthropic || got.Policy.Sub != state.ClaudeRouteWaired {
		t.Errorf("unexpected policy: %+v", got.Policy)
	}
}

func TestClient_SetClaudeRouting_POSTBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
		}
		var req management.ClaudeRoutingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// Only Sub set; Main must be left nil (untouched).
		if req.Main != nil {
			t.Errorf("Main should be nil, got %v", *req.Main)
		}
		if req.Sub == nil || *req.Sub != state.ClaudeRouteWaired {
			t.Errorf("Sub = %v, want waired", req.Sub)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"policy":{"main":"auto","sub":"waired"}}`))
	}))
	t.Cleanup(srv.Close)

	sub := state.ClaudeRouteWaired
	c := newTestClient(srv.URL)
	got, err := c.SetClaudeRouting(context.Background(), management.ClaudeRoutingRequest{Sub: &sub})
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy.Sub != state.ClaudeRouteWaired {
		t.Errorf("resulting sub = %q, want waired", got.Policy.Sub)
	}
}

func TestClient_ClaudeRouting_UnsupportedYields404Sentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	if _, err := c.ClaudeRouting(context.Background()); !errors.Is(err, ErrClaudeRoutingUnsupported) {
		t.Errorf("GET: expected ErrClaudeRoutingUnsupported, got %v", err)
	}
	main := state.ClaudeRouteAuto
	if _, err := c.SetClaudeRouting(context.Background(), management.ClaudeRoutingRequest{Main: &main}); !errors.Is(err, ErrClaudeRoutingUnsupported) {
		t.Errorf("POST: expected ErrClaudeRoutingUnsupported, got %v", err)
	}
}
