//go:build testharness && !linux

// Non-linux builds compile this stub instead of the iptables-driven
// scenario implementations. DefaultRegistry returns an empty map so
// activeDispatcher.NewActive reports StateUnknownScenario for every
// directive — Windows / Darwin agents participate in the testnet as
// observers but cannot inject iptables-style fault scenarios on the
// receive side.
package scenarios

import (
	"github.com/waired-ai/waired-agent/internal/testharness"
)

func DefaultRegistry() map[string]testharness.Scenario {
	return map[string]testharness.Scenario{}
}
