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

// modelTarget names the model the browser wizard chose, so the model wait
// can report on THAT model instead of on whatever the agent happens to be
// serving (#306).
//
// A sibling of setupWatch over the same seam, and deliberately not the
// same type: the two have opposite lifetimes and opposite arming rules.
// setupWatch reports the false->true EDGE once and then leaves the daemon
// alone, and newSetupWatch makes it inert when a setup was ALREADY
// driving — which is exactly the case #306 is about, since the wizard is
// usually on screen before the model wait begins. This is a LEVEL that
// has to stay readable for the whole wait. Folding them together would
// mean weakening both of setupWatch's #308 contracts.
type modelTarget struct {
	// state reads the daemon's setup view. nil = inert: no wizard can be
	// driving here, so nothing is polled and no request is made.
	state func() management.SetupStateResponse
	// every throttles the reads, as in setupWatch. Zero means every poll
	// reads — for tests that drive reads rather than time.
	every time.Duration
	next  time.Time
	// want is the last model a LIVE wizard asked for, resolved to the
	// catalog id. Once set it is only ever replaced, never cleared — see
	// Poll.
	want string
	// refusedCode / refusedDetail are the daemon's account of REFUSING to
	// apply want (waired-agent#404), latched from the same read Poll
	// already makes. Empty until one is reported, and dropped whenever
	// want is replaced: a refusal belongs to one desired model, and the
	// daemon drops it on the same event.
	refusedCode   string
	refusedDetail string
}

// targetLatchedBackoff slows the reads once a target is latched. From
// there the only thing left to notice is an operator going back to change
// their mind, which is a human action; at the unlatched cadence a wizard's
// 8-hour residency budget would be ~14k loopback GETs, each one
// synchronous inside a render loop that ticks once a second.
const targetLatchedBackoff = 15 // 2s -> 30s

// newModelTarget arms the target for one model wait. An unsupported
// session (a daemon older than the executor routes) yields an inert
// target, which keeps every path that is byte-identical today
// byte-identical, at zero extra requests.
//
// There is no alreadyActive parameter, unlike newSetupWatch: a wizard that
// was already driving when the wait began is the case this exists for, not
// the case it opts out of. It is likewise not gated on the caller's
// nonInteractive/noBrowser flags: a `-Yes` install still has a browser
// driving it, and the bounded grace in init_pull.go is what keeps being
// wrong about that cheap.
func newModelTarget(s *executorSession) *modelTarget {
	if !s.Supported() {
		return &modelTarget{}
	}
	return &modelTarget{state: s.State, every: setupStatePollInterval}
}

// Poll returns the model id the wait should key on, or "" while no browser
// wizard has named one.
//
// Two rules, both learned from the daemon's side of #308:
//
//   - Only a LIVE instruction sets it. setupDriving is the single CLI
//     predicate for that; the control plane never clears desired_model_id,
//     so a device set up once carries an instruction naming a model THIS
//     run was never asked to wait for.
//   - The answer is never cleared. All three ways setupDriving can go
//     false mid-wait mean something other than "the wizard left": the
//     daemon's freshness window is 60 minutes while this wait may run an
//     8-hour residency budget; a daemon that RESTARTS reports stale for
//     the rest of its life, because it marks an instruction fresh only by
//     watching it CHANGE and the first frame after boot is the baseline;
//     and a momentarily unreachable daemon answers with the zero value.
//     Clearing on any of those drops the target mid-download and sends the
//     wait back to reporting the agent's own model, which is the bug.
//
// A later live instruction naming a different model DOES replace it: the
// contract is "the model the wizard chose", present tense. An operator who
// went back and picked another one must not be shown a bar for the one
// they abandoned — the daemon has already stopped fetching it.
func (t *modelTarget) Poll() string {
	if t == nil {
		return ""
	}
	if t.state == nil {
		return t.want
	}
	now := time.Now()
	if now.Before(t.next) {
		// The latched answer, not "": returning "" between reads would
		// flap the wait between keyed and unkeyed on most of its ticks.
		return t.want
	}
	if st := t.state(); setupDriving(st) && st.DesiredModelID != "" {
		t.want = canonicalBundledModelID(st.DesiredModelID)
		// Taken from the SAME read that named the model, so a refusal
		// can never be attributed to a model it was not about — and an
		// operator who picks again drops the old answer with the same
		// assignment, because the daemon keys it on the desired model too.
		t.refusedCode, t.refusedDetail = st.ModelErrorCode, st.ModelErrorDetail
	}
	// Scheduled from the state this read just established, not the one
	// before it, so the read that latches a target is already the one that
	// backs off. Until then the reads stay prompt: the wait cannot report
	// the right model until it has been told which one that is.
	every := t.every
	if t.want != "" {
		every *= targetLatchedBackoff
	}
	t.next = now.Add(every)
	return t.want
}

// Refused reports the daemon's refusal to apply the model this target
// names, if it has recorded one (waired-agent#404). ok is false on a
// daemon too old to say — every caller must then fall back to the
// behaviour it had before, not to an assumption.
//
// A refusal is not a fresh observation: the reads back off to
// targetLatchedBackoff once a target is latched, so this can be up to
// half a minute behind. That is the same trade the target itself makes,
// and it replaces a five-minute blind grace.
func (t *modelTarget) Refused() (code, detail string, ok bool) {
	if t == nil || t.refusedCode == "" {
		return "", "", false
	}
	return t.refusedCode, t.refusedDetail, true
}
