// Package testharness provides the agent-side dispatcher for test
// directives carried in the signed Network Map.
//
// The production binary compiles only types.go and dispatcher.go and
// uses NoopDispatcher: ActiveTestScenario is received but ignored.
// The testharness build (//go:build testharness) additionally compiles
// dispatcher_testharness.go (activeDispatcher), iptables_*.go, and the
// scenarios sub-package — those are the only files that perform
// privileged iptables operations, so the production binary never ships
// them.
package testharness

import (
	"context"
	"log/slog"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// Dispatcher applies test directives carried in the signed Network Map.
// Apply is invoked once per Network Map frame (after the reconciler
// applies the map); Stop is invoked on agent shutdown.
//
// Apply must be non-blocking — it is called from the agent's network-map
// stream consumer and must not back-pressure it on slow iptables work
// (issue #303). The active dispatcher hands the latest map to a worker
// goroutine (latest-wins: intermediate maps may coalesce) and returns
// immediately; the returned error is always nil. Applying a directive is
// idempotent: same scenario+peer+nonce+resolved-IP-set is a no-op; any
// change triggers Revert+Apply. Stop must be safe to call when no
// scenario is active and is idempotent across repeated calls.
type Dispatcher interface {
	Apply(ctx context.Context, nm *signer.NetworkMap) error
	Stop(ctx context.Context) error
}

// Scenario implements one test scenario's Apply / Revert behaviour.
//
// Apply must be idempotent — a second Apply with identical params
// either no-ops or replays the same effect. Revert must also be
// idempotent — calling Revert when Apply has not been called must be
// a no-op (the dispatcher's state machine never calls Revert without
// a prior Apply, but defensive Revert keeps Stop-after-crash safe).
type Scenario interface {
	ID() string
	Apply(ctx context.Context, p ScenarioParams) error
	Revert(ctx context.Context) error
}

// ScenarioParams carries the dispatcher-resolved inputs a scenario
// needs to run. Direction echoes the per-receiver wire value
// ("both"/"inbound"/"outbound") set by CP-side projection — the
// dispatcher does no additional translation. PeerEndpoints is the
// deduped sorted IP set extracted from nm.Peers[X].Endpoints (relay:
// entries excluded).
type ScenarioParams struct {
	PeerDeviceID  string
	PeerOverlayIP string
	PeerEndpoints []string
	Direction     string
	Nonce         int64
}

// Reporter receives one record per dispatcher state transition. The
// production NoopDispatcher never invokes Reporter; the testharness
// activeDispatcher emits one record per Apply / Revert / Stop / error.
//
// The label set is part of the contract with the testnet CI runner
// (Session 3): runner.sh polls Cloud Logging for state == "applied" /
// "reverted" matching the PUT-side scenario_id+nonce.
type Reporter interface {
	ReportScenario(state, scenarioID, peerDeviceID string, nonce int64, errMsg string)
}

// Reporter state values. Pinned for the runner.sh contract.
const (
	StateApplied         = "applied"
	StateReverted        = "reverted"
	StateApplyError      = "apply_error"
	StateRevertError     = "revert_error"
	StateUnknownScenario = "unknown_scenario"
)

// Logger is *slog.Logger. Aliased to keep dispatcher signatures compact
// across both builds.
type Logger = *slog.Logger
