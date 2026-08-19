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
