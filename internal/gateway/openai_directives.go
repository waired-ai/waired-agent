package gateway

import (
	"log/slog"

	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
	"github.com/waired-ai/waired-agent/internal/router"
)

// The OpenAI-dialect surface's half of the reserved route directives
// (waired-agent#1306).
//
// The Claude intercept has understood ids that name a COMPUTER rather than a
// model since waired-agent#830 — waired, waired/local, waired/peer,
// waired/peer-<node>, waired/public. The OpenAI leg understood none of them:
// it built router.Request{Model, StickyID} from the raw client string, so
// "waired/peer" missed the catalog and came back a 404 (measured on sv-mag,
// 0.0.3-rc6, 2026-09-12). The owner's rc6 review asked for the same choices in
// OpenCode's and OpenClaw's pickers that Claude Code's /model has
// (waired-ai/waired#1349).
//
// Three things had to meet for that to work, and the other two are elsewhere:
// the listing has to OFFER the ids (handleOpenAIModels), and the Selector
// behind the listener has to READ the directive (the agent wires the same
// directive-aware Selector on the Local Gateway that the Claude intercept has
// always had — cmd/waired-agent/inference.go). This file is the request half.

// applyRouteDirective fills in the route-directive fields of a request built
// from an OpenAI-dialect body, and reports whether the id named a directive.
//
// The model id is mapped here rather than through Deps.ResolveUnknownModel,
// which is what the Anthropic leg uses for the same job. That hook answers
// "the caller named none" for EVERY id the catalog missed, which is right
// where every id the client sends is an Anthropic one, and wrong here: on this
// surface a typo'd model name is a typo, and turning it into the host's
// default silently serves a model nobody asked for. So only the ids this build
// recognises as directives are mapped, and everything else keeps its 404.
func (h *HandlerSet) applyRouteDirective(req *router.Request) bool {
	if !h.deps.RouteDirectives || !isWairedDirective(req.Model) {
		return false
	}
	// The directive travels in its own field because Model does not survive:
	// a directive names a node, and the node still has to be given a model to
	// run, so Model becomes the "caller named none" alias the router ranks
	// nodes for (router.DefaultModelAlias). Reading the choice back off Model
	// later would read the alias (docs/decisions/20260820/0200-model-picker-
	// can-name-a-node.md §2).
	req.NodeDirective = NodeDirectiveFor(req.Model)
	// Same seat, same reason: a tier the client spelled into the id is a
	// promise about the serving node, and it has to outlive the rewrite. This
	// surface has no Anthropic-Beta header, so the id is the only place a
	// tier can be stated — which is why RequiredWindowFor is asked here and
	// RequiredWindowForRequest is not.
	req.MinContextWindow = RequiredWindowFor(req.Model)
	slog.Debug("openai route directive",
		"requested", req.Model, "node_directive", req.NodeDirective,
		"min_context_window", req.MinContextWindow)
	req.Model = router.DefaultModelAlias
	return true
}

// routeDirectiveRows is what GET /v1/models advertises above the catalog, and
// is empty on a listener that does not honour directives at all.
//
// With no hook wired the fixed table is still offered. That is the honest
// answer for a build that cannot see a mesh: the four ids route on any host,
// and a host with no peers gets a peer row that fails with a reason rather
// than a menu that silently lacks the choice. The rows a host CANNOT keep —
// the local row with local inference off, the public row without Public
// Share — are exactly the ones that need the daemon's facts, so a hook-less
// listener errs towards the pre-waired-agent#830 table.
//
// The any-node row is spelled waired/default here, not "waired". They are one
// destination: DefaultModelAlias is the router's "the caller named no model,
// rank the nodes" (#632), which is what the any-node row asks for, and it is
// the id this listing has advertised since long before the route directives
// reached it. The bare spelling cannot be the one offered here because both
// clients that read this listing address a model as <provider>/<model> —
// OpenCode composes the picker ref from the map key, OpenClaw from the
// allowlist entry — so a bare "waired" has no second segment to be. Measured
// on OpenClaw 2026.9.4 (2026-09-12): an allowlist entry of "waired" is read as
// the model "waired" on the provider "openai". The bare id is still ACCEPTED
// on the wire — isWairedDirective knows it, and a person who types it, or a
// config written against the Claude picker, must not get a 404 for naming the
// same thing a different way.
func (h *HandlerSet) routeDirectiveRows() []modelrows.Row {
	if !h.deps.RouteDirectives {
		return nil
	}
	rows := h.deps.RouteDirectiveRows
	if rows == nil {
		rows = func() []modelrows.Row { return modelrows.Rows(modelrows.Facts{LocalServes: true}) }
	}
	out := rows()
	for i := range out {
		if out[i].ID == ModelWairedAny {
			out[i].ID = router.DefaultModelAlias
		}
	}
	return out
}
