//go:build testharness && linux

// Package scenarios contains the agent-side implementations of the
// four data-plane regression test scenarios driven by CP's
// /v1/test/scenario API. Linux-only because the scenarios drive
// iptables (`internal/testharness/iptables_linux.go`); on non-linux
// builds (`registry_other.go`) DefaultRegistry returns an empty map
// and the dispatcher reports StateUnknownScenario for every directive.
package scenarios

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/testharness"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fallbackBlocker implements the three "block UDP both directions"
// scenarios — fallback-basic, upgrade-basic, flap-suppression. The
// three share Apply / Revert behaviour; only the scenario id reported
// to the dispatcher and Reporter differs. The CI runner side
// distinguishes the three by polling for different post-conditions
// (path switch, dwell-window observation, etc.).
type fallbackBlocker struct {
	id   string
	ipt  testharness.IPTabler
	port int
}

func newFallbackBlocker(id string, ipt testharness.IPTabler, port int) *fallbackBlocker {
	return &fallbackBlocker{id: id, ipt: ipt, port: port}
}

func (f *fallbackBlocker) ID() string { return f.id }

func (f *fallbackBlocker) Apply(ctx context.Context, p testharness.ScenarioParams) error {
	if err := testharness.InstallChain(ctx, f.ipt); err != nil {
		return err
	}
	return testharness.BlockUDPDirect(ctx, f.ipt, f.port, p.PeerEndpoints, signer.ScenarioDirectionBoth)
}

func (f *fallbackBlocker) Revert(ctx context.Context) error {
	return testharness.FlushChain(ctx, f.ipt)
}
