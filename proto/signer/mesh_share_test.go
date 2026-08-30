package signer

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestInferenceState_DesiredShare_CanonicalJSON is the byte-identity pin
// required of every additive proto change
// (docs/decisions/20260719/0000-concurrent-proto-development.md §3), for
// the waired#1298 addition.
//
// InferenceState rides the signed NetworkMap, so a device the control
// plane has said nothing to must encode byte-for-byte as it does today
// or every existing signature stops verifying on a rolling upgrade.
func TestInferenceState_DesiredShare_CanonicalJSON(t *testing.T) {
	// No instruction: byte-for-byte the pre-addition encoding. This is
	// the whole fleet today, and every device nobody has configured.
	none := InferenceState{
		Reachable: true,
		Type:      InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		LastCheck: "2026-08-30T00:00:00Z",
	}
	const wantNone = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-08-30T00:00:00Z"}`
	data, err := json.Marshal(&none)
	if err != nil {
		t.Fatalf("marshal without an instruction: %v", err)
	}
	if got := string(data); got != wantNone {
		t.Errorf("a device with no mesh-share instruction changed the encoding:\n got %s\nwant %s", got, wantNone)
	}

	// Instructed: the key sits after desired_idle_timeout, in
	// struct-declaration order, next to the other control-plane asks.
	asked := InferenceState{
		Reachable:          true,
		Type:               InferenceTypeOllama,
		Endpoint:           "http://127.0.0.1:11434",
		LastCheck:          "2026-08-30T00:00:00Z",
		DesiredIdleTimeout: "30m0s",
		DesiredShare:       DesiredShareOff,
	}
	const wantAsked = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-08-30T00:00:00Z","desired_idle_timeout":"30m0s","desired_share":"off"}`
	data, err = json.Marshal(&asked)
	if err != nil {
		t.Fatalf("marshal with an instruction: %v", err)
	}
	if got := string(data); got != wantAsked {
		t.Errorf("desired_share encoding drifted:\n got %s\nwant %s", got, wantAsked)
	}

	// Round trip: the agent reads it off the signed map and must reach
	// the same setting the control plane wrote.
	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.DesiredShare != DesiredShareOff {
		t.Errorf("round trip lost the instruction: got %q, want %q", out.DesiredShare, DesiredShareOff)
	}

	// Canonical re-marshal is what signature verification does. A reader
	// that knows the field has to reproduce the control plane's canonical
	// bytes exactly, or the map it was sent stops verifying.
	before, err := CanonicalJSON(&asked)
	if err != nil {
		t.Fatalf("canonicalize the sent state: %v", err)
	}
	after, err := CanonicalJSON(&out)
	if err != nil {
		t.Fatalf("canonicalize the read state: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("canonical form changed across a round trip:\n sent %s\n read %s", before, after)
	}

	// A pre-addition payload parses with the field empty, which means "no
	// instruction" — the device keeps sharing the way it always has.
	var pre InferenceState
	if err := json.Unmarshal([]byte(wantNone), &pre); err != nil {
		t.Fatalf("unmarshal pre-addition: %v", err)
	}
	if pre.DesiredShare != "" {
		t.Errorf("DesiredShare = %q, want empty on a pre-addition payload", pre.DesiredShare)
	}
}

// TestInferenceState_DesiredShare_IsNotNotShared pins the pair apart.
// They answer different questions — the control plane's distribution ask
// travels down, the machine's own hard kill travels up — and a reader
// that collapsed them would let a switched-on distribution silently
// override a device that is refusing (waired#1297).
func TestInferenceState_DesiredShare_IsNotNotShared(t *testing.T) {
	both := InferenceState{
		Reachable:    true,
		Type:         InferenceTypeOllama,
		LastCheck:    "2026-08-30T00:00:00Z",
		DesiredShare: DesiredShareOn,
		NotShared:    true,
	}
	data, err := json.Marshal(&both)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out InferenceState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.DesiredShare != DesiredShareOn || !out.NotShared {
		t.Fatalf("the two fields did not survive together: desired_share=%q not_shared=%v",
			out.DesiredShare, out.NotShared)
	}
}

// TestCapabilityMeshShareV1_Value pins the literal, because both sides of
// the wire compare the string and only one of them is in this repo.
func TestCapabilityMeshShareV1_Value(t *testing.T) {
	if CapabilityMeshShareV1 != "mesh-share-v1" {
		t.Errorf("capability string changed: got %q", CapabilityMeshShareV1)
	}
}
