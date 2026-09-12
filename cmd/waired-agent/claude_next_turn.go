package main

import (
	"context"
	"errors"
	"strings"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// nextTurnForClaude answers the one question the Claude Code footer exists to
// answer: can anything on Waired take the next turn, and if not, why.
//
// It is HERE, and not in the footer, because the footer cannot answer it. It
// had two booleans — this device's engine health, and
// `mesh.known && mesh.reachable` — and neither consults the operator's
// minimum model class. On an engine-less host under a floor that excludes
// every peer, that printed a GREEN "on Waired (peer …)" while the very same
// turn was refused with `local_model_too_small`: the footer and the refusal
// contradicted each other about one turn (waired-agent#1129, still open on
// that issue after PR #1154).
//
// Adding wording there could not fix it. The floor lives in the Selector, so
// the predicate has to be asked of the Selector — and re-deriving the floor
// in the CLI would be a second implementation of a rule that has already
// moved twice.
//
// Cost: it runs the ordering, which since waired-agent#1302 is one list over
// this device and the mesh, so one call answers for both. No network — the
// probe layer is above this. It rides the route endpoint the footer ALREADY
// fetches, so the segment costs no extra round trip. And it is a DRY run:
// the Selector it builds carries no Recorder and no public-share nudge hook,
// so asking the question records no selection event and does not spend the
// one-shot nudge on a turn nobody sent.
func (p *agentInferenceProvider) nextTurnForClaude(ctx context.Context) *management.ClaudeNextTurn {
	if p == nil {
		return nil
	}
	sel := &claudeSelector{p: p}
	node, err := sel.effectivePref(router.Request{})
	if err != nil {
		return &management.ClaudeNextTurn{Reason: nextTurnReason(err)}
	}
	in := p.selectorInputs(ctx, node.pref, node.publicOnly)
	// A question is not a selection. Both of these emit once per real
	// SelectK, and the nudge is one-shot per host.
	in.Recorder = nil
	in.OnPublicNudge = nil
	in.OnPublicGrantDemand = nil
	cands, err := router.NewSelector(in).SelectK(ctx, router.Request{
		Model: router.DefaultModelAlias,
		Class: state.ClaudeClassMain,
	}, 1)
	if err != nil || len(cands) == 0 {
		return &management.ClaudeNextTurn{Reason: nextTurnReason(err)}
	}
	out := &management.ClaudeNextTurn{CanServe: true, Where: cands[0].ExecutionMode}
	if cands[0].PeerDisplayID != "" {
		out.Peer = cands[0].PeerDisplayID
	}
	return out
}

// nextTurnReason renders a selection failure as the short phrase a footer can
// print inside parentheses.
//
// Short on purpose: this lands in a one-line segment beside the rest of the
// user's status line, so it names the thing to act on and stops. The full
// sentence is what the turn itself returns.
func nextTurnReason(err error) string {
	switch {
	case err == nil:
		return "no computer can take this turn"
	case router.BelowModelSizeFloor(err):
		// The case waired-agent#1129 was left open on, and the reason this
		// function exists: the floor is the operator's own setting, so the
		// footer names the setting rather than reporting a fault.
		if floor := router.ModelSizeFloor(err); floor != "" {
			return "no computer runs a " + router.ModelSizePhrase(floor)
		}
		return "no computer meets the model floor"
	case errors.Is(err, router.ErrLocalInferenceOff):
		return "local inference is off, and no other computer can answer"
	case errors.Is(err, router.ErrPinnedPeerUnreachable):
		return "the pinned computer is not answering"
	case errors.Is(err, router.ErrAllPeersOverloaded):
		return "every computer is busy"
	case errors.Is(err, router.ErrPeersDidNotAnswer):
		return "no computer answered"
	case errors.Is(err, router.ErrModelNotReady) && router.ModelIsArriving(err):
		return "the model is still downloading"
	case errors.Is(err, router.ErrModelNotReady), errors.Is(err, router.ErrModelNotFound):
		return "no computer serves this model"
	default:
		// Anything unforeseen: say the shortest true thing rather than
		// leaking a package-prefixed diagnosis into a footer.
		if s := strings.TrimSpace(err.Error()); s != "" && len(s) < 60 {
			return s
		}
		return "no computer can take this turn"
	}
}
