package main

import (
	"context"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/notice"
)

// noticeRepublish is how often a producer repeats what it wants shown.
//
// The cadence and notice.DefaultTTL (60 s) are a lease with a heartbeat,
// the same shape and the same reasoning as the setup executor's
// (setup_desired.go): three missed heartbeats before a standing notice
// lapses, which is enough that a busy tick does not blink a row off
// screen and short enough that a daemon which stops deriving stops
// showing.
//
// It is not a poll for freshness — the surfaces already read the list
// far more often than this. It is what makes a message the daemon has to
// keep meaning: nothing clears a notice, a producer simply stops saying
// it.
const noticeRepublish = 15 * time.Second

// The producer names. The registry keys a lease by name and one producer
// cannot overwrite another's set, so these are the whole coordination
// between them: each says its own thing, on its own schedule, and stops
// saying it without consulting the others.
const (
	noticeSourceRecommendation = "inference-recommendation"
	noticeSourceUpdate         = "update"
	noticeSourceEngine         = "engine"
	noticeSourceLoadFailure    = "inference-load-failure"
	noticeSourceCustomModels   = "custom-models"
)

// noticeProvider adapts the registry to management.NoticeProvider.
//
// The registry is daemon state rather than session state — a notice is
// about this computer, not about who is signed in — so it is passed
// directly instead of through the switchboard. Before enrollment the
// route answers with an empty list, which is the truth.
type noticeProvider struct{ reg *notice.Registry }

func (p noticeProvider) Notices() []notice.Notice {
	if p.reg == nil {
		return nil
	}
	return p.reg.Active()
}

// runNoticeLoop calls republish every `every` until ctx ends.
//
// It publishes once before the first tick: a daemon that has just
// restarted with a standing condition should say so at once, and waiting
// out a heartbeat would make a restart look like the condition had
// cleared.
//
// A free function taking the republisher rather than a method, so the
// loop's own behaviour — publish first, then on the tick, and stop with
// the context — is testable without a host profile or an engine to ask
// for a version.
func runNoticeLoop(ctx context.Context, every time.Duration, republish func(context.Context)) {
	if republish == nil {
		return
	}
	if every <= 0 {
		every = noticeRepublish
	}
	republish(ctx)

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			republish(ctx)
		}
	}
}

// publishRecommendationNotices republishes the model-switch suggestions
// as notices, deriving them fresh from the last benchmark.
//
// Deriving on every call is the point: the registry never holds a
// verdict of its own, so when the condition stops being true the notice
// stops being published and lapses. Nothing has to notice that it went
// away.
//
// Also called directly from the paths that change the answer — declining
// a suggestion, switching model — because a person who has just acted
// should not watch the row they answered sit there for another
// heartbeat.
func (p *agentInferenceProvider) publishRecommendationNotices(ctx context.Context) {
	if p == nil || p.notices == nil {
		return
	}
	p.notices.Publish(noticeSourceRecommendation, p.recommendationNotices(ctx))
}

// recommendationNotices turns the live recommendation into notices.
//
// There is at most one — the lighter-model suggestion; the upgrade one is
// retired (waired-ai/waired-agent#1342) — but it returns a slice because
// the registry's unit is a producer's whole set, and "nothing to say" has
// to be expressible.
//
// A dismissed suggestion produces nothing, matching every other surface:
// Dismissed exists so the CLI and tray stay quiet about a pairing the
// person has already declined.
func (p *agentInferenceProvider) recommendationNotices(ctx context.Context) []notice.Notice {
	return noticesFromRecommendation(p.currentRecommendation(ctx))
}

// noticesFromRecommendations is the mapping, split from the derivation
// above so the rules it encodes are testable without a host profile or
// an engine to ask for a version.
func noticesFromRecommendation(rec *management.BenchmarkRecommendation) []notice.Notice {
	if showable(rec) {
		return []notice.Notice{notice.LighterModel(
			rec.FromModelID, rec.ToModelID, rec.TurnSeconds, rec.TurnFloorSeconds, rec.BudgetSeconds)}
	}
	return nil
}

// publishLoadFailureNotices republishes "this model did not fit in this
// computer's memory" (waired-agent#1453).
//
// Its own producer rather than part of the recommendation set above,
// because the two say different things and stop being true at different
// moments: a speed suggestion lapses when the next benchmark disagrees,
// while this one stands until the build changes or the person chooses
// something else. The registry keys a lease per producer, so each can keep
// its own answer without consulting the other.
func (p *agentInferenceProvider) publishLoadFailureNotices(ctx context.Context) {
	if p == nil || p.notices == nil {
		return
	}
	p.notices.Publish(noticeSourceLoadFailure, p.loadFailureNotices(ctx))
}

// loadFailureNotices derives the notice from the record, on every call, so
// that when the record stops applying — a new engine build, a new driver, a
// different model — the notice stops being published and lapses with nothing
// having to notice.
func (p *agentInferenceProvider) loadFailureNotices(ctx context.Context) []notice.Notice {
	rec, blocked := p.engineLoadIsBlocked()
	if !blocked {
		return nil
	}
	return []notice.Notice{
		notice.ModelDidNotLoad(rec.ModelID, p.smallerAlternative(ctx, rec), rec.Reason),
	}
}

// showable is the predicate every other surface already applies: a
// suggestion with no target says nothing, and a dismissed one is a
// pairing the person has already declined.
func showable(rec *management.BenchmarkRecommendation) bool {
	return rec != nil && !rec.Dismissed && rec.ToModelID != ""
}
