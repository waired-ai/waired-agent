package testharness

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// NoopDispatcher is the only thing compiled in production builds.
// These tests run without any build tag; they cover both the no-op
// behaviour AND that the parseHostFromAddr / ipSetEqual helpers exposed
// to the no-tag world stay correct (helpers themselves live in the
// testharness build, so this file only covers the noop surface).

func TestNoopDispatcher_AppliesScenarioAsNoOp(t *testing.T) {
	d := NoopDispatcher{}
	ctx := context.Background()
	nm := &signer.NetworkMap{
		ActiveTestScenario: &signer.ActiveTestScenario{
			ScenarioID:    signer.ScenarioIDFallbackBasic,
			PeerDeviceID:  "dev_b",
			Direction:     signer.ScenarioDirectionBoth,
			ExpectedNonce: 1,
		},
	}
	if err := d.Apply(ctx, nm); err != nil {
		t.Fatalf("Apply with scenario: %v", err)
	}
	if err := d.Apply(ctx, &signer.NetworkMap{}); err != nil {
		t.Fatalf("Apply without scenario: %v", err)
	}
	if err := d.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
