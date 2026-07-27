package main

import "strings"

// The copy of the takeover exchange. Kept together so the wording of the
// question and of both answers can be read as one conversation.
const (
	takeoverExplainLine = "Taking over means this terminal drives setup and the browser page stops. " +
		"Any model download keeps running either way."
	takeoverQuestionLine = "  Take over setup in this terminal? [y/N] (default: No)"
	takeoverAcceptedLine = "Taking over — setup continues in this terminal."
	takeoverDeclinedLine = "Continuing in your browser — keep this terminal window open until setup finishes."

	// setupKeepTerminalOpenLine is the load-bearing line of the whole
	// browser-driven flow (waired#939). Neither surface used to say it,
	// and the terminal said the opposite — it offered to switch, at
	// exactly the point where closing it costs the install.
	//
	// The asymmetry is the reason: this process is the elevated executor,
	// the management API has no engine-install route, so a closed terminal
	// leaves the browser with nothing it can do but send the operator back
	// to the machine. The published walkthrough has carried this caution
	// since it was written; the product never spoke it.
	//
	// It lives beside the takeover copy so the two are read as one
	// conversation — and so the ordering rule survives: the persistence
	// line comes BEFORE any mention of switching. When waired-agent#198
	// removes the switch offer after the point of no return, it only has
	// to drop a line, not rewrite this one.
	setupKeepTerminalOpenLine = "Keep this terminal window open until setup finishes — " +
		"it does the parts the browser can't."

	// setupTerminalDoneLine replaces it once this process has finished its
	// share: the executor's work is done and the lease is about to drop, so
	// repeating "keep it open" would be an instruction that no longer
	// applies (waired#939 asks for the degraded wording, not the same one).
	setupTerminalDoneLine = "Setup is continuing in your browser — nothing more is needed from this terminal."
)

// enterWatch is how a foreground wait notices the operator asking for
// the terminal back, without a reader of its own: it polls whatever the
// stdin owner already has, so nothing is ever parked in a read that a
// later prompt would have to reconcile (#185, #132, #223).
//
// It serves the two waits that offer an escape, and they are NOT the
// same question:
//
//   - the takeover (waired#835 §4.1, newTakeoverWatch) — an offer nobody
//     asked for, competing with later prompts. Enter is still the key the
//     offer names and the docs teach, but it no longer switches mode by
//     itself: it says what taking over does and asks a [y/N] question
//     that only an affirmative answer completes (#184). That matters
//     because the sign-in step above can leave an Enter in the buffer —
//     pressed to open a browser, arriving here — and a silent mode switch
//     at that moment is the failure #184 describes. A second bare Enter
//     answers with the default, No.
//   - Enter-to-background (waired#774, newBackgroundWatch) — the escape
//     of a download the operator explicitly accepted. There a bare Enter
//     genuinely means "stop watching", so the first line acts.
type enterWatch struct {
	in      *stdinReader // nil = inert (no terminal, or an older daemon)
	confirm bool         // ask before acting (the takeover)
	asked   bool         // the confirmation question is on screen
	fired   bool
}

// newTakeoverWatch arms the confirming watch over the init stdin owner.
// A nil owner yields an inert watch — never a nil one — so callers can
// poll unconditionally.
func newTakeoverWatch(in *stdinReader) *enterWatch {
	return &enterWatch{in: in, confirm: true}
}

// newBackgroundWatch arms the non-confirming watch: the first line
// backgrounds the wait, which is the waired#774 contract for a download
// the operator just accepted.
func newBackgroundWatch(in *stdinReader) *enterWatch {
	return &enterWatch{in: in}
}

// Poll consumes at most one already-typed line and advances the
// exchange. It never blocks.
//
// note is what the terminal should say, if anything; the caller prints
// it AFTER terminating any in-place progress line, so the bar is not
// clobbered. fired latches true once the wait should end, and later
// polls then stay silent.
func (w *enterWatch) Poll() (fired bool, note string) {
	if w == nil || w.in == nil || w.fired {
		return w.Fired(), ""
	}
	line, ok := w.in.Poll()
	if !ok {
		return false, ""
	}
	if !w.confirm {
		// waired#774: the operator asked for this wait, so a keystroke
		// ends it. The caller narrates what happens next.
		w.fired = true
		return true, ""
	}
	if !w.asked {
		// First keystroke: explain, then ask. Nothing has changed yet.
		w.asked = true
		return false, takeoverExplainLine + "\n" + takeoverQuestionLine
	}
	// The question is on screen; this line is its answer.
	w.asked = false
	if takeoverAffirmative(line) {
		w.fired = true
		return true, takeoverAcceptedLine
	}
	return false, takeoverDeclinedLine
}

// Fired reports whether the watch has ended its wait — for the takeover,
// that the operator confirmed. Safe on a nil watch.
func (w *enterWatch) Fired() bool { return w != nil && w.fired }

// takeoverAffirmative recognises the same yes vocabulary as ynPrompt, so
// the confirmation behaves like every other question `waired init` asks.
// Anything else — including the bare Enter that means "the default", and
// the default here is No — declines.
func takeoverAffirmative(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
