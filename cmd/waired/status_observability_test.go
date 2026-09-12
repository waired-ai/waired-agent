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
		"2 running, 10 conversations kept warm",
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
	if !strings.Contains(out, "predates the observability API") {
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

// TestPrintObservabilitySection_Text_ModelLoading is the CLI half of
// waired-agent#1307. PRODUCT CONTRACT: this line does not say "ready"
// while the weights are being read into memory.
//
// The three values this line had — ready / not ready / engine failed —
// were all blind to it: the engine is up and the model file is on disk
// in every one of them. On the host that filed the issue that meant
// "ready", printed 2 s before a request that then waited 148 s.
//
// The elapsed figure rather than a word, per the owner ruling in
// docs/decisions/20260821/1130-first-token-is-shown-not-judged.md.
func TestPrintObservabilitySection_Text_ModelLoading(t *testing.T) {
	cold := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.ObservabilityState{
			Agent: management.AgentState{
				EngineReady:         true,
				ModelID:             "qwen3.8-flash-next",
				ShareEnabled:        true,
				ModelResident:       &cold,
				ModelLoading:        true,
				ModelLoadingSeconds: 16,
			},
		})
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() { printObservabilitySection(srv.URL, "") })
	line := engineLine(t, out)
	if strings.Contains(line, "ready") && !strings.Contains(line, "not ready") {
		t.Errorf("Engine line = %q, want it to stop claiming ready during a load", line)
	}
	if !strings.Contains(line, "loading the model") {
		t.Errorf("Engine line = %q, want `loading the model`", line)
	}
	if !strings.Contains(line, "16s") {
		t.Errorf("Engine line = %q, want the elapsed seconds", line)
	}
}

// Observed cold with no load running. Ordinarily brief — the residency
// maintainer starts one within a probe tick — but it is its own answer,
// because "nothing is loading it" is the case an operator can act on.
func TestPrintObservabilitySection_Text_ModelNotLoaded(t *testing.T) {
	cold := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.ObservabilityState{
			Agent: management.AgentState{
				EngineReady:   true,
				ModelID:       "qwen3:8b",
				ShareEnabled:  true,
				ModelResident: &cold,
			},
		})
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() { printObservabilitySection(srv.URL, "") })
	if line := engineLine(t, out); !strings.Contains(line, "model not loaded") {
		t.Errorf("Engine line = %q, want `model not loaded`", line)
	}
}

// An agent predating the fields says nothing about residency, and a
// daemon with inference off never observes it. Neither may be rendered
// as a fault: nil is "we have not looked".
func TestPrintObservabilitySection_Text_UnobservedResidencyStaysReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.ObservabilityState{
			Agent: management.AgentState{
				EngineReady: true, ModelID: "qwen3:8b", ShareEnabled: true,
			},
		})
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() { printObservabilitySection(srv.URL, "") })
	line := engineLine(t, out)
	if !strings.Contains(line, "ready") || strings.Contains(line, "not loaded") {
		t.Errorf("Engine line = %q, want the pre-#1307 reading for a silent daemon", line)
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
