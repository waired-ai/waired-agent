package tray

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
)

func TestClient_ObservabilityState(t *testing.T) {
	want := management.ObservabilityState{
		Agent: management.AgentState{
			DeviceID:    "dev_a",
			EngineReady: true,
			ModelID:     "qwen3:8b",
		},
		Mesh: management.MeshState{
			PeersEnrolled:  4,
			PeersReachable: 3,
			PeersReady:     2,
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/observability/state" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	got, err := c.ObservabilityState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent.DeviceID != "dev_a" || got.Mesh.PeersReady != 2 {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestClient_ObservabilityState_404IsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	_, err := c.ObservabilityState(context.Background())
	if !errors.Is(err, ErrObservabilityUnsupported) {
		t.Errorf("got %v, want ErrObservabilityUnsupported", err)
	}
}

func TestClient_ObservabilityEvents(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/observability/events" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{
			Events:    []observability.Event{{Seq: 7, Kind: observability.KindFallback}},
			NextSince: 7,
		})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	resp, err := c.ObservabilityEvents(
		context.Background(),
		3,
		[]observability.Kind{observability.KindFallback},
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "since=3") ||
		!strings.Contains(gotQuery, "limit=10") ||
		!strings.Contains(gotQuery, "kinds=fallback") {
		t.Errorf("query missing expected params: %q", gotQuery)
	}
	if resp.NextSince != 7 || len(resp.Events) != 1 {
		t.Errorf("response decode failed: %+v", resp)
	}
}

func TestClient_ObservabilityEvents_404IsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	_, err := c.ObservabilityEvents(context.Background(), 0, nil, 0)
	if !errors.Is(err, ErrObservabilityUnsupported) {
		t.Errorf("got %v, want ErrObservabilityUnsupported", err)
	}
}
