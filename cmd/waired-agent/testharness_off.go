//go:build !testharness

package main

import (
	"github.com/waired-ai/waired-agent/internal/network/disco"
	"github.com/waired-ai/waired-agent/internal/testharness"
)

// newTestHarnessDispatcher returns the production NoopDispatcher in
// the default build. Active test scenarios in the signed Network Map
// are received but silently ignored — neither the iptables ops nor
// the scenario implementations are compiled into this binary.
//
// discoSvc is ignored (production builds don't subscribe to disco's
// CMM hook); the parameter exists to keep the signature aligned with
// the //go:build testharness variant.
func newTestHarnessDispatcher(_ testharness.Logger, _ testharness.Reporter, _ string, _ *disco.Service) testharness.Dispatcher {
	return testharness.NoopDispatcher{}
}
