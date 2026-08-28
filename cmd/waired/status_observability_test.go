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
				TTFTMs:    380,
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
		"ttft=380ms",
		"latency=412ms",
		"fallback=no",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

// PRODUCT CONTRACT (waired-agent#874): zero means the serving leg could
// not observe a first token, so the term is left out rather than printed
// as ttft=0ms, which would read as "it was instant". Mirrors this
// block's existing rule that empty fields are elided.
func TestPrintObservabilitySection_Text_UnobservedTTFTIsElided(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.ObservabilityState{
			Agent: management.AgentState{EngineReady: true, ModelID: "qwen3:8b"},
			LastInference: &management.LastInference{
				TS:        "2026-05-16T10:22:15.000000000Z",
				Decision:  "local",
				Model:     "qwen3:8b",
				LatencyMs: 412,
			},
		})
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() {
		printObservabilitySection(srv.URL, "")
	})
	if strings.Contains(out, "ttft=") {
		t.Errorf("unobserved TTFT rendered anyway:\n%s", out)
	}
	if !strings.Contains(out, "latency=412ms") {
		t.Errorf("eliding ttft dropped the rest of the line:\n%s", out)
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

// THE #1106 BAR. PRODUCT CONTRACT: the Engine line reports `engine failed`
// and the reason beside it, which is what docs-site's troubleshooting page
// documents for the line it tells people to read first.
//
// It printed neither. A dead engine rendered as `not ready` —
// indistinguishable from a model still downloading, which is the very
// distinction that page is drawing — and EngineFailureReason, populated
// from servingFailureReason and sitting on this same struct, was read by
// `waired doctor` alone. Captured live on a Windows host whose engine had
// failed with a named cause: "Engine:   not ready (model=(unknown))".
func TestPrintObservabilitySection_Text_EngineFailedCarriesTheReason(t *testing.T) {
	const reason = "another program is already listening on 127.0.0.1:9475, " +
		"the port the inference engine was told to use"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.ObservabilityState{
			Agent: management.AgentState{
				DeviceID:            "dev_a",
				EngineReady:         false,
				EngineName:          "ollama",
				EngineFailureReason: reason,
			},
		})
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() { printObservabilitySection(srv.URL, "") })

	if !strings.Contains(out, "engine failed") {
		t.Errorf("Engine line = %q, want the documented `engine failed` value — `not ready`\n"+
			"reads as a download still running", engineLine(t, out))
	}
	if !strings.Contains(out, reason) {
		t.Errorf("Engine line = %q, want the reason on the same line, as the\n"+
			"troubleshooting page promises", engineLine(t, out))
	}
}

// A paused engine is the operator's own doing and keeps saying so: telling
// someone their engine failed when they stopped it is a worse answer than
// the reason, even when a stale reason is still on the struct.
func TestPrintObservabilitySection_Text_PausedOutranksTheFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.ObservabilityState{
			Agent: management.AgentState{
				Paused:              true,
				EngineFailureReason: "something the engine said before it was stopped",
			},
		})
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() { printObservabilitySection(srv.URL, "") })
	if !strings.Contains(out, "paused") {
		t.Errorf("Engine line = %q, want `paused`", engineLine(t, out))
	}
	if strings.Contains(out, "engine failed") {
		t.Errorf("Engine line = %q, want the operator's own stop reported as itself",
			engineLine(t, out))
	}
}

// engineLine pulls the one line under test out of the block, so a failure
// message shows what was printed rather than the whole dump.
func engineLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "Engine:") {
			return strings.TrimSpace(l)
		}
	}
	return "(no Engine line)"
}
