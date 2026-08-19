package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// testOllamaAdapter builds an adapter pointed at a test server. The
// adapter derives its base URL from Host/Port, so the URL is split
// rather than assigned.
func testOllamaAdapter(t *testing.T, rawURL string, keepAlive time.Duration) *infruntime.OllamaAdapter {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", rawURL, err)
	}
	return infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Host: u.Hostname(), Port: port, KeepAlive: keepAlive,
	})
}

// TestResidencyFromPS pins the mapping from an /api/ps body onto the
// recorded observation. Record of today's behaviour, except where noted.
func TestResidencyFromPS(t *testing.T) {
	now := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		body         psResponse
		wantResident bool
		wantModel    string
		wantUntil    time.Time
	}{
		{
			name: "nothing loaded is observed, not unknown",
			body: psResponse{},
		},
		{
			name:         "resident with a parseable expiry",
			body:         psResponse{Models: []psModel{{Name: "m:q4", ExpiresAt: "2026-08-20T02:30:00Z"}}},
			wantResident: true,
			wantModel:    "m:q4",
			wantUntil:    time.Date(2026, 8, 20, 2, 30, 0, 0, time.UTC),
		},
		{
			// An indefinite keep-alive is not a sentinel: Ollama renders
			// it as a date centuries out. A far-future value must survive
			// as an ordinary time, not be rejected as nonsense.
			name:         "indefinite renders as a far-future date",
			body:         psResponse{Models: []psModel{{Name: "m:q4", ExpiresAt: "2318-11-30T00:54:37Z"}}},
			wantResident: true,
			wantModel:    "m:q4",
			wantUntil:    time.Date(2318, 11, 30, 0, 54, 37, 0, time.UTC),
		},
		{
			// The weights are in memory whether or not we can read the
			// clock, so an unparseable expiry must still count as resident.
			name:         "unparseable expiry stays resident",
			body:         psResponse{Models: []psModel{{Name: "m:q4", ExpiresAt: "not-a-time"}}},
			wantResident: true,
			wantModel:    "m:q4",
		},
		{
			name:         "missing expiry stays resident",
			body:         psResponse{Models: []psModel{{Name: "m:q4"}}},
			wantResident: true,
			wantModel:    "m:q4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := residencyFromPS(tc.body, now)
			if !got.Observed {
				t.Fatalf("Observed = false; a body we decoded is an observation")
			}
			if got.Resident() != tc.wantResident {
				t.Errorf("Resident() = %v, want %v", got.Resident(), tc.wantResident)
			}
			if got.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tc.wantModel)
			}
			if !got.Until.Equal(tc.wantUntil) {
				t.Errorf("Until = %v, want %v", got.Until, tc.wantUntil)
			}
			if !got.At.Equal(now) {
				t.Errorf("At = %v, want %v", got.At, now)
			}
		})
	}
}

// TestRefreshOllamaResidencyKeepsLastOnError pins that a probe failure
// leaves the previous observation alone. Rendering an unreachable engine
// as "not resident" would report an unload that did not happen — the
// same shape of wrong answer waired-agent#879 exists to remove.
func TestRefreshOllamaResidencyKeepsLastOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ad := testOllamaAdapter(t, srv.URL, 0)
	ad.SetResidency(infruntime.ModelResidency{Observed: true, Model: "m:q4"})
	refreshOllamaResidency(context.Background(), ad, srv.Client())

	if got := ad.Residency(); !got.Resident() || got.Model != "m:q4" {
		t.Errorf("a failed probe overwrote the last observation: %+v", got)
	}
}

// TestUnloadServingModel drives the release valve against a fake engine
// and asserts the request that actually frees the memory: keep_alive 0
// against the resident tag (waired-agent#861).
func TestUnloadServingModel(t *testing.T) {
	var loaded atomic.Bool
	loaded.Store(true)
	var gotKeepAlive, gotModel atomic.Value
	gotKeepAlive.Store("")
	gotModel.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.Header().Set("Content-Type", "application/json")
			if loaded.Load() {
				_ = json.NewEncoder(w).Encode(psResponse{Models: []psModel{{Name: "m:q4", ExpiresAt: "2318-11-30T00:54:37Z"}}})
				return
			}
			_ = json.NewEncoder(w).Encode(psResponse{})
		case "/api/generate":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ka, ok := body["keep_alive"].(string); ok {
				gotKeepAlive.Store(ka)
			}
			if m, ok := body["model"].(string); ok {
				gotModel.Store(m)
			}
			loaded.Store(false)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"done":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ad := testOllamaAdapter(t, srv.URL, 0)
	p := &agentInferenceProvider{ollama: ad}

	tag, err := p.UnloadServingModel(context.Background())
	if err != nil {
		t.Fatalf("UnloadServingModel: %v", err)
	}
	if tag != "m:q4" {
		t.Errorf("unloaded tag = %q, want m:q4", tag)
	}
	if got := gotKeepAlive.Load().(string); got != "0" {
		t.Errorf("keep_alive = %q, want \"0\" (unload after this request)", got)
	}
	if got := gotModel.Load().(string); got != "m:q4" {
		t.Errorf("model = %q, want m:q4", got)
	}
	// The surfaces must reflect the unload, not assume it.
	if res := ad.Residency(); !res.Observed || res.Resident() {
		t.Errorf("residency after unload = %+v, want observed and not resident", res)
	}

	// Unloading again is a success with an empty tag: the caller wanted
	// the memory back and the memory is back.
	tag, err = p.UnloadServingModel(context.Background())
	if err != nil {
		t.Fatalf("second UnloadServingModel: %v", err)
	}
	if tag != "" {
		t.Errorf("second unload returned %q, want empty", tag)
	}
}

// TestProviderKeepAliveFollowsConfig pins that the per-request value the
// warm path sends is the operator's setting, not a constant. Before
// waired-agent#861 this was a hardcoded 60m that no setting could move.
func TestProviderKeepAliveFollowsConfig(t *testing.T) {
	p := &agentInferenceProvider{ollama: testOllamaAdapter(t, "http://127.0.0.1:1", 20*time.Minute)}
	if got := p.keepAlive(); got != "20m0s" {
		t.Errorf("keepAlive() = %q, want 20m0s", got)
	}
	p = &agentInferenceProvider{ollama: testOllamaAdapter(t, "http://127.0.0.1:1", 0)}
	if got := p.keepAlive(); got != "-1" {
		t.Errorf("keepAlive() with the default = %q, want -1", got)
	}
}
