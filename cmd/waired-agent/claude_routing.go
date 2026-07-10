package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// claudeRoutingController owns the boot-level unified per-class Claude routing
// policy: which route (auto / waired / anthropic) serves the main conversation
// and which serves subagents. It supersedes the former split route-mode (#580)
// + node-policy (#645/#665) controllers. It lives for the whole process
// lifetime so the setting works even before enrollment or while degraded.
// Reads are a lock-free atomic for the per-request intercept hot path; writes
// persist to the state dir so a choice survives an agent restart.
//
// Implements management.ClaudeRoutingControl. It also supplies the intercept
// Deps.ClassRoute / OnFallback / OnNodeFallback / OnServed hooks and the Claude
// surface selector's node-fallback recorder.
type claudeRoutingController struct {
	stateDir string
	logger   *slog.Logger

	// policy is the live per-class policy; atomic.Pointer keeps the intercept
	// hot path lock-free and readers Load() once per request so a concurrent
	// SetClass cannot tear the (main, sub) pair.
	policy atomic.Pointer[state.ClaudeRoutingPolicy]

	mu             sync.Mutex // serialises persisted read-modify-write + last* fields
	lastFallback   *management.ClaudeRoutingFallbackEvent
	lastLocalModel string
	lastServedBy   string // peer DeviceID; "" = this device

	ring *observability.Ring // optional; nil disables emission
}

func newClaudeRoutingController(stateDir string, initial state.ClaudeRoutingPolicy, logger *slog.Logger) *claudeRoutingController {
	if logger == nil {
		logger = slog.Default()
	}
	c := &claudeRoutingController{stateDir: stateDir, logger: logger}
	c.policy.Store(&initial)
	return c
}

// WithObservability wires the optional event ring so route changes and
// fallbacks emit events. Returns the receiver for chaining.
func (c *claudeRoutingController) WithObservability(r *observability.Ring) *claudeRoutingController {
	c.ring = r
	return c
}

// Policy is the lock-free read consumed by the intercept on every request.
func (c *claudeRoutingController) Policy() state.ClaudeRoutingPolicy {
	if p := c.policy.Load(); p != nil {
		return *p
	}
	return state.DefaultClaudeRoutingPolicy()
}

// RouteFor resolves one traffic class to a concrete route string
// (auto|waired|anthropic), collapsing the sub "same" sentinel onto main.
// Wired to intercept.Deps.ClassRoute.
func (c *claudeRoutingController) RouteFor(class string) string {
	return string(c.Policy().Effective(class))
}

// SetClass persists + applies one class's route (management.ClaudeRoutingControl).
func (c *claudeRoutingController) SetClass(_ context.Context, class string, route state.ClaudeRouteClass) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.Policy()
	next := prev
	var from state.ClaudeRouteClass
	switch class {
	case state.ClaudeClassMain:
		from, next.Main = prev.Main, route
	case state.ClaudeClassSub:
		from, next.Sub = prev.Sub, route
	default:
		return fmt.Errorf("claude routing: unknown traffic class %q", class)
	}
	if err := state.WriteDesiredClaudeRouting(c.stateDir, next); err != nil {
		return err
	}
	// Re-read what was persisted: WriteDesiredClaudeRouting coerces empty
	// fields, and the live policy must match the disk form.
	persisted, err := state.ReadDesiredClaudeRouting(c.stateDir)
	if err != nil {
		return err
	}
	c.policy.Store(&persisted)
	c.logger.Info("claude routing: class set", "class", class, "from", from, "to", route)
	if c.ring != nil && from != route {
		c.ring.Append(observability.Event{
			Kind: observability.KindClaudeNodeChange,
			ClaudeNodeChange: &observability.ClaudeNodeChangeEvent{
				Class:    class,
				FromKind: string(from),
				ToKind:   string(route),
			},
		})
	}
	return nil
}

// RecordFallback is the intercept OnFallback hook: an auto-routed request's
// pre-first-byte local error was rescued by the real Anthropic API. Records
// the most recent such fallback (direction=anthropic) so the CLI/tray can
// show that Waired kept Claude working.
func (c *claudeRoutingController) RecordFallback(reason string) {
	c.recordFallbackEvent("", "", reason, "anthropic")
	c.logger.Warn("claude routing: fallback to real Anthropic", "reason", reason)
}

// RecordNodeFallback is the intercept OnNodeFallback + claudeSelector hook: an
// anthropic-routed request whose upstream was unreachable, or a pinned-peer
// request whose peer was unavailable, was served locally instead. Records it
// (direction=local); the persisted policy is NOT demoted — routing resumes
// when the node returns — so this record is what keeps the degrade from being
// silent (the "Claude integration must not silently break" principle).
func (c *claudeRoutingController) RecordNodeFallback(class, peerDeviceID, reason string) {
	c.recordFallbackEvent(class, peerDeviceID, reason, "local")
	c.logger.Warn("claude routing: node unavailable, serving locally",
		"class", class, "peer", peerDeviceID, "reason", reason)
	if c.ring != nil {
		c.ring.Append(observability.Event{
			Kind: observability.KindClaudeNodeFallback,
			ClaudeNodeFallback: &observability.ClaudeNodeFallbackEvent{
				Class:        class,
				PeerDeviceID: peerDeviceID,
				Reason:       reason,
			},
		})
	}
}

func (c *claudeRoutingController) recordFallbackEvent(class, peer, reason, direction string) {
	c.mu.Lock()
	prev := int64(0)
	if c.lastFallback != nil {
		prev = c.lastFallback.Count
	}
	c.lastFallback = &management.ClaudeRoutingFallbackEvent{
		When:      time.Now().UTC(),
		Class:     class,
		Reason:    reason,
		Peer:      peer,
		Direction: direction,
		Count:     prev + 1,
	}
	c.mu.Unlock()
}

// RecordServed is the intercept OnServed hook: it remembers the catalog model
// id that answered the last waired-served Claude request plus the serving peer
// ("" = this device), so the statusline can show which model is doing the work
// and where (#601/#602). Fires per request, so it stays quiet in the logs.
func (c *claudeRoutingController) RecordServed(modelID, peerDeviceID string) {
	c.mu.Lock()
	c.lastLocalModel = modelID
	c.lastServedBy = peerDeviceID
	c.mu.Unlock()
}

// State reports the live policy + last fallback + last served local model
// (management.ClaudeRoutingControl).
func (c *claudeRoutingController) State() management.ClaudeRoutingState {
	c.mu.Lock()
	lf := c.lastFallback
	lm := c.lastLocalModel
	sb := c.lastServedBy
	c.mu.Unlock()
	return management.ClaudeRoutingState{
		Policy:         c.Policy(),
		LastFallback:   lf,
		LastLocalModel: lm,
		LastServedBy:   sb,
	}
}
