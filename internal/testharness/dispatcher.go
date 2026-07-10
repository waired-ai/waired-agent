package testharness

import (
	"context"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// NoopDispatcher is the production-build Dispatcher. ActiveTestScenario
// directives in the signed Network Map are received by the agent but
// silently ignored — neither the iptables ops nor the scenario
// implementations are compiled into the production binary.
type NoopDispatcher struct{}

func (NoopDispatcher) Apply(_ context.Context, _ *signer.NetworkMap) error { return nil }
func (NoopDispatcher) Stop(_ context.Context) error                        { return nil }
