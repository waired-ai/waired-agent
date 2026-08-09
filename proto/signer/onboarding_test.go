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
		"desired_integrations", "desired_model_gen"} {
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
			Enabled: []string{signer.IntegrationClaudeCode, signer.IntegrationOpenClaw},
		},
		DesiredModelGen:  2,
		DesiredInference: signer.DesiredInferenceOff,
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
				Enabled: []string{signer.IntegrationClaudeCode},
			}
		}},
		// An on-path attacker bumping this one costs the operator a
		// re-download rather than a wrong model, which is why it is a
		// counter and not a command — but it is signed all the same.
		{"DesiredModelGen", func(m *signer.NetworkMap) { m.Self.InferenceState.DesiredModelGen = 9 }},
		// An on-path attacker flipping off→on would restart local AI on
		// a host whose operator explicitly declined it (#597) — the
		// closed two-word set is validated CP-side, and the value is
		// signed here.
		{"DesiredInference", func(m *signer.NetworkMap) {
			m.Self.InferenceState.DesiredInference = signer.DesiredInferenceOn
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
		ModelGen:  2,
	}
	raw, err := json.Marshal(progress)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"steps", "id", "status", "completed_bytes",
		"total_bytes", "rate_bps", "error_code", "error_detail", "benchmark", "gen",
		"measured_tokps", "trial", "trials", "sample_tokps", "median_tokps",
		"spread_pct", "method", "last_check", "driver", "model_gen"} {
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
		"error_code", "error_detail", "benchmark", "driver", "model_gen"} {
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
		signer.SetupErrorSetupCommandNotRun, signer.SetupErrorTimeout,
		signer.SetupErrorInternal} {
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
		signer.IntegrationOpenClaw} {
		if !signer.IsValidIntegrationTarget(target) {
			t.Fatalf("IsValidIntegrationTarget(%q) = false, want true", target)
		}
	}
	// A retired target is not valid either — that is the whole mechanism
	// that lets a removed integration drain out of stored instructions
	// (waired-agent#333). Product contract, not a record of today's
	// behaviour: flipping it back would make agents that no longer carry
	// the adapter fail the coding-tools step instead of skipping it.
	for _, target := range []string{"", "emacs", signer.IntegrationOpenCode} {
		if signer.IsValidIntegrationTarget(target) {
			t.Fatalf("IsValidIntegrationTarget(%q) = true, want false", target)
		}
	}
}

// TestSetupErrorCodes_WireValues pins the literals NAVI keys its copy and
// recovery affordances off. A reword here is a wire break, not a rename:
// the control plane validator rejects an unknown code outright, and the
// wizard falls back to a generic failure for one it does not know.
//
// The three "this step has no author" codes are pinned together and
// asserted distinct because that is exactly what waired-agent#312 found
// collapsed. They answer three different questions:
//
//   - setup_command_not_run — it never ran here
//   - executor_gone — it ran and exited before reaching this row
//   - permission_denied — it ran and the write itself was refused
//
// The first two send the operator to the same command and the third does
// not, so folding any pair of them puts wrong copy on the wizard.
func TestSetupErrorCodes_WireValues(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{signer.SetupErrorEngineNotReady, "engine_not_ready"},
		{signer.SetupErrorDiskFull, "disk_full"},
		{signer.SetupErrorModelNotFound, "model_not_found"},
		{signer.SetupErrorNetworkError, "network_error"},
		{signer.SetupErrorPermissionDenied, "permission_denied"},
		{signer.SetupErrorExecutorGone, "executor_gone"},
		{signer.SetupErrorSetupCommandNotRun, "setup_command_not_run"},
		{signer.SetupErrorTimeout, "timeout"},
		{signer.SetupErrorInternal, "internal"},
	} {
		if c.got != c.want {
			t.Fatalf("setup error code = %q, want %q", c.got, c.want)
		}
	}
	for _, c := range []struct{ a, b string }{
		{signer.SetupErrorSetupCommandNotRun, signer.SetupErrorExecutorGone},
		{signer.SetupErrorSetupCommandNotRun, signer.SetupErrorPermissionDenied},
		{signer.SetupErrorExecutorGone, signer.SetupErrorPermissionDenied},
	} {
		if c.a == c.b {
			t.Fatalf("setup error codes %q and %q must stay distinct", c.a, c.b)
		}
	}
}

// TestIsRetiredIntegrationTarget separates the two ways a target can be
// invalid. The control plane needs the distinction: an id nobody ever
// shipped is a malformed request and earns a 4xx, while an id Waired
// itself withdrew is a stale row or a stale browser tab and must be
// dropped silently — failing the whole desired-state write over it would
// wedge setup on a value the operator never typed.
func TestIsRetiredIntegrationTarget(t *testing.T) {
	if !signer.IsRetiredIntegrationTarget(signer.IntegrationOpenCode) {
		t.Fatalf("IsRetiredIntegrationTarget(%q) = false, want true", signer.IntegrationOpenCode)
	}
	for _, target := range []string{"", "emacs",
		signer.IntegrationClaudeCode, signer.IntegrationOpenClaw} {
		if signer.IsRetiredIntegrationTarget(target) {
			t.Fatalf("IsRetiredIntegrationTarget(%q) = true, want false", target)
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
			&signer.DesiredIntegrations{Enabled: []string{signer.IntegrationOpenClaw}},
			`{"desired_integrations":{"enabled":["openclaw"]}}`},
		// A retired target still has to MARSHAL: rows written before the
		// removal keep naming it, and the agent has to receive it to
		// recognise and drop it. Retirement is a validation rule, not a
		// wire change.
		{"asked, retired target still on the wire",
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

// TestCapabilityOnboardingV3_WireValue does the same for the v3 gate,
// which the CP compares before emitting DesiredModelGen. All three must
// stay distinct for the same reason v1 and v2 do — a v2 agent has no
// applier for the model generation and would drop the field on
// canonical re-marshal, failing verification.
func TestCapabilityOnboardingV3_WireValue(t *testing.T) {
	if signer.CapabilityOnboardingV3 != "onboarding-v3" {
		t.Fatalf("CapabilityOnboardingV3 = %q, want %q",
			signer.CapabilityOnboardingV3, "onboarding-v3")
	}
	if signer.CapabilityOnboardingV3 == signer.CapabilityOnboardingV1 ||
		signer.CapabilityOnboardingV3 == signer.CapabilityOnboardingV2 {
		t.Fatal("onboarding v1, v2 and v3 capabilities must stay distinct")
	}
}
