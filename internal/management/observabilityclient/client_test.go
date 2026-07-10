package observabilityclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/observability"
)

func TestGetState_OK(t *testing.T) {
	want := management.ObservabilityState{
		Agent: management.AgentState{
			DeviceID:      "dev_a",
			Version:       "1.2.3",
			UptimeSeconds: 4200,
			EngineReady:   true,
			ModelID:       "qwen3:8b",
			ShareEnabled:  true,
			CapacityTotal: 10,
			CapacityUsed:  3,
			Inflight:      3,
		},
		Mesh: management.MeshState{
			PeersEnrolled:  4,
			PeersReachable: 3,
			PeersReady:     2,
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/observability/state" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	got, err := GetState(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if got.Agent.DeviceID != want.Agent.DeviceID || got.Mesh.PeersReady != want.Mesh.PeersReady {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestGetState_404IsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	_, err := GetState(context.Background(), srv.URL)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}

func TestGetState_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := GetState(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("503 must not be reported as ErrUnsupported: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error %q should mention HTTP 503", err.Error())
	}
}

func TestGetState_DialError(t *testing.T) {
	// Closed server → connect refused. Use a server that we immediately
	// shut down so the URL points at a dead port.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := GetState(ctx, deadURL)
	if err == nil {
		t.Fatal("expected dial error, got nil")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("dial error must not be ErrUnsupported: %v", err)
	}
}

func TestGetEvents_BuildsQueryString(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/observability/events" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(EventsResponse{
			Events:    []observability.Event{{Seq: 42, Kind: observability.KindFallback}},
			NextSince: 42,
			OldestSeq: 1,
			Gap:       false,
		})
	}))
	defer srv.Close()

	resp, err := GetEvents(
		context.Background(),
		srv.URL,
		17,
		[]observability.Kind{observability.KindFallback, observability.KindRequest},
		25,
	)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}

	for _, want := range []string{"since=17", "limit=25", "kinds=fallback%2Crequest"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
	if len(resp.Events) != 1 || resp.Events[0].Seq != 42 {
		t.Errorf("events round-trip failed: %+v", resp.Events)
	}
	if resp.NextSince != 42 || resp.OldestSeq != 1 {
		t.Errorf("cursor fields wrong: NextSince=%d OldestSeq=%d", resp.NextSince, resp.OldestSeq)
	}
}

func TestGetEvents_ZeroParamsOmitsQueryKeys(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(EventsResponse{NextSince: 0})
	}))
	defer srv.Close()

	if _, err := GetEvents(context.Background(), srv.URL, 0, nil, 0); err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	// since=0 / limit=0 / kinds=nil should each be omitted entirely so
	// the server falls back to its defaults.
	for _, bad := range []string{"since=", "limit=", "kinds="} {
		if strings.Contains(gotQuery, bad) {
			t.Errorf("query %q must omit %q", gotQuery, bad)
		}
	}
}

func TestGetEvents_404IsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	_, err := GetEvents(context.Background(), srv.URL, 0, nil, 0)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}

func TestGetEvents_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := GetEvents(context.Background(), srv.URL, 0, nil, 0)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error %q should mention decode", err.Error())
	}
}

func TestGetState_TrailingSlashTolerated(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Must not see a doubled slash in the path.
		if strings.Contains(r.URL.Path, "//") {
			t.Errorf("path has doubled slash: %q", r.URL.Path)
		}
		hits++
		_ = json.NewEncoder(w).Encode(management.ObservabilityState{})
	}))
	defer srv.Close()

	if _, err := GetState(context.Background(), srv.URL+"/"); err != nil {
		t.Fatalf("GetState with trailing slash: %v", err)
	}
	if hits != 1 {
		t.Errorf("hits=%d, want 1", hits)
	}
}
