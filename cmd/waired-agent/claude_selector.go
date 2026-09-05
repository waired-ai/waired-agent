package main

import (
	"net/http"

	"context"
	"fmt"
	"strings"

	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// claudeSelector is the Claude-intercept surface's Selector (#645/#647,
// reworked for the unified routing model). The per-class auto/waired/anthropic
// decision is made ABOVE this layer by the intercept; by the time a request
// reaches here it has already been dispatched locally. This selector's only job is to pick WHICH
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
// fails outright is decided above this layer — there is no route to the real
// Anthropic API left for a Waired id to take (waired-agent#1184).
type claudeSelector struct {
	p *agentInferenceProvider
}

// subagentIDHeader is the attribution id Claude Code stamps on a request
// from an agent it spawned inside the session, and only on such a request
// ("present only on requests from an agent Claude Code spawned inside the
// session" — https://code.claude.com/docs/en/llm-gateway-protocol#request-headers;
// measured on 2.1.261, 2026-09-06: absent on the main turn and on the title
// generation, present on the subagent's, with no parent-id header alongside
// it for a top-level subagent).
const subagentIDHeader = "X-Claude-Code-Agent-Id"

// classifyClaudeClass derives the traffic class from the request's headers.
//
// waired used to pin a model id of its own — `waired/subagent`, written into
// managed settings as CLAUDE_CODE_SUBAGENT_MODEL — purely so this function
// had something to read, and then every passthrough leg had to rewrite that
// id back into a real one before it could leave the machine. The header says
// the same thing and is Claude Code's own (waired-agent#1186). It is also
// strictly better at the job: the label missed any subagent whose definition
// pinned a model, because that model id is what arrived.
//
// This is the single classifier for every layer that needs the class: the
// gateway's Deps.ClassifyRequest and the intercept's (proxy.go).
func classifyClaudeClass(h http.Header) string {
	if h.Get(subagentIDHeader) != "" {
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

// nodeDirectivePref maps a node-naming /model directive to the routing
// preference THIS REQUEST runs under, or reports that there is none and the
// operator's own preference applies.
//
// Pure over its inputs so every case is a table row rather than a wired-up
// aggregator (CLAUDE.md §Test discipline). Returns a VALUE: nothing here
// writes, so choosing "Waired peer" in /model cannot move the operator's
// persisted `waired worker` setting — picking a model for one conversation
// is not an instruction about the machine.
//
// peer-only rather than peer-preferred because that is what the owner asked
// for ("peer での推論に限定するモードとして", waired-ai/waired#1223) and what
// the mode already means: fail-closed, never falling back to this device
// (docs/decisions/20260801/1840-tray-routing-split-and-peer-only.md §3).
// peers is the live snapshot the per-peer ids were generated from. A
// per-peer id is resolved by re-deriving each peer's slug and comparing —
// the same function that produced the id — rather than by parsing the id,
// so the two can never disagree about what a name reduces to.
//
// operator is the standing `waired worker` preference, and it is an INPUT
// rather than a fallback the caller applies afterwards because of the one
// case where the two say compatible things: a worker pinned to a peer. The
// bare peer entry used to replace that pin with plain peer-only, which
// re-ranked the mesh and sent the turn to a different computer than the one
// the operator had chosen — observed on 0.0.3-rc4 going twice to the
// slowest peer while the pin named the fastest (waired-agent#1040). A pin
// is always to a peer, so honouring it is still peer-only and still
// fail-closed; it is peer-only with the machine named. Owner ruling
// 2026-08-28 on waired-agent#1040: the pin wins.
func nodeDirectivePref(directive string, peers []inferencemesh.PeerView,
	operator state.RoutingPreference,
) (nodeSelection, bool, error) {
	switch {
	case directive == "":
		return nodeSelection{}, false, nil
	case directive == gateway.ModelWairedPeer:
		if operator.Mode == state.RoutingModePinned && operator.PinnedPeerDeviceID != "" {
			return nodeSelection{pref: operator}, true, nil
		}
		return nodeSelection{pref: orderingFrom(operator, state.RoutingPreference{
			Mode: state.RoutingModePeerOnly,
		})}, true, nil
	case directive == gateway.ModelWairedPublic:
		// peer-only AND public-only. The posture still decides what is
		// admissible — this narrows, it does not override (owner ruling
		// 2026-08-20, waired-agent#901) — so on a host whose posture is
		// `auto` the tier comparison still applies and the request can
		// legitimately find nothing.
		//
		// Deliberately NOT pin-aware like the bare peer entry above: a
		// `waired worker` pin names one machine on this operator's own
		// network, which is the population this entry exists to leave.
		return nodeSelection{
			pref: orderingFrom(operator, state.RoutingPreference{
				Mode: state.RoutingModePeerOnly,
			}),
			publicOnly: true,
		}, true, nil
	case !claudecode.IsPeerDirectiveID(directive):
		return nodeSelection{}, false, nil
	}
	for _, p := range peers {
		name, ok := inferencemesh.PeerDisplayName(p)
		if !ok {
			continue
		}
		if claudecode.PeerDirectiveID(name) != directive {
			continue
		}
		displayID, _ := inferencemesh.PeerDisplayID(p)
		return nodeSelection{pref: orderingFrom(operator, state.RoutingPreference{
			Mode:                state.RoutingModePinned,
			PinnedPeerDeviceID:  p.DeviceID,
			PinnedPeerDisplayID: displayID,
		})}, true, nil
	}
	// The machine this entry named is gone — renamed, powered off, or
	// dropped out of the mesh since the picker cache was written (which has
	// no TTL). Falling back to the operator's preference would serve the
	// request from somewhere else while the client still displays the name
	// the user picked, which is the silent substitution waired-agent#325
	// took out of the pin. ErrModelNotReady so the gateway answers 404 and
	// the client shows a visible model error.
	return nodeSelection{}, false, fmt.Errorf(
		"%w: no computer named in %q is on the mesh right now — reopen /model after restarting Claude Code",
		router.ErrModelNotReady, strings.TrimPrefix(directive, gateway.ModelWairedPeerPrefix))
}

// nodeSelection is how one request's /model choice narrows node selection: a
// routing preference, plus the axes that are not expressible as one.
//
// publicOnly is separate rather than a sixth state.RoutingMode because
// RoutingMode is the operator's PERSISTED setting — it is serialised to
// <state-dir>/runtime/desired-worker and rendered by the tray, `waired
// worker` and the management API. A per-request choice does not belong in a
// type whose whole purpose is to outlive the request.
type nodeSelection struct {
	pref       state.RoutingPreference
	publicOnly bool
}

// effectivePref is how one request is selected: the directive's choice when
// the client picked a node-naming /model entry, the operator's otherwise —
// and, for the bare peer entry, the operator's pin carried through the
// directive (see nodeDirectivePref).
//
// The preference is read ONCE and handed down, so the value the directive
// was resolved against is the value the fallback would have used. Reading it
// twice would let a `waired worker` change land between the two and produce
// a selection that matches neither.
func (c *claudeSelector) effectivePref(req router.Request) (nodeSelection, error) {
	var peers []inferencemesh.PeerView
	if req.NodeDirective != "" && c.p != nil && c.p.meshSnapshotFn != nil {
		peers = c.p.meshSnapshotFn().Peers
	}
	operator := c.workerPref()
	sel, ok, err := nodeDirectivePref(req.NodeDirective, peers, operator)
	if err != nil {
		return nodeSelection{}, err
	}
	if ok {
		return sel, nil
	}
	return nodeSelection{pref: operator}, nil
}

// selectWithWorkerPref is the one implementation of the worker-preference
// selection shared by Select and SelectK, so node choice cannot drift between
// the two entry points. run executes one selection against a Selector built
// for the preference that applies to this request; its error — including a
// pinned peer that cannot serve, or peer-only with no peer able to answer —
// is returned untouched.
func selectWithWorkerPref[T any](ctx context.Context, c *claudeSelector, req router.Request,
	run func(ctx context.Context, sel *router.Selector, req router.Request) (T, error),
) (T, error) {
	node, err := c.effectivePref(req)
	if err != nil {
		var zero T
		return zero, err
	}
	return run(ctx, c.p.buildSelectorWith(ctx, node.pref, node.publicOnly), req)
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

// orderingFrom carries the operator's ordering preferences onto a
// preference this directive built (waired-agent#1128).
//
// A /model directive says WHERE inference may run — this device only,
// a peer only, one named machine. It says nothing about which of several
// admissible computers to prefer, or how small a model this operator is
// willing to route to; those are separate axes, set on a separate
// surface, and rebuilding the preference from the mode alone silently
// discarded them.
//
// It is the same defect waired-agent#1040 found on the pin, one field
// over: measured on real hardware, `claude-waired-peer` ignored a `large`
// routing floor and served from a medium model.
func orderingFrom(operator, built state.RoutingPreference) state.RoutingPreference {
	built.Prefer = operator.Prefer
	built.MinModelSize = operator.MinModelSize
	return built
}
