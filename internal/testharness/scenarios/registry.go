//go:build testharness && linux

package scenarios

import (
	"github.com/waired-ai/waired-agent/internal/testharness"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// dataPlanePort is the WireGuard listen port the agent uses on the
// data plane. Hardcoded to 51820 today because every testnet VM
// listens on the same port; if/when per-agent ports become a thing
// (e.g., bare-metal deployments), the dispatcher would need to surface
// the receiver's own listen port through ScenarioParams.
const dataPlanePort = 51820

// DefaultRegistry returns the four-scenario registry the testharness
// dispatcher uses. The three fallback variants share fallbackBlocker
// (different scenario id, identical Apply / Revert); asymmetric-direct
// uses asymmetricBlocker which routes BlockUDPDirect's direction by
// the per-agent Direction in ScenarioParams.
func DefaultRegistry() map[string]testharness.Scenario {
	ipt := testharness.ExecIPTabler{}
	return map[string]testharness.Scenario{
		signer.ScenarioIDFallbackBasic:    newFallbackBlocker(signer.ScenarioIDFallbackBasic, ipt, dataPlanePort),
		signer.ScenarioIDUpgradeBasic:     newFallbackBlocker(signer.ScenarioIDUpgradeBasic, ipt, dataPlanePort),
		signer.ScenarioIDFlapSuppression:  newFallbackBlocker(signer.ScenarioIDFlapSuppression, ipt, dataPlanePort),
		signer.ScenarioIDAsymmetricDirect: newAsymmetricBlocker(ipt, dataPlanePort),
	}
}
