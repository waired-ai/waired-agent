package main

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/notice"
)

// engineNoticePublisher is the "engine" producer: what the engine that
// answers this computer's requests has to say about itself.
//
// It reads EngineProvenance, which is the daemon's own answer to "which
// engine is serving" (waired-agent#1076) — better than the heuristic the
// tray used to apply to the runtime map, and the same source `waired
// doctor` already reads through the observability state.
//
// Started with the other inference producers: a host that runs no engine
// has no engine to warn about, and the loop simply never starts there.
// answering is the pair of readings that disagree when the engine is
// running and not answering, and it is nil on a host with nothing to ask
// (see engineNotAnswering).
func engineNoticePublisher(reg *notice.Registry, info func() engineProvenance, answering func() (live, latchedReady, known bool)) func(context.Context) {
	if reg == nil || info == nil {
		return nil
	}
	return func(context.Context) {
		live, latchedReady, known := false, false, false
		if answering != nil {
			live, latchedReady, known = answering()
		}
		reg.Publish(noticeSourceEngine, engineNotices(info(), live, latchedReady, known))
	}
}

// engineNotices is the mapping, split from the read above so the rules
// it encodes are testable without an engine to ask for a version.
//
// A LIST, not a chain. `waired doctor` returned one finding per engine
// and reached the tuning warning only when there was no version warning,
// so a host with both was told about one of them and never learned about
// the other (waired-agent#1229). Two independent facts about the engine
// are two notices; nothing here has to choose between them.
//
// last_error is deliberately absent. It is why the engine is not running
// — state, not advice — and every surface already shows it in the place
// it shows engine state.
func engineNotices(p engineProvenance, live, latchedReady, known bool) []notice.Notice {
	var out []notice.Notice
	if p.VersionWarning != "" {
		out = append(out, notice.EngineVersion(p.Engine, p.VersionWarning))
	}
	if p.TuningWarning != "" {
		out = append(out, notice.EngineTuning(p.Engine, p.TuningWarning, p.TuningDegraded))
	}
	if engineNotAnswering(live, latchedReady, known) {
		out = append(out, notice.EngineNotAnswering(p.Engine))
	}
	return out
}

// engineNotAnswering is the disagreement between this computer's two
// observers of its own engine, and nothing else (waired-agent#1220).
//
//   - live is the probe loop's last call to the engine's own health
//     endpoint, the reading published to the mesh, which is why other
//     computers stop sending work here within a tick;
//   - latchedReady is what every LOCAL surface reads: the serving
//     adapter's state, plus the operator's switches. The adapter
//     re-observes the engine only when its PROCESS exits, so an engine
//     that is stuck says ready for as long as the daemon lives.
//
// Both readings are needed, and in this order. latchedReady false is the
// ordinary case for a host whose engine is off, stopped or failed — the
// operator's own doing, or a fault the surfaces already name with its own
// last_error — and warning there would tell a person their engine is
// broken because they turned it off.
//
// known false means the probe has not reported yet: boot, a host with no
// engine, a daemon whose loop has not run. Not observed is not a fault.
func engineNotAnswering(live, latchedReady, known bool) bool {
	return known && latchedReady && !live
}
