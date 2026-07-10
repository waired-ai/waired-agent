package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func TestPrintObservabilitySection_Text_Healthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.ObservabilityState{
			Agent: management.AgentState{
				DeviceID:      "dev_a",
				UptimeSeconds: 4200, // 1h10m
				EngineReady:   true,
				ModelID:       "qwen3:8b",
				CapacityTotal: 10,
				CapacityUsed:  2,
				Inflight:      2,
				ShareEnabled:  true,
			},
			Mesh: management.MeshState{PeersEnrolled: 3, PeersReachable: 2, PeersReady: 2},
			LastInference: &management.LastInference{
				TS:        "2026-05-16T10:22:15.000000000Z",
				Decision:  "remote",
				PeerID:    "peer_b",
				Model:     "qwen3:8b",
				LatencyMs: 412,
			},
		})
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() {
		printObservabilitySection(srv.URL, "")
	})
	for _, want := range []string{
		"Observability:",
		"dev_a",
		"1h10m",
		"Engine:   ready",
		"qwen3:8b",
		"2/10 slots used",
		"Share:    enabled",
		"Paused: no",
		"Mesh:     3 enrolled / 2 reachable / 2 ready",
		"Last:     2026-05-16T10:22:15.000000000Z",
		"decision=remote",
		"latency=412ms",
		"fallback=no",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestPrintObservabilitySection_Text_PausedHidesEngineReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.ObservabilityState{
			Agent: management.AgentState{Paused: true, EngineReady: true, ModelID: "x"},
		})
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() {
		printObservabilitySection(srv.URL, "")
	})
	if !strings.Contains(out, "Engine:   paused") {
		t.Errorf("paused agent should render Engine: paused, got\n%s", out)
	}
}

func TestPrintObservabilitySection_JSON_PassesThrough(t *testing.T) {
	want := management.ObservabilityState{
		Agent: management.AgentState{DeviceID: "dev_a", EngineReady: true},
		Mesh:  management.MeshState{PeersEnrolled: 1},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() {
		printObservabilitySection(srv.URL, "json")
	})
	var got management.ObservabilityState
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON:\n%s\nerr=%v", out, err)
	}
	if got.Agent.DeviceID != "dev_a" || got.Mesh.PeersEnrolled != 1 {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, want)
	}
}

func TestPrintObservabilitySection_404RendersUpgradeHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() {
		printObservabilitySection(srv.URL, "")
	})
	if !strings.Contains(out, "predates Phase 9") {
		t.Errorf("404 should suggest upgrade, got\n%s", out)
	}
}
