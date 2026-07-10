//go:build testharness && linux

package scenarios

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/testharness"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// asymmetricBlocker implements asymmetric-direct: each agent blocks
// only one direction of UDP between itself and the named peer, per
// the per-receiver Direction projection set by the CP (the sender
// agent gets Direction=outbound, the receiver gets Direction=inbound).
// The pairwise effect is "outbound from A drops on A, inbound to B
// drops on B" — i.e., A's packets never leave A AND B's reply attempts
// never reach B's listen port either way.
type asymmetricBlocker struct {
	ipt  testharness.IPTabler
	port int
}

func newAsymmetricBlocker(ipt testharness.IPTabler, port int) *asymmetricBlocker {
	return &asymmetricBlocker{ipt: ipt, port: port}
}

func (a *asymmetricBlocker) ID() string { return signer.ScenarioIDAsymmetricDirect }

func (a *asymmetricBlocker) Apply(ctx context.Context, p testharness.ScenarioParams) error {
	if err := testharness.InstallChain(ctx, a.ipt); err != nil {
		return err
	}
	return testharness.BlockUDPDirect(ctx, a.ipt, a.port, p.PeerEndpoints, p.Direction)
}

func (a *asymmetricBlocker) Revert(ctx context.Context) error {
	return testharness.FlushChain(ctx, a.ipt)
}
