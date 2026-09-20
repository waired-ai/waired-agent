package main

import (
	"context"
	"log/slog"
	"sync"
)

// An engine that is held off can be held off for two different reasons, and
// until waired-agent#1464 the product could only say "stopped".
//
// One is the operator's hard stop (#186): a usable engine exists and is
// intentionally down, and nothing should undo that but the operator. The
// other is this product stopping a load that was running the computer out of
// memory (#1453) — nothing is broken, the person has not asked for anything,
// and the moment the reason stops applying the engine should come back.
//
// Telling them apart is what lets the second one recover on its own. It is
// also what stops the first one recovering by accident.
//
// The MECHANISM stays one latch. The adapter's `parked` bool still means
// "EnsureRunning refuses to spawn", every one of its readers is unchanged,
// and peers still skip this host because EngineReady reads that latch. Only
// the REASON lives here, because the reason is policy and the latch is not.

// parkCause is why the engine is held off.
type parkCause int

const (
	// parkCauseNone is an engine that is not held off at all.
	parkCauseNone parkCause = iota
	// parkCauseOperator is the hard stop a person asked for.
	parkCauseOperator
	// parkCauseOutOfMemory is a load this product stopped because the
	// computer was running out of memory, or a load that failed for want
	// of it. The engine is fine; the weights did not fit here.
	parkCauseOutOfMemory
)

func (c parkCause) String() string {
	switch c {
	case parkCauseOperator:
		return "operator"
	case parkCauseOutOfMemory:
		return "out_of_memory"
	default:
		return "none"
	}
}

// parkState records why the engine was last held off.
type parkState struct {
	mu    sync.Mutex
	cause parkCause
}

func (s *parkState) set(c parkCause) {
	s.mu.Lock()
	s.cause = c
	s.mu.Unlock()
}

func (s *parkState) get() parkCause {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cause
}

// noteParked records why the engine is being held off. Called by whoever
// parks it, before or after the park itself — the cause is only ever read
// together with the latch (see parkedBecause).
func (p *agentInferenceProvider) noteParked(c parkCause) {
	if p == nil {
		return
	}
	p.parkedFor.set(c)
}

// parkedBecause is why this host's engine is held off, or parkCauseNone when
// it is not.
//
// The latch is the authority, not this record. Reading them together is what
// keeps a stale cause from outliving the park: `waired inference engine
// start` calls Unpark directly on the adapter, and an engine that came back
// that way must not still claim a reason. The same is true in reverse — a
// park nobody attributed reads as the operator's, which is the behaviour
// before this existed.
func (p *agentInferenceProvider) parkedBecause() parkCause {
	if p == nil || !p.engineIsParked() {
		return parkCauseNone
	}
	if c := p.parkedFor.get(); c != parkCauseNone {
		return c
	}
	return parkCauseOperator
}

// parkForOutOfMemory holds the engine off because a load ran this computer
// out of memory (waired-agent#1453).
//
// Parking rather than leaving the engine up is the whole point: a request
// that arrives now would start the same load again, take minutes over it,
// and put the machine back under the pressure it just came out of. Held off,
// the gateway answers at once and peers stop choosing this host, because
// EngineReady reads the same latch.
//
// The operator's own stop wins. If a person has already hard-stopped the
// engine there is nothing here to do, and overwriting the cause would make
// their stop look like ours — and then clear itself on the next model
// switch.
func (p *agentInferenceProvider) parkForOutOfMemory(ctx context.Context, why string) {
	if p == nil || p.ollama == nil {
		return
	}
	if p.parkedBecause() == parkCauseOperator {
		return
	}
	p.noteParked(parkCauseOutOfMemory)
	if err := p.ollama.Park(ctx); err != nil {
		// Park failed, so the engine is still up and the cause would be a
		// claim about a state this host is not in.
		p.noteParked(parkCauseNone)
		if p.logger != nil {
			p.logger.Warn("could not stop the engine after a load ran the computer out of memory",
				"err", err)
		}
		return
	}
	if p.logger != nil {
		p.logger.Warn("inference stopped: this computer ran out of memory loading the model",
			"why", why)
	}
}

// resumeAfterOutOfMemory releases an engine held off for memory, and reports
// whether it did anything.
//
// It is deliberately narrow: only a memory park is released, and the
// operator's hard stop is left exactly where it is. A person who stopped the
// engine did not ask for it back because they changed model.
//
// Nothing here is on a timer. Every caller is either a person asking for
// something or a fact about the machine that changed — see the callers for
// the four of them (waired-agent#1464). A load that failed does not become
// loadable because time passed, and retrying on a schedule is what put the
// reference host under the same memory pressure twice (#1443, #1450).
func (p *agentInferenceProvider) resumeAfterOutOfMemory(because string) bool {
	if p == nil || p.ollama == nil {
		return false
	}
	if p.parkedBecause() != parkCauseOutOfMemory {
		return false
	}
	p.noteParked(parkCauseNone)
	p.ollama.Unpark()
	// The give-up latch goes with it, for the reason an explicit start
	// clears it: the thing that made this engine stop has changed.
	p.ollama.ClearFailure()
	if p.logger != nil {
		p.logger.Info("inference resumed: the reason it was stopped no longer applies", "because", because)
	} else {
		slog.Info("inference resumed", "because", because)
	}
	p.requestEngineReconcile(false)
	return true
}
