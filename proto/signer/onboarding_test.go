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
	for _, key := range []string{"desired_engine", "desired_model_id", "desired_benchmark_gen",
		"desired_integrations"} {
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
		DesiredIntegrations: &signer.DesiredIntegrations{
			Enabled: []string{signer.IntegrationClaudeCode, signer.IntegrationOpenCode},
		},
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
		// Replaces the pointer rather than editing the slice in place:
		// the copy above is shallow, so mutating Enabled would rewrite
		// the signed map every later subtest verifies against.
		{"DesiredIntegrations", func(m *signer.NetworkMap) {
			m.Self.InferenceState.DesiredIntegrations = &signer.DesiredIntegrations{
				Enabled: []string{signer.IntegrationClaudeCode, signer.IntegrationOpenClaw},
			}
		}},
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
			{ID: "engine_download", Status: signer.SetupStatusDone,
				CompletedBytes: 1503238553, TotalBytes: 1503238553, RateBps: 72800000},
			{ID: "engine_install", Status: signer.SetupStatusDone},
			{ID: "model_pull", Status: signer.SetupStatusRunning,
				CompletedBytes: 3221225472, TotalBytes: 8589934592, RateBps: 41943040},
			{ID: "benchmark", Status: signer.SetupStatusFailed,
				ErrorCode: signer.SetupErrorEngineNotReady, ErrorDetail: "probe: connection refused"},
			{ID: "integration", Status: signer.SetupStatusPending},
		},
		Benchmark: &signer.SetupBenchmark{
			Gen: 3, MeasuredTokps: 78.2,
			Trial: 2, Trials: 3, SampleTokps: 80.1, MedianTokps: 78.2, SpreadPct: 4.5,
			Method: signer.BenchmarkMethodOllamaEval,
		},
		LastCheck: "2026-07-19T00:00:00Z",
		Driver:    signer.SetupDriverBrowser,
	}
	raw, err := json.Marshal(progress)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"steps", "id", "status", "completed_bytes",
		"total_bytes", "rate_bps", "error_code", "error_detail", "benchmark", "gen",
		"measured_tokps", "trial", "trials", "sample_tokps", "median_tokps",
		"spread_pct", "method", "last_check", "driver"} {
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
	for _, key := range []string{"completed_bytes", "total_bytes", "rate_bps",
		"error_code", "error_detail", "benchmark", "driver"} {
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
	// Empty is valid for both v2 enums: an onboarding-v1 agent reports
	// neither, and rejecting its push over a field it cannot know would
	// take the whole progress report down with it.
	for _, d := range []string{"", signer.SetupDriverBrowser, signer.SetupDriverTerminal} {
		if !signer.IsValidSetupDriver(d) {
			t.Fatalf("IsValidSetupDriver(%q) = false, want true", d)
		}
	}
	if signer.IsValidSetupDriver("tray") {
		t.Fatal(`IsValidSetupDriver("tray") = true, want false`)
	}
	for _, m := range []string{"", signer.BenchmarkMethodOllamaEval,
		signer.BenchmarkMethodOpenAISlope, signer.BenchmarkMethodWallClock} {
		if !signer.IsValidBenchmarkMethod(m) {
			t.Fatalf("IsValidBenchmarkMethod(%q) = false, want true", m)
		}
	}
	if signer.IsValidBenchmarkMethod("vibes") {
		t.Fatal(`IsValidBenchmarkMethod("vibes") = true, want false`)
	}
	// Integration targets are the one enum where empty is NOT valid:
	// unlike a status the agent may not report yet, an entry in
	// Enabled names something to configure, and "" names nothing.
	for _, target := range []string{signer.IntegrationClaudeCode,
		signer.IntegrationOpenCode, signer.IntegrationOpenClaw} {
		if !signer.IsValidIntegrationTarget(target) {
			t.Fatalf("IsValidIntegrationTarget(%q) = false, want true", target)
		}
	}
	for _, target := range []string{"", "emacs"} {
		if signer.IsValidIntegrationTarget(target) {
			t.Fatalf("IsValidIntegrationTarget(%q) = true, want false", target)
		}
	}
}

// TestDesiredIntegrations_ThreeStates pins the reason the field is a
// pointer to a struct rather than a bare []string: "asked, and every
// toggle is off" has to stay distinguishable from "never asked", or the
// wizard cannot tell an integration step that is never coming from a
// device that was never given the instruction.
func TestDesiredIntegrations_ThreeStates(t *testing.T) {
	cases := []struct {
		name string
		in   *signer.DesiredIntegrations
		want string
	}{
		{"no instruction", nil, `{}`},
		{"asked, all off", &signer.DesiredIntegrations{}, `{"desired_integrations":{}}`},
		{"asked, one on",
			&signer.DesiredIntegrations{Enabled: []string{signer.IntegrationOpenCode}},
			`{"desired_integrations":{"enabled":["opencode"]}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(struct {
				D *signer.DesiredIntegrations `json:"desired_integrations,omitempty"`
			}{D: c.in})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != c.want {
				t.Fatalf("marshalled %s:\n got %s\nwant %s", c.name, raw, c.want)
			}
		})
	}
}

// TestIntegrationTargets_WireValues pins the target literals to the
// agent's own adapter IDs (internal/integration.AgentID). proto may not
// import internal/, so the two lists are only kept in step by this
// test failing when one side is reworded.
func TestIntegrationTargets_WireValues(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{signer.IntegrationClaudeCode, "claude-code"},
		{signer.IntegrationOpenCode, "opencode"},
		{signer.IntegrationOpenClaw, "openclaw"},
	} {
		if c.got != c.want {
			t.Fatalf("integration target = %q, want %q", c.got, c.want)
		}
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

// TestCapabilityOnboardingV2_WireValue does the same for the v2 gate,
// which the CP compares before emitting DesiredIntegrations. v1 must
// keep its own value: the two are separate declarations, and folding
// them would emit a signed-map field to agents that reject it.
func TestCapabilityOnboardingV2_WireValue(t *testing.T) {
	if signer.CapabilityOnboardingV2 != "onboarding-v2" {
		t.Fatalf("CapabilityOnboardingV2 = %q, want %q",
			signer.CapabilityOnboardingV2, "onboarding-v2")
	}
	if signer.CapabilityOnboardingV2 == signer.CapabilityOnboardingV1 {
		t.Fatal("onboarding v1 and v2 capabilities must stay distinct")
	}
}
