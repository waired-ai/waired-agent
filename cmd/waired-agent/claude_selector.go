package main

import (
	"context"
	"errors"

	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// claudeSelector is the Claude-intercept surface's Selector (#645/#647,
// reworked for the unified routing model). The per-class auto/waired/anthropic
// decision is made ABOVE this layer by the intercept; by the time a request
// reaches here it has already been dispatched locally (the "waired" leg, or
// the local-first leg of "auto"). This selector's only job is to pick WHICH
// Waired node serves it — this device or a mesh peer — which follows the
// operator's `waired worker` routing preference, exactly as general inference
// does. Node selection thus lives in one place, not a Claude-specific policy.
//
// Unlike the :9474 overlay's localOnlySelector this selector may route to
// mesh peers — the Claude intercept is a LOCAL surface (loopback from Claude
// Code on this device), so dispatching to a peer here is one hop, and the
// peer's own overlay stays local-only (loop prevention unchanged).
//
// When the worker preference pins a peer that cannot serve the request right
// now, selection retries locally instead of failing the turn — the persisted
// preference is NOT demoted (routing resumes when the peer returns) and the
// fallback is recorded via onNodeFallback so it is never silent.
type claudeSelector struct {
	p *agentInferenceProvider
	// onNodeFallback records a pinned-node local fallback (class, peer
	// device id, reason). nil disables recording (tests).
	onNodeFallback func(class, peerDeviceID, reason string)
}

// classifyClaudeModel derives the traffic class from the ORIGINAL client
// model id, before any unknown-model remap (a remap would erase the marker):
// the managed-settings subagent label is the only robust marker (#646).
// Everything else — including all traffic from setups that predate the label
// — is main.
//
// This is the single classifier for every layer that needs the class: the
// gateway's Deps.ClassifyModel and the intercept's per-class route decision
// (Deps.ClassRoute wiring in proxy.go).
func classifyClaudeModel(modelID string) string {
	if modelID == claudemanaged.SubagentModelID {
		return state.ClaudeClassSub
	}
	return state.ClaudeClassMain
}

// workerPref reads the operator's live worker routing preference — the same
// node selection general inference uses (auto / local-only / peer-preferred /
// pinned). A nil provider routing accessor falls back to auto.
func (c *claudeSelector) workerPref() state.RoutingPreference {
	if c.p != nil && c.p.routing != nil {
		return c.p.routing()
	}
	return state.RoutingPreference{Mode: state.RoutingModeAuto}
}

// localFallbackRequest rewrites the request for the local retry leg: a pinned
// peer's model is usually not on local disk, so the local leg asks for the
// device-active model instead. The response body echoes the client's original
// model id regardless (gateway contract), so the rewrite only affects
// selection + telemetry.
func (c *claudeSelector) localFallbackRequest(req router.Request) router.Request {
	if id, ok := c.p.ActiveModelID(); ok {
		req.Model = id
	}
	return req
}

// pinFellThrough reports whether a pinned-mode selection error is one of the
// two "the pin cannot serve this right now" shapes that warrant the
// non-destructive local retry. ErrAllPeersOverloaded is deliberately
// excluded: the pin IS serving, it's at capacity — surface the 503 +
// Retry-After (overflow policy is #651).
func pinFellThrough(err error) (reason string, ok bool) {
	switch {
	case errors.Is(err, router.ErrPinnedPeerUnreachable):
		return "unreachable", true
	case errors.Is(err, router.ErrModelNotReady):
		return "model_not_ready", true
	default:
		return "", false
	}
}

func (c *claudeSelector) recordFallback(class, peer, reason string) {
	if c.onNodeFallback != nil {
		c.onNodeFallback(class, peer, reason)
	}
}

// selectWithWorkerPref is the one implementation of the worker-preference
// selection + non-destructive pin fallback shared by Select and SelectK, so
// the fallback semantics cannot drift between the two entry points. run
// executes one selection against a Selector built for the given preference.
func selectWithWorkerPref[T any](ctx context.Context, c *claudeSelector, req router.Request,
	run func(ctx context.Context, sel *router.Selector, req router.Request) (T, error),
) (T, error) {
	pref := c.workerPref()
	out, err := run(ctx, c.p.buildSelectorWith(ctx, pref), req)
	if err == nil || pref.Mode != state.RoutingModePinned {
		return out, err
	}
	reason, retry := pinFellThrough(err)
	if !retry {
		var zero T
		return zero, err
	}
	c.recordFallback(req.Class, pref.PinnedPeerDeviceID, reason)
	local := state.RoutingPreference{Mode: state.RoutingModeLocalOnly}
	return run(ctx, c.p.buildSelectorWith(ctx, local), c.localFallbackRequest(req))
}

func (c *claudeSelector) Select(ctx context.Context, req router.Request) (router.Selection, error) {
	return selectWithWorkerPref(ctx, c, req,
		func(ctx context.Context, sel *router.Selector, req router.Request) (router.Selection, error) {
			return sel.Select(ctx, req)
		})
}

func (c *claudeSelector) SelectK(ctx context.Context, req router.Request, k int) ([]router.Candidate, error) {
	return selectWithWorkerPref(ctx, c, req,
		func(ctx context.Context, sel *router.Selector, req router.Request) ([]router.Candidate, error) {
			return sel.SelectK(ctx, req, k)
		})
}
