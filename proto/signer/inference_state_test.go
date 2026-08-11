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

// TestHardwareSummary_HostFitFields_CanonicalJSON is the byte-identity
// pin required of every additive proto change
// (docs/decisions/20260719/0000-concurrent-proto-development.md).
// UnifiedMemory / UsableVRAMMB / Vendor were added for the control
// plane's onboarding host-fit; an agent that does not set them must
// still produce EXACTLY the pre-addition bytes, because HardwareSummary
// rides the signed NetworkMap and a shifted encoding would churn the
// map for every peer on a rolling upgrade.
func TestHardwareSummary_HostFitFields_CanonicalJSON(t *testing.T) {
	// Discrete-GPU host that predates the three fields: byte-for-byte
	// the encoding published under proto/v0.2.3.
	legacy := HardwareSummary{
		GPUs: []HardwareGPUSummary{{
			Model:       "NVIDIA GeForce RTX 4090",
			VRAMTotalMB: 24564,
			ComputeCap:  "8.9",
		}},
		RAMTotalGB: 64,
	}
	const wantLegacy = `{"gpus":[{"model":"NVIDIA GeForce RTX 4090","vram_total_mb":24564,` +
		`"compute_cap":"8.9"}],"ram_total_gb":64}`
	data, err := json.Marshal(&legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if got := string(data); got != wantLegacy {
		t.Errorf("unset host-fit fields changed the encoding:\n got %s\nwant %s", got, wantLegacy)
	}

	// UMA host that does set them — the keys appear only when non-zero,
	// and in struct-declaration order.
	uma := HardwareSummary{
		GPUs: []HardwareGPUSummary{{
			Model:       "Apple M3 Max",
			VRAMTotalMB: 40960,
			Vendor:      "apple",
		}},
		RAMTotalGB:    64,
		UnifiedMemory: true,
		UsableVRAMMB:  49152,
	}
	const wantUMA = `{"gpus":[{"model":"Apple M3 Max","vram_total_mb":40960,"vendor":"apple"}],` +
		`"ram_total_gb":64,"unified_memory":true,"usable_vram_mb":49152}`
	data, err = json.Marshal(&uma)
	if err != nil {
		t.Fatalf("marshal uma: %v", err)
	}
	if got := string(data); got != wantUMA {
		t.Errorf("host-fit encoding drifted:\n got %s\nwant %s", got, wantUMA)
	}

	// And they survive a round trip (the CP reads them off the stored
	// push to decide which models it may offer for this device).
	var out HardwareSummary
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&uma, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", uma, out)
	}
}

// TestHardwareSummary_HostFitBackwardCompat pins the rolling-upgrade
// direction the other way: JSON written by an agent that predates the
// three fields parses cleanly, leaving them zero. The control plane
// reads a zero UsableVRAMMB as "unknown" and falls back to the GPU's
// raw VRAMTotalMB, so an old agent degrades to the previous behaviour
// rather than being judged unable to run anything.
func TestHardwareSummary_HostFitBackwardCompat(t *testing.T) {
	preJSON := []byte(`{"gpus":[{"model":"NVIDIA GeForce RTX 4090","vram_total_mb":24564,` +
		`"compute_cap":"8.9"}],"ram_total_gb":64}`)
	var hs HardwareSummary
	if err := json.Unmarshal(preJSON, &hs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if hs.UnifiedMemory {
		t.Errorf("UnifiedMemory = true, want false on a pre-addition payload")
	}
	if hs.UsableVRAMMB != 0 {
		t.Errorf("UsableVRAMMB = %d, want 0 on a pre-addition payload", hs.UsableVRAMMB)
	}
	if len(hs.GPUs) != 1 || hs.GPUs[0].Vendor != "" {
		t.Errorf("GPUs = %+v, want one GPU with an empty Vendor", hs.GPUs)
	}
}

// TestHardwareSummary_RAMAvailable_CanonicalJSON is the byte-identity
// pin for the #568 addition. RAMAvailableGB is the install-time
// available-memory measurement; a host that has not measured (and an
// agent that predates the field) sends 0, and 0 must keep the
// pre-addition encoding byte-for-byte, because HardwareSummary rides
// the signed NetworkMap and a shifted encoding would churn the map for
// every peer on a rolling upgrade.
func TestHardwareSummary_RAMAvailable_CanonicalJSON(t *testing.T) {
	// Unset: byte-for-byte the pre-#568 encoding.
	unmeasured := HardwareSummary{
		GPUs: []HardwareGPUSummary{{
			Model:       "NVIDIA GeForce RTX 4090",
			VRAMTotalMB: 24564,
			ComputeCap:  "8.9",
		}},
		RAMTotalGB: 64,
	}
	const wantUnmeasured = `{"gpus":[{"model":"NVIDIA GeForce RTX 4090","vram_total_mb":24564,` +
		`"compute_cap":"8.9"}],"ram_total_gb":64}`
	data, err := json.Marshal(&unmeasured)
	if err != nil {
		t.Fatalf("marshal unmeasured: %v", err)
	}
	if got := string(data); got != wantUnmeasured {
		t.Errorf("an unmeasured host changed the encoding:\n got %s\nwant %s", got, wantUnmeasured)
	}

	// Set: the key appears last, in struct-declaration order.
	measured := HardwareSummary{
		GPUs: []HardwareGPUSummary{{
			Model:       "Apple M3 Max",
			VRAMTotalMB: 40960,
			Vendor:      "apple",
		}},
		RAMTotalGB:     64,
		UnifiedMemory:  true,
		UsableVRAMMB:   49152,
		RAMAvailableGB: 41,
	}
	const wantMeasured = `{"gpus":[{"model":"Apple M3 Max","vram_total_mb":40960,"vendor":"apple"}],` +
		`"ram_total_gb":64,"unified_memory":true,"usable_vram_mb":49152,"ram_available_gb":41}`
	data, err = json.Marshal(&measured)
	if err != nil {
		t.Fatalf("marshal measured: %v", err)
	}
	if got := string(data); got != wantMeasured {
		t.Errorf("ram_available_gb encoding drifted:\n got %s\nwant %s", got, wantMeasured)
	}

	// Round trip (the CP reads it off the stored push to compute the
	// same OS deduction the agent computes).
	var out HardwareSummary
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&measured, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", measured, out)
	}

	// And a pre-addition payload parses with the field zero — which
	// every consumer treats as "measurement unavailable" and answers
	// with the OSMemoryAllowanceGB constant.
	var pre HardwareSummary
	if err := json.Unmarshal([]byte(wantUnmeasured), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition: %v", err)
	}
	if pre.RAMAvailableGB != 0 {
		t.Errorf("RAMAvailableGB = %d, want 0 on a pre-addition payload", pre.RAMAvailableGB)
	}
}

// TestCapabilityRAMAvailableV1_WireValue pins the capability literal:
// CP poll intake, distribution gate, and agent poller all compare this
// exact string, so a reword is a wire-protocol break, not a rename.
func TestCapabilityRAMAvailableV1_WireValue(t *testing.T) {
	if CapabilityRAMAvailableV1 != "ram-available-v1" {
		t.Fatalf("CapabilityRAMAvailableV1 = %q, want %q",
			CapabilityRAMAvailableV1, "ram-available-v1")
	}
}

// TestCapabilityRAMAvailableV2_WireValue pins the second literal, and
// that it is a DIFFERENT string from the first. The two gate different
// fields on the same struct (waired-agent#699): an agent declaring only
// v1 must keep receiving maps with no ram_available_measured_at, or it
// drops the key on canonical re-marshal and fails verification. Collapse
// the constants and that agent breaks.
func TestCapabilityRAMAvailableV2_WireValue(t *testing.T) {
	if CapabilityRAMAvailableV2 != "ram-available-v2" {
		t.Fatalf("CapabilityRAMAvailableV2 = %q, want %q",
			CapabilityRAMAvailableV2, "ram-available-v2")
	}
	if CapabilityRAMAvailableV2 == CapabilityRAMAvailableV1 {
		t.Fatalf("the two RAM-available capabilities must stay distinct, both = %q",
			CapabilityRAMAvailableV2)
	}
}

// TestHardwareSummary_RAMAvailableMeasuredAt_CanonicalJSON is the
// byte-identity pin for waired-agent#699. What it protects is the
// v1-only agent: the CP strips this key for a poller that has not
// declared v2, and the map it then serves has to be byte-for-byte what
// that agent verified before the field existed.
func TestHardwareSummary_RAMAvailableMeasuredAt_CanonicalJSON(t *testing.T) {
	// Stripped (or never set): byte-for-byte the pre-#699 encoding, which
	// is what a v1-only poller must keep receiving.
	v1Only := HardwareSummary{
		RAMTotalGB:     64,
		RAMAvailableGB: 41,
	}
	const wantV1Only = `{"ram_total_gb":64,"ram_available_gb":41}`
	data, err := json.Marshal(&v1Only)
	if err != nil {
		t.Fatalf("marshal v1-only: %v", err)
	}
	if got := string(data); got != wantV1Only {
		t.Errorf("a stripped payload changed the encoding:\n got %s\nwant %s", got, wantV1Only)
	}

	// Set: the key appears last, in struct-declaration order, after the
	// value it dates.
	measured := HardwareSummary{
		RAMTotalGB:             64,
		RAMAvailableGB:         41,
		RAMAvailableMeasuredAt: "2026-08-09T16:47:06.123456789Z",
	}
	const wantMeasured = `{"ram_total_gb":64,"ram_available_gb":41,` +
		`"ram_available_measured_at":"2026-08-09T16:47:06.123456789Z"}`
	data, err = json.Marshal(&measured)
	if err != nil {
		t.Fatalf("marshal measured: %v", err)
	}
	if got := string(data); got != wantMeasured {
		t.Errorf("ram_available_measured_at encoding drifted:\n got %s\nwant %s", got, wantMeasured)
	}

	var out HardwareSummary
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&measured, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", measured, out)
	}

	// A pre-addition payload parses with the field empty — no claim, and
	// nothing in the deduction arithmetic reads it.
	var pre HardwareSummary
	if err := json.Unmarshal([]byte(wantV1Only), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition: %v", err)
	}
	if pre.RAMAvailableMeasuredAt != "" {
		t.Errorf("RAMAvailableMeasuredAt = %q, want empty on a pre-addition payload",
			pre.RAMAvailableMeasuredAt)
	}
	if pre.RAMAvailableGB != 41 {
		t.Errorf("RAMAvailableGB = %d, want 41 — the timestamp's absence must not disturb the value",
			pre.RAMAvailableGB)
	}
}

// TestHardwareSummary_MemoryBandwidthSpec_CanonicalJSON is the same
// byte-identity pin for the #251 addition. It matters more than most:
// MemoryBandwidthSpecGBs is populated from a chip table, so the hosts
// that leave it unset are not just old agents — they are every part
// nobody has added yet, indefinitely. If omitempty did not hold here,
// those hosts would churn the signed NetworkMap for every peer.
func TestHardwareSummary_MemoryBandwidthSpec_CanonicalJSON(t *testing.T) {
	// Unset: byte-for-byte the pre-#251 encoding, including for a UMA
	// host, which is the case the field was added for.
	unknownPart := HardwareSummary{
		GPUs: []HardwareGPUSummary{{
			Model:       "Apple M9 Ultra",
			VRAMTotalMB: 262144,
			Vendor:      "apple",
		}},
		RAMTotalGB:    256,
		UnifiedMemory: true,
		UsableVRAMMB:  196608,
	}
	const wantUnknown = `{"gpus":[{"model":"Apple M9 Ultra","vram_total_mb":262144,"vendor":"apple"}],` +
		`"ram_total_gb":256,"unified_memory":true,"usable_vram_mb":196608}`
	data, err := json.Marshal(&unknownPart)
	if err != nil {
		t.Fatalf("marshal unknown part: %v", err)
	}
	if got := string(data); got != wantUnknown {
		t.Errorf("an unrecognised part changed the encoding:\n got %s\nwant %s", got, wantUnknown)
	}

	// Set, including a fractional figure — the M1 base is 68.25 GB/s, so
	// the field cannot be an int and the encoding has to keep the decimal.
	m1 := HardwareSummary{
		GPUs: []HardwareGPUSummary{{
			Model:       "Apple M1",
			VRAMTotalMB: 16384,
			Vendor:      "apple",
		}},
		RAMTotalGB:             16,
		UnifiedMemory:          true,
		UsableVRAMMB:           12288,
		MemoryBandwidthSpecGBs: 68.25,
	}
	const wantM1 = `{"gpus":[{"model":"Apple M1","vram_total_mb":16384,"vendor":"apple"}],` +
		`"ram_total_gb":16,"unified_memory":true,"usable_vram_mb":12288,` +
		`"memory_bandwidth_spec_gbs":68.25}`
	data, err = json.Marshal(&m1)
	if err != nil {
		t.Fatalf("marshal m1: %v", err)
	}
	if got := string(data); got != wantM1 {
		t.Errorf("bandwidth encoding drifted:\n got %s\nwant %s", got, wantM1)
	}

	var out HardwareSummary
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&m1, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", m1, out)
	}

	// The other rolling-upgrade direction: a pre-#251 payload parses
	// cleanly and leaves the field at 0, which every consumer must read as
	// "no claim" — hostfit then falls back to the population constant and
	// declines to exclude anything.
	var pre HardwareSummary
	if err := json.Unmarshal([]byte(wantUnknown), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition payload: %v", err)
	}
	if pre.MemoryBandwidthSpecGBs != 0 {
		t.Errorf("MemoryBandwidthSpecGBs = %v, want 0 on a pre-addition payload",
			pre.MemoryBandwidthSpecGBs)
	}
}

// TestInferenceState_NotShared_CanonicalJSON is the byte-identity pin
// required of every additive proto change
// (docs/decisions/20260719/0000-concurrent-proto-development.md), for the
// waired#1030 addition.
//
// Product contract, not a record of today: the DEFAULT is sharing ON
// (agentconfig's ShareWithMesh defaults to true), so the overwhelming
// majority of pushes must encode exactly as they did before the field
// existed. Getting this wrong would not merely churn bytes — an operator
// who never touched the toggle would start emitting a field older readers
// drop on canonical re-marshal.
func TestInferenceState_NotShared_CanonicalJSON(t *testing.T) {
	// Sharing ON (the default): byte-for-byte the pre-addition encoding of
	// a reachable ollama host.
	shared := InferenceState{
		Reachable: true,
		Type:      InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		Models:    []string{"qwen3:8b-q4_K_M"},
		LastCheck: "2026-08-02T12:00:00Z",
	}
	const wantShared = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"models":["qwen3:8b-q4_K_M"],"last_check":"2026-08-02T12:00:00Z"}`
	data, err := json.Marshal(&shared)
	if err != nil {
		t.Fatalf("marshal shared: %v", err)
	}
	if got := string(data); got != wantShared {
		t.Errorf("the default (sharing on) changed the encoding:\n got %s\nwant %s", got, wantShared)
	}

	// Sharing OFF: exactly one key more, at the tail (struct-declaration
	// order). The engine keeps reporting itself — the point of the field is
	// that the push no longer stops.
	notShared := shared
	notShared.NotShared = true
	const wantNotShared = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"models":["qwen3:8b-q4_K_M"],"last_check":"2026-08-02T12:00:00Z","not_shared":true}`
	data, err = json.Marshal(&notShared)
	if err != nil {
		t.Fatalf("marshal not-shared: %v", err)
	}
	if got := string(data); got != wantNotShared {
		t.Errorf("not-shared encoding drifted:\n got %s\nwant %s", got, wantNotShared)
	}

	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&notShared, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", notShared, out)
	}

	// The other rolling-upgrade direction: a pre-addition payload parses
	// cleanly and leaves the field false, which the control plane must read
	// as "this device is sharing" — the same answer it gave before the
	// field existed, so an agent that predates it is never withheld from
	// its peers.
	var pre InferenceState
	if err := json.Unmarshal([]byte(wantShared), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition payload: %v", err)
	}
	if pre.NotShared {
		t.Errorf("NotShared = true on a pre-addition payload; a legacy agent must read as sharing")
	}
}

// TestInferenceState_ContextWindow_CanonicalJSON pins the byte-identity
// that lets an undeclared window ride the SIGNED map unchanged.
//
// This field differs from the fields above in where it appears: it is
// agent-reported and travels on PEER entries, not only on the poller's
// own Self. So the encoding of a device that declares nothing has to be
// exactly what it was before the field existed, or every peer entry in
// every map changes shape at once.
func TestInferenceState_ContextWindow_CanonicalJSON(t *testing.T) {
	undeclared := InferenceState{
		Reachable: true,
		Type:      InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		Models:    []string{"qwen3:8b-q4_K_M"},
		LastCheck: "2026-08-02T12:00:00Z",
	}
	const wantUndeclared = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"models":["qwen3:8b-q4_K_M"],"last_check":"2026-08-02T12:00:00Z"}`
	data, err := json.Marshal(&undeclared)
	if err != nil {
		t.Fatalf("marshal undeclared: %v", err)
	}
	if got := string(data); got != wantUndeclared {
		t.Errorf("a device declaring no window changed the encoding:\n got %s\nwant %s",
			got, wantUndeclared)
	}

	declared := undeclared
	declared.ContextWindow = 200704
	const wantDeclared = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"models":["qwen3:8b-q4_K_M"],"last_check":"2026-08-02T12:00:00Z","context_window":200704}`
	data, err = json.Marshal(&declared)
	if err != nil {
		t.Fatalf("marshal declared: %v", err)
	}
	if got := string(data); got != wantDeclared {
		t.Errorf("declared-window encoding drifted:\n got %s\nwant %s", got, wantDeclared)
	}

	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&declared, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", declared, out)
	}

	// The rolling-upgrade direction that matters: a payload from an agent
	// that predates the field leaves it 0, and 0 must mean "declares
	// nothing" — never "serves a zero-token window", which would black-hole
	// every legacy peer the moment a requester started filtering on it.
	var pre InferenceState
	if err := json.Unmarshal([]byte(wantUndeclared), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition payload: %v", err)
	}
	if pre.ContextWindow != 0 {
		t.Errorf("ContextWindow = %d on a pre-addition payload, want 0", pre.ContextWindow)
	}
}

// waired#1064 added two fields for the peer picker: the model a device is
// committed to, in the one namespace every host agrees on, and why it is or
// is not serving it. Both ride the SIGNED map on peer entries, so the same
// byte-identity rule ContextWindow documents applies — a device that
// declares neither has to encode exactly as it did before they existed.
func TestInferenceState_ActiveModelAndSubsystemState_CanonicalJSON(t *testing.T) {
	undeclared := InferenceState{
		Reachable: true,
		Type:      InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		Models:    []string{"qwen3:8b-q4_K_M"},
		LastCheck: "2026-08-02T12:00:00Z",
	}
	const wantUndeclared = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"models":["qwen3:8b-q4_K_M"],"last_check":"2026-08-02T12:00:00Z"}`
	data, err := json.Marshal(&undeclared)
	if err != nil {
		t.Fatalf("marshal undeclared: %v", err)
	}
	if got := string(data); got != wantUndeclared {
		t.Errorf("a device declaring neither field changed the encoding:\n got %s\nwant %s",
			got, wantUndeclared)
	}

	declared := undeclared
	declared.ActiveModel = "qwen3-8b-instruct"
	declared.SubsystemState = SubsystemStateReady
	const wantDeclared = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"models":["qwen3:8b-q4_K_M"],"last_check":"2026-08-02T12:00:00Z",` +
		`"active_model":"qwen3-8b-instruct","subsystem_state":"ready"}`
	data, err = json.Marshal(&declared)
	if err != nil {
		t.Fatalf("marshal declared: %v", err)
	}
	if got := string(data); got != wantDeclared {
		t.Errorf("declared encoding drifted:\n got %s\nwant %s", got, wantDeclared)
	}

	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&declared, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", declared, out)
	}

	// PRODUCT CONTRACT (waired#1064): the state is reported INDEPENDENTLY of
	// Models, which is the opposite of ContextWindow. A device mid-pull has
	// withdrawn its advertisement — that is exactly the case the field was
	// added to explain, so an empty Models must not suppress it.
	pulling := InferenceState{
		Reachable:      true,
		Type:           InferenceTypeOllama,
		Endpoint:       "http://127.0.0.1:11434",
		LastCheck:      "2026-08-02T12:00:00Z",
		ActiveModel:    "qwen3-8b-instruct",
		SubsystemState: SubsystemStateLoading,
	}
	const wantPulling = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-08-02T12:00:00Z","active_model":"qwen3-8b-instruct",` +
		`"subsystem_state":"loading"}`
	data, err = json.Marshal(&pulling)
	if err != nil {
		t.Fatalf("marshal pulling: %v", err)
	}
	if got := string(data); got != wantPulling {
		t.Errorf("mid-pull encoding drifted:\n got %s\nwant %s", got, wantPulling)
	}

	// The rolling-upgrade direction: a payload from an agent that predates
	// the fields leaves both empty, and empty must mean "declares nothing"
	// — never "runs no model" or "is in a state named by the empty string".
	var pre InferenceState
	if err := json.Unmarshal([]byte(wantUndeclared), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition payload: %v", err)
	}
	if pre.ActiveModel != "" || pre.SubsystemState != "" {
		t.Errorf("pre-addition payload decoded to ActiveModel=%q SubsystemState=%q, want both empty",
			pre.ActiveModel, pre.SubsystemState)
	}
}

// The accepted set is a wire contract: the control plane validates pushes
// against it, so a value the agent can produce and this rejects would drop
// the device's whole inference push, not just the field.
func TestIsValidSubsystemState(t *testing.T) {
	valid := []string{
		"initializing", "ready", "awaiting_model", "loading", "pull_failed",
		"degraded", "no_engine", "stopped", "starting", "engine_failed", "disabled",
	}
	for _, s := range valid {
		if !IsValidSubsystemState(s) {
			t.Errorf("IsValidSubsystemState(%q) = false, want true", s)
		}
	}
	// Empty is "declares nothing", handled by the consumer, not by this
	// predicate — see the doc comment.
	for _, s := range []string{"", "Ready", "READY", "unknown", "ok", "serving"} {
		if IsValidSubsystemState(s) {
			t.Errorf("IsValidSubsystemState(%q) = true, want false", s)
		}
	}
}

// TestHostSpeed_PrefillFloor_CanonicalJSON is the byte-identity pin the
// concurrent-proto rules require of a change to this contract face
// (docs/decisions/20260719/0000-concurrent-proto-development.md, decision
// 3). waired-agent#579 adds no FIELD here — it adds a Method value under
// which the existing fields say something different — so what needs
// pinning is the encoding of that shape, and above all which fields are
// absent from it.
//
// Product contract (waired-agent#579, the Stage 3 contract table on the
// issue).
func TestHostSpeed_PrefillFloor_CanonicalJSON(t *testing.T) {
	// A screen verdict: a prefill rate read at the calibration depth, the
	// turn as a LOWER BOUND in its OWN field, no turn_seconds at all, and
	// no decode rate because none was measured.
	floor := HostSpeed{
		ProbeModelID:     "qwen3.5-0.8b",
		DepthTokens:      21000,
		PromptTokens:     2812,
		PrefillTokps:     104.4,
		TurnFloorSeconds: 201.15,
		Method:           BenchmarkMethodOllamaPrefillFloor,
		Samples:          2,
		SpreadPct:        1.4,
		EngineKind:       "ollama",
		EngineVersion:    "0.31.1",
		MeasuredAt:       "2026-08-09T14:20:00Z",
	}
	const wantFloor = `{"probe_model_id":"qwen3.5-0.8b","depth_tokens":21000,` +
		`"prompt_tokens":2812,"prefill_tokps":104.4,"turn_floor_seconds":201.15,` +
		`"method":"ollama_prefill_floor","samples":2,"spread_pct":1.4,` +
		`"engine_kind":"ollama","engine_version":"0.31.1","measured_at":"2026-08-09T14:20:00Z"}`
	data, err := json.Marshal(&floor)
	if err != nil {
		t.Fatalf("marshal floor: %v", err)
	}
	if got := string(data); got != wantFloor {
		t.Errorf("prefill-floor encoding drifted:\n got %s\nwant %s", got, wantFloor)
	}

	// The two absences are the point of the owner ruling, so they are
	// asserted rather than left to the byte comparison to imply.
	//
	// turn_seconds absent: a consumer that has not been taught this method
	// reads "no measurement" and declines to judge. Had the bound ridden
	// turn_seconds, that same consumer would have read a bound as a
	// measured turn time.
	//
	// decode_tokps absent, not zero: a zero read as a measured decode rate
	// computes an infinite turn for a host this method says nothing about.
	for _, absent := range []string{"turn_seconds", "decode_tokps"} {
		if indexOf(string(data), absent) >= 0 {
			t.Errorf("%s appears in a prefill-floor payload: %s", absent, data)
		}
	}

	// A full measurement is unchanged by any of this — the same struct,
	// the turn in turn_seconds, the method it always carried.
	full := HostSpeed{
		ProbeModelID:  "qwen3.5-0.8b",
		DepthTokens:   21000,
		PromptTokens:  21066,
		PrefillTokps:  671.17,
		DecodeTokps:   28.47,
		TurnSeconds:   66.4,
		Method:        BenchmarkMethodOllamaEval,
		Samples:       3,
		SpreadPct:     1.4,
		EngineKind:    "ollama",
		EngineVersion: "0.31.1",
		MeasuredAt:    "2026-08-09T14:20:00Z",
	}
	const wantFull = `{"probe_model_id":"qwen3.5-0.8b","depth_tokens":21000,` +
		`"prompt_tokens":21066,"prefill_tokps":671.17,"decode_tokps":28.47,` +
		`"turn_seconds":66.4,"method":"ollama_eval","samples":3,"spread_pct":1.4,` +
		`"engine_kind":"ollama","engine_version":"0.31.1","measured_at":"2026-08-09T14:20:00Z"}`
	data, err = json.Marshal(&full)
	if err != nil {
		t.Fatalf("marshal full: %v", err)
	}
	if got := string(data); got != wantFull {
		t.Errorf("full-measurement encoding drifted:\n got %s\nwant %s", got, wantFull)
	}
	if indexOf(string(data), "turn_floor_seconds") >= 0 {
		t.Errorf("turn_floor_seconds appears in a full measurement: %s", data)
	}

	// The rolling-upgrade direction: a payload from an agent that predates
	// both additions leaves Method empty and TurnFloorSeconds zero, and a
	// zero bound has to mean "no bound reported" — never "instant".
	var pre HostSpeed
	if err := json.Unmarshal([]byte(`{"probe_model_id":"qwen3.5-0.8b","turn_seconds":66.4}`), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition payload: %v", err)
	}
	if pre.Method != "" || pre.TurnFloorSeconds != 0 {
		t.Errorf("pre-addition payload: Method=%q TurnFloorSeconds=%v, want empty and 0",
			pre.Method, pre.TurnFloorSeconds)
	}

	var out HostSpeed
	if err := json.Unmarshal([]byte(wantFloor), &out); err != nil {
		t.Fatalf("unmarshal floor: %v", err)
	}
	if !reflect.DeepEqual(floor, out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", floor, out)
	}
}

// TestInferenceState_LocalModelChoiceAt_CanonicalJSON is the byte-identity
// pin the concurrent-proto rules require of a change to this contract face
// (docs/decisions/20260719/0000-concurrent-proto-development.md, decision 3).
//
// Product contract (waired-agent#647, the wire-contract field table on the
// issue): the field is a timestamp only, it is absent unless a person at
// this host answered the model question, and its absence is "no claim".
func TestInferenceState_LocalModelChoiceAt_CanonicalJSON(t *testing.T) {
	// The common case — a device whose preference was never set by a person
	// here, or an agent that predates the field — is byte-identical to what
	// it encoded before the field existed.
	silent := InferenceState{
		Reachable:   true,
		Type:        InferenceTypeOllama,
		Endpoint:    "http://127.0.0.1:11434",
		Models:      []string{"qwen3:8b-q4_K_M"},
		LastCheck:   "2026-08-02T12:00:00Z",
		ActiveModel: "qwen3-8b-instruct",
	}
	const wantSilent = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"models":["qwen3:8b-q4_K_M"],"last_check":"2026-08-02T12:00:00Z",` +
		`"active_model":"qwen3-8b-instruct"}`
	data, err := json.Marshal(&silent)
	if err != nil {
		t.Fatalf("marshal silent: %v", err)
	}
	if got := string(data); got != wantSilent {
		t.Errorf("a device making no claim changed the encoding:\n got %s\nwant %s",
			got, wantSilent)
	}

	// The demotion case the field exists for: the operator was told the
	// assigned model measured slow, accepted the lighter one, and the device
	// now serves something other than what it was told to serve.
	chose := silent
	chose.ActiveModel = "qwen3.5-2b"
	chose.LocalModelChoiceAt = "2026-08-10T02:31:04.512Z"
	const wantChose = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"models":["qwen3:8b-q4_K_M"],"last_check":"2026-08-02T12:00:00Z",` +
		`"active_model":"qwen3.5-2b","local_model_choice_at":"2026-08-10T02:31:04.512Z"}`
	data, err = json.Marshal(&chose)
	if err != nil {
		t.Fatalf("marshal chose: %v", err)
	}
	if got := string(data); got != wantChose {
		t.Errorf("choice encoding drifted:\n got %s\nwant %s", got, wantChose)
	}

	// No model id rides with it: ActiveModel is the one answer to "which
	// model", and a second one on the same push could disagree with it.
	if indexOf(string(data), "local_model_choice_model") >= 0 {
		t.Errorf("the choice carries a model id of its own: %s", data)
	}

	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&chose, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", chose, out)
	}

	// The rolling-upgrade direction: a payload from an agent that predates
	// the field leaves it empty, and empty must mean "no claim" — never
	// "nobody has ever chosen on that host", which would license moving a
	// desired-state instruction the operator did give.
	var pre InferenceState
	if err := json.Unmarshal([]byte(wantSilent), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition payload: %v", err)
	}
	if pre.LocalModelChoiceAt != "" {
		t.Errorf("pre-addition payload decoded to LocalModelChoiceAt=%q, want empty",
			pre.LocalModelChoiceAt)
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
