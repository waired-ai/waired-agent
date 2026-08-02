package main

import (
	"context"

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
// The worker pin is FAIL-CLOSED here, exactly as on every other surface
// (waired-agent#325): when the pinned peer cannot serve the request the
// selection error propagates. This surface used to retry locally instead,
// which hid an explicit operator action — and, because the retry also
// rewrote the request to the device-active model, it quietly served a
// different model than the one the pin was chosen for. Whether the turn then
// fails outright or reaches the real Anthropic API is the per-class route's
// decision (`waired claude route`), taken above this layer.
type claudeSelector struct {
	p *agentInferenceProvider
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

// selectWithWorkerPref is the one implementation of the worker-preference
// selection shared by Select and SelectK, so node choice cannot drift between
// the two entry points. run executes one selection against a Selector built
// for the operator's live preference; its error — including a pinned peer
// that cannot serve — is returned untouched.
func selectWithWorkerPref[T any](ctx context.Context, c *claudeSelector, req router.Request,
	run func(ctx context.Context, sel *router.Selector, req router.Request) (T, error),
) (T, error) {
	return run(ctx, c.p.buildSelectorWith(ctx, c.workerPref()), req)
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
