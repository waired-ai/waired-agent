package signer

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestInferenceState_Phase7Fields_RoundTrip ensures the two Phase 7
// fields (Hardware, Capacity) survive JSON marshal/unmarshal byte-for-
// byte. The InferenceState struct is the wire contract between agent
// push, Spanner, and NetworkMap distribution, so a silent drop on
// any of these would silently disable Phase 7 routing.
//
// PeerErrorRates and PeerRTTs were removed 20260517: both were
// wire-only with zero consumers. The Selector tie-break reads the
// agent's *own* error-window snapshot and disco RTT snapshot — RTT
// in particular is per-observer-pair (A→B differs from C→B), so
// publishing your view of the mesh as a hint for other peers was
// meaningless by construction.
func TestInferenceState_Phase7Fields_RoundTrip(t *testing.T) {
	in := InferenceState{
		Reachable: true,
		Type:      InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		Models:    []string{"qwen3:8b-q4_K_M"},
		LastCheck: "2026-05-14T12:00:00Z",
		Hardware: &HardwareSummary{
			GPUs: []HardwareGPUSummary{{
				Model:       "NVIDIA GeForce RTX 4090",
				VRAMTotalMB: 24564,
				ComputeCap:  "8.9",
			}},
			RAMTotalGB: 64,
		},
		Capacity: 8,
	}

	data, err := json.Marshal(&in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&in, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", in, out)
	}
}

// TestInferenceState_BackwardCompat verifies that JSON written by a
// pre-Phase-7 agent (only the original 6 fields) parses cleanly into
// the new struct, with all new fields at their zero values. The
// Phase 7 router treats zero Capacity as "unlimited" and empty maps
// as "no observations", so this is the documented graceful-degradation
// path during a rolling upgrade.
func TestInferenceState_BackwardCompat(t *testing.T) {
	preP7JSON := []byte(`{
		"reachable": true,
		"type": "ollama",
		"endpoint": "http://127.0.0.1:11434",
		"models": ["qwen3:8b-q4_K_M"],
		"last_check": "2026-05-14T12:00:00Z"
	}`)

	var state InferenceState
	if err := json.Unmarshal(preP7JSON, &state); err != nil {
		t.Fatalf("unmarshal pre-Phase-7 JSON: %v", err)
	}
	if state.Hardware != nil {
		t.Errorf("Hardware = %+v, want nil for pre-Phase-7 push", state.Hardware)
	}
	if state.Capacity != 0 {
		t.Errorf("Capacity = %d, want 0 (= unlimited) for pre-Phase-7 push", state.Capacity)
	}
	// Original fields must still parse correctly.
	if !state.Reachable || state.Type != InferenceTypeOllama || state.Endpoint == "" {
		t.Errorf("original fields lost in pre-Phase-7 parse: %+v", state)
	}
}

// TestInferenceState_OmitemptyOnZero ensures a zero-state push (e.g.
// from an agent that has no engine to expose) doesn't bloat the
// NetworkMap with empty JSON for every new field. NetworkMap is signed
// per device, and superfluous fields multiply bandwidth across N
// peers — the wire form must stay minimal.
func TestInferenceState_OmitemptyOnZero(t *testing.T) {
	zero := InferenceState{
		Reachable: false,
		Type:      InferenceTypeNone,
		LastCheck: "2026-05-14T12:00:00Z",
	}
	data, err := json.Marshal(&zero)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(data)
	for _, fragment := range []string{
		`"hardware"`,
		`"capacity"`,
		`"priority"`,
	} {
		if contains := indexOf(got, fragment) >= 0; contains {
			t.Errorf("zero-state push contains %s: %s", fragment, got)
		}
	}
}

// TestInferenceState_PriorityWireForm pins the on-wire encoding the
// requesting router and the older-agent compatibility story both depend on:
// Middle (the default, 0) is omitted, while High(1)/Low(-1) are emitted as a
// non-zero "priority" field. Low must serialize distinctly from the omitted
// default, otherwise an explicit Low would look identical to Middle.
func TestInferenceState_PriorityWireForm(t *testing.T) {
	cases := []struct {
		name     string
		priority int
		wantSub  string // substring that must appear ("" = must be absent)
	}{
		{"middle omitted", 0, ""},
		{"high emitted", 1, `"priority":1`},
		{"low emitted distinctly", -1, `"priority":-1`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := json.Marshal(&InferenceState{
				Reachable: true, Type: InferenceTypeOllama,
				LastCheck: "2026-05-14T12:00:00Z", Priority: c.priority,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := string(data)
			has := indexOf(got, `"priority"`) >= 0
			if c.wantSub == "" {
				if has {
					t.Errorf("priority %d should be omitted, got %s", c.priority, got)
				}
				return
			}
			if indexOf(got, c.wantSub) < 0 {
				t.Errorf("priority %d: want %s in %s", c.priority, c.wantSub, got)
			}
		})
	}
}

// TestHardwareSummary_OmitemptyOnEmpty verifies a HardwareSummary with
// no GPUs and no RAMTotalGB marshals to "{}" rather than verbose
// "{\"gpus\":null,\"ram_total_gb\":0}". The pointer-typed Hardware
// field in InferenceState handles the outer omit; this inner shape
// matters when Hardware is non-nil but truly empty (CPU-only host
// with unknown RAM).
func TestHardwareSummary_OmitemptyOnEmpty(t *testing.T) {
	hs := HardwareSummary{}
	data, err := json.Marshal(&hs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(data); got != "{}" {
		t.Errorf("empty HardwareSummary marshals to %q, want %q", got, "{}")
	}
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
