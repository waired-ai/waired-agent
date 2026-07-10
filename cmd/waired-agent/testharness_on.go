//go:build testharness

package main

import (
	"github.com/waired-ai/waired-agent/internal/network/disco"
	"github.com/waired-ai/waired-agent/internal/testharness"
	"github.com/waired-ai/waired-agent/internal/testharness/scenarios"
)

// newTestHarnessDispatcher returns the active testharness dispatcher
// in the //go:build testharness build, wired with the four data-plane
// regression scenarios from scenarios.DefaultRegistry() and the
// optional disco service so scenarios can incrementally extend their
// iptables block when call_me_maybe surfaces additional peer
// endpoints. discoSvc may be nil (punch disabled / forced-relay /
// MultiplexBind unavailable), which falls back to NM-only behaviour.
func newTestHarnessDispatcher(log testharness.Logger, rep testharness.Reporter, selfDeviceID string, discoSvc *disco.Service) testharness.Dispatcher {
	var src testharness.DiscoEndpointSource
	if discoSvc != nil {
		src = discoSvc
	}
	return testharness.NewActive(log, rep, selfDeviceID, scenarios.DefaultRegistry(), src)
}
