package main

import (
	"context"
	"time"
)

// engineDrainPoll is how often a drain re-reads the in-flight count. Short
// relative to the budget (ten minutes by default) so a turn that finishes
// early releases the bounce promptly, long enough that a ten-minute wait
// costs a few hundred atomic loads rather than a busy loop.
const engineDrainPoll = 500 * time.Millisecond

// drainOutcome says how a drain ended, for the log line and for the tests.
type drainOutcome uint8

const (
	// drainIdle — nothing was running, so nothing was waited for. The
	// steady-state case: the log stays silent.
	drainIdle drainOutcome = iota
	// drainQuiet — turns were running and all of them finished.
	drainQuiet
	// drainBudgetSpent — the budget ran out with turns still running. The
	// bounce happens anyway and those turns are cut.
	drainBudgetSpent
	// drainStopping — the daemon is going away. A statement about this
	// process, and none at all about the turns.
	drainStopping
	// drainDisabled — the budget is 0, so the caller did not ask to wait.
	drainDisabled
)

func (o drainOutcome) String() string {
	switch o {
	case drainIdle:
		return "idle"
	case drainQuiet:
		return "quiet"
	case drainBudgetSpent:
		return "budget_spent"
	case drainStopping:
		return "stopping"
	default:
		return "disabled"
	}
}

// engineDrainBudget is the operator's budget for one drain, or 0 when they
// turned the wait off.
func (p *agentInferenceProvider) engineDrainBudget() time.Duration {
	ms := p.effectiveCfg().EngineDrainBudgetMs
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// awaitEngineDrain blocks until this machine's engine is serving nothing, and
// reports how the wait ended (waired-agent#1304).
//
// It is for a bounce this device CHOSE. Stopping `ollama serve` severs every
// request the engine is in the middle of, and a same-engine model switch does
// exactly that despite applying "in process"
// (docs/decisions/20260813/2123-model-swap-applies-in-process.md: the agent
// stays up, the engine process does not). On the 0.0.3-rc6 fleet that ended
// whatever turn was running, mid-stream as a truncation and pre-headers as a
// 502.
//
// It is NOT for crash recovery. There the engine is already gone: the turns
// are already lost and waiting would only lengthen the outage. The same line
// deferRetuneWhilePulling draws, and for a neighbouring reason.
//
// Nor does it contradict that function's "an operator's model switch must not
// be held". What it refuses to hold a switch behind is a DOWNLOAD the person
// did not ask about. A turn is the opposite: it is the person's own work, on
// the model they are replacing, and cutting it is the cost the switch was
// silently charging them.
//
// The wait is bounded because nothing here blocks new arrivals — admission is
// the gateway's, and holding it would answer a turn with a 503 to spare it a
// truncation. So on a machine under load in-flight may never reach zero, and
// past the budget the bounce proceeds.
//
// Reads servingInFlight, which counts BOTH this device's own turns and the
// mesh turns it is serving for peers (internal/inference inflightCounter).
// Both are severed by the bounce; neither asked for it.
func (p *agentInferenceProvider) awaitEngineDrain(ctx context.Context, budget time.Duration) drainOutcome {
	if budget <= 0 {
		return drainDisabled
	}
	if p.servingInFlight() == 0 {
		return drainIdle
	}
	deadline := time.Now().Add(budget)
	for {
		select {
		case <-ctx.Done():
			return drainStopping
		case <-time.After(engineDrainPoll):
		}
		if p.servingInFlight() == 0 {
			return drainQuiet
		}
		if !time.Now().Before(deadline) {
			return drainBudgetSpent
		}
	}
}

// drainBeforeBounce runs awaitEngineDrain and logs both ends, or neither.
//
// why names the bounce in the log ("model switch", "residency respawn",
// "serve-env change"), so a reader who finds a ten-minute gap between the
// operator's click and the engine restarting can see what was being waited
// for without reading this file.
//
// A drain that found nothing running writes nothing: the steady-state bounce
// is byte-identical in the journal to the one before #1304.
func (p *agentInferenceProvider) drainBeforeBounce(ctx context.Context, why string) drainOutcome {
	budget := p.engineDrainBudget()
	if budget <= 0 {
		return drainDisabled
	}
	inflight := p.servingInFlight()
	if inflight == 0 {
		return drainIdle
	}
	p.logger.Info("holding the engine bounce while turns are still running",
		"why", why, "inflight", inflight, "budget_ms", budget.Milliseconds())
	started := time.Now()
	out := p.awaitEngineDrain(ctx, budget)
	p.logger.Info("engine bounce hold ended",
		"why", why, "outcome", out.String(),
		"waited_ms", time.Since(started).Milliseconds(),
		"inflight", p.servingInFlight())
	return out
}

// noteEngineStopped records that this device stopped its own engine on
// purpose, right now (waired-agent#1304).
//
// Called from the deliberate stops only. Crash recovery does not call it: the
// engine there died by itself, and a turn that died with it was not ended by
// anything this device decided.
func (p *agentInferenceProvider) noteEngineStopped() {
	if p == nil {
		return
	}
	at := time.Now()
	p.engineStoppedAt.Store(&at)
}

// engineRestartedSince reports whether this device stopped its own engine on
// purpose at any point after since.
//
// The gateway asks it about a request that failed, passing the instant that
// request started, so the answer is "the engine was pulled out from under
// this turn" and not "a bounce happened near this turn". A zero since — a
// caller with no start time — is never answered yes.
func (p *agentInferenceProvider) engineRestartedSince(since time.Time) bool {
	if p == nil || since.IsZero() {
		return false
	}
	at := p.engineStoppedAt.Load()
	return at != nil && at.After(since)
}
