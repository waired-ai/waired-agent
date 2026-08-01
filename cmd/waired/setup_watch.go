package main

import (
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// setupWatch notices a browser setup that starts AFTER awaitSetupBudget
// gave up waiting for one (#308).
//
// That wait is a single 3-minute window: if the operator is still reading
// the model picker when it expires, the CLI prints "No setup started in
// the browser; continuing here." and drops into terminal-driven mode — and
// nothing downstream ever looked at the setup state again. So a wizard
// confirmed at minute four left the terminal offering to take over a setup
// the browser had already started (#309), on the short model-wait budget,
// asking its own questions over a browser that was driving.
//
// The watch is what the long waits poll instead. It is deliberately not an
// *executorSession: the behaviour under test is "what does the wait do on
// the false->true edge", and a function returning the daemon's view puts
// the seam directly under it (the session's own reads stay covered by
// setup_executor_test.go's fake daemon).
type setupWatch struct {
	// state reads the daemon's setup view. nil = inert: no browser setup
	// can start here, so nothing is polled and no request is made.
	state func() management.SetupStateResponse
	// alreadyActive records that the caller was already in browser-driven
	// mode when the watch was armed. Kept as its own flag rather than
	// folded into a nil state so the reason a watch is inert stays legible
	// — and so one built by hand cannot arm by accident.
	alreadyActive bool
	// every throttles the reads: each one is a loopback round trip, and
	// the waits that poll this tick once a second. Zero means every poll
	// reads — for tests that drive reads rather than time, and the only
	// setting that cannot be swallowed by a coarse clock.
	every time.Duration
	next  time.Time
	// started latches the edge. Reported once — the caller narrates the
	// handoff — and the daemon is left alone afterwards.
	started bool
}

// newSetupWatch arms the watch for one model wait. An unsupported session
// (a daemon older than the executor routes) or a setup that is already
// driving yields an inert watch, which is what keeps every path that is
// byte-identical today byte-identical, at zero extra requests.
func newSetupWatch(s *executorSession, alreadyActive bool) *setupWatch {
	if !s.Supported() || alreadyActive {
		return &setupWatch{alreadyActive: alreadyActive}
	}
	return &setupWatch{state: s.State, every: setupStatePollInterval}
}

// Poll reports the moment a browser setup starts, at most once.
//
// It prints nothing: the waits that call it own an in-place progress bar,
// and every line they emit has to land after that bar is terminated
// (init_pull.go). budget and engineComing come from the same read that saw
// the edge, so the caller needs no second round trip.
func (w *setupWatch) Poll() (started bool, budget time.Duration, engineComing bool) {
	if w == nil || w.state == nil || w.alreadyActive || w.started {
		return false, 0, false
	}
	now := time.Now()
	if now.Before(w.next) {
		return false, 0, false
	}
	w.next = now.Add(w.every)

	st := w.state()
	if !setupDriving(st) {
		return false, 0, false
	}
	w.started = true
	return true, setupResidencyBudget, engineArrivalPending(st)
}

// Started reports whether a browser setup was observed since the watch was
// armed. This is how the caller learns that setupActive must flip, and with
// it the §4.2 rule that a terminal must not ask its own questions while the
// browser is driving.
func (w *setupWatch) Started() bool { return w != nil && w.started }
