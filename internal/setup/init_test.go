package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
)

// fakeEnrollResult is a canned, fully-populated EnrollResult the enroll
// stub returns so the ConfigureInference hook can assert it received the
// real registration values.
func fakeEnrollResult() *EnrollResult {
	return &EnrollResult{
		DeviceID:     "dev-123",
		NetworkName:  "net-a",
		NetworkID:    "n1",
		OverlayIP:    "100.64.0.2",
		AccountEmail: "u@example.com",
	}
}

// stubEnroll points the package-level enrollFn seam at a canned outcome
// for the duration of the test, then restores it. This lets Init reach
// the ConfigureInference hook + Deploy without a fake Control Plane.
func stubEnroll(t *testing.T, res *EnrollResult, err error) {
	t.Helper()
	prev := enrollFn
	enrollFn = func(context.Context, EnrollOptions) (*EnrollResult, error) {
		return res, err
	}
	t.Cleanup(func() { enrollFn = prev })
}

// baseInitOpts returns InitOptions valid enough to pass
// validateInitOptions, with integration skipped (no host side effects).
func baseInitOpts(t *testing.T) InitOptions {
	t.Helper()
	return InitOptions{
		ControlURL:      "https://cp.example",
		StateDir:        t.TempDir(),
		Endpoint:        "1.2.3.4:51820",
		Inference:       defaultInference(),
		SkipIntegration: true,
	}
}

// TestInit_ConfigureInferenceReceivesEnrollResult proves the hook runs
// AFTER enroll (it is handed the just-produced EnrollResult, which also
// equals res.Enroll) and is invoked exactly once.
func TestInit_ConfigureInferenceReceivesEnrollResult(t *testing.T) {
	want := fakeEnrollResult()
	stubEnroll(t, want, nil)

	var calls int
	var got *EnrollResult
	opts := baseInitOpts(t)
	opts.SkipDeploy = true // isolate: only enroll + hook run.
	opts.ConfigureInference = func(_ context.Context, e *EnrollResult) (agentconfig.InferenceConfig, error) {
		calls++
		got = e
		return defaultInference(), nil
	}

	res, err := Init(context.Background(), opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if calls != 1 {
		t.Fatalf("ConfigureInference calls = %d, want 1", calls)
	}
	if got == nil || got.DeviceID != want.DeviceID || got.OverlayIP != want.OverlayIP {
		t.Errorf("hook got enroll %#v, want %#v", got, want)
	}
	if res.Enroll != want {
		t.Errorf("res.Enroll = %#v, want %#v", res.Enroll, want)
	}
}

// TestInit_ConfigureInferenceOverridesDeployInference proves the hook's
// return value (not opts.Inference) drives Deploy: opts says inference is
// enabled, the hook disables it, and Deploy records the disabled choice.
// That the hook's output reaches Deploy establishes hook-before-deploy.
func TestInit_ConfigureInferenceOverridesDeployInference(t *testing.T) {
	withOllamaInPATH(t)
	stubEnroll(t, fakeEnrollResult(), nil)

	enabled := defaultInference()
	enabled.Enabled = true

	opts := baseInitOpts(t)
	opts.Inference = enabled // opts wants inference ON …
	opts.AllowMutations = true
	opts.ConfigureInference = func(context.Context, *EnrollResult) (agentconfig.InferenceConfig, error) {
		disabled := defaultInference()
		disabled.Enabled = false // … but the hook turns it OFF.
		return disabled, nil
	}

	res, err := Init(context.Background(), opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Deploy == nil {
		t.Fatalf("res.Deploy = nil, want a deploy result")
	}
	if !hasNoteMatching(res.Deploy.Notes, "inference disabled by operator choice") {
		t.Errorf("Deploy did not use the hook's disabled config; notes = %#v", res.Deploy.Notes)
	}
}

// TestInit_ConfigureInferenceRunsWhenSkipDeploy proves the hook is
// invoked even when Deploy is skipped (it sits outside the SkipDeploy
// guard) so the CLI can still print status / persist config.
func TestInit_ConfigureInferenceRunsWhenSkipDeploy(t *testing.T) {
	stubEnroll(t, fakeEnrollResult(), nil)

	var calls int
	opts := baseInitOpts(t)
	opts.SkipDeploy = true
	opts.ConfigureInference = func(context.Context, *EnrollResult) (agentconfig.InferenceConfig, error) {
		calls++
		return defaultInference(), nil
	}

	res, err := Init(context.Background(), opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if calls != 1 {
		t.Errorf("ConfigureInference calls = %d, want 1 (must run even with SkipDeploy)", calls)
	}
	if res.Deploy != nil {
		t.Errorf("res.Deploy = %#v, want nil (SkipDeploy)", res.Deploy)
	}
}

// TestInit_ConfigureInferenceErrorAbortsBeforeDeploy proves a hook error
// is wrapped as "configure inference: …" and aborts Init fail-fast,
// before Deploy runs (res.Deploy stays nil).
func TestInit_ConfigureInferenceErrorAbortsBeforeDeploy(t *testing.T) {
	withOllamaInPATH(t)
	stubEnroll(t, fakeEnrollResult(), nil)

	opts := baseInitOpts(t)
	opts.AllowMutations = true
	opts.ConfigureInference = func(context.Context, *EnrollResult) (agentconfig.InferenceConfig, error) {
		return agentconfig.InferenceConfig{}, errors.New("boom")
	}

	res, err := Init(context.Background(), opts)
	if err == nil {
		t.Fatal("Init err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "configure inference:") {
		t.Errorf("err = %v, want it wrapped with 'configure inference:'", err)
	}
	if res.Deploy != nil {
		t.Errorf("res.Deploy = %#v, want nil (Deploy must not run after hook error)", res.Deploy)
	}
	if res.Enroll == nil {
		t.Errorf("res.Enroll = nil; enroll completed before the hook so it should be set")
	}
}

// TestInit_NilConfigureInferenceUsesOptsInference proves backward-compat:
// with no hook, Deploy uses opts.Inference verbatim.
func TestInit_NilConfigureInferenceUsesOptsInference(t *testing.T) {
	withOllamaInPATH(t)
	stubEnroll(t, fakeEnrollResult(), nil)

	disabled := defaultInference()
	disabled.Enabled = false

	opts := baseInitOpts(t)
	opts.Inference = disabled
	opts.AllowMutations = true
	opts.ConfigureInference = nil

	res, err := Init(context.Background(), opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Deploy == nil {
		t.Fatalf("res.Deploy = nil, want a deploy result")
	}
	if !hasNoteMatching(res.Deploy.Notes, "inference disabled by operator choice") {
		t.Errorf("Deploy did not use opts.Inference (disabled); notes = %#v", res.Deploy.Notes)
	}
}

// stubIntegration points the package-level integrationFn seam at a
// canned success and returns pointers that record how many times it ran
// and the IntegrationOptions Init built for it.
func stubIntegration(t *testing.T) (calls *int, got *IntegrationOptions) {
	t.Helper()
	calls = new(int)
	got = new(IntegrationOptions)
	prev := integrationFn
	integrationFn = func(_ context.Context, opts IntegrationOptions) (*IntegrationResult, error) {
		*calls++
		*got = opts
		return &IntegrationResult{}, nil
	}
	t.Cleanup(func() { integrationFn = prev })
	return calls, got
}

// TestInit_ConfigureIntegrationDecisionDrivesIntegration proves the
// hook's decision (Force) and opts.WiredBinary are threaded into the
// IntegrationOptions Init builds, and that both the hook and the
// integration phase run exactly once.
func TestInit_ConfigureIntegrationDecisionDrivesIntegration(t *testing.T) {
	stubEnroll(t, fakeEnrollResult(), nil)
	integCalls, integOpts := stubIntegration(t)

	var hookCalls int
	opts := baseInitOpts(t)
	opts.SkipDeploy = true
	opts.SkipIntegration = false
	opts.WiredBinary = "/opt/waired/bin/waired"
	opts.ConfigureIntegration = func(context.Context) (IntegrationDecision, error) {
		hookCalls++
		return IntegrationDecision{Force: true}, nil
	}

	res, err := Init(context.Background(), opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if hookCalls != 1 {
		t.Fatalf("ConfigureIntegration calls = %d, want 1", hookCalls)
	}
	if *integCalls != 1 {
		t.Fatalf("integration calls = %d, want 1", *integCalls)
	}
	if !integOpts.Force {
		t.Errorf("IntegrationOptions.Force = false, want true (from decision)")
	}
	if integOpts.WiredBinary != opts.WiredBinary {
		t.Errorf("IntegrationOptions.WiredBinary = %q, want %q", integOpts.WiredBinary, opts.WiredBinary)
	}
	if res.Integration == nil {
		t.Errorf("res.Integration = nil, want the integration result")
	}
}

// TestInit_ConfigureIntegrationSkipSkipsPhase proves Decision.Skip
// bypasses the integration phase entirely (operator declined, or the
// caller runs it out-of-process for the sudo hop).
func TestInit_ConfigureIntegrationSkipSkipsPhase(t *testing.T) {
	stubEnroll(t, fakeEnrollResult(), nil)
	integCalls, _ := stubIntegration(t)

	opts := baseInitOpts(t)
	opts.SkipDeploy = true
	opts.SkipIntegration = false
	opts.ConfigureIntegration = func(context.Context) (IntegrationDecision, error) {
		return IntegrationDecision{Skip: true}, nil
	}

	res, err := Init(context.Background(), opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if *integCalls != 0 {
		t.Errorf("integration calls = %d, want 0 (Skip)", *integCalls)
	}
	if res.Integration != nil {
		t.Errorf("res.Integration = %#v, want nil (Skip)", res.Integration)
	}
}

// TestInit_ConfigureIntegrationNotCalledWhenSkipIntegration proves the
// hook lives inside the SkipIntegration guard — the renew path (which
// sets SkipIntegration) must never prompt.
func TestInit_ConfigureIntegrationNotCalledWhenSkipIntegration(t *testing.T) {
	stubEnroll(t, fakeEnrollResult(), nil)
	integCalls, _ := stubIntegration(t)

	var hookCalls int
	opts := baseInitOpts(t) // SkipIntegration: true
	opts.SkipDeploy = true
	opts.ConfigureIntegration = func(context.Context) (IntegrationDecision, error) {
		hookCalls++
		return IntegrationDecision{Force: true}, nil
	}

	if _, err := Init(context.Background(), opts); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if hookCalls != 0 {
		t.Errorf("ConfigureIntegration calls = %d, want 0 (SkipIntegration)", hookCalls)
	}
	if *integCalls != 0 {
		t.Errorf("integration calls = %d, want 0 (SkipIntegration)", *integCalls)
	}
}

// TestInit_ConfigureIntegrationErrorAbortsPhase proves a hook error is
// wrapped as "configure integration: …" and aborts before any
// integration mutation.
func TestInit_ConfigureIntegrationErrorAbortsPhase(t *testing.T) {
	stubEnroll(t, fakeEnrollResult(), nil)
	integCalls, _ := stubIntegration(t)

	opts := baseInitOpts(t)
	opts.SkipDeploy = true
	opts.SkipIntegration = false
	opts.ConfigureIntegration = func(context.Context) (IntegrationDecision, error) {
		return IntegrationDecision{}, errors.New("boom")
	}

	_, err := Init(context.Background(), opts)
	if err == nil {
		t.Fatal("Init err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "configure integration:") {
		t.Errorf("err = %v, want it wrapped with 'configure integration:'", err)
	}
	if *integCalls != 0 {
		t.Errorf("integration calls = %d, want 0 (hook error must abort first)", *integCalls)
	}
}

// TestInit_NilConfigureIntegrationKeepsDetectGating proves
// backward-compat: with no hook, the phase still runs with Force=false
// (legacy Detect-gated semantics for library callers).
func TestInit_NilConfigureIntegrationKeepsDetectGating(t *testing.T) {
	stubEnroll(t, fakeEnrollResult(), nil)
	integCalls, integOpts := stubIntegration(t)

	opts := baseInitOpts(t)
	opts.SkipDeploy = true
	opts.SkipIntegration = false
	opts.ConfigureIntegration = nil

	if _, err := Init(context.Background(), opts); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if *integCalls != 1 {
		t.Fatalf("integration calls = %d, want 1", *integCalls)
	}
	if integOpts.Force {
		t.Errorf("IntegrationOptions.Force = true, want false (nil hook keeps Detect gating)")
	}
}
