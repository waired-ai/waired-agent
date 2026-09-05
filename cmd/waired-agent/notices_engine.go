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
func engineNoticePublisher(reg *notice.Registry, info func() engineProvenance) func(context.Context) {
	if reg == nil || info == nil {
		return nil
	}
	return func(context.Context) {
		reg.Publish(noticeSourceEngine, engineNotices(info()))
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
func engineNotices(p engineProvenance) []notice.Notice {
	var out []notice.Notice
	if p.VersionWarning != "" {
		out = append(out, notice.EngineVersion(p.Engine, p.VersionWarning))
	}
	if p.TuningWarning != "" {
		out = append(out, notice.EngineTuning(p.Engine, p.TuningWarning, p.TuningDegraded))
	}
	return out
}
