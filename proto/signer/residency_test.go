package signer

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// TestInferenceState_DesiredIdleTimeout_CanonicalJSON is the byte-identity
// pin required of every additive proto change
// (docs/decisions/20260719/0000-concurrent-proto-development.md §3), for
// the waired-agent#861 addition.
//
// It carries the weight the earlier desired-state pins do: InferenceState
// rides the signed NetworkMap, so a device with no instruction must
// encode byte-for-byte as it does today or every existing signature stops
// verifying on a rolling upgrade.
func TestInferenceState_DesiredIdleTimeout_CanonicalJSON(t *testing.T) {
	// No instruction: byte-for-byte the pre-addition encoding. This is
	// the whole fleet today, and every device the operator never touches.
	none := InferenceState{
		Reachable: true,
		Type:      InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		LastCheck: "2026-08-20T00:00:00Z",
	}
	const wantNone = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-08-20T00:00:00Z"}`
	data, err := json.Marshal(&none)
	if err != nil {
		t.Fatalf("marshal without an instruction: %v", err)
	}
	if got := string(data); got != wantNone {
		t.Errorf("a device with no residency instruction changed the encoding:\n got %s\nwant %s", got, wantNone)
	}

	// Instructed: the key sits after desired_inference, in
	// struct-declaration order, next to the other CP-injected asks.
	asked := InferenceState{
		Reachable:          true,
		Type:               InferenceTypeOllama,
		Endpoint:           "http://127.0.0.1:11434",
		LastCheck:          "2026-08-20T00:00:00Z",
		DesiredInference:   DesiredInferenceOn,
		DesiredIdleTimeout: "30m0s",
	}
	const wantAsked = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-08-20T00:00:00Z","desired_inference":"on","desired_idle_timeout":"30m0s"}`
	data, err = json.Marshal(&asked)
	if err != nil {
		t.Fatalf("marshal with an instruction: %v", err)
	}
	if got := string(data); got != wantAsked {
		t.Errorf("desired_idle_timeout encoding drifted:\n got %s\nwant %s", got, wantAsked)
	}

	// Round trip: the agent reads it off the signed map and must reach
	// the same setting the control plane wrote.
	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&asked, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", asked, out)
	}

	// A pre-addition payload parses with the field empty, which means "no
	// instruction" — the device keeps whatever residency it has locally.
	var pre InferenceState
	if err := json.Unmarshal([]byte(wantNone), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition: %v", err)
	}
	if pre.DesiredIdleTimeout != "" {
		t.Errorf("DesiredIdleTimeout = %q, want empty on a pre-addition payload", pre.DesiredIdleTimeout)
	}
}

// TestInferenceState_DesiredIdleTimeout_ZeroIsAnInstruction pins the
// distinction the whole field rests on, and the one a consumer is most
// likely to collapse: "0s" is an instruction meaning hold the model
// indefinitely (the owner ruling on waired-agent#861, recorded in
// docs/decisions/20260820/0130-model-residency-is-a-setting.md), while
// the empty string is the absence of one.
//
// Collapsing them would make clearing the value in the control plane pin
// every device to a default instead of returning it to local control.
func TestInferenceState_DesiredIdleTimeout_ZeroIsAnInstruction(t *testing.T) {
	held := InferenceState{DesiredIdleTimeout: "0s"}
	data, err := json.Marshal(&held)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The four always-present fields are not omitempty; what matters here
	// is that the instruction itself is not swallowed by one.
	const want = `{"reachable":false,"type":"","endpoint":"","last_check":"",` +
		`"desired_idle_timeout":"0s"}`
	if got := string(data); got != want {
		t.Fatalf("got %s, want %s — an instruction to hold must survive omitempty", got, want)
	}

	// And the absence of one leaves no trace at all.
	empty, err := json.Marshal(&InferenceState{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if bytes.Contains(empty, []byte("desired_idle_timeout")) {
		t.Fatalf("no instruction still encoded the key: %s", empty)
	}

	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.DesiredIdleTimeout != "0s" {
		t.Errorf("DesiredIdleTimeout = %q, want %q", out.DesiredIdleTimeout, "0s")
	}
}

// TestCapabilityResidencyV1_WireValue pins the capability literal, and
// that it is distinct from its neighbours. The CP compares this exact
// string to decide whether to inject desired_idle_timeout at all, so a
// reword is a wire break rather than a rename — and an agent that
// receives the field without knowing it drops the key on canonical
// re-marshal and fails verification.
func TestCapabilityResidencyV1_WireValue(t *testing.T) {
	if CapabilityResidencyV1 != "residency-v1" {
		t.Fatalf("CapabilityResidencyV1 = %q, want %q", CapabilityResidencyV1, "residency-v1")
	}
	for _, other := range []string{
		CapabilityOnboardingV1, CapabilityOnboardingV2, CapabilityOnboardingV3,
		CapabilityOnboardingV4, CapabilityPublicShareV1, CapabilityContextWindowV1,
		CapabilityRAMAvailableV1, CapabilityRAMAvailableV2, CapabilityVRAMFreeV1,
	} {
		if CapabilityResidencyV1 == other {
			t.Fatalf("capability literals must stay distinct, both = %q", other)
		}
	}
}

// TestInferenceState_ResidencyReport_CanonicalJSON is the byte-identity
// pin for the upward half of model residency (waired#1232): what the
// device reports it is actually doing, and when a person here last chose
// it.
//
// Both fields are push-only — they must never reach a peer entry — but
// the pin still carries the usual weight, because InferenceState is one
// struct in both directions: an agent that fills in neither field has to
// encode exactly as it does today, or a rolling upgrade changes the bytes
// of a map that is already signed.
func TestInferenceState_ResidencyReport_CanonicalJSON(t *testing.T) {
	// Reports nothing: byte-for-byte the pre-addition encoding. This is
	// every agent in the fleet until the producer ships, and it stays the
	// encoding for a host that has no residency to report at all.
	silent := InferenceState{
		Reachable: true,
		Type:      InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		LastCheck: "2026-08-21T00:00:00Z",
	}
	const wantSilent = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-08-21T00:00:00Z"}`
	data, err := json.Marshal(&silent)
	if err != nil {
		t.Fatalf("marshal without a report: %v", err)
	}
	if got := string(data); got != wantSilent {
		t.Errorf("an agent reporting no residency changed the encoding:\n got %s\nwant %s", got, wantSilent)
	}

	// Reporting: both keys sit at the end, in struct-declaration order,
	// after the push-only block they belong to.
	reported := InferenceState{
		Reachable:              true,
		Type:                   InferenceTypeOllama,
		Endpoint:               "http://127.0.0.1:11434",
		LastCheck:              "2026-08-21T00:00:00Z",
		ResidencyIdleTimeout:   "45m0s",
		LocalResidencyChoiceAt: "2026-08-21T09:15:04.5Z",
	}
	const wantReported = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-08-21T00:00:00Z","residency_idle_timeout":"45m0s",` +
		`"local_residency_choice_at":"2026-08-21T09:15:04.5Z"}`
	data, err = json.Marshal(&reported)
	if err != nil {
		t.Fatalf("marshal with a report: %v", err)
	}
	if got := string(data); got != wantReported {
		t.Errorf("residency report encoding drifted:\n got %s\nwant %s", got, wantReported)
	}

	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&reported, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", reported, out)
	}

	// A pre-addition payload parses with both empty, which is "no claim"
	// on each axis independently.
	var pre InferenceState
	if err := json.Unmarshal([]byte(wantSilent), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition: %v", err)
	}
	if pre.ResidencyIdleTimeout != "" || pre.LocalResidencyChoiceAt != "" {
		t.Errorf("pre-addition payload reported residency %q / %q, want both empty",
			pre.ResidencyIdleTimeout, pre.LocalResidencyChoiceAt)
	}
}

// TestInferenceState_ResidencyReport_ZeroIsAValue pins on the report side
// the distinction the instruction side already rests on: "0s" is a
// reported value meaning the model is held indefinitely, and "" is the
// absence of a report.
//
// A consumer that collapses them reads a host that publishes nothing —
// an older agent, a host with no engine, or a vLLM host whose pool is
// reserved at start-up and has no idle setting at all — as one that
// reported "hold indefinitely". On the realignment path that is the
// difference between leaving an instruction alone and overwriting it
// with a value nobody chose.
func TestInferenceState_ResidencyReport_ZeroIsAValue(t *testing.T) {
	held := InferenceState{ResidencyIdleTimeout: "0s"}
	data, err := json.Marshal(&held)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"reachable":false,"type":"","endpoint":"","last_check":"",` +
		`"residency_idle_timeout":"0s"}`
	if got := string(data); got != want {
		t.Fatalf("got %s, want %s — a reported indefinite hold must survive omitempty", got, want)
	}

	silent, err := json.Marshal(&InferenceState{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	for _, key := range []string{"residency_idle_timeout", "local_residency_choice_at"} {
		if bytes.Contains(silent, []byte(key)) {
			t.Fatalf("reporting nothing still encoded %s: %s", key, silent)
		}
	}

	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ResidencyIdleTimeout != "0s" {
		t.Errorf("ResidencyIdleTimeout = %q, want %q", out.ResidencyIdleTimeout, "0s")
	}
}

// TestInferenceState_ResidencyDirectionsAreDistinctFields guards the one
// mistake that would make the whole arrangement circular: reusing
// DesiredIdleTimeout for the report, so the device echoes the instruction
// back and the control plane reads its own ask as confirmation.
//
// They are separate fields carrying separate facts, and a payload may
// legitimately hold two different values at once — that state is exactly
// what a realignment is for.
func TestInferenceState_ResidencyDirectionsAreDistinctFields(t *testing.T) {
	drifted := InferenceState{
		DesiredIdleTimeout:     "15m0s",
		ResidencyIdleTimeout:   "45m0s",
		LocalResidencyChoiceAt: "2026-08-21T09:15:04.5Z",
	}
	data, err := json.Marshal(&drifted)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.DesiredIdleTimeout != "15m0s" {
		t.Errorf("the instruction was altered by the report: %q", out.DesiredIdleTimeout)
	}
	if out.ResidencyIdleTimeout != "45m0s" {
		t.Errorf("the report was altered by the instruction: %q", out.ResidencyIdleTimeout)
	}
	if out.DesiredIdleTimeout == out.ResidencyIdleTimeout {
		t.Fatal("the two directions collapsed into one value")
	}
}

// TestInferenceState_ResidencyUnsupported_CanonicalJSON is the
// byte-identity pin required of every additive proto change
// (docs/decisions/20260719/0000-concurrent-proto-development.md §3), for
// the waired-agent#1030 addition.
//
// The pin carries more than the usual weight here because the field is a
// negative-sense bool: false must be indistinguishable from a payload
// written before the field existed, or a rolling upgrade re-encodes every
// ollama host's entry and the signatures stop verifying.
func TestInferenceState_ResidencyUnsupported_CanonicalJSON(t *testing.T) {
	// A host with a keep-alive axis — the whole fleet except vLLM — must
	// encode exactly as it did before the field existed.
	axis := InferenceState{
		Reachable:            true,
		Type:                 InferenceTypeOllama,
		Endpoint:             "http://127.0.0.1:9475",
		LastCheck:            "2026-08-27T00:00:00Z",
		ResidencyIdleTimeout: "0s",
	}
	const wantAxis = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:9475",` +
		`"last_check":"2026-08-27T00:00:00Z","residency_idle_timeout":"0s"}`
	data, err := json.Marshal(&axis)
	if err != nil {
		t.Fatalf("marshal a host with the axis: %v", err)
	}
	if got := string(data); got != wantAxis {
		t.Errorf("a host with a keep-alive axis changed the encoding:\n got %s\nwant %s", got, wantAxis)
	}

	// A vLLM host reports the same "0s" — that is the true reading of its
	// hold — and the new key is what separates the two. It sits directly
	// after residency_idle_timeout, in struct-declaration order.
	noAxis := InferenceState{
		Reachable:            true,
		Type:                 InferenceTypeVLLM,
		Endpoint:             "http://127.0.0.1:9479",
		LastCheck:            "2026-08-27T00:00:00Z",
		ResidencyIdleTimeout: "0s",
		ResidencyUnsupported: true,
	}
	const wantNoAxis = `{"reachable":true,"type":"vllm","endpoint":"http://127.0.0.1:9479",` +
		`"last_check":"2026-08-27T00:00:00Z","residency_idle_timeout":"0s",` +
		`"residency_unsupported":true}`
	data, err = json.Marshal(&noAxis)
	if err != nil {
		t.Fatalf("marshal a host without the axis: %v", err)
	}
	if got := string(data); got != wantNoAxis {
		t.Errorf("residency_unsupported encoding drifted:\n got %s\nwant %s", got, wantNoAxis)
	}

	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&noAxis, &out) {
		t.Errorf("round-trip mismatch\n in: %+v\nout: %+v", noAxis, out)
	}

	// A payload written before the addition parses as false, which every
	// consumer must read as "no claim, behave as before" — presets offered,
	// exactly as they were.
	var pre InferenceState
	if err := json.Unmarshal([]byte(wantAxis), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition: %v", err)
	}
	if pre.ResidencyUnsupported {
		t.Error("a pre-addition payload reported ResidencyUnsupported=true")
	}

	// The two facts are independent: a host may have an axis and no
	// report, and the absent report may not be read as an absent axis.
	silent, err := json.Marshal(&InferenceState{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if bytes.Contains(silent, []byte("residency_unsupported")) {
		t.Fatalf("reporting nothing still encoded residency_unsupported: %s", silent)
	}
}
