package signer_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestNetworkMapWithoutDesiredState_NoNewFieldsInCanonical pins the
// waired#835 §6/§14 invariant the wire freeze rests on: when none of
// the desired-state fields are set, the canonical JSON bytes contain
// no trace of them, so maps served to pollers that did not declare
// CapabilityOnboardingV1 are byte-identical to pre-freeze maps and
// existing signatures keep verifying. A regression here (dropping an
// omitempty) would break signature verification fleet-wide.
func TestNetworkMapWithoutDesiredState_NoNewFieldsInCanonical(t *testing.T) {
	nm := scenarioTestNetworkMap()
	nm.Peers[0].InferenceState = &signer.InferenceState{
		Reachable: true,
		Type:      signer.InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		LastCheck: "2026-07-19T00:00:00Z",
	}
	canonical, err := signer.CanonicalJSON(nm)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	for _, key := range []string{"desired_engine", "desired_model_id", "desired_benchmark_gen"} {
		if bytes.Contains(canonical, []byte(`"`+key+`"`)) {
			t.Fatalf("canonical JSON unexpectedly contains %q:\n%s", key, canonical)
		}
	}
}

// TestNetworkMapWithDesiredState_RoundTripVerifies covers the capable-
// poller path: CP-injected desired-state on the Self entry round-trips
// through sign/verify, and tampering with any field (an on-path
// attacker rewriting the model ID, bumping the benchmark generation)
// is rejected.
func TestNetworkMapWithDesiredState_RoundTripVerifies(t *testing.T) {
	k, err := signer.Generate()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	nm := scenarioTestNetworkMap()
	nm.Self.InferenceState = &signer.InferenceState{
		Reachable:           true,
		Type:                signer.InferenceTypeOllama,
		Endpoint:            "http://127.0.0.1:11434",
		LastCheck:           "2026-07-19T00:00:00Z",
		DesiredEngine:       signer.InferenceTypeOllama,
		DesiredModelID:      "qwen3:8b",
		DesiredBenchmarkGen: 3,
	}
	signed, err := k.SignNetworkMap(nm)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := signer.VerifyNetworkMap(k.Public, signed); err != nil {
		t.Fatalf("verify: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*signer.NetworkMap)
	}{
		{"DesiredEngine", func(m *signer.NetworkMap) { m.Self.InferenceState.DesiredEngine = signer.InferenceTypeVLLM }},
		{"DesiredModelID", func(m *signer.NetworkMap) { m.Self.InferenceState.DesiredModelID = "evil:latest" }},
		{"DesiredBenchmarkGen", func(m *signer.NetworkMap) { m.Self.InferenceState.DesiredBenchmarkGen = 4 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tampered := signed
			self := *signed.Self.InferenceState
			tampered.Self.InferenceState = &self
			c.mutate(&tampered)
			if err := signer.VerifyNetworkMap(k.Public, tampered); err == nil {
				t.Fatalf("expected verification failure after tampering %s", c.name)
			}
		})
	}
}

// TestSetupProgress_RoundTrip pins the §7 push wire shape shared by
// the agent reporter and the CP intake (POST /v1/devices/self/
// setup-progress).
func TestSetupProgress_RoundTrip(t *testing.T) {
	progress := signer.SetupProgress{
		Steps: []signer.SetupStep{
			{ID: "engine_install", Status: signer.SetupStatusDone},
			{ID: "model_pull", Status: signer.SetupStatusRunning,
				CompletedBytes: 3221225472, TotalBytes: 8589934592},
			{ID: "benchmark", Status: signer.SetupStatusFailed,
				ErrorCode: signer.SetupErrorEngineNotReady, ErrorDetail: "probe: connection refused"},
		},
		Benchmark: &signer.SetupBenchmark{Gen: 3, MeasuredTokps: 78.2},
		LastCheck: "2026-07-19T00:00:00Z",
	}
	raw, err := json.Marshal(progress)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"steps", "id", "status", "completed_bytes",
		"total_bytes", "error_code", "error_detail", "benchmark", "gen",
		"measured_tokps", "last_check"} {
		if !bytes.Contains(raw, []byte(`"`+key+`"`)) {
			t.Fatalf("marshalled progress missing %q:\n%s", key, raw)
		}
	}
	var got signer.SetupProgress
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(progress, got) {
		t.Fatalf("round-trip mismatch:\nwant %+v\ngot  %+v", progress, got)
	}
}

// TestSetupProgress_OmitemptyKeepsHealthyPushSmall pins that optional
// fields stay off the wire when unset, so steady-state pushes carry no
// error/byte-count noise for the CP validator to clamp.
func TestSetupProgress_OmitemptyKeepsHealthyPushSmall(t *testing.T) {
	progress := signer.SetupProgress{
		Steps:     []signer.SetupStep{{ID: "engine_install", Status: signer.SetupStatusDone}},
		LastCheck: "2026-07-19T00:00:00Z",
	}
	raw, err := json.Marshal(progress)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"completed_bytes", "total_bytes",
		"error_code", "error_detail", "benchmark"} {
		if bytes.Contains(raw, []byte(`"`+key+`"`)) {
			t.Fatalf("marshalled progress unexpectedly contains %q:\n%s", key, raw)
		}
	}
}

// TestSetupEnums pins the validator helpers both wire ends rely on.
func TestSetupEnums(t *testing.T) {
	for _, s := range []string{signer.SetupStatusPending, signer.SetupStatusRunning,
		signer.SetupStatusDone, signer.SetupStatusFailed, signer.SetupStatusSkipped} {
		if !signer.IsValidSetupStatus(s) {
			t.Fatalf("IsValidSetupStatus(%q) = false, want true", s)
		}
	}
	if signer.IsValidSetupStatus("exploded") {
		t.Fatal(`IsValidSetupStatus("exploded") = true, want false`)
	}
	for _, c := range []string{"", signer.SetupErrorEngineNotReady, signer.SetupErrorDiskFull,
		signer.SetupErrorModelNotFound, signer.SetupErrorNetworkError,
		signer.SetupErrorPermissionDenied, signer.SetupErrorExecutorGone,
		signer.SetupErrorTimeout, signer.SetupErrorInternal} {
		if !signer.IsValidSetupErrorCode(c) {
			t.Fatalf("IsValidSetupErrorCode(%q) = false, want true", c)
		}
	}
	if signer.IsValidSetupErrorCode("sad") {
		t.Fatal(`IsValidSetupErrorCode("sad") = true, want false`)
	}
}

// TestCapabilityOnboardingV1_WireValue pins the capability literal:
// CP poll intake, distribution gate, and agent poller all compare this
// exact string, so a reword is a wire-protocol break, not a rename.
func TestCapabilityOnboardingV1_WireValue(t *testing.T) {
	if signer.CapabilityOnboardingV1 != "onboarding-v1" {
		t.Fatalf("CapabilityOnboardingV1 = %q, want %q",
			signer.CapabilityOnboardingV1, "onboarding-v1")
	}
}
